// Package feishu implements the Feishu channel adapter.
package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/cnlangzi/nightme/internal/gateway"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcallback "github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	larkdispatcher "github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"

	"github.com/cnlangzi/nightme/internal/channel"
	"github.com/cnlangzi/nightme/internal/config"
)

const maxMessageBytes = 3800

// sendMessageFunc is kept behind the adapter so unit tests can exercise the
// channel without making an HTTP request to Feishu.
type sendMessageFunc func(ctx context.Context, chatID, msgType, content string) (string, error)

// Adapter is the Feishu implementation of channel.Channel.
//
// The SDK's WebSocket Start method blocks for the lifetime of the connection,
// so Start launches it in a goroutine. The REST client is used synchronously by
// SendMessage; callers that need batching should use SendLongMessage.
type Adapter struct {
	client     *larkws.Client
	larkClient *lark.Client
	incoming   chan channel.Message
	cfg        *config.Config
	cancel     context.CancelFunc

	mu             sync.RWMutex
	publishMu      sync.Mutex
	done           chan struct{}
	started        bool
	stopped        bool
	incomingClosed bool
	stopDone       chan struct{}
	wsDone         chan struct{}

	// logger is the structured-log target for outgoing-message
	// traces. Each send / reaction / update emits an info-level
	// line so the CLI surface shows both halves of a conversation
	// (the inbound "received:" line is logged by the channel
	// pump). Defaults to slog.Default(); settable via SetLogger.
	logger *slog.Logger

	// These hooks have production defaults and are intentionally kept as
	// fields so tests can replace the network boundary with a small function.
	wsStart  func(context.Context) error
	wsClose  func()
	sendFunc sendMessageFunc
}

// NewAdapter constructs a Feishu adapter and validates the credentials needed
// by both the WebSocket and REST clients.
func NewAdapter(cfg *config.Config) (*Adapter, error) {
	if cfg == nil {
		return nil, errors.New("feishu: config is required")
	}
	if strings.TrimSpace(cfg.Feishu.AppID) == "" {
		return nil, errors.New("feishu: app_id is required; run `nightme auth login feishu`")
	}
	if strings.TrimSpace(cfg.Feishu.AppSecret) == "" {
		return nil, errors.New("feishu: app_secret is required; run `nightme auth login feishu`")
	}

	a := &Adapter{
		incoming: make(chan channel.Message, 128),
		cfg:      cfg,
		done:     make(chan struct{}),
		logger:   slog.Default(),
	}

	handler := larkdispatcher.NewEventDispatcher(
		cfg.Feishu.VerificationToken,
		cfg.Feishu.EncryptKey,
	).
		OnP2MessageReceiveV1(a.handleMessage).
		// Interactive-card button clicks (e.g. permission card Allow/Deny).
		// Required by the card.action.trigger callback registered in
		// DefaultAddons; without this handler registration the bot
		// would log "no handler for card.action.trigger" and the user
		// clicks would be lost.
		OnP2CardActionTrigger(a.handleCardAction).
		// User-driven reactions on bot messages are not a designed
		// input channel (no /react command, no ack/cancel UX). Swallow
		// the event so the SDK doesn't log "not found handler". The
		// reaction event subscription is intentionally absent from
		// DefaultAddons in internal/auth/feishu/feishu.go.
		OnP2MessageReactionCreatedV1(func(_ context.Context, _ *larkim.P2MessageReactionCreatedV1) error {
			return nil
		}).
		// Pair with the created handler above so reaction removal
		// (e.g. user un-clicks an emoji) doesn't generate spurious
		// "not found handler" errors either.
		OnP2MessageReactionDeletedV1(func(_ context.Context, _ *larkim.P2MessageReactionDeletedV1) error {
			return nil
		})

	a.client = larkws.NewClient(
		cfg.Feishu.AppID,
		cfg.Feishu.AppSecret,
		larkws.WithEventHandler(handler),
		larkws.WithOnReady(func() {
			log.Printf("Feishu WebSocket connected")
		}),
		larkws.WithOnError(func(err error) {
			if err != nil {
				log.Printf("Feishu WebSocket error: %v", err)
			}
		}),
	)
	a.larkClient = lark.NewClient(cfg.Feishu.AppID, cfg.Feishu.AppSecret)
	a.wsStart = a.client.Start
	a.wsClose = a.client.Close
	a.sendFunc = a.sendViaLark
	return a, nil
}

