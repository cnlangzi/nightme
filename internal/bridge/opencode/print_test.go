// print_test.go — unit tests for the print-mode parser surface
// (handleRunEvent, runNDJSON, buildPrintArgs). These tests do
// NOT require a real opencode binary; they exercise the pure
// Go logic against canned JSON wire format. The real-binary
// integration tests live in print_real_unix_test.go.
//
// Wire shape under test (per packages/opencode/src/cli/cmd/run.ts,
// sst/opencode@dev, observed on 1.18.14):
//
//	{ "type": "<event>", "timestamp": <ms>, "sessionID": "<id>", ... }
//
// Event types we act on: text, reasoning, tool_use, step_start,
// step_finish, error. Unknown types are tolerated (ignored).

package opencode

import (
	"context"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
)

// TestHandleRunEvent_Text exercises the happy-path accumulator:
// one text event must append to state.text and flip
// hadContent.
func TestHandleRunEvent_Text(t *testing.T) {
	var st printState
	handleRunEvent(runEvent{
		Type:      "text",
		SessionID: "ses_abc",
		Part:      []byte(`{"text":"hello"}`),
	}, &st)

	if got, want := st.text.String(), "hello"; got != want {
		t.Errorf("text = %q, want %q", got, want)
	}
	if !st.hadContent {
		t.Errorf("hadContent = false, want true")
	}
	if st.sessionID != "ses_abc" {
		t.Errorf("sessionID = %q, want ses_abc", st.sessionID)
	}
	if st.done {
		t.Errorf("done = true after a text event, want false")
	}
}

// TestHandleRunEvent_TextConcatenation — multiple text events
// must concatenate in arrival order (opencode 1.18 streams
// token-by-token via `text` events with `delta` payloads; the
// first server-build may emit a single `text` event with the
// full payload; either way, order is preserved).
func TestHandleRunEvent_TextConcatenation(t *testing.T) {
	var st printState
	for _, s := range []string{"foo", " bar", " baz"} {
		handleRunEvent(runEvent{
			Type: "text",
			Part: []byte(`{"text":"` + s + `"}`),
		}, &st)
	}
	if got, want := st.text.String(), "foo bar baz"; got != want {
		t.Errorf("text = %q, want %q", got, want)
	}
}

// TestHandleRunEvent_StepFinish_Terminal — step_finish must
// flip done=true and must NOT be terminal for the *text*
// accumulator (text already arrived). Mirrors codex behavior
// where the result event carries the final text on the same
// wire event.
func TestHandleRunEvent_StepFinish_Terminal(t *testing.T) {
	var st printState
	handleRunEvent(runEvent{Type: "text", Part: []byte(`{"text":"READY"}`)}, &st)
	handleRunEvent(runEvent{Type: "step_finish"}, &st)

	if !st.done {
		t.Errorf("done = false after step_finish, want true")
	}
	if got, want := st.text.String(), "READY"; got != want {
		t.Errorf("text = %q, want %q", got, want)
	}
}

// TestHandleRunEvent_ReasoningNotAccumulated — reasoning
// events flip hadContent (so Done doesn't go "empty") but do
// not surface to RunResult.Text. This matches the long-lived
// translator's choice and keeps RunOnce output stable.
func TestHandleRunEvent_ReasoningNotAccumulated(t *testing.T) {
	var st printState
	handleRunEvent(runEvent{
		Type: "reasoning",
		Part: []byte(`{"text":"thinking..."}`),
	}, &st)

	if st.text.Len() != 0 {
		t.Errorf("text.Len = %d, want 0", st.text.Len())
	}
	if !st.hadContent {
		t.Errorf("hadContent = false, want true (reasoning counts as content)")
	}
}

