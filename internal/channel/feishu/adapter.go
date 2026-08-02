// Package feishu implements the Feishu channel adapter.
package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkdispatcher "github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkcallback "github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/channel"
	"github.com/cnlangzi/nightme/internal/config"
	"github.com/cnlangzi/nightme/internal/gateway"
)

const maxMessageBytes = 3800

// interactiveMessageType is the Feishu msg_type for v1 interactive
// cards (permission requests, multi-choice polls, etc.). Used by
// Channel.Send (OutCard kind) and CardActionTrigger callback
// parsing in handleCardAction.
const interactiveMessageType = "interactive"

// receiptMessageType is the Feishu msg_type we use for receipt
// reply cards and standalone text messages. We deliberately use
// the `post` type (rich text) instead of `text` so we can carry a
// `content_v2` body with an `md` tag — Feishu then renders the
// markdown natively, including collapsible code blocks (the
// "N 行代码 >" expand button that Feishu auto-adds when a
// markdown code fence crosses ~4 lines). With `msg_type=text`,
// the markdown is treated as literal characters and the ``` code
// fences never collapse.
//
// Feishu IM message Content schema reference:
// https://open.larkoffice.com/document/server-docs/im-v1/message-content-description/message_content
// — see the "Rich text post" section under msg_type=post.
const receiptMessageType = "post"

// wrapAsPostContent serialises a markdown body as a `post` type
// message Content using the modern content_v2 / md-tag format.
//
// The result is a JSON string suitable for larkim.CreateMessageReq /
// UpdateMessageReq / ReplyMessageReq's Body.Content field. Feishu
// will render the markdown natively (code fences collapse when
// long, bold/italic/lists render as expected, etc.).
//
// Layout choice: one outer paragraph containing a single `md`
// tag with the whole body. Splitting into many paragraphs would
// be wasteful for our rolling-log structure — the receipt already
// uses blank lines and ``` fences as its own separators.
//
// Marshal failure (e.g. unencoded NULs in body) falls back to
// wrapping the body in a Go string literal with safe defaults
// so a failed encode doesn't drop the user's reply entirely.
// This path is essentially unreachable in practice because the
// body is rendered from receipt entries (UTF-8 already sanitised
// at Append time).
func wrapAsPostContent(body string) string {
	content := map[string]any{
		"content_v2": [][]map[string]any{{
			{"tag": "md", "text": body},
		}},
	}
	b, err := json.Marshal(content)
	if err != nil {
		// Defensive fallback: produce a plain post body that
		// still surfaces the text, just without markdown
		// rendering. The receiver gets a readable message
		// rather than a dropped send.
		fb := map[string]any{
			"content": [][]map[string]any{{
				{"tag": "text", "text": body},
			}},
		}
		if bb, err2 := json.Marshal(fb); err2 == nil {
			return string(bb)
		}
		return `{"content":[[{"tag":"text","text":"(unrenderable body)"}]]}`
	}
	return string(b)
}

// sendMessageFunc is kept behind the adapter so unit tests can exercise the
// channel without making an HTTP request to Feishu.
type sendMessageFunc func(ctx context.Context, chatID, msgType, content string) (string, error)