// Name returns the stable channel name used by the daemon.
func (a *Adapter) Name() string { return "feishu" }

// DownloadAttachments is a convenience wrapper for callers that
// hold an *Adapter and don't want to dig out the lark client.
// See the package-level DownloadAttachments for semantics.
func (a *Adapter) DownloadAttachments(ctx context.Context,
	messageID string, atts []channel.Attachment, sessionID string,
) DownloadResult {
	return DownloadAttachments(ctx, a.larkClient, messageID, atts, sessionID)
}

// Start starts the Feishu WebSocket receive loop. The SDK itself handles
// reconnects; this method only owns the lifetime context and goroutine.
func (a *Adapter) Start(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}

	a.mu.Lock()
	if a.stopped {
		a.mu.Unlock()
		return errors.New("feishu: adapter is stopped")
	}
	if a.started {
		a.mu.Unlock()
		return nil
	}
	if a.incoming == nil {
		a.mu.Unlock()
		return errors.New("feishu: incoming channel is nil")
	}
	start := a.wsStart
	if start == nil && a.client != nil {
		start = a.client.Start
	}
	if start == nil {
		a.mu.Unlock()
		return errors.New("feishu: WebSocket client is nil")
	}
	runCtx, cancel := context.WithCancel(ctx)
	a.cancel = cancel
	a.started = true
	a.wsDone = make(chan struct{})
	wsDone := a.wsDone
	a.mu.Unlock()

	go func() {
		defer close(wsDone)
		if err := start(runCtx); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("Feishu WebSocket stopped: %v", err)
		}
	}()
	return nil
}

