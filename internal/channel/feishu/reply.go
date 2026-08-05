// Reply helpers — Feishu outbound reply (3 F-37 / F-40 types).
//
// References:
//   - docs/channel/feishu.md §13.10 (F-40 / F-37 three-type decision table)
//   - openclaw-lark/reply_in_both.js (2026-08-05 visual confirmation)
//   - openclaw-lark/src/messaging/outbound/deliver.ts (sendImMessage branch)
//
// Server-side behavior (per reply_in_both.js L6-L21):
//
//   ReplyInThread (reply_in_thread: true)
//     - Main chat stream empty; only gray "N replies" indicator on parent
//     - Full body in Details / Thread side panel
//
//   ReplyInBoth (field omitted / omitempty nil bool)
//     - Main chat shows body inline with "Reply to <sender>: ..." quote header
//     - Parent gets "1 reply" badge; Details panel shows same body
//     - **Only holds while parent has no thread**: once any reply_in_thread=true
//       lands on the parent, server pulls subsequent replies into the existing
//       thread and the inline main-chat render is lost.
//
//   ReplyInChat (top-level Create, no reply API call)
//     - Standalone top-level bubble, no reply relationship.
//
// This file contains ONLY the three reply methods + the package doc.
// All builder-population / JSON-encoding helpers (mustJSON, PostElement,
// PostBlock, PostPayload) live in reply_test.go because they are
// test-only. Callers use the SDK builders directly:
//
//	b := larkim.NewReplyMessageReqBodyBuilder()
//	b.MsgType("text").Content(`{"text":"hello"}`)
//	a.ReplyInBoth(ctx, parentID, b)
//
//	b := larkim.NewReplyMessageReqBodyBuilder()
//	b.MsgType("text").Content(`{"text":"hi"}`)
//	a.ReplyInThread(ctx, parentID, b) // reply.go forces reply_in_thread=true internally
//
//	b := larkim.NewCreateMessageReqBodyBuilder().ReceiveId(chatID)
//	b.MsgType("text").Content(`{"text":"hi"}`)
//	a.ReplyInChat(ctx, chatID, b)
//
// # ⚠️ IMPORTANT — parent-promotion is PERMANENT and SERVER-SIDE (confirmed 2026-08-05)
//
// Live-tested against a real Feishu tenant (both p2p DM and a "group"/
// group_message_type=chat group — i.e. a NON-topic chat) via
// openclaw-lark/reply_in_both.js. Full transcript / raw API responses:
// see the "openclaw-lark 回复效果" chat session, 2026-08-05.
//
//  1. A parent message starts with NO reply-tree state at all
//     (no parent_id/root_id/thread_id of its own).
//
//  2. The FIRST time ANY reply to that parent uses reply_in_thread=true
//     (i.e. goes through ReplyInThread), Feishu's server does not just
//     create a thread for the *new* reply — it retroactively PATCHes the
//     PARENT message's own record: re-fetching the parent afterwards
//     shows `updated: true`, a bumped `update_time`, and a newly attached
//     `thread_id`. This was directly observed via `im.message.get` on the
//     parent, before and after sending a ReplyInThread reply to it.
//
//  3. This promotion is a ONE-WAY, PERMANENT state transition for the
//     lifetime of that parent message. After it happens:
//       - Every SUBSEQUENT reply to that parent — even one sent through
//         ReplyInBoth (reply_in_thread omitted) — gets pulled into the
//         existing thread by the server (its response carries the SAME
//         thread_id) and renders collapsed into the "N replies" summary
//         card instead of as its own inline "Reply to ...: <body>" bubble.
//       - The official Feishu client reflects this: right-click "Reply"
//         (the ReplyInBoth affordance) disappears from the context menu
//         for that parent; only "Reply In Thread" remains.
//     There is no way to "un-promote" a parent via the public Open API.
//
//  4. Messages that were already sent via ReplyInBoth BEFORE the parent
//     was promoted are NOT retroactively affected — their own message
//     records never gain a thread_id and they keep rendering as
//     independent inline bubbles. This was verified by re-fetching a
//     pre-promotion ReplyInBoth message via im.message.get after a later
//     ReplyInThread call promoted the same parent: no thread_id appeared,
//     updated stayed false, message_position stayed a normal main-chat
//     sequence number.
//
// ## The safe pattern: reserve early, then PATCH — never re-Reply late
//
// Consequence of (3)+(4): **ORDER MATTERS.** Any ReplyInBoth call that
// needs to be guaranteed inline-in-main-chat MUST either:
//
//   - happen before any ReplyInThread call targeting the same parent
//     in that turn, OR
//   - be delivered by PATCHing (Adapter.PatchMessage /
//     im.message.patch) a message_id that was already reserved via
//     ReplyInBoth BEFORE any promotion happened — PATCH edits an
//     existing message's body in place and does NOT re-enter the
//     reply/thread routing logic, so it is immune to a promotion that
//     happens after the reservation.
//
// PatchMessage only supports msg_type=interactive (card) bodies — a
// PATCH against a text/post message fails with 230001 "This message is
// NOT a card". So any placeholder you intend to PATCH later must be
// sent as a card via ReplyInBoth from the start.
//
// KNOWN GAP (not fixed by this file): as of this writing,
// Adapter.sendResultAsReply (F-39, the independent "final answer"
// reply) always calls sendContent(..., userMsgID, false) — i.e. issues
// a brand-new Reply every time — instead of PatchMessage-ing an
// early-reserved placeholder. If OutThinking/OutToolStart
// (ReplyInThread, F-37 tool-thread-routing) promoted userMsgID earlier
// in the same turn, sendResultAsReply's "new reply" gets silently
// absorbed into that thread, defeating F-39's "final answer must stay
// visible in main chat" goal. The receipt card (renderLocked in
// receipt.go) already does this correctly — first render posts via
// Reply, every subsequent render goes through PatchMessage against the
// same cardMsgID — sendResultAsReply should eventually follow the same
// shape (reuse/PATCH a reserved card) rather than re-Replying.
//
// ## Native-client-only capability: NOT reproducible via Open API
//
// The Feishu client offers a "同时发送到群/会话" ("Also send to the
// chat") checkbox when replying from inside the Thread side panel. It
// produces what LOOKS like a single message that is both fully inline
// in main chat AND part of the thread. Verified via im.message.list on
// both the chat and the thread container: this is actually TWO
// separate messages with two different message_ids (one plain,
// thread-less, top-level-shaped message in the main chat; one proper
// ReplyInThread message inside the thread), sent with identical
// (millisecond-equal) create_time. No shared/back-reference field
// (e.g. upper_message_id) is exposed for either message via
// im.message.get or im.message.list — whatever links them for
// rendering purposes is Feishu-internal and not part of the public
// Open API message schema. Bots cannot call this checkbox's behavior;
// the closest approximation is sending both halves yourself (one
// ReplyInThread + one ReplyInChat with a hand-rendered quote prefix),
// which loses the native click-to-jump quote header.

