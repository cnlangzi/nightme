package telegram

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
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
	webhook   *http.Server
	mu        sync.Mutex
	started   bool
	stopped   bool
	offset    int64
	botID     int64
	botName   string
	callbacks map[string]struct{}
	limiter   *Limiter
	retry     RetryConfig
}

func NewAdapter(cfg *config.Config) (*Adapter, error) {
	if cfg == nil {
		return nil, errors.New("telegram: config is nil")
	}
	botToken := strings.TrimSpace(cfg.Telegram.BotToken)
	if botToken == "" {
		return nil, errors.New("telegram: bot_token is required")
	}
	mode := strings.ToLower(strings.TrimSpace(cfg.Telegram.Mode))
	if mode == "" {
		mode = "polling"
	}
	if mode != "polling" && mode != "webhook" {
		return nil, fmt.Errorf("telegram: unsupported mode %q", mode)
	}
	timeout := cfg.Telegram.PollingTimeout
	if timeout <= 0 {
		timeout = 30
	}
	if timeout > 50 {
		timeout = 50
	}
	cfgCopy := cfg.Telegram
	cfgCopy.Mode = mode
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
	if copy.Mode == "" {
		copy.Mode = "polling"
	}
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
	if a.config.Mode == "webhook" {
		if err := a.startWebhook(a.ctx); err != nil {
			a.Stop(context.Background())
			return err
		}
	} else {
		go a.pollLoop(a.ctx)
	}
	a.logger.Info("telegram: started", "mode", a.config.Mode, "username", a.botName)
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
	server := a.webhook
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if server != nil {
		shutdownContext, cancelShutdown := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancelShutdown()
		_ = server.Shutdown(shutdownContext)
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
	return nil
}

func (a *Adapter) pollLoop(ctx context.Context) {
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
	if message.Chat.Type != "private" && a.config.RequireMentionInGroup() && !hasMention {
		return
	}
	chatID := strconv.FormatInt(message.Chat.ID, 10)
	topicID, err := a.ensureTopic(ctx, message)
	if err != nil {
		a.logger.Warn("telegram: ensure topic failed", "chat_id", chatID, "err", err)
		return
	}
	if topicID > 0 {
		if err := a.ensurePlaceholder(ctx, chatID, topicID, message.MessageID); err != nil {
			a.logger.Warn("telegram: ensure placeholder failed", "chat_id", chatID, "topic_id", topicID, "err", err)
		}
	}
	attachments, err := a.attachments(ctx, message, chatID)
	if err != nil && a.logger != nil {
		a.logger.Warn("telegram: download attachments failed", "chat_id", chatID, "message_id", message.MessageID, "err", err)
	}
	_ = topicID
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
func (a *Adapter) handleMessageReaction(ctx context.Context, update *MessageReactionUpdate) {
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
	chatID := strconv.FormatInt(update.Chat.ID, 10)
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
func (a *Adapter) handleMyChatMember(ctx context.Context, update *ChatMemberUpdate) {
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
func (a *Adapter) handleChatMember(ctx context.Context, update *ChatMemberUpdate) {
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

func (a *Adapter) ensureTopic(ctx context.Context, message *Message) (int, error) {
	chatID := strconv.FormatInt(message.Chat.ID, 10)
	if message.Chat.Type == "private" {
		return 0, nil
	}
	if message.MessageThreadID > 0 {
		state, _ := a.state.topic(chatID, message.MessageThreadID)
		if state == nil {
			state = &TopicState{ChatID: chatID, TopicID: message.MessageThreadID, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
			if err := a.state.putTopic(state); err != nil {
				return 0, err
			}
		}
		return message.MessageThreadID, nil
	}
	if state, ok := a.state.topicForChat(chatID); ok {
		return state.TopicID, nil
	}
	name := "nightme"
	if message.From != nil && message.From.Username != "" {
		name += " · " + message.From.Username
	} else if message.From != nil {
		name += " · " + strconv.FormatInt(message.From.ID, 10)
	}
	topicID, err := a.createTopic(ctx, chatID, name)
	if err != nil {
		return 0, err
	}
	state := &TopicState{ChatID: chatID, TopicID: topicID, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := a.state.putTopic(state); err != nil {
		return 0, err
	}
	return topicID, nil
}

func (a *Adapter) ensurePlaceholder(ctx context.Context, chatID string, topicID, userMessageID int) error {
	state, ok := a.state.topic(chatID, topicID)
	if !ok {
		state = &TopicState{ChatID: chatID, TopicID: topicID, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	}
	if state.PlaceholderMessageID == 0 {
		result, err := a.sendTelegramMessage(ctx, chatID, topicID, "<b>🤖 Working...</b>", nil)
		if err != nil {
			return err
		}
		state.PlaceholderMessageID = result.MessageID
		state.UserMessageID = strconv.Itoa(userMessageID)
		state.LastMessageID = result.MessageID
		return a.state.putTopic(state)
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
// Returns (0, nil) when topicID is 0 (p2p chat — no topic scope).
// Returns (0, err) on send failure; the caller falls back to
// sending a standalone heartbeat bubble.
func (a *Adapter) ensurePlaceholderForHeartbeat(ctx context.Context, chatID string, topicID int) (int, error) {
	if topicID <= 0 {
		return 0, nil
	}
	state, ok := a.state.topic(chatID, topicID)
	if ok && state.PlaceholderMessageID > 0 {
		return state.PlaceholderMessageID, nil
	}
	result, err := a.sendTelegramMessage(ctx, chatID, topicID, "<b>🤖 Working...</b>", nil)
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
	if a.botName == "" {
		return !a.config.RequireMentionInGroup()
	}
	return strings.Contains(strings.ToLower(text), "@"+strings.ToLower(a.botName))
}

func (a *Adapter) sendChoice(ctx context.Context, msg messages.OutboundMessage) error {
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
	result, err := a.sendTelegramMessage(ctx, msg.ChatID, topicID, renderChoice(state), a.choiceKeyboard(state))
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
	if err := a.editTelegramMessage(ctx, state.ChatID, state.MessageID, renderChoice(state), keyboard); err != nil {
		return err
	}
	return a.state.putChoice(state)
}

func (a *Adapter) Send(ctx context.Context, msg messages.OutboundMessage) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if msg.ChatID == "" {
		return errors.New("telegram: outbound ChatID is empty")
	}
	// Drop empty text for kinds that carry plain text. Avoids
	// Telegram rejecting sendMessage with empty text, and keeps
	// the topic from accumulating empty bubbles (matches Feishu
	// adapter's pre-F-44 silent-drop behaviour).
	switch msg.Kind {
	case messages.OutReply, messages.OutCommandReply, messages.OutResult, messages.OutThinking, messages.OutInit:
		if strings.TrimSpace(msg.Text) == "" {
			return nil
		}
	}
	topicID := a.sessionTopicID(msg.ChatID)
	switch msg.Kind {
	case messages.OutChoice:
		return a.sendChoice(ctx, msg)
	case messages.OutChoicePatch:
		return a.patchChoice(ctx, msg)
	case messages.OutHeartbeat:
		if topicID > 0 {
			// Ensure placeholder exists so we always have a
			// stable anchor to PATCH. handleMessage normally
			// creates it; this catches the case where it
			// failed or the heartbeat raced ahead.
			if messageID, err := a.ensurePlaceholderForHeartbeat(ctx, msg.ChatID, topicID); err == nil && messageID > 0 {
				text := "🤖 Working..."
				if msg.Heartbeat != nil {
					text = heartbeatText(msg.Heartbeat)
				}
				return a.editTelegramMessage(ctx, msg.ChatID, messageID, renderInlineText(text), nil)
			}
		}
		// Fallback: standalone heartbeat bubble in the topic.
		return a.sendRenderedText(ctx, msg.ChatID, topicID, msg.Text)
	case messages.OutMessageState:
		if msg.MessageState == nil || msg.MessageState.MessageID == "" {
			return errors.New("telegram: OutMessageState missing MessageState or MessageID")
		}
		messageID, err := strconv.Atoi(msg.MessageState.MessageID)
		if err != nil {
			return err
		}
		return a.setMessageReaction(ctx, msg.ChatID, messageID, msg.MessageState.Emoji)
	case messages.OutMessageStateRemoved:
		if msg.MessageState == nil || msg.MessageState.MessageID == "" {
			return errors.New("telegram: OutMessageStateRemoved missing MessageState or MessageID")
		}
		messageID, err := strconv.Atoi(msg.MessageState.MessageID)
		if err != nil {
			return err
		}
		return a.setMessageReaction(ctx, msg.ChatID, messageID, "")
	case messages.OutToolStart, messages.OutToolEnd:
		return a.sendRenderedText(ctx, msg.ChatID, topicID, formatTool(msg))
	case messages.OutTaskCreate, messages.OutTaskUpdate:
		return a.sendRenderedText(ctx, msg.ChatID, topicID, formatTaskList(msg.TaskList))
	case messages.OutError:
		text := msg.Text
		if msg.Diagnostic != nil && msg.Diagnostic.StderrTail != "" {
			text += "\n\n<pre>" + escapeHTML(msg.Diagnostic.StderrTail) + "</pre>"
		}
		return a.sendRenderedText(ctx, msg.ChatID, topicID, text)
	case messages.OutInit:
		// Silent drop — matches feishu F-44. The Init payload
		// (session_id, model, agent name, …) is still on the
		// wire so callers can read it, but we don't surface a
		// "Agent: x · Model: y · Session: z" bubble in the
		// topic. Session identity already shows up in the
		// placeholder text when the heartbeat-driven PATCH
		// upgrades it (future work — see docs/channel/telegram.md
		// §14 Known Limitations).
		return nil
	default:
		return a.sendRenderedText(ctx, msg.ChatID, topicID, msg.Text)
	}
}

func (a *Adapter) OnPromptEnded(ctx context.Context, chatID, userMsgID string) {
	if chatID == "" {
		return
	}
	if topicID := a.sessionTopicID(chatID); topicID > 0 {
		if state, ok := a.state.topic(chatID, topicID); ok && state.PlaceholderMessageID > 0 {
			_ = a.editTelegramMessage(ctx, chatID, state.PlaceholderMessageID, "<b>✅ Completed</b>", nil)
		}
	}
}

func (a *Adapter) HealthSnapshot() (string, json.RawMessage, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	payload, _ := json.Marshal(map[string]any{"mode": a.config.Mode, "username": a.botName, "connected": a.started && !a.stopped, "offset": a.offset})
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

func (a *Adapter) startWebhook(ctx context.Context) error {
	if a.config.WebhookURL == "" {
		return errors.New("telegram: webhook_url is required in webhook mode")
	}
	if a.config.WebhookSecret == "" {
		return errors.New("telegram: webhook_secret is required in webhook mode")
	}
	if err := a.api.call(ctx, "setWebhook", map[string]any{"url": a.config.WebhookURL, "secret_token": a.config.WebhookSecret}, nil); err != nil {
		return err
	}
	parsed, err := url.Parse(a.config.WebhookURL)
	if err != nil || parsed.Host == "" {
		return errors.New("telegram: webhook_url must include a host")
	}
	a.webhook = &http.Server{Addr: parsed.Host, Handler: http.HandlerFunc(a.HandleWebhook)}
	go func() {
		if err := a.webhook.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) && ctx.Err() == nil {
			a.logger.Warn("telegram: webhook server failed", "err", err)
		}
	}()
	return nil
}

func (a *Adapter) HandleWebhook(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writer.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	expected := a.config.WebhookSecret
	provided := request.Header.Get("X-Telegram-Bot-Api-Secret-Token")
	if expected == "" || subtle.ConstantTimeCompare([]byte(expected), []byte(provided)) != 1 {
		writer.WriteHeader(http.StatusUnauthorized)
		return
	}
	var update Update
	if err := json.NewDecoder(request.Body).Decode(&update); err != nil {
		writer.WriteHeader(http.StatusBadRequest)
		return
	}
	a.handleUpdate(request.Context(), update)
	writer.WriteHeader(http.StatusOK)
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

var _ channel.Channel = (*Adapter)(nil)
