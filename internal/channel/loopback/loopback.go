// Package loopback implements an in-process channel that lets a
// program drive nightme the way a human drives it through Feishu.
//
// The channel has two halves:
//
//   - Transport: Client.Send pushes an inbound message into a chat,
//     exactly like a message arriving from an IM platform would. The
//     daemon treats it like any other channel message — same
//     dispatch, same slash commands, same agent turn.
//
//   - Tracking: every command the client sends is tracked from
//     submission to a structured result. The daemon's outbound
//     messages for the chat are correlated back to the command that
//     produced them, so the driver learns what happened from facts
//     (a messages.Outcome) instead of parsing human-readable text.
//
// The channel knows nothing about workflows: it is transport plus
// bookkeeping. The workflow half of internal/channel/bot stays
// there.
//
// The channel registers itself in the channel registry under the
// "lb_" chat-id prefix, so a binary that links this package and
// builds its channels from the registry gets one. The builder needs
// no configuration, so it always succeeds. The daemon does not link
// this package: the channel is driven in-process by a program that
// links it.
package loopback

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/channel"
	"github.com/cnlangzi/nightme/internal/config"
	"github.com/cnlangzi/nightme/internal/messages"
)

const (
	// channelName is the registry name of this channel.
	channelName = "loopback"

	// chatIDPrefix namespaces every chat id this channel produces.
	chatIDPrefix = "lb_"

	// userID is the user every inbound message is attributed to.
	userID = "loopback"

	// inboundBuffer is the depth of the inbound queue. A program
	// that sends faster than the daemon drains gets back-pressure
	// on Send rather than a silently dropped command.
	inboundBuffer = 64

	// watchBuffer is the depth of one watcher's snapshot queue. A
	// watcher that stops reading loses snapshots — the outbound
	// path must never block on a slow reader.
	watchBuffer = 16

	// MaxTranscriptEntries caps the transcript of one execution.
	// The head and the tail are kept; the middle is dropped.
	MaxTranscriptEntries = 200

	// transcriptHead / transcriptTail split the kept part of an
	// over-long transcript.
	transcriptHead = MaxTranscriptEntries / 2
	transcriptTail = MaxTranscriptEntries - transcriptHead

	// executionTTL is how long a settled execution is kept after it
	// settled, for a driver that never awaited it.
	executionTTL = 10 * time.Minute

	// unsettledTTL is how long an execution that never settled is
	// kept. A command whose settle never arrives (the bridge died,
	// the daemon dropped the message) would otherwise stay in the
	// store for the life of the process.
	unsettledTTL = 24 * time.Hour
)

func init() {
	// The loopback channel needs no credentials, so its builder
	// always succeeds — a program that drives nightme gets a chat
	// to drive it in.
	channel.Register(channelName, chatIDPrefix, func(*config.Config) (channel.Channel, error) {
		return New(), nil
	})
}

// Channel is the loopback channel. It satisfies channel.Channel.
//
// Inbound messages arrive through Client.Send; outbound messages
// arrive through Send, which is where the tracking happens.
type Channel struct {
	name   string
	logger *slog.Logger

	// in is the queue the daemon's inbound pump reads from.
	in chan messages.InboundMessage

	mu sync.Mutex
	// seq numbers the generated message ids.
	seq uint64
	// execs holds the live executions, keyed by message id.
	execs map[string]*execution
	// inflight names the execution a message without a ReplyTo
	// belongs to, per chat: the most recent one that has not
	// settled.
	inflight map[string]string
	started  bool
}

// New constructs a loopback Channel.
func New() *Channel {
	return &Channel{
		name:     channelName,
		in:       make(chan messages.InboundMessage, inboundBuffer),
		execs:    make(map[string]*execution),
		inflight: make(map[string]string),
	}
}

// Compile-time check: Channel satisfies channel.Channel.
var _ channel.Channel = (*Channel)(nil)

// Name implements channel.Channel.
func (c *Channel) Name() string { return c.name }

// Start implements channel.Channel. The channel is in-process: there
// is nothing to connect to.
func (c *Channel) Start(context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.started = true
	return nil
}

// Stop implements channel.Channel.
func (c *Channel) Stop(context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.started = false
	return nil
}

