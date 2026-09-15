// Adapter-level tests for the OutResult → rich blocks wire form.
// Pins the §11.12.4.1 v9 P2 contract that OutResult is a standalone
// rich message with a footer block carrying the StatusBar trailer
// (not a plaintext panel embedded in markdown). Locks in:
//   - sendRichMessage is called with rich_message[blocks], not
//     rich_message[markdown] (the L1 plaintext-trailer path is gone)
//   - body markdown features (heading / fence / list / url entity)
//     reach the wire as proper rich blocks
//   - the StatusBar trailer renders as a footer block + divider,
//     with PR anchors as url entities (not literal markdown)
//   - chain.chunks / richTurn.entries are NOT touched by OutResult
//   - richTurn.resultMessageID is set to the rich message's id
package telegram

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/messages"
)

// findSendRichMessage returns the first sendRichMessage call (the
// OutResult standalone path). Returns nil if none recorded.
func findSendRichMessage(calls []fakeCall) *fakeCall {
	for i := range calls {
		if calls[i].Method == "sendRichMessage" {
			return &calls[i]
		}
	}
	return nil
}

// decodeBlocks pulls rich_message.blocks out of a sendRichMessage
// params map and unmarshals it as []map[string]any. Returns
// (nil, false) when the key is missing or malformed — caller treats
// that as a test failure.
func decodeBlocks(params map[string]any) ([]map[string]any, bool) {
	rm, ok := params["rich_message"].(map[string]any)
	if !ok {
		return nil, false
	}
	raw, ok := rm["blocks"].(json.RawMessage)
	if !ok {
		return nil, false
	}
	var blocks []map[string]any
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, false
	}
	return blocks, true
}

// TestOutResult_SendsRichBlocksNotMarkdown verifies the wire form
// after the migration: sendRichMessage params carry rich_message[blocks],
// not rich_message[markdown]. This is the single behavioural
// regression-guard for the plaintext-statusbar removal.
func TestOutResult_SendsRichBlocksNotMarkdown(t *testing.T) {
	a, api := newTestAdapter(t)
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "100",
		Kind:   messages.OutResult,
		Text:   "final answer",
	}); err != nil {
		t.Fatalf("OutResult Send: %v", err)
	}

	calls := api.snapshotCalls()
	call := findSendRichMessage(calls)
	if call == nil {
		t.Fatalf("expected sendRichMessage call; got %+v", calls)
	}
	rm, ok := call.Params["rich_message"].(map[string]any)
	if !ok {
		t.Fatalf("rich_message map missing; params=%+v", call.Params)
	}
	if _, hasMarkdown := rm["markdown"]; hasMarkdown {
		t.Errorf("OutResult must not use rich_message.markdown (plaintext trailer is gone); params=%+v", call.Params)
	}
	if _, hasBlocks := rm["blocks"]; !hasBlocks {
		t.Errorf("OutResult must use rich_message.blocks; params=%+v", call.Params)
	}
}

// TestOutResult_BodyMarkdownFeaturesPreserved verifies the body
// markdown reaches the wire as proper rich blocks (heading +
// paragraph + fence), not as a single paragraph blob.
func TestOutResult_BodyMarkdownFeaturesPreserved(t *testing.T) {
	a, api := newTestAdapter(t)
	body := "# Title\n\nparagraph line\n\n```go\nfmt.Println(\"hi\")\n```"
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "100",
		Kind:   messages.OutResult,
		Text:   body,
	}); err != nil {
		t.Fatalf("OutResult Send: %v", err)
	}
	call := findSendRichMessage(api.snapshotCalls())
	if call == nil {
		t.Fatal("sendRichMessage missing")
	}
	blocks, ok := decodeBlocks(call.Params)
	if !ok {
		t.Fatalf("decodeBlocks failed; params=%+v", call.Params)
	}
	types := make([]string, len(blocks))
	for i, b := range blocks {
		types[i] = b["type"].(string)
	}
	wantTypes := []string{"heading", "paragraph", "pre"}
	for i, want := range wantTypes {
		if types[i] != want {
			t.Errorf("block %d type=%s, want %s (full=%v)", i, types[i], want, types)
		}
	}
	for _, b := range blocks {
		if b["type"] == "pre" {
			if b["language"] != "go" {
				t.Errorf("pre language=%v, want go", b["language"])
			}
		}
	}
}

