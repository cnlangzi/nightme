package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/channel"
	"github.com/cnlangzi/nightme/internal/config"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/internal/statusbar"
	"github.com/cnlangzi/nightme/internal/stt"
)

type Adapter struct {
	name      string
	api       apiClient
	state     *stateStore
	incoming  chan messages.InboundMessage
	logger    *slog.Logger
	config    config.TelegramConfig
	dataDir   string
	ctx       context.Context
	cancel    context.CancelFunc
	mu        sync.Mutex
	started   bool
	stopped   bool
	offset    int64
	botID     int64
	botName   string
	callbacks map[string]struct{}
	limiter   *Limiter
	retry     RetryConfig

	// muMessageStates guards messageStates. The map stores the last
	// rendered MessageState per userMsgID so duplicate emits of the
	// same state can short-circuit without hitting Telegram's API.
	// Mirrors feishu's messageStates dedup (see
	// internal/channel/feishu/adapter.go).
	muMessageStates sync.Mutex
	messageStates   map[string]agent.MessageState

	// richTurns is the L3 per-turn rich-message index. When
	// RichMode is on, chain-attached kinds (OutThinking / OutTool*
	// / OutTask* / OutError / OutReply-default) accumulate into a
	// single rich message per turn and PATCH via
	// editMessageText(rich_message=...). Chain stays intact for
	// fallback when RichMode is off.
	richTurns *richTurnsIndex

	// draftStreamer (sendMessageDraft path) was retired after
	// observing the animated draft locks the user's send button
	// for the duration of the turn — preventing the user from
	// interjecting while the agent is composing. Both DM and
	// group now flow through liveDraft (simulated
	// DraftMessage: real rich message + editMessageText /
	// deleteMessage), which leaves the user free to type.
	//
	// liveDraft simulates the sendMessageDraft surface for
	// every non-channel ChatKind via a real Telegram message +
	// editMessageText / deleteMessage trio. Per-turn entries
	// live in memory only — there is no persisted DraftMessageID
	// on TopicState; a daemon restart mid-turn leaves the
	// in-flight DraftMessage orphaned in chat (acceptable since
	// the user is unlikely to resume the same turn, and the next
	// turn creates a fresh DraftMessage). See live_draft.go for
	// the lifecycle contract.
	liveDraft *liveDraftManager

	// voiceHandler is the seam to the local nightme-stt worker
	// (set via SetVoiceHandler at startup). When non-nil,
	// handleMessage routes incoming Voice attachments through
	// it before publishing to the agent, so the agent sees a
	// text transcript instead of raw opus bytes. nil means
	// voice messages are passed through as audio attachments
	// (legacy behaviour; new deployments always wire one).
	voiceHandler *VoiceHandler
}

func NewAdapter(cfg *config.Config) (*Adapter, error) {
	if cfg == nil {
		return nil, errors.New("telegram: config is nil")
	}
	botToken := strings.TrimSpace(cfg.Telegram.BotToken)
	if botToken == "" {
		return nil, errors.New("telegram: bot_token is required")
	}
	timeout := cfg.Telegram.PollingTimeout
	if timeout <= 0 {
		timeout = 30
	}
	if timeout > 50 {
		timeout = 50
	}
	cfgCopy := cfg.Telegram
	cfgCopy.PollingTimeout = timeout
	dataDir := cfg.Paths.DataDir
	if dataDir == "" {
		dataDir = os.TempDir()
	}
	state, err := newStateStore(filepath.Join(dataDir, "telegram_state.json"))
	if err != nil {
		return nil, fmt.Errorf("telegram: load state: %w", err)
	}
	out := &Adapter{
		name:      "telegram",
		api:       newHTTPClient(botToken),
		state:     state,
		incoming:  make(chan messages.InboundMessage, 128),
		logger:    slog.Default(),
		config:    cfgCopy,
		dataDir:   dataDir,
		callbacks: make(map[string]struct{}),
		limiter:   NewLimiter(nil, slog.Default()),
		retry:     DefaultRetryConfig,
		richTurns: newRichTurnsIndex(defaultRichTurnCap),
	}
	out.liveDraft = newLiveDraftManager(out.api, out.logger)
	out.wireVoiceHandler()
	return out, nil
}

// wireVoiceHandler constructs the local STT manager and voice
// handler. Best-effort: a missing nightme-stt binary or
// unreadable data dir logs and silently no-ops, so the adapter
// still serves non-voice traffic (the failure surfaces as a
// per-message "🎙 run nightme stt install" notice from the
// voice handler itself, not at adapter construction).
//
// The manager's spawner only fails when first asked to spawn
// (lazy), so wiring it at startup costs nothing when nightme-stt
// is absent — the first Voice message pays the lookup cost.
func (a *Adapter) wireVoiceHandler() {
	if a.dataDir == "" || a.dataDir == os.TempDir() {
		// dataDir is unset / fell back to /tmp — no place to
		// put the nightme-stt binary. Skip silently; the
		// legacy attachment-passthrough path remains in
		// place.
		return
	}
	ep, err := stt.DefaultEndpoint(a.dataDir)
	if err != nil {
		if a.logger != nil {
			a.logger.Warn("telegram: stt endpoint unavailable; voice transcription disabled",
				"err", err, "data_dir", a.dataDir)
		}
		return
	}
	manager, err := stt.NewManager(stt.DefaultTransport(),
		stt.ProductionSpawner(a.dataDir), ep)
	if err != nil {
		if a.logger != nil {
			a.logger.Warn("telegram: stt manager unavailable; voice transcription disabled",
				"err", err)
		}
		return
	}
	a.voiceHandler = NewVoiceHandler(manager, a.logger)
}

func NewAdapterWithClient(cfg *config.Config, api apiClient, dataDir string) *Adapter {
	if cfg == nil {
		cfg = &config.Config{}
	}
	if dataDir == "" {
		dataDir = os.TempDir()
	}
	state, err := newStateStore(filepath.Join(dataDir, "telegram_state.json"))
	if err != nil {
		state = &stateStore{topics: make(map[string]*TopicState), choices: make(map[string]*ChoiceState)}
	}
	copy := cfg.Telegram
	if copy.PollingTimeout == 0 {
		copy.PollingTimeout = 30
	}
	if api == nil {
		api = newHTTPClient("test-token")
	}
	return &Adapter{
		name:      "telegram",
		api:       api,
		state:     state,
		incoming:  make(chan messages.InboundMessage, 128),
		logger:    slog.Default(),
		config:    copy,
		dataDir:   dataDir,
		callbacks: make(map[string]struct{}),
		limiter:   NewLimiter(nil, slog.Default()),
		retry:     DefaultRetryConfig,
		richTurns: newRichTurnsIndex(defaultRichTurnCap),
		liveDraft: newLiveDraftManager(api, slog.Default()),
	}
}

func (a *Adapter) Name() string { return a.name }

func (a *Adapter) Incoming() <-chan messages.InboundMessage { return a.incoming }

func (a *Adapter) SetLogger(logger *slog.Logger) {
	if logger == nil {
		return
	}
	a.logger = logger
	if a.limiter != nil {
		a.limiter.logger = logger
	}
}

// SetVoiceHandler wires the local-nightme-stt voice transcription
// seam. When set, handleMessage routes incoming Voice attachments
// through handler.HandleVoice before publishing to the agent.
// Idempotent: subsequent calls replace the previous handler.
func (a *Adapter) SetVoiceHandler(h *VoiceHandler) {
	a.mu.Lock()
	a.voiceHandler = h
	a.mu.Unlock()
}

func (a *Adapter) Start(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	a.mu.Lock()
	if a.stopped {
		a.mu.Unlock()
		return errors.New("telegram: adapter is stopped")
	}
	if a.started {
		a.mu.Unlock()
		return nil
	}
	a.started = true
	a.ctx, a.cancel = context.WithCancel(ctx)
	a.mu.Unlock()

	var me UserInfo
	if err := a.api.call(a.ctx, "getMe", nil, &me); err != nil {
		a.Stop(context.Background())
		return err
	}
	a.mu.Lock()
	a.botID = me.ID
	a.botName = me.Username
	a.mu.Unlock()
	if a.botName == "" {
		a.Stop(context.Background())
		return errors.New("telegram: getMe returned empty username")
	}
	go a.pollLoop(a.ctx)
	a.logger.Info("telegram: started", "username", a.botName)
	return nil
}

func (a *Adapter) Stop(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	a.mu.Lock()
	if !a.started && !a.stopped {
		a.stopped = true
		close(a.incoming)
		a.mu.Unlock()
		return nil
	}
	if a.stopped {
		a.mu.Unlock()
		return nil
	}
	a.stopped = true
	cancel := a.cancel
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	done := make(chan struct{})
	go func() {
		a.mu.Lock()
		close(a.incoming)
		a.mu.Unlock()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		return ctx.Err()
	}
	a.logger.Info("telegram: stopped", "username", a.botName)
	return nil
}

// pollLoopGuard is a process-wide singleton gate. If a buggy
// runtime path somehow creates and Starts multiple Adapter
// instances (we've seen this in the wild — see the 409-Conflict
// diagnosis in the fix-telegram branch), each one would spawn
// its own pollLoop and race Telegram's getUpdates long-poll
// slot, causing perpetual 409s. The first pollLoop to acquire
// this guard wins; all subsequent ones exit immediately. The
// underlying multi-Adapter bug is still there (root cause TBD),
// but this stops the runtime symptom and lets the daemon
// actually deliver messages in the meantime.
var pollLoopGuard sync.Once

func (a *Adapter) pollLoop(ctx context.Context) {
	started := false
	pollLoopGuard.Do(func() { started = true })
	if !started {
		a.logger.Error("telegram: pollLoop already running in this process; "+
			"this adapter is a duplicate. Suppressing to avoid 409 Conflict. "+
			"Investigate why runtime created >1 Adapter for telegram.",
			"this_adapter", fmt.Sprintf("%p", a))
		return
	}
	for {
		// Honour cancellation at the top of every iteration,
		// not just inside the err-handling branches below. A
		// successful api.call that races with Stop() would
		// otherwise loop forever — the daemon's Stop() fires
		// a.cancel(), ctx is done, but the loop's only exit
		// paths are inside the err branch and the (rare)
		// apiErr.RetryAfter branch. This guard is what lets
		// Stop() actually drain the goroutine (PR #224
		// windows-latest job 95925744882 was OOMing because
		// fakeAPI in tests didn't propagate ctx and the
		// loop spun forever).
		if err := ctx.Err(); err != nil {
			return
		}
		// telegram's getUpdates response has `result` as a JSON
		// ARRAY of Update objects. Unmarshal directly into []Update
		// rather than into a wrapper struct (api.go's `call`
		// already decoded the envelope; this decodes the inner
		// array via envelope.Result).
		var updates []Update
		err := a.api.call(ctx, "getUpdates", map[string]any{
			"offset":  a.offset,
			"limit":   100,
			"timeout": a.config.PollingTimeout,
			// 仅订阅正常消息与交互按钮回调：所有其它 update 类型
			//（message_reaction / message_reaction_count / chat_member /
			// my_chat_member / edited_message / channel_post 等）一律不下发。
			"allowed_updates": []string{"message", "callback_query"},
		}, &updates)
		if err != nil {
			var apiErr *apiError
			if errors.As(err, &apiErr) {
				if apiErr.RetryAfter > 0 {
					time.Sleep(time.Duration(apiErr.RetryAfter) * time.Second)
					continue
				}
			}
			if ctx.Err() == nil {
				a.logger.Warn("telegram: getUpdates failed", "err", err)
			}
			if !sleepContext(ctx, time.Second) {
				return
			}
			continue
		}
		for _, update := range updates {
			a.offset = update.UpdateID + 1
			a.handleUpdate(ctx, update)
		}
	}
}

