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
//
// replyInThread controls the Feishu body field reply_in_thread (F-37). When
// true, the message is rendered ONLY in the thread/topic (main chat shows
// a "X replies" indicator with no body); when false (default) the message
// appears inline in the main chat as a reply and is also collected in the
// thread panel. Thread-only is used for the intermediate agent progress
// stream (OutThinking / OutToolStart / OutToolEnd / OutCompaction) so the
// main chat stays focused on the final answer. Create-endpoint calls
// ignore this flag (the field has no equivalent there).
type sendMessageFunc func(ctx context.Context, chatID, msgType, content, rootID string, replyInThread bool) (string, error)

// updateMessageFunc is the in-place update boundary (Feishu PATCH
// /im/v1/messages/{id}). Kept behind a function field so unit tests
// can replace the network call with a small in-memory stub. PATCH
// is the SDK's *Patch* method (NOT *Update* — Update only supports
// text/rich-text messages; see receipt.go and docs/channel/feishu.md
// §3.4 for the rationale).
type updateMessageFunc func(ctx context.Context, messageID, content string) error

// mergeTextMessageFunc is the F-38 §3.1.3 in-place update boundary
// for the tool-merge path. Unlike updateMessageFunc (which goes via
// SDK Patch + card envelope), mergeTextMessageFunc edits a *text*
// thread reply in place via SDK Update + text envelope, so the two
// paths cannot share the same hook.
//
// Kept behind a function field so tests can replace the network call
// with a small in-memory stub without instantiating a larkClient.
// See internal/channel/feishu/tool_thread_merge.go for the merge
// flow.
type mergeTextMessageFunc func(ctx context.Context, messageID, merged string) error

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

	// health tracks the live WebSocket lifecycle for the
	// `nightme health` subcommand. Updated from SDK OnReady /
	// OnReconnecting / OnReconnected / OnDisconnected / OnError
	// callbacks plus inbound event dispatch + successful outbound
	// sends. Read via Health() / Adapter.HealthSnapshot().
	health *WSHealth

	// prober is the F-41 active-reconnect prober. Started on
	// OnDisconnected, stopped on OnReconnected / OnReady. The
	// prober's snapshot is merged into WSHealthSnapshot.Prober
	// for `nightme health` output.
	prober *prober

	// limiter 全局共享 token bucket（F-35）。所有出口 SDK call 前
	// 都过 Wait()，预防触发飞书 230001 / 230020 限流错误码。
	// 默认 5 QPS / burst 1（保守）。详见 docs/feat/F-35-ratelimit.md。
	limiter *Limiter

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

	// threadReplyLimiter enforces Feishu's 5 QPS per-user /
	// per-group bot API limit on thread replies. Without this
	// gate, a hot agent (10 tool/sec) overruns the limit and
	// Feishu returns 230020 / HTTP 429 for some replies,
	// leaving the thread with gaps. See postThreadReply.
	threadReplyLimiter *threadReplyLimiter

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

	// toolEventBuf (F-38 §3.1.3) tracks in-flight OutToolStart
	// thread replies so the matching OutToolEnd can PATCH the
	// same message with the merged body (start line + result
	// line) instead of posting a second thread reply. Keyed by
	// userMsgID (= ReplyTo = currentTurnUserMsgID per SPEC §2.2);
	// FIFO per userMsgID because stream-json's tool_use →
	// tool_result pairs are strictly ordered. Lifecycle: entries
	// are pushed on OutToolStart, popped on the matching
	// OutToolEnd, and cleared on turn end (clearToolEvents) or
	// adapter stop (clearAllToolEvents).
	//
	// Concurrency: reads/writes go through a.mu. Same lock as
	// receipts / messageStates — adapter.Send is single-threaded
	// per ChatSession (SPEC §1.3), so contention is non-issue.
	toolEventBuf map[string][]toolEventEntry

	// These hooks have production defaults and are intentionally kept as
	// fields so tests can replace the network boundary with a small function.
	wsStart    func(context.Context) error
	wsClose    func()
	sendFunc   sendMessageFunc
	updateFunc updateMessageFunc
	// mergeTextFunc (F-38 §3.1.3) is the hookable boundary for the
	// tool-merge PATCH path. Wired in NewAdapter to
	// mergeTextViaUpdate; tests can replace to inject stubs
	// without instantiating a larkClient. Separate from
	// updateFunc because the two update paths use different SDK
	// methods (Update for text, Patch for cards).
	mergeTextFunc mergeTextMessageFunc

	// F-watch §6.10: lazy-cached bot open_id for mention strip /
	// HasMention detection. Populated on first inbound by
	// fetchBotOpenID via SDK GetBotIdentity (which itself caches
	// 30min). Empty string = identity fetch not yet succeeded or
	// failed; computeHasMention falls back to false in that case
	// (safe: drop a few messages rather than attribute wrong).
	botOpenID     string
	botOpenIDOnce sync.Once
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
		threadReplyLimiter:  newThreadReplyLimiter(200*time.Millisecond, 800*time.Millisecond),
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
			now := time.Now()
			a.health.recordConnect(now)
			a.logger.Info("feishu: ws connected",
				"app_id", cfg.Feishu.AppID,
				"reconnect_count", a.health.Snapshot().ReconnectCount)
		}),
		larkws.WithOnError(func(err error) {
			if err == nil {
				return
			}
			now := time.Now()
			a.health.recordError(now, err.Error())
			a.logger.Warn("feishu: ws error",
				"app_id", cfg.Feishu.AppID,
				"err", err.Error())
		}),
		larkws.WithOnDisconnected(func() {
			now := time.Now()
			a.health.recordDisconnect(now)
			a.logger.Warn("feishu: ws disconnected",
				"app_id", cfg.Feishu.AppID)
			// F-41: start the 30s prober that force-reconnects until
			// the SDK reports OnReconnected. Started on every
			// disconnect — the prober is self-stopping on reconnect
			// and idempotent (Start is a no-op when already running).
			if a.prober != nil {
				if a.prober.Start() {
					a.logger.Info("feishu: reconnect prober started",
						"app_id", cfg.Feishu.AppID,
						"interval", defaultProberInterval.String())
				}
			}
		}),
		larkws.WithOnReconnecting(func() {
			now := time.Now()
			a.health.recordReconnecting(now, "")
			snap := a.health.Snapshot()
			a.logger.Warn("feishu: ws reconnecting",
				"app_id", cfg.Feishu.AppID,
				"reconnect_count", snap.ReconnectCount)
		}),
		larkws.WithOnReconnected(func() {
			now := time.Now()
			a.health.recordConnect(now)
			a.logger.Info("feishu: ws reconnected",
				"app_id", cfg.Feishu.AppID,
				"reconnect_count", a.health.Snapshot().ReconnectCount)
			// F-41: stop the prober — the WS is back, no more
			// forced Stop+Start needed. Safe to call when the prober
			// isn't running (no-op).
			if a.prober != nil {
				a.prober.Stop()
				a.logger.Info("feishu: reconnect prober stopped",
					"app_id", cfg.Feishu.AppID,
					"force_attempts", a.prober.Snapshot().ForceCount)
			}
		}),
	)
	a.larkClient = lark.NewClient(cfg.Feishu.AppID, cfg.Feishu.AppSecret)
	a.wsStart = a.client.Start
	a.wsClose = a.client.Close
	a.sendFunc = a.sendViaLark
	a.updateFunc = a.updateViaLark
	// F-38 §3.1.3: tool-merge PATCH path. mergeTextFunc defaults
	// to mergeTextViaUpdate (a thin wrapper around UpdateMessage)
	// so callers can mock via the field without instantiating a
	// larkClient. Distinct from updateFunc (card PATCH).
	a.mergeTextFunc = a.mergeTextViaUpdate

	// F-35: 全局共享 token bucket（保守 5 QPS / burst 1）。
	// 进程内单实例，覆盖所有 5 类飞书出口 API。
	a.limiter = NewLimiter(cfg.Feishu.RateLimit, slog.Default())

	// F-40: WS lifecycle observability. Allocates the WSHealth
	// struct; the SDK callbacks wired above (WithOnReady / OnError /
	// OnDisconnected / OnReconnecting / OnReconnected) update it.
	a.health = &WSHealth{}

	// F-41: active reconnect prober. The restarter closure forces
	// ch.Stop() + sleep + ch.Start() on every 30s tick while the
	// WS is disconnected, which effectively overrides the SDK's
	// 2-minute default reconnectInterval. Self-stops when the SDK
	// reports Connected=true (checked via Health() inside the tick).
	a.prober = newProber(a, func(ctx context.Context) error {
		// Respect the prober's lifecycle context so daemon shutdown
		// can interrupt an in-flight Stop+100ms+Start cycle.
		if err := a.Stop(ctx); err != nil {
			return err
		}
		select {
		case <-time.After(defaultProberBackoff):
		case <-ctx.Done():
			return ctx.Err()
		}
		return a.Start(ctx)
	})

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

	// F-38 §3.1.3: release all per-turn tool-merge state on
	// shutdown. Any in-flight tool pairs (Start posted, End
	// never arrived — agent crashed mid-turn) lose their PATCH
	// target; the underlying thread replies remain posted and
	// visible to the user (we just don't try to merge them
	// after restart). Cheap; not on the hot path.
	a.clearAllToolEvents()

	return waitErr
}

