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
	if text == "" {
		text = "READY"
	}
	usage := os.Getenv("CURSOR_STUB_USAGE")
	if usage == "" {
		usage = defaultUsage
	}

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
		fmt.Printf(`{"type":"result","subtype":"success","is_error":false,"duration_ms":42,"duration_api_ms":42,"result":%q,"session_id":"00000000-0000-0000-0000-000000000001","request_id":"00000000-0000-0000-0000-000000000002","usage":%s}`,
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