// replyMessageFunc is the ReplyMessage variant of sendMessageFunc.
// Takes the userMsgID being replied to (Feishu puts the reply in
// the thread rooted at that message), the msg_type, and the
// encoded content body. Returns the new message id on success.
type replyMessageFunc func(ctx context.Context, userMsgID, msgType, content string) (string, error)

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

	// mu is the adapter's state mutex. It guards:
	//   - receipts and receiptsByUserMsgID (concurrent writers:
	//     dispatchLoop's SendUserMessage and pumpOutbound's
	//     receiptFor are independent goroutines per chat)
	//   - stopped, incomingClosed (Stop vs incoming handlers)
	//   - logger (SetLogger vs logInbound / logOutgoing readers)
	//
	// Locking discipline: NEVER call a method that takes mu.RLock
	// (logInbound, logOutgoing, MarkExecuting, the receiptFor
	// lookup) while holding mu.Lock. Go's sync.RWMutex is not
	// reentrant — the holder-with-RLock-request pattern is a
	// self-deadlock. See SendUserMessage for the eviction path,
	// which captures the old receipt under the lock and only
	// calls SetCompleted after releasing.
	mu             sync.RWMutex
	publishMu      sync.Mutex
	done           chan struct{}
	started        bool
	stopped        bool
	incomingClosed bool
	stopDone       chan struct{}
	wsDone         chan struct{}

	// logger is the structured-log target for both inbound and
	// outgoing message traces. handleMessage emits one info
	// line per inbound via logInbound; every send / reaction /
	// update emits another via logOutgoing. Defaults to
	// slog.Default(); settable via SetLogger.
	logger *slog.Logger

	// receiptsByUserMsgID is the SOLE receipt index. One receipt
	// per user message; multiple receipts coexist per chat when
	// the gateway's InputBuffer batches several user messages
	// into one agent turn. Adapter.Send looks up the receipt
	// for an OutboundMessage by its ReplyTo (userMsgID); a
	// missing receipt triggers cold-start creation via the
	// Feishu ReplyMessage API so the new card is anchored to
	// the same user message.
	receiptsByUserMsgID map[string]*MessageReceipt

	// receiptFooters caches the static session-attribution prefix
	// per chat ("Agent: <name> | cwd: <path> | Provider: <p>")
	// so we can re-compose the full footer on every OutUsage
	// without re-reading the session metadata. nightme does not
	// track tokens itself — the cache only remembers the
	// session-level parts the gateway injected; the dynamic
	// tokens segment is rebuilt from EventUsage.Meta on each
	// usage event and concatenated onto the cached prefix.
	receiptFooters map[string]string

	// These hooks have production defaults and are intentionally kept as
	// fields so tests can replace the network boundary with a small function.
	wsStart   func(context.Context) error
	wsClose   func()
	sendFunc  sendMessageFunc
	replyFunc replyMessageFunc
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
		incoming:            make(chan channel.Message, 128),
		receiptsByUserMsgID: make(map[string]*MessageReceipt),
		receiptFooters:      make(map[string]string),
		cfg:                 cfg,
		done:                make(chan struct{}),
		logger:              slog.Default(),
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
	a.replyFunc = a.replyViaLark
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
// SendUserMessage is the F-25 entry point used by the gateway's
// fallback handler to hand a user message to the agent. It creates
// a MessageReceipt (⏳ emoji + reply) and returns the receipt so
// the caller can drive state via MarkExecuting (on dispatch) and
// SetCompleted (on agent done). The reply text is the user's caption
// (rendered via BuildForwardedTextFromBlocks so attachment paths
// are visible).
//
// Attachments are NOT downloaded here — that happens earlier in
// the channel pump (downloadAttachments) before SendUserMessage is
// called. The blocks the caller passes should already carry
// LocalPath for any attachments.
func (a *Adapter) SendUserMessage(ctx context.Context, chatID, userMsgID, content string) (*MessageReceipt, error) {
	if a == nil {
		return nil, errors.New("feishu: nil adapter")
	}
	if strings.TrimSpace(chatID) == "" {
		return nil, errors.New("feishu: chat_id is required")
	}
	if strings.TrimSpace(userMsgID) == "" {
		return nil, errors.New("feishu: user_msg_id is required")
	}

	// Idempotent: a duplicate userMsgID reuses the existing
	// receipt (handles retries / dup events).
	a.mu.Lock()
	if existing, ok := a.receiptsByUserMsgID[userMsgID]; ok && existing != nil {
		a.mu.Unlock()
		return existing, nil
	}
	a.mu.Unlock()

	// New message: post the initial ⏳ receipt reply and register
	// the receipt. The reply is posted via ReplyMessage (not
	// SendMessageText) so Feishu shows the native "Reply to
	// <user>: <preview>" header above the body — that visual
	// pairing is how users know which user message the bot is
	// answering. The reaction swap and the receipt body come
	// later via Send.
	replyText := content
	if replyText == "" {
		replyText = "⏳ 等待中"
	}
	msgID, err := a.ReplyMessage(ctx, chatID, userMsgID, replyText)
	if err != nil {
		return nil, fmt.Errorf("feishu: post initial receipt reply: %w", err)
	}

	receipt := NewMessageReceiptForReply(chatID, userMsgID, msgID, a)
	// Evict any prior receipt for this chat (a follow-up user
	// message arrived while a previous turn was still in-flight).
	// The new message becomes the active one.
	//
	// Capture the old receipt under the lock, then call
	// SetCompleted AFTER releasing. SetCompleted → renderLocked →
	// UpdateMessage → logOutgoing needs mu.RLock; holding mu.Lock
	// while requesting it is a self-deadlock (Go's sync.RWMutex is
	// not reentrant). The old receipt stays out of the indexes
	// once we drop the lock, so any concurrent lookup sees only
	// the new receipt and the old SetCompleted runs to completion
	// without interleaving with the new turn's writes.
	//
	// v0.3.1: the receipt model switched from per-chat to
	// per-userMsgID. Multiple receipts can coexist in one chat
	// (buffered batch flushes several user messages as one
	// agent turn; each user message gets its own ReplyCard).
	// Eviction across user messages is no longer the right
	// semantic — instead, the receipt's terminal lifecycle
	// (SetCompleted / SetError on EventDone / EventError) tears
	// down each receipt independently. The receipt stays in
	// the index for the duration of its user message's turn
	// and is removed when the gateway disposes it (see
	// DisposeReceipt below).
	a.mu.Lock()
	a.receiptsByUserMsgID[userMsgID] = receipt
	a.mu.Unlock()

	return receipt, nil
}

