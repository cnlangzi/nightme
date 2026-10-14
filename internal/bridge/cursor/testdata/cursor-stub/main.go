// testdata/cursor-stub — a minimal stub binary that simulates
// cursor's `agent -p` print-mode behavior for testing.
//
// Behavior is controlled by the CURSOR_STUB_BEHAVIOR env var:
//
//   - "happy"     → prints CURSOR_STUB_TEXT (default "READY"), exit 0
//   - "exit_1"    → prints CURSOR_STUB_TEXT to stderr, exit 1
//   - "empty"     → prints nothing, exit 0
//   - "echo"      → prints the last positional arg (the prompt), exit 0
//   - "json"      → prints a single-line cursorResultEnvelope JSON
//                   with CURSOR_STUB_TEXT as the `result` field
//                   and CURSOR_STUB_USAGE as a JSON-encoded usage
//                   object (default: {"inputTokens":100,"outputTokens":20,
//                   "cacheReadTokens":40,"cacheWriteTokens":0}).
//                   Verifies that --output-format json was honored
//                   and that the parser populates RunResult.Usage.
//   - "json_is_error" → same envelope shape as "json" but with
//                   `is_error: true` and `subtype` from
//                   CURSOR_STUB_SUBTYPE (default "error_max_turns").
//                   Verifies that runPrintMode surfaces the failure
//                   as an error rather than a successful body.
//   - "json_with_stderr" → prints the same envelope as "json" to
//                   stdout AND a non-JSON noise line to stderr.
//                   Verifies that the separate stdout / stderr
//                   capture prevents stderr contamination from
//                   breaking the JSON parse (CombinedOutput()
//                   would have concatenated them and the parse
//                   would have fallen back to text mode).
//   - "raw_text"  → prints CURSOR_STUB_TEXT verbatim on stdout,
//                   NOT as JSON. Used to verify the parser's
//                   fallback path keeps working when an old /
//                   third-party cursor binary ignores
//                   --output-format json.
package main

import (
	"fmt"
	"os"
	"strings"
)

const defaultUsage = `{"inputTokens":100,"outputTokens":20,"cacheReadTokens":40,"cacheWriteTokens":0}`

func main() {
	behavior := os.Getenv("CURSOR_STUB_BEHAVIOR")
	text := os.Getenv("CURSOR_STUB_TEXT")
	// No default — each behavior carries its own fallback if it
	// needs text. Letting `text == ""` propagate lets the
	// is_error-empty-body test exercise the "no further detail"
	// fallback path.
	usage := os.Getenv("CURSOR_STUB_USAGE")
	if usage == "" {
		usage = defaultUsage
	}
	subtype := os.Getenv("CURSOR_STUB_SUBTYPE")
	// No default — `json_is_error` callers either set the env
	// explicitly (success-path failure modes) or leave it
	// empty to exercise the "is_error envelope with no
	// subtype" fallback path.

	switch behavior {
	case "happy":
		fmt.Print(text)
	case "exit_1":
		fmt.Fprintln(os.Stderr, text)
		os.Exit(1)
	case "empty":
		// print nothing
	case "echo":
		// print the last positional arg (the prompt)
		args := os.Args[1:]
		// find "-p" and take the next arg
		for i, a := range args {
			if a == "-p" && i+1 < len(args) {
				fmt.Print(args[i+1])
				return
			}
		}
		// fallback: print all args
		fmt.Print(strings.Join(args, " "))
	case "json":
		// Emit a single-line result envelope mirroring what
		// cursor-agent 2026.10.01+ prints under --output-format
		// json. text is JSON-escaped via %q so embedded quotes /
		// newlines don't break the envelope; the stub stays
		// deterministic and easy to assert against.
		fmt.Printf(`{"type":"result","subtype":"success","is_error":false,"result":%q,"session_id":"00000000-0000-0000-0000-000000000001","usage":%s}`,
			text, usage)
	case "json_is_error":
		// Mirrors cursor's failure envelopes — "error_max_turns"
		// and "refusal" both ship with is_error=true plus a
		// prose body in the result field. The parser must
		// surface this as an error so /gtw doesn't treat the
		// failure prose as a successful body.
		fmt.Fprintf(os.Stderr, "")
		fmt.Printf(`{"type":"result","subtype":%q,"is_error":true,"result":%q,"session_id":"00000000-0000-0000-0000-000000000002","usage":%s}`,
			subtype, text, usage)
	case "json_with_stderr":
		// JSON envelope on stdout + a non-JSON noise line on
		// stderr (simulating cursor's update notice / MCP log
		// lines). The parser must succeed on stdout even with
		// stderr noise present — CombinedOutput() would have
		// concatenated them and broken the parse.
		fmt.Fprintf(os.Stderr, "[cursor] update notice: just a heads-up\n")
		fmt.Printf(`{"type":"result","subtype":"success","is_error":false,"result":%q,"session_id":"00000000-0000-0000-0000-000000000003","usage":%s}`,
			text, usage)
	case "raw_text":
		// Pretend to be an older cursor / non-cursor binary
		// that ignored --output-format json. stdout is plain
		// text and the parser must fall back to text mode.
		fmt.Print(text)
	default:
		fmt.Print(text)
	}
}