// TestHandleRunEvent_ToolUseBookkeeping — tool_use events are
// dropped from RunResult (RunOnce is single-turn; tool
// progress is a chat-session concern). hadContent must NOT
// flip on tool_use alone so the "empty answer" guard at the
// end of runPrintMode can still distinguish a model that did
// nothing from a model that used tools but wrote nothing.
func TestHandleRunEvent_ToolUseBookkeeping(t *testing.T) {
	var st printState
	handleRunEvent(runEvent{
		Type: "tool_use",
		Part: []byte(`{"tool":"bash","state":{"status":"completed","output":"x"}}`),
	}, &st)

	if st.hadContent {
		t.Errorf("hadContent = true after tool_use, want false")
	}
	if st.text.Len() != 0 {
		t.Errorf("text.Len = %d, want 0", st.text.Len())
	}
}

// TestHandleRunEvent_Error_String — `error` events with a
// JSON string payload must populate errMsg verbatim.
func TestHandleRunEvent_Error_String(t *testing.T) {
	var st printState
	handleRunEvent(runEvent{
		Type:  "error",
		Error: []byte(`"rate limited"`),
	}, &st)

	if got, want := st.errMsg, "rate limited"; got != want {
		t.Errorf("errMsg = %q, want %q", got, want)
	}
}

// TestHandleRunEvent_Error_Null — `error` events with a literal
// `null` payload must fall through to the "unknown error event"
// sentinel. Without this guard, `state.errMsg` would stay empty
// (json.Unmarshal of "null" into string returns ""), and the
// outer `errMsg != ""` check in runPrintMode would silently
// drop the failure — the caller would see a successful result
// with no text.
func TestHandleRunEvent_Error_Null(t *testing.T) {
	var st printState
	handleRunEvent(runEvent{
		Type:  "error",
		Error: []byte(`null`),
	}, &st)

	if got, want := st.errMsg, "unknown error event"; got != want {
		t.Errorf("errMsg = %q, want %q", got, want)
	}
}

// TestHandleRunEvent_Error_Empty — `error` events with no
// payload at all (the `error` field omitted from the JSON
// envelope) must also fall through to the sentinel.
func TestHandleRunEvent_Error_Empty(t *testing.T) {
	var st printState
	handleRunEvent(runEvent{
		Type: "error",
	}, &st)

	if got, want := st.errMsg, "unknown error event"; got != want {
		t.Errorf("errMsg = %q, want %q", got, want)
	}
}

// TestHandleRunEvent_Error_Object — `error` events with a
// non-string payload fall back to the raw JSON rendering so
// the caller can still inspect the failure shape.
func TestHandleRunEvent_Error_Object(t *testing.T) {
	var st printState
	handleRunEvent(runEvent{
		Type:  "error",
		Error: []byte(`{"code":429,"message":"quota"}`),
	}, &st)

	if !strings.Contains(st.errMsg, "quota") {
		t.Errorf("errMsg = %q, want contains 'quota'", st.errMsg)
	}
}

// TestHandleRunEvent_SessionID_FirstWins — sessionID on the
// first parsed event wins. RunOnce is one-shot so we never
// expect a second event to carry a different id; this is just
// a defensive invariant.
func TestHandleRunEvent_SessionID_FirstWins(t *testing.T) {
	var st printState
	handleRunEvent(runEvent{Type: "text", SessionID: "ses_first", Part: []byte(`{"text":"a"}`)}, &st)
	handleRunEvent(runEvent{Type: "text", SessionID: "ses_other", Part: []byte(`{"text":"b"}`)}, &st)

	if got, want := st.sessionID, "ses_first"; got != want {
		t.Errorf("sessionID = %q, want %q (first wins)", got, want)
	}
}

// TestHandleRunEvent_UnknownTypeTolerated — a future opencode
// release adding a new event type must not break the parser.
func TestHandleRunEvent_UnknownTypeTolerated(t *testing.T) {
	var st printState
	// Must not panic, must not flip done.
	handleRunEvent(runEvent{Type: "compaction_finish", SessionID: "ses_x"}, &st)
	if st.done {
		t.Errorf("done = true after unknown type, want false")
	}
}

