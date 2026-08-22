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

	commandServices "github.com/cnlangzi/nightme/internal/command/services"
	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/channel"
	"github.com/cnlangzi/nightme/internal/config"
	"github.com/cnlangzi/nightme/internal/messages"
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
	// Adapt the rest of the function to thread_id / state-key
	// variables rather than the legacy topicID name.
	topicID := threadID
	attachments, err := a.attachments(ctx, message, chatID)
	if err != nil && a.logger != nil {
		a.logger.Warn("telegram: download attachments failed", "chat_id", chatID, "message_id", message.MessageID, "err", err)
	}
	a.state.mu.Lock()
	needsSave := false
	if stored, ok := a.state.topics[a.state.topicKey(chatID, topicID)]; ok {
		if stored.UserMessageID != strconv.Itoa(message.MessageID) {
			stored.UserMessageID = strconv.Itoa(message.MessageID)
			needsSave = true
		}
	}
	a.state.mu.Unlock()
	if needsSave {
		_ = a.state.save()
	}
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

func (a *Adapter) ensurePlaceholder(ctx context.Context, chatID string, topicID, userMessageID int) error {
	state, ok := a.state.topic(chatID, topicID)
	if !ok {
		state = &TopicState{ChatID: chatID, TopicID: topicID, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	}
	if state.PlaceholderMessageID == 0 {
		result, err := a.sendTelegramMessage(ctx, chatID, topicID, 0, "<b>🤖 Working...</b>", nil)
		if err != nil {
			return err
		}
		state.PlaceholderMessageID = result.MessageID
		state.UserMessageID = strconv.Itoa(userMessageID)
		state.LastMessageID = result.MessageID
		if err := a.state.putTopic(state); err != nil {
			return err
		}
		a.logger.Debug("telegram: placeholder created",
			"chat_id", chatID,
			"thread_id", topicID,
			"placeholder_message_id", result.MessageID,
		)
		return nil
	}
	state.UserMessageID = strconv.Itoa(userMessageID)
	return a.state.putTopic(state)
}

// ensurePlaceholderForHeartbeat lazily creates the placeholder
// message if it does not already exist, returning the message id
// the caller should PATCH. Used by the OutHeartbeat path so the
// first heartbeat always has a stable anchor even if the
// handleMessage-side ensurePlaceholder failed (e.g. transient
// network error after retries were exhausted, or a heartbeat that
// races ahead of the user-message handler finishing).
//
// Works for both real Telegram topics (topicID > 0) and DM /
// main-window (topicID == 0); in the DM case the placeholder is
// the visual anchor every OutXxx bubble replies to (see
// docs/channel/telegram.md §11.11).
//
// Concurrency note: ensurePlaceholder (handleMessage-side) and
// ensurePlaceholderForHeartbeat (Send-side) are not mutex-
// coordinated. In the steady-state runtime flow both run on the
// same goroutine — handleMessage publishes the inbound before
// the resulting ChatSession emits any OutXxx, so the two calls
// are strictly ordered per chatID and the second call always
// observes the first call's putTopic. A theoretical race where
// handleMessage is still mid-ensurePlaceholder (sendMessage
// issued but putTopic not yet committed) when Send runs would
// cause this function to send a SECOND placeholder message in
// Telegram. This race is structurally impossible in the current
// runtime: chatsession.AcceptInbound is called synchronously
// from the inbound pump goroutine and emits no OutXxx before
// HandleInbound returns, so Send cannot run concurrently with
// handleMessage for the same chatID. If future runtime changes
// break that ordering (e.g., fire-and-forget accept), wire a
// per-chatID mutex around ensurePlaceholder and this function.
func (a *Adapter) ensurePlaceholderForHeartbeat(ctx context.Context, chatID string, topicID int) (int, error) {
	state, ok := a.state.topic(chatID, topicID)
	if ok && state.PlaceholderMessageID > 0 {
		return state.PlaceholderMessageID, nil
	}
	result, err := a.sendTelegramMessage(ctx, chatID, topicID, 0, "<b>🤖 Working...</b>", nil)
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
	// Resolve the per-turn placeholder anchor up front. In real
	// topics (topicID > 0) the anchor is whatever the
	// handleMessage-side ensurePlaceholder persisted; in DM /
	// main-window (topicID == 0) we lazy-create it here so the
	// first OutXxx bubble in a turn has something to reply to.
	// See docs/channel/telegram.md §11.11 for the full UX.
	placeholderAnchor, placeholderErr := a.ensurePlaceholderForHeartbeat(ctx, rawChatID, topicID)
	if placeholderErr != nil && a.logger != nil {
		a.logger.Warn("telegram: placeholder resolve failed",
			"chat_id", rawChatID,
			"thread_id", topicID,
			"err", placeholderErr,
		)
	}
	// Reply chain (reply_to_message_id = placeholder) is a
	// DM-only visual cue. Real Telegram topics already group
	// their events via message_thread_id; threading every
	// OutXxx through reply_to_message_id=placeholder there would
	// make every bubble show "replying to 🤖 Working..." in the
	// client UI, which is redundant and visually noisy. The
	// placeholder in topic mode is used only for OutHeartbeat
	// PATCH and OnPromptEnded ✅ Completed edits.
	var replyAnchor int
	if topicID == 0 {
		replyAnchor = placeholderAnchor
	}
	switch msg.Kind {
	case messages.OutChoice:
		return a.sendChoice(ctx, msg, replyAnchor)
	case messages.OutChoicePatch:
		return a.patchChoice(ctx, msg)
	case messages.OutHeartbeat:
		// PATCH the placeholder so the topic / DM main window
		// shows live "Working..." progress without spawning a
		// new bubble. placeholderAnchor was resolved at the top
		// of Send via ensurePlaceholderForHeartbeat, so this
		// works for both real topics (topicID > 0) and DM /
		// main-window (topicID == 0).
		if placeholderAnchor > 0 {
			text := "🤖 Working..."
			if msg.Heartbeat != nil {
				text = heartbeatText(msg.Heartbeat)
			}
			return a.editTelegramMessage(ctx, rawChatID, placeholderAnchor, renderInlineText(text), nil)
		}
		// Fallback: standalone heartbeat bubble. Should be
		// rare — placeholderAnchor resolution only fails when
		// the lazy send above returned an error.
		return a.sendRenderedText(ctx, rawChatID, topicID, 0, msg.Text)
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
			return nil // 未知 state silent drop, 跟 feishu 对齐
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
		if err := a.setMessageReaction(ctx, rawChatID, messageID, emoji); err != nil {
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
		return a.setMessageReaction(ctx, rawChatID, messageID, "")
	case messages.OutToolStart, messages.OutToolEnd:
		return a.sendRenderedText(ctx, rawChatID, topicID, replyAnchor, formatTool(msg))
	case messages.OutTaskCreate, messages.OutTaskUpdate:
		return a.sendRenderedText(ctx, rawChatID, topicID, replyAnchor, formatTaskList(msg.TaskList))
	case messages.OutError:
		text := msg.Text
		if msg.Diagnostic != nil && msg.Diagnostic.StderrTail != "" {
			text += "\n\n<pre>" + escapeHTML(msg.Diagnostic.StderrTail) + "</pre>"
		}
		return a.sendRenderedText(ctx, rawChatID, topicID, replyAnchor, text)
	case messages.OutInit:
		// Silent drop — matches feishu F-44. The Init payload
		// (session_id, model, agent name, …) is still on the
		// wire for callers, but we don't surface a
		// "Agent: x · Model: y · Session: z" bubble in the
		// topic. Session identity already shows up in the
		// placeholder text when the heartbeat-driven PATCH
		// upgrades it (future work — see docs/channel/telegram.md
		// §14 Known Limitations).
		return nil
	default:
		return a.sendRenderedText(ctx, rawChatID, topicID, replyAnchor, msg.Text)
	}
}

// OnPromptEnded flips the placeholder message text from
// "🤖 Working..." to "<b>✅ Completed</b>" once the agent turn is
// done. Works for both real Telegram topics (topicID > 0) and
// DM / main-window (topicID == 0) — see docs/channel/telegram.md
// §11.11 for the DM placeholder + reply-chain UX.
//
// chatID accepts both forms: the namespaced session form
// ("tg_<chatid>[:thread_id]") that the runtime passes, and the
// raw form used by direct unit tests. splitSessionID returns
// ok=false for the raw form so we fall back to using chatID as
// the raw chat id (matches the existing TopicState key shape).
func (a *Adapter) OnPromptEnded(ctx context.Context, chatID, userMsgID string) {
	if chatID == "" {
		return
	}
	rawChatID, _, ok := splitSessionID(chatID)
	if !ok {
		rawChatID = chatID
	}
	topicID := a.sessionTopicID(chatID)
	state, ok := a.state.topic(rawChatID, topicID)
	if !ok || state.PlaceholderMessageID <= 0 {
		return
	}
	_ = a.editTelegramMessage(ctx, rawChatID, state.PlaceholderMessageID, "<b>✅ Completed</b>", nil)
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


func formatTool(msg messages.OutboundMessage) string {
	if msg.Tool == nil {
		return msg.Text
	}
	name := msg.Tool.Name
	if name == "" {
		name = "tool"
	}
	prefix := "🔧"
	if msg.Kind == messages.OutToolEnd {
		prefix = "✅"
	}
	result := prefix + " " + name
	if msg.Tool.Args != "" {
		result += "\n" + msg.Tool.Args
	}
	if msg.Tool.Output != "" {
		result += "\n" + msg.Tool.Output
	}
	if msg.Err != nil {
		result += "\n⚠️ " + msg.Err.Error()
	}
	return result
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

func heartbeatText(snapshot *messages.HeartbeatSnapshot) string {
	if snapshot == nil {
		return "🤖 Working..."
	}
	return "💭 " + strconv.Itoa(snapshot.ThinkCount) + " · 🔧 " + strconv.Itoa(snapshot.ToolCount)
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
func mapStateToTelegramEmoji(state agent.MessageState) string {
	switch state {
	case agent.MessageQueued:
		return "⏳"
	case agent.MessageSubmitted:
		return "🔄"
	case agent.MessageDone:
		return "✅"
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
