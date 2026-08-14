#!/usr/bin/env bash
# Mock Claude Code CLI for nightme's claudecode bridge print-mode tests.
#
# Speaks just enough of `claude -p <prompt> --output-format stream-json
# --verbose --permission-mode ...` to drive runPrintMode without depending
# on the real `claude` binary. Tunable via env vars so each test path
# can drive the failure mode it wants without forking this script.
#
# Wire protocol (LF-delimited JSON, one event per stdout line) — matches
# what real `claude --output-format stream-json --verbose -p` emits:
#
#   - system/init (with session_id, model)         — first event
#   - assistant (with content[].text "...")        — model reply
#   - result   (with result "..." / is_error)      — terminal marker
#
# Env-var knobs:
#   CLAUDE_PRINT_TEXT     — assistant text + result.result (default: "hello")
#   CLAUDE_PRINT_FAIL     — if "1", skip the result event and exit 1
#                           (tests the "exit without result" path).
#   CLAUDE_PRINT_NO_RESULT — if "1", emit init + assistant but NO result
#                           (tests the same path without non-zero exit).
#   CLAUDE_PRINT_STDERR   — if non-empty, written to stderr before exit
#                           (tests stderr surfacing on non-zero exit).
#   CLAUDE_PRINT_IS_ERROR  — if "1", emit a result event with
#                            is_error=true and exit 0 (tests the
#                            "result.is_error → wrap as error" path).
#   CLAUDE_PRINT_MODEL     — model name to emit on system/init
#                            (default: "claude-mock-1"). Lets tests
#                            verify RunResult.Model capture.
#   CLAUDE_PRINT_USAGE      — if "1", include a usage block on the
#                            result event (input=1234, output=56,
#                            cache_read=128, cost_usd=0.0021).
#                            Lets tests verify RunResult.Usage capture.
#   CLAUDE_PRINT_DUMP_ENV   — if non-empty, every NIGHTME_TEST_*
#                            env var is echoed to stderr (one
#                            KEY=VALUE per line) before the first
#                            protocol event fires. Lets tests verify
#                            cfg.Env is forwarded to the child.
#   CLAUDE_PRINT_LARGE_STDERR — if set to a byte count N, emit
#                            approximately N bytes of fake stderr
#                            (repeating marker lines) before the
#                            terminal result. Used by the
#                            stderrCapBytes test to verify the
#                            bridge caps stderr at 64 KiB (rather
#                            than OOMing on a chatty failing
#                            child).

set -eu

TEXT="${CLAUDE_PRINT_TEXT:-hello}"

# CLAUDE_PRINT_DUMP_ENV — echo every NIGHTME_TEST_* env var to
# stderr. Runs FIRST so the dump is observed regardless of
# which path the test triggers (clean / fail / no-result /
# is-error). Catches the regression where cfg.Env was
# silently dropped on the print-mode path.
if [ -n "${CLAUDE_PRINT_DUMP_ENV:-}" ]; then
  for v in $(env | grep ^NIGHTME_TEST_ | sort); do
    printf '%s\n' "$v" >&2
  done
fi

emit() {
  printf '%s\n' "$1"
}

emit_init() {
  local model="${CLAUDE_PRINT_MODEL:-claude-mock-1}"
  emit "{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":\"mock-print-session\",\"model\":\"$model\",\"cwd\":\"/tmp\"}"
}

emit_assistant() {
  local text="$1"
  local model="${CLAUDE_PRINT_MODEL:-claude-mock-1}"
  # Escape text for JSON (handle backslashes and double quotes).
  local esc
  esc=$(printf '%s' "$text" | sed 's/\\/\\\\/g; s/"/\\"/g')
  emit "{\"type\":\"assistant\",\"message\":{\"id\":\"msg-mock-1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"$model\",\"content\":[{\"type\":\"text\",\"text\":\"$esc\"}],\"stop_reason\":\"end_turn\"}}"
}

# usageBlock returns either an empty string (CLAUDE_PRINT_USAGE!=1)
# or a JSON object literal representing result.usage + modelUsage.
# Matches Claude Code's wire shape so decodeUsage can populate
# *agent.UsageInfo end-to-end.
usage_block() {
  if [ "${CLAUDE_PRINT_USAGE:-0}" != "1" ]; then
    printf ''
    return
  fi
  local model="${CLAUDE_PRINT_MODEL:-claude-mock-1}"
  printf ',"usage":{"input_tokens":1234,"output_tokens":56,"cache_read_input_tokens":128,"cache_creation_input_tokens":0},"modelUsage":{"%s":{"costUSD":0.0021,"contextWindow":200000,"inputTokens":1234,"outputTokens":56}}' "$model"
}

emit_result() {
  local text="$1"
  local esc
  esc=$(printf '%s' "$text" | sed 's/\\/\\\\/g; s/"/\\"/g')
  local usage
  usage=$(usage_block)
  emit "{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"duration_ms\":42,\"duration_api_ms\":40,\"num_turns\":1,\"result\":\"$esc\",\"session_id\":\"mock-print-session\"${usage}}"
}

emit_error_result() {
  local text="$1"
  local esc
  esc=$(printf '%s' "$text" | sed 's/\\/\\\\/g; s/"/\\"/g')
  local usage
  usage=$(usage_block)
  emit "{\"type\":\"result\",\"subtype\":\"error_max_turns\",\"is_error\":true,\"duration_ms\":42,\"duration_api_ms\":40,\"num_turns\":1,\"result\":\"$esc\",\"session_id\":\"mock-print-session\"${usage}}"
}

# Default: full clean run. Order matters — large-stderr dump
# runs early (after the cfg.Env dump) so the bridge's stderr
# goroutine has read everything by the time the bridge waits
# for exit. Tests reading stderr via the wrapped error message
# see the full dump.
emit_init
emit_assistant "$TEXT"

# CLAUDE_PRINT_LARGE_STDERR — emit ~N bytes of fake stderr
# (repeating marker lines) before the terminal result. Used by
# the stderrCapBytes test to verify the bridge caps stderr at
# 64 KiB (rather than OOMing on a chatty failing child).
if [ -n "${CLAUDE_PRINT_LARGE_STDERR:-}" ]; then
  N="${CLAUDE_PRINT_LARGE_STDERR}"
  # emit 25-byte marker lines until total >= N
  i=0
  while [ "$i" -lt 10000 ]; do
    printf 's%06d.stderr_marker_padding\n' "$i" >&2
    i=$((i+1))
    [ "$i" -gt "$((N / 25 + 1))" ] && break
  done
  # short-circuit: emit a result event that is_error so the
  # bridge surfaces stderr via the wrapped error.
  emit_error_result "$TEXT"
  exit 0
fi

if [ "${CLAUDE_PRINT_FAIL:-0}" = "1" ]; then
  if [ -n "${CLAUDE_PRINT_STDERR:-}" ]; then
    printf '%s\n' "$CLAUDE_PRINT_STDERR" >&2
  fi
  exit 1
fi
if [ "${CLAUDE_PRINT_NO_RESULT:-0}" = "1" ]; then
  exit 0
fi
if [ "${CLAUDE_PRINT_IS_ERROR:-0}" = "1" ]; then
  emit_error_result "$TEXT"
  exit 0
fi
emit_result "$TEXT"
exit 0