// Stop cancels the adapter context, closes the SDK connection, and closes the
// normalized incoming stream. It is safe to call more than once.
func (a *Adapter) Stop(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}

	a.mu.Lock()
	if a.stopped {
		stopDone := a.stopDone
		a.mu.Unlock()
		if stopDone != nil {
			select {
			case <-stopDone:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}
	a.stopped = true
	if a.done == nil {
		a.done = make(chan struct{})
	}
	close(a.done)
	stopDone := make(chan struct{})
	a.stopDone = stopDone
	cancel := a.cancel
	closeWS := a.wsClose
	if closeWS == nil && a.client != nil {
		closeWS = a.client.Close
	}
	wsDone := a.wsDone
	a.mu.Unlock()
	defer close(stopDone)

	if cancel != nil {
		cancel()
	}
	if closeWS != nil {
		closeWS()
	}

	var waitErr error
	// Wait only when the SDK loop is already on its way out. The v3 SDK's
	// Start method intentionally blocks after Close, so waiting unconditionally
	// here would make a graceful daemon shutdown hang forever.
	if wsDone != nil {
		select {
		case <-wsDone:
		default:
			select {
			case <-wsDone:
			case <-ctx.Done():
				waitErr = ctx.Err()
			default:
			}
		}
	}

	// Stop publishing before closing the channel. A handler can be blocked on
	// a full incoming buffer; closing done above makes it leave that send.
	a.publishMu.Lock()
	a.publishMu.Unlock()

	a.mu.Lock()
	if !a.incomingClosed && a.incoming != nil {
		close(a.incoming)
		a.incomingClosed = true
	}
	a.mu.Unlock()
	return waitErr
}

// Incoming returns the adapter's normalized message stream.
func (a *Adapter) Incoming() <-chan channel.Message { return a.incoming }

// Send implements the v0.3 channel.Channel interface. It dispatches
// each OutboundKind to the corresponding Feishu API call:
//
//	OutText              → CreateMessage (msg_type=text)
//	OutToolStart/End     → folded into the per-chat receipt via the
//	                       existing AddReaction / UpdateMessage flow;
//	                       this path is taken by the Feishu channel's
//	                       display strategy (Stage 3 migrates the
//	                       receipt-rendering logic here). Stage 1's
//	                       Send handles OutText directly so the
//	                       existing /help / /run fallback paths keep
//	                       working.
//	OutReaction          → AddReaction on Meta["message_id"]
//	OutReactionRemoved   → DeleteReaction on Meta["reaction_id"]
//	OutCard              → send interactive card via sendContent
//	OutThinking          → dropped (no native Feishu equivalent;
//	                       future: a sub-message indicator)
//	OutTyping            → dropped (no native Feishu equivalent)
//
// Errors from the underlying API are logged and returned; the
// Gateway treats Send as fire-and-ack (no retry).
func (a *Adapter) Send(ctx context.Context, msg gateway.OutboundMessage) error {
	switch msg.Kind {
	case gateway.OutText:
		_, err := a.SendMessageText(ctx, msg.ChatID, msg.Text)
		return err

	case gateway.OutReaction:
		if msg.Reaction == nil || msg.Reaction.EmojiType == "" {
			return errors.New("feishu: OutReaction missing emoji_type")
		}
		messageID, _ := msg.Meta["message_id"].(string)
		if messageID == "" {
			return errors.New("feishu: OutReaction missing message_id in Meta")
		}
		_, err := a.AddReaction(ctx, messageID, msg.Reaction.EmojiType)
		return err

	case gateway.OutReactionRemoved:
		if msg.Reaction == nil || msg.Reaction.ReactionID == "" {
			return errors.New("feishu: OutReactionRemoved missing reaction_id")
		}
		messageID, _ := msg.Meta["message_id"].(string)
		if messageID == "" {
			return errors.New("feishu: OutReactionRemoved missing message_id in Meta")
		}
		return a.DeleteReaction(ctx, messageID, msg.Reaction.ReactionID)

	case gateway.OutCard:
		if msg.Card == nil {
			return errors.New("feishu: OutCard missing card payload")
		}
		content, err := buildInteractiveCard(msg.Card)
		if err != nil {
			return err
		}
		_, err = a.sendContent(ctx, msg.ChatID, interactiveMessageType, content)
		return err

	case gateway.OutToolStart, gateway.OutToolEnd, gateway.OutThinking, gateway.OutTyping:
		// Stage 1: not yet routed through the receipt. Stage 3
		// migrates the Feishu receipt-rendering logic here so each
		// event appends to the same rolling-log message. For now
		// we drop these kinds silently; the Gateway still emits them
		// to its structured log so we know they happened.
		return nil
	}
	return fmt.Errorf("feishu: unsupported outbound kind %v", msg.Kind)
}

// buildInteractiveCard renders a v1 Feishu card from an abstract
// Card. Each option becomes a primary button whose value carries
// the request_id so the inbound Action carries it back. Stage 3
// will move the F-25 permission-card logic here.
func buildInteractiveCard(c *gateway.Card) (string, error) {
	if c.RequestID == "" {
		return "", errors.New("feishu: card missing request_id")
	}
	options := c.Options
	if len(options) == 0 {
		options = []string{"Allow", "Deny"}
	}
	headerJSON, _ := json.Marshal(map[string]any{
		"title":    map[string]any{"tag": "plain_text", "content": c.Title},
		"template": "blue",
	})
	body := c.Title
	if c.Body != "" {
		body = c.Body
	}
	actions := make([]map[string]any, 0, len(options))
	for _, opt := range options {
		v, _ := json.Marshal(map[string]string{
			"request_id": c.RequestID,
			"option":     opt,
		})
		actions = append(actions, map[string]any{
			"tag":   "button",
			"text":  map[string]any{"tag": "plain_text", "content": opt},
			"type":  "primary",
			"value": map[string]any{"key": string(v)},
		})
	}
	card := map[string]any{
		"config":   map[string]any{"wide_screen_mode": true},
		"header":   json.RawMessage(headerJSON),
		"elements": []map[string]any{
			{"tag": "div", "text": map[string]any{"tag": "lark_md", "content": body}},
			{"tag": "action", "actions": actions},
		},
	}
	b, err := json.Marshal(card)
	if err != nil {
		return "", err
	}
	envelope := map[string]any{"card": json.RawMessage(b)}
	eb, err := json.Marshal(envelope)
	if err != nil {
		return "", err
	}
	return string(eb), nil
}

// SetLogger swaps the structured logger used for outgoing-message
// traces. Pass nil to fall back to slog.Default().
func (a *Adapter) SetLogger(l *slog.Logger) {
	if l == nil {
		l = slog.Default()
	}
	a.mu.Lock()
	a.logger = l
	a.mu.Unlock()
}

// logOutgoing emits one info-level line per outgoing Feishu call so
// the CLI surface shows both halves of the conversation. The
// inbound "received: …" line is printed by the channel pump in
// cmd/nightme/run.go; without this trace the user sees their own
// inputs but not what the bot sent back.
//
// `kind` is one of "send_text", "add_reaction", "delete_reaction",
// "update_message", "send_card". `target` is the chat ID for text
// sends and the message ID for reactions / updates. `id` is the
// created resource ID (message ID, reaction ID) or "" if not
// applicable. err is non-nil for failed calls.
func (a *Adapter) logOutgoing(kind, target, id string, err error) {
	a.mu.RLock()
	logger := a.logger
	a.mu.RUnlock()
	if logger == nil {
		return
	}
	attrs := []slog.Attr{
		slog.String("kind", kind),
		slog.String("target", target),
		slog.String("id", id),
	}
	if err != nil {
		logger.LogAttrs(context.Background(), slog.LevelWarn, "feishu: outgoing failed", append(attrs, slog.String("err", err.Error()))...)
		return
	}
	logger.LogAttrs(context.Background(), slog.LevelInfo, "feishu: outgoing", attrs...)
}

// logInbound emits one info-level line per inbound Feishu
// message so the CLI surface shows the user message that
// triggered the handler. Companion to logOutgoing (which traces
// the bot side of the conversation).
func (a *Adapter) logInbound(msg channel.Message) {
	a.mu.RLock()
	logger := a.logger
	a.mu.RUnlock()
	if logger == nil {
		return
	}
	logger.LogAttrs(context.Background(), slog.LevelInfo, "feishu: incoming",
		slog.String("chat_id", msg.ChatID),
		slog.String("message_id", msg.MessageID),
		slog.String("text", msg.Text),
		slog.Int("attachments", len(msg.Attachments)),
	)
}

// SendMessage sends one text message to chatID and returns the
// created message ID. The Channel interface in
// internal/channel.Channel accepts (ctx, chatID, text) -> error
// for backwards compat, so the public wrapper drops the message
// ID on error and uses a hidden return-path for callers that need
// the ID (see SendMessageText for the receipt code path).
func (a *Adapter) SendMessage(ctx context.Context, chatID, text string) error {
	_, err := a.SendMessageText(ctx, chatID, text)
	return err
}

// SendMessageText is the message-ID-returning variant of SendMessage.
// Used by MessageReceipt (F-25) so the reply text line can be edited
// in place via UpdateMessage on subsequent state transitions.
//
// Returns (messageID, error). On error, messageID is "".
func (a *Adapter) SendMessageText(ctx context.Context, chatID, text string) (string, error) {
	if strings.TrimSpace(chatID) == "" {
		return "", errors.New("feishu: chat_id is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	content, err := json.Marshal(struct {
		Text string `json:"text"`
	}{Text: text})
	if err != nil {
		return "", fmt.Errorf("feishu: encode text: %w", err)
	}
	msgID, err := a.sendContent(ctx, chatID, larkim.MsgTypeText, string(content))
	a.logOutgoing("send_text", chatID, msgID, err)
	return msgID, err
}

// AddReaction adds a reaction emoji to an existing message and
// returns the new reaction's ID. Used by MessageReceipt for the F-25
// dual-track status display (see receipt.go).
//
// The reaction ID is needed because the only way to "change" a
// reaction in Feishu is Delete + Create — Feishu does not expose a
// UpdateReaction API. The caller stores the returned ID so a later
// state transition can swap the emoji by deleting the old reaction
// and creating a new one in the same message row.
//
// Errors are non-fatal — best-effort. On error the empty string is
// returned; the caller falls back to "leave old reaction in place"
// rather than dropping the visual cue.
//
// reactionType must be a Feishu Emoji type. The built-in emojis
// (THumbs, SMILE, etc.) are exposed as constants on the SDK's
// larkim package; for arbitrary unicode emoji we pass them as a
// string via larkim.NewEmoji builder.
func (a *Adapter) AddReaction(ctx context.Context, messageID, reactionType string) (string, error) {
	if a.larkClient == nil || a.larkClient.Im == nil || a.larkClient.Im.V1 == nil || a.larkClient.Im.V1.MessageReaction == nil {
		return "", errors.New("feishu: REST client not initialized")
	}
	body := larkim.NewCreateMessageReactionReqBodyBuilder().
		ReactionType(larkim.NewEmojiBuilder().EmojiType(reactionType).Build()).
		Build()
	req := larkim.NewCreateMessageReactionReqBuilder().
		MessageId(messageID).
		Body(body).
		Build()
	resp, err := a.larkClient.Im.V1.MessageReaction.Create(ctx, req)
	if err != nil {
		return "", fmt.Errorf("feishu: add reaction: %w", err)
	}
	if resp == nil || !resp.Success() {
		code := 0
		if resp != nil {
			code = resp.Code
		}
		return "", fmt.Errorf("feishu: add reaction failed with code %d", code)
	}
	var rid string
	if resp.Data != nil && resp.Data.ReactionId != nil {
		rid = *resp.Data.ReactionId
	}
	a.logOutgoing("add_reaction", messageID, rid, nil)
	return rid, nil
}

// DeleteReaction removes a reaction by its ID. Used by
// MessageReceipt to swap the state emoji (⏳ → 🔄 → ✅) — Feishu
// has no UpdateReaction API, so we delete the old one and create a
// new one in the same message row. The user always sees ONE
// reaction emoji per user message.
//
// Errors are non-fatal — best-effort. The user is better off seeing
// a stale emoji (Waiting when actually Executing) than seeing an
// error overlay. On failure the caller can fall back to leaving the
// old reaction in place.
func (a *Adapter) DeleteReaction(ctx context.Context, messageID, reactionID string) error {
	if a.larkClient == nil || a.larkClient.Im == nil || a.larkClient.Im.V1 == nil || a.larkClient.Im.V1.MessageReaction == nil {
		return errors.New("feishu: REST client not initialized")
	}
	if strings.TrimSpace(reactionID) == "" {
		return errors.New("feishu: reaction_id is required")
	}
	req := larkim.NewDeleteMessageReactionReqBuilder().
		MessageId(messageID).
		ReactionId(reactionID).
		Build()
	resp, err := a.larkClient.Im.V1.MessageReaction.Delete(ctx, req)
	if err != nil {
		err = fmt.Errorf("feishu: delete reaction: %w", err)
		a.logOutgoing("delete_reaction", messageID, reactionID, err)
		return err
	}
	if resp == nil || !resp.Success() {
		code := 0
		if resp != nil {
			code = resp.Code
		}
		err = fmt.Errorf("feishu: delete reaction failed with code %d", code)
		a.logOutgoing("delete_reaction", messageID, reactionID, err)
		return err
	}
	a.logOutgoing("delete_reaction", messageID, reactionID, nil)
	return nil
}

// UpdateMessage edits an existing text message's content in-place.
// Used by MessageReceipt to keep the reply line single-message
// across heartbeat ticks (per F-25 spec: "永远只有一行").
//
// Feishu restrictions (per official docs):
//   - Only text and post (rich-text) message types can be updated
//   - Messages older than 48h may not be editable
//   - Each message can be updated at most 20 times
//
// Errors are non-fatal — the receipt falls back to a fresh message
// on update failure.
func (a *Adapter) UpdateMessage(ctx context.Context, messageID, text string) error {
	if a.larkClient == nil || a.larkClient.Im == nil || a.larkClient.Im.V1 == nil || a.larkClient.Im.V1.Message == nil {
		return errors.New("feishu: REST client not initialized")
	}
	if strings.TrimSpace(messageID) == "" {
		return errors.New("feishu: message_id is required")
	}
	content, err := json.Marshal(struct {
		Text string `json:"text"`
	}{Text: text})
	if err != nil {
		return fmt.Errorf("feishu: encode text: %w", err)
	}
	body := larkim.NewUpdateMessageReqBodyBuilder().
		MsgType(larkim.MsgTypeText).
		Content(string(content)).
		Build()
	req := larkim.NewUpdateMessageReqBuilder().
		MessageId(messageID).
		Body(body).
		Build()
	resp, err := a.larkClient.Im.V1.Message.Update(ctx, req)
	if err != nil {
		err = fmt.Errorf("feishu: update message: %w", err)
		a.logOutgoing("update_message", messageID, "", err)
		return err
	}
	if resp == nil || !resp.Success() {
		code := 0
		if resp != nil {
			code = resp.Code
		}
		err = fmt.Errorf("feishu: update message failed with code %d", code)
		a.logOutgoing("update_message", messageID, "", err)
		return err
	}
	a.logOutgoing("update_message", messageID, "", nil)
	return nil
}

// SendLongMessage splits text at newline boundaries where possible and sends
// each resulting chunk. The 3.8 KiB limit leaves room below Feishu's request
// limit for protocol overhead.
func (a *Adapter) SendLongMessage(ctx context.Context, chatID, text string) error {
	for _, part := range splitLongMessage(text, maxMessageBytes) {
		if ctx != nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
		}
		if err := a.SendMessage(ctx, chatID, part); err != nil {
			return err
		}
	}
	return nil
}

