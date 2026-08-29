package slack

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	slackgo "github.com/slack-go/slack"
)

// maxMarkdownChunkRunes is Slack's documented ceiling for a
// markdown_text chunk. Longer content is split across chunks rather
// than truncated — the whole point of the streaming API is that
// there is no envelope to overflow.
const maxMarkdownChunkRunes = 12000

// maxFinalizationBlocks is how many Block Kit blocks chat.stopStream
// accepts alongside the streamed content.
const maxFinalizationBlocks = 50

// errStreamClosed is returned when a caller appends to a turn whose
// stream has already been finalized.
var errStreamClosed = errors.New("slack: stream already closed")

// turnStream owns exactly one Slack streaming message: the placeholder
// for a single user turn.
//
// Buffering model: callers enqueue chunks, which accumulate in
// pending. A flush drains pending into one API call —
// chat.startStream on the first flush (which mints the ts) and
// chat.appendStream after that. The throttle spaces flushes apart;
// anything arriving inside the window is coalesced into the next one.
//
// Because chat.appendStream APPENDS, a flush never re-sends what is
// already on screen. That is the core difference from the Feishu
// receipt card, which re-PATCHes the whole body every time and
// therefore needs overflow eviction, element budgets and same-body
// short-circuiting. None of that applies here.
type turnStream struct {
	chatID    string
	channelID string
	threadTS  string
	userMsgID string

	api   apiClient
	limit *Limiter
	retry RetryConfig
	log   *slog.Logger
	state *stateStore

	throttle time.Duration
	now      func() time.Time

	mu        sync.Mutex
	ts        string
	pending   []slackgo.StreamChunk
	lastFlush time.Time
	timer     *time.Timer
	closed    bool
	// footer is the StatusBar snapshot, replaced wholesale on each
	// stamp and rendered once at stopStream.
	footer []string
	// toolSeq numbers tool cards within this turn; toolPending is
	// the FIFO of started-but-unfinished tool ids. messages.ToolInfo
	// has no call id, so start/end are paired positionally — the
	// same approach the Feishu adapter uses.
	toolSeq     int
	toolPending []pendingTool
}

// pendingTool is one started-but-unfinished tool card.
type pendingTool struct {
	id    string
	title string
}

func newTurnStream(chatID, channelID, threadTS, userMsgID string, deps streamDeps) *turnStream {
	return &turnStream{
		chatID:    chatID,
		channelID: channelID,
		threadTS:  threadTS,
		userMsgID: userMsgID,
		api:       deps.api,
		limit:     deps.limiter,
		retry:     deps.retry,
		log:       deps.logger,
		state:     deps.state,
		throttle:  deps.throttle,
		now:       time.Now,
	}
}

// streamDeps bundles what every turnStream needs from the adapter.
type streamDeps struct {
	api      apiClient
	limiter  *Limiter
	retry    RetryConfig
	logger   *slog.Logger
	state    *stateStore
	throttle time.Duration
}

// appendMarkdown queues agent text. urgent bypasses the throttle
// window (docs/channel/slack.md §4.2): a blocking prompt that shows
// up three seconds late recreates the very "is it stuck?" doubt the
// placeholder exists to remove.
func (s *turnStream) appendMarkdown(ctx context.Context, text string, urgent bool) error {
	if text == "" {
		return nil
	}
	chunks := make([]slackgo.StreamChunk, 0, 1)
	for _, part := range splitRunes(text, maxMarkdownChunkRunes) {
		chunks = append(chunks, slackgo.NewMarkdownTextChunk(part))
	}
	return s.enqueue(ctx, chunks, urgent)
}

// appendTask queues a task card update. Slack merges updates that
// share an id, which is what lets one tool call render as a single
// card that transitions pending -> in_progress -> complete.
//
// NOTE: that merge behaviour is asserted by Slack's docs but has not
// been confirmed against a live workspace (docs/channel/slack.md §9
// probe 2). If it turns out to append rather than merge, tool calls
// will render as two cards and the caller should stop emitting the
// start half.
func (s *turnStream) appendTask(ctx context.Context, id, title string, status slackgo.TaskCardStatus, details string, urgent bool) error {
	if id == "" || title == "" {
		return nil
	}
	chunk := slackgo.NewTaskUpdateChunk(id, truncateRunes(title, taskFieldMaxRunes))
	chunk.Status = status
	chunk.Details = truncateRunes(details, taskFieldMaxRunes)
	return s.enqueue(ctx, []slackgo.StreamChunk{chunk}, urgent)
}

