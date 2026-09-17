package discord

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/config"
	"github.com/cnlangzi/nightme/internal/messages"
)

// userAgent is the User-Agent the API client + Gateway dial
// stamp on every outbound call. Discord's developer portal
// requires the format "DiscordBot (<url>, <version>)" — nightme
// uses the GitHub URL as a stable identifier. The version
// component is omitted (Discord accepts either form; the URL is
// the part Discord's rate-limit-abuse heuristics check).
const userAgent = "DiscordBot (https://github.com/cnlangzi/nightme)"

// attachmentDownloadTimeout bounds the goroutine that fetches
// each inbound message's attachments. Discord caps attachments
// at 25 MiB; a CDN stall must not pin the worker indefinitely,
// so the whole message (all attachments) shares this budget.
const attachmentDownloadTimeout = 60 * time.Second

// Adapter implements channel.Channel for Discord.
type Adapter struct {
	name string
	cfg  config.DiscordConfig

	api restClient
	gw  *gatewayClient

	state     *stateStore
	incoming  chan messages.InboundMessage
	logger    *slog.Logger
	botUserID string
	botName   string

	// dataDir is the parent directory for both the persisted state
	// file (state.json) and the per-chat attachment cache
	// (dataDir/discord/<chatID>/<messageID>/). Empty when the
	// operator did not configure Paths.DataDir; attachment
	// downloads then become best-effort (WriteFile fails and the
	// error surfaces on Attachment.Error).
	dataDir string

	// choiceStore maps RequestID → in-flight choice state.
	// Populated by sendChoice (OutChoice path) and read by the
	// INTERACTION_CREATE handler (callback.go). In-memory only;
	// Discord's 3 s interaction-token TTL means stale entries
	// wouldn't be useful across restarts anyway.
	choiceStore *choiceStore

	// lastMessageState records the most recent MessageState
	// stamped on a user message id. Used to suppress redundant
	// 👌 reactions when the runtime emits the same transition
	// twice in a row.
	lastStateMu        sync.Mutex
	lastMessageStateDB map[string]string

	// lastResultMessageID records the most recent OutResult
	// message id per (chatID, userMsgID) pair so OnPromptEnded
	// can stamp 🎉 / ❌ on the result bubble instead of the user
	// bubble. Falls back to the user message id when no result
	// was sent (bridge crash, /think off turn, etc.).
	lastResultMu          sync.Mutex
	lastResultMessageIDDB map[string]string

	startStopMu sync.Mutex
	started     bool
	stopped     bool
	gatewayDone chan struct{}
}

// NewAdapter constructs a Discord adapter. Returns an error
// when BotToken is empty (mirrors feishu's "missing credentials"
// contract — BuildAll skips the channel rather than starting a
// half-configured instance).
func NewAdapter(cfg *config.Config) (*Adapter, error) {
	if cfg == nil {
		return nil, errors.New("discord: config is nil")
	}
	token := strings.TrimSpace(cfg.Discord.BotToken)
	if token == "" {
		return nil, errors.New("discord: bot_token is required")
	}

	statePath := ""
	if cfg.Paths.DataDir != "" {
		statePath = filepath.Join(cfg.Paths.DataDir, "discord_state.json")
	}
	state, err := newStateStore(statePath)
	if err != nil {
		return nil, err
	}

	limiter := NewLimiter(nil, nil)
	rest := newRESTClient(token, limiter, DefaultRetryConfig, userAgent)
	gw := &gatewayClient{
		logger: nil,
		api:    rest,
		state:  state,
		cfg: gatewaySnapshot{
			Token:     token,
			Intents:   cfg.Discord.Intents,
			UserAgent: userAgent,
		},
	}

	a := &Adapter{
		name:                  "discord",
		cfg:                   cfg.Discord,
		api:                   rest,
		gw:                    gw,
		state:                 state,
		incoming:              make(chan messages.InboundMessage, 64),
		logger:                slog.Default(),
		dataDir:               cfg.Paths.DataDir,
		choiceStore:           newChoiceStore(),
		lastMessageStateDB:    make(map[string]string),
		lastResultMessageIDDB: make(map[string]string),
	}
	gw.onReady = a.onReady
	gw.onMessage = a.onMessage
	gw.onInteraction = a.onInteraction
	return a, nil
}

// Name implements channel.Channel.
func (a *Adapter) Name() string { return a.name }

// Incoming implements channel.Channel.
func (a *Adapter) Incoming() <-chan messages.InboundMessage { return a.incoming }

// SetLogger implements channel.Channel.
func (a *Adapter) SetLogger(l *slog.Logger) {
	if l == nil {
		l = slog.Default()
	}
	a.logger = l
	a.gw.logger = l
}