// sendContent sends an arbitrary Feishu message type and returns the
// created message ID. Renderer uses this for interactive permission
// cards while the public Channel API remains text-only.
//
// Returns "" + error on failure. Empty message ID on success is
// possible if the API omits it (defensive — should not happen).
func (a *Adapter) sendContent(ctx context.Context, chatID, msgType, content string) (string, error) {
	a.mu.RLock()
	send := a.sendFunc
	if send == nil && a.larkClient != nil {
		send = a.sendViaLark
	}
	a.mu.RUnlock()
	if send == nil {
		return "", errors.New("feishu: REST client is nil")
	}
	return send(ctx, chatID, msgType, content)
}

func (a *Adapter) sendViaLark(ctx context.Context, chatID, msgType, content string) (string, error) {
	if a.larkClient == nil || a.larkClient.Im == nil || a.larkClient.Im.V1 == nil || a.larkClient.Im.V1.Message == nil {
		return "", errors.New("feishu: REST client is nil")
	}
	body := &larkim.CreateMessageReqBody{
		ReceiveId: &chatID,
		MsgType:   &msgType,
		Content:   &content,
	}
	req := larkim.NewCreateMessageReqBuilder().
		ReceiveIdType(larkim.CreateMessageV1ReceiveIDTypeChatId).
		Body(body).
		Build()
	resp, err := a.larkClient.Im.V1.Message.Create(ctx, req)
	if err != nil {
		return "", fmt.Errorf("feishu: create message: %w", err)
	}
	if resp == nil {
		return "", errors.New("feishu: create message returned nil response")
	}
	if !resp.Success() {
		return "", fmt.Errorf("feishu: create message failed with code %d", resp.Code)
	}
	var msgID string
	if resp.Data != nil && resp.Data.MessageId != nil {
		msgID = *resp.Data.MessageId
	}
	return msgID, nil
}

