// print_ndjson_test.go — direct unit tests for runNDJSON.
//
// runNDJSON is the NDJSON event parser that drives RunResult's
// SessionID + Usage fields. Without these tests the parsing
// layer was only covered via e2e (TestRunPrintMode_HappyPath)
// which exercises only the happy path — schema breakage or
// malformed-line tolerance wouldn't surface until codex CLI
// changed its wire format.

//go:build !windows

package codex

import (
	"context"
	"strings"
	"testing"
)

// TestRunNDJSON_ParsesExpectedEvents — feeds a fixture that
// mirrors the real codex 0.145.0 NDJSON stream and verifies
// each event is delivered to the callback with the right
// fields extracted.
func TestRunNDJSON_ParsesExpectedEvents(t *testing.T) {
	in := strings.NewReader(strings.Join([]string{
		`{"type":"thread.started","thread_id":"thr-abc"}`,
		`{"type":"turn.started"}`,
		`{"type":"item.completed","item":{"id":"i1","type":"agent_message","text":"hello"}}`,
		`{"type":"turn.completed","usage":{"input_tokens":100,"cached_input_tokens":5,"output_tokens":20}}`,
		``,
		`{"type":"thread.started","thread_id":"thr-ignore-me"}`, // duplicate: should not overwrite
	}, "\n"))

	var got []codexExecEvent
	err := runNDJSON(context.Background(), in, func(ev codexExecEvent) {
		got = append(got, ev)
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}

	// We expect 5 callbacks (empty line skipped):
	//   thread.started (thr-abc)
	//   turn.started
	//   item.completed (text=hello)
	//   turn.completed (usage 100/5/20)
	//   thread.started (thr-ignore-me — duplicate, first wins in
	//     real runPrintMode, but runNDJSON itself emits both)
	if len(got) != 5 {
		t.Fatalf("got %d events, want 5: %+v", len(got), got)
	}
	if got[0].Type != "thread.started" || got[0].ThreadID != "thr-abc" {
		t.Errorf("event[0] = %+v, want thread.started thr-abc", got[0])
	}
	if got[3].Usage == nil || got[3].Usage.InputTokens != 100 ||
		got[3].Usage.CachedInputTokens != 5 || got[3].Usage.OutputTokens != 20 {
		t.Errorf("event[3].Usage = %+v, want 100/5/20", got[3].Usage)
	}
}

// TestRunNDJSON_SkipsMalformedLines — invalid JSON lines must
// be logged + skipped, not abort the run (mirrors pumpStream's
// permissiveness in the long-lived bridge).
func TestRunNDJSON_SkipsMalformedLines(t *testing.T) {
	in := strings.NewReader(strings.Join([]string{
		`{"type":"thread.started","thread_id":"good-1"}`,
		`{this is not valid json`,
		`{"type":"thread.started","thread_id":"good-2"}`,
		`{"truncated`,
	}, "\n"))

	var got []string
	err := runNDJSON(context.Background(), in, func(ev codexExecEvent) {
		if ev.ThreadID != "" {
			got = append(got, ev.ThreadID)
		}
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	want := []string{"good-1", "good-2"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestRunNDJSON_HandlesEmptyInput — EOF with no events is not
// an error.
func TestRunNDJSON_HandlesEmptyInput(t *testing.T) {
	in := strings.NewReader("")
	called := 0
	err := runNDJSON(context.Background(), in, func(codexExecEvent) {
		called++
	})
	if err != nil {
		t.Errorf("err = %v, want nil", err)
	}
	if called != 0 {
		t.Errorf("called = %d, want 0", called)
	}
}

// TestRunNDJSON_HandlesLongLine — a line exceeding the default
// bufio.Scanner buffer must not silently truncate; verify the
// scanner's max-buffer expansion kicks in. We feed a 200 KiB
// JSON line (well above the 64 KiB initial buffer but below
// the 1 MiB max).
func TestRunNDJSON_HandlesLongLine(t *testing.T) {
	longText := strings.Repeat("x", 200*1024)
	in := strings.NewReader(`{"type":"item.completed","item":{"id":"i1","type":"agent_message","text":"` + longText + `"}}` + "\n")

	var got codexExecEvent
	err := runNDJSON(context.Background(), in, func(ev codexExecEvent) {
		if ev.Type == "item.completed" {
			got = ev
		}
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got.Type != "item.completed" {
		t.Fatalf("event type = %q, want item.completed", got.Type)
	}
}

// TestRunNDJSON_HandlesEOFAfterPartialEvents — runNDJSON must
// return cleanly when the underlying reader hits EOF mid-stream
// (e.g., codex exits after emitting only thread.started but
// before turn.completed). The function should NOT return an
// error — EOF is normal termination, not a parse failure.
func TestRunNDJSON_HandlesEOFAfterPartialEvents(t *testing.T) {
	in := strings.NewReader(`{"type":"thread.started","thread_id":"only-one"}` + "\n")

	var got codexExecEvent
	err := runNDJSON(context.Background(), in, func(ev codexExecEvent) {
		got = ev
	})
	if err != nil {
		t.Errorf("err = %v, want nil (EOF is not an error)", err)
	}
	if got.ThreadID != "only-one" {
		t.Errorf("ThreadID = %q, want only-one", got.ThreadID)
	}
}

// Note: a direct "ctx cancellation interrupts runNDJSON"
// test was considered but is not implementable —
// bufio.Scanner.Scan() blocks on the underlying Read, so the
// ctx.Err() check inside the loop is only reached AFTER a
// line is consumed. In production, ctx cancellation is
// handled by exec.CommandContext killing the child, which
// closes the stdout pipe and produces EOF (Scanner returns
// false). The ctx check is defensive code that handles the
// microsecond window between consuming one event and the
// next Scan() blocking — not worth a dedicated test.