// Start implements channel.Channel.
func (a *Adapter) Start(ctx context.Context) error {
	a.startStopMu.Lock()
	defer a.startStopMu.Unlock()
	if a.started {
		return nil
	}
	if a.stopped {
		return errors.New("discord: adapter already stopped")
	}

	// Fail fast on bad token — GetMe returns a 401 via apiError,
	// which IsTransient classifies as terminal (4xx), so retry
	// gives up immediately and Start surfaces the error.
	me, err := a.api.GetMe(ctx)
	if err != nil {
		return err
	}
	a.botUserID = string(me.ID)
	a.botName = me.Username
	if me.GlobalName != "" {
		a.botName = me.GlobalName
	}
	a.logger.Info("discord: bot verified",
		"bot_id", a.botUserID,
		"username", a.botName,
	)

	done := make(chan struct{})
	a.gatewayDone = done
	a.started = true
	go func() {
		defer close(done)
		if err := a.gw.run(ctx); err != nil {
			a.logger.Error("discord gateway terminated", "err", err.Error())
		}
	}()
	return nil
}

// Stop implements channel.Channel.
func (a *Adapter) Stop(ctx context.Context) error {
	a.startStopMu.Lock()
	defer a.startStopMu.Unlock()
	if a.stopped {
		return nil
	}
	a.stopped = true
	// The gateway's run loop honours ctx cancellation (its read
	// deadlines + select on ctx.Done() guarantee prompt exit).
	// Closing a.incoming wakes any channel.Buffered readers
	// sitting on the Gateway side.
	if a.gatewayDone != nil {
		select {
		case <-a.gatewayDone:
		case <-ctx.Done():
		}
	}
	close(a.incoming)
	return nil
}

// BuildBlocks implements channel.Channel.
func (a *Adapter) BuildBlocks(text string, attachments []messages.Attachment) []agent.ContentBlock {
	blocks := make([]agent.ContentBlock, 0, len(attachments)+1)
	if text != "" {
		blocks = append(blocks, agent.ContentBlock{Type: agent.ContentText, Text: text})
	}
	for _, at := range attachments {
		blockType := agent.ContentFile
		if strings.HasPrefix(strings.ToLower(at.MimeType), "image/") {
			blockType = agent.ContentImage
		}
		blocks = append(blocks, agent.ContentBlock{
			Type:      blockType,
			Path:      at.LocalPath,
			MediaType: at.MimeType,
		})
	}
	return blocks
}

// HealthSnapshot implements channel.Channel.
func (a *Adapter) HealthSnapshot() (string, json.RawMessage, error) {
	a.startStopMu.Lock()
	connected := a.started && !a.stopped
	a.startStopMu.Unlock()

	sessionID, lastSeq, _, intentsVersion := a.state.snapshot()
	payload, err := json.Marshal(map[string]any{
		"username":        a.botName,
		"bot_id":          a.botUserID,
		"connected":       connected,
		"session_id":      sessionID,
		"last_seq":        lastSeq,
		"intents":         a.cfg.Intents,
		"intents_version": intentsVersion,
	})
	if err != nil {
		return a.name, nil, err
	}
	return a.name, payload, nil
}

// onReady is wired into gatewayClient.onReady. Caches the bot
// user id and name so handleMessageCreate / HealthSnapshot can
// read them without an extra REST round trip.
func (a *Adapter) onReady(u User) {
	a.botUserID = string(u.ID)
	a.botName = u.Username
	if u.GlobalName != "" {
		a.botName = u.GlobalName
	}
}

// onMessage is wired into gatewayClient.onMessage. Maps a
// Discord MESSAGE_CREATE into messages.InboundMessage and
// publishes it on a.incoming.
//
// Blocks is left nil: the runtime dispatcher (see
// internal/runtime/dispatcher.go) falls back to
// ch.BuildBlocks(msg.Text, msg.Attachments) when Blocks is
// empty, so the adapter shouldn't pre-populate — otherwise the
// dispatcher would double-build.
//
// Attachments are downloaded in a goroutine so a slow / stalled
// CDN response cannot block the gateway read loop and starve
// heartbeats (which would force a Discord disconnect). The
// dispatcher reads from a.incoming on its own goroutine, so the
// async publish still lands in arrival order.
//
// The F-14 invariant (messages/inbound.go:42-46) — LocalPath
// populated before the InboundMessage reaches the dispatcher —
// is upheld because downloadAndPublish only publishes after the
// downloads complete.
func (a *Adapter) onMessage(msg *Message) {
	if msg == nil {
		return
	}
	if a.botUserID != "" && string(msg.Author.ID) == a.botUserID {
		return
	}
	if !isSupportedChannelType(msg.ChannelType) {
		return
	}
	chatID := sessionChatID(string(msg.ChannelID))
	inbound := messages.InboundMessage{
		ChatID:     chatID,
		UserID:     string(msg.Author.ID),
		Text:       msg.Content,
		MessageID:  string(msg.ID),
		Time:       msg.Timestamp,
		ReplyTo:    replyTargetOf(msg),
		HasMention: a.computeHasMention(msg),
	}
	if len(msg.Attachments) == 0 {
		a.publish(inbound)
		return
	}
	go a.downloadAndPublish(msg, chatID, inbound)
}

