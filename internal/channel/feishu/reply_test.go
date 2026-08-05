// Live-API smoke tests for the three reply methods on *Adapter.
//
// Usage:FEISHU_TEST_DM_CHAT_ID=oc_xxx go test -run TestReply_ -v ./internal/channel/feishu/
//
// Config reading reuses internal/config.LoadDefault() (equivalent to
// ~/.config/nightme/config.yaml). FEISHU_TEST_DM_CHAT_ID is required;
// without it, every live test is SKIPped.
//
// Each test creates a fresh top-level parent + invokes the corresponding
// reply method, then prints the message_id so the dev/user can confirm
// the visual rendering in the Feishu DM.
package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	"github.com/cnlangzi/nightme/internal/config"
)

// mustJSON serializes a value to a JSON string; panics on failure.
// Test-only helper kept in _test.go so the production code in reply.go
// does not depend on test-side concerns. A marshal failure here is a
// caller bug (illegal value), not a runtime concern, so panicking is
// the right behavior.
func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Errorf("feishu: mustJSON: %w", err))
	}
	return string(b)
}

// PostElement / PostBlock / PostPayload — test-only typed data shapes
// for the post bonus path. Kept in _test.go because production (adapter.go)
// still uses string-based content; these are only used by the
// ApplyPost smoke check below.
type PostElement struct {
	Tag      string `json:"tag"` // text | md | at | img | link | emotion | hr
	Text     string `json:"text,omitempty"`
	Href     string `json:"href,omitempty"`
	UserID   string `json:"user_id,omitempty"` // for @mention (open_id)
	UserName string `json:"user_name,omitempty"`
	ImageKey string `json:"image_key,omitempty"` // for inline img inside post
	FileName string `json:"file_name,omitempty"`
}

type PostBlock struct {
	Title   string          `json:"title,omitempty"`
	Content [][]PostElement `json:"content"`
}

type PostPayload struct {
	ZhCn *PostBlock `json:"zh_cn,omitempty"`
	EnUs *PostBlock `json:"en_us,omitempty"`
}

// testDMChatID reads FEISHU_TEST_DM_CHAT_ID; SKIPs the test when unset.
// Example:FEISHU_TEST_DM_CHAT_ID=oc_xxxxxxx go test -run TestReply_ ./internal/channel/feishu/
func testDMChatID(t *testing.T) string {
	t.Helper()
	id := os.Getenv("FEISHU_TEST_DM_CHAT_ID")
	if id == "" {
		t.Skip("FEISHU_TEST_DM_CHAT_ID not set; live API tests require a real Feishu DM chat_id")
	}
	return id
}

// newTestAdapter — reuses nightme's own config loader, SKIPs when no
// credentials are available. Credential resolution order matches
// openclaw-lark/reply_in_both.js:
//  1. LARK_APP_ID / LARK_APP_SECRET environment variables
//  2. config.LoadDefault() (equivalent to ~/.config/nightme/config.yaml;
//     it has already merged env overrides, so the two are not in conflict)
func newTestAdapter(t *testing.T) *Adapter {
	t.Helper()
	cfg, err := config.LoadDefault()
	if err != nil {
		t.Skipf("load nightme config: %v (set LARK_APP_ID / LARK_APP_SECRET, or put feishu.app_id/secret in ~/.config/nightme/config.yaml)", err)
	}
	if cfg.Feishu.AppID == "" || cfg.Feishu.AppSecret == "" {
		t.Skip("feishu.app_id / app_secret missing in nightme config")
	}
	return &Adapter{
		larkClient: lark.NewClient(cfg.Feishu.AppID, cfg.Feishu.AppSecret),
		limiter:    NewLimiter(nil, nil),
	}
}

// liveCreateMessage — creates a fresh top-level parent, equivalent to
// the no-rootID branch in adapter.sendViaLark (which now routes
// through a.ReplyInChat). Returns the message_id to use as the
// reply test's parent.
func liveCreateMessage(t *testing.T, a *Adapter, chatID, content string) string {
	t.Helper()
	msgType := "post"
	body := &larkim.CreateMessageReqBody{
		ReceiveId: &chatID,
		MsgType:   &msgType,
		Content:   &content,
	}
	req := larkim.NewCreateMessageReqBuilder().
		ReceiveIdType(larkim.CreateMessageV1ReceiveIDTypeChatId).
		Body(body).
		Build()
	resp, err := a.larkClient.Im.V1.Message.Create(context.Background(), req)
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}
	if resp == nil || !resp.Success() || resp.Data == nil || resp.Data.MessageId == nil {
		t.Fatalf("create parent failed: %+v", resp)
	}
	return *resp.Data.MessageId
}

func postContent(label string) string {
	return `{"zh_cn":{"content":[[{"tag":"md","text":"PARENT-LIVE · ` + label + `"}]]}}`
}