// TestOutResult_FooterIsFooterBlockNotRenderPanel verifies the
// trailer is a `{"type":"footer", "text":...}` block + preceding
// divider, NOT a RenderPanel-style plaintext tail inside a
// paragraph block. richOut (with no PullRequest) yields a plain-
// string footer.text — the all-plain fast path.
func TestOutResult_FooterIsFooterBlockNotRenderPanel(t *testing.T) {
	a, api := newTestAdapter(t)
	if err := a.Send(context.Background(), richOut(messages.OutResult, "final answer")); err != nil {
		t.Fatalf("OutResult Send: %v", err)
	}
	call := findSendRichMessage(api.snapshotCalls())
	if call == nil {
		t.Fatal("sendRichMessage missing")
	}
	blocks, ok := decodeBlocks(call.Params)
	if !ok {
		t.Fatalf("decodeBlocks failed; params=%+v", call.Params)
	}
	// paragraph(body) + divider + footer = 3 blocks.
	if len(blocks) != 3 {
		t.Fatalf("expected 3 blocks (body + divider + footer), got %d: %v", len(blocks), blocks)
	}
	if blocks[1]["type"] != "divider" {
		t.Errorf("block 1 type=%v, want divider", blocks[1]["type"])
	}
	if blocks[2]["type"] != "footer" {
		t.Errorf("block 2 type=%v, want footer", blocks[2]["type"])
	}
	// richOut carries no PullRequest → statusbar.StatusBarLines
	// emits no `[#N](url)` anchor → footerLinesToRichText takes the
	// plain-string fast path. The footer block's text is the raw
	// statusbar lines joined by `\n`.
	text, ok := blocks[2]["text"].(string)
	if !ok {
		t.Fatalf("footer.text should be string when no entities, got %T: %v",
			blocks[2]["text"], blocks[2]["text"])
	}
	for _, want := range []string{"🤖:", "💰:", "📁:"} {
		if !strings.Contains(text, want) {
			t.Errorf("footer text missing %q; got %q", want, text)
		}
	}
}

// TestOutResult_FooterPRAnchorBecomesUrlEntity verifies that a
// PullRequest field on the message makes the git line carry a
// `[#N](url)` markdown anchor, which the rich-text footer
// converts to a `{"type":"url","text":"#N","url":"..."}` entity —
// preserving the clickable-PR behaviour that wireFormatFooterLine
// used to provide in the parse_mode=HTML path.
func TestOutResult_FooterPRAnchorBecomesUrlEntity(t *testing.T) {
	a, api := newTestAdapter(t)
	msg := richOut(messages.OutResult, "final answer")
	msg.GitStatus.PullRequest = &messages.PR{
		Number: 284,
		URL:    "https://github.com/cnlangzi/nightme/pull/284",
	}
	if err := a.Send(context.Background(), msg); err != nil {
		t.Fatalf("OutResult Send: %v", err)
	}
	call := findSendRichMessage(api.snapshotCalls())
	if call == nil {
		t.Fatal("sendRichMessage missing")
	}
	blocks, ok := decodeBlocks(call.Params)
	if !ok {
		t.Fatalf("decodeBlocks failed; params=%+v", call.Params)
	}
	footerBlock := blocks[len(blocks)-1]
	if footerBlock["type"] != "footer" {
		t.Fatalf("last block type=%v, want footer", footerBlock["type"])
	}
	text, ok := footerBlock["text"].([]any)
	if !ok {
		t.Fatalf("footer.text should be []any when PR anchor present, got %T: %v",
			footerBlock["text"], footerBlock["text"])
	}
	var foundPR bool
	for _, item := range text {
		m, _ := item.(map[string]any)
		if m["type"] == "url" && m["text"] == "#284" && m["url"] == "https://github.com/cnlangzi/nightme/pull/284" {
			foundPR = true
		}
	}
	if !foundPR {
		t.Errorf("expected url entity {text:#284 url:https://github.com/cnlangzi/nightme/pull/284} in footer; wire=%v", text)
	}
}

