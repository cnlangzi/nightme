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

# mock_mode echoes the current mode string ("all" or "abort-only")
# for the dispatch loop to consult per-line. When
# MOCK_PI_CONTROL_FILE points to a file, the script reads the
# first line of that file on every dispatch iteration so tests
# can flip the mock between normal-prompt and abort-only modes
# mid-run. Env vars don't propagate to a running subprocess, so
# the file is the only way to change behaviour after Start.
mock_mode() {
  if [ -z "${MOCK_PI_CONTROL_FILE:-}" ]; then
    if [ "${MOCK_PI_ABORT_ONLY:-0}" = "1" ]; then
      echo "abort-only"
    else
      echo "all"
    fi
    return
  fi
  if [ -f "${MOCK_PI_CONTROL_FILE}" ]; then
    head -n1 "${MOCK_PI_CONTROL_FILE}" | tr -d '[:space:]'
    return
  fi
  echo "all"
}

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

# reply_abort answers the bridge's abort RPC. In the well-behaved
# default the mock also emits agent_settled so the chat layer's
# IsReady flips via the normal path. MOCK_PI_ABORT_NO_SETTLED=1
# disables that emission, simulating a pi build that ACKs abort
# but does not emit agent_settled — this is the regression mode
# for the original "stop leaves turnActive stuck" bug, and the
# fix-pi-stop tests rely on both shapes.
reply_abort() {
  local id="$1"
  cat <<EOF
{"id":"${id}","type":"response","command":"abort","success":true}
EOF
  if [ "${MOCK_PI_ABORT_NO_SETTLED:-0}" = "1" ]; then
    : # intentionally no agent_settled — silence simulates a
      # pi build that ACKs abort but doesn't settle the turn.
    return
  fi
  cat <<EOF
{"type":"agent_settled"}
EOF
}

# Read one JSONL line at a time from stdin. The bridge always
# terminates the last line with a newline, so we use read -r to
# preserve the raw bytes.
while IFS= read -r line; do
  # Read the current mode each iteration so the file-driven
  # toggle flips the mock between "all" and "abort-only" mid-
  # test. Abort-only: only get_state + abort get answered.
  current_mode=$(mock_mode)
  if [ "$current_mode" = "abort-only" ]; then
    case "$line" in
      *'"type":"get_state"'*) reply_get_state ;;
      *'"type":"abort"'*)
        id=$(printf '%s' "$line" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
        if [ -z "$id" ]; then id="abort-1"; fi
        reply_abort "$id"
        ;;
      # Every other command (prompt, etc.) is silently dropped.
    esac
    continue
  fi

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
    *'"type":"abort"'*)
      # Stop the in-flight turn. Default behaviour: ack abort
      # and emit agent_settled (matches pi's documented wire
      # contract). MOCK_PI_ABORT_NO_SETTLED=1 suppresses the
      # settled event so the test can exercise the
      # "pi-silent-after-abort" branch.
      id=$(printf '%s' "$line" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
      if [ -z "$id" ]; then
        id="abort-1"
      fi
      reply_abort "$id"
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
