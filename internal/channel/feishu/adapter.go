// Package feishu implements the Feishu channel adapter.
package feishu

import (
	"bytes"
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

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
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

// sendMessageFunc is kept behind the adapter so unit tests can exercise the
// channel without making an HTTP request to Feishu.
//
// rootID is the Feishu root_id parameter (reply-in-thread). When non-empty,
// the sent message is rendered as a reply to that user message; when empty,
// the message is a fresh top-level message in the chat. v1.3.x (§13.10)
// forwards msg.ReplyTo here so all bot messages that have a user-side anchor
// thread visually to the user's original message.
type sendMessageFunc func(ctx context.Context, chatID, msgType, content, rootID string) (string, error)

// updateMessageFunc is the in-place update boundary (Feishu PATCH
// /im/v1/messages/{id}). Kept behind a function field so unit tests
// can replace the network call with a small in-memory stub. PATCH
// is the SDK's *Patch* method (NOT *Update* — Update only supports
// text/rich-text messages; see receipt.go and docs/channel/feishu.md
// §3.4 for the rationale).
type updateMessageFunc func(ctx context.Context, messageID, content string) error

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

	// Stage 3: rolling-log receipt state lives on the adapter
	// (Channel.Send is the display strategy). The map is keyed by
	// chatID; one receipt per chat at a time. When the gateway's
	// per-session pump emits the first OutText for a chat that
	// doesn't have an active receipt, we lazily create one (this
	// covers the case where the user message is forwarded via
	// the gateway's messageDispatcher but the renderer path is gone).
	receipts map[string]*MessageReceipt

	// receiptsByUserMsgID is the secondary index that lets
	// MarkExecuting and incoming card-action callbacks find the
	// receipt from the Feishu user message id. Same lifecycle
	// rules as receipts (delete together when SetCompleted).
	receiptsByUserMsgID map[string]*MessageReceipt

	// messageStates tracks the last successfully-rendered
	// MessageState per user message id, so same-state emits
	// (heartbeats, retries) skip a duplicate AddReaction. v1.3
	// (F-31) replaces the per-receipt currentReaction field which
	// was removed when MessageReceipt stopped owning reactions.
	//
	// Concurrency: reads/writes go through a.mu (same lock as
	// receipts). Same lifecycle — entries persist for the
	// ChatSession lifetime and are evicted only on adapter stop.
	messageStates map[string]agent.MessageState

	// These hooks have production defaults and are intentionally kept as
	// fields so tests can replace the network boundary with a small function.
	wsStart    func(context.Context) error
	wsClose    func()
	sendFunc   sendMessageFunc
	updateFunc updateMessageFunc
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
		messageStates:       make(map[string]agent.MessageState),
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
	a.updateFunc = a.updateViaLark
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
//	                       existing UpdateMessage flow (card PATCH);
//	                       this path is taken by the Feishu channel's
//	                       display strategy (Stage 3 migrates the
//	                       receipt-rendering logic here). Stage 1's
//	                       Send handles OutText directly so the
//	                       existing /help / /use paths keep
//	                       working.
//	OutMessageState      → AddReaction on Meta["message_id"] with
//	                       state-specific emoji_type (F-31 §8.3).
//	                       Idempotency via a.messageStates map.
//	OutMessageStateRemoved → DeleteReaction on Meta["reaction_id"]
//	                       (reserved; v1.3 uses append-only)
//	OutCard              → send interactive card via sendContent
//	OutThinking          → dropped (no native Feishu equivalent;
//	                       future: a sub-message indicator)
//	OutTyping            → dropped (no native Feishu equivalent)
//
// Errors from the underlying API are logged and returned; the
// Gateway treats Send as fire-and-ack (no retry).


// --- v1.1 receipt lifecycle API (channel.Channel contract) ---
//
// receiptFor returns the receipt for (chatID, userMsgID), lazily
// cold-creating one if the gateway stamped an OutboundMessage with
// a ReplyTo that doesn't yet have a registered receipt.
//
// v1.3 (SPEC §2.2): each OutboundMessage{ReplyTo: userMsgID} maps
// to exactly one receipt per userMsgID. The cold-start posts a
// minimal ⏳ card via SendCard and registers the receipt keyed by
// the actual userMsgID (NOT a synthetic chatID + timestamp — that
// was the pre-v1.3 design and caused turn 2 events to silently
// drop on the per-chat active receipt).
//
// userMsgID == "" means orphan event (startup EventInit, internal
// log). Return nil so the caller falls back to sendRawOutText
// (the card surface is meaningless for orphan events).
//
// Locking: look-up / register under a.mu. The actual cold-start
// SendCard runs without the lock (network call; must not block
// other adapter operations).
func (a *Adapter) receiptFor(ctx context.Context, chatID, userMsgID string) *MessageReceipt {
	if userMsgID == "" {
		return nil
	}
	a.mu.Lock()
	if r, ok := a.receiptsByUserMsgID[userMsgID]; ok && r != nil {
		a.mu.Unlock()
		return r
	}
	a.mu.Unlock()

	// Cold start: post a minimal ⏳ card. buildColdStartCard builds
	// the Card 2.0 envelope; SendCard routes through sendFunc so the
	// resulting cardMsgID is set on the FIRST render — no
	// "text then card" transition. See docs/channel/feishu.md §5.2.
	//
	// v1.3.x (§13.10): pass userMsgID as rootID so the card is
	// rendered as a reply to the user's message in the main chat.
	// Subsequent PatchMessage calls inherit the thread automatically.
	cardBody, err := buildColdStartCard()
	if err != nil {
		a.logger.Warn("feishu: cold-start card build failed",
			"err", err, "chat_id", chatID, "user_msg_id", userMsgID)
		return nil
	}
	msgID, err := a.SendCard(ctx, chatID, cardBody, userMsgID)
	if err != nil {
		a.logger.Warn("feishu: cold-start receipt card failed",
			"err", err, "chat_id", chatID, "user_msg_id", userMsgID)
		return nil
	}
	// Use NewMessageReceiptForCard because the cold-start card
	// is already posted; set cardMsgID so the FIRST Append goes
	// straight to PATCH (no "text then card" transition and no
	// duplicate SendCard call inside the receipt's renderLocked).
	receipt := NewMessageReceiptForCard(chatID, userMsgID, msgID, a)
	a.mu.Lock()
	// Re-check under the lock in case a concurrent send cold-created
	// the same receipt between our look-up and register.
	if existing, ok := a.receiptsByUserMsgID[userMsgID]; ok && existing != nil {
		a.mu.Unlock()
		return existing
	}
	a.receiptsByUserMsgID[userMsgID] = receipt
	a.mu.Unlock()
	return receipt
}

func (a *Adapter) Send(ctx context.Context, msg gateway.OutboundMessage) error {
	switch msg.Kind {
	case gateway.OutText:
		// Folded into the active receipt's rolling log. The
		// Feishu reply is the single message the user sees; we
		// edit it in place via UpdateMessage on each event.
		receipt := a.receiptFor(ctx, msg.ChatID, msg.ReplyTo)
		if receipt == nil {
			// receiptFor's SendMessageText failed. Try a direct
			// send as a last resort so the user sees the text.
			return a.sendRawOutText(ctx, msg.ChatID, msg.Text)
		}
		return receipt.Append(ctx, agent.AgentEvent{
			Kind: agent.EventText,
			Text: msg.Text,
		})

	case gateway.OutThinking:
		receipt := a.receiptFor(ctx, msg.ChatID, msg.ReplyTo)
		if receipt == nil {
			return nil
		}
		// v1.3.x (§13.1 bug fix): the Gateway's translate.go
		// strips the [思考] prefix before emitting OutThinking,
		// but the receipt's eventToEntry detects thinking entries
		// by checking HasPrefix(text, thinkingPrefix). Without the
		// prefix the Kind="thinking" branch never fires and the
		// collapsible_panel rendering for thinking is dead code.
		// Prepending the prefix here restores the detection.
		return receipt.Append(ctx, agent.AgentEvent{
			Kind: agent.EventText,
			Text: thinkingPrefix + msg.Text,
		})

	case gateway.OutMessageState:
		// F-31: read abstract state from Meta, map to feishu
		// emoji_type internally. Channel decides how to render.
		messageID, _ := msg.Meta["message_id"].(string)
		if messageID == "" {
			return errors.New("feishu: OutMessageState missing message_id in Meta")
		}
		stateRaw, ok := msg.Meta["state"]
		if !ok {
			return errors.New("feishu: OutMessageState missing state in Meta")
		}
		state, ok := stateRaw.(agent.MessageState)
		if !ok {
			return fmt.Errorf("feishu: OutMessageState state has unexpected type %T", stateRaw)
		}
		emoji := mapStateToFeishuEmoji(state)
		if emoji == "" {
			// Unknown state: silent drop (forward-compatible).
			return nil
		}
		// Idempotency: skip if we already rendered this state for
		// this userMsgID. Tracks last-rendered state to avoid
		// duplicate AddReaction calls on retries / heartbeats.
		//
		// v1.3.1 fix: use the comma-ok form to distinguish "no
		// entry yet" (first emit) from "previous state is
		// StateReceived" (which is the zero value of MessageState
		// and was incorrectly treated as "already rendered",
		// silently dropping every first StateReceived emit).
		a.mu.Lock()
		prev, hasPrev := a.messageStates[messageID]
		skip := hasPrev && prev == state
		if !skip {
			a.messageStates[messageID] = state
		}
		a.mu.Unlock()
		if skip {
			return nil
		}
		if _, err := a.AddReaction(ctx, messageID, emoji); err != nil {
			a.mu.Lock()
			// Revert on failure so a later retry can re-add.
			if a.messageStates[messageID] == state {
				delete(a.messageStates, messageID)
			}
			a.mu.Unlock()
			a.logger.Warn("feishu: OutMessageState add reaction failed",
				"err", err, "emoji", emoji, "user_msg_id", messageID)
			return err
		}
		return nil

	case gateway.OutMessageStateRemoved:
		// v1.3: not used (append-only reactions). Reserved for
		// future when channels need mutable state markers.
		messageID, _ := msg.Meta["message_id"].(string)
		if messageID == "" {
			return errors.New("feishu: OutMessageStateRemoved missing message_id in Meta")
		}
		reactionID, _ := msg.Meta["reaction_id"].(string)
		if reactionID == "" {
			return errors.New("feishu: OutMessageStateRemoved missing reaction_id in Meta")
		}
		return a.DeleteReaction(ctx, messageID, reactionID)

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
		// v1.3.x (§13.10): thread permission card to the user's message
		// so the user sees a visual "reply" line connecting the
		// permission request to the task that triggered it.
		_, err = a.sendContent(ctx, msg.ChatID, interactiveMessageType, content, msg.ReplyTo)
		return err

	case gateway.OutToolStart:
		receipt := a.receiptFor(ctx, msg.ChatID, msg.ReplyTo)
		if receipt == nil {
			return nil
		}
		return receipt.Append(ctx, agent.AgentEvent{
			Kind:      agent.EventToolStart,
			ToolStart: &agent.ToolStartEvent{Name: toolName(msg), Args: toolArgs(msg)},
		})

	case gateway.OutToolEnd:
		receipt := a.receiptFor(ctx, msg.ChatID, msg.ReplyTo)
		if receipt == nil {
			return nil
		}
		return receipt.Append(ctx, agent.AgentEvent{
			Kind:    agent.EventToolEnd,
			ToolEnd: &agent.ToolEndEvent{Name: toolName(msg), Output: toolOutput(msg)},
		})

	case gateway.OutTyping:
		// No native Feishu equivalent (typing indicators come from
		// the OpenAPI, not the bot's message API). Silently drop.
		return nil

	case gateway.OutResult:
		receipt := a.receiptFor(ctx, msg.ChatID, msg.ReplyTo)
		if receipt == nil {
			// receipt creation failed — degrade to a standalone
			// message so the user still sees the final reply
			// instead of a silent drop.
			if msg.Text != "" {
				return a.sendRawOutText(ctx, msg.ChatID, msg.Text)
			}
			return nil
		}
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
		receipt := a.receiptFor(ctx, msg.ChatID, msg.ReplyTo)
		if receipt == nil {
			return nil
		}
		return receipt.Append(ctx, agent.AgentEvent{
			Kind:  agent.EventUsage,
			Usage: usageFromMeta(msg),
		})

	case gateway.OutCompaction:
		receipt := a.receiptFor(ctx, msg.ChatID, msg.ReplyTo)
		if receipt == nil {
			return nil
		}
		return receipt.Append(ctx, agent.AgentEvent{
			Kind:       agent.EventCompaction,
			Compaction: &agent.CompactionEvent{Subtype: metaString(msg, "subtype")},
		})

	case gateway.OutInit:
		receipt := a.receiptFor(ctx, msg.ChatID, msg.ReplyTo)
		if receipt == nil {
			return nil
		}
		// Forward agent identity + workspace so the receipt
		// card's foot note can render
		// "Agent | cwd | tokens" (see docs/channel/feishu.md
		// §9.3 + the footLine composer in receipt.go).
		return receipt.Append(ctx, agent.AgentEvent{
			Kind: agent.EventInit,
			Init: &agent.InitEvent{
				SessionID: metaString(msg, "session_id"),
				Model:     metaString(msg, "model"),
				AgentName: metaString(msg, "agent_name"),
				Workspace: metaString(msg, "workspace"),
				Branch:    metaString(msg, "branch"),
			},
		})

	case gateway.OutCommandReply:
		// Slash command response (or runtime error reply). Plain
		// text, no receipt, no in-place update — the user sees a
		// standalone text bubble. The Feishu SendMessageText path
		// uses msg_type: "text" so the message renders as a normal
		// chat bubble, not an interactive card.
		//
		// v1.3.x (§13.10): thread the reply to the user's
		// /command message via msg.ReplyTo as Feishu root_id, so
		// the user sees a visual "reply" line connecting the bot
		// reply to their command. msg.ReplyTo is empty for orphan
		// replies (e.g. gateway startup logs); in that case the
		// message stays top-level.
		if msg.Text == "" {
			return errors.New("feishu: OutCommandReply missing text")
		}
		_, err := a.SendMessageText(ctx, msg.ChatID, msg.Text, msg.ReplyTo)
		return err
	}
	return fmt.Errorf("feishu: unsupported outbound kind %v", msg.Kind)
}

// sendRawOutText is the degraded send path used when a receipt
// can't be created (e.g. the channel.post text API failed). Sends
// the text as a new standalone message so the user still sees
// something.
func (a *Adapter) sendRawOutText(ctx context.Context, chatID, text string) error {
	// Empty rootID: this degraded path is hit when receipt cold-start
	// failed and there's no userMsgID to thread to. The fallback
	// message remains a top-level text bubble (legacy behavior).
	_, err := a.SendMessageText(ctx, chatID, text, "")
	return err
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

// buildInteractiveCard renders a Feishu Card 2.0 permission card
// from an abstract gateway.Card. Each option becomes a primary
// button whose value carries the request_id so the inbound Action
// carries it back.
//
// Card 2.0 shape (https://open.feishu.cn/document/uAjLw4CM/ukzMukzMukzM/feishu-cards/card-json-v2-structure):
//
//	{
//	  "schema": "2.0",
//	  "config": { "width_mode": "fill" },
//	  "header": { "title": {...}, "template": "blue" },
//	  "body":   { "elements": [ ... ] }
//	}
//
// Returned string is the card JSON itself — NOT wrapped in
// {"card": ...}. The wrapper is wrong: Feishu reads this string as
// the value of the `content` field in the create_message / patch
// request body, and `content` is the card object directly (no extra
// `card` key). The pre-migration code did wrap in {"card": ...},
// which Feishu silently accepted as a no-op for some configurations
// but rendered as blank/garbage for the receipt card (the user
// reported "feishu上没看到起效果" until we aligned with the Card
// 2.0 envelope).
func buildInteractiveCard(c *gateway.Card) (string, error) {
	if c.RequestID == "" {
		return "", errors.New("feishu: card missing request_id")
	}
	options := c.Options
	if len(options) == 0 {
		options = []string{"Allow", "Deny"}
	}
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
		"schema": "2.0",
		"config": map[string]any{"width_mode": "fill"},
		"header": map[string]any{
			"title":    map[string]any{"tag": "plain_text", "content": c.Title},
			"template": "blue",
		},
		"body": map[string]any{
			"elements": []map[string]any{
				{"tag": "markdown", "content": body},
				{"tag": "action", "actions": actions},
			},
		},
	}
	b, err := encodeCardJSON(card)
	if err != nil {
		return "", fmt.Errorf("feishu: encode permission card: %w", err)
	}
	return string(b), nil
}

// encodeCardJSON serialises a card map to JSON with HTMLEscape
// disabled. Go's default json.Marshal escapes "<" and ">" as <
// and > (HTMLEscape=true) which clutters the wire payload with
// unnecessary escapes. Card bodies legitimately contain "<text_tag
// color='neutral'>..." inline HTML — keeping the literal form is
// both shorter and matches the examples in Feishu's official
// documentation.
func encodeCardJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	// json.Encoder.Encode appends a trailing newline; Feishu's
	// API doesn't care, but trimming keeps the on-the-wire bytes
	// identical to what the previous json.Marshal produced.
	out := buf.Bytes()
	if n := len(out); n > 0 && out[n-1] == '\n' {
		out = out[:n-1]
	}
	return out, nil
}