func (a *Adapter) handleUpdate(ctx context.Context, update Update) {
	if update.Message != nil {
		a.handleMessage(ctx, update.Message)
		return
	}
	if update.CallbackQuery != nil {
		a.handleCallbackQuery(ctx, update.CallbackQuery)
	}
}

func (a *Adapter) handleMessage(ctx context.Context, message *Message) {
	if message == nil || message.Chat.ID == 0 {
		return
	}
	a.mu.Lock()
	botID := a.botID
	a.mu.Unlock()
	if message.From != nil && botID != 0 && message.From.ID == botID {
		return
	}
	text := message.Text
	if text == "" {
		text = message.Caption
	}
	// Drop bare /start before any state mutation. Telegram's
	// `/start` is a platform convention for "begin a conversation" —
	// forwarding it to the agent would publish a "🤖 Working..."
	// placeholder and produce a long reply explaining the command
	// isn't registered, neither of which the user wants. Variants
	// like `/start foo` flow through normally.
	if isBareStartCommand(text, a.botName) {
		if a.logger != nil {
			a.logger.Debug("telegram: dropped bare /start",
				"chat_id", message.Chat.ID,
				"message_id", message.MessageID,
			)
		}
		return
	}
	hasMention := a.hasMention(message, text)
	chatID := strconv.FormatInt(message.Chat.ID, 10)
	threadID, err := a.ensureTopic(ctx, message)
	if err != nil {
		a.logger.Warn("telegram: ensure topic failed", "chat_id", chatID, "err", err)
		return
	}
	// Placeholder ("Working...") anchors every turn's reply chain.
	// Real Telegram topics (thread_id > 0) and DM / main-window
	// (thread_id == 0) both use it; in the latter case the
	// TopicState is keyed by chatID with TopicID=0 and the
	// placeholder is later used as reply_to_message_id for all
	// OutXxx bubbles (see docs/channel/telegram.md §11.11).
	if err := a.ensurePlaceholder(ctx, chatID, threadID, message.MessageID, message); err != nil {
		a.logger.Warn("telegram: ensure placeholder failed", "chat_id", chatID, "thread_id", threadID, "err", err)
	}
	// (no StatusBar cache to reset — see §18; the renderer is
	// a pure consumer of msg fields stamped by runtime /
	// chatsession.)
	// Adapt the rest of the function to thread_id / state-key
	// variables rather than the legacy topicID name.
	topicID := threadID
	downloadRes := a.downloadAttachments(ctx, message, chatID)
	attachments := downloadRes.Atts
	// F-61 parity with feishu: when all downloads fail, surface a
	// user-visible note so the user knows their image was lost
	// (rather than watching the bot silently produce a text-only
	// reply). Pure-image messages (text == "") are dropped —
	// feishu does the same — because there's nothing left to send.
	if downloadRes.AllFailed {
		if a.logger != nil {
			a.logger.Warn("telegram: all attachment retries exhausted",
				"chat_id", chatID,
				"message_id", message.MessageID,
				"failed_count", downloadRes.FailureCount,
				"attempts", downloadRetryConfig.MaxAttempts,
			)
		}
		a.notifyDownloadFailure(chatID, topicID, downloadRes, text == "")
		if text == "" {
			return
		}
		attachments = nil
	} else if downloadRes.FailureCount > 0 {
		if a.logger != nil {
			a.logger.Info("telegram: partial attachment download failure; sending the rest",
				"chat_id", chatID,
				"message_id", message.MessageID,
				"failed_count", downloadRes.FailureCount,
				"succeeded_count", len(downloadRes.Atts)-downloadRes.FailureCount,
				"attempts", downloadRetryConfig.MaxAttempts,
			)
		}
		a.notifyDownloadFailure(chatID, topicID, downloadRes, false)
	}
	// Voice transcription (issue #381 / docs/channel/telegram.md
	// §21): when a Voice attachment is present and a voice
	// handler is wired, run the audio through nightme-stt before
	// the agent sees it. The transcript replaces the inbound
	// text (or augments the caption when one is present) so the
	// agent receives a clean text prompt; the voice attachment
	// is stripped from the inbound.Attachments slice so the
	// agent never sees raw opus bytes it couldn't consume.
	if message.Voice != nil {
		voiceFile := findVoiceAttachment(downloadRes.Atts)
		if voiceFile == "" {
			// Download failed or no matching att — the
			// failure path above already notified the
			// user. Drop the voice attachment from the
			// list so the agent doesn't try to consume
			// an empty / failed file.
			attachments = dropVoiceAttachment(attachments)
		} else if a.voiceHandler != nil {
			outcome := a.voiceHandler.HandleVoice(ctx, message, voiceFile)
			attachments = dropVoiceAttachment(attachments)
			if outcome.Drop {
				if outcome.Text != "" {
					if text == "" {
						text = outcome.Text
					} else {
						text = text + "\n\n" + outcome.Text
					}
				}
			}
			if outcome.Err != nil {
				notice := voiceFailureText(outcome)
				if notice != "" {
					if err := a.Send(context.Background(), messages.OutboundMessage{
						ChatID: a.sessionChatID(chatID, topicID),
						Kind:   messages.OutError,
						Text:   notice,
					}); err != nil && a.logger != nil {
						a.logger.Warn("telegram: voice failure notice failed",
							"chat_id", chatID, "err", err)
					}
				}
				if a.logger != nil {
					a.logger.Warn("telegram: voice transcription failed",
						"chat_id", chatID,
						"message_id", message.MessageID,
						"err", outcome.Err,
					)
				}
			}
		}
	}
	// (UserMessageID is already updated by ensurePlaceholder above;
	// this redundant block was removed in the 2026-08-22 plan-C
	// revision since ensurePlaceholder now persists state.)
	if a.handleForceReply(ctx, message) {
		return
	}
	inbound := messages.InboundMessage{
		ChatID:      a.sessionChatID(chatID, topicID),
		UserID:      userID(message),
		Text:        text,
		Attachments: attachments,
		ReplyTo:     replyToID(message),
		MessageID:   strconv.Itoa(message.MessageID),
		Time:        time.Unix(message.Date, 0).UTC(),
		Raw:         message,
		HasMention:  hasMention,
	}
	if inbound.Blocks == nil {
		inbound.Blocks = a.BuildBlocks(text, attachments)
	}
	a.logger.Info("telegram: incoming",
		"chat_id", chatID,
		"chat_type", message.Chat.Type,
		"thread_id", topicID,
		"user_id", userID(message),
		"has_mention", hasMention,
		"message_id", message.MessageID,
		"text_len", len(text),
	)
	a.publish(inbound)
}

func (a *Adapter) publish(inbound messages.InboundMessage) {
	select {
	case a.incoming <- inbound:
	case <-a.ctxDone():
		// Adapter is shutting down — drop silently. This is the
		// expected race window when Stop() is called mid-publish.
		a.logger.Warn("telegram: publish dropped (adapter stopping)",
			"chat_id", inbound.ChatID, "user_msg_id", inbound.MessageID)
	default:
		// incoming channel is full (buffer=128). Runtime is not
		// draining Inbound — likely a chatsession pump stall.
		// Surface this loudly; otherwise the user sees nothing.
		a.logger.Warn("telegram: publish dropped (incoming channel full)",
			"chat_id", inbound.ChatID, "user_msg_id", inbound.MessageID,
			"buffer_size", cap(a.incoming))
	}
}

func (a *Adapter) ctxDone() <-chan struct{} {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ctx == nil {
		return closedChannel()
	}
	return a.ctx.Done()
}

func closedChannel() <-chan struct{} {
	channel := make(chan struct{})
	close(channel)
	return channel
}

// ensureTopic returns the thread_id a Telegram update should be
// routed to. It is a pure function of the incoming message — no
// daemon state, no config, no sentinel topic creation.
//
// Behavior:
//   - private chat (DM)                  → (0, nil); TopicState for
//     topic_id=0 is created on demand by ensurePlaceholder so the
//     DM can carry the same placeholder + reply-chain UX as a real
//     topic (see docs/channel/telegram.md §11.11).
//   - message in a real Telegram topic   → (message.MessageThreadID, nil);
//     TopicState is created/updated here so subsequent updates in
//     the same topic resolve to the same ChatSession.
//   - group / supergroup main window     → (0, nil); same DM-style
//     placeholder path.
//
// We no longer auto-create a "nightme" sentinel topic for the
// main window (deleted in the 2026-08 refactor). The chatID for
// a main-window message is "tg_<chatid>" (no thread suffix), and
// replies land directly in the main window. This makes chatID a
// stable function of (chat.id, thread_id) — see docs/CHANNEL.md
// §5.5 for the stability contract.
func (a *Adapter) ensureTopic(_ context.Context, message *Message) (int, error) {
	if message == nil {
		return 0, nil
	}
	if message.MessageThreadID > 0 {
		// Persist the (chat_id, thread_id) pair so the next
		// message in the same Telegram topic can do lookups
		// against the same TopicState. Idempotent on repeat.
		chatID := strconv.FormatInt(message.Chat.ID, 10)
		if _, ok := a.state.topic(chatID, message.MessageThreadID); !ok {
			if err := a.state.putTopic(&TopicState{
				ChatID:    chatID,
				TopicID:   message.MessageThreadID,
				ChatKind:  ClassifyChat(message.Chat.Type),
				CreatedAt: time.Now().UTC(),
				UpdatedAt: time.Now().UTC(),
			}); err != nil {
				return 0, err
			}
			a.logger.Debug("telegram: topic created",
				"chat_id", chatID,
				"thread_id", message.MessageThreadID,
				"trigger_message_id", message.MessageID,
			)
		}
		return message.MessageThreadID, nil
	}
	// DM / main-window: thread_id is 0. ensurePlaceholder is
	// responsible for materialising a TopicState{topicID: 0}
	// carrying the placeholder message id (see handleMessage).
	// Returning (0, nil) here keeps chatID stable ("tg_<chatid>")
	// and lets the placeholder path own the stateStore write.
	return 0, nil
}

