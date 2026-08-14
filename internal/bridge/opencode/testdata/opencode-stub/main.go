// opencode-stub: minimal stand-in for `opencode run --format json`
// used by print_stub_test.go when no real opencode binary is
// available. Reads behavior from env vars so a single binary
// can simulate several test scenarios without recompilation.
//
// Cross-platform: only uses stdlib (fmt / os) and emits one JSON
// event per `\n`-terminated stdout line. Windows consumers read
// the same bytes via bufio.Scanner, which handles `\n` and CRLF
// uniformly. The test harness (print_stub_test.go) appends `.exe`
// to the build target on Windows so the binary can be invoked
// via cmd.NewCmd's Windows shim wrapper.
//
// Argv-aware: this stub reads os.Args[1] and uses the LAST
// element as the model input. The "echo" / "echo_image" /
// "exit_1" behaviors include that prompt verbatim in their
// response so a regression that drops the positional prompt
// at the spawn site would produce a different stub output
// and fail the integration test. This is the regression guard
// for F-OPENCODE-PRINT-001.
//
// Env vars consumed:
//
//	OPENCODE_STUB_BEHAVIOR  (required)
//	  "happy"             — emit text + step_finish, exit 0
//	                       (text = OPENCODE_STUB_TEXT, default "READY")
//	  "echo"              — emit text containing the trailing
//	                       positional prompt, exit 0. Locks the
//	                       "prompt was actually forwarded to the
//	                       child" invariant.
//	  "echo_image"        — like "echo" but also verifies the
//	                       presence of a `-f` flag in argv
//	                       before emitting the response. Locks
//	                       the "-f flag was forwarded" invariant.
//	  "reasoning_only"    — emit reasoning + step_finish with
//	                       NO text event. Locks the hadContent-
//	                       based empty-answer guard (a stream
//	                       with reasoning must NOT trip
//	                       "opencode: empty answer").
//	  "model_error"       — emit text (from OPENCODE_STUB_TEXT) THEN
//	                       error event + step_finish, exit 0.
//	  "empty"             — emit only step_finish (no text), exit 0
//	  "exit_1"            — emit step_finish + write "boom" to stderr, exit 1
//
//	OPENCODE_STUB_TEXT     (optional)
//	  Text to emit (any behavior that includes text). Defaults to
//	  "READY" for happy, "partial" for model_error.
package main

import (
	"fmt"
	"os"
	"strings"
)

func main() {
	behavior := os.Getenv("OPENCODE_STUB_BEHAVIOR")
	text := os.Getenv("OPENCODE_STUB_TEXT")
	if text == "" {
		text = "READY"
	}

	// argv-aware: os.Args[0] is the binary itself, os.Args[1]
	// is the subcommand ("run"), and the LAST element is the
	// positional message (verified against the actual opencode
	// CLI shape: "opencode run --format json --dir <ws> [-f
	// <path>]... <message>"). We use the last element for
	// argv-aware behaviors ("echo" / "echo_image") and -1 (no
	// match) for the rest.
	promptFromArgv := ""
	for i, a := range os.Args {
		if i == len(os.Args)-1 {
			promptFromArgv = a
		}
	}

	emit := func(typ string, part string) {
		if part == "" {
			fmt.Printf(`{"type":%q,"timestamp":1,"sessionID":"ses_stub"}`+"\n", typ)
			return
		}
		fmt.Printf(`{"type":%q,"timestamp":1,"sessionID":"ses_stub","part":%s}`+"\n", typ, part)
	}

	// emitError mirrors emit but writes the payload into the
	// `error` field instead of `part`. The wire shape differs
	// by event type per run.ts source.
	emitError := func(payload string) {
		fmt.Printf(`{"type":"error","timestamp":1,"sessionID":"ses_stub","error":%s}`+"\n", payload)
	}

	switch behavior {
	case "happy":
		emit("text", fmt.Sprintf(`{"text":%q}`, text))
		emit("step_finish", "")
	case "echo":
		// The text response embeds the trailing positional
		// prompt verbatim. Tests assert result.Text contains
		// the prompt they passed in via cfg.Blocks — a
		// regression that drops the prompt from argv produces
		// text != prompt and fails the test.
		// The prompt may contain real newlines (e.g.
		// "describe this image\n[image: ...]"); we JSON-escape
		// before emitting so the wire stays valid.
		emit("text", fmt.Sprintf(`{"text":"echo:%s"}`, jsonEscape(promptFromArgv)))
		emit("step_finish", "")
	case "echo_image":
		// Verify both the prompt AND a `-f` flag are present
		// in argv before responding. Without `-f` (i.e.
		// buildPrintArgs forgot the flag for image blocks)
		// the response is an error and the test fails.
		hasFFlag := false
		for _, a := range os.Args {
			if a == "-f" {
				hasFFlag = true
				break
			}
		}
		if !hasFFlag {
			fmt.Fprintln(os.Stderr, "echo_image: missing -f flag in argv")
			os.Exit(1)
		}
		emit("text", fmt.Sprintf(`{"text":"echo:%s"}`, jsonEscape(promptFromArgv)))
		emit("step_finish", "")
	case "reasoning_only":
		// No `text` event — only reasoning + step_finish.
		// Tests assert result.Text is empty (reasoning is
		// dropped by the bridge) but the call SUCCEEDS
		// because hadContent was set on the reasoning event.
		// This is the regression guard for the empty-answer
		// guard's switch from text.Len()==0 to !hadContent.
		emit("reasoning", `{"text":"thinking..."}`)
		emit("step_finish", "")
	case "model_error":
		// The text-then-error-then-step_finish shape exercises
		// the F-OPENCODE-PRINT-001 fix that preserves partial
		// text alongside the wire-level error event so the
		// caller sees both. Real opencode can produce this shape
		// when the model streams some bytes then the API errors
		// mid-turn (rate limit, network drop, etc.).
		if text == "" || text == "READY" {
			text = "partial"
		}
		emit("text", fmt.Sprintf(`{"text":%q}`, text))
		emitError(`"rate limited"`)
		emit("step_finish", "")
	case "empty":
		emit("step_finish", "")
	case "exit_1":
		emit("text", fmt.Sprintf(`{"text":%q}`, text))
		emit("step_finish", "")
		fmt.Fprintln(os.Stderr, "boom: synthetic CLI failure")
		os.Exit(1)
	default:
		fmt.Fprintf(os.Stderr, "opencode-stub: unknown OPENCODE_STUB_BEHAVIOR=%q\n", behavior)
		os.Exit(2)
	}

	_ = strings.Builder{} // placeholder if we later need string ops
}

// jsonEscape replaces characters that would break a
// single-line JSON wire format (newline, carriage return, tab,
// backslash, double quote) with their JSON escape sequences.
// Used by the "echo" / "echo_image" behaviors so the embedded
// prompt survives transit through bufio.Scanner's line-based
// parser on the consumer side. Real opencode emits JSON via
// JSON.stringify which handles this automatically; the stub
// builds strings with %s so it has to do the escaping itself.
func jsonEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}