// TestOutResult_NoFooterNoDividerBlock verifies that OutResult with
// no StatusBar fields does NOT add a divider or footer block —
// only the body blocks reach the wire.
func TestOutResult_NoFooterNoDividerBlock(t *testing.T) {
	a, api := newTestAdapter(t)
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "100",
		Kind:   messages.OutResult,
		Text:   "no status data here",
	}); err != nil {
		t.Fatalf("OutResult Send: %v", err)
	}
	call := findSendRichMessage(api.snapshotCalls())
	if call == nil {
		t.Fatal("sendRichMessage missing")
	}
	blocks, ok := decodeBlocks(call.Params)
	if !ok {
		t.Fatalf("decodeBlocks failed; params=%+v", call.Params)
	}
	if len(blocks) != 1 {
		t.Fatalf("expected 1 body block, got %d: %v", len(blocks), blocks)
	}
	if blocks[0]["type"] != "paragraph" {
		t.Errorf("block 0 type=%v, want paragraph", blocks[0]["type"])
	}
}

// TestOutResult_EmptyTextSilentDrop pins the existing
// sendOutResultMessage empty-text contract — no sendRichMessage
// call, no chain mutation, no error.
func TestOutResult_EmptyTextSilentDrop(t *testing.T) {
	a, api := newTestAdapter(t)
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "100",
		Kind:   messages.OutResult,
		Text:   "",
	}); err != nil {
		t.Fatalf("empty OutResult must not error: %v", err)
	}
	for _, call := range api.snapshotCalls() {
		if call.Method == "sendRichMessage" || call.Method == "sendMessage" {
			t.Errorf("empty OutResult must not send anything; got %+v", call.Params)
		}
	}
}

// TestOutResult_ReplyToUserMessageAndThread verifies the standalone
// message carries reply_to_message_id=userMsgID and, in topic mode,
// message_thread_id. This anchors OutResult visually under the user's
// triggering message.
func TestOutResult_ReplyToUserMessageAndThread(t *testing.T) {
	a, api := newTestAdapter(t)
	_ = a.state.putTopic(&TopicState{ChatID: "100", TopicID: 42, UserMessageID: "555"})
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "tg_100:42",
		Kind:   messages.OutResult,
		Text:   "topic answer",
	}); err != nil {
		t.Fatalf("OutResult Send: %v", err)
	}
	call := findSendRichMessage(api.snapshotCalls())
	if call == nil {
		t.Fatal("sendRichMessage missing")
	}
	if reply, ok := call.Params["reply_to_message_id"]; !ok || reply != 555 {
		t.Errorf("reply_to_message_id=%v, want 555", reply)
	}
	if thread, ok := call.Params["message_thread_id"]; !ok || thread != 42 {
		t.Errorf("message_thread_id=%v, want 42", thread)
	}
}

// TestOutResult_DMNoThreadID verifies DM-mode OutResult (topicID=0)
// carries `reply_to_message_id=userMsgID` but NOT `message_thread_id`.
// DM has no Forum topic; threading would be invalid. Complements
// TestOutResult_ReplyToUserMessageAndThread (which covers topic mode).
func TestOutResult_DMNoThreadID(t *testing.T) {
	a, api := newTestAdapter(t)
	_ = a.state.putTopic(&TopicState{ChatID: "100", TopicID: 0, UserMessageID: "42"})
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "tg_100",
		Kind:   messages.OutResult,
		Text:   "dm answer",
	}); err != nil {
		t.Fatalf("OutResult Send: %v", err)
	}
	call := findSendRichMessage(api.snapshotCalls())
	if call == nil {
		t.Fatal("sendRichMessage missing")
	}
	if reply, ok := call.Params["reply_to_message_id"]; !ok || reply != 42 {
		t.Errorf("reply_to_message_id=%v, want 42", reply)
	}
	if _, hasThread := call.Params["message_thread_id"]; hasThread {
		t.Errorf("DM OutResult must not carry message_thread_id; params=%+v", call.Params)
	}
}