// ensurePlaceholder materialises the per-turn placeholder
// message and pins the user-message anchor for this turn.
//
// L3 (§20.6.3): the rich turn replaces the v9 chain's first
// chunk. Cold-create behaviour is "create an empty rich message
// anchored to the user's message"; the first OutHeartbeat sets
// the header (visible "🤖 Working..." or active state); subsequent
// Out* events append entries to the same rich message via
// editMessageText(rich_message=...). No "Working..." banner ships
// eagerly — the header only appears when there's actually a
// heartbeat to show, which is cleaner than v9's eager banner.
//
// Each user message triggers a NEW rich turn. The previous turn's
// rich message is left untouched (it was already PATCHed with
// the 🎉 reaction by OnPromptEnded and stays in the Telegram
// timeline as that turn's permanent status marker).
//
// OutXxx bubbles carry `reply_to_message_id = userMsgID` so the
// reply chain hangs under the user's own message ("hi"), not
// under the rich message. The rich message is the turn's status
// ticker (heartbeat header → 🎉 stamp), not the reply anchor.
// See docs/channel/telegram.md §11.11.
func (a *Adapter) ensurePlaceholder(ctx context.Context, chatID string, topicID, userMessageID int, message *Message) error {
	state, ok := a.state.topic(chatID, topicID)
	if !ok {
		state = &TopicState{ChatID: chatID, TopicID: topicID, ChatKind: ChatKindGroup, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	}
	// Refresh ChatKind on every turn so Send() routes correctly
	// even if chat.type changes (e.g. user invites bot to a forum).
	// Both ChatKindPrivate and ChatKindGroup share the same
	// liveDraft (simulated DraftMessage) path.
	if message != nil && message.Chat.Type != "" {
		state.ChatKind = ClassifyChat(message.Chat.Type)
	}
	// Per-turn liveDraft reset for the group ChatKind. Private
	// (chat.type=="private") has no persisted DraftMessageID to
	// recover from either — the in-memory liveDraft entry from a
	// prior turn, if any, is dropped below by endProcess in
	// ensurePlaceholder's prior-turn cleanup. The next OutThinking
	// in this turn cold-creates a fresh DraftMessage via
	// streamDraftEvent → liveDraft.streamDraftEvent.
	//
	// Orphan recovery: if the previous turn left a DraftMessage
	// on disk (daemon crashed mid-turn, or the endProcess
	// deleteMessage failed and state still carries the id),
	// delete it synchronously before clearing the state. Blocking
	// the inbound for the ~100ms the delete takes is fine —
	// ensurePlaceholder already fires the rich turn cold-create
	// (a sendMessage) on the next line, so the user is paying
	// an API round-trip anyway. Failure leaves the orphan; apiCall
	// already retried transient errors, so a permanent failure
	// here means the message is stuck (e.g. revoked bot perms).
	if state.ChatKind == ChatKindGroup {
		// No persisted DraftMessageID to recover from: the liveDraft
		// is purely in-memory per-turn state. A daemon crash mid-turn
		// leaves any in-flight DraftMessage orphaned in the chat —
		// acceptable since the user is unlikely to resume the same
		// turn, and endProcess's deleteMessage would have cleaned it
		// up on the happy path.
	}
	// ChatKindPrivate is handled the same way: the simulated
	// DraftMessage lives in liveDraft, not in a per-chat-kind
	// streamer. No additional per-kind cleanup needed here.

	// Drop any in-memory rich turn for this turn — the previous
	// turn's rich message stays in Telegram chat (no further edits)
	// but we don't track it anymore. Next Out* gets a fresh rich
	// turn. Mirrors v9 chain's purge semantics.
	a.richTurns.purge(chatID, topicID, userMessageID)

	// Draft streamer is per-(chat, thread, userMsgID) — the prior
	// turn's streamer was evicted by the cleanup above (or by the
	// production endProcess calls). The next Out* event for this
	// turn allocates a fresh streamer with a fresh draft_id, so
	// the server-side draft surface is unambiguous per turn.

	// Eager placeholder create: send the rich message right now
	// (per turn) instead of waiting for the first Out* event. The
	// user gets immediate visual feedback that the bot received
	// the message and is processing it — same UX as Feishu's OnIt
	// reaction. Without this, slow agent turns (e.g., multi-step
	// reasoning before any tool call) show nothing for many
	// seconds and the user wonders if the bot is alive.
	//
	// cold-create is idempotent — Telegram rejects empty blocks
	// with RICH_MESSAGE_EMPTY, so we send a single empty
	// paragraph block as the placeholder body. The header
	// (heartbeat line) is added later via renderRichTurnBlocksLocked
	// only when the runtime emits an OutHeartbeat with a valid
	// LastBeatAt — see patchChainHeader's Empty() guard. Before
	// that, the placeholder card just shows entries as they
	// arrive (no premature "🤖 Working…" banner).
	turn := a.richTurns.getOrCreate(chatID, topicID, userMessageID)
	turn.mu.Lock()
	turn.headerLine = defaultRichTurnHeader
	if state.ChatKind != ChatKindPrivate {
		// Non-DM: cold-create the rich turn placeholder so the
		// chain path has something to edit from turn start.
		if err := a.sendRichTurnColdCreate(turn); err != nil {
			a.logger.Warn("telegram: eager placeholder cold-create failed",
				"chat_id", chatID,
				"err", err)
			// Fall through — first Out* event will retry cold-create
			// via appendRichTurn's lazy path.
		}
	} else {
		// DM: skip rich turn cold-create. The draft IS the live
		// surface for think/tool events; an empty rich turn
		// placeholder would sit in the chat as a useless "🤖
		// Working..." real message alongside the draft. Any
		// subsequent Out* event that genuinely needs a real
		// message (OutReply / OutResult / OutError) will lazily
		// cold-create its own rich message via appendRichTurn's
		// getOrCreate path — at that point there's actual
		// content to render.
		a.logger.Debug("telegram: skipping rich turn placeholder in DM (draft is live surface)",
			"chat_id", chatID)
	}
	turn.mu.Unlock()

	// The state.PlaceholderMessageID is intentionally not set here:
	// L3 derives the 🎉 anchor from richTurn.messageID at
	// OnPromptEnded time. The legacy field is preserved as
	// read-only (downstream consumers may still inspect it; v9 P2
	// §11.12.10 already documented this).

	state.LastMessageID = 0
	state.UserMessageID = strconv.Itoa(userMessageID)
	a.logger.Debug("telegram: rich turn initialised (eager)",
		"chat_id", chatID,
		"thread_id", topicID,
		"user_message_id", userMessageID,
		"placeholder_message_id", turn.messageID,
	)
	return a.state.putTopic(state)
}

// placeholderInitialText was the cold-create header formatter;
// removed 2026-08-23 in favour of heartbeatText(nil) — both
// produce identical output (`<b>🤖 Working...</b>`) and there
// is no point keeping two functions in sync. Cold-create
// callers (ensurePlaceholder / appendSegment case-1 /
// patchChainHeader fallback) now invoke heartbeatText(nil)
// directly.
//
// ensurePlaceholderForHeartbeat was removed on 2026-08-23 in
// favour of the eager ensurePlaceholder-in-handleMessage path
// + the Compose header-skip rule (§11.12.5). The lifecycle is:
//   - handleMessage (per incoming user msg) calls
//     ensurePlaceholder synchronously, before publishing the
//     inbound. state.UserMessageID + state.PlaceholderMessageID
//     are populated atomically with the chain entry, so any Out*
//     that the runtime eventually emits finds a ready chain.
//   - chunkBody.hasHeartbeat stays false for cold-create and
//     for any non-agent turn (slash command reply, error path,
//     reaction-only click). Compose's
//     "render header iff hasHeartbeat || entries==empty" rule
//     is what hides the legacy frozen-banner bug — see
//     §11.12.5.
//
// What this removed: the lazy bootstrap attempt from Send()
// (create placeholder on demand if handleMessage hadn't
// shipped yet) was racy by design and got superseded by
// simply not being needed.

// downloadAttachments collects the inbound message's media sources
// and downloads them with an outer retry ladder. Returns a
// downloadResult so the caller can distinguish "nothing to download"
// from "all attempts failed" and surface the right user-facing
// notification.
//
// F-61 parity with feishu: outer ladder is 3 attempts with
// 0s / 5s / 15s backoff between them. The inner per-attachment
// retry in downloadAttachment (maxDownloadAttempts) catches
// transient transport blips; this outer ladder catches ctx
// cancellations and infra failures that span the whole message.
func (a *Adapter) downloadAttachments(ctx context.Context, message *Message, chatID string) downloadResult {
	sources := a.collectAttachmentSources(message)
	if len(sources) == 0 {
		return downloadResult{}
	}
	var last downloadResult
	for attempt := 1; attempt <= downloadRetryConfig.MaxAttempts; attempt++ {
		if attempt > 1 {
			wait := downloadRetryConfig.Backoffs[attempt-1]
			if wait > 0 {
				timer := time.NewTimer(wait)
				select {
				case <-timer.C:
				case <-ctx.Done():
					timer.Stop()
					return last
				}
			}
		}
		last = a.downloadSourcesOnce(ctx, sources, chatID, message.MessageID)
		if !last.AllFailed {
			return last
		}
	}
	return last
}

// collectAttachmentSources walks the inbound Telegram message and
// builds one attachmentSource per downloadable resource. Photos /
// Voice / Audio / Video are categorised by their envelope field;
// Document is classified by MIME via telegramAttachmentType since
// Telegram's document envelope doesn't carry msg_type metadata.
func (a *Adapter) collectAttachmentSources(message *Message) []attachmentSource {
	var sources []attachmentSource
	if len(message.Photo) > 0 {
		sources = append(sources, attachmentSource{
			FileID:   message.Photo[len(message.Photo)-1].FileID,
			Name:     "image.jpg",
			MimeType: "image/jpeg",
			Type:     "image",
		})
	}
	if message.Document != nil {
		sources = append(sources, attachmentSource{
			FileID:   message.Document.FileID,
			Name:     message.Document.FileName,
			MimeType: message.Document.MimeType,
			Type:     telegramAttachmentType(message.Document.MimeType),
		})
	}
	if message.Audio != nil {
		sources = append(sources, attachmentSource{
			FileID:   message.Audio.FileID,
			Name:     message.Audio.FileName,
			MimeType: message.Audio.MimeType,
			Type:     "audio",
		})
	}
	if message.Voice != nil {
		sources = append(sources, attachmentSource{
			FileID:   message.Voice.FileID,
			Name:     "voice.ogg",
			MimeType: message.Voice.MimeType,
			Type:     "audio",
		})
	}
	if message.Video != nil {
		sources = append(sources, attachmentSource{
			FileID:   message.Video.FileID,
			Name:     message.Video.FileName,
			MimeType: message.Video.MimeType,
			Type:     "media",
		})
	}
	return sources
}

// downloadSourcesOnce performs one outer attempt: every source
// gets a single download, failures are recorded with the original
// source metadata so the downstream can retry-merge without
// losing Type / Name / MimeType hints.
func (a *Adapter) downloadSourcesOnce(ctx context.Context, sources []attachmentSource, chatID string, messageID int) downloadResult {
	atts := make([]messages.Attachment, 0, len(sources))
	failedCount := 0
	for _, source := range sources {
		att, err := a.downloadAttachment(ctx, source, chatID, messageID)
		if err != nil {
			atts = append(atts, messages.Attachment{
				Type:     source.Type,
				Name:     source.Name,
				MimeType: source.MimeType,
				Error:    err,
			})
			failedCount++
			continue
		}
		atts = append(atts, att)
	}
	return downloadResult{
		Atts:         atts,
		AllFailed:    failedCount == len(sources) && len(sources) > 0,
		FailureCount: failedCount,
	}
}

// notifyDownloadFailure emits a user-visible note about an
// attachment download failure. Uses a fresh background ctx because
// the inbound ctx may be cancelled by the time the retry ladder
// runs out (matches feishu's F-61 fix #3 — reusing inbound ctx was
// the 2026-08-12 silent-drop incident root cause).
//
// pureImage flips the wording: a text-bearing message degrades to
// text-only ("⚠️ ... sending text only") while a pure-image message
// is dropped entirely ("❌ ... please retry").
func (a *Adapter) notifyDownloadFailure(rawChatID string, topicID int, res downloadResult, pureImage bool) {
	// Send → appendRichTurn → flushRichTurn all run on their own
	// background ctx; the inbound ctx is already past its use-by
	// date by the time the retry ladder gives up. Pass
	// context.Background() so the rich turn's debounce + cold-
	// create don't see a half-cancelled ctx.
	sessionChatID := a.sessionChatID(rawChatID, topicID)
	var text string
	if pureImage {
		text = fmt.Sprintf("❌ %d attachment(s) failed to download after %d attempts. Message dropped — please retry.",
			res.FailureCount, downloadRetryConfig.MaxAttempts)
	} else {
		text = fmt.Sprintf("⚠️ %d attachment(s) failed to download after %d attempts; sending text only.",
			res.FailureCount, downloadRetryConfig.MaxAttempts)
	}
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: sessionChatID,
		Kind:   messages.OutError,
		Text:   text,
	}); err != nil && a.logger != nil {
		a.logger.Warn("telegram: notify download failure failed",
			"chat_id", sessionChatID, "err", err)
	}
}