// buildReceiptCard renders the rolling-log receipt as a Feishu
// Card 2.0 interactive card. Layout (top → bottom):
//
//  1. Header markdown — state.headerLine(r) (e.g. "⏳ 等待中",
//     "🔄 ⏳ N · HH:MM:SS", "✅ 已完成 HH:MM:SS").
//  2. Evicted marker — only when r.evicted > 0; rendered as
//     <text_tag color='neutral'> so it visually de-emphasises.
//  3. Entries — each r.entries[i] as a markdown element
//     "{Icon} {Text}". The log is FIFO; eviction is handled in the
//     receipt itself (see receipt.go: evictOverflowLocked).
//  4. <hr> divider — separates the log from the foot note.
//  5. Foot note — state.footLine(r) wrapped in <text_tag color='neutral'>.
//     Omitted entirely when footLine is empty (no hr, no footer).
//
// Returned string is the card JSON itself — NOT wrapped in
// {"card": ...}. See buildInteractiveCard for the rationale (Feishu
// reads the string as the value of `content`; the card object IS
// the content; no extra wrapper).
//
// Foot note content uses U+00B7 (·) as the separator — NEVER ": " —
// so Feishu's lark_md renderer doesn't parse it as a Markdown
// definition list and hoist the first value into the body
// (OpenClaw issue #59360). For footLine specifically the current
// state.String() never contains a key/value pair, so the pitfall
// only matters once a future PR adds agent identity fields
// (see docs/channel/feishu.md §9.4).
func buildReceiptCard(r *MessageReceipt) (string, error) {
	if r == nil {
		return "", errors.New("feishu: receipt is nil")
	}

	// Pre-size elements: 1 (header) + 1 (evicted, optional) +
	// len(entries) + 2 (hr + footer, when footLine is non-empty).
	// The evictOverflowLocked on the receipt side keeps entries
	// within Feishu's 50-element limit; this is the second line of
	// defence.
	headerLine := r.state.headerLine(r)
	elements := make([]map[string]any, 0, 3+len(r.entries))
	if headerLine != "" {
		elements = append(elements, map[string]any{
			"tag":     "markdown",
			"content": headerLine,
		})
	}
	if r.evicted > 0 {
		elements = append(elements, map[string]any{
			"tag":     "markdown",
			"content": fmt.Sprintf("…(前 %d 条已省略)", r.evicted),
		})
	}
	for i := range r.entries {
		e := r.entries[i]
		if e.Icon == "" && e.Text == "" {
			continue
		}
		content := e.Icon
		if e.Text != "" {
			if content != "" {
				content += " "
			}
			content += e.Text
		}
		// Thinking entries are rendered as a collapsible
		// panel (collapsed by default) so the long
		// reasoning text doesn't push the final answer
		// off the visible card surface. Mirrors the
		// OpenClaw Lark plugin's reasoning panel
		// (openclaw-lark src/card/builder.ts — see the
		// reasoning section in buildCompleteCard). The
		// icon + i18n_content + icon_position +
		// icon_expanded_angle fields are copied verbatim
		// from OpenClaw; they're not strictly required
		// by the schema but the Feishu client uses
		// `icon` to drive the click-to-expand affordance
		// and the panel renders as a flat (non-collapse)
		// block when it's missing.
		if e.Kind == "thinking" {
			elements = append(elements, map[string]any{
				"tag":      "collapsible_panel",
				"expanded": false,
				"header": map[string]any{
					"title": map[string]any{
						"tag":     "markdown",
						"content": "💭 思考",
						"i18n_content": map[string]any{
							"zh_cn": "💭 思考",
							"en_us": "💭 Thought",
						},
					},
					"vertical_align": "center",
					"icon": map[string]any{
						"tag":   "standard_icon",
						"token": "down-small-ccm_outlined",
						"size":  "16px 16px",
					},
					"icon_position":       "follow_text",
					"icon_expanded_angle": -180,
				},
				"border": map[string]any{
					"color":        "grey",
					"corner_radius": "5px",
				},
				"vertical_spacing": "8px",
				"padding":          "8px 8px 8px 8px",
				"elements": []map[string]any{
					{
						"tag":       "markdown",
						"content":   e.Text,
						"text_size": "notation",
					},
				},
			})
			continue
		}
		// v1.3.x (§13.6 / §13.9): tool_start and tool_end both
		// emit Kind="tool" so they collapse into collapsible
		// panels. Panel header shows e.Icon (🔧 / ✅ / ❌) +
		// e.Text (already pre-formatted as
		// "tool_name(args)" or "tool_name → output" /
		// "tool_name failed: err" by eventToEntry). Body
		// holds the same e.Text for users who expand.
		if e.Kind == "tool" {
			elements = append(elements, map[string]any{
				"tag":      "collapsible_panel",
				"expanded": false,
				"header": map[string]any{
					"title": map[string]any{
						"tag":     "markdown",
						"content": e.Icon + " " + e.Text,
					},
					"vertical_align": "center",
					"icon": map[string]any{
						"tag":   "standard_icon",
						"token": "down-small-ccm_outlined",
						"size":  "16px 16px",
					},
					"icon_position":       "follow_text",
					"icon_expanded_angle": -180,
				},
				"border": map[string]any{
					"color":        "grey",
					"corner_radius": "5px",
				},
				"vertical_spacing": "8px",
				"padding":          "8px 8px 8px 8px",
				"elements": []map[string]any{
					{
						"tag":       "markdown",
						"content":   e.Text,
						"text_size": "notation",
					},
				},
			})
			continue
		}
		elements = append(elements, map[string]any{
			"tag":     "markdown",
			"content": content,
		})
	}
	if note := r.state.footLine(r); note != "" {
		elements = append(elements, map[string]any{"tag": "hr"})
		// Footer styling matches the OpenClaw Lark plugin
		// (openclaw-lark src/card/builder.ts::buildFooter):
		//   - text_size: "notation" gives the foot note a
		//     compact, dim visual weight (the standard
		//     Card 2.0 size for footnotes / status lines).
		//   - On error, the content is wrapped in
		//     <font color='red'>...</font> so a failed
		//     session's footer is visually distinct from
		//     a successful one. OpenClaw wraps the i18n
		//     copies in red when isError is true.
		footerContent := note
		if r.state == StateError {
			footerContent = "<font color='red'>" + note + "</font>"
		}
		elements = append(elements, map[string]any{
			"tag":       "markdown",
			"content":   footerContent,
			"text_size": "notation",
		})
	}

	card := map[string]any{
		"schema": "2.0",
		"config": map[string]any{"width_mode": "fill"},
		"body":   map[string]any{"elements": elements},
	}
	b, err := encodeCardJSON(card)
	if err != nil {
		return "", fmt.Errorf("feishu: encode receipt card: %w", err)
	}
	return string(b), nil
}