// Incoming implements channel.Channel. The daemon's inbound pump
// reads from here.
func (c *Channel) Incoming() <-chan messages.InboundMessage { return c.in }

// Send implements channel.Channel. Every outbound message of this
// chat is correlated back to the command that produced it.
func (c *Channel) Send(_ context.Context, msg messages.OutboundMessage) error {
	c.observe(msg)
	return nil
}

// OnPromptEnded implements channel.Channel as a no-op: the loopback
// channel renders no receipts. A run is complete when the daemon
// reports the settled outcome, not when the prompt ends.
func (c *Channel) OnPromptEnded(context.Context, string, string, agent.PromptEndReason) {
}

// HealthSnapshot implements channel.Channel.
func (c *Channel) HealthSnapshot() (string, json.RawMessage, error) {
	return c.Name(), json.RawMessage(`{"type":"loopback"}`), nil
}

// HealthSummary implements channel.Channel.HealthSummary.
//
// The channel is in-process, so there is no remote link to verify —
// the link always counts as verified.
func (c *Channel) HealthSummary() channel.HealthSummary {
	c.mu.Lock()
	started := c.started
	c.mu.Unlock()
	return channel.HealthSummary{
		Channel:      c.Name(),
		Connected:    started,
		LinkVerified: true,
	}
}

// SetLogger implements channel.Channel.
func (c *Channel) SetLogger(logger *slog.Logger) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.logger = logger
}

// BuildBlocks implements channel.Channel. The channel has no
// rich-text awareness: one text block.
func (c *Channel) BuildBlocks(text string, _ []messages.Attachment) []agent.ContentBlock {
	if text == "" {
		return nil
	}
	return []agent.ContentBlock{{Type: agent.ContentText, Text: text}}
}

// send pushes text into the chat as an inbound message and returns
// the generated message id.
//
// The chat id must carry the channel's own namespace prefix. Chat
// ids are channel-namespaced: a chat of another channel belongs to
// that channel's session, and a chat id without a known prefix is
// dropped by the chat store on the next load. Sending one here
// would fork the other channel's session in two.
//
// The execution is registered before the message is queued, so a
// reply that comes back before this call returns is still tracked.
func (c *Channel) send(ctx context.Context, chatID, text string) (string, error) {
	if chatID == "" {
		return "", errors.New("loopback: empty chat id")
	}
	if !strings.HasPrefix(chatID, chatIDPrefix) {
		return "", fmt.Errorf("loopback: chat id %q is not a loopback chat id (prefix %q)", chatID, chatIDPrefix)
	}

	now := time.Now()
	c.mu.Lock()
	c.seq++
	msgID := fmt.Sprintf("lb-%d", c.seq)
	ex := &execution{
		msgID:     msgID,
		chatID:    chatID,
		text:      text,
		phase:     PhaseSent,
		sentAt:    now,
		settledCh: make(chan struct{}),
	}
	c.execs[msgID] = ex
	c.inflight[chatID] = msgID
	c.sweepLocked(now)
	c.mu.Unlock()

	msg := messages.InboundMessage{
		ChatID:    chatID,
		UserID:    userID,
		Text:      text,
		MessageID: msgID,
		Time:      now,
		// The WatchMode gate drops group messages that do not
		// address the bot. A program's command always does.
		HasMention: true,
	}
	select {
	case c.in <- msg:
		return msgID, nil
	case <-ctx.Done():
		// The message never reached the daemon, so it can never
		// settle. Drop the execution instead of leaving a ghost
		// behind.
		c.mu.Lock()
		delete(c.execs, msgID)
		if c.inflight[chatID] == msgID {
			delete(c.inflight, chatID)
		}
		c.mu.Unlock()
		return "", ctx.Err()
	}
}

// observe folds one outbound message into the execution it belongs
// to.
func (c *Channel) observe(msg messages.OutboundMessage) {
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sweepLocked(now)

	ex := c.targetLocked(msg)
	if ex == nil {
		// The message belongs to no execution of this channel: it
		// names a message id nobody sent here, or it arrived for a
		// chat that has no run in flight.
		if c.logger != nil {
			c.logger.Debug("loopback: message not correlated",
				"kind", msg.Kind.String(),
				"chat_id", msg.ChatID,
				"reply_to", msg.ReplyTo,
			)
		}
		return
	}
	changed, settled := c.applyLocked(ex, msg, now)
	if changed {
		c.notifyLocked(ex)
	}
	if settled {
		c.closeWatchersLocked(ex)
	}
}