// MarkExecuting is the F-25 receipt lifecycle hook the gateway
// calls once the session dispatches the user message to the agent.
// Flips the receipt's reaction emoji from ⏳ to 🔄 and writes
// the "🔄 ⏳ N · HH:MM:SS" header so the user sees the session
// is alive.
//
// Falls back to a no-op when the receipt has already moved on
// (e.g. a /kill arrived before dispatch) so callers don't have
// to coordinate locking.
func (a *Adapter) MarkExecuting(ctx context.Context, userMsgID string) error {
	a.mu.RLock()
	receipt, ok := a.receiptsByUserMsgID[userMsgID]
	a.mu.RUnlock()
	if !ok || receipt == nil {
		return nil
	}
	return receipt.SetExecuting(ctx)
}

// --- v1.1 receipt lifecycle API (channel.Channel contract) ---
//
// CreateReceipt / UpdateReceipt / DisposeReceipt are the v1.1
// interface methods that the Gateway uses to drive the receipt
// FSM. They mirror the existing SendUserMessage / MarkExecuting /
// SetCompleted paths but expose the cross-channel channel.Receipt
// opaque type and channel.ReceiptState enum.
//
// Commit 1 of the v1.1 refactor (see docs/feat/F-26-gateway-hub.md
// §6). Existing SendUserMessage / MarkExecuting continue to work
// for the legacy fallback path in cmd/nightme/run.go until commit
// 3 migrates it.

// CreateReceipt creates a new Feishu receipt for an incoming user
// message and returns the opaque channel.Receipt handle. The
// initial state is Pending (⏳). blocks is the structured user
// turn; the receipt body is rendered via BuildForwardedTextFromBlocks
// so attachment paths are visible to the user.
//
// The receipt's reply card is posted via Feishu's ReplyMessage
// API (anchored to userMsgID) so the chat surface shows the
// native "Reply to <user>: <preview>" header above the body —
// the pairing cue users rely on. Subsequent agent events edit
// that single card in place via UpdateMessage; the user always
// sees exactly one receipt reply per user message (F-25 v1.1).
//
// On error the caller (Gateway) skips receipt bookkeeping and
// falls back to a plain Send.
func (a *Adapter) CreateReceipt(ctx context.Context, chatID, userMsgID string, blocks []agent.ContentBlock) (channel.Receipt, error) {
	if a == nil {
		return nil, errors.New("feishu: nil adapter")
	}
	text := BuildForwardedTextFromBlocks(blocks)
	receipt, err := a.SendUserMessage(ctx, chatID, userMsgID, text)
	if err != nil {
		return nil, err
	}
	return channel.Receipt(receipt), nil
}

// UpdateReceipt transitions the receipt to the given state. The
// receipt must be a *MessageReceipt (the only concrete type this
// adapter produces). Idempotent for the same state.
func (a *Adapter) UpdateReceipt(ctx context.Context, receipt channel.Receipt, state channel.ReceiptState) error {
	if a == nil {
		return errors.New("feishu: nil adapter")
	}
	if receipt == nil {
		return errors.New("feishu: nil receipt")
	}
	r, ok := receipt.(*MessageReceipt)
	if !ok {
		return fmt.Errorf("feishu: receipt is not *MessageReceipt: %T", receipt)
	}
	return r.applyState(ctx, state)
}

// DisposeReceipt cleans up the receipt (deletes the receipt
// message + reaction). Idempotent.
func (a *Adapter) DisposeReceipt(ctx context.Context, receipt channel.Receipt) error {
	if a == nil {
		return errors.New("feishu: nil adapter")
	}
	if receipt == nil {
		return errors.New("feishu: nil receipt")
	}
	r, ok := receipt.(*MessageReceipt)
	if !ok {
		return fmt.Errorf("feishu: receipt is not *MessageReceipt: %T", receipt)
	}
	return r.dispose(ctx)
}