// TestReplyInBoth_Live — F-37 ReplyInThreadAndChat:
// main chat shows body inline with a "Reply to ..." header; parent gets
// a "1 reply" badge; the right-side Details panel contains the same body.
// Pre-condition: parent must be fresh (never previously threaded by an
// in:true reply).
func TestReplyInBoth_Live(t *testing.T) {
	a := newTestAdapter(t)
	ctx := context.Background()
	chatID := testDMChatID(t)

	parentID := liveCreateMessage(t, a, chatID, postContent("ReplyInBoth"))
	t.Logf("[1] fresh parent: %s", parentID)

	body := larkim.NewReplyMessageReqBodyBuilder()
	body.MsgType("text").Content(mustJSON(struct {
		Text string `json:"text"`
	}{Text: "REPLY-LIVE · ReplyInBoth — verify F-37 rendering: main-chat inline + Reply to header + parent 1 reply badge."}))
	replyID, err := a.ReplyInBoth(ctx, parentID, body)
	if err != nil {
		t.Fatalf("ReplyInBoth: %v", err)
	}
	if replyID == "" {
		t.Fatal("ReplyInBoth returned empty message_id")
	}
	t.Logf("[2] reply (text via SDK builder + mustJSON): %s", replyID)
	t.Logf("    → Feishu DM visual check:")
	t.Logf("      (a) parent '%s' has reply below it, with 'Reply to LzBook: ...' header on top", parentID)
	t.Logf("      (b) parent shows '1 reply' badge")
	t.Logf("      (c) right-side Details panel has the full body")

	// Bonus: also exercise post payload path (struct literal — no helper)
	parentID2 := liveCreateMessage(t, a, chatID, postContent("ReplyInBoth/post"))
	body2 := larkim.NewReplyMessageReqBodyBuilder()
	body2.MsgType("post").Content(mustJSON(&PostPayload{
		EnUs: &PostBlock{Content: [][]PostElement{
			{{Tag: "md", Text: "REPLY-LIVE · ReplyInBoth + post — verify post-type build."}},
		}},
	}))
	replyID2, err := a.ReplyInBoth(ctx, parentID2, body2)
	if err != nil {
		t.Fatalf("ReplyInBoth + post: %v", err)
	}
	t.Logf("[3] reply (post): parent=%s, reply=%s", parentID2, replyID2)
}

// TestReplyInThread_Live — F-37 ReplyInThread:
// main chat is empty (parent has only the "1 replies" gray indicator);
// body lives only in the thread panel.
func TestReplyInThread_Live(t *testing.T) {
	a := newTestAdapter(t)
	ctx := context.Background()
	chatID := testDMChatID(t)

	parentID := liveCreateMessage(t, a, chatID, postContent("ReplyInThread"))
	t.Logf("[1] fresh parent: %s", parentID)

	body := larkim.NewReplyMessageReqBodyBuilder()
	body.MsgType("text").Content(mustJSON(struct {
		Text string `json:"text"`
	}{Text: "REPLY-LIVE · ReplyInThread — verify in-thread rendering"}))
	replyID, err := a.ReplyInThread(ctx, parentID, body) // reply.go forces reply_in_thread=true internally
	if err != nil {
		t.Fatalf("ReplyInThread: %v", err)
	}
	if replyID == "" {
		t.Fatal("ReplyInThread returned empty message_id")
	}
	t.Logf("[2] reply (text via SDK builder, reply.go forces reply_in_thread): %s", replyID)
	t.Logf("    → Feishu DM visual check:")
	t.Logf("      (a) parent '%s' has NO inline reply below it", parentID)
	t.Logf("      (b) parent shows '1 replies' gray indicator (no body entry)")
	t.Logf("      (c) click parent → right-side Details panel has the full body")
}

// TestReplyInChat_Live — F-37 ReplyInChat (top-level Create):
// standalone top-level bubble, no parent/thread relationship, main chat
// shows it directly with no "Reply to" header.
func TestReplyInChat_Live(t *testing.T) {
	a := newTestAdapter(t)
	ctx := context.Background()
	chatID := testDMChatID(t)

	body := larkim.NewCreateMessageReqBodyBuilder()
	body.MsgType("text").Content(mustJSON(struct {
		Text string `json:"text"`
	}{Text: "REPLY-LIVE · ReplyInChat — top-level Create, standalone bubble, no parent/thread association."}))
	replyID, err := a.ReplyInChat(ctx, chatID, body)
	if err != nil {
		t.Fatalf("ReplyInChat: %v", err)
	}
	if replyID == "" {
		t.Fatal("ReplyInChat returned empty message_id")
	}
	t.Logf("[1] created standalone: %s", replyID)
	t.Logf("    → Feishu DM visual check:")
	t.Logf("      (a) main chat shows standalone bubble, no 'Reply to' header")
	t.Logf("      (b) no thread badge / entry")
	t.Logf("      (c) not a reply to any other message")
}

// (postTextContent helper was removed — no longer needed.)