// beginTool allocates a task card id for a starting tool and pushes
// it onto the pairing FIFO.
func (s *turnStream) beginTool(title string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.toolSeq++
	id := toolTaskID(s.toolSeq)
	s.toolPending = append(s.toolPending, pendingTool{id: id, title: title})
	return id
}

// endTool pops the oldest unfinished tool card. ok is false when an
// end arrives without a matching start (a bridge that only emits the
// end half, or a start that failed before it was recorded); the
// caller then mints a standalone card instead of dropping the event.
func (s *turnStream) endTool() (id, title string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.toolPending) == 0 {
		return "", "", false
	}
	head := s.toolPending[0]
	s.toolPending = s.toolPending[1:]
	return head.id, head.title, true
}

// newStandaloneToolID mints an id for an unpaired tool end.
func (s *turnStream) newStandaloneToolID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.toolSeq++
	return toolTaskID(s.toolSeq)
}

// appendPlan queues a plan title update — the mutable slot the
// heartbeat falls back to when the assistant status API is
// unavailable.
func (s *turnStream) appendPlan(ctx context.Context, title string) error {
	if title == "" {
		return nil
	}
	return s.enqueue(ctx, []slackgo.StreamChunk{
		slackgo.NewPlanUpdateChunk(truncateRunes(title, taskFieldMaxRunes)),
	}, false)
}

// appendBlocks queues Block Kit blocks. Agent-UI blocks (Alert,
// Card, Carousel, TaskCard…) are only accepted through the streaming
// chunks transport; chat.postMessage rejects them outright.
func (s *turnStream) appendBlocks(ctx context.Context, blocks []slackgo.Block, urgent bool) error {
	if len(blocks) == 0 {
		return nil
	}
	if len(blocks) > maxFinalizationBlocks {
		blocks = blocks[:maxFinalizationBlocks]
	}
	return s.enqueue(ctx, []slackgo.StreamChunk{slackgo.NewBlocksChunk(blocks...)}, urgent)
}

// stampFooter replaces the StatusBar snapshot. It does not itself
// trigger a flush: the footer is rendered once, at stopStream, as
// finalization blocks.
func (s *turnStream) stampFooter(lines []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.footer = append([]string(nil), lines...)
}

// enqueue buffers chunks and decides whether to flush now or arm a
// timer for the remainder of the throttle window.
func (s *turnStream) enqueue(ctx context.Context, chunks []slackgo.StreamChunk, urgent bool) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errStreamClosed
	}
	s.pending = append(s.pending, chunks...)

	elapsed := s.now().Sub(s.lastFlush)
	ready := urgent || s.lastFlush.IsZero() || elapsed >= s.throttle
	if ready {
		s.stopTimerLocked()
		return s.flushLocked(ctx)
	}

	// Inside the window: make sure exactly one timer is pending to
	// drain whatever accumulates.
	if s.timer == nil {
		delay := s.throttle - elapsed
		s.timer = time.AfterFunc(delay, s.timerFlush)
	}
	s.mu.Unlock()
	return nil
}

// timerFlush drains the buffer when the throttle window reopens.
// It runs on a fresh context: the request context that queued the
// content may already be cancelled by the time the timer fires.
func (s *turnStream) timerFlush() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	s.mu.Lock()
	s.timer = nil
	if s.closed || len(s.pending) == 0 {
		s.mu.Unlock()
		return
	}
	if err := s.flushLocked(ctx); err != nil {
		s.log.Warn("slack: throttled stream flush failed",
			"chat_id", s.chatID, "err", err)
	}
}