// receiptFor returns the receipt for the given userMsgID. Returns
// nil if no receipt has been created yet for that user message
// (the caller is expected to create one via the cold-start path
// in Send when ReplyTo is set but no receipt exists yet).
//
// Receipts are keyed by userMsgID (not chatID) so multiple
// receipts can coexist per chat — the gateway's InputBuffer may
// flush N user messages as one agent turn, and each user message
// gets its own ReplyCard anchored to its own userMsgID via the
// ReplyMessage API.
func (a *Adapter) receiptFor(userMsgID string) *MessageReceipt {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.receiptsByUserMsgID[userMsgID]
}

// Send implements the v0.3.1 channel.Channel interface. Each
// OutboundMessage is dispatched based on (1) whether it's anchored
// to a user message via ReplyTo and (2) its Kind.
//
// Anchoring semantics:
//
//	msg.ReplyTo == ""  → no anchor; the message goes out as a
//	                     plain text (or card / reaction) without
//	                     any receipt or rolling log. Use for
//	                     genuinely unsolicited output (startup
//	                     notices, /run previews, etc.).
//
//	msg.ReplyTo != ""  → anchor to that user message via
//	                     Feishu's ReplyMessage API. If a
//	                     receipt already exists for that
//	                     userMsgID, append this event to its
//	                     rolling log (UpdateMessage in place).
//	                     Otherwise cold-start a new receipt
//	                     whose reply card is anchored to the
//	                     same user message.
//
// All agent-event-bearing kinds (OutText / OutThinking /
// OutToolStart / OutToolEnd / OutResult / OutUsage / OutInit /
// OutCompaction) route through the receipt. Reaction / Card /
// Typing kinds are channel-global (not anchored to a single
// user message) so they ignore ReplyTo.
func (a *Adapter) Send(ctx context.Context, msg gateway.OutboundMessage) error {
	switch msg.Kind {
	case gateway.OutReaction, gateway.OutReactionRemoved, gateway.OutCard, gateway.OutTyping:
		// Channel-global kinds — ReplyTo is not meaningful.
		return a.sendGlobal(ctx, msg)

	default:
		// Agent-event-bearing kinds — must anchor to ReplyTo.
		return a.sendAnchored(ctx, msg)
	}
}

// sendGlobal handles kinds that don't carry a user-message
// anchor: reactions, cards, typing. ReplyTo is ignored.
func (a *Adapter) sendGlobal(ctx context.Context, msg gateway.OutboundMessage) error {
	switch msg.Kind {
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
		if msg.Card.RequestID == "" {
			msg.Card.RequestID = fmt.Sprintf("%s:%d", msg.ChatID, time.Now().UnixNano())
		}
		content, err := buildInteractiveCard(msg.Card)
		if err != nil {
			return err
		}
		_, err = a.sendContent(ctx, msg.ChatID, interactiveMessageType, content)
		return err

	case gateway.OutTyping:
		// No native Feishu equivalent. Silently drop.
		return nil
	}
	return fmt.Errorf("feishu: unsupported global kind %v", msg.Kind)
}

// sendAnchored handles kinds that carry agent events to be
// rendered on a receipt reply card. Anchors to msg.ReplyTo via
// the receipt index; cold-starts a new receipt (via ReplyMessage)
// when none exists yet for that userMsgID.
//
// Drops to a plain text message when ReplyTo is empty AND the
// caller wants the event surfaced to the user anyway (e.g.
// OutText with no anchor). Receipt-only kinds (OutInit, OutUsage,
// OutCompaction) silently drop when ReplyTo is empty — they're
// meaningless without a card to render on.
func (a *Adapter) sendAnchored(ctx context.Context, msg gateway.OutboundMessage) error {
	if msg.ReplyTo == "" {
		return a.sendUnanchored(ctx, msg)
	}
	receipt := a.receiptFor(msg.ReplyTo)
	if receipt == nil {
		newReceipt, err := a.coldStartReceipt(ctx, msg)
		if err != nil {
			return err
		}
		if newReceipt == nil {
			return nil
		}
		receipt = newReceipt
	}
	return a.appendToReceipt(ctx, receipt, msg)
}