// TestHandleRunEvent_TextMalformedPartTolerated — a `text`
// event with a non-JSON `part` payload must be silently
// dropped (no panic, no accumulated garbage).
func TestHandleRunEvent_TextMalformedPartTolerated(t *testing.T) {
	var st printState
	handleRunEvent(runEvent{Type: "text", Part: []byte(`not-json`)}, &st)
	if st.text.Len() != 0 {
		t.Errorf("text.Len = %d, want 0 on malformed part", st.text.Len())
	}
	if st.hadContent {
		t.Errorf("hadContent = true on malformed part, want false")
	}
}

// TestRunNDJSON_HappyPath — pipe a multi-event JSONL stream
// through runNDJSON; cb sees each event in order.
func TestRunNDJSON_HappyPath(t *testing.T) {
	input := strings.Join([]string{
		`{"type":"text","sessionID":"ses_1","part":{"text":"hello"}}`,
		`{"type":"step_finish","sessionID":"ses_1"}`,
		``, // empty line must be skipped
		`{"type":"text","sessionID":"ses_1","part":{"text":" world"}}`,
	}, "\n")

	var got []string
	err := runNDJSON(context.Background(), strings.NewReader(input), func(ev runEvent) {
		got = append(got, ev.Type)
	})
	if err != nil {
		t.Fatalf("runNDJSON err = %v", err)
	}
	want := []string{"text", "step_finish", "text"}
	if len(got) != len(want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("events[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestRunNDJSON_MalformedLineSkipped — a bad JSON line is
// logged + skipped, not fatal. This mirrors the long-lived
// bridge's pumpStream permissiveness.
func TestRunNDJSON_MalformedLineSkipped(t *testing.T) {
	input := strings.Join([]string{
		`not-json-at-all`,
		`{"type":"text","part":{"text":"ok"}}`,
	}, "\n")

	var got []string
	err := runNDJSON(context.Background(), strings.NewReader(input), func(ev runEvent) {
		got = append(got, ev.Type)
	})
	if err != nil {
		t.Fatalf("runNDJSON err = %v", err)
	}
	if len(got) != 1 || got[0] != "text" {
		t.Errorf("events = %v, want [text] (malformed line skipped)", got)
	}
}

// TestBuildPrintArgs_Text — basic text-only block produces the
// minimum argv shape AND the prompt as the trailing positional.
// The trailing-positional assertion is the regression guard for
// the F-OPENCODE-PRINT-001 prompt-drop bug; without it the test
// would pass even if `prompt` was never appended.
func TestBuildPrintArgs_Text(t *testing.T) {
	args := buildPrintArgs(
		agent.StartConfig{Workspace: "/tmp/ws"},
		[]agent.ContentBlock{{Type: agent.ContentText, Text: "hi"}},
	)
	want := []string{"run", "--format", "json", "--dir", "/tmp/ws", "hi"}
	if !equalSlice(args, want) {
		t.Errorf("args = %v, want %v (prompt must be trailing positional)", args, want)
	}
}

// TestBuildPrintArgs_ImageFile — image blocks emit `-f <path>`
// AND a `[image: <path> (<mime>)]` placeholder so the model
// sees both the binary AND its position in the message AND
// knows the user intended an image (vs a generic file
// attachment). Mirrors claudecode/pi blocksToPrompt.
func TestBuildPrintArgs_ImageFile(t *testing.T) {
	args := buildPrintArgs(
		agent.StartConfig{Workspace: "/tmp/ws"},
		[]agent.ContentBlock{
			{Type: agent.ContentText, Text: "before"},
			{Type: agent.ContentImage, Path: "/tmp/img.png", MediaType: "image/png"},
			{Type: agent.ContentText, Text: "after"},
		},
	)
	want := []string{
		"run", "--format", "json", "--dir", "/tmp/ws",
		"-f", "/tmp/img.png",
		"before\n[image: /tmp/img.png (image/png)]\nafter",
	}
	if !equalSlice(args, want) {
		t.Errorf("args = %v, want %v", args, want)
	}
}

// TestBuildPrintArgs_ImageWithoutMime — when MediaType is
// empty (rare but possible for callers that pass raw path
// without MIME discovery), the placeholder omits the parens
// rather than emitting "[image: /p ()]" with an empty MIME.
// Both image and file attachments still produce `-f <path>`.
func TestBuildPrintArgs_ImageWithoutMime(t *testing.T) {
	args := buildPrintArgs(
		agent.StartConfig{Workspace: "/tmp/ws"},
		[]agent.ContentBlock{
			{Type: agent.ContentImage, Path: "/tmp/img.png"},
		},
	)
	want := []string{
		"run", "--format", "json", "--dir", "/tmp/ws",
		"-f", "/tmp/img.png",
		"[image: /tmp/img.png]",
	}
	if !equalSlice(args, want) {
		t.Errorf("args = %v, want %v", args, want)
	}
}

// TestBuildPrintArgs_ContentFileSameAsImage — ContentFile
// blocks use the same `-f` mechanism as ContentImage; the
// placeholder format is identical.
func TestBuildPrintArgs_ContentFileSameAsImage(t *testing.T) {
	args := buildPrintArgs(
		agent.StartConfig{Workspace: "/tmp/ws"},
		[]agent.ContentBlock{
			{Type: agent.ContentFile, Path: "/tmp/data.csv"},
		},
	)
	want := []string{
		"run", "--format", "json", "--dir", "/tmp/ws",
		"-f", "/tmp/data.csv",
		"[file: /tmp/data.csv]",
	}
	if !equalSlice(args, want) {
		t.Errorf("args = %v, want %v", args, want)
	}
}

// TestBuildPrintArgs_EmptyPathDropped — image/file blocks
// with empty Path must be skipped entirely (no `-f ""`, no
// placeholder). Empty-text blocks are also dropped.
func TestBuildPrintArgs_EmptyPathDropped(t *testing.T) {
	args := buildPrintArgs(
		agent.StartConfig{Workspace: "/tmp/ws"},
		[]agent.ContentBlock{
			{Type: agent.ContentText, Text: ""},
			{Type: agent.ContentImage, Path: ""},
			{Type: agent.ContentFile, Path: ""},
			{Type: agent.ContentText, Text: "keep"},
		},
	)
	want := []string{"run", "--format", "json", "--dir", "/tmp/ws", "keep"}
	if !equalSlice(args, want) {
		t.Errorf("args = %v, want %v", args, want)
	}
}

// TestBuildPrintArgs_AllImagesFallbackPrompt — when every
// block is an image/file with a Path, the placeholders give
// the prompt non-empty content (so the fallback sentinel
// does NOT fire — the placeholders themselves tell the model
// "see attached files"). The fallback only fires when
// promptParts is empty after all blocks are processed.
func TestBuildPrintArgs_AllImagesFallbackPrompt(t *testing.T) {
	args := buildPrintArgs(
		agent.StartConfig{Workspace: "/tmp/ws"},
		[]agent.ContentBlock{
			{Type: agent.ContentImage, Path: "/tmp/a.png"},
			{Type: agent.ContentImage, Path: "/tmp/b.png"},
		},
	)
	prompt := args[len(args)-1]
	if !strings.Contains(prompt, "[image: /tmp/a.png]") ||
		!strings.Contains(prompt, "[image: /tmp/b.png]") {
		t.Errorf("prompt = %q, want both image placeholders present", prompt)
	}
}

// TestBuildPrintArgs_FallbackFiresWhenTrulyEmpty — the
// "(see attached content)" fallback must fire when promptParts
// is empty after every block was skipped (empty Text, empty
// Path). opencode run treats zero positional args as stdin
// (verified on 1.18.14), which would hang the child.
func TestBuildPrintArgs_FallbackFiresWhenTrulyEmpty(t *testing.T) {
	args := buildPrintArgs(
		agent.StartConfig{Workspace: "/tmp/ws"},
		[]agent.ContentBlock{
			{Type: agent.ContentText, Text: ""},
			{Type: agent.ContentImage, Path: ""},
			{Type: agent.ContentFile, Path: ""},
		},
	)
	if got := args[len(args)-1]; got != "(see attached content)" {
		t.Errorf("trailing positional = %q, want fallback sentinel", got)
	}
}

// TestBuildPrintArgs_MultipleFiles — multiple `-f` flags
// appear in order, mirroring codex's multiple `-i` behavior.
func TestBuildPrintArgs_MultipleFiles(t *testing.T) {
	args := buildPrintArgs(
		agent.StartConfig{Workspace: "/tmp/ws"},
		[]agent.ContentBlock{
			{Type: agent.ContentImage, Path: "/tmp/a.png"},
			{Type: agent.ContentFile, Path: "/tmp/b.txt"},
		},
	)
	// Note: no trailing prompt assertion here because multiple-
	// file blocks produce a placeholder-rich prompt that
	// varies; the next test (TestBuildPrintArgs_PromptIsLast)
// verifies the trailing-positional invariant for the whole
	// suite in one place.
	wantPrefix := []string{
		"run", "--format", "json", "--dir", "/tmp/ws",
		"-f", "/tmp/a.png",
		"-f", "/tmp/b.txt",
	}
	if !equalSlice(args[:len(wantPrefix)], wantPrefix) {
		t.Errorf("args prefix = %v, want %v", args[:len(wantPrefix)], wantPrefix)
	}
	if len(args) != len(wantPrefix)+1 {
		t.Errorf("len(args) = %d, want %d (one trailing positional)", len(args), len(wantPrefix)+1)
	}
}

// TestBuildPrintArgs_PromptIsLast — explicit regression guard
// for the F-OPENCODE-PRINT-001 prompt-drop bug. The previous
// shape `(args, prompt string)` made it easy to forget to
// append prompt at the call site, and tests that only checked
// the args prefix passed silently. This test asserts the
// trailing-positional invariant on every shape of blocks.
func TestBuildPrintArgs_PromptIsLast(t *testing.T) {
	cases := []struct {
		name   string
		blocks []agent.ContentBlock
	}{
		{"empty blocks", nil},
		{"text only", []agent.ContentBlock{{Type: agent.ContentText, Text: "hi"}}},
		{"image only", []agent.ContentBlock{{Type: agent.ContentImage, Path: "/tmp/x.png"}}},
		{"file only", []agent.ContentBlock{{Type: agent.ContentFile, Path: "/tmp/x.csv"}}},
		{"mixed", []agent.ContentBlock{
			{Type: agent.ContentText, Text: "before"},
			{Type: agent.ContentImage, Path: "/tmp/x.png"},
			{Type: agent.ContentFile, Path: "/tmp/x.csv"},
			{Type: agent.ContentText, Text: "after"},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := buildPrintArgs(agent.StartConfig{Workspace: "/tmp/ws"}, tc.blocks)
			if len(args) == 0 {
				t.Fatal("args is empty")
			}
			last := args[len(args)-1]
			if last == "" {
				t.Errorf("trailing positional is empty string; the fallback sentinel was lost")
			}
			// The argv must end with the assembled prompt,
			// never with a flag like -f or --dir.
			if strings.HasPrefix(last, "-") {
				t.Errorf("trailing positional = %q (looks like a flag, not a prompt)", last)
			}
		})
	}
}

// equalSlice is a tiny helper that avoids pulling in
// reflect.DeepEqual's package-level surface for what is
// effectively a string-slice equality check.
func equalSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}