// Incoming returns the adapter's normalized message stream.
func (a *Adapter) Incoming() <-chan channel.Message { return a.incoming }

// Send implements the v0.3 channel.Channel interface. It dispatches
// each OutboundKind to the corresponding Feishu API call:
//
//	OutText              → CreateMessage (msg_type=text)
//	OutToolStart/End     → F-34 thread-reply as the "● Tool(args)"
//	                       / "⎿  …" Claude Code-style lines. F-38
//	                       §3.1.3: each Start posts a fresh thread
//	                       reply; the matching End PATCHes that same
//	                       reply with start body + "\n" + result body
//	                       (one thread reply per tool, not two).
//	                       Orphan Ends + PATCH failures fall back to
//	                       posting a fresh result thread reply.
//	OutMessageState      → AddReaction on MessageState.MessageID
//	                       with state-specific emoji_type (F-31 §8.3).
//	                       Idempotency via a.messageStates map.
//	OutMessageStateRemoved → DeleteReaction on MessageState.ReactionID
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
	// F-37: replyInThread=false — the cold-start card is the
	// pinned main-chat answer; thread-only would leave the main
	// chat empty until the receipt PATCHes happen.
	msgID, err := a.SendCard(ctx, chatID, cardBody, userMsgID, false)
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
		// F-34: thinking is posted to the user message thread so
		// the main chat stays focused on the final answer. Falls
		// back to a top-level send if ReplyTo is empty (orphan
		// event, e.g. startup init).
		//
		// F-think §3.1.2: thinking is rendered as a Feishu
		// Card 2.0 with lark_md content (via
		// postThreadMarkdownReply) so code blocks / lists /
		// emphasis survive the round-trip. Plain text would
		// collapse to raw markdown source in the chat. Long
		// bodies are split into multiple div elements by
		// F-37's splitMarkdownForDivs inside buildThinkingCard.
		//
		// F-34 review P1-3: also Touch the receipt so the main
		// chat's receipt card header keeps ticking while the
		// agent thinks. Without this the "🔄 ⏳ N · HH:MM:SS"
		// line freezes between visible events.
		//
		// F-37: replyOnly=true keeps the 💭 line OUT of the main
		// chat — it lives only in the thread so the receipt card
		// (the pinned final answer) stays the main visible item.
		//
		// Runtime gate (cmd/nightme/run.go::newEventHandler):
		// when the chat has /think off, OutThinking is dropped
		// before reaching this case. This case therefore assumes
		// the chat wants thinking rendered — see docs/SPEC.md
		// §3.1.2.
		if err := a.postThreadMarkdownReply(ctx, msg.ChatID, msg.ReplyTo, "💭 "+msg.Text, true); err != nil {
			return err
		}
		if r := a.receiptFor(ctx, msg.ChatID, msg.ReplyTo); r != nil {
			_ = r.Touch(ctx)
		}
		return nil

	case gateway.OutMessageState:
		// F-31: read abstract state from typed MessageStatePayload,
		// map to feishu emoji_type internally. Channel decides
		// how to render.
		if msg.MessageState == nil {
			return errors.New("feishu: OutMessageState missing MessageState payload")
		}
		if msg.MessageState.MessageID == "" {
			return errors.New("feishu: OutMessageState missing MessageID")
		}
		messageID := msg.MessageState.MessageID
		state := msg.MessageState.State
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
		if msg.MessageState == nil {
			return errors.New("feishu: OutMessageStateRemoved missing MessageState payload")
		}
		if msg.MessageState.MessageID == "" {
			return errors.New("feishu: OutMessageStateRemoved missing MessageID")
		}
		if msg.MessageState.ReactionID == "" {
			return errors.New("feishu: OutMessageStateRemoved missing ReactionID")
		}
		return a.DeleteReaction(ctx, msg.MessageState.MessageID, msg.MessageState.ReactionID)

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
		//
		// F-37: reply_in_thread stays false (default) for the
		// permission card — discoverability matters more than chat
		// cleanliness when a tool is blocked on user approval.
		_, err = a.sendContent(ctx, msg.ChatID, interactiveMessageType, content, msg.ReplyTo, false)
		return err

	case gateway.OutToolStart:
		// F-34: tool_start is posted to the user message
		// thread as the "call" line (`● Tool(args)`), matching
		// Claude Code's terminal UX. The receipt card no
		// longer carries tool entries.
		//
		// F-38 §3.1.3: ONE-thread-reply UX. We post the call
		// line here and remember its Feishu message_id. When
		// the matching OutToolEnd arrives, we PATCH the same
		// reply with the merged body (start body + newline +
		// result body) instead of posting a second thread
		// reply. See tool_thread_merge.go for the buffer
		// mechanics.
		//
		// F-37: replyOnly=true — `● Tool(args)` lives in the
		// thread, not the main chat. Receipt card stays the
		// pinned answer.
		body := formatToolStartCall(toolName(msg), toolArgs(msg))
		startMsgID, err := a.postThreadReplyWithID(ctx, msg.ChatID, msg.ReplyTo, body, true)
		if err != nil {
			return err
		}
		// F-38: remember the start msg_id so OutToolEnd can
		// PATCH it. pushToolStart is a no-op when startMsgID is
		// empty (orphan path — rootID was "" → sendRawOutText
		// fallback → no msg_id to PATCH). On the next End the
		// buffer miss will route through the fallback path
		// below (postThreadReply as fresh thread reply).
		a.pushToolStart(msg.ReplyTo, startMsgID, body)
		// F-34 review P1-3: Touch the receipt so the header
		// keeps ticking while tools run.
		if r := a.receiptFor(ctx, msg.ChatID, msg.ReplyTo); r != nil {
			_ = r.Touch(ctx)
		}
		return nil

	case gateway.OutToolEnd:
		// F-34: tool_end is posted to the user message thread
		// as the "result" line (`⎿  summary`), the second half
		// of Claude Code's two-line UX. Args are NOT included
		// here — they live on the preceding call line from
		// OutToolStart.
		//
		// F-38 §3.1.3: instead of posting a fresh thread reply,
		// try to PATCH the preceding OutToolStart's reply with
		// the merged body. On hit, the user sees a single
		// reply containing both call and result. On miss
		// (orphan End — buffer empty) or PATCH failure, fall
		// back to posting the result as a fresh thread reply
		// so the data is never silently dropped.
		var toolErr error
		if msg.Tool != nil {
			toolErr = msg.Tool.Err
		}
		resultBody := summarizeToolResult(toolName(msg), toolOutput(msg), toolErr)
		entry, ok := a.popToolStart(msg.ReplyTo)
		if ok {
			merged := entry.startBody + "\n" + resultBody
			// Wrap in transient retry (F-36) — PATCH can hit
			// timeout / EOF just like Create; a transient
			// blip shouldn't drop the merged result. After
			// retry exhaustion the caller falls through to
			// the fresh-thread-reply fallback below.
			mergeErr := a.mergeToolReply(ctx, entry.startMsgID, merged)
			if mergeErr == nil {
				// F-34 review P1-3: Touch the receipt so the
				// header keeps ticking while tools run.
				if r := a.receiptFor(ctx, msg.ChatID, msg.ReplyTo); r != nil {
					_ = r.Touch(ctx)
				}
				return nil
			}
			// PATCH failed (retry exhausted or non-transient).
			// Log + fall through to fallback so the result
			// isn't lost.
			a.logger.Warn("feishu: tool merge PATCH failed, falling back to fresh thread reply",
				"chat_id", msg.ChatID,
				"user_msg_id", msg.ReplyTo,
				"start_msg_id", entry.startMsgID,
				"err", mergeErr)
		}
		// Fallback: post resultBody as a fresh thread reply.
		// Same path as pre-F-38 behaviour; preserves visibility
		// when the merge can't happen.
		if err := a.postThreadReply(ctx, msg.ChatID, msg.ReplyTo, resultBody, true); err != nil {
			return err
		}
		// F-34 review P1-3: Touch the receipt so the header
		// keeps ticking while tools run.
		if r := a.receiptFor(ctx, msg.ChatID, msg.ReplyTo); r != nil {
			_ = r.Touch(ctx)
		}
		return nil

	case gateway.OutTyping:
		// No native Feishu equivalent (typing indicators come from
		// the OpenAPI, not the bot's message API). Silently drop.
		return nil

	case gateway.OutResult:
		if msg.Result == nil {
			return errors.New("feishu: OutResult missing Result payload")
		}
		text := strings.TrimSpace(msg.Result.Text)
		if text == "" && !msg.Result.IsError {
			return nil
		}
		// Icon prefix only when there's actual error text — without it
		// the user would see a meaningless bare-emoji standalone message.
		if msg.Result.IsError && text != "" {
			text = "❌ " + text
		}
		// F-39: deliver the full result as an independent reply anchored at
		// userMsgID. We deliberately do NOT call receipt.SetCompleted here —
		// the receipt stays StateExecuting so subsequent OutUsage / OutInit /
		// TaskList can still update the footer (token counts, agent name,
		// task checklist). EventDone / EventError is the terminal signal that
		// flips state to StateCompleted and collapses the rolling log.
		return a.sendResultAsReply(ctx, msg.ChatID, msg.ReplyTo, text)

	case gateway.OutUsage:
		receipt := a.receiptFor(ctx, msg.ChatID, msg.ReplyTo)
		if receipt == nil {
			return nil
		}
		if msg.Usage == nil {
			return errors.New("feishu: OutUsage missing Usage payload")
		}
		return receipt.Append(ctx, agent.AgentEvent{
			Kind: agent.EventUsage,
			Usage: &agent.UsageEvent{
				InputTokens:              msg.Usage.InputTokens,
				OutputTokens:             msg.Usage.OutputTokens,
				CacheCreationInputTokens: msg.Usage.CacheCreationInputTokens,
				CacheReadInputTokens:     msg.Usage.CacheReadInputTokens,
				CostUSD:                  msg.Usage.CostUSD,
			},
		})

	case gateway.OutCompaction:
		// Compaction marker is a one-shot, low-frequency event
		// (not a stream like tool calls). Per ops decision
		// 2026-08-04: "OutToolStart/End/OutThinking use
		// ReplyInThread; everything else uses
		// ReplyInThreadAndChat" — OutCompaction falls in
		// 'everything else', so it stays visible in the main
		// chat (replyOnly=false). A brief "✶ Compacting…" line
		// in main chat is informative, not noise.
		if err := a.postThreadReply(ctx, msg.ChatID, msg.ReplyTo,
			"✶ Compacting conversation…", false); err != nil {
			return err
		}
		// F-34 review P1-3: Touch the receipt so the header
		// keeps ticking during compaction.
		if r := a.receiptFor(ctx, msg.ChatID, msg.ReplyTo); r != nil {
			_ = r.Touch(ctx)
		}
		return nil

	case gateway.OutInit:
		receipt := a.receiptFor(ctx, msg.ChatID, msg.ReplyTo)
		if receipt == nil {
			return nil
		}
		// Forward agent identity + workspace so the receipt
		// card's foot note can render
		// "Agent | cwd | tokens" (see docs/channel/feishu.md
		// §9.3 + the footLine composer in receipt.go).
		if msg.Init == nil {
			return errors.New("feishu: OutInit missing Init payload")
		}
		return receipt.Append(ctx, agent.AgentEvent{
			Kind: agent.EventInit,
			Init: msg.Init,
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
		//
		// F-37: slash-command replies stay visible in main chat
		// (reply_in_thread=false) — the user fired the command and
		// is waiting for the answer at the cursor.
		if msg.Text == "" {
			return errors.New("feishu: OutCommandReply missing text")
		}
		_, err := a.SendMessageText(ctx, msg.ChatID, msg.Text, msg.ReplyTo, false)
		return err

	case gateway.OutTaskCreate, gateway.OutTaskUpdate:
		// F-38: replace the per-turn checklist in the current
		// receipt. We never call postThreadReply for task tools —
		// the bridge suppresses the generic ToolStart/ToolEnd pair
		// on a confirmed success result so the user sees a single
		// task element in the receipt rather than two thread lines
		// per operation.
		if msg.TaskList == nil {
			return errors.New("feishu: OutTask*/TaskList payload is nil")
		}
		receipt := a.receiptFor(ctx, msg.ChatID, msg.ReplyTo)
		if receipt == nil {
			// Cold-start failed (existing pattern for other Kinds).
			// Degrade gracefully so the user still sees the
			// checklist in the main chat as a standalone text
			// bubble, even when no receipt card could be PATCHed.
			return a.sendRawOutText(ctx, msg.ChatID, renderTaskFallbackText(msg.TaskList))
		}
		return receipt.SetTaskList(ctx, msg.TaskList)
	}
	return fmt.Errorf("feishu: unsupported outbound kind %v", msg.Kind)
}