// telegramAttachmentType maps a Telegram document MIME to the
// channel-native Attachment.Type vocabulary that the chatsession
// manager's unified fallback path branches on (see
// internal/chatsession/manager.go — only "image" gets the special
// ContentImage block; everything else is ContentFile).
//
// Photos / Voice / Audio / Video attach sites hard-code their own
// Type because the Telegram message envelope already discriminates
// them; this helper exists only for Document, which can carry any
// MIME and therefore needs to be classified by content.
//
// MIME comparisons are case-insensitive — Telegram clients today
// emit lower-case, but the RFC allows any case, and getting this
// wrong means a document image ("Image/PNG") falls through to the
// generic "file" branch and the bridge encodes it as a
// non-multimodal ContentFile block. Anthropic API rejects that
// payload for image/* MIME, surfacing as a confusing bridge error.
func telegramAttachmentType(mimeType string) string {
	lower := strings.ToLower(mimeType)
	switch {
	case strings.HasPrefix(lower, "image/"):
		return "image"
	case strings.HasPrefix(lower, "audio/"):
		return "audio"
	case strings.HasPrefix(lower, "video/"):
		return "media"
	default:
		return "file"
	}
}

// downloadResult aggregates the outcome of attempting to download
// every attachment on an inbound message. Mirrors feishu's
// attachment.DownloadResult so the caller's notification logic can
// distinguish "no attachments" / "all-failed" / "partial-failure"
// and surface the right user-facing message.
type downloadResult struct {
	// Atts has one entry per source. LocalPath is populated on
	// success; Error is populated on failure. Both cannot be set.
	Atts []messages.Attachment
	// AllFailed is true iff every source failed. The caller should
	// either drop a pure-image message or degrade a text-bearing
	// one to text-only + warn the user.
	AllFailed bool
	// FailureCount is the number of sources that failed in this
	// attempt. Used by the user-facing notification ("3 attachment(s)
	// failed to download") — no need to carry raw FileIDs.
	FailureCount int
}

// downloadRetryConfig controls the outer ladder wrapping
// downloadAttachments. Matches feishu's downloadRetryConfig in
// attachment.go:402 — 3 attempts, 0s / 5s / 15s backoff. The
// inner per-attachment retry (downloadOneWithRetry in feishu,
// downloadAttachment's single-shot here — kept simple for now)
// catches transport blips; this outer ladder catches longer
// failures that span the whole message.
var downloadRetryConfig = struct {
	MaxAttempts int
	Backoffs    []time.Duration
}{
	MaxAttempts: 3,
	Backoffs:    []time.Duration{0, 5 * time.Second, 15 * time.Second},
}

type attachmentSource struct {
	FileID   string
	Name     string
	MimeType string
	// Type is the channel-native category (mirrors Feishu's
	// "image" / "file" / "audio" / "media" vocab so the unified
	// fallback path in chatsession.Manager.HandleInbound can
	// classify the block correctly even when the channel-side
	// BuildBlocks is bypassed).
	Type string
}

func (a *Adapter) downloadAttachment(ctx context.Context, source attachmentSource, chatID string, messageID int) (messages.Attachment, error) {
	filePath, err := a.downloadTelegramFile(ctx, source.FileID)
	if err != nil {
		return messages.Attachment{}, err
	}
	data, err := a.api.download(ctx, filePath)
	if err != nil {
		return messages.Attachment{}, err
	}
	directory := filepath.Join(a.dataDir, "telegram", chatID, strconv.Itoa(messageID))
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return messages.Attachment{}, err
	}
	name := filepath.Base(source.Name)
	if name == "." || name == "" {
		name = "attachment"
	}
	localPath := filepath.Join(directory, name)
	if err := os.WriteFile(localPath, data, 0o600); err != nil {
		return messages.Attachment{}, err
	}
	return messages.Attachment{LocalPath: localPath, MimeType: source.MimeType, Name: name, FileKey: source.FileID, FileName: name, Type: source.Type}, nil
}

func (a *Adapter) hasMention(message *Message, text string) bool {
	if message == nil || message.Chat.Type == "private" {
		return true
	}
	if message.ReplyToMessage != nil && message.ReplyToMessage.From != nil && message.ReplyToMessage.From.ID == a.botID {
		return true
	}
	if strings.HasPrefix(strings.TrimSpace(text), "/") {
		return true
	}
	return strings.Contains(strings.ToLower(text), "@"+strings.ToLower(a.botName))
}

// isBareStartCommand reports whether text is exactly `/start` or
// `/start@<botusername>` (case-insensitive). Telegram's `/start` is a
// platform convention for "begin a conversation" — it has no
// meaning to nightme's command set, and forwarding it to the agent
// produces a spurious "🤖 Working..." placeholder plus a long reply
// explaining the command isn't registered. Drop it silently at the
// adapter boundary so neither side burns time on it. Variants with
// extra text (`/start foo`) are NOT matched — those are real
// user prompts and should flow through normally.
func isBareStartCommand(text, botUsername string) bool {
	t := strings.TrimSpace(text)
	if t == "" {
		return false
	}
	low := strings.ToLower(t)
	if low == "/start" {
		return true
	}
	if botUsername == "" {
		return false
	}
	return low == "/start@"+strings.ToLower(botUsername)
}

func (a *Adapter) sendChoice(ctx context.Context, msg messages.OutboundMessage, placeholderAnchor int) error {
	if msg.Choice == nil || msg.Choice.RequestID == "" {
		return errors.New("telegram: OutChoice missing Choice or RequestID")
	}
	topicID := a.sessionTopicID(msg.ChatID)
	state := &ChoiceState{
		RequestID: msg.Choice.RequestID,
		ChatID:    msg.ChatID,
		TopicID:   topicID,
		Choice:    cloneChoiceValue(msg.Choice),
		Step:      0,
		Picks:     make([]string, len(msg.Choice.Questions)),
	}
	// Telegram Bot API requires the raw chat.id (no "tg_" prefix).
	// rawChatIDFromSession falls back to the input on parse failure
	// so unit tests using raw chatID still work; runtime namespaced
	// chatID always strips cleanly.
	result, err := a.sendRichFromHTML(ctx, rawChatIDFromSession(msg.ChatID), topicID, placeholderAnchor, renderChoice(state), a.choiceKeyboard(state))
	if err != nil {
		return err
	}
	state.MessageID = result.MessageID
	if topicID > 0 {
		topic, _ := a.state.topic(msg.ChatID, topicID)
		if topic != nil {
			topic.LastMessageID = result.MessageID
			if err := a.state.putTopic(topic); err != nil {
				return err
			}
		}
	}
	return a.state.putChoice(state)
}

func (a *Adapter) patchChoice(ctx context.Context, msg messages.OutboundMessage) error {
	if msg.Choice == nil || msg.Choice.RequestID == "" {
		return errors.New("telegram: OutChoicePatch missing Choice or RequestID")
	}
	state, ok := a.state.choiceByRequestID(msg.Choice.RequestID)
	if !ok {
		return nil
	}
	state.Choice = cloneChoiceValue(msg.Choice)
	state.Settled = msg.Choice.Settled
	state.SelectedID = msg.Choice.SelectedID
	keyboard := map[string]any{"inline_keyboard": []any{}}
	if !state.Settled {
		keyboard = a.choiceKeyboard(state)
	}
	// Telegram Bot API requires the raw chat.id. rawChatIDFromSession
	// falls back to the input on parse failure so unit tests using
	// raw chatID still work; runtime namespaced chatID always
	// strips cleanly.
	if err := a.editRichFromHTML(ctx, rawChatIDFromSession(state.ChatID), state.MessageID, renderChoice(state), keyboard); err != nil {
		return err
	}
	return a.state.putChoice(state)
}

