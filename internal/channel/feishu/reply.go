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
//   This method does NOT clear reply_in_thread if the caller pre-set it.
//   If you wrote body.ReplyInThread(true) earlier and then pass the
//   same builder here, the call lands at the API as reply_in_thread=true
//   and the server pulls it into the thread panel (the opposite of
//   ReplyInBoth's rendering). Build a fresh builder per call.
//
// Caller invariant (F-40 / openclaw-lark/reply_in_both.js L111):
//
//   The parent message must NOT already be in a thread. If the parent
//   already carries a thread_id (from a prior reply_in_thread=true call),
//   Feishu server pulls this reply INTO the existing thread panel and the
//   main-chat inline rendering is lost. Callers must either guarantee
//   fresh-parent state, or route such inputs through ReplyInThread instead.
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
//   This method calls body.ReplyInThread(true) on the caller-supplied
//   builder. If the caller reuses the same builder for a subsequent
//   reply, the reply_in_thread=true state carries over. Build a fresh
//   builder per call if you need independent state.
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