// sendRawOutText is the degraded send path used when a receipt
// can't be created (e.g. the channel.post text API failed). Sends
// the text as a new standalone message so the user still sees
// something.
func (a *Adapter) sendRawOutText(ctx context.Context, chatID, text string) error {
	// Empty rootID: this degraded path is hit when receipt cold-start
	// failed and there's there's no userMsgID to thread to. The fallback
	// message remains a top-level text bubble (legacy behavior).
	// replyInThread stays false — Create path with empty rootID
	// can't honor it anyway.
	_, err := a.SendMessageText(ctx, chatID, text, "", false)
	return err
}

// sendResultAsReply (F-39) posts the final agent result as a SEPARATE reply
// anchored at userMsgID, independent from the rolling-log receipt card so
// no dedup is needed. Always renders via Feishu's rich markdown surface
// (Card 2.0 or Post+md or plain text) so code blocks, tables, lists, and
// headers in Claude Code's stream-json output survive the round-trip.
//
// Dispatch (mirrors cc-connect `platform/feishu/feishu.go::buildReplyContent`):
//   - no markdown indicators  → MsgTypeText (plain text bubble)
//   - tables > resultCardTableLimit → MsgTypePost + tag:"md"
//   - default                 → MsgTypeInteractive (Card 2.0)
//
// envelopeDefense (defensive ceiling below the 30 KB Card body envelope):
// if the rendered body still exceeds resultCardEnvelopeBudget after the
// perResultMaxBytes cap, the input text is truncated and re-built. For
// OutResult from Claude Code this is a guard for adversarial input, not
// a hot path.
//
// Always renders as ReplyInThreadAndChat (reply_in_thread omitted) so
// the result card is visible inline in the main chat — matching the
// F-37 §13.13 surface-table mapping for the final answer.
//
// RATE-LIMIT / RETRY layering:
//   - layer 1: F-35 global limiter (`a.limiter.Wait`) inside sendContent;
//     prevents the SDK hitting Feishu's 230001 / 230020 throttle codes.
//   - layer 2: F-36 transient retry wraps sendContent; transient network
//     errors get exponential backoff. Final hit returns to caller for log.
func (a *Adapter) sendResultAsReply(
	ctx context.Context, chatID, userMsgID, text string,
) error {
	if strings.TrimSpace(chatID) == "" {
		return errors.New("feishu: chat_id is required")
	}
	if strings.TrimSpace(text) == "" {
		// Defensive: caller already filters empty non-error results.
		return nil
	}

	// Orphan result (no userMsgID) — top-level plain text bubble.
	// Mirrors the OutCommandReply fallback: no anchor → no thread.
	if userMsgID == "" {
		return a.sendRawOutText(ctx, chatID, text)
	}

	// Apply markdown sanitize pipeline (URL / fence / image / heading /
	// code-block protection) so the rendered output doesn't get rejected
	// by Feishu (230001 invalid href) or break lark_md rendering on edge
	// cases. See internal/channel/feishu/card_sanitize.go.
	sanitized := SanitizeCardMarkdown(text)

	// Hard cap to stay well under 30 KB envelope with margin for JSON
	// envelope + future growth.
	if len(sanitized) > perResultMaxBytes*4 { // byte budget (multi-byte safe)
		sanitized = truncateRunes(sanitized, perResultMaxBytes)
	}

	msgType, body, err := buildResultPayload(sanitized)
	if err != nil {
		return err
	}

	// Envelope defensive cap. Adversarial input that survives the byte
	// budget above (e.g., very wide ASCII tables or emojis) might still
	// push the card body past 28 KB once envelope JSON wrapping is added.
	if len(body) > resultCardEnvelopeBudget {
		a.logger.Warn("feishu: result reply over envelope, truncating",
			"chat_id", chatID, "user_msg_id", userMsgID,
			"body_bytes", len(body))
		truncated := truncateRunes(sanitized, resultCardEnvelopeBudget/3)
		msgType, body, err = buildResultPayload(truncated)
		if err != nil {
			return err
		}
	}

	_, err = a.sendContent(ctx, chatID, msgType, body, userMsgID, false)
	return err
}