// Send is the single outbound egress for the telegram adapter.
//
// msg.ChatID is the channel-namespaced form ("tg_<chat.id>[:thread_id]")
// the inbound adapter stamps on every update. The Telegram Bot
// API expects the raw chat.id, so we strip the "tg_" prefix once
// at the top of Send and pass the raw chatID to every downstream
// Telegram call (sendMessage / editMessageText / sendMediaGroup /
// setMessageReaction / etc.). This keeps the API surface
// exclusively in raw form while the rest of the runtime sees
// the namespaced form.
// streamDraftEvent routes a single OutThinking / OutToolStart /
// OutToolEnd event into the per-(chat,thread) draft surface for
// the current chat kind.
//
// Routing rules:
//   - ChatKind == "private" / "group": simulated DraftMessage via
//     liveDraftManager (sendRichMessage cold-create +
//     editMessageText(rich_message=…) + deleteMessage at turn end).
//     Both ChatKinds share the same surface — the animated
//     sendMessageDraft path was retired because its send-button
//     lock prevented the user from interjecting while the agent
//     was composing. Cold-create failure returns handled=false so
//     the caller can fall through to the richTurn chain; edit
//     failure returns handled=true so the next event retries on
//     top of the prior buffer (#391 contract).
//   - ChatKind == "channel" / unknown / no state: handled=false,
//     caller falls through to the richTurn chain path.
//
// Callers can ignore the error return (informational). The
// returned bool is the only signal they need.
func (a *Adapter) streamDraftEvent(ctx context.Context, rawChatID string, topicID int, userMsgID int, segment string, kind messages.OutboundKind) (bool, error) {
	a.logger.Info("telegram: streamDraftEvent entered",
		"chat_id", rawChatID,
		"thread_id", topicID,
		"user_msg_id", userMsgID,
		"kind", kind.String(),
	)
	state, ok := a.state.topic(rawChatID, topicID)
	if !ok {
		// No state yet — caller falls through to chain.
		a.logger.Info("telegram: streamDraftEvent fallthrough (no topic state)",
			"chat_id", rawChatID,
			"thread_id", topicID,
			"user_msg_id", userMsgID,
			"kind", kind.String(),
		)
		return false, nil
	}
	a.logger.Info("telegram: streamDraftEvent dispatching",
		"chat_id", rawChatID,
		"thread_id", topicID,
		"chat_kind", state.ChatKind,
	)
	switch state.ChatKind {
	case ChatKindPrivate, ChatKindGroup:
		if a.liveDraft == nil {
			return false, nil
		}
		return a.liveDraft.streamDraftEvent(ctx, rawChatID, topicID, userMsgID, segment, kind)
	default:
		// channel / unknown — fall through to chain.
		return false, nil
	}
}