// targetLocked picks the execution an outbound message belongs to.
//
// A message that carries a ReplyTo names its execution outright; a
// message without one belongs to the chat's in-flight execution.
// A ReplyTo that names no live execution — or one from another chat
// — is not this channel's business and is dropped.
func (c *Channel) targetLocked(msg messages.OutboundMessage) *execution {
	if msg.ReplyTo != "" {
		ex, ok := c.execs[msg.ReplyTo]
		if !ok || ex.chatID != msg.ChatID {
			return nil
		}
		return ex
	}
	id, ok := c.inflight[msg.ChatID]
	if !ok {
		return nil
	}
	ex, ok := c.execs[id]
	if !ok {
		return nil
	}
	return ex
}

// applyLocked folds one outbound message into an execution. It
// reports whether the execution changed and whether it settled.
func (c *Channel) applyLocked(ex *execution, msg messages.OutboundMessage, now time.Time) (changed, settled bool) {
	switch msg.Kind {
	case messages.OutMessageState:
		if msg.MessageState == nil {
			return false, false
		}
		switch msg.MessageState.State {
		case agent.MessageQueued:
			changed = ex.advance(PhaseQueued, now)
		case agent.MessageSubmitted:
			changed = ex.advance(PhaseRunning, now)
		}

	case messages.OutCommandReply:
		// The command answered. The first answer means the message
		// was picked up; a later one (a command that answers after
		// its agent turn) does not move the phase backwards.
		changed = ex.advance(PhaseQueued, now)
		if ex.appendText(msg.Kind, msg.Text, now) {
			changed = true
		}

	case messages.OutReply, messages.OutResult, messages.OutError:
		changed = ex.advance(PhaseRunning, now)
		if ex.appendText(msg.Kind, msg.Text, now) {
			changed = true
		}

	case messages.OutThinking, messages.OutToolStart, messages.OutToolEnd,
		messages.OutTaskCreate, messages.OutTaskUpdate:
		changed = ex.advance(PhaseRunning, now)

	case messages.OutHeartbeat:
		changed = ex.advance(PhaseRunning, now)
		if msg.Heartbeat != nil {
			ex.progress = Progress{
				ThinkCount: msg.Heartbeat.ThinkCount,
				ToolCount:  msg.Heartbeat.ToolCount,
				LastBeatAt: msg.Heartbeat.LastBeatAt,
			}
			changed = true
		}

	case messages.OutChoice:
		changed = ex.advance(PhaseRunning, now)
		if msg.Choice != nil {
			ex.choices = addChoice(ex.choices, *msg.Choice)
			changed = true
		}

	case messages.OutChoicePatch:
		if msg.Choice != nil {
			var patched bool
			ex.choices, patched = patchChoice(ex.choices, *msg.Choice)
			changed = patched
		}

	case messages.OutMessageSettled:
		return c.settleLocked(ex, msg, now)
	}
	return changed, false
}

// settleLocked completes an execution: the phase moves to settled,
// the outcome is copied over, and the waiters are released.
func (c *Channel) settleLocked(ex *execution, msg messages.OutboundMessage, now time.Time) (changed, settled bool) {
	if ex.phase == PhaseSettled {
		return false, false
	}
	ex.phase = PhaseSettled
	ex.settledAt = now
	if msg.Outcome != nil {
		ex.outcome = *msg.Outcome
	}
	if ex.outcome.Status == "" {
		ex.outcome.Status = messages.OutcomeUnknown
	}
	if c.inflight[ex.chatID] == ex.msgID {
		delete(c.inflight, ex.chatID)
	}
	close(ex.settledCh)
	return true, true
}