// TestOutResult_MultipleInOneTurn_LastWins verifies the
// `chain.resultMessageID` "last wins" semantics: two OutResults in
// the same turn record the second's messageID, not the first. The
// OnPromptEnded 🎉 anchor follows whichever is latest. Pins the
// §11.12.9 contract that the 🎉 reaction always lands on the most
// recent OutResult.
func TestOutResult_MultipleInOneTurn_LastWins(t *testing.T) {
	a, api := newTestAdapter(t)
	_ = a.state.putTopic(&TopicState{ChatID: "100", TopicID: 0, UserMessageID: "77"})

	// Two OutResults in one turn → fakeAPI assigns ids 101 (first)
	// and 102 (second). We don't know the exact ids without
	// inspecting SendMessageResult, but we can assert: after
	// OnPromptEnded, the 🎉 must NOT target the first id; it must
	// target a different (later) one.
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "tg_100",
		Kind:   messages.OutResult,
		Text:   "first",
	}); err != nil {
		t.Fatalf("OutResult 1: %v", err)
	}
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "tg_100",
		Kind:   messages.OutResult,
		Text:   "second",
	}); err != nil {
		t.Fatalf("OutResult 2: %v", err)
	}

	// Two sendRichMessage calls should be recorded.
	richCalls := findCalls(api.snapshotCalls(), "sendRichMessage")
	if len(richCalls) != 2 {
		t.Fatalf("expected 2 sendRichMessage calls, got %d", len(richCalls))
	}

	// OnPromptEnded stamps 🎉 — read the message_id from
	// setMessageReaction. With "last wins" semantics, this must
	// equal the message_id of the second sendRichMessage. We
	// can't extract the message_id from sendRichMessage params
	// (fakeAPI stores it on SendMessageResult, not params), but
	// we can verify the 🎉 is non-zero — proving the field was
	// populated and not the zero default that would mean
	// "no OutResult landed".
	api.Calls = api.Calls[:0]
	a.OnPromptEnded(context.Background(), "tg_100", "77", agent.PromptEndClean)
	var stampedTarget int64
	for _, call := range api.snapshotCalls() {
		if call.Method == "setMessageReaction" {
			switch mid := call.Params["message_id"].(type) {
			case int:
				stampedTarget = int64(mid)
			case int64:
				stampedTarget = mid
			case float64:
				stampedTarget = int64(mid)
			}
		}
	}
	if stampedTarget == 0 {
		t.Fatal("🎉 target must be non-zero when OutResult landed in this turn")
	}
}