func (a *Adapter) Send(ctx context.Context, msg messages.OutboundMessage) (err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	// Most call send sites (chatsession, runtime dispatcher, shell)
	// don't log Send errors — they bubble up. Log here so every
	// outgoing failure leaves a trace, regardless of caller.
	defer func() {
		if err != nil {
			a.logger.Warn("telegram: outgoing failed",
				"chat_id", msg.ChatID,
				"kind", msg.Kind.String(),
				"err", err,
			)
		}
	}()
	if msg.ChatID == "" {
		return errors.New("telegram: outbound ChatID is empty")
	}
	rawChatID, _, ok := splitSessionID(msg.ChatID)
	if !ok {
		// Non-telegram chatID (e.g. someone wired the wrong
		// adapter) — fall back to the raw value so the API
		// call still goes through (and Telegram will reject
		// it with a clear error, which the runtime logs).
		rawChatID = msg.ChatID
	}
	topicID := a.sessionTopicID(msg.ChatID)
	// Drop empty text and silent-drop kinds BEFORE we materialise
	// a placeholder — otherwise OutReply with empty text or
	// OutInit would still send a "🤖 Working..." bubble to
	// Telegram. Matches Feishu's pre-F-44 silent-drop behaviour.
	// See docs/channel/telegram.md §11.11.
	switch msg.Kind {
	case messages.OutReply, messages.OutCommandReply, messages.OutResult, messages.OutThinking:
		if strings.TrimSpace(msg.Text) == "" {
			return nil
		}
	case messages.OutInit:
		// OutInit is a silent-drop regardless of text — its
		// purpose is to publish session identity on the wire for
		// callers, not to surface a bubble. Without this guard
		// the placeholder resolution below would still send
		// "🤖 Working..." to Telegram.
		return nil
	}
	// Resolve replyAnchor up front. reply_to_message_id for OutXxx
	// bubbles — always the user message id that triggered this
	// turn (TopicState.UserMessageID, populated eagerly by
	// handleMessage → ensurePlaceholder). Reply chain hangs
	// under the user's own "hi" message in both DM and topic
	// modes, so the user sees their own message at the top of the
	// chain.
	//
	// The "placeholder message id" anchor (used to live here as
	// placeholderAnchor) is no longer computed at Send-time. v9
	// P1 (2026-08-23) removed ensurePlaceholderForHeartbeat
	// entirely: the per-turn placeholder is created eagerly in
	// handleMessage (chain → chunk body via ensurePlaceholder);
	// OutHeartbeat's only job is to PATCH the active chunk's
	// header (hasHeartbeat flips → Compose renders), and the
	// chunk's Telegram messageID is resolved inside
	// patchChainHeader via the rich-turn index. No state lookup needed
	// here. The legacy race-window guard (UserMessageID not yet
	// populated → silent drop) is also gone: handleMessage is
	// synchronous and writes state before publishing, so by the
	// time any Out* reaches Send the chain is already in place.
	//
	// See docs/channel/telegram.md §11.11 for the full UX and
	// §11.12.11.3 for the per-prompt DraftMessage isolation rule.
	//
	// replyAnchor is the per-event turn anchor (Telegram
	// reply_to_message_id, also the liveDraftKey suffix, also the
	// richTurns key). It MUST come from msg.ReplyTo —
	// that is the userMsgID the runtime stamped on this specific
	// event in runtime/handler.go and gateway/outbound/emitter_sink.go.
	// Reading state.UserMessageID here would conflate back-to-back
	// turns (an event for the previous turn lands on the new
	// turn's DraftMessage / rich turn when a new user message has
	// already overwritten state.UserMessageID via ensurePlaceholder).
	//
	// Feishu parity: receiptFor(ctx, chatID, userMsgID) likewise
	// uses the caller-provided userMsgID as the only anchor — the
	// adapter never re-derives it from a "current turn" field.
	//
	// Fallback to state.UserMessageID is reserved for orphan events
	// (shell /gtw / one-shot dispatchers, startup EventAgentReady)
	// where the caller genuinely has no per-turn anchor — matches
	// Feishu's orphan-fallback contract where an event without
	// ReplyTo attaches to the most recent receipt rather than being
	// silently dropped.
	var replyAnchor int
	if msg.ReplyTo != "" {
		if uid, err := strconv.Atoi(msg.ReplyTo); err == nil && uid > 0 {
			replyAnchor = uid
		}
	}
	if replyAnchor == 0 {
		if state, ok := a.state.topic(rawChatID, topicID); ok {
			if uid, err := strconv.Atoi(state.UserMessageID); err == nil && uid > 0 {
				replyAnchor = uid
			}
		}
	}
	switch msg.Kind {
	case messages.OutChoice:
		return a.sendChoice(ctx, msg, replyAnchor)
	case messages.OutChoicePatch:
		return a.patchChoice(ctx, msg)
	case messages.OutHeartbeat:
		// Issue #368: heartbeat goes through the draft ticker
		// (feedTicker on the wrapper side). The rich turn no
		// longer carries the heartbeat as a heading; the draft
		// preview is the single source of truth for ticker
		// visuals. patchChainHeader keeps the log line and is the
		// historical entry point; it now does nothing functional.
		return a.patchChainHeader(msg)
	case messages.OutMessageState:
		if msg.MessageState == nil || msg.MessageState.MessageID == "" {
			return errors.New("telegram: OutMessageState missing MessageState or MessageID")
		}
		// Channel自治: 用 State 调自己的映射函数, 不读 payload 字段。
		// (MessageStatePayload.Emoji 已删除 — runtime 不再生产半成品 emoji,
		//  每个 channel 自维护 state → emoji 映射, 跟 feishu 的
		//  mapStateToFeishuEmoji 对位。详见 docs/channel/telegram.md §14。)
		state := msg.MessageState.State
		emoji := mapStateToTelegramEmoji(state)
		if emoji == "" {
			return nil // 未知 state silent drop, 跟 feishu 对位
		}
		// 幂等: 同 state 跳过 API 调用, 避免 Telegram API 抖动。
		// 跟 feishu 的 messageStates LRU 行为对称。
		if prev, ok := a.lastMessageState(msg.MessageState.MessageID); ok && prev == state {
			return nil
		}
		messageID, err := strconv.Atoi(msg.MessageState.MessageID)
		if err != nil {
			return err
		}
		// v6.1: Telegram bots are limited to ONE reaction per
		// message (REACTIONS_TOO_MANY if you try to set >1). Each
		// state emits a single reaction that REPLACES the prior
		// one (Telegram setMessageReaction is SET, not append) —
		// the user sees the emoji CHANGE as state progresses:
		//   Queued    → "" (silent drop; placeholder text
		//     "🤖 Working..." already announces arrival)
		//   Submitted → 👌 (thinking — replaces prior emoji on
		//     the single reaction slot)
		//   Done      → "" (silent drop; OnPromptEnded stamps
		//     ✅ on the per-turn placeholder instead)
		reactions := []map[string]any{{"type": "emoji", "emoji": emoji}}
		if err := a.setMessageReactions(ctx, rawChatID, messageID, reactions); err != nil {
			return err
		}
		a.rememberMessageState(msg.MessageState.MessageID, state)
		return nil
	case messages.OutMessageStateRemoved:
		if msg.MessageState == nil || msg.MessageState.MessageID == "" {
			return errors.New("telegram: OutMessageStateRemoved missing MessageState or MessageID")
		}
		messageID, err := strconv.Atoi(msg.MessageState.MessageID)
		if err != nil {
			return err
		}
		// OutMessageStateRemoved clears the entire reaction list
		// (no emoji). Caller uses this to explicitly drop reactions
		// (e.g., MessageDropped).
		return a.setMessageReactions(ctx, rawChatID, messageID, nil)
	case messages.OutToolStart:
		// DM draft path: REPLACE semantics — OutToolStart is the
		// "first half" of a tool display; OutToolEnd stacks below.
		// Group path: cold-creates the simulated DraftMessage
		// (live_draft.go); subsequent events editMessageText in
		// place. On cold-create failure falls through to richTurn.
		if handled, _ := a.streamDraftEvent(ctx, rawChatID, topicID, replyAnchor, formatTool(msg), messages.OutToolStart); handled {
			return nil
		}
		// L3 (§20.6.3): route through richTurn. The tool call line
		// (`● Tool(args)`) becomes one paragraph block in the
		// turn's rich message. The matching OutToolEnd appends the
		// result line (`⎿  result`) as a sibling block. Both render
		// in the same Telegram message because the rich turn flushes
		// once per debounce — visually identical to the v9 chain's
		// "in-place rewrite" UX, with the rewrite semantics replaced
		// by "append second entry + debounced PATCH" semantics.
		//
		// Issue #368: statusbar trailer no longer rides on chain
		// chunks — the draft_ticker wrapper handles the trailer.
		// Pass nil to skip the now-unused footer parameter.
		if msg.Tool == nil {
			return a.appendSegmentForKind(ctx, msg, rawChatID, topicID, replyAnchor,
				formatTool(msg))
		}
		startName := msg.Tool.Name
		if startName == "" {
			startName = "tool"
		}
		startBody := formatToolStartCall(startName, msg.Tool.Args)
		return a.appendRichTurnAndFlush(ctx, rawChatID, topicID, replyAnchor,
			richTurnEntry{kind: "tool", body: startBody},
			statusbar.StatusBarLines(&msg))

	case messages.OutToolEnd:
		// DM draft path: ACCUMULATE semantics — append result line
		// below the matching OutToolStart, so the user sees the
		// full "🔧 call / ✅ result" pair in one draft body.
		// Group path: edits the simulated DraftMessage with the
		// appended result line (REPLACE/ACCUMULATE per #383).
		if handled, _ := a.streamDraftEvent(ctx, rawChatID, topicID, replyAnchor, formatTool(msg), messages.OutToolEnd); handled {
			return nil
		}
		// L3: route through richTurn. The result line lands as a
		// sibling block to the Start line in the same rich message.
		// msg.Err vs msg.Tool.Err: gateway always sets Tool.Err;
		// using Tool.Err is the canonical-error contract.
		if msg.Tool == nil {
			return a.appendSegmentForKind(ctx, msg, rawChatID, topicID, replyAnchor,
				formatTool(msg))
		}
		endName := msg.Tool.Name
		if endName == "" {
			endName = "tool"
		}
		toolErr := msg.Tool.Err
		resultBody := summarizeToolResult(endName, msg.Tool.Output, toolErr)

		// Synchronous flush so the user sees `● Tool(args)\n⎿ result`
		// as a single visual block. The rich turn's debounce will
		// also catch any subsequent events; this synchronous flush
		// just ensures the result is rendered before the next
		// OutToolStart arrives.
		return a.appendRichTurnAndFlush(ctx, rawChatID, topicID, replyAnchor,
			richTurnEntry{kind: "tool", body: resultBody},
			statusbar.StatusBarLines(&msg))
	case messages.OutTaskCreate, messages.OutTaskUpdate:
		// L3: route through richTurn. taskList is its own rich
		// blocks section (heading + list) — renderRichTurnBlocksLocked
		// emits it as a single `list` block when present. Mirror
		// v9 P2 contract: nil TaskList silent drops; empty Items
		// clears the section. Pass nil for footer — issue #368.
		if msg.TaskList == nil {
			return nil
		}
		items := make([]taskListItem, 0, len(msg.TaskList.Items))
		for _, it := range msg.TaskList.Items {
			items = append(items, taskListItem{
				Status:     taskStatusToString(it.Status),
				ID:         it.ID,
				Subject:    it.Subject,
				ActiveForm: it.ActiveForm,
			})
		}
		a.setRichTurnTaskList(rawChatID, topicID, replyAnchor, items, statusbar.StatusBarLines(&msg))
		return nil
	case messages.OutError:
		// L3: route through richTurn. The error body lands as one
		// paragraph (markdown fenced-code wrap is preserved by the
		// rich block renderer's fenced-code handling — the walker
		// detects ``` fences and produces pre blocks natively).
		// Footer param nil — issue #368.
		body := msg.Text
		if msg.Diagnostic != nil && msg.Diagnostic.StderrTail != "" {
			body += "\n\n```\n" + msg.Diagnostic.StderrTail + "\n```"
		}
		a.appendRichTurn(ctx, rawChatID, topicID, replyAnchor,
			richTurnEntry{kind: "error", body: body},
			statusbar.StatusBarLines(&msg))
		return nil
	case messages.OutInit:
		// Silent drop — matches feishu F-44. The Init payload
		// (session_id, model, agent name, …) is still on the
		// wire for callers, but we don't surface a
		// "Agent: x · Model: y · Session: z" bubble in the
		// topic. Session identity shows up via the StatusBar
		// trailer on every subsequent outbound message (see §18).
		return nil
	case messages.OutThinking:
		// DM draft path: REPLACE semantics — one thinking event
		// displays alone as "💭 <text>". The "💭 " prefix
		// matches the chain path's body format (F-think parity with
		// feishu) so DM and forum-topic visuals stay consistent.
		// Group path: cold-creates / edits the simulated DraftMessage
		// (live_draft.go) with the REPLACE buffer.
		// Empty-text silent drop already happened at the top of
		// Send, so msg.Text is non-empty here.
		if handled, _ := a.streamDraftEvent(ctx, rawChatID, topicID, replyAnchor, "💭 "+msg.Text, messages.OutThinking); handled {
			return nil
		}
		// F-think parity with feishu: prefix the reasoning body
		// with `💭 ` so the user can scan the chat and instantly
		// see "this is the agent's thinking" without reading the
		// full prose. Feishu does the same inline in
		// postThreadMarkdownReply before rendering as lark_md;
		// Telegram renders markdown via chunk_body.Compose's
		// RenderMarkdown pass, so the prefix just needs to be in
		// the segment text.
		//
		// Empty-text silent drop already happened at the top of
		// Send (case messages.OutReply, ... OutThinking: if
		// strings.TrimSpace(msg.Text) == "" { return nil }), so
		// msg.Text here is non-empty. We DO NOT trim the body
		// here — the user's whitespace is content. If a future
		// caller bypasses the drop gate with all-whitespace
		// text, appendSegmentForKind's own
		// `strings.TrimSpace(segment) == ""` check at the top
		// catches it (the prefix `💭 ` is non-whitespace so we
		// can't mirror the trim here).
		body := "💭 " + msg.Text
		return a.appendSegmentForKind(ctx, msg, rawChatID, topicID, replyAnchor, body)
	case messages.OutResult:
		// v9 P2: OutResult is the user-facing final output of the
		// turn. Fold it into the chain and you lose the visual
		// difference between "thinking" / "tool call" / "result" —
		// the chat becomes a wall of mixed segments. Instead, send
		// it as a STANDALONE reply-anchored message with its own
		// StatusBar trailer. The chain keeps handling the in-turn
		// activity (thinking / tools / reply / error / task /
		// command), and the result message becomes the
		// OnPromptEnded 🎉 anchor (recorded on chain.resultMessageID).
		// Long bodies (>3900 chars) split into multiple reply-anchored
		// pieces; only the LAST piece's messageID is recorded —
		// "last wins" semantics so the 🎉 lands on the visual end
		// of the result block.
		//
		// End the draft process before sending the real message so
		// the next process (next turn) starts with a fresh draft_id
		// and empty textBuf. Telegram server will also push out the
		// draft as soon as this real message lands.
		// End the simulated DraftMessage before the real OutResult lands:
		// flush any remaining buffered events via editMessageText
		// PATCH, then deleteMessage the DraftMessage so the chat
		// timeline shows only the result message. Empty-text silent
		// drop already happened at the top of Send, so msg.Text is
		// non-empty here.
		if a.liveDraft != nil {
			a.liveDraft.endProcess(ctx, rawChatID, topicID, replyAnchor)
		}
		return a.sendOutResultMessage(ctx, msg, rawChatID, topicID, replyAnchor)
	default:
		// OutReply / OutCommandReply: every remaining text-emitting
		// kind folds onto the active chain chunk (v9 §11.12).
		// StatusBar trailing only lands on segments produced by
		// footer-bearing kinds (OutReply / OutTaskCreate /
		// OutTaskUpdate); see §11.12.6 for the in-memory footer
		// semantics. OutResult is intentionally NOT here — handled
		// by the explicit case above.
		//
		// Every OutReply / OutCommandReply routes through
		// appendSegmentForKind → appendRichTurn → renderRichTurnBlocksLocked
		// → editMessageText(rich_message=...) PATCH. The placeholder
		// card is the single visual surface for the turn (created
		// eagerly by ensurePlaceholder); updates PATCH in place.
		//
		// The pre-merge L2 walker path (trySendRichBlocks →
		// sendRichMessage) was REMOVED because it stood up a
		// second standalone rich message instead of editing the
		// placeholder, breaking the "single visual surface"
		// invariant. The walker itself (markdownToRichBlocks)
		// is still used by appendSegmentForKind's chain-attached
		// kinds to convert markdown bodies into rich blocks.
		return a.appendSegmentForKind(ctx, msg, rawChatID, topicID, replyAnchor, msg.Text)
	}
}

// appendSegmentForKind is the L3 richTurn ingest helper. Single
// point of entry for every Out* kind that maps onto a rich-turn
// entry (OutReply / OutThinking / OutToolStart / OutToolEnd /
// OutError / OutTaskCreate / OutTaskUpdate / OutCommandReply).
// The kind is derived from msg.Kind so callers can pass a raw
// markdown body without re-classifying.
//
// v9 P2: OutResult does not flow through this path — it goes
// through sendOutResultMessage and lands as a standalone reply-
// anchored Telegram message with its own StatusBar trailer.
// See docs/channel/telegram.md §11.12.4.1 for the full rationale.
func (a *Adapter) appendSegmentForKind(
	ctx context.Context,
	msg messages.OutboundMessage,
	rawChatID string,
	topicID, userMessageID int,
	segment string,
) error {
	if strings.TrimSpace(segment) == "" {
		return nil
	}

	// L3: route through richTurn. The chain-attached kind is
	// derived from msg.Kind so callers (OutReply, OutThinking,
	// etc.) don't need to repeat the switch.
	kind := ""
	switch msg.Kind {
	case messages.OutReply, messages.OutCommandReply:
		kind = "reply"
	case messages.OutThinking:
		kind = "thinking"
	case messages.OutToolStart, messages.OutToolEnd:
		kind = "tool"
	case messages.OutError:
		kind = "error"
	case messages.OutTaskCreate, messages.OutTaskUpdate:
		kind = "task"
	}
	if kind != "" {
		// Strip the trailing "\n" we used to add for the chain
		// renderer's separator; the rich walker handles its own
		// inter-block spacing. Footer carries the latest statusbar
		// snapshot for this turn — renderRichTurnBlocksLocked
		// emits it as a `pre` block at the bottom of the placeholder
		// card.
		body := strings.TrimRight(segment, "\n")
		return a.appendRichTurn(ctx, rawChatID, topicID, userMessageID,
			richTurnEntry{kind: kind, body: body},
			statusbar.StatusBarLines(&msg))
	}

	// RichMode=off path: silent drop. v9 chain is gone in L3;
	// users running RichMode=off still get the message via the
	// legacy plain-text sendMessage path. (Today, no such path
	// exists in this branch — the migration is on. Real fall-
	// through can land in a follow-up PR if RichMode=off needs
	// to be supported without rich messages.)
	a.logger.Debug("telegram: appendSegmentForKind dropped (RichMode=off)",
		"chat_id", rawChatID, "kind", msg.Kind.String())
	return nil
}