// addChoice records a pending interactive prompt. A prompt that is
// already known is replaced, so a re-render does not duplicate it.
func addChoice(choices []messages.Choice, choice messages.Choice) []messages.Choice {
	for i := range choices {
		if choices[i].RequestID != "" && choices[i].RequestID == choice.RequestID {
			choices[i] = choice
			return choices
		}
	}
	return append(choices, choice)
}

// patchChoice folds an update of an interactive prompt into the
// pending list.
//
// A patch that names its prompt updates that prompt. A settled
// prompt is answered — it is no longer pending.
//
// A patch that names none is the daemon settling a gate it resolved
// outside the channel (the dashboard): the run has one outstanding
// prompt, and it is answered, so nothing is pending any more.
func patchChoice(choices []messages.Choice, patch messages.Choice) ([]messages.Choice, bool) {
	if patch.RequestID == "" {
		if !patch.Settled || len(choices) == 0 {
			return choices, false
		}
		return nil, true
	}
	for i := range choices {
		if choices[i].RequestID != patch.RequestID {
			continue
		}
		if patch.Settled {
			return append(choices[:i], choices[i+1:]...), true
		}
		choices[i] = patch
		return choices, true
	}
	if patch.Settled {
		return choices, false
	}
	return append(choices, patch), true
}

// notifyLocked hands a fresh snapshot to every watcher.
func (c *Channel) notifyLocked(ex *execution) {
	if len(ex.watchers) == 0 {
		return
	}
	snap := ex.snapshot()
	for _, w := range ex.watchers {
		select {
		case w <- snap:
		default:
			// The watcher is not keeping up. Dropping a progress
			// snapshot is better than stalling the daemon's
			// outbound path.
		}
	}
}

// closeWatchersLocked ends every watch of a settled execution.
func (c *Channel) closeWatchersLocked(ex *execution) {
	for _, w := range ex.watchers {
		close(w)
	}
	ex.watchers = nil
}

// sweepLocked drops the executions nobody needs any more: the ones
// that settled long ago, and the ones that never settled at all.
//
// The sweep runs on every access to the store, so an execution is
// reaped as soon as anything touches the channel again.
func (c *Channel) sweepLocked(now time.Time) {
	for id, ex := range c.execs {
		if ex.phase == PhaseSettled {
			if now.Sub(ex.settledAt) < executionTTL {
				continue
			}
		} else if now.Sub(ex.sentAt) < unsettledTTL {
			continue
		}
		delete(c.execs, id)
		if c.inflight[ex.chatID] == ex.msgID {
			delete(c.inflight, ex.chatID)
		}
	}
}

// lookup returns the live execution for a message id.
func (c *Channel) lookup(msgID string) (*execution, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sweepLocked(time.Now())
	ex, ok := c.execs[msgID]
	return ex, ok
}

// snapshotOf copies the live state of an execution.
func (c *Channel) snapshotOf(ex *execution) Execution {
	c.mu.Lock()
	defer c.mu.Unlock()
	return ex.snapshot()
}

// take settles the account of a read execution: the snapshot is
// returned and the execution leaves the store. A settled execution
// that has been read is not needed again.
func (c *Channel) take(ex *execution) Execution {
	c.mu.Lock()
	defer c.mu.Unlock()
	snap := ex.snapshot()
	delete(c.execs, ex.msgID)
	return snap
}

// peek copies the live state of an execution without consuming it.
func (c *Channel) peek(msgID string) (Execution, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sweepLocked(time.Now())
	ex, ok := c.execs[msgID]
	if !ok {
		return Execution{}, false
	}
	return ex.snapshot(), true
}

// watch registers a watcher on an execution.
//
// The watcher starts from the state the execution is in now, so it
// never has to wait for the next change to learn where the run
// stands. A settled execution has nothing left to report: the
// watcher gets the final snapshot and the channel closes.
func (c *Channel) watch(msgID string) <-chan Execution {
	c.mu.Lock()
	defer c.mu.Unlock()

	ch := make(chan Execution, watchBuffer)
	ex, ok := c.execs[msgID]
	if !ok {
		close(ch)
		return ch
	}
	snap := ex.snapshot()
	if ex.phase == PhaseSettled {
		ch <- snap
		close(ch)
		return ch
	}
	ex.watchers = append(ex.watchers, ch)
	ch <- snap
	return ch
}