// sendUnanchored handles the rare case where an agent-event-
// bearing kind arrives without a ReplyTo anchor. Receipt-only
// kinds (OutInit / OutUsage / OutCompaction) silently drop; user-
// facing kinds (OutText / OutThinking / OutToolStart / OutToolEnd
// / OutResult) degrade to a plain text message so the user still
// sees the event.
func (a *Adapter) sendUnanchored(ctx context.Context, msg gateway.OutboundMessage) error {
	switch msg.Kind {
	case gateway.OutInit, gateway.OutUsage, gateway.OutCompaction:
		// Card-only metadata — meaningless without a receipt.
		return nil
	}
	if msg.Text != "" {
		return a.sendPlainText(ctx, msg.ChatID, msg.Text)
	}
	return nil
}

// sendPlainText is the no-anchor fallback: post msg.Text as a
// fresh standalone message via SendMessageText. Used by the
// fallback handler ("no workspace set", etc.) when ReplyTo is
// empty or the receipt path is unavailable.
func (a *Adapter) sendPlainText(ctx context.Context, chatID, text string) error {
	_, err := a.SendMessageText(ctx, chatID, text)
	return err
}

// coldStartReceipt creates a new receipt for msg.ReplyTo (no
// prior receipt exists). Posts the initial receipt reply via
// the Feishu ReplyMessage API so the card is anchored to the
// same user message. Returns nil receipt + nil error when the
// kind has no useful initial body (OutInit / OutUsage /
// OutCompaction) so the caller doesn't try to render an empty
// card.
func (a *Adapter) coldStartReceipt(ctx context.Context, msg gateway.OutboundMessage) (*MessageReceipt, error) {
	body, ok := initialReceiptBody(msg)
	if !ok {
		return nil, nil
	}
	replyMsgID, err := a.ReplyMessage(ctx, msg.ChatID, msg.ReplyTo, body)
	if err != nil {
		a.logger.Warn("feishu: cold-start receipt reply failed",
			"err", err, "user_msg_id", msg.ReplyTo, "kind", msg.Kind)
		return nil, err
	}
	receipt := NewMessageReceiptForReply(msg.ChatID, msg.ReplyTo, replyMsgID, a)
	a.mu.Lock()
	a.receiptsByUserMsgID[msg.ReplyTo] = receipt
	a.mu.Unlock()
	return receipt, nil
}

// initialReceiptBody returns the text to post as the initial
// receipt reply card on cold start. Returns (_, false) for
// kinds whose initial body would be empty (OutInit, OutUsage,
// OutCompaction) — those events are only meaningful once the
// receipt is already established by a prior text-bearing event.
//
// The returned body is the same string that would land in the
// rolling log via Append, so cold start + subsequent Appends
// render identically to warm-start Appends.
func initialReceiptBody(msg gateway.OutboundMessage) (string, bool) {
	switch msg.Kind {
	case gateway.OutText, gateway.OutThinking, gateway.OutResult:
		if msg.Text == "" {
			return "", false
		}
		return msg.Text, true
	case gateway.OutToolStart, gateway.OutToolEnd:
		if msg.Text == "" {
			return "", false
		}
		return msg.Text, true
	default:
		return "", false
	}
}