// buildColdStartCard renders the minimal "⏳ 等待中" receipt used
// by Adapter.receiptFor when the gateway's pumpOutbound emits an
// OutText without a prior SendUserMessage (i.e. the agent turn
// started before the user message — a startup edge case). The
// returned card is posted via SendCard and the resulting message id
// seeds the receipt's cardMsgID so subsequent events PATCH the same
// surface in place. See docs/channel/feishu.md §5.2 for the
// first-send-then-PATCH strategy.
func buildColdStartCard() (string, error) {
	card := map[string]any{
		"schema": "2.0",
		"config": map[string]any{"width_mode": "fill"},
		"body": map[string]any{
			"elements": []map[string]any{
				{"tag": "markdown", "content": "⏳ 等待中"},
			},
		},
	}
	b, err := encodeCardJSON(card)
	if err != nil {
		return "", fmt.Errorf("feishu: encode cold-start card: %w", err)
	}
	return string(b), nil
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
	_, err := a.SendMessageText(ctx, chatID, text, "")
	return err
}

// SendMessageText is the message-ID-returning variant of SendMessage.
// Used by MessageReceipt (F-25) so the reply text line can be edited
// in place via UpdateMessage on subsequent state transitions.
//
// rootID is forwarded as Feishu root_id (reply-in-thread anchor). v1.3.x
// (§13.10) passes msg.ReplyTo here for slash-command replies so they
// thread visually to the user's `/command` message. Empty rootID yields
// a fresh top-level message (legacy behavior).
//
// Returns (messageID, error). On error, messageID is "".
func (a *Adapter) SendMessageText(ctx context.Context, chatID, text, rootID string) (string, error) {
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
	msgID, err := a.sendContent(ctx, chatID, larkim.MsgTypeText, string(content), rootID)
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
// mapStateToFeishuEmoji converts an abstract MessageState value
// to the corresponding Feishu predefined emoji_type identifier.
// F-31 §8.1 contract: Channel owns the state → visual mapping.
//
// Must use Feishu predefined emoji_type identifiers (not raw
// unicode) — passing unicode to the reaction API returns
// 99992354 "data not found".
//
// Returns "" for unknown states (forward-compatible silent drop).
// mapStateToFeishuEmoji converts an abstract MessageState value
// to the corresponding Feishu predefined emoji_type identifier.
// F-31 §8.1 contract: Channel owns the state → visual mapping.
//
// Must use Feishu predefined emoji_type identifiers (not raw
// unicode) — passing unicode to the reaction API returns
// 99992354 "data not found".
//
// Returns "" for unknown states (forward-compatible silent drop).
func mapStateToFeishuEmoji(state agent.MessageState) string {
	switch state {
	case agent.StateReceived:
		return "OneSecond" // ⏳
	case agent.StateForwarded:
		return "OnIt" // 🔄
	case agent.StateDone:
		return "DONE" // ✅
	case agent.StateError:
		return "THUMBSUP" // closest predefined indicator of "failed"
	}
	return ""
}

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
// rootID is the Feishu root_id (reply-in-thread anchor). When non-empty,
// the message is rendered as a reply to that user message; empty means
// a fresh top-level message. See §15.2 of docs/channel/feishu.md.
//
// v1.3.x (§13.10 fallback): when the Reply endpoint returns a
// terminal error code (230011 recalled / 231003 deleted), we retry
// as a top-level Create so the user still sees a message. The
// pattern mirrors openclaw-lark's runWithMessageUnavailableGuard.
// See docs/channel/feishu.md §13.10 / §15.7.
//
// Returns "" + error on failure. Empty message ID on success is
// possible if the API omits it (defensive — should not happen).
func (a *Adapter) sendContent(ctx context.Context, chatID, msgType, content, rootID string) (string, error) {
	a.mu.RLock()
	send := a.sendFunc
	if send == nil && a.larkClient != nil {
		send = a.sendViaLark
	}
	a.mu.RUnlock()
	if send == nil {
		return "", errors.New("feishu: REST client is nil")
	}
	msgID, err := send(ctx, chatID, msgType, content, rootID)
	if err != nil && rootID != "" && isFeishuTerminalMessageCode(err) {
		a.logger.Warn("feishu: reply target unavailable, falling back to top-level",
			"root_id", rootID, "msg_type", msgType, "err", err)
		return send(ctx, chatID, msgType, content, "")
	}
	return msgID, err
}

func (a *Adapter) sendViaLark(ctx context.Context, chatID, msgType, content, rootID string) (string, error) {
	if a.larkClient == nil || a.larkClient.Im == nil || a.larkClient.Im.V1 == nil || a.larkClient.Im.V1.Message == nil {
		return "", errors.New("feishu: REST client is nil")
	}

	// v1.3.x (§13.10): when rootID is set, dispatch to the Reply
	// endpoint (`POST /im/v1/messages/{message_id}/reply`) which
	// uses the path message_id as the Feishu root_id. PatchMessage
	// (PATCH /im/v1/messages/{id}) preserves root_id automatically
	// across subsequent in-place updates, so once Reply-creates the
	// card the thread is locked in.
	if rootID != "" {
		return a.sendViaLarkReply(ctx, rootID, msgType, content)
	}
	return a.sendViaLarkCreate(ctx, chatID, msgType, content)
}

// sendViaLarkReply invokes POST /im/v1/messages/{rootID}/reply
// and returns the new message ID. Used when v1.3.x §13.10 needs
// to thread a bot reply to a specific user message. Returns a
// formatted error carrying the Feishu API code when the call
// fails; callers inspect with isFeishuTerminalMessageCode to
// decide whether to fall back to Create.
func (a *Adapter) sendViaLarkReply(ctx context.Context, rootID, msgType, content string) (string, error) {
	replyBody := larkim.NewReplyMessageReqBodyBuilder().
		MsgType(msgType).
		Content(content).
		Build()
	req := larkim.NewReplyMessageReqBuilder().
		MessageId(rootID).
		Body(replyBody).
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
	if resp.Data != nil && resp.Data.MessageId != nil {
		return *resp.Data.MessageId, nil
	}
	return "", nil
}

// sendViaLarkCreate invokes POST /im/v1/messages (top-level Create).
// Used both as the no-rootID path and as the fallback when Reply
// fails on a recalled/deleted user message.
func (a *Adapter) sendViaLarkCreate(ctx context.Context, chatID, msgType, content string) (string, error) {
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
	if resp.Data != nil && resp.Data.MessageId != nil {
		return *resp.Data.MessageId, nil
	}
	return "", nil
}

// isFeishuTerminalMessageCode returns true when a Reply failure
// carries Feishu error code 230011 (message recalled by sender) or
// 231003 (message deleted). These are terminal states — the
// root_id will never be replyable again, so the caller should
// degrade gracefully (fall back to top-level Create) instead of
// retrying.
//
// The pattern mirrors openclaw-lark's runWithMessageUnavailableGuard
// (src/core/message-unavailable.ts). We diverge from openclaw-lark
// by skipping the global unavailability cache for now: per-turn
// retry storms are unlikely in our hot path, and a per-call fallback
// is simpler. Add the cache if retry spam becomes an issue in
// production. See docs/channel/feishu.md §15.7.
func isFeishuTerminalMessageCode(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	// Cheap string match for the formatted "code NNNNN" suffix our
	// sendViaLarkReply helper attaches (e.g. "...code 230011").
	// Faster than unwrapping and avoids depending on a specific SDK
	// error type for the transport-error path. We also accept the
	// colon form ("code:NNNNN") in case future helpers format that
	// way or upstream wrappers translate the error.
	switch {
	case strings.Contains(msg, "code 230011"),
		strings.Contains(msg, "code:230011"):
		return true
	case strings.Contains(msg, "code 231003"),
		strings.Contains(msg, "code:231003"):
		return true
	}
	// Defensive: also unwrap a *larkcore.CodeError if the SDK
	// returns one directly (covers future SDK changes that might
	// drop the formatted suffix).
	var ce *larkcore.CodeError
	if errors.As(err, &ce) {
		return ce.Code == 230011 || ce.Code == 231003
	}
	return false
}

// SendCard posts an interactive card and returns the created message
// ID. The content must be a JSON-serialized card envelope
// ({"card": {...}}) — see buildInteractiveCard / buildReceiptCard
// for the producers. Used by the receipt FSM for the first-send step
// (receipt.renderLocked → SendCard → later PatchMessage cycles).
//
// rootID is forwarded as the Feishu root_id (reply-in-thread anchor).
// v1.3.x: receipt cold-start passes the user message id here so the
// card is rendered as a reply to the user's message; subsequent
// PatchMessage calls preserve the thread automatically. See §15.2 of
// docs/channel/feishu.md.
func (a *Adapter) SendCard(ctx context.Context, chatID, content, rootID string) (string, error) {
	if strings.TrimSpace(chatID) == "" {
		return "", errors.New("feishu: chat_id is required")
	}
	if strings.TrimSpace(content) == "" {
		return "", errors.New("feishu: card content is required")
	}
	msgID, err := a.sendContent(ctx, chatID, larkim.MsgTypeInteractive, content, rootID)
	a.logOutgoing("send_card", chatID, msgID, err)
	return msgID, err
}

// PatchMessage replaces the entire body of an existing message with
// the supplied content (Feishu PATCH /im/v1/messages/{id}). For
// interactive cards content is the card envelope; for text/rich-text
// messages it's the standard {"text": "..."} or {"post": ...} shape.
//
// IMPORTANT: This is the SDK's *Patch* endpoint, NOT *Update*. The
// Update method only supports text/rich-text messages; Patch is the
// only one that handles interactive cards. Confusing the two is the
// single most common pitfall here — see docs/channel/feishu.md §3.4
// and §6.3.
//
// Returns "" on success (PATCH has no useful body). The
// receipt's renderLocked is the only caller today; it ignores the
// return value other than nil/non-nil.
func (a *Adapter) PatchMessage(ctx context.Context, messageID, content string) error {
	if strings.TrimSpace(messageID) == "" {
		return errors.New("feishu: message_id is required")
	}
	if strings.TrimSpace(content) == "" {
		return errors.New("feishu: card content is required")
	}
	a.mu.RLock()
	update := a.updateFunc
	a.mu.RUnlock()
	if update == nil {
		return errors.New("feishu: update client is nil")
	}
	err := update(ctx, messageID, content)
	a.logOutgoing("patch_message", "", messageID, err)
	return err
}

// updateViaLark is the production implementation of the PATCH dispatch
// (wired in by NewAdapter as a.updateFunc). Builds a PatchMessageReq
// and calls larkClient.Im.V1.Message.Patch (the PATCH endpoint). The
// body must already be the JSON-encoded card envelope (or text/post
// envelope) the API expects.
func (a *Adapter) updateViaLark(ctx context.Context, messageID, content string) error {
	if a.larkClient == nil || a.larkClient.Im == nil || a.larkClient.Im.V1 == nil || a.larkClient.Im.V1.Message == nil {
		return errors.New("feishu: REST client is nil")
	}
	body := larkim.NewPatchMessageReqBodyBuilder().
		Content(content).
		Build()
	req := larkim.NewPatchMessageReqBuilder().
		MessageId(messageID).
		Body(body).
		Build()
	resp, err := a.larkClient.Im.V1.Message.Patch(ctx, req)
	if err != nil {
		return fmt.Errorf("feishu: patch message: %w", err)
	}
	if resp == nil {
		return errors.New("feishu: patch message returned nil response")
	}
	if !resp.Success() {
		return fmt.Errorf("feishu: patch message failed with code %d", resp.Code)
	}
	return nil
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