// handleMessage is registered with the SDK dispatcher for im.message.receive_v1.
func (a *Adapter) handleMessage(ctx context.Context, event *larkim.P2MessageReceiveV1) error {
	if event == nil || event.Event == nil || event.Event.Message == nil {
		return nil
	}
	message := event.Event.Message
	chatID := stringValue(message.ChatId)
	if chatID == "" {
		return nil
	}
	content := stringValue(message.Content)
	chatType := normalizeChatType(stringValue(message.ChatType))
	msgType := stringValue(message.MessageType)
	text, attachments := extractAttachments(msgType, content)
	msg := channel.Message{
		ChatID:      chatID,
		Text:        text,
		UserID:     senderID(event),
		Time:        messageTime(message.CreateTime),
		ChatType:    gateway.ChatType(chatType),
		MessageID:   stringValue(message.MessageId),
		Attachments: attachments,
	}
	if ctx == nil {
		ctx = context.Background()
	}

	// Block here (outside the RW lock) so concurrent Stop closes the
	// done channel and unblocks this send without contending for the
	// same lock Stop acquires to close `incoming`.
	a.mu.RLock()
	if a.stopped || a.incoming == nil {
		a.mu.RUnlock()
		return nil
	}
	done := a.done
	in := a.incoming
	a.mu.RUnlock()

	a.publishMu.Lock()
	defer a.publishMu.Unlock()
	a.mu.RLock()
	if a.stopped || a.incoming == nil {
		a.mu.RUnlock()
		return nil
	}
	a.mu.RUnlock()
	select {
	case in <- msg:
	case <-ctx.Done():
	case <-done:
	}
	return nil
}