// postThreadReply posts body as a thread reply anchored at rootID.
// rootID = msg.ReplyTo = currentTurnUserMsgID. When rootID is empty
// (orphan event, e.g. startup EventInit) the helper falls back to a
// top-level text send via sendRawOutText so the user still sees the
// message.
//
// replyOnly (F-37) controls Feishu body field reply_in_thread. true
// keeps the message OUT of the main chat (only the "X replies"
// indicator is visible) so the agent progress stream doesn't pollute
// the user's main view; false keeps the reply visible inline in the
// main chat. OutThinking / OutToolStart / OutToolEnd / OutCompaction
// pass true; receipt-card / outCard / slash-command replies pass false.
func (a *Adapter) postThreadReply(ctx context.Context, chatID, rootID, body string, replyOnly bool) error {
	// P1-4 (F-34 review): Feishu's bot API caps at 5 QPS per
	// user / per group. A hot agent running 10+ tools per turn
	// would otherwise overrun the limit and Feishu would drop
	// some replies, leaving the thread with gaps. The limiter
	// sleeps up to maxWait per call; if the wait would exceed
	// maxWait (the chat is already saturated) we return an
	// error so the caller can log + move on rather than block
	// the pump indefinitely.
	if a.threadReplyLimiter != nil {
		if err := a.threadReplyLimiter.Wait(ctx, chatID); err != nil {
			a.logger.Warn("feishu: thread reply rate-limited",
				"chat_id", chatID, "err", err)
			return err
		}
	}
	if rootID == "" {
		return a.sendRawOutText(ctx, chatID, body)
	}
	_, err := a.SendMessageText(ctx, chatID, body, rootID, replyOnly)
	return err
}