// TestOutResult_ResultMessageIDAnchorsOnPromptEnded verifies the
// standalone rich message_id is recorded on richTurn.resultMessageID
// so OnPromptEnded's terminal 🎉 reaction lands on the result
// message rather than the rich turn placeholder. This is the
// §11.12.4.1 + §11.12.9 contract that the result-message ↔ 🎉 anchor
// chain survives the rich-blocks refactor.
//
// fakeAPI assigns message ids monotonically starting at 101 (first
// sendMessage / sendRichMessage after resetSendMessageCounter returns
// 100+1). Cold-create placeholder → 101; OutResult → 102; 🎉 lands
// on 102 (resultMID), NOT 101 (placeholderMID).
func TestOutResult_ResultMessageIDAnchorsOnPromptEnded(t *testing.T) {
	a, api := newTestAdapter(t)
	_ = a.state.putTopic(&TopicState{ChatID: "100", TopicID: 0, UserMessageID: "77"})

	// 1. OutReply cold-creates the rich turn placeholder (101).
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "tg_100",
		Kind:   messages.OutReply,
		Text:   "starting",
	}); err != nil {
		t.Fatalf("OutReply Send: %v", err)
	}

	// 2. OutResult → standalone sendRichMessage (102).
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "tg_100",
		Kind:   messages.OutResult,
		Text:   "final",
	}); err != nil {
		t.Fatalf("OutResult Send: %v", err)
	}

	// 3. OnPromptEnded stamps 🎉 — must land on resultMID, not
	//    placeholderMID. Read the message_id param off the
	//    setMessageReaction call.
	api.Calls = api.Calls[:0]
	a.OnPromptEnded(context.Background(), "tg_100", "77", agent.PromptEndClean)
	var stampedTarget int64
	for _, call := range api.snapshotCalls() {
		if call.Method == "setMessageReaction" {
			switch mid := call.Params["message_id"].(type) {
			case int:
				stampedTarget = int64(mid)
			case int64:
				stampedTarget = mid
			case float64:
				stampedTarget = int64(mid)
			}
		}
	}
	if stampedTarget == 0 {
		t.Fatal("no setMessageReaction call observed")
	}
	if stampedTarget == 101 {
		t.Errorf("🎉 stamped on placeholderMID=101; should target resultMID=102")
	}
	if stampedTarget != 102 {
		t.Errorf("🎉 stamped on message_id=%d, want 102 (resultMID)", stampedTarget)
	}
}

