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
	commandServices "github.com/cnlangzi/nightme/internal/command/services"
	"github.com/cnlangzi/nightme/internal/config"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/internal/statusbar"
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

	// chains is the v9 per-turn placeholder chain index (see
	// docs/channel/telegram.md §11.12.2). Pure in-memory; never
	// persisted to telegram_state.json. Reset on Adapter.Stop.
	chains *chainLRU
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
	return &Adapter{
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
		chains:    newChainLRU(defaultChainLRUCap),
	}, nil
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
		chains:    newChainLRU(defaultChainLRUCap),
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
		// Drop in-memory chain index on cold-stop. (Chains are
		// per-process anyway; explicit reset documents intent.)
		if a.chains != nil {
			a.chains.reset()
		}
		return nil
	}
	if a.stopped {
		a.mu.Unlock()
		return nil
	}
	a.stopped = true
	cancel := a.cancel
	a.mu.Unlock()
	if a.chains != nil {
		a.chains.reset()
	}
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
			"offset":          a.offset,
			"limit":           100,
			"timeout":         a.config.PollingTimeout,
			"allowed_updates": []string{"message", "callback_query", "my_chat_member", "chat_member", "message_reaction"},
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
		return
	}
	if update.MessageReaction != nil {
		a.handleMessageReaction(ctx, update.MessageReaction)
		return
	}
	if update.MyChatMember != nil {
		a.handleMyChatMember(ctx, update.MyChatMember)
		return
	}
	if update.ChatMember != nil {
		a.handleChatMember(ctx, update.ChatMember)
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
	if err := a.ensurePlaceholder(ctx, chatID, threadID, message.MessageID); err != nil {
		a.logger.Warn("telegram: ensure placeholder failed", "chat_id", chatID, "thread_id", threadID, "err", err)
	}
	// (no StatusBar cache to reset — see §18; the renderer is
	// a pure consumer of msg fields stamped by runtime /
	// chatsession.)
	// Adapt the rest of the function to thread_id / state-key
	// variables rather than the legacy topicID name.
	topicID := threadID
	attachments, err := a.attachments(ctx, message, chatID)
	if err != nil && a.logger != nil {
		a.logger.Warn("telegram: download attachments failed", "chat_id", chatID, "message_id", message.MessageID, "err", err)
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

// handleMessageReaction converts a Telegram user-reaction
// update into an InboundMessage.Reaction so the runtime can route
// it to ChatSession.HandleAction (gtwDrafts decision / F-50 reaction
// routing / F-31 MessageState FSM in that order).
//
// Telegram delivers one update per change. We only forward events
// that have at least one emoji in the new reaction list — pure
// removals (NewReaction empty) are reported with Emoji="" so the
// runtime can clear its state.
//
// chatID MUST match the namespaced form produced by the message
// path ("tg_<chat.id>") — otherwise runtime.findChatSession cannot
// resolve the owning ChatSession and the reaction is silently
// dropped at the gtw reaction handler (see the 2026-08-22 fix
// notes; previously this function used the raw Telegram chat.id
// which broke gtw emoji-reaction routing entirely).
//
// Known limitation: MessageReactionUpdate carries no message_thread_id,
// so reactions on messages inside a Forum topic always resolve to
// the chat-level chatID ("tg_<chat.id>" without thread suffix).
// Topic-resident gtw drafts are keyed by the per-topic chatID
// ("tg_<chat.id>:<thread_id>") and therefore cannot be reached by
// a native emoji reaction. Documented in docs/channel/telegram.md
// §15 (limitations / gap catalog).
func (a *Adapter) handleMessageReaction(_ context.Context, update *MessageReactionUpdate) {
	if update == nil || update.User.ID == 0 {
		return
	}
	a.mu.Lock()
	botID := a.botID
	a.mu.Unlock()
	if botID != 0 && update.User.ID == botID {
		return
	}
	emoji := ""
	if len(update.NewReaction) > 0 {
		emoji = update.NewReaction[0].Emoji
	}
	rawChatID := strconv.FormatInt(update.Chat.ID, 10)
	chatID := a.sessionChatID(rawChatID, 0)
	inbound := messages.InboundMessage{
		ChatID:     chatID,
		UserID:     strconv.FormatInt(update.User.ID, 10),
		MessageID:  strconv.Itoa(update.MessageID),
		Time:       time.Unix(update.Date, 0).UTC(),
		HasMention: true,
		Reaction: &commandServices.ReactionEvent{
			TargetMsgID: strconv.Itoa(update.MessageID),
			Emoji:       emoji,
			UserID:      strconv.FormatInt(update.User.ID, 10),
			ChatID:      chatID,
		},
	}
	a.publish(inbound)
}

// handleMyChatMember tracks the bot's own membership changes:
// added to / removed from / promoted in a chat. We just log; the
// runtime doesn't act on bot lifecycle today, but a future
// self-healing path (e.g. drop the chat from state when the bot
// is kicked) can hook here.
func (a *Adapter) handleMyChatMember(_ context.Context, update *ChatMemberUpdate) {
	if update == nil || update.NewChatMember == nil {
		return
	}
	chatID := strconv.FormatInt(update.Chat.ID, 10)
	if a.logger != nil {
		a.logger.Info("telegram: my_chat_member",
			"chat_id", chatID,
			"old_status", chatMemberStatus(update.OldChatMember),
			"new_status", update.NewChatMember.Status,
		)
	}
}

// handleChatMember is fired when a non-bot user joins/leaves a
// chat. We log only; the runtime doesn't act on user membership
// today.
func (a *Adapter) handleChatMember(_ context.Context, update *ChatMemberUpdate) {
	if update == nil || update.NewChatMember == nil {
		return
	}
	chatID := strconv.FormatInt(update.Chat.ID, 10)
	if a.logger != nil {
		a.logger.Debug("telegram: chat_member",
			"chat_id", chatID,
			"user_id", update.NewChatMember.User.ID,
			"new_status", update.NewChatMember.Status,
		)
	}
}

func chatMemberStatus(m *ChatMember) string {
	if m == nil {
		return ""
	}
	return m.Status
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
	// carrying the placeholder message id (see handleMessage
	// and ensurePlaceholderForHeartbeat). Returning (0, nil) here
	// keeps chatID stable ("tg_<chatid>") and lets the placeholder
	// path own the stateStore write.
	return 0, nil
}

// ensurePlaceholder materialises the per-turn placeholder
// message and pins the user-message anchor for this turn.
//
// Each user message triggers a NEW placeholder. The previous
// turn's placeholder is left untouched (it was already PATCHed
// to `<b>✅ Completed</b>` by OnPromptEnded and stays in the
// Telegram timeline as that turn's permanent status marker).
//
// OutXxx bubbles carry `reply_to_message_id = userMsgID` so the
// reply chain hangs under the user's own message ("hi"),
// not under the placeholder. The placeholder is the turn's
// status ticker (Working… → ✅ Completed), not the reply anchor.
// See docs/channel/telegram.md §11.11.
func (a *Adapter) ensurePlaceholder(ctx context.Context, chatID string, topicID, userMessageID int) error {
	state, ok := a.state.topic(chatID, topicID)
	if !ok {
		state = &TopicState{ChatID: chatID, TopicID: topicID, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	}
	// Drop any in-memory chain for this turn — the previous turn's
	// chunks stay in Telegram chat (frozen, no further edits) but we
	// don't track them anymore. Next Out* gets a fresh chain.
	if a.chains != nil {
		a.chains.purge(chatID, topicID)
	}

	// Cold-create the first chunk via send. Header carries the
	// turn-start timestamp; body holds no entries yet (segments
	// arrive on Out* events). v9 chains start with header-only.
	header := placeholderInitialText(time.Now().UTC())
	result, err := a.sendTelegramMessage(ctx, chatID, topicID, userMessageID, header, nil)
	if err != nil {
		return err
	}

	// Materialise the in-memory chain so ensurePlaceholderForHeartbeat
	// / Send can resolve it without recreating.
	chain := a.chains.getOrCreate(chatID, topicID)
	chain.mu.Lock()
	chain.chunks = []*placeholderChunk{{
		messageID:  int64(result.MessageID),
		headerLine: header,
	}}
	chain.cursor = 0
	chain.dirty = false
	chain.lastFooter = nil
	chain.mu.Unlock()

	state.PlaceholderMessageID = result.MessageID
	state.LastMessageID = result.MessageID
	state.UserMessageID = strconv.Itoa(userMessageID)
	a.logger.Debug("telegram: chain created (v9)",
		"chat_id", chatID,
		"thread_id", topicID,
		"chunk_message_id", result.MessageID,
		"user_message_id", userMessageID,
	)
	return a.state.putTopic(state)
}

// placeholderInitialText is the canonical text for a freshly
// created per-turn placeholder. Both ensurePlaceholder
// (handleMessage path) and ensurePlaceholderForHeartbeat
// (lazy-create fallback) use this so the two creation paths
// can't drift apart on format. v7: timestamp `⏱ HH:MM:SS`
// so the user can see when the turn started.
func placeholderInitialText(now time.Time) string {
	return "<b>🤖 Working...</b> · ⏱ " + now.Format("15:04:05")
}

// ensurePlaceholderForHeartbeat returns the placeholder message
// id for the current turn, creating one if missing (e.g., when
// the first OutHeartbeat races ahead of handleMessage's
// ensurePlaceholder, or after a transient network failure). Used
// by the OutHeartbeat PATCH path and by Send prelude's
// placeholderAnchor resolution.
//
// Race-window guard (2026-08-22, codex review): if the
// ChatSession state has no UserMessageID set yet (handleMessage
// hasn't run yet for this turn), we DON'T lazy-create — the
// created placeholder would be orphan-raced: handleMessage's
// subsequent ensurePlaceholder call would overwrite
// state.PlaceholderMessageID with a new placeholder, leaving
// the heartbeat-created bubble in the chat as a permanent
// "🤖 Working..." without terminal PATCH or 🎉 reaction.
// Returning (0, nil) is safe: Send's OutHeartbeat path
// silently drops when placeholderAnchor == 0 (the in-turn status
// ticker is then conveyed only after handleMessage lands).
//
// Topic mode and DM mode behave the same: return existing
// placeholder if state.PlaceholderMessageID > 0, otherwise
// create one (only when UserMessageID is set, see above).
// The placeholder is the per-turn status ticker
// (Working… → ✅ Completed); reply chain goes to userMsgID.
// See docs/channel/telegram.md §11.11.
func (a *Adapter) ensurePlaceholderForHeartbeat(ctx context.Context, chatID string, topicID int) (int, error) {
	state, ok := a.state.topic(chatID, topicID)
	if ok && state.PlaceholderMessageID > 0 {
		return state.PlaceholderMessageID, nil
	}
	// Race-window guard: handleMessage hasn't populated the state
	// yet. Wait for it instead of creating a placeholder that
	// will be orphan'd by the subsequent ensurePlaceholder call.
	if !ok || state.UserMessageID == "" {
		return 0, nil
	}
	// Genuine "placeholder was supposed to exist but didn't" case
	// (e.g., ensurePlaceholder failed with transient network
	// error). Create one now.
	initialText := placeholderInitialText(time.Now().UTC())
	result, err := a.sendTelegramMessage(ctx, chatID, topicID, 0, initialText, nil)
	if err != nil {
		return 0, err
	}
	if state == nil {
		state = &TopicState{ChatID: chatID, TopicID: topicID, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	}
	state.PlaceholderMessageID = result.MessageID
	state.LastMessageID = result.MessageID
	if err := a.state.putTopic(state); err != nil {
		return result.MessageID, err
	}
	return result.MessageID, nil
}

func (a *Adapter) attachments(ctx context.Context, message *Message, chatID string) ([]messages.Attachment, error) {
	values := make([]attachmentSource, 0, 2)
	if len(message.Photo) > 0 {
		values = append(values, attachmentSource{FileID: message.Photo[len(message.Photo)-1].FileID, Name: "image.jpg", MimeType: "image/jpeg"})
	}
	if message.Document != nil {
		values = append(values, attachmentSource{FileID: message.Document.FileID, Name: message.Document.FileName, MimeType: message.Document.MimeType})
	}
	if message.Audio != nil {
		values = append(values, attachmentSource{FileID: message.Audio.FileID, Name: message.Audio.FileName, MimeType: message.Audio.MimeType})
	}
	if message.Voice != nil {
		values = append(values, attachmentSource{FileID: message.Voice.FileID, Name: "voice.ogg", MimeType: message.Voice.MimeType})
	}
	if message.Video != nil {
		values = append(values, attachmentSource{FileID: message.Video.FileID, Name: message.Video.FileName, MimeType: message.Video.MimeType})
	}
	attachments := make([]messages.Attachment, 0, len(values))
	for _, source := range values {
		attachment, err := a.downloadAttachment(ctx, source, chatID, message.MessageID)
		if err != nil {
			attachments = append(attachments, messages.Attachment{Name: source.Name, MimeType: source.MimeType, Error: err})
			continue
		}
		attachments = append(attachments, attachment)
	}
	return attachments, nil
}

type attachmentSource struct {
	FileID   string
	Name     string
	MimeType string
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
	return messages.Attachment{LocalPath: localPath, MimeType: source.MimeType, Name: name, FileKey: source.FileID, FileName: name, Type: "file"}, nil
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
	result, err := a.sendTelegramMessage(ctx, rawChatIDFromSession(msg.ChatID), topicID, placeholderAnchor, renderChoice(state), a.choiceKeyboard(state))
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
	if err := a.editTelegramMessage(ctx, rawChatIDFromSession(state.ChatID), state.MessageID, renderChoice(state), keyboard); err != nil {
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
	// Resolve anchors up front. Two distinct concepts (both used
	// in BOTH topic and DM mode — per-user-message semantics):
	//
	//   placeholderAnchor — the per-turn "🤖 Working..." bot
	//     message id. Each user message creates a fresh one.
	//     Used by OutHeartbeat (PATCH status text) and
	//     OnPromptEnded (PATCH to ✅ Completed). The previous
	//     turn's placeholder stays in the Telegram timeline as
	//     that turn's permanent status marker.
	//
	//   replyAnchor — reply_to_message_id for OutXxx bubbles.
	//     Always the user message id that triggered this turn
	//     (TopicState.UserMessageID). Reply chain hangs under
	//     the user's own "hi" message in both DM and topic
	//     modes, so the user sees their own message at the top
	//     of the chain.
	//
	// See docs/channel/telegram.md §11.11 for the full UX.
	placeholderAnchor, placeholderErr := a.ensurePlaceholderForHeartbeat(ctx, rawChatID, topicID)
	if placeholderErr != nil && a.logger != nil {
		a.logger.Warn("telegram: placeholder resolve failed",
			"chat_id", rawChatID,
			"thread_id", topicID,
			"err", placeholderErr,
		)
	}
	var replyAnchor int
	if state, ok := a.state.topic(rawChatID, topicID); ok {
		if uid, err := strconv.Atoi(state.UserMessageID); err == nil && uid > 0 {
			replyAnchor = uid
		}
	}
	switch msg.Kind {
	case messages.OutChoice:
		return a.sendChoice(ctx, msg, replyAnchor)
	case messages.OutChoicePatch:
		return a.patchChoice(ctx, msg)
	case messages.OutHeartbeat:
		// PATCH the per-turn placeholder for live "Working..."
		// v9: heartbeat only PATCHes the active chunk's headerLine
		// (in-memory). The next debounce flush writes the full
		// rendered text to Telegram. If no chunk exists yet (the
		// first heartbeat raced ahead of handleMessage), silently
		// drop — OutMessageState's 👌 reaction already announces
		// the turn.
		if placeholderAnchor > 0 {
			return a.patchChainHeader(ctx, rawChatID, topicID, msg)
		}
		return nil
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
	case messages.OutToolStart, messages.OutToolEnd:
		return a.appendSegmentForKind(ctx, msg, rawChatID, topicID, replyAnchor,
			formatTool(msg))
	case messages.OutTaskCreate, messages.OutTaskUpdate:
		return a.appendSegmentForKind(ctx, msg, rawChatID, topicID, replyAnchor,
			formatTaskList(msg.TaskList))
	case messages.OutError:
		text := msg.Text
		if msg.Diagnostic != nil && msg.Diagnostic.StderrTail != "" {
			text += "\n\n<pre>" + escapeHTML(msg.Diagnostic.StderrTail) + "</pre>"
		}
		return a.appendSegmentForKind(ctx, msg, rawChatID, topicID, replyAnchor, text)
	case messages.OutInit:
		// Silent drop — matches feishu F-44. The Init payload
		// (session_id, model, agent name, …) is still on the
		// wire for callers, but we don't surface a
		// "Agent: x · Model: y · Session: z" bubble in the
		// topic. Session identity shows up via the StatusBar
		// trailer on every subsequent outbound message (see §18).
		return nil
	default:
		// OutReply / OutResult / OutThinking / OutCommandReply:
		// every text-emitting kind folds onto the active chain
		// chunk (v9 §11.12). StatusBar trailing only lands on
		// segments produced by footer-bearing kinds (OutReply /
		// OutResult / OutTaskCreate / OutTaskUpdate); see
		// §11.12.6 for the in-memory footer semantics.
		return a.appendSegmentForKind(ctx, msg, rawChatID, topicID, replyAnchor, msg.Text)
	}
}

// appendSegmentForKind is the v9 chain ingest helper. Single
// point of entry for every Out* kind that maps onto a chain
// segment (OutReply / OutResult / OutThinking / OutToolStart /
// OutToolEnd / OutError / OutTaskCreate / OutTaskUpdate). It
// computes whether the current event is footer-bearing
// (drives whether lastFooter is refreshed) and hands off to
// the package-level appendSegment primitive. Always schedules
// a debounced flush on return so the active chunk's text gets
// back to Telegram.
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
	chain := a.chains.getOrCreate(rawChatID, topicID)

	// Footer-bearing kinds carry session context for StatusBar.
	// Others (OutThinking / OutToolStart / OutToolEnd /
	// OutError) leave lastFooter untouched (§11.12.6).
	var sb []string
	switch msg.Kind {
	case messages.OutReply, messages.OutResult,
		messages.OutTaskCreate, messages.OutTaskUpdate:
		sb = statusbar.StatusBarLines(&msg)
	}

	userMsgID, err := strconv.Atoi(strconv.Itoa(userMessageID))
	if err != nil {
		return err
	}
	if err := appendSegment(
		ctx, chain,
		rawChatID, topicID, userMsgID,
		segment+"\n", sb,
		a.chainSendFn(), a.chainEditFn(ctx),
	); err != nil {
		return err
	}

	scheduleFlushDebounced(chain, rawChatID, topicID, userMessageID)
	return nil
}

// chainSendFn returns a closure bound to the Adapter's sendTelegramMessage
// for use by the package-level appendSegment primitive. Sends via the
// existing rate limiter / retry pipeline (commit #2 wires this to the
// Adapter apiClient through sendTelegramMessage).
func (a *Adapter) chainSendFn() sendChunkFn {
	return func(ctx context.Context, chatID string, topicID int, replyToMessageID int, text string) (int64, error) {
		res, err := a.sendTelegramMessage(ctx, chatID, topicID, replyToMessageID, text, nil)
		if err != nil {
			return 0, err
		}
		return int64(res.MessageID), nil
	}
}

// chainEditFn returns a closure bound to the Adapter's editTelegramMessage.
// Long-text overflow path in flushChainNow may need to issue multiple
// edits — each one goes through apiCall's rate limit / retry pipeline.
func (a *Adapter) chainEditFn(ctx context.Context) editChunkFn {
	return func(ctx context.Context, chatID string, messageID int64, text string) error {
		return a.editTelegramMessage(ctx, chatID, int(messageID), text, nil)
	}
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
func (a *Adapter) OnPromptEnded(ctx context.Context, chatID, userMsgID string) {
	if chatID == "" {
		return
	}
	rawChatID, _, ok := splitSessionID(chatID)
	if !ok {
		rawChatID = chatID
	}
	topicID := a.sessionTopicID(chatID)

	// v9: chain is the truth. Resolve the active chunk via
	// chainLRU (not state.PlaceholderMessageID which is now
	// read-only back-compat). Flush the in-memory buffer to
	// Telegram first so the [reaction] lands on a fully-rendered
	// chunk.
	chain := a.chains.getOrCreate(rawChatID, topicID)
	if chain.cursor < 0 {
		a.chains.purge(rawChatID, topicID)
		return
	}

	// Best-effort flush before stamping the terminal reaction.
	if err := flushChainNow(
		ctx, chain,
		rawChatID, topicID, atoiUserMsgID(userMsgID),
		a.chainEditFn(ctx), a.chainSendFn(),
	); err != nil && a.logger != nil {
		a.logger.Warn("telegram: OnPromptEnded flush failed",
			"chat_id", rawChatID, "err", err)
	}

	cur := chain.chunks[chain.cursor]
	if cur != nil {
		// [reaction] on the active chunk. v6.3 single-reaction
		// budget on USER MSG slot is preserved; this stamp lands
		// on the placeholder, not the user's original message.
		// [emoji] is in the official ReactionTypeEmoji whitelist
		// ([other emoji] U+2705 was rejected by Telegram API).
		_ = a.setMessageReactions(ctx, rawChatID, int(cur.messageID),
			[]map[string]any{{"type": "emoji", "emoji": "\U0001F389"}})
	}

	// Turn-end cleanup: forget the in-memory chain. Frozen
	// chunks remain in chat as historical evidence (no edit
	// touches them again). Next user message re-materialises
	// a fresh chain via ensurePlaceholder.
	a.chains.purge(rawChatID, topicID)
}

// patchChainHeader refreshes the active chunk's headerLine
// (in-memory) and arms the debounced flush. Bound to the
// OutHeartbeat Send case (v9 §11.12.8). Header text is the same
// "💭 N · 🔧 M · ⏱ ..." shape as v8's heartbeat.
func (a *Adapter) patchChainHeader(
	ctx context.Context,
	chatID string,
	topicID int,
	msg messages.OutboundMessage,
) error {
	chain := a.chains.getOrCreate(chatID, topicID)
	chain.mu.Lock()
	if chain.cursor < 0 {
		chain.mu.Unlock()
		return nil
	}
	cur := chain.chunks[chain.cursor]
	status := "🤖 Working..."
	if msg.Heartbeat != nil {
		status = heartbeatText(msg.Heartbeat)
	}
	cur.headerLine = status + " · ⏱ " + time.Now().UTC().Format("15:04:05")
	chain.dirty = true
	chain.mu.Unlock()

	scheduleFlushDebounced(chain, chatID, topicID, 0)
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

// renderBodyWithStatusBar appends the per-turn StatusBar footer
// to a message body when one is available, then renders the
// combined text through RenderMarkdown (Telegram restricted
// HTML subset). The footer is wrapped in a chevron-tail ASCII
// panel (statusbar.RenderPanel — ┌/└ on the left, `›` chevron
// tail on the right) so users can tell at a glance "this is
// session metadata, not reply content". The right side opens
// outward because StatusBar content can extend right and a
// closed `┐`/`┘` would imply a hard boundary that doesn't
// exist. Returns body unchanged when StatusBar is empty so a
// chunk with all-zero statusbar fields (e.g. a streaming
// OutReply that hasn't received its terminal Usage yet) just
// omits the trailer — zero-omit per F-45 §1.6.
//
// The body itself is plain text — RenderMarkdown treats it as
// markdown source and converts to <b>/<i>/<code>/<a>. Pre-escaped
// HTML fragments (e.g. OutError's <pre>stderr</pre> block) must
// NOT go through this helper — use sendRenderedText directly so
// RenderMarkdown doesn't re-parse them and break the literal tags.
//
// No cache: per the F-44 / fix-placehold-card contract, every
// OutboundMessage that reaches Send has AgentName / Model /
// SessionID / GitStatus stamped by runtime/chatsession, and
// Usage is filled at terminal. StatusBarLines is a pure
// consumer of msg fields — we render what's there, nothing
// more. See docs/channel/telegram.md §18.
func (a *Adapter) renderBodyWithStatusBar(body string, msg *messages.OutboundMessage) string {
	snap := statusbar.StatusBarLines(msg)
	if len(snap) == 0 {
		return body
	}
	full := body + "\n\n" + statusbar.RenderPanel(snap)
	rendered, err := RenderMarkdown(full)
	if err != nil {
		// Mirror renderInlineText: RenderMarkdown failure → fall
		// back to escapeHTML on the raw combined string. StatusBar
		// lines + panel borders are plain text so this preserves
		// them verbatim.
		return escapeHTML(full)
	}
	return rendered
}

func (a *Adapter) HealthSnapshot() (string, json.RawMessage, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	payload, _ := json.Marshal(map[string]any{"username": a.botName, "connected": a.started && !a.stopped, "offset": a.offset})
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
		if strings.HasPrefix(attachment.MimeType, "image/") {
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
//   `● Bash(ls -la)`
//
// OutToolEnd emits a one-line "result" line that hides raw output:
//   `⎿  📄 Read → 47 lines`
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

func formatTaskList(taskList *agent.AgentTaskListEvent) string {
	if taskList == nil || len(taskList.Items) == 0 {
		return ""
	}
	var result strings.Builder
	for _, item := range taskList.Items {
		result.WriteString("- [")
		if item.Status == agent.TaskCompleted {
			result.WriteString("x")
		} else if item.Status == agent.TaskInProgress {
			result.WriteString("~")
		} else {
			result.WriteString(" ")
		}
		result.WriteString("] ")
		result.WriteString(item.Subject)
		result.WriteByte('\n')
	}
	return result.String()
}

func formatInit(msg messages.OutboundMessage) string {
	parts := make([]string, 0, 4)
	if msg.AgentName != "" {
		parts = append(parts, "Agent: "+msg.AgentName)
	}
	if msg.Model != "" {
		parts = append(parts, "Model: "+msg.Model)
	}
	if msg.SessionID != "" {
		parts = append(parts, "Session: "+msg.SessionID)
	}
	if len(parts) == 0 {
		return msg.Text
	}
	return strings.Join(parts, " · ")
}

// heartbeatText renders the in-turn status ticker. v7 added the
// `⏱ HH:MM:SS` timestamp so the user can see when the last
// heartbeat was emitted (≈ "agent is alive at this clock time").
// The wall clock is taken from snapshot.LastBeatAt which the
// chatsession heartbeat tracker refreshes on every think/tool
// event (see internal/chatsession/heartbeat.go).
func heartbeatText(snapshot *messages.HeartbeatSnapshot) string {
	if snapshot == nil {
		return "🤖 Working..."
	}
	text := "💭 " + strconv.Itoa(snapshot.ThinkCount) + " · 🔧 " + strconv.Itoa(snapshot.ToolCount)
	if !snapshot.LastBeatAt.IsZero() {
		text += " · ⏱ " + snapshot.LastBeatAt.UTC().Format("15:04:05")
	}
	return text
}

func renderInlineText(text string) string {
	rendered, err := RenderMarkdown(text)
	if err != nil {
		return escapeHTML(text)
	}
	return rendered
}

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