// appendToReceipt converts the OutboundMessage into an AgentEvent
// and routes it to the receipt's rolling log. OutInit / OutUsage
// also stamp footer metadata before the Append so the receipt
// re-renders with the latest session context + token counts.
func (a *Adapter) appendToReceipt(ctx context.Context, receipt *MessageReceipt, msg gateway.OutboundMessage) error {
	switch msg.Kind {
	case gateway.OutText:
		return receipt.Append(ctx, agent.AgentEvent{Kind: agent.EventText, Text: msg.Text})

	case gateway.OutThinking:
		return receipt.Append(ctx, agent.AgentEvent{Kind: agent.EventText, Text: msg.Text})

	case gateway.OutToolStart:
		return receipt.Append(ctx, agent.AgentEvent{
			Kind:      agent.EventToolStart,
			ToolStart: &agent.ToolStartEvent{Name: toolName(msg), Args: toolArgs(msg)},
		})

	case gateway.OutToolEnd:
		return receipt.Append(ctx, agent.AgentEvent{
			Kind:    agent.EventToolEnd,
			ToolEnd: &agent.ToolEndEvent{Name: toolName(msg), Output: toolOutput(msg)},
		})

	case gateway.OutResult:
		return receipt.Append(ctx, agent.AgentEvent{
			Kind: agent.EventResult,
			Result: &agent.ResultEvent{
				Text:       msg.Text,
				DurationMs: durationMs(msg),
				IsError:    isErrorOut(msg),
				Subtype:    subtypeOut(msg),
			},
		})

	case gateway.OutUsage:
		// Re-compose the footer with the latest token counts
		// from the agent's EventUsage.Meta. nightme does not
		// track tokens itself; the values are relayed
		// straight from the gateway's translateAndSend. The
		// static session-attribution prefix is cached on the
		// adapter (set on OutInit) — we only refresh the
		// dynamic tokens segment here. Labels are dropped
		// from the rendered footer (the user reads the
		// "<in> / <out>" pair as in/out tokens from
		// position).
		if prefix := a.cachedFooterPrefix(msg.ChatID); prefix != "" {
			if in, out := metaInt(msg, "input_tokens"), metaInt(msg, "output_tokens"); in > 0 || out > 0 {
				receipt.SetFooter(prefix + " | " + humanTokens(in) + " / " + humanTokens(out))
			}
		}
		return receipt.Append(ctx, agent.AgentEvent{
			Kind:  agent.EventUsage,
			Usage: usageFromMeta(msg),
		})

	case gateway.OutInit:
		// Compose the static session-attribution prefix
		// ("Agent: <name> | cwd: <path> | Provider: <p>") and
		// cache it on the adapter so OutUsage can refresh the
		// dynamic tokens segment without re-reading session
		// metadata. Empty segments drop themselves so a
		// partial set still renders cleanly. Tokens are NOT
		// included here — they're agent-context, owned by the
		// agent's own session, and only surface when an
		// OutUsage arrives.
		prefix := composeFooterPrefix(
			metaString(msg, "agent_name"),
			shortenWorkspace(metaString(msg, "workspace")),
			metaString(msg, "provider"),
		)
		a.cacheFooterPrefix(msg.ChatID, prefix)
		receipt.SetFooter(prefix)
		return receipt.Append(ctx, agent.AgentEvent{
			Kind: agent.EventInit,
			Init: &agent.InitEvent{
				SessionID: metaString(msg, "session_id"),
				Model:     metaString(msg, "model"),
			},
		})

	case gateway.OutCompaction:
		return receipt.Append(ctx, agent.AgentEvent{
			Kind:       agent.EventCompaction,
			Compaction: &agent.CompactionEvent{Subtype: metaString(msg, "subtype")},
		})
	}
	return fmt.Errorf("feishu: unsupported anchored kind %v", msg.Kind)
}

// toolName / toolArgs / toolOutput pull the well-known fields from
// OutboundMessage.Meta. The translator fills these so the receiver
// doesn't have to parse the formatted text.
func toolName(m gateway.OutboundMessage) string {
	if n, _ := m.Meta["tool_name"].(string); n != "" {
		return n
	}
	return "tool"
}

func toolArgs(m gateway.OutboundMessage) string {
	if a, _ := m.Meta["args"].(string); a != "" {
		return a
	}
	return ""
}

func toolOutput(m gateway.OutboundMessage) string {
	if o, _ := m.Meta["output"].(string); o != "" {
		return o
	}
	return ""
}

// metaString / metaInt / metaFloat / metaBool pull well-known typed
// fields from OutboundMessage.Meta. The translator (gateway/translate.go)
// is the single producer of these keys; receivers tolerate any key
// being absent / wrong-typed by returning the zero value rather than
// erroring. Mirrors the existing toolName / toolArgs / toolOutput
// helpers.
func metaString(m gateway.OutboundMessage, key string) string {
	if v, ok := m.Meta[key].(string); ok {
		return v
	}
	return ""
}

func metaInt(m gateway.OutboundMessage, key string) int {
	switch v := m.Meta[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		// JSON numbers decode to float64 by default. Coerce when
		// the value is integral so we don't silently truncate
		// large token counts.
		return int(v)
	}
	return 0
}

func metaFloat(m gateway.OutboundMessage, key string) float64 {
	if v, ok := m.Meta[key].(float64); ok {
		return v
	}
	return 0
}

func metaBool(m gateway.OutboundMessage, key string) bool {
	if v, ok := m.Meta[key].(bool); ok {
		return v
	}
	return false
}

// durationMs / isErrorOut / subtypeOut / usageFromMeta are the
// OutResult / OutUsage payload reconstruction helpers. They read the
// same keys the translator writes (see gateway/translate.go).
func durationMs(m gateway.OutboundMessage) int64 {
	return int64(metaInt(m, "duration_ms"))
}

func isErrorOut(m gateway.OutboundMessage) bool {
	return metaBool(m, "is_error")
}

