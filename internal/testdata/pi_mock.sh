#!/usr/bin/env bash
# Mock pi CLI for nightme's pi bridge integration test. It speaks
# just enough of `pi --mode rpc` to drive a full Start -> SendBlocks
# -> agent_settled -> SendBlocks (second turn) -> Close round trip.
#
# Wire protocol (LF-delimited JSON; see docs/feat/F-32-pi-rpc-bridge.md):
#   - First line on stdin MUST be a get_state command. We reply
#     with a single response carrying session metadata.
#   - Subsequent prompt commands get an immediate response, then a
#     short event stream (text_delta -> text_delta -> tool_execution_start
#     -> tool_execution_end -> message_end -> agent_settled).
#   - On EOF (stdin closed) we exit 0.
#
# This mock is intentionally minimal: it does not implement
# abort, extension_ui_request, or compaction. The F-32 MVP does
# not depend on those paths.

set -eu

PROMPT_COUNT=0
WORKSPACE="${MOCK_PI_WORKSPACE:-/tmp}"

# Reply to a JSON line on stdin. stdout goes to the bridge.
reply_get_state() {
  cat <<EOF
{"id":"boot","type":"response","command":"get_state","success":true,"data":{"model":{"id":"claude-sonnet-4-20250514","name":"Claude Sonnet 4","provider":"anthropic"},"sessionId":"mock-session-1","sessionName":"mock"}}
EOF
}

reply_prompt() {
  local id="$1"
  PROMPT_COUNT=$((PROMPT_COUNT + 1))
  cat <<EOF
{"id":"${id}","type":"response","command":"prompt","success":true}
{"type":"agent_start"}
{"type":"message_start","message":{"role":"assistant"}}
{"type":"message_update","assistantMessageEvent":{"type":"text_start","contentIndex":0,"partial":{"type":"text","text":""}}}
{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"hello "}}
{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"turn ${PROMPT_COUNT}"}}
{"type":"message_update","assistantMessageEvent":{"type":"text_end","contentIndex":0,"content":"hello turn ${PROMPT_COUNT}"}}
{"type":"message_end","message":{"role":"assistant","stopReason":"stop","content":[{"type":"text","text":"hello turn ${PROMPT_COUNT}"}],"usage":{"input":10,"output":5,"cacheRead":0,"cacheWrite":0,"totalTokens":15,"cost":{"input":0.001,"output":0.002,"total":0.003}},"model":"claude-sonnet-4-20250514","provider":"anthropic"}}
{"type":"agent_end","willRetry":false}
{"type":"agent_settled"}
EOF
}

# Read one JSONL line at a time from stdin. The bridge always
# terminates the last line with a newline, so we use read -r to
# preserve the raw bytes.
while IFS= read -r line; do
  # When MOCK_PI_SILENT is set, drop the get_state response so
  # the bridge's handshake timeout fires. Other commands still
  # work normally so the test can still drive prompts.
  case "$line" in
    *'"type":"get_state"'*)
      if [ "${MOCK_PI_SILENT:-0}" = "1" ]; then
        : # intentionally no response; sleep so the bridge
          # sees a real timeout, not an immediate EOF.
        sleep 30
        exit 0
      fi
      reply_get_state
      ;;
    *'"type":"prompt"'*)
      # MOCK_PI_SILENT also covers prompt hang so the legacy
      # handshake-timeout test still finds a quiet child on the
      # second turn. MOCK_PI_PROMPT_HANG is a finer knob for
      # prompt-deadline coverage: it answers get_state normally
      # but silently swallows every prompt, letting the bridge's
      # promptTimeout fire. Hang window must exceed promptTimeout
      # (default 90 s) so the test waits for the deadline, not
      # the script exit. After the hang the script exits so no
      # orphan lingers between test runs.
      if [ "${MOCK_PI_SILENT:-0}" = "1" ]; then
        : # same trick -- never respond to prompts either.
        sleep 30
        exit 0
      fi
      if [ "${MOCK_PI_PROMPT_HANG:-0}" = "1" ]; then
        sleep 300
        exit 0
      fi
      # Extract the id field for the response.
      id=$(printf '%s' "$line" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
      if [ -z "$id" ]; then
        id="req-001"
      fi
      reply_prompt "$id"
      ;;
    *)
      # Unknown command -- send a generic success so the bridge
      # does not treat it as a fatal error.
      cat <<EOF
{"type":"response","command":"unknown","success":true}
EOF
      ;;
  esac
done

# Drain any remaining stderr output (we use it only for debug).
echo "mock pi: stdin EOF, exiting" 1>&2
exit 0