// sendOutResultMessage emits msg.Text as a standalone reply-anchored
// rich message via sendRichMessage. Empty text is silently dropped
// at the top of Send (caller invariant).
//
// Wire form: rich_message[blocks] carrying the markdown body walked
// through markdownToRichBlocks (so headings / fences / lists / quotes
// / tables / inline entities reach the wire as proper rich blocks),
// followed by a divider + footer block whose `text` field is the
// StatusBar lines rendered as RichText (PR anchors survive as
// `{"type":"url",...}` entities, not literal markdown).
//
// Why no plain-text fallback: the migration to sendRichMessage
// exists precisely to lift the 4096-char plain-text ceiling (see
// docs/channel/telegram.md §20.1 — Bot API 10.1 supports up to
// 32K+ chars per rich block). Falling back to sendMessage +
// splitTelegramText(3900) when the rich path fails would re-impose
// the very limit the migration removes. If sendRichMessage errors,
// the caller surfaces the failure to the runtime — the user
// gets a visible "send failed" rather than a silently truncated
// message that hides what was rejected.
//
// Preflight is delegated to buildResultBlocks / markdownToRichBlocks
// (block-count cap = 400/500, char cap = 32K). When the walker bails
// (block-cap / char-cap / malformed shape), buildResultBlocks
// degrades to a single paragraph block rather than dropping the
// message. If the server still rejects the resulting blocks
// (RICH_MESSAGE_BLOCKS_TOO_MANY), the caller sees the error and
// decides what to do (today: log + return).
//
// 🎉 anchor: the rich message_id is stored on richTurn.resultMessageID
// so OnPromptEnded's terminal reaction lands on the result message
// rather than the active chain chunk (chain no longer exists in L3).
func (a *Adapter) sendOutResultMessage(
	ctx context.Context,
	msg messages.OutboundMessage,
	rawChatID string,
	topicID, userMessageID int,
) error {
	if strings.TrimSpace(msg.Text) == "" {
		return nil
	}

	blocksJSON, ok := buildResultBlocks(msg.Text, statusbar.StatusBarLines(&msg))
	if !ok {
		// Walker succeeded but produced 0 blocks (shouldn't
		// happen — body is non-empty here — but defensive:
		// silent drop rather than an empty rich message).
		return nil
	}

	mid, err := a.trySendRichBlocks(ctx, rawChatID, topicID, userMessageID, blocksJSON)
	if err != nil {
		a.logger.Warn("telegram: rich OutResult failed (no plain fallback; migration target is rich)",
			"chat_id", rawChatID,
			"thread_id", topicID,
			"blocks_len", len(blocksJSON),
			"err", err)
		return err
	}
	turn := a.richTurns.getOrCreate(rawChatID, topicID, userMessageID)
	turn.mu.Lock()
	turn.resultMessageID = mid
	turn.mu.Unlock()
	return nil
}

// isTextEmittingKind reports whether msg.Kind flows through the
// chain-rolling-log path with a StatusBar trailer (v8 §18
// contract). Excludes OutChoice (its own InlineKeyboard card),
// OutMessageState / OutMessageStateRemoved (reactions, no text),
// OutInit (silent drop), OutHeartbeat (status ticker, not entry
// text), and OutResult (v9 P2 — handled by sendOutResultMessage
// as a standalone reply, not a chain entry).
func isTextEmittingKind(k messages.OutboundKind) bool {
	switch k {
	case messages.OutReply, messages.OutThinking,
		messages.OutToolStart, messages.OutToolEnd,
		messages.OutTaskCreate, messages.OutTaskUpdate,
		messages.OutError, messages.OutCommandReply:
		return true
	}
	return false
}

// OnPromptEnded marks the turn as done by stamping a ✅
// reaction on the per-turn placeholder.
//
// v6.3: Telegram bot single-reaction budget — the user
// message's reaction slot is reserved for MessageSubmitted
// ("AI thinking"). OnPromptEnded does NOT overwrite that
// slot with 👌; the terminal visual is conveyed via the
// per-turn placeholder's 🎉 reaction.
//
// userMsgID is part of the interface contract (and the runtime
// still threads it through eventbus e.UserMsgID) but is no longer
// consumed by this adapter — the placeholder's identity is
// already pinned by state.PlaceholderMessageID. The parameter
// is kept in the signature to match the channel.Channel
// interface contract; callers should not rely on this side
// reacting on the user message.
//
// chatID accepts both forms: the namespaced session form
// ("tg_<chatid>[:thread_id]") that the runtime passes, and the
// raw form used by direct unit tests. splitSessionID returns
// ok=false for the raw form so we fall back to using chatID as
// the raw chat id (matches the existing TopicState key shape).
// See docs/channel/telegram.md §11.11 (v6.3).
func (a *Adapter) OnPromptEnded(ctx context.Context, chatID, userMsgID string, reason agent.PromptEndReason) {
	if chatID == "" {
		return
	}
	rawChatID, _, ok := splitSessionID(chatID)
	if !ok {
		rawChatID = chatID
	}
	topicID := a.sessionTopicID(chatID)
	parsedUserMsgID := atoiUserMsgID(userMsgID)

	// 1. Synchronously flush the rich turn (if any) so the 🎉
	// lands on the fully-rendered rich message. L3: the v9 chain
	// is gone — the rich turn IS the source of truth.
	a.OnPromptEndedRichTurn(rawChatID, topicID, parsedUserMsgID)

	// 2. Pick the 🎉 anchor. v9 P2 priority: resultMessageID
	// (L1/L3 standalone OutResult) > rich turn messageID (fallback).
	var targetID int64
	if turn, ok := a.richTurns.lookup(rawChatID, topicID, parsedUserMsgID); ok && turn != nil {
		turn.mu.Lock()
		targetID = turn.resultMessageID
		if targetID == 0 {
			targetID = turn.messageID
		}
		turn.mu.Unlock()
	}

	// 3. Stamp the reaction. v6.3 single-reaction budget on the
	// USER MSG slot is preserved — the stamp lands on a bot-owned
	// message, never on the user's original message.
	//
	// 🎉 (clean) / ❌ (any non-clean reason). ✅ U+2705 was rejected
	// by Telegram API in v5 live probes; 🎉 is the stable clean
	// replacement.
	if targetID != 0 {
		emoji := "🎉"
		if reason.IsError() {
			emoji = "❌"
		}
		_ = a.setMessageReactions(ctx, rawChatID, int(targetID),
			[]map[string]any{{"type": "emoji", "emoji": emoji}})
	}

	// 4. Turn-end cleanup: forget the in-memory rich turn. The
	// frozen rich message remains in chat as historical evidence.
	// Next user message re-materialises a fresh rich turn via
	// ensurePlaceholder.
	a.richTurns.purge(rawChatID, topicID, parsedUserMsgID)

	// 5. End the simulated DraftMessage process for the turn so the
	// next turn starts fresh. Single surface for both DM and group:
	// liveDraft.endProcess flushes any remaining buffered events
	// via sendRichMessage (cold-create or editMessageText PATCH) and
	// deletes the underlying Telegram message — the server does not
	// auto-disappear a real message the way sendMessageDraft pushes
	// a draft, so the bot owns the cleanup.
	//
	// Safety net for turns with NO OutResult (OutError-only, runtime
	// crash, etc.) — otherwise OutResult's send path covers it.
	if a.liveDraft != nil && parsedUserMsgID > 0 {
		// Skip when no per-turn anchor — the streamer already no-ops
		// internally but skipping here avoids the warn log and the
		// map lookup for truly orphan OnPromptEnded calls (startup,
		// tests). Feishu parity: OnPromptEnded strictly acts on the
		// caller-provided userMsgID and never re-derives it.
		a.liveDraft.endProcess(ctx, rawChatID, topicID, parsedUserMsgID)
	}
}

func (a *Adapter) patchChainHeader(msg messages.OutboundMessage) error {
	if msg.Heartbeat == nil {
		return nil
	}

	// Resolve routing keys the same way Send does, so the rich
	// turn lookup hits the same cache entry the eventual
	// editMessageText PATCH will land on.
	rawChatID, _, ok := splitSessionID(msg.ChatID)
	if !ok {
		rawChatID = msg.ChatID
	}
	topicID := a.sessionTopicID(msg.ChatID)
	// Per the §11.12.11.3 per-prompt isolation rule, the turn anchor
	// comes from msg.ReplyTo (the runtime stamps it on every
	// OutboundMessage including OutHeartbeat follow-ups — see
	// runtime/heartbeat_followup.go). state.UserMessageID is the
	// fallback only — reading it first would misroute a back-to-back
	// turn's heartbeat edit onto the new turn's rich message.
	userMessageID := 0
	if msg.ReplyTo != "" {
		if uid, err := strconv.Atoi(msg.ReplyTo); err == nil && uid > 0 {
			userMessageID = uid
		}
	}
	if userMessageID == 0 {
		if state, ok := a.state.topic(rawChatID, topicID); ok {
			if uid, err := strconv.Atoi(state.UserMessageID); err == nil && uid > 0 {
				userMessageID = uid
			}
		}
	}

	// Hide the heartbeat line until the runtime has observed real
	// activity. Empty() returns false for terminal snapshots
	// (HeartbeatDone / HeartbeatError) so the verdict still shows
	// even with zero counters / LastBeatAt.
	if msg.Heartbeat.Empty() {
		return nil
	}

	a.updateRichTurnHeader(rawChatID, topicID, userMessageID, heartbeatText(msg.Heartbeat))
	return nil
}

// atoiUserMsgID parses the userMsgID (string) back to int for
// the reply_to chain message ID. Returns 0 on failure.
func atoiUserMsgID(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}