// downloadAndPublish fetches every attachment off the gateway
// goroutine and publishes the populated InboundMessage when the
// downloads complete. Failures don't block the publish — each
// attachment entry carries Error so the dispatcher can surface
// the failure to the user.
func (a *Adapter) downloadAndPublish(msg *Message, chatID string, inbound messages.InboundMessage) {
	ctx, cancel := context.WithTimeout(context.Background(), attachmentDownloadTimeout)
	defer cancel()
	inbound.Attachments = a.downloadAttachments(ctx, msg, chatID)
	a.publish(inbound)
}

func (a *Adapter) publish(inbound messages.InboundMessage) {
	select {
	case a.incoming <- inbound:
	default:
		a.logger.Warn("discord: incoming channel full; dropping message",
			"chat_id", inbound.ChatID,
			"message_id", inbound.MessageID,
		)
	}
}

// computeHasMention mirrors the channel.go InboundMessage.HasMention
// contract: DMs are always addressed; group messages require
// @mention or slash-command prefix; mention_everyone also counts.
func (a *Adapter) computeHasMention(msg *Message) bool {
	if msg.ChannelType == 1 {
		return true
	}
	if msg.MentionEveryone {
		return true
	}
	if a.botUserID != "" {
		for _, m := range msg.Mentions {
			if string(m.ID) == a.botUserID {
				return true
			}
		}
	}
	if strings.HasPrefix(strings.TrimSpace(msg.Content), "/") {
		return true
	}
	return false
}

// replyTargetOf picks the channel-native reply target. Discord
// surfaces reply linkage in two places; preferred order is the
// explicit message_reference (DEFAULT / 0) and the already-
// resolved referenced_message (REPLY / 19) when the former is
// missing.
func replyTargetOf(msg *Message) string {
	if msg.MessageReference != nil && msg.MessageReference.MessageID != "" {
		return string(msg.MessageReference.MessageID)
	}
	if msg.ReferencedMessage != nil && msg.ReferencedMessage.ID != "" {
		return string(msg.ReferencedMessage.ID)
	}
	return ""
}

// rememberMessageState records the most recent MessageState for a
// user message id so repeat transitions can be suppressed.
func (a *Adapter) rememberMessageState(messageID, state string) {
	a.lastStateMu.Lock()
	a.lastMessageStateDB[messageID] = state
	a.lastStateMu.Unlock()
}

// previousMessageState returns the last state we stamped on this
// message id, if any.
func (a *Adapter) previousMessageState(messageID string) (string, bool) {
	a.lastStateMu.Lock()
	defer a.lastStateMu.Unlock()
	s, ok := a.lastMessageStateDB[messageID]
	return s, ok
}

// resultKey is the (chatID, userMsgID) tuple used to remember
// which result message id corresponds to a user message. The two
// fields are joined with a NUL byte — neither side ever carries
// one in practice, so the concatenation is unambiguous.
func resultKey(chatID, userMsgID string) string {
	return chatID + "\x00" + userMsgID
}

// rememberResultMessageID records the most recent OutResult
// message id for a (chatID, userMsgID) pair so OnPromptEnded can
// stamp 🎉 / ❌ on the result bubble. Called from sendResult
// (send.go) on every successful OutResult.
func (a *Adapter) rememberResultMessageID(chatID, userMsgID, messageID string) {
	if chatID == "" || messageID == "" {
		return
	}
	a.lastResultMu.Lock()
	defer a.lastResultMu.Unlock()
	a.lastResultMessageIDDB[resultKey(chatID, userMsgID)] = messageID
}

// lastResultMessageID returns the recorded result message id for
// (chatID, userMsgID), or "" when none was sent. OnPromptEnded
// falls back to the user message id in that case.
//
// One-shot consumption: the entry is deleted after the read so
// the map cannot grow unboundedly across long-running daemons.
// A second OnPromptEnded for the same userMsgID therefore returns
// "" and falls back to the user message id — exactly the
// "no result was sent" semantics we want for repeat turn-end
// events (the runtime occasionally re-emits).
func (a *Adapter) lastResultMessageID(chatID, userMsgID string) string {
	key := resultKey(chatID, userMsgID)
	a.lastResultMu.Lock()
	defer a.lastResultMu.Unlock()
	value, ok := a.lastResultMessageIDDB[key]
	if !ok {
		return ""
	}
	delete(a.lastResultMessageIDDB, key)
	return value
}