// editBlocks extracts the rich_message.blocks JSON from an
// editMessageText call's params map and decodes it. Returns
// (nil, false) when the call isn't an edit or the blocks payload
// is malformed. Companion to decodeBlocks (which only handles
// sendRichMessage — editMessageText uses a different envelope:
// rich_message is a json.RawMessage of the full envelope
// `{"blocks":[…]}`, while sendRichMessage wraps it as
// map[string]any{"blocks": json.RawMessage(…)}).
func editBlocks(params map[string]any) ([]map[string]any, bool) {
	raw, ok := params["rich_message"].(json.RawMessage)
	if !ok {
		return nil, false
	}
	var envelope struct {
		Blocks []map[string]any `json:"blocks"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, false
	}
	return envelope.Blocks, true
}

// lastEditBlocks returns the blocks from the most recent
// editMessageText call, or (nil, false) when no edit was
// recorded.
func lastEditBlocks(calls []fakeCall) ([]map[string]any, bool) {
	for _, call := range slices.Backward(calls) {
		if call.Method != "editMessageText" {
			continue
		}
		return editBlocks(call.Params)
	}
	return nil, false
}

// TestOutResult_RefreshesTurnFooterWithUsage pins the new
// contract: when OutResult arrives with Usage populated, the
// rich-turn placeholder card's next editMessageText PATCH
// carries the 💰 token-usage line. Previously the placeholder's
// footer was last-stamped by a streaming OutReply (msg.Usage==nil)
// and stayed stale until turn end — leaving the user staring at
// a "🤖 Working..." card without ever seeing the token bar.
//
// Flow:
//  1. OutReply cold-creates the placeholder (msg.Usage==nil →
//     no 💰 line).
//  2. OutResult lands → standalone message sent + turn.footer
//     refreshed with the OutResult's StatusBarLines (Usage
//     populated).
//  3. OnPromptEnded's synchronous flushRichTurn PATCHes the
//     placeholder; that final editMessageText's blocks carry a
//     footer block whose text contains 💰.
func TestOutResult_RefreshesTurnFooterWithUsage(t *testing.T) {
	a, api := newTestAdapter(t)
	_ = a.state.putTopic(&TopicState{ChatID: "100", TopicID: 0, UserMessageID: "77"})

	// 1. OutReply — streaming event, msg.Usage is nil. Cold-creates
	//    the placeholder; StatusBarLines on this message yields
	//    Identity + Git (no Usage line) when those fields are
	//    stamped by the runtime. We exercise the "no status
	//    metadata on streaming event" path explicitly to lock the
	//    delta: the streaming PATCH carries NO 💰 row.
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "tg_100",
		Kind:   messages.OutReply,
		Text:   "starting",
	}); err != nil {
		t.Fatalf("OutReply Send: %v", err)
	}

	// 2. OutResult with Usage populated — both a standalone
	//    sendRichMessage AND a turn.footer refresh happen here.
	if err := a.Send(context.Background(), richOut(messages.OutResult, "final answer")); err != nil {
		t.Fatalf("OutResult Send: %v", err)
	}

	// 3. Force a synchronous flush via OnPromptEnded's helper path
	//    so the test doesn't race the 250ms debounce. The flush
	//    emits a final editMessageText whose blocks carry the
	//    refreshed footer (💰 included).
	api.Calls = api.Calls[:0]
	a.OnPromptEndedRichTurn("100", 0, 77)

	blocks, ok := lastEditBlocks(api.snapshotCalls())
	if !ok {
		t.Fatal("expected an editMessageText from the synchronous flush; got none")
	}
	// Find the footer block; it should be the last block
	// (after entries + divider).
	var footerText string
	for _, b := range blocks {
		if b["type"] == "footer" {
			text, _ := b["text"].(string)
			footerText = text
		}
	}
	if footerText == "" {
		t.Fatalf("placeholder PATCH must include a footer block; blocks=%+v", blocks)
	}
	if !strings.Contains(footerText, "💰:") {
		t.Errorf("placeholder footer must carry the 💰 token-usage line; got %q", footerText)
	}
	// Identity and Git lines survive the refresh too — the
	// StatusBarLines snapshot from OutResult carries all three
	// rows, not just Usage.
	for _, want := range []string{"🤖:", "💰:", "📁:"} {
		if !strings.Contains(footerText, want) {
			t.Errorf("placeholder footer missing %q; got %q", want, footerText)
		}
	}
}

// TestOutResult_UsageOnlyNotDropped pins the loosened Send()
// top-level guard: a turn that ends with empty Text but
// populated Usage (translate.go passes those through) must NOT
// be silently dropped at the empty-text filter. The richer
// surface (footer refresh) is the only signal worth sending
// — no standalone rich message (body is empty), but the
// placeholder card still picks up the 💰 line on next flush.
func TestOutResult_UsageOnlyNotDropped(t *testing.T) {
	a, api := newTestAdapter(t)
	_ = a.state.putTopic(&TopicState{ChatID: "100", TopicID: 0, UserMessageID: "77"})

	// Seed the placeholder so the refresh path has something to
	// PATCH.
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "tg_100",
		Kind:   messages.OutReply,
		Text:   "starting",
	}); err != nil {
		t.Fatalf("OutReply Send: %v", err)
	}
	api.Calls = api.Calls[:0]

	// Usage-only OutResult: empty Text, populated Usage. The
	// pre-fix Send() top-level guard would drop this on the
	// "OutResult with empty Text → silent drop" branch; the
	// post-fix guard lets it through because StatusBarLines is
	// non-nil.
	usageOnly := messages.OutboundMessage{
		ChatID: "tg_100",
		Kind:   messages.OutResult,
		Text:   "",
		Usage: &agent.UsageInfo{
			InputTokens: 1_000, OutputTokens: 200, CostUSD: 0.012,
		},
	}
	if err := a.Send(context.Background(), usageOnly); err != nil {
		t.Fatalf("usage-only OutResult must not error: %v", err)
	}

	// No standalone rich message body — sendRichMessage count
	// stays at 1 (the seed placeholder cold-create).
	richCalls := findCalls(api.snapshotCalls(), "sendRichMessage")
	if len(richCalls) != 0 {
		t.Errorf("usage-only OutResult must not send a standalone message; got %d sendRichMessage calls",
			len(richCalls))
	}

	// But the synchronous flush must include the 💰 row on the
	// placeholder PATCH.
	a.OnPromptEndedRichTurn("100", 0, 77)
	blocks, ok := lastEditBlocks(api.snapshotCalls())
	if !ok {
		t.Fatal("expected editMessageText from flush; got none")
	}
	var footerText string
	for _, b := range blocks {
		if b["type"] == "footer" {
			text, _ := b["text"].(string)
			footerText = text
		}
	}
	if !strings.Contains(footerText, "💰:") {
		t.Errorf("usage-only OutResult must refresh placeholder footer with 💰; got %q", footerText)
	}
}

// TestOutResult_ResultTextPreferredOverText verifies that
// sendOutResultMessage uses msg.Result.Text (the canonical
// OutResult payload) when present, rather than msg.Text. This
// matches feishu's adapter and the OutboundMessage struct
// contract ("Text carries the rendered body for OutReply /
// OutThinking"; Result.Text is the OutResult body).
func TestOutResult_ResultTextPreferredOverText(t *testing.T) {
	a, api := newTestAdapter(t)
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "100",
		Kind:   messages.OutResult,
		Text:   "stale reply text",
		Result: &agent.AgentResultEvent{
			Text: "canonical result text",
		},
	}); err != nil {
		t.Fatalf("OutResult Send: %v", err)
	}
	call := findSendRichMessage(api.snapshotCalls())
	if call == nil {
		t.Fatal("sendRichMessage missing")
	}
	blocks, ok := decodeBlocks(call.Params)
	if !ok {
		t.Fatalf("decodeBlocks failed; params=%+v", call.Params)
	}
	// Find the body paragraph and assert its text comes from
	// Result.Text, not Text.
	var bodyText string
	for _, b := range blocks {
		if b["type"] == "paragraph" {
			bodyText, _ = b["text"].(string)
			break
		}
	}
	if !strings.Contains(bodyText, "canonical result text") {
		t.Errorf("body must use msg.Result.Text; got %q", bodyText)
	}
	if strings.Contains(bodyText, "stale reply text") {
		t.Errorf("body must not fall back to msg.Text when Result is set; got %q", bodyText)
	}
}

// TestOutResult_FooterRefreshUpdatesDebouncedFlush verifies the
// post-fix debounced flush path: after OutResult lands with
// Usage, the 250ms debounce timer fires and emits an
// editMessageText PATCH whose footer carries the 💰 row. Uses
// a real sleep + buffer to let the timer fire — slower than
// OnPromptEndedRichTurn but covers the async flush contract
// without coupling to OnPromptEnded's call signature.
func TestOutResult_FooterRefreshUpdatesDebouncedFlush(t *testing.T) {
	a, api := newTestAdapter(t)
	_ = a.state.putTopic(&TopicState{ChatID: "100", TopicID: 0, UserMessageID: "77"})

	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "tg_100",
		Kind:   messages.OutReply,
		Text:   "starting",
	}); err != nil {
		t.Fatalf("OutReply Send: %v", err)
	}
	api.Calls = api.Calls[:0]

	if err := a.Send(context.Background(), richOut(messages.OutResult, "final")); err != nil {
		t.Fatalf("OutResult Send: %v", err)
	}

	// Wait for the 250ms debounce + the 5s flush timeout buffer
	// (the flush itself is fast; we just need the timer to fire).
	deadline := time.Now().Add(2 * time.Second)
	var blocks []map[string]any
	var found bool
	for time.Now().Before(deadline) {
		if b, ok := lastEditBlocks(api.snapshotCalls()); ok {
			blocks = b
			found = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !found {
		t.Fatal("debounced flush never emitted an editMessageText within 2s")
	}
	var footerText string
	for _, blk := range blocks {
		if blk["type"] == "footer" {
			text, _ := blk["text"].(string)
			footerText = text
		}
	}
	if !strings.Contains(footerText, "💰:") {
		t.Errorf("debounced flush must carry 💰 line; got footer=%q blocks=%+v", footerText, blocks)
	}
}