// postThreadReplyWithID is the F-38 §3.1.3 sibling of
// postThreadReply that returns the freshly-posted Feishu
// message_id so the caller can PATCH it later (tool merge path).
// On the orphan path (rootID == "") or any send failure, returns
// ("", err) — empty message_id signals "nothing to PATCH" to
// callers.
//
// Returns (msgID, err). err is non-nil only when the underlying
// SDK call failed; the (msgID == "") case is a successful
// fallback (sendRawOutText) and err is nil — caller decides
// whether to pushToolStart based on msgID emptiness, not on err.
//
// Limiter ordering is identical to postThreadReply: Wait() FIRST,
// then any work. Rationale same as the sibling helper (limiter
// check is cheap; SDK call is expensive).
func (a *Adapter) postThreadReplyWithID(ctx context.Context, chatID, rootID, body string, replyOnly bool) (string, error) {
	if a.threadReplyLimiter != nil {
		if err := a.threadReplyLimiter.Wait(ctx, chatID); err != nil {
			a.logger.Warn("feishu: thread reply rate-limited",
				"chat_id", chatID, "err", err)
			return "", err
		}
	}
	if rootID == "" {
		// Orphan: sendRawOutText returns "" message_id and nil
		// err on success — caller (pushToolStart) treats empty
		// msgID as a no-op.
		return "", a.sendRawOutText(ctx, chatID, body)
	}
	return a.SendMessageText(ctx, chatID, body, rootID, replyOnly)
}

// threadReplyLimiter is a per-key (chatID) simple next-allowed-send
// gate. Wait blocks until the slot opens or the caller's ctx is
// cancelled, then reserves the next slot for the same key. If the
// requested wait would exceed maxWait (the chat is already
// saturated beyond recoverability), Wait returns an error
// immediately so the caller can drop the message and continue.
//
// The limiter is intentionally simple — a single shared mutex
// guards the map of next-send times. Hot-path throughput is
// dominated by the network round-trip (~100ms), not the lock
// acquisition (~10ns), so contention is a non-issue. Replace
// with golang.org/x/time/rate if multi-channel adapters are
// ever introduced.
type threadReplyLimiter struct {
	mu       sync.Mutex
	nextSend map[string]time.Time
	interval time.Duration // 200ms = 5 QPS
	maxWait  time.Duration // hard cap on per-call wait
}

func newThreadReplyLimiter(interval, maxWait time.Duration) *threadReplyLimiter {
	return &threadReplyLimiter{
		nextSend: make(map[string]time.Time),
		interval: interval,
		maxWait:  maxWait,
	}
}

