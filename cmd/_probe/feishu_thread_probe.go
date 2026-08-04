// Command feishu_thread_probe is a one-shot investigation tool.
//
// It does NOT call real Feishu. It stands up a local HTTP mock that
// pretends to be lark /im/v1/messages, points the official
// larksuite/oapi-sdk-go client at it via WithOpenBaseUrl, then
// fires 8 representative combinations and dumps:
//
//   1. the JSON request bytes the SDK actually sent (path + body)
//   2. the response IDs the SDK would parse (root_id / parent_id /
//      thread_id / message_id)
//
// Run with:
//
//	go run ./cmd/_probe
//
// Why this exists: F-37 picks reply_in_thread=true|false path by
// OutboundKind. Before locking that in, we want to characterise
// what the SDK payload looks like for each combination — and what
// the response IDs the SDK parses back. Real Feishu rendering
// (the "X replies" indicator UI) cannot be observed from this
// probe; it requires ops to run against a real chat and screenshot
// the result. This probe answers the *deterministic* half
// (request shape + ID relationship) and explicitly tags the part
// that needs real-server validation.
//
// This file is intentionally a single-file `package main` so it
// can be deleted once the F-37 decision is locked in.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/larksuite/oapi-sdk-go/v3"
	"github.com/larksuite/oapi-sdk-go/v3/core"
	"github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

