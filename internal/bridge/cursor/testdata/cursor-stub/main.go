// testdata/cursor-stub — a minimal stub binary that simulates
// cursor's `agent -p` print-mode behavior for testing.
//
// Behavior is controlled by the CURSOR_STUB_BEHAVIOR env var:
//
//   - "exit_1"         → prints CURSOR_STUB_TEXT to stderr, exit 1
//   - "json"           → prints a single-line cursorResultEnvelope JSON
//                       with CURSOR_STUB_TEXT as the `result` field
//                       and CURSOR_STUB_USAGE as a JSON-encoded usage
//                       object (default: {"inputTokens":100,
//                       "outputTokens":20,"cacheReadTokens":40,
//                       "cacheWriteTokens":0}).
//   - "json_is_error"  → same envelope shape as "json" but with
//                       `is_error: true` and `subtype` from
//                       CURSOR_STUB_SUBTYPE (verbatim — no default).
//   - "json_with_stderr" → same envelope as "json" to stdout AND
//                       a non-JSON noise line to stderr.
//   - "json_empty_result" → valid envelope with empty `result`
//                       field — covers the empty-answer guard.
//   - "malformed_json" → emits a non-JSON line (e.g. "{not json}")
//                       to stdout — covers the hard parse-error
//                       path; runPrintMode must surface this as
//                       an error rather than silently fall back
//                       to text mode (which was a real bug before
//                       the JSON-only contract).
//   - "json_unknown_type" → valid JSON but `type != "result"` —
//                       covers the schema-drift guard.
package main

import (
	"fmt"
	"os"
)

const defaultUsage = `{"inputTokens":100,"outputTokens":20,"cacheReadTokens":40,"cacheWriteTokens":0}`

func main() {
	behavior := os.Getenv("CURSOR_STUB_BEHAVIOR")
	text := os.Getenv("CURSOR_STUB_TEXT")
	// No default text — let each behavior control its own.
	// Empty text is a meaningful test input for some cases
	// (json_empty_result, json_is_error empty-body).
	usage := os.Getenv("CURSOR_STUB_USAGE")
	if usage == "" {
		usage = defaultUsage
	}
	subtype := os.Getenv("CURSOR_STUB_SUBTYPE")
	// No default subtype — json_is_error callers either set
	// it explicitly (success-path failure modes) or leave it
	// empty to exercise the "is_error envelope with no subtype"
	// fallback path.

	switch behavior {
	case "exit_1":
		fmt.Fprintln(os.Stderr, text)
		os.Exit(1)
	case "json":
		fmt.Printf(`{"type":"result","subtype":"success","is_error":false,"result":%q,"session_id":"00000000-0000-0000-0000-000000000001","usage":%s}`,
			text, usage)
	case "json_is_error":
		fmt.Printf(`{"type":"result","subtype":%q,"is_error":true,"result":%q,"session_id":"00000000-0000-0000-0000-000000000002","usage":%s}`,
			subtype, text, usage)
	case "json_with_stderr":
		fmt.Fprintf(os.Stderr, "[cursor] update notice: just a heads-up\n")
		fmt.Printf(`{"type":"result","subtype":"success","is_error":false,"result":%q,"session_id":"00000000-0000-0000-0000-000000000003","usage":%s}`,
			text, usage)
	case "json_empty_result":
		// Valid envelope, empty result. Hits the empty-answer
		// guard (NOT the is_error branch).
		fmt.Printf(`{"type":"result","subtype":"success","is_error":false,"result":"","session_id":"00000000-0000-0000-0000-000000000004","usage":%s}`, usage)
	case "malformed_json":
		// Non-JSON stdout. runPrintMode must surface as a
		// parse error (cursor schema drift, binary not
		// honouring --output-format json, etc.) — NOT fall
		// back to text mode.
		fmt.Fprint(os.Stdout, "{not valid json")
	case "json_unknown_type":
		// Valid JSON but envelope type is something other
		// than "result" (cursor shipped a future envelope
		// shape we don't know about).
		fmt.Fprint(os.Stdout, `{"type":"future_envelope","payload":"unknown"}`)
	default:
		// Unknown behavior — emit a malformed line so the
		// test harness notices it forgot to set CURSOR_STUB_BEHAVIOR.
		fmt.Fprint(os.Stderr, "cursor-stub: unknown CURSOR_STUB_BEHAVIOR="+behavior+"\n")
		os.Exit(2)
	}
}