func subtypeOut(m gateway.OutboundMessage) string {
	return metaString(m, "subtype")
}

func usageFromMeta(m gateway.OutboundMessage) *agent.UsageEvent {
	return &agent.UsageEvent{
		InputTokens:              metaInt(m, "input_tokens"),
		OutputTokens:             metaInt(m, "output_tokens"),
		CacheCreationInputTokens: metaInt(m, "cache_creation_input_tokens"),
		CacheReadInputTokens:     metaInt(m, "cache_read_input_tokens"),
		CostUSD:                  metaFloat(m, "cost_usd"),
	}
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
		"config": map[string]any{"wide_screen_mode": true},
		"header": json.RawMessage(headerJSON),
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
// inbound "received: …" line is emitted by handleMessage via
// logInbound; without this trace the user sees their own inputs
// but not what the bot sent back.
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

// ReplyMessage creates a new Feishu reply anchored to userMsgID and
// returns the created message ID. Feishu renders a "Reply to
// <user>: <preview>" header above the body — the native pairing
// cue users rely on to associate a bot reply with the triggering
// user message.
//
// Used by SendUserMessage (the CreateReceipt path) so every
// receipt's reply card is visually paired with its user message.
// Falls back to SendMessageText when userMsgID is empty (defensive;
// the cold-start path in receiptFor uses a synthetic userMsgID and
// shouldn't reach here in practice).
//
// Body is wrapped as `post` type with content_v2 + `md` tag so
// Feishu renders the markdown natively. See wrapAsPostContent.
//
// Returns (messageID, error). On error, messageID is "".
func (a *Adapter) ReplyMessage(ctx context.Context, chatID, userMsgID, text string) (string, error) {
	if strings.TrimSpace(chatID) == "" {
		return "", errors.New("feishu: chat_id is required")
	}
	if strings.TrimSpace(userMsgID) == "" {
		return "", errors.New("feishu: user_msg_id is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	content := wrapAsPostContent(text)
	msgID, err := a.replyContent(ctx, userMsgID, receiptMessageType, content)
	a.logOutgoing("reply_message", userMsgID, msgID, err)
	return msgID, err
}

// replyContent dispatches to a.replyFunc when set (test mock) or
// a.replyViaLark (production). Mirrors sendContent's contract for
// the ReplyMessage API.
func (a *Adapter) replyContent(ctx context.Context, userMsgID, msgType, content string) (string, error) {
	a.mu.RLock()
	reply := a.replyFunc
	if reply == nil && a.larkClient != nil {
		reply = a.replyViaLark
	}
	a.mu.RUnlock()
	if reply == nil {
		return "", errors.New("feishu: REST client is nil")
	}
	return reply(ctx, userMsgID, msgType, content)
}

func (a *Adapter) replyViaLark(ctx context.Context, userMsgID, msgType, content string) (string, error) {
	if a.larkClient == nil || a.larkClient.Im == nil || a.larkClient.Im.V1 == nil || a.larkClient.Im.V1.Message == nil {
		return "", errors.New("feishu: REST client is nil")
	}
	body := larkim.NewReplyMessageReqBodyBuilder().
		MsgType(msgType).
		Content(content).
		Build()
	req := larkim.NewReplyMessageReqBuilder().
		MessageId(userMsgID).
		Body(body).
		Build()
	resp, err := a.larkClient.Im.V1.Message.Reply(ctx, req)
	if err != nil {
		return "", fmt.Errorf("feishu: reply message: %w", err)
	}
	if resp == nil {
		return "", errors.New("feishu: reply message returned nil response")
	}
	if !resp.Success() {
		return "", fmt.Errorf("feishu: reply message failed with code %d", resp.Code)
	}
	var msgID string
	if resp.Data != nil && resp.Data.MessageId != nil {
		msgID = *resp.Data.MessageId
	}
	return msgID, nil
}

// SendMessageText is the message-ID-returning variant of SendMessage.
// Used by MessageReceipt (F-25) so the reply text line can be edited
// in place via UpdateMessage on subsequent state transitions.
//
// The body is wrapped as a `post` type message with content_v2 +
// `md` tag so Feishu renders the markdown natively (collapsible
// code fences for thinking, headers / lists / etc.). See
// wrapAsPostContent for the schema details.
//
// Returns (messageID, error). On error, messageID is "".
func (a *Adapter) SendMessageText(ctx context.Context, chatID, text string) (string, error) {
	if strings.TrimSpace(chatID) == "" {
		return "", errors.New("feishu: chat_id is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	content := wrapAsPostContent(text)
	msgID, err := a.sendContent(ctx, chatID, receiptMessageType, content)
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

// DeleteReaction removes a reaction by its ID. Used by the adapter's
// OutReactionRemoved send path (Meta["reaction_id"]). Receipts no
// longer delete reactions — they append lifecycle emojis instead —
// but the public method stays for other adapter consumers.
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

// UpdateMessage edits an existing post-type message's content
// in-place. Used by MessageReceipt to keep the reply line as a
// single message across heartbeat ticks (per F-25 spec: "永远只有
// 一行"). The receipt is created as a `post` type via ReplyMessage
// (see wrapAsPostContent); updates must match.
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
	content := wrapAsPostContent(text)
	body := larkim.NewUpdateMessageReqBodyBuilder().
		MsgType(receiptMessageType).
		Content(content).
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
		UserID:      senderID(event),
		Time:        messageTime(message.CreateTime),
		ChatType:    gateway.ChatType(chatType),
		MessageID:   stringValue(message.MessageId),
		Attachments: attachments,
	}
	// Trace every inbound message before the publish lock so the
	// CLI surface shows the user message that triggered the
	// handler even if the channel send is cancelled or timed out.
	// Companion to logOutgoing (the bot side of the conversation).
	a.logInbound(msg)
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

// shortenWorkspace renders a workspace path in the form users see
// in their shell prompt — leading $HOME is replaced with "~". The
// gateway forwards the session's Workspace verbatim (the
// canonical absolute path stored on Session.Workspace), so this
// is purely a display affordance for the receipt footer.
//
// Defensive: returns path unchanged when $HOME is unset or path
// doesn't start with it. Doesn't try to handle non-canonical
// representations (symlinks, "./" prefixes, etc.) — the receipt
// shows the path the session was actually given, just in shorter
// form.
func shortenWorkspace(path string) string {
	if path == "" {
		return ""
	}
	home := os.Getenv("HOME")
	if home == "" {
		return path
	}
	if path == home {
		return "~"
	}
	if strings.HasPrefix(path, home+"/") {
		return "~" + path[len(home):]
	}
	return path
}

// composeFooterPrefix builds the static session-attribution
// portion of the receipt footer. Empty segments drop themselves
// so a partial set ("claude" arrived but cwd not yet) still
// renders cleanly. Tokens are NOT included — they're
// agent-context and only surface when an OutUsage arrives.
//
// Shape (labels omitted — users recognize the segments from
// position: agent name first, cwd second):
//
//	<name> | <cwd>
//
// Provider was dropped from the footer; the agent name already
// disambiguates the session context, and the provider-level
// routing is internal to the agent bridge.
//
// Returns "" when both segments are empty (no footer at all),
// so the caller can skip the SetFooter call entirely.
func composeFooterPrefix(agent, cwd, provider string) string {
	var parts []string
	if agent != "" {
		parts = append(parts, agent)
	}
	if cwd != "" {
		parts = append(parts, cwd)
	}
	_ = provider // deprecated — see doc comment above
	return strings.Join(parts, " | ")
}

// cachedFooterPrefix returns the per-chat static footer prefix
// (set on the most recent OutInit). Empty when none has arrived
// yet for this chat. Thread-safe.
func (a *Adapter) cachedFooterPrefix(chatID string) string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.receiptFooters[chatID]
}

// cacheFooterPrefix stores the per-chat static footer prefix.
// Called from OutInit; later OutUsage events read it back via
// cachedFooterPrefix to rebuild the full footer with fresh
// token counts. Thread-safe.
func (a *Adapter) cacheFooterPrefix(chatID, prefix string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if prefix == "" {
		delete(a.receiptFooters, chatID)
		return
	}
	a.receiptFooters[chatID] = prefix
}

// humanTokens renders a token count with k-suffix rounding
// ("12.3K", "1K"). Mirrors the convention used in Claude Code's
// own CLI output so the footer line matches what users see in
// the terminal. Values < 1000 are returned verbatim so small
// counts stay precise.
//
// Local to the adapter because the receipt package is purely a
// renderer; the formatting decision (k-suffix thresholds) is
// the adapter's choice.
func humanTokens(n int) string {
	switch {
	case n < 1000:
		return strconv.Itoa(n)
	case n < 10000:
		return fmt.Sprintf("%.1fK", float64(n)/1000)
	default:
		return strconv.Itoa(n/1000) + "K"
	}
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