// onMessage is a descriptive alias useful to tests and future event handlers.
func (a *Adapter) onMessage(ctx context.Context, event *larkim.P2MessageReceiveV1) error {
	return a.handleMessage(ctx, event)
}

// handleCardAction is the entry point for the card.action.trigger
// callback. It is wired in via OnP2CardActionTrigger at construction
// and paired with the matching callback registration in
// DefaultAddons.
//
// v0.2 scope: the handler returns a Toast acknowledging the click.
// The actual click-value → permission-decision routing (e.g. updating
// the SessionManager's pending permission state for Allow/Deny) is
// wired in a follow-up; for now we surface a user-visible ack so the
// button doesn't look broken. The full click-value decoding is
// documented inline so the next commit has a clear seam.
func (a *Adapter) handleCardAction(ctx context.Context, event *larkcallback.CardActionTriggerEvent) (*larkcallback.CardActionTriggerResponse, error) {
	if event == nil || event.Event == nil {
		return &larkcallback.CardActionTriggerResponse{}, nil
	}
	req := event.Event
	choice := "unknown"
	if req.Action != nil {
		if req.Action.Option != "" {
			choice = req.Action.Option
		} else if req.Action.Name != "" {
			choice = req.Action.Name
		}
	}
	log.Printf("feishu: card action received chat=%s action=%s",
		req.Context.OpenChatID, choice)
	return &larkcallback.CardActionTriggerResponse{
		Toast: &larkcallback.Toast{
			Type:    "info",
			Content: "Recorded: " + choice,
		},
	}, nil
}