func main() {
	// ------------------------------------------------------------------
	// 1) Set up mock Feishu server.
	//
	// Routes we care about:
	//   POST /open-apis/auth/v3/tenant_access_token/internal
	//   POST /open-apis/im/v1/messages                (Create)
	//   POST /open-apis/im/v1/messages/{id}/reply     (Reply)
	//
	// The mock records the request body and returns a canned response
	// that includes root_id / parent_id / thread_id so we can see
	// what the SDK parses back.
	// ------------------------------------------------------------------

	var (
		mu        sync.Mutex
		trace     []mockCall
		callCount int
	)

	mux := http.NewServeMux()

	mux.HandleFunc("/open-apis/auth/v3/tenant_access_token/internal", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		callCount++
		trace = append(trace, mockCall{
			Path: r.URL.Path, Method: r.Method,
			RequestBody:  "<auth>",
			ResponseBody: `{"code":0,"msg":"ok","tenant_access_token":"t-mock","expire":7200}`,
		})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"code":0,"msg":"ok","tenant_access_token":"t-mock","expire":7200}`)
	})

	mux.HandleFunc("/open-apis/im/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ReceiveID string `json:"receive_id"`
			MsgType   string `json:"msg_type"`
			Content   string `json:"content"`
			RootID    string `json:"root_id"`
			UUID      string `json:"uuid"`
		}
		_ = json.Unmarshal(body, &req)

		// Synthesise IDs. We pretend the receive_id is a chat_id
		// (real Feishu behaviour).  Root_id is whatever the caller
		// set, or self if omitted.
		msgID := fmt.Sprintf("om_create_%d", callCount+1)
		rootID := req.RootID
		if rootID == "" {
			rootID = msgID // self-root for top-level Create
		}

		resp := map[string]any{
			"code": 0,
			"msg":  "ok",
			"data": map[string]any{
				"message_id": msgID,
				"root_id":    rootID,
				// top-level Create has no parent; mock returns ""
				"parent_id": "",
				"thread_id": fmt.Sprintf("t_thread_create_%d", callCount+1),
				"msg_type":  req.MsgType,
				"create_time": fmt.Sprintf("%d", time.Now().UnixMilli()),
				"update_time": fmt.Sprintf("%d", time.Now().UnixMilli()),
				"deleted":   false,
			},
		}

		mu.Lock()
		callCount++
		trace = append(trace, mockCall{
			Path: r.URL.Path, Method: r.Method,
			RequestBody:  string(body),
			ResponseBody: asJSON(resp),
		})
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})

	mux.HandleFunc("/open-apis/im/v1/messages/", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		// Extract parent_id from URL: .../messages/{id}/reply
		//                                ^      ^
		segs := strings.Split(strings.TrimPrefix(r.URL.Path, "/open-apis/im/v1/messages/"), "/")
		if len(segs) < 2 || segs[1] != "reply" {
			http.NotFound(w, r)
			return
		}
		parentID := segs[0]

		var req struct {
			MsgType       string `json:"msg_type"`
			Content       string `json:"content"`
			ReplyInThread *bool  `json:"reply_in_thread,omitempty"`
			UUID          string `json:"uuid"`
		}
		_ = json.Unmarshal(body, &req)

		// Mock Feishu behaviour for root_id when replying:
		//   - if parent itself is a root (no further ancestor),
		//     root_id = parent_id
		//   - if parent has its own parent (we'd need a registry
		//     to track — out of scope for this probe), Feishu would
		//     walk up. For our cases we only ever reply to user
		//     messages (which are roots), so root_id == parent_id.
		rootID := parentID // simplification, true for our cases

		// thread_id: mock computes deterministically from root_id
		// (real Feishu does the same — all replies with same root
		// share a thread_id).
		threadID := "t_thread_" + rootID

		msgID := fmt.Sprintf("om_reply_%d", callCount+1)

		// Annotate response: include reply_in_thread ECHO so we can
		// confirm the SDK accepted it.
		ann := ""
		if req.ReplyInThread != nil {
			ann = fmt.Sprintf("(reply_in_thread=%v)", *req.ReplyInThread)
		} else {
			ann = "(reply_in_thread=omitted)"
		}
		resp := map[string]any{
			"code": 0,
			"msg":  "ok " + ann,
			"data": map[string]any{
				"message_id": msgID,
				"root_id":    rootID,
				"parent_id":  parentID,
				"thread_id":  threadID,
				"msg_type":   req.MsgType,
				"create_time": fmt.Sprintf("%d", time.Now().UnixMilli()),
				"update_time": fmt.Sprintf("%d", time.Now().UnixMilli()),
				"deleted":    false,
			},
		}

		mu.Lock()
		callCount++
		trace = append(trace, mockCall{
			Path: r.URL.Path, Method: r.Method,
			RequestBody:  string(body),
			ResponseBody: asJSON(resp),
		})
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	// ------------------------------------------------------------------
	// 2) Build lark client pointed at the mock.
	// ------------------------------------------------------------------
	cli := lark.NewClient(
		"cli_test", "secret_test",
		lark.WithOpenBaseUrl(srv.URL),
		lark.WithLogLevel(larkcore.LogLevelError), // silence SDK chatter
		lark.WithEnableTokenCache(false),
	)

	ctx := context.Background()
	const chatID = "oc_test_chat"
	const userMsgID = "om_user_root" // user's original message; our "M0" anchor

	// ------------------------------------------------------------------
	// 3) Run 8 representative combinations.
	// ------------------------------------------------------------------
	type combo struct {
		Name        string
		Op          func() (larkimIDs, error)
		Goal        string
	}
	combos := []combo{
		{
			Name: "A. Create top-level (no root_id, no parent)",
			Op: func() (larkimIDs, error) {
				resp, err := cli.Im.V1.Message.Create(ctx, larkim.NewCreateMessageReqBuilder().
					ReceiveIdType(larkim.CreateMessageV1ReceiveIDTypeChatId).
					Body(&larkim.CreateMessageReqBody{
						ReceiveId: ptr(chatID),
						MsgType:   ptr(larkim.MsgTypeText),
						Content:   ptr(`{"text":"hello top-level"}`),
					}).Build())
				return larkimIDsFromCreateResp(resp), err
			},
			Goal: "baseline top-level message; no thread",
		},
		{
			Name: "B. Reply to user msg, reply_in_thread omitted (default false)",
			Op: func() (larkimIDs, error) {
				resp, err := cli.Im.V1.Message.Reply(ctx, larkim.NewReplyMessageReqBuilder().
					MessageId(userMsgID).
					Body(larkim.NewReplyMessageReqBodyBuilder().
						MsgType(larkim.MsgTypeText).
						Content(`{"text":"reply default"}`).
						Build()).
					Build())
				return larkimIDsFromReplyResp(resp), err
			},
			Goal: "what nightme did BEFORE F-37: body has NO reply_in_thread field",
		},
		{
			Name: "C. Reply to user msg, reply_in_thread=false (explicit)",
			Op: func() (larkimIDs, error) {
				resp, err := cli.Im.V1.Message.Reply(ctx, larkim.NewReplyMessageReqBuilder().
					MessageId(userMsgID).
					Body(larkim.NewReplyMessageReqBodyBuilder().
						MsgType(larkim.MsgTypeText).
						Content(`{"text":"reply visible"}`).
						ReplyInThread(false).
						Build()).
					Build())
				return larkimIDsFromReplyResp(resp), err
			},
			Goal: "explicit false — should be identical to (B) in body bytes",
		},
		{
			Name: "D. Reply to user msg, reply_in_thread=true (F-37 path)",
			Op: func() (larkimIDs, error) {
				resp, err := cli.Im.V1.Message.Reply(ctx, larkim.NewReplyMessageReqBuilder().
					MessageId(userMsgID).
					Body(larkim.NewReplyMessageReqBodyBuilder().
						MsgType(larkim.MsgTypeText).
						Content(`{"text":"thread-only"}`).
						ReplyInThread(true).
						Build()).
					Build())
				return larkimIDsFromReplyResp(resp), err
			},
			Goal: "F-37 path: intermediate events stay out of main chat",
		},
		{
			Name: "E. Chain reply: reply to a previous reply, reply_in_thread=true",
			Op: func() (larkimIDs, error) {
				// First send a reply so we have something to chain.
				first, err := cli.Im.V1.Message.Reply(ctx, larkim.NewReplyMessageReqBuilder().
					MessageId(userMsgID).
					Body(larkim.NewReplyMessageReqBodyBuilder().
						MsgType(larkim.MsgTypeText).
						Content(`{"text":"first reply"}`).
						Build()).
					Build())
				if err != nil {
					return larkimIDs{}, err
				}
				firstID := deref(first.Data.MessageId)
				// Now chain a reply to that reply.
				resp, err := cli.Im.V1.Message.Reply(ctx, larkim.NewReplyMessageReqBuilder().
					MessageId(firstID).
					Body(larkim.NewReplyMessageReqBodyBuilder().
						MsgType(larkim.MsgTypeText).
						Content(`{"text":"chained reply"}`).
						ReplyInThread(true).
						Build()).
					Build())
				return larkimIDsFromReplyResp(resp), err
			},
			Goal: "test thread walking: parent_id != M0, root_id should still == M0",
		},
		{
			Name: "F. Raw HTTP POST Create with explicit root_id (the user's hypothesis)",
			Op: func() (larkimIDs, error) {
				// The larkim.CreateMessageReqBody struct does NOT
				// expose a RootId field (verified in v3.9.9 — only
				// receive_id, msg_type, content, uuid). To test
				// whether a Create with root_id would even work, we
				// bypass the SDK and POST raw JSON. This tells us
				// (a) what bytes the wire would carry, and
				// (b) what the mock returns; the real Feishu answer
				//     can only be confirmed against a live server.
				body := fmt.Sprintf(
					`{"receive_id":"%s","msg_type":"text","content":"{\"text\":\"raw with root_id\"}","root_id":"%s"}`,
					chatID, userMsgID,
				)
				req, _ := http.NewRequest("POST", srv.URL+"/open-apis/im/v1/messages?receive_id_type=chat_id", strings.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Authorization", "Bearer t-mock")
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					return larkimIDs{}, err
				}
				defer resp.Body.Close()
				raw, _ := io.ReadAll(resp.Body)

				var parsed struct {
					Code int `json:"code"`
					Data struct {
						MessageID string `json:"message_id"`
						RootID    string `json:"root_id"`
						ParentID  string `json:"parent_id"`
						ThreadID  string `json:"thread_id"`
					} `json:"data"`
				}
				_ = json.Unmarshal(raw, &parsed)
				mu.Lock()
				callCount++
				trace = append(trace, mockCall{
					Path: "/open-apis/im/v1/messages (raw)", Method: "POST",
					RequestBody: body, ResponseBody: string(raw),
				})
				mu.Unlock()
				return larkimIDs{
					MessageID: parsed.Data.MessageID,
					ParentID:  parsed.Data.ParentID,
					RootID:    parsed.Data.RootID,
					ThreadID:  parsed.Data.ThreadID,
				}, nil
			},
			Goal: "user's hypothesis: can a Create with root_id=parent thread into M0?",
		},
		{
			Name: "G. Reply then re-reply with reply_in_thread=true (twice)",
			Op: func() (larkimIDs, error) {
				var last larkimIDs
				for i, label := range []string{"first", "second"} {
					_ = label
					resp, err := cli.Im.V1.Message.Reply(ctx, larkim.NewReplyMessageReqBuilder().
						MessageId(userMsgID).
						Body(larkim.NewReplyMessageReqBodyBuilder().
							MsgType(larkim.MsgTypeText).
							Content(fmt.Sprintf(`{"text":"%s reply-in-thread"}`, label)).
							ReplyInThread(true).
							Build()).
						Build())
					if err != nil {
						return larkimIDs{}, err
					}
					last = larkimIDsFromReplyResp(resp)
					_ = i
				}
				return last, nil
			},
			Goal: "two replies in same thread; both should share root_id/thread_id",
		},
		{
			Name: "H. Reply in 1-on-1 DM context (we can't easily simulate — same wire)",
			Op: func() (larkimIDs, error) {
				// Structurally identical to D; the difference is purely
				// in the chat_id (DM oc vs group oc). SDK doesn't care.
				resp, err := cli.Im.V1.Message.Reply(ctx, larkim.NewReplyMessageReqBuilder().
					MessageId(userMsgID).
					Body(larkim.NewReplyMessageReqBodyBuilder().
						MsgType(larkim.MsgTypeText).
						Content(`{"text":"dm-context reply"}`).
						ReplyInThread(true).
						Build()).
					Build())
				return larkimIDsFromReplyResp(resp), err
			},
			Goal: "DM edge-case: 1-on-1 has no real 'main chat' / 'thread panel' UI",
		},
	}

	// Print the run header.
	fmt.Println(strings.Repeat("=", 96))
	fmt.Println("F-37 thread-routing probe")
	fmt.Printf("mock server: %s\n", srv.URL)
	fmt.Printf("chat_id:    %s\n", chatID)
	fmt.Printf("user_msg_id (anchor): %s\n", userMsgID)
	fmt.Println(strings.Repeat("=", 96))

	results := make([]runRow, 0, len(combos))
	for i, c := range combos {
		fmt.Println()
		fmt.Printf("─── [%d/%d] %s ───\n", i+1, len(combos), c.Name)
		fmt.Printf("goal: %s\n", c.Goal)

		// Reset call trace delta
		mu.Lock()
		before := len(trace)
		mu.Unlock()

		ids, err := c.Op()

		mu.Lock()
		calls := append([]mockCall(nil), trace[before:]...)
		mu.Unlock()

		if err != nil {
			fmt.Printf("  ERROR: %v\n", err)
			results = append(results, runRow{Name: c.Name, Err: err.Error()})
			continue
		}
		results = append(results, runRow{Name: c.Name, IDs: ids, Calls: calls})
	}

	// ------------------------------------------------------------------
	// 4) Final summary: compact table.
	// ------------------------------------------------------------------
	fmt.Println()
	fmt.Println(strings.Repeat("=", 96))
	fmt.Println("Summary")
	fmt.Println(strings.Repeat("=", 96))
	for i, r := range results {
		fmt.Printf("\n[%d] %s\n", i+1, r.Name)
		if r.Err != "" {
			fmt.Printf("    ERROR: %s\n", r.Err)
			continue
		}
		fmt.Printf("    returned IDs: message_id=%s  parent_id=%s  root_id=%s  thread_id=%s\n",
			orDash(r.IDs.MessageID), orDash(r.IDs.ParentID),
			orDash(r.IDs.RootID), orDash(r.IDs.ThreadID))
		fmt.Printf("    outbound request body summary: %s\n", summariseBodies(r.Calls))
	}

	// ------------------------------------------------------------------
	// 5) Highlight the relationship table.
	// ------------------------------------------------------------------
	fmt.Println()
	fmt.Println(strings.Repeat("=", 96))
	fmt.Println("ID-relationship table (parent / root / thread for each call)")
	fmt.Println(strings.Repeat("=", 96))
	fmt.Printf("%-10s | %-30s | %-30s | %-30s\n", "combo", "parent_id", "root_id", "thread_id")
	fmt.Println(strings.Repeat("-", 96))
	for i, r := range results {
		if r.Err != "" {
			continue
		}
		fmt.Printf("%-10s | %-30s | %-30s | %-30s\n",
			fmt.Sprintf("%c", 'A'+rune(i)),
			orDash(r.IDs.ParentID),
			orDash(r.IDs.RootID),
			orDash(r.IDs.ThreadID))
	}

	// ------------------------------------------------------------------
	// 6) What this probe does NOT cover.
	// ------------------------------------------------------------------
	fmt.Println()
	fmt.Println(strings.Repeat("=", 96))
	fmt.Println("What this probe does NOT tell us (needs real Feishu + UI):")
	fmt.Println(strings.Repeat("=", 96))
	fmt.Println(`
  1. Real Feishu root-id resolution. Our mock computes root_id
     = parent_id when the parent is itself a root, and computes
     thread_id deterministically from root_id. Real Feishu has
     its own resolver; we trust the SDK passes our bodies through
     faithfully, but the server-side behaviour we cannot observe
     from a mock.

  2. The "X replies" indicator in main chat. reply_in_thread=true
     SHOULD hide the message body from the main chat and show a
     "X replies" indicator instead. Only a real Feishu client
     screenshot can confirm this rendering.

  3. Topic-mode group chats. When a group is in topic-mode
     (用户把群设为"以话题形式回复"), ALL replies render as
     thread-only regardless of reply_in_thread. The flag is then
     effectively a no-op for that chat. We did not exercise this
     path; needs ops + a topic-mode group.

  4. DM (1-on-1) has no "main chat vs thread panel" UI split —
     all messages show in one stream. The "thread-only" toggle
     may not even render differently in DM. We did not test this.

  5. Whether Feishu rejects Create+RootId (combo F) outright,
     silently ignores it, or honours it. Mock accepts it.
`)

	// Print pretty request bodies for first 4 combos so user can
	// eyeball the JSON difference between B and D.
	fmt.Println()
	fmt.Println(strings.Repeat("=", 96))
	fmt.Println("Selected request bodies (B vs C vs D — should differ only by reply_in_thread)")
	fmt.Println(strings.Repeat("=", 96))
	for _, r := range results {
		if !strings.HasPrefix(r.Name, "B.") && !strings.HasPrefix(r.Name, "C.") && !strings.HasPrefix(r.Name, "D.") {
			continue
		}
		fmt.Printf("\n%s\n", r.Name)
		for _, c := range r.Calls {
			if c.Path == "/open-apis/auth/v3/tenant_access_token/internal" {
				continue
			}
			fmt.Printf("  %s %s\n", c.Method, c.Path)
			fmt.Printf("  body: %s\n", prettyJSON(c.RequestBody))
		}
	}

	_ = sort.StringSlice{} // silence unused import in some go versions
}

// ----------------------------------------------------------------------
// Helpers
// ----------------------------------------------------------------------

type mockCall struct {
	Path         string
	Method       string
	RequestBody  string
	ResponseBody string
}

type larkimIDs struct {
	MessageID string
	ParentID  string
	RootID    string
	ThreadID  string
}

type runRow struct {
	Name  string
	Err   string
	IDs   larkimIDs
	Calls []mockCall
}

func larkimIDsFromReplyResp(r *larkim.ReplyMessageResp) larkimIDs {
	if r == nil || r.Data == nil {
		return larkimIDs{}
	}
	return larkimIDs{
		MessageID: deref(r.Data.MessageId),
		ParentID:  deref(r.Data.ParentId),
		RootID:    deref(r.Data.RootId),
		ThreadID:  deref(r.Data.ThreadId),
	}
}

func larkimIDsFromCreateResp(r *larkim.CreateMessageResp) larkimIDs {
	if r == nil || r.Data == nil {
		return larkimIDs{}
	}
	return larkimIDs{
		MessageID: deref(r.Data.MessageId),
		ParentID:  deref(r.Data.ParentId),
		RootID:    deref(r.Data.RootId),
		ThreadID:  deref(r.Data.ThreadId),
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func ptr(s string) *string { return &s }

func asJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func summariseBodies(calls []mockCall) string {
	if len(calls) == 0 {
		return "(no calls captured)"
	}
	var summaries []string
	for _, c := range calls {
		if c.Path == "/open-apis/auth/v3/tenant_access_token/internal" {
			continue
		}
		// Show only the keys, not values, so the table fits.
		var parsed map[string]any
		_ = json.Unmarshal([]byte(c.RequestBody), &parsed)
		keys := make([]string, 0, len(parsed))
		for k := range parsed {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		summaries = append(summaries, fmt.Sprintf("%s{keys=%v}", c.Path, keys))
	}
	return strings.Join(summaries, " | ")
}

func prettyJSON(s string) string {
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return s
	}
	b, _ := json.MarshalIndent(v, "    ", "  ")
	return string(b)
}

// Make linter happy if any helper becomes unused during edits.
var _ = os.Exit