// Wait blocks until the slot for key is open, then reserves the
// next slot. Returns ctx.Err() if ctx is cancelled while waiting,
// or an error if the wait would exceed maxWait (caller should
// drop the message rather than stall).
func (l *threadReplyLimiter) Wait(ctx context.Context, key string) error {
	if l == nil || l.interval <= 0 {
		return nil
	}
	l.mu.Lock()
	due, ok := l.nextSend[key]
	var wait time.Duration
	if !ok || !time.Now().Before(due) {
		l.nextSend[key] = time.Now().Add(l.interval)
		l.mu.Unlock()
		return nil
	}
	wait = time.Until(due)
	l.nextSend[key] = due.Add(l.interval)
	l.mu.Unlock()

	if wait > l.maxWait {
		return fmt.Errorf("feishu: thread reply rate limit exceeded (wait %dms > cap %dms)", wait.Milliseconds(), l.maxWait.Milliseconds())
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// toolName / toolArgs / toolOutput read from OutboundMessage.Tool
// (the typed ToolInfo). The gateway now transports the unified
// tool concept via the Tool field; Meta is no longer the carrier
// for tool data. See gateway/messages.go ToolInfo docs and
// gateway/translate.go for the producer side.
func toolName(m gateway.OutboundMessage) string {
	if m.Tool != nil && m.Tool.Name != "" {
		return m.Tool.Name
	}
	return "tool"
}

func toolArgs(m gateway.OutboundMessage) string {
	if m.Tool != nil {
		return m.Tool.Args
	}
	return ""
}

func toolOutput(m gateway.OutboundMessage) string {
	if m.Tool != nil {
		return m.Tool.Output
	}
	return ""
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
	// Sanitize the permission card body markdown. Permission card titles
	// are plain_text (escaped by SDK) and don't need it, but the body
	// uses lark_md rendering and shares the OutResult / OutThinking
	// surface protections (URL / fence / image / heading).
	body = SanitizeCardMarkdown(body)
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
		// F-34: thinking / tool_start / tool_end / compaction
		// entries are no longer rendered into the receipt card;
		// the adapter routes them to Feishu thread replies
		// (Adapter.Send → postThreadReply). eventToEntry returns
		// (_, false) for them so they normally never reach this
		// loop. This guard is a defensive backstop: if a future
		// bridge or a buggy caller appends one of these Kinds
		// directly to r.entries (e.g. when constructing fixtures
		// in tests), the card body still hides it instead of
		// leaking thinking/tool content into the rolling log.
		switch e.Kind {
		case "thinking", "tool", "tool_start", "tool_end", "compaction":
			continue
		}
		content := e.Icon
		if e.Text != "" {
			if content != "" {
				content += " "
			}
			content += e.Text
		}
		// F-37 thread-route: thinking/tool entries are NOT
		// rendered here at all — they were routed to Feishu
		// thread replies upstream. The old collapsible_panel
		// branches for `Kind == "thinking"` and `Kind == "tool"`
		// (origin/main version) are intentionally absent on
		// this branch: those kinds no longer reach the card
		// surface. Only OutText / OutResult / OutInit / OutUsage
		// entries make it here.
		//
		// F-37 multi-div content split: if the entry's text
		// exceeds divTextCharLimit (Feishu hard limit), split
		// at paragraph boundaries into multiple markdown elements
		// so the full content renders without truncation. See
		// docs/feat/F-37-multi-div-content-split.md.
		chunks := splitMarkdownForDivs(content, divTextCharLimit)
		for _, chunk := range chunks {
			elements = append(elements, map[string]any{
				"tag":     "markdown",
				"content": chunk,
			})
		}
	}
	// F-38: task checklist — one markdown element per chunk
	// rendered below the answer entries and above the footer
	// divider. The bridge always sends the full snapshot, so we
	// copy it verbatim and let buildTaskChecklistChunks handle
	// glyph / ordering / truncation / div splitting. Omitted
	// entirely when no tasks have been reported. Each chunk is
	// already <= divTextCharLimit runes (the splitter's contract),
	// so we may emit multiple elements from a single checklist
	// when the list is long; the 50-element budget still applies
	// across the whole card.
	for _, chunk := range buildTaskChecklistChunks(r.tasks) {
		elements = append(elements, map[string]any{
			"tag":     "markdown",
			"content": chunk,
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

// Health returns a live snapshot of the WebSocket lifecycle state.
// The snapshot is a copy (deep-enough for JSON marshaling), so
// callers may mutate it freely without affecting the adapter's
// internal state. nil-safe (returns a zero snapshot if the adapter
// has no health — only happens in tests that bypass NewAdapter).
//
// Used by:
//   - cmd/nightme/run.go to register with the daemoncontrol "health"
//     RPC so `nightme health` can answer.
//   - the daemoncontrol server's response handler.
func (a *Adapter) Health() WSHealthSnapshot {
	if a.health == nil {
		return WSHealthSnapshot{}
	}
	snap := a.health.Snapshot()
	// F-41: layer the prober snapshot on top. Snapshots are
	// independent of WSHealth so we don't bake prober state into
	// the health struct (avoids cross-coupling when one is reset).
	if a.prober != nil {
		snap.Prober = a.prober.Snapshot()
	}
	return snap
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
	health := a.health
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
		// F-40: surface failed sends on the health event ring so
		// `nightme health` shows the recent degradation timeline.
		if health != nil {
			health.recordError(time.Now(), err.Error())
		}
		logger.LogAttrs(context.Background(), slog.LevelWarn, "feishu: outgoing failed", append(attrs, slog.String("err", err.Error()))...)
		return
	}
	// F-40: stamp outbound liveness on success. Drives the
	// "outbound stuck" detection (last_outbound_at > N seconds ago).
	if health != nil {
		health.recordOutbound(time.Now(), target, kind)
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
	_, err := a.SendMessageText(ctx, chatID, text, "", false)
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
// replyInThread (F-37) is forwarded to the Reply body when rootID != "".
// false = visible in main chat (default for slash commands and
// receipt-cold-start); true = thread-only (used by postThreadReply for
// the agent progress stream). Has no effect on the top-level Create path.
//
// Returns (messageID, error). On error, messageID is "".
func (a *Adapter) SendMessageText(ctx context.Context, chatID, text, rootID string, replyInThread bool) (string, error) {
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
	msgID, err := a.sendContent(ctx, chatID, larkim.MsgTypeText, string(content), rootID, replyInThread)
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
	// F-35: 出口过限速器，预防触发飞书 230001。
	if err := a.limiter.Wait(ctx); err != nil {
		return "", err
	}
	// F-36: transient 错误（timeout/EOF）按指数退避重试。
	// permanent 错误（含 reaction 不存在 / reaction 重复添加）立即返回。
	return WithTransientRetryMsg(ctx, RetryOpts{
		Op:     "add_reaction",
		Cfg:    DefaultRetryConfig,
		Logger: a.logger,
		Attrs: []any{
			"message_id", messageID,
			"reaction_type", reactionType,
		},
	}, func() (string, error) {
		return a.addReactionOnce(ctx, messageID, reactionType)
	})
}

// addReactionOnce is the unwrapped AddReaction body — the F-36 retry
// loop in AddReaction calls this so a transient failure re-executes
// the whole SDK call (including the limiter wait).
func (a *Adapter) addReactionOnce(ctx context.Context, messageID, reactionType string) (string, error) {
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

// DeleteReaction removes a reaction by its ID. Used by the
// OutMessageStateRemoved send path (MessageState.ReactionID).
// Receipts no longer delete reactions — they append lifecycle
// emojis instead — but the public method stays for other adapter
// consumers.
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
// replyInThread (F-37) controls the Feishu body field reply_in_thread
// when the Reply endpoint is selected (rootID != ""). It has no
// effect on the top-level Create path. See postThreadReply for the
// author-side policy that decides which callsites pass true.
//
// v1.3.x (§13.10 fallback): when the Reply endpoint returns a
// terminal error code (230011 recalled / 231003 deleted), we retry
// as a top-level Create so the user still sees a message. The
// pattern mirrors openclaw-lark's runWithMessageUnavailableGuard.
// See docs/channel/feishu.md §13.10 / §15.7.
//
// Returns "" + error on failure. Empty message ID on success is
// possible if the API omits it (defensive — should not happen).
func (a *Adapter) sendContent(ctx context.Context, chatID, msgType, content, rootID string, replyInThread bool) (string, error) {
	a.mu.RLock()
	send := a.sendFunc
	if send == nil && a.larkClient != nil {
		send = a.sendViaLark
	}
	a.mu.RUnlock()
	if send == nil {
		return "", errors.New("feishu: REST client is nil")
	}

	// Layer 1（retry.go）：transient 错误（timeout/EOF/reset）按
	// 指数退避重试；permanent 错误（含 230011/231003 terminal）
	// 立即返回。retry 与 F-35 limiter 正交：limiter 在 SDK call
	// 内部防 230001，本层在 sendContent 外层防 transient。
	msgID, err := WithTransientRetryMsg(ctx, RetryOpts{
		Op:     "send",
		Cfg:    DefaultRetryConfig,
		Logger: a.logger,
		Attrs: []any{
			"chat_id", chatID,
			"msg_type", msgType,
			"root_id", rootID,
			"reply_in_thread", replyInThread,
		},
	}, func() (string, error) {
		return send(ctx, chatID, msgType, content, rootID, replyInThread)
	})

	// §15.2 fallback：Reply target unavailable（230011/231003），
	// 重试一次不带 rootID 的 Create；fallback 也走 retry 救活
	// 瞬时失败（target unavailable 与 transient 无关，但保留对称）。
	// reply_in_thread 在 fallback path 没意义（Create 不接受），
	// 退化时一律清掉。
	if err != nil && rootID != "" && isFeishuTerminalMessageCode(err) {
		logDegradation(a.logger, degradationFallbackTopLevel, RetryOpts{
			Op:     "send",
			Logger: a.logger,
			Attrs: []any{
				"chat_id", chatID,
				"msg_type", msgType,
				"root_id", rootID,
				"reply_in_thread", replyInThread,
				"reason", "reply_target_unavailable",
			},
		}, 0, 0, err, nil)
		return WithTransientRetryMsg(ctx, RetryOpts{
			Op:     "send_top_level",
			Cfg:    DefaultRetryConfig,
			Logger: a.logger,
			Attrs: []any{
				"chat_id", chatID,
				"msg_type", msgType,
				"root_id", "", // fallback intentionally clears root_id
			},
		}, func() (string, error) {
			return send(ctx, chatID, msgType, content, "", false)
		})
	}
	return msgID, err
}

func (a *Adapter) sendViaLark(ctx context.Context, chatID, msgType, content, rootID string, replyInThread bool) (string, error) {
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
		return a.sendViaLarkReply(ctx, rootID, msgType, content, replyInThread)
	}
	return a.sendViaLarkCreate(ctx, chatID, msgType, content)
}

// sendViaLarkReply invokes POST /im/v1/messages/{rootID}/reply
// and returns the new message ID. Used when v1.3.x §13.10 needs
// to thread a bot reply to a specific user message. Returns a
// formatted error carrying the Feishu API code when the call
// fails; callers inspect with isFeishuTerminalMessageCode to
// decide whether to fall back to Create.
//
// replyInThread (F-37) controls the body field reply_in_thread.
// true = thread/topic form only (main chat shows "X replies"
// indicator without body); false (default) = visible in main chat
// as an inline reply AND in thread panel. See postThreadReply /
// F-37 §2.1 for which OutboundKind values pass true.
func (a *Adapter) sendViaLarkReply(ctx context.Context, rootID, msgType, content string, replyInThread bool) (string, error) {
	// F-35: 出口过限速器。
	if err := a.limiter.Wait(ctx); err != nil {
		return "", err
	}
	bodyBuilder := larkim.NewReplyMessageReqBodyBuilder().
		MsgType(msgType).
		Content(content)
	if replyInThread {
		// Field is omitempty — only set when true so the default
		// payload stays byte-for-byte identical to pre-F-37 calls
		// (preserves backward compat with the §13.10 recorder logs
		// and any internal idempotency cache keyed on body hash).
		bodyBuilder = bodyBuilder.ReplyInThread(true)
	}
	req := larkim.NewReplyMessageReqBuilder().
		MessageId(rootID).
		Body(bodyBuilder.Build()).
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
// fails on a recalled/deleted user message. replyInThread is unused
// here (Create API does not accept reply_in_thread).
func (a *Adapter) sendViaLarkCreate(ctx context.Context, chatID, msgType, content string) (string, error) {
	// F-35: 出口过限速器。
	if err := a.limiter.Wait(ctx); err != nil {
		return "", err
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
//
// replyInThread (F-37) is accepted for symmetry with sendContent /
// SendMessageText but is effectively a no-op for the receipt cold-start
// path (the cold-start card MUST stay visible in the main chat —
// chat-pinned visual answer is the whole point of the rolling log). It is
// plumbed through so future callers (e.g. permission card as thread-only)
// can opt in without a second signature change.
func (a *Adapter) SendCard(ctx context.Context, chatID, content, rootID string, replyInThread bool) (string, error) {
	if strings.TrimSpace(chatID) == "" {
		return "", errors.New("feishu: chat_id is required")
	}
	if strings.TrimSpace(content) == "" {
		return "", errors.New("feishu: card content is required")
	}
	msgID, err := a.sendContent(ctx, chatID, larkim.MsgTypeInteractive, content, rootID, replyInThread)
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
	// F-36: transient 错误（timeout/EOF）按指数退避重试。
	// PATCH 5/s per message_id 由 MessageReceipt.renderLocked
	// mutex 天然满足；F-35 limiter 在 retry 内层守住全局
	// 5/s per-user / per-group 桶。
	return WithTransientRetry(ctx, RetryOpts{
		Op:     "patch_message",
		Cfg:    DefaultRetryConfig,
		Logger: a.logger,
		Attrs: []any{
			"message_id", messageID,
		},
	}, func() error {
		return a.patchMessageOnce(ctx, messageID, content)
	})
}

// patchMessageOnce is the unwrapped PATCH body — F-36 retry loop
// in updateViaLark calls this so a transient failure re-executes the
// whole SDK call (including the F-35 limiter wait).
func (a *Adapter) patchMessageOnce(ctx context.Context, messageID, content string) error {
	// F-35: 出口过限速器。
	if err := a.limiter.Wait(ctx); err != nil {
		return err
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
	msgType := stringValue(message.MessageType)
	text, attachments, blocks := extractAttachments(msgType, content)
	// F-14 visibility: trace the inbound attachment shape BEFORE any
	// download is attempted. With logging.level=debug this surfaces
	// (a) what extractAttachments decoded (msg_type, file_keys),
	// (b) whether LocalPath is empty (would mean DownloadAttachments
	// has not run). Pairs with the dispatcher-level "blocks built"
	// trace to pinpoint whether an image was lost at the channel
	// boundary or further downstream.
	a.mu.RLock()
	logger := a.logger
	a.mu.RUnlock()
	if logger != nil && len(attachments) > 0 {
		types := make([]string, 0, len(attachments))
		keys := make([]string, 0, len(attachments))
		for _, att := range attachments {
			types = append(types, att.Type)
			keys = append(keys, att.FileKey)
		}
		logger.Debug("feishu: attachments extracted",
			"msg_type", msgType,
			"count", len(attachments),
			"types", types,
			"file_keys", keys,
		)
	}
	// F-watch §6.10: strip leading @bot/@_all mention prefix from
	// text so slash commands like `/watch on` parse correctly
	// (ParseCommand requires HasPrefix "/"). computeHasMention
	// returns the original-message semantic ("did this message
	// address the bot") which Gateway dispatcher combines with
	// ChatSession.WatchMode() to drop non-mention group messages.
	botOpenID := a.fetchBotOpenID(ctx)
	hasMention := computeHasMention(message, botOpenID)
	text = stripMentionPrefix(text, message.Mentions, botOpenID)
	messageID := stringValue(message.MessageId)

	// F-14 v1.4a: synchronously download attachments before publish
	// so LocalPath is populated when downstream code reads it. The
	// inbox dir is keyed by chatID (per-chat isolation; stable
	// across AgentSession re-spawns within the same chat). AllFailed
	// aborts the message entirely with a user-facing notification;
	// partial failures emit a warning but continue with the rest.
	if len(attachments) > 0 {
		result := a.DownloadAttachments(ctx, messageID, attachments, chatID)
		if result.AllFailed {
			if logger != nil {
				logger.Info("feishu: all attachments failed to download; dropping message",
					"message_id", messageID,
					"failed_count", len(result.FailureKeys),
					"failure_keys", result.FailureKeys,
				)
			}
			_ = a.SendMessage(ctx, chatID,
				fmt.Sprintf("❌ %d attachment(s) failed to download, please retry",
					len(result.FailureKeys)))
			return nil
		}
		if len(result.FailureKeys) > 0 {
			if logger != nil {
				logger.Info("feishu: partial attachment download failure; sending the rest",
					"message_id", messageID,
					"failed_count", len(result.FailureKeys),
					"failed_keys", result.FailureKeys,
					"succeeded_count", len(result.Atts)-len(result.FailureKeys),
				)
			}
			_ = a.SendMessage(ctx, chatID,
				fmt.Sprintf("⚠️ %d of %d attachment(s) failed to download; sending the rest",
					len(result.FailureKeys), len(attachments)))
		}
		attachments = result.Atts
		// F-14 v1.4b: back-fill LocalPath into post rich-text image
		// blocks whose Path currently holds a FileKey placeholder.
		if blocks != nil {
			blocks = resolveBlocks(blocks, attachments)
		}
	}

	msg := channel.Message{
		ChatID:    chatID,
		Text:      text,
		UserID:    senderID(event),
		Time:      messageTime(message.CreateTime),
		MessageID: messageID,
		// F-33 §13.11 / D3: ReplyTo = Feishu message.ParentId. The
		// thread-top-level RootId is intentionally not surfaced to
		// nightme — we only track point-to-point reply relationships.
		// Empty for top-level messages (ParentId == "") and for
		// topic-group thread-root messages where the user started a
		// new line in an existing thread.
		ReplyTo: stringValue(message.ParentId),
		// F-14 v1.4a: Attachments[i].LocalPath is now populated
		// before publish (DownloadAttachments runs synchronously above).
		Attachments: attachments,
		// F-14 v1.4b: ordered rich-text blocks for post messages;
		// nil for single-resource msg_types (legacy BuildBlocks path).
		Blocks: blocks,
		// F-watch: see docs/channel/feishu.md §6.10 + SPEC §3.1.1.
		HasMention: hasMention,
	}
	// Trace every inbound message before the publish lock so the
	// CLI surface shows the user message that triggered the
	// handler even if the channel send is cancelled or timed out.
	// Companion to logOutgoing (the bot side of the conversation).
	a.logInbound(msg)
	// F-40: stamp the inbound liveness signal for the
	// `nightme health` command. Drives the "inbound stuck"
	// detection (last_inbound_at > N seconds ago).
	if a.health != nil {
		a.health.recordInbound(time.Now(), chatID, msgType)
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

// normalizeChatType was removed in F-33 (D1+D2). Feishu's native
// chat_type values (p2p / group / topic_group) no longer flow into
// the nightme data model: ChatSession, BindingEntry, and the
// registry schema no longer carry a ChatType field. topic_group
// (Feishu threads) is intentionally treated identically to plain
// groups — both share the same chat_id space (oc_xxx) and the
// thread is a Feishu-side rendering detail handled by the adapter
// via the Reply API path parameter. See docs/SPEC.md §3.1 and
// docs/channel/feishu.md §13.11.

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