package feishu

import (
	"context"
	"errors"
	"fmt"

	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

// ReplyInBoth sends a reply to parentMsgID with the "ReplyInBoth" rendering:
//   - Body appears INLINE in main chat with "Reply to <sender>: ..." quote
//   - Parent gets "1 reply" badge and opens the same body in Details /
//     Thread side panel
//
// Implementation: a single im.message.reply call. The caller passes the
// SDK builder (msg_type + content already set). Without the caller having
// set reply_in_thread=true on the body, the field is omitempty-omitted
// from the request — that is the "ReplyInBoth" semantic.
//
// Caller contract — DO NOT pre-set reply_in_thread on the body:
//
//	This method does NOT clear reply_in_thread if the caller pre-set it.
//	If you wrote body.ReplyInThread(true) earlier and then pass the
//	same builder here, the call lands at the API as reply_in_thread=true
//	and the server pulls it into the thread panel (the opposite of
//	ReplyInBoth's rendering). Build a fresh builder per call.
//
// Caller invariant (F-40 / openclaw-lark/reply_in_both.js L111):
//
//	The parent message must NOT already be in a thread. If the parent
//	already carries a thread_id (from a prior reply_in_thread=true call),
//	Feishu server pulls this reply INTO the existing thread panel and the
//	main-chat inline rendering is lost. Callers must either guarantee
//	fresh-parent state, or route such inputs through ReplyInThread instead.
//
// ⚠️ Confirmed 2026-08-05 (see package doc "parent-promotion is PERMANENT"):
// promotion is a one-way state change on parentMsgID itself, so "fresh
// parent state" cannot be re-checked defensively at call time — it must be
// guaranteed by CALL ORDER (this method before any ReplyInThread call on
// the same parent, in the same turn). If you need this reply's body to
// survive a LATER promotion of parentMsgID (e.g. a concurrent
// OutThinking/OutToolStart on the same turn), do not call ReplyInBoth
// again after the fact — instead call it once, early, with a card body
// (msg_type=interactive), keep the returned message_id, and deliver later
// updates via Adapter.PatchMessage(ctx, messageID, cardJSON) instead of a
// second ReplyInBoth/Reply call. PatchMessage edits the message in place
// and does not re-enter reply/thread routing, so it is immune to a
// promotion that happens after the reservation (verified: re-fetching a
// pre-promotion ReplyInBoth message after a later ReplyInThread call on
// the same parent showed no thread_id and unaffected message_position).
//
// Returns:
//   - new message_id, "" + nil on no-op paths
//   - SDK error if the underlying call fails
func (a *Adapter) ReplyInBoth(ctx context.Context, parentMsgID string, body *larkim.ReplyMessageReqBodyBuilder) (string, error) {
	if a == nil || a.larkClient == nil {
		return "", errors.New("feishu: ReplyInBoth: adapter or larkClient not initialized")
	}
	if parentMsgID == "" {
		return "", errors.New("feishu: ReplyInBoth: parentMsgID is empty")
	}
	if body == nil {
		return "", errors.New("feishu: ReplyInBoth: body is nil")
	}
	// F-35: outbound rate limiter (5 QPS per bot).
	if err := a.limiter.Wait(ctx); err != nil {
		return "", fmt.Errorf("feishu: ReplyInBoth rate limit: %w", err)
	}
	req := larkim.NewReplyMessageReqBuilder().
		MessageId(parentMsgID).
		Body(body.Build()).
		Build()
	resp, err := a.larkClient.Im.V1.Message.Reply(ctx, req)
	if err != nil {
		return "", fmt.Errorf("feishu: ReplyInBoth reply: %w", err)
	}
	if resp == nil {
		return "", errors.New("feishu: ReplyInBoth: nil response")
	}
	if !resp.Success() {
		return "", fmt.Errorf("feishu: ReplyInBoth failed: code=%d msg=%s", resp.Code, resp.Msg)
	}
	if resp.Data != nil && resp.Data.MessageId != nil {
		return *resp.Data.MessageId, nil
	}
	return "", nil
}

// ReplyInThread sends a reply to parentMsgID visible ONLY in the Thread
// side panel. Main chat shows only the gray "N replies" entry on the
// parent, with no inline body.
//
// Implementation: im.message.reply call with reply_in_thread=true.
// ReplyInThread MUTATES the caller's body via body.ReplyInThread(true)
// to enforce the field. Server assigns (or reuses) a thread_id; the
// main-chat stream listing does NOT include the new message_id.
//
// Caller contract — body is mutated:
//
//	This method calls body.ReplyInThread(true) on the caller-supplied
//	builder. If the caller reuses the same builder for a subsequent
//	reply, the reply_in_thread=true state carries over. Build a fresh
//	builder per call if you need independent state.
//
// Caller semantics:
//   - If parent already carries a thread_id, the new reply joins that thread
//     (thread_id inherited).
//   - If parent has no thread_id yet, the server creates a new omt_xxx
//     and backfills it onto the parent.
//   - Edge case: path.message_id is a reply-to-reply (not top-level) AND
//     the immediate parent has thread_id=null. Server treats the parent
//     as a new thread root and produces an orphan thread. This is
//     client-side problematic; callers should avoid it.
//
// ⚠️ PERMANENT SIDE EFFECT on parentMsgID (confirmed 2026-08-05, see package
// doc): "backfills it onto the parent" above is not just metadata — the
// parent message's OWN record gets server-side PATCHed (re-fetching it
// shows updated=true, a bumped update_time, and the new thread_id). This
// is irreversible for the lifetime of parentMsgID: from this call onward,
// EVERY future reply to parentMsgID — via this method OR via ReplyInBoth,
// called from anywhere in the codebase, by this bot or a human in the
// native client — is absorbed into this thread. Any caller that still
// wants a ReplyInBoth-style inline reply on parentMsgID after calling this
// must have reserved it BEFORE this call (see ReplyInBoth's doc for the
// reserve-then-PatchMessage pattern); it cannot be obtained afterwards by
// any combination of request parameters.
//
// Returns:
//   - new message_id, "" + nil on no-op paths
//   - SDK error if the underlying call fails
func (a *Adapter) ReplyInThread(ctx context.Context, parentMsgID string, body *larkim.ReplyMessageReqBodyBuilder) (string, error) {
	if a == nil || a.larkClient == nil {
		return "", errors.New("feishu: ReplyInThread: adapter or larkClient not initialized")
	}
	if parentMsgID == "" {
		return "", errors.New("feishu: ReplyInThread: parentMsgID is empty")
	}
	if body == nil {
		return "", errors.New("feishu: ReplyInThread: body is nil")
	}
	if err := a.limiter.Wait(ctx); err != nil {
		return "", fmt.Errorf("feishu: ReplyInThread rate limit: %w", err)
	}
	// Force reply_in_thread=true directly on the SDK builder. Idempotent
	// even if the caller already set it on the body.
	body.ReplyInThread(true)
	built := body.Build()
	req := larkim.NewReplyMessageReqBuilder().
		MessageId(parentMsgID).
		Body(built).
		Build()
	resp, err := a.larkClient.Im.V1.Message.Reply(ctx, req)
	if err != nil {
		return "", fmt.Errorf("feishu: ReplyInThread reply: %w", err)
	}
	if resp == nil {
		return "", errors.New("feishu: ReplyInThread: nil response")
	}
	if !resp.Success() {
		return "", fmt.Errorf("feishu: ReplyInThread failed: code=%d msg=%s", resp.Code, resp.Msg)
	}
	if resp.Data != nil && resp.Data.MessageId != nil {
		return *resp.Data.MessageId, nil
	}
	return "", nil
}

// ReplyInChat sends a brand-new top-level message to chatID without any
// parent/thread relationship. Appears as a standalone bubble in main
// chat. The Thread side panel does not show it; the parent has no badge.
//
// Implementation: im.message.create with receive_id=chatID.
//
// Caller semantics:
//   - Default output when the bot should respond WITHOUT any reply
//     relationship to the user input (e.g. ephemeral status, scheduled
//     output).
//   - Per nightme F-37 §13.10, nightme itself does NOT route through
//     this path; it is reserved as the fallback (230011/231003 rootID
//     invalid) only.
//
// Returns:
//   - new message_id, "" + nil on no-op paths
//   - SDK error if the underlying call fails
func (a *Adapter) ReplyInChat(ctx context.Context, chatID string, body *larkim.CreateMessageReqBodyBuilder) (string, error) {
	if a == nil || a.larkClient == nil {
		return "", errors.New("feishu: ReplyInChat: adapter or larkClient not initialized")
	}
	if chatID == "" {
		return "", errors.New("feishu: ReplyInChat: chatID is empty")
	}
	if body == nil {
		return "", errors.New("feishu: ReplyInChat: body is nil")
	}
	if err := a.limiter.Wait(ctx); err != nil {
		return "", fmt.Errorf("feishu: ReplyInChat rate limit: %w", err)
	}
	// chatID was set when the caller built the body with
	// NewCreateMessageReqBody(chatID), but they may have overridden it.
	// Force the method-arg chatID here to defend against wrong-wiring.
	built := body.ReceiveId(chatID).Build()
	req := larkim.NewCreateMessageReqBuilder().
		ReceiveIdType(larkim.CreateMessageV1ReceiveIDTypeChatId).
		Body(built).
		Build()
	resp, err := a.larkClient.Im.V1.Message.Create(ctx, req)
	if err != nil {
		return "", fmt.Errorf("feishu: ReplyInChat create: %w", err)
	}
	if resp == nil {
		return "", errors.New("feishu: ReplyInChat: nil response")
	}
	if !resp.Success() {
		return "", fmt.Errorf("feishu: ReplyInChat failed: code=%d msg=%s", resp.Code, resp.Msg)
	}
	if resp.Data != nil && resp.Data.MessageId != nil {
		return *resp.Data.MessageId, nil
	}
	return "", nil
}