// v8 had a renderBodyWithStatusBar helper that paired markdown
// rendering with a StatusBar trailer for the per-bubble path. v9
// split this responsibility: chain entries render via
// chunkBody.Compose() with the trailer stitched in from
// chain.lastFooter. L3 retired the chain and the plain-text
// fallback: sendOutResultMessage is now rich-only and routes
// through buildResultBlocks (result_blocks.go) → trySendRichBlocks
// (rich.go) → rich_message[blocks] body carrying markdown walker
// blocks plus a divider + footer block. No legacy renderForWire
// path, no plaintext trailer-in-markdown shape.

func (a *Adapter) HealthSnapshot() (string, json.RawMessage, error) {
	a.mu.Lock()
	pendingRichTurns := 0
	if a.richTurns != nil {
		pendingRichTurns = a.richTurns.size()
	}
	a.mu.Unlock()
	payload, _ := json.Marshal(map[string]any{
		"username":           a.botName,
		"connected":          a.started && !a.stopped,
		"offset":             a.offset,
		"rich_path_enabled":  true,
		"pending_rich_turns": pendingRichTurns,
	})
	return "telegram", payload, nil
}

func (a *Adapter) BuildBlocks(text string, attachments []messages.Attachment) []agent.ContentBlock {
	blocks := make([]agent.ContentBlock, 0, len(attachments)+1)
	if text != "" {
		blocks = append(blocks, agent.ContentBlock{Type: agent.ContentText, Text: text})
	}
	for _, attachment := range attachments {
		if attachment.LocalPath == "" {
			continue
		}
		blockType := agent.ContentFile
		if strings.HasPrefix(strings.ToLower(attachment.MimeType), "image/") {
			blockType = agent.ContentImage
		}
		blocks = append(blocks, agent.ContentBlock{Type: blockType, Path: attachment.LocalPath, MediaType: attachment.MimeType})
	}
	return blocks
}

// formatTool produces a single-line chain segment for the active
// placeholder chunk. v9 dispatches the call line and result line to
// separate helpers (formatToolStartCall / summarizeToolResult) —
// feishu parallel from internal/channel/feishu/summarize_tool.go.
//
// OutToolStart emits a "call" line:
//
//	`● Bash(ls -la)`
//
// OutToolEnd emits a one-line "result" line that hides raw output:
//
//	`⎿  📄 Read → 47 lines`
//
// PII posture: raw output is NEVER included in the result line —
// only the byte/lines/file-count heuristic. Custom / unknown tools
// report only the byte count.
//
// Falls back to msg.Text when msg.Tool is nil (defensive — the
// dispatch in Send already guards this, but formatTool is also
// reachable from places that haven't been migrated yet).
func formatTool(msg messages.OutboundMessage) string {
	if msg.Tool == nil {
		if msg.Text == "" {
			return ""
		}
		return msg.Text
	}
	name := msg.Tool.Name
	if name == "" {
		name = "tool"
	}
	switch msg.Kind {
	case messages.OutToolStart:
		return formatToolStartCall(name, msg.Tool.Args)
	case messages.OutToolEnd:
		return summarizeToolResult(name, msg.Tool.Output, msg.Err)
	}
	return ""
}

// formatInit was the v8 OutInit pre-render helper. v9 silenced
// OutInit (matches feishu F-44); session identity now travels
// through StatusBar on every footer-bearing outbound message.
// Removed.

// heartbeatText composes the per-beat progress line as plain
// text (no HTML markup). Renders into a Telegram rich_message
// paragraph block — NOT a heading — so it sits at the same
// scale as surrounding content entries rather than dominating
// the placeholder card.
//
// Timestamp source: snapshot.LastBeatAt — the last think/tool
// event wall-clock, refreshed by chatsession heartbeat tracker.
// NOT time.Now() at heartbeat emission (those can diverge when
// the heartbeat timer fires after agent stalls). Returning the
// activity time means the user sees "agent was last thinking
// at HH:MM:SS".
//
// Local-time rendering (no .UTC()): the user reads the bot's
// chat in their own timezone and expects the wall-clock they
// see on their phone, not UTC. We normalise via .Local() so a
// snapshot stamped as UTC internally still renders as local.
//
// No snapshot → return the bare "🤖 Working..." banner without
// a timestamp (we have nothing to attribute it to).
func heartbeatText(snapshot *messages.HeartbeatSnapshot) string {
	if snapshot == nil {
		return "🤖 Working..."
	}
	// Skip-zero-chips semantics aligned with feishu's
	// renderHeartbeatHeader (F-63 §3.6): a zero ThinkCount /
	// ToolCount does NOT contribute a chip, so a /think off +
	// /tools off turn that still emits an OutHeartbeat produces
	// just the prefix (or empty body when running).
	var parts []string
	if snapshot.ThinkCount > 0 {
		parts = append(parts, fmt.Sprintf("💭 %d", snapshot.ThinkCount))
	}
	if snapshot.ToolCount > 0 {
		parts = append(parts, fmt.Sprintf("🔧 %d", snapshot.ToolCount))
	}
	if !snapshot.LastBeatAt.IsZero() {
		parts = append(parts, "⏱ "+snapshot.LastBeatAt.Local().Format("15:04:05"))
	}
	body := strings.Join(parts, " · ")
	// Terminal-only snapshots produce just the verdict prefix so
	// the user still sees "✅ Done" / "❌ Failed" on a turn that
	// never accumulated counters (e.g. one-shot answer with
	// /think off + /tools off).
	switch snapshot.Status {
	case messages.HeartbeatDone:
		return "✅ " + body
	case messages.HeartbeatError:
		return "❌ " + body
	}
	return body
}

// renderInlineText was a thin wrapper around RenderMarkdown +
// escapeHTML fallback — same shape as the package-level
// RenderMarkdown. Removed 2026-08-23 (chain unification).
// All call sites should use RenderMarkdown directly.

func userID(message *Message) string {
	if message == nil || message.From == nil {
		return ""
	}
	return strconv.FormatInt(message.From.ID, 10)
}

func replyToID(message *Message) string {
	if message == nil || message.ReplyToMessage == nil {
		return ""
	}
	return strconv.Itoa(message.ReplyToMessage.MessageID)
}

func cloneChoiceValue(choice *messages.Choice) *messages.Choice {
	if choice == nil {
		return nil
	}
	copy := *choice
	copy.Options = append([]messages.ChoiceOption(nil), choice.Options...)
	copy.Questions = append([]messages.ChoiceQuestion(nil), choice.Questions...)
	for questionIndex := range copy.Questions {
		copy.Questions[questionIndex].Options = append([]messages.ChoiceOption(nil), choice.Questions[questionIndex].Options...)
	}
	return &copy
}

func sleepContext(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// apiCall is the retry + rate-limit wrapped api.call. All
// outbound side-effects (send / edit / reaction / topic / file)
// funnel through this so transient errors retry and the global
// token bucket prevents 429s.
//
// Method is the Telegram method name (e.g. "sendMessage"). The
// caller passes any extra retry context (chat_id, message_id) via
// attrs.
func (a *Adapter) apiCall(ctx context.Context, method string, params map[string]any, result any, attrs ...any) error {
	if err := a.limiter.Wait(ctx); err != nil {
		return err
	}
	opts := RetryOpts{
		Op:     method,
		Cfg:    a.retry,
		Logger: a.logger,
		Attrs:  attrs,
	}
	return WithTransientRetry(ctx, opts, func() error {
		return a.api.call(ctx, method, params, result)
	})
}

// mapStateToTelegramEmoji converts a runtime MessageState into the
// unicode emoji that Telegram's setMessageReaction accepts.
//
// Mirrors internal/channel/feishu/adapter.go::mapStateToFeishuEmoji in
// shape and contract: the adapter decides the emoji, NOT the runtime.
// Telegram reactions are unicode codepoints (Feishu uses predefined
// emoji_type identifiers — OneSecond/OnIt/DONE — because the Feishu
// reaction service rejects unicode input with code 99992354; see
// docs/channel/feishu.md §6.6.3). Telegram accepts unicode directly.
//
// Unknown states return "" so callers can silent-drop, matching feishu's
// forward-compatible behaviour.
//
// MessageDropped is intentionally unmapped: feishu conveys failure via
// the reply text's ❌ prefix rather than a user-message reaction, and
// telegram follows the same convention to keep the cross-channel
// rendering consistent.
// mapStateToTelegramEmoji converts a runtime MessageState into
// the unicode emoji that Telegram's setMessageReaction accepts.
//
// v6.3: Telegram bots can only set ONE reaction per message.
// Spending the single slot on the most informative state
// (MessageSubmitted = "AI thinking") keeps the user informed
// during the long-running async turn, when no other UI signal
// is changing. MessageQueued is too transient to be useful
// (gone within ~50ms in a healthy run), MessageDone is captured
// separately by the per-turn placeholder text PATCH + ✅
// reaction on the placeholder message (see OnPromptEnded).
//
// Probed via live API (docs/channel/telegram.md §11.11.3):
//   - MessageQueued    → "" (silent drop; placeholder text
//     "🤖 Working..." already announces the
//     message reached the adapter)
//   - MessageSubmitted → 👌  ("AI thinking" — OK-hand emoji,
//     the single reaction slot is reserved for the long-running
//     async turn)
//   - MessageDone      → "" (silent drop; OnPromptEnded
//     stamps ✅ on the per-turn placeholder instead)
func mapStateToTelegramEmoji(state agent.MessageState) string {
	switch state {
	case agent.MessageSubmitted:
		return "👌"
	}
	return ""
}

// lastMessageState returns the last rendered MessageState for userMsgID
// and whether an entry exists. The bool distinguishes "never rendered"
// (no entry, no API call wasted) from "rendered MessageQueued earlier"
// (entry present, may dedup against a re-emit of the same state).
//
// MessageState's zero value is MessageQueued — the first valid state we
// render — so the bool is load-bearing: returning just agent.MessageState
// would falsely dedup the very first emit of MessageQueued.
func (a *Adapter) lastMessageState(userMsgID string) (agent.MessageState, bool) {
	a.muMessageStates.Lock()
	defer a.muMessageStates.Unlock()
	s, ok := a.messageStates[userMsgID]
	return s, ok
}

// rememberMessageState records the last rendered MessageState for
// userMsgID so the next emit can dedup via lastMessageState.
//
// The map is lazily allocated on first use; nil-safe under muMessageStates.
// No LRU eviction today — userMsgID churn is bounded by inbound message
// volume (one entry per user message that ever enters the system), which
// in practice stays well under a few thousand per daemon session.
// Revisit if memory pressure surfaces.
func (a *Adapter) rememberMessageState(userMsgID string, state agent.MessageState) {
	a.muMessageStates.Lock()
	defer a.muMessageStates.Unlock()
	if a.messageStates == nil {
		a.messageStates = make(map[string]agent.MessageState)
	}
	a.messageStates[userMsgID] = state
}

var _ channel.Channel = (*Adapter)(nil)