// flushLocked drains pending in one API call. It is called with
// s.mu held and RELEASES it before returning — network work must not
// happen under the lock, or a slow Slack response would block every
// other event for this turn.
func (s *turnStream) flushLocked(ctx context.Context) error {
	if len(s.pending) == 0 {
		s.mu.Unlock()
		return nil
	}
	batch := s.pending
	s.pending = nil
	ts := s.ts
	channelID := s.channelID
	threadTS := s.threadTS
	s.lastFlush = s.now()
	s.mu.Unlock()

	if ts == "" {
		newTS, err := s.startStream(ctx, channelID, threadTS, batch)
		if err != nil {
			s.requeue(batch)
			return err
		}
		s.mu.Lock()
		s.ts = newTS
		s.mu.Unlock()
		// Persist before anything else can fail: an open stream we
		// have forgotten is one nobody will ever close.
		s.state.putStream(&OpenStream{
			ChatID:    s.chatID,
			ChannelID: channelID,
			ThreadTS:  threadTS,
			TS:        newTS,
			UserMsgID: s.userMsgID,
			StartedAt: time.Now().UTC(),
		})
		return nil
	}

	if err := s.appendStream(ctx, channelID, ts, batch); err != nil {
		s.requeue(batch)
		return err
	}
	return nil
}

// requeue puts a failed batch back at the FRONT of pending so
// ordering survives a transient failure.
func (s *turnStream) requeue(batch []slackgo.StreamChunk) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.pending = append(batch, s.pending...)
}

func (s *turnStream) startStream(ctx context.Context, channelID, threadTS string, chunks []slackgo.StreamChunk) (string, error) {
	var ts string
	err := withTransientRetry(ctx, s.retry, s.log, "startStream", func() error {
		if err := s.limit.Wait(ctx); err != nil {
			return err
		}
		var innerErr error
		ts, innerErr = s.api.StartStream(ctx, channelID, threadTS, chunks)
		return innerErr
	})
	return ts, err
}

func (s *turnStream) appendStream(ctx context.Context, channelID, ts string, chunks []slackgo.StreamChunk) error {
	return withTransientRetry(ctx, s.retry, s.log, "appendStream", func() error {
		if err := s.limit.Wait(ctx); err != nil {
			return err
		}
		return s.api.AppendStream(ctx, channelID, ts, chunks)
	})
}

// finish drains anything buffered and closes the stream, rendering
// the StatusBar footer as finalization blocks.
//
// The whole sequence runs under s.mu (timer cancel -> final flush ->
// stop -> mark closed) so a timer that fires concurrently cannot
// resurrect the stream after it has been closed. Telegram learned
// this the expensive way: an orphaned debounce timer firing after
// the turn was purged would edit the previous turn's message
// (docs/channel/telegram.md §11.12; the fix was to hold the chain
// lock across the same four steps).
func (s *turnStream) finish(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.stopTimerLocked()
	s.closed = true

	batch := s.pending
	s.pending = nil
	ts := s.ts
	channelID := s.channelID
	footer := append([]string(nil), s.footer...)
	s.mu.Unlock()

	// A turn that produced nothing never opened a stream; there is
	// nothing to close.
	if ts == "" && len(batch) == 0 {
		return nil
	}

	if ts == "" {
		newTS, err := s.startStream(ctx, channelID, s.threadTS, batch)
		if err != nil {
			return err
		}
		ts = newTS
		s.mu.Lock()
		s.ts = ts
		s.mu.Unlock()
		s.state.putStream(&OpenStream{
			ChatID:    s.chatID,
			ChannelID: channelID,
			ThreadTS:  s.threadTS,
			TS:        ts,
			UserMsgID: s.userMsgID,
			StartedAt: time.Now().UTC(),
		})
	} else if len(batch) > 0 {
		if err := s.appendStream(ctx, channelID, ts, batch); err != nil {
			// Losing the tail is better than leaving the stream open.
			s.log.Warn("slack: final append failed, closing anyway",
				"chat_id", s.chatID, "err", err)
		}
	}

	err := withTransientRetry(ctx, s.retry, s.log, "stopStream", func() error {
		if err := s.limit.Wait(ctx); err != nil {
			return err
		}
		return s.api.StopStream(ctx, channelID, ts, footerBlocks(footer))
	})
	if err != nil {
		return err
	}
	s.state.dropStream(channelID, ts)
	return nil
}

// messageTS reports the stream's Slack message id, or "" if the
// stream has not been opened yet.
func (s *turnStream) messageTS() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ts
}

func (s *turnStream) stopTimerLocked() {
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
}