func senderID(event *larkim.P2MessageReceiveV1) string {
	if event == nil || event.Event == nil || event.Event.Sender == nil || event.Event.Sender.SenderId == nil {
		return ""
	}
	id := event.Event.Sender.SenderId
	if id.OpenId != nil {
		return *id.OpenId
	}
	if id.UserId != nil {
		return *id.UserId
	}
	if id.UnionId != nil {
		return *id.UnionId
	}
	return ""
}

func messageText(content string) string {
	if content == "" {
		return ""
	}
	var payload struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(content), &payload); err == nil && payload.Text != "" {
		return payload.Text
	}
	// The EventMessage.Content field is a JSON string; if it is not the
	// structured `{text: ...}` shape (e.g. image / post), preserve the
	// raw text so downstream consumers can decide.
	return content
}

// normalizeChatType maps a Feishu chat_type value to the channel-
// neutral constants in internal/channel. Feishu sends "p2p" for
// 1-on-1 DM, "group" for normal groups, and "topic_group" for
// topic groups. Unknown values pass through unchanged so future
// Feishu additions don't silently misclassify.
func normalizeChatType(raw string) string {
	switch raw {
	case "p2p":
		return channel.ChatTypeP2P
	case "group":
		return channel.ChatTypeGroup
	case "topic_group":
		return channel.ChatTypeThread
	default:
		return raw
	}
}

func messageTime(value *string) time.Time {
	if value != nil {
		if millis, err := strconv.ParseInt(*value, 10, 64); err == nil {
			return time.UnixMilli(millis)
		}
	}
	return time.Now()
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func splitLongMessage(text string, maxBytes int) []string {
	if maxBytes <= 0 {
		maxBytes = maxMessageBytes
	}
	if len(text) <= maxBytes {
		return []string{text}
	}

	parts := make([]string, 0, len(text)/maxBytes+1)
	var current strings.Builder
	flush := func() {
		if current.Len() == 0 {
			return
		}
		parts = append(parts, current.String())
		current.Reset()
	}

	for len(text) > 0 {
		line := text
		if newline := strings.IndexByte(text, '\n'); newline >= 0 {
			line = text[:newline+1]
			text = text[newline+1:]
		} else {
			text = ""
		}

		for len(line) > 0 {
			piece := line
			if len(piece) > maxBytes {
				cut := maxBytes
				for cut > 0 && !utf8.RuneStart(piece[cut]) {
					cut--
				}
				if cut == 0 {
					cut = maxBytes
				}
				piece = line[:cut]
				line = line[cut:]
			} else {
				line = ""
			}

			if current.Len()+len(piece) > maxBytes {
				flush()
			}
			current.WriteString(piece)
			if current.Len() == maxBytes {
				flush()
			}
		}
	}
	flush()
	return parts
}

var _ channel.Channel = (*Adapter)(nil)
