#!/usr/bin/env bash
# Mock pi CLI for nightme's pi bridge print-mode tests.
#
# Speaks just enough of `pi --mode json -p <prompt>` to drive
# runPrintMode: a session envelope, agent_start, a short text
# stream, message_end, agent_settled, then exit 0. Tunable via
# env vars so each test path can drive the failure mode it
# wants without forking this script.
#
# Wire protocol (LF-delimited JSON, one event per stdout line):
#
#   - Always emits `{"type":"session",...}` first (mirrors real
#     pi's behaviour at print-mode startup; the bridge's
#     translator drops it via the default case).
#   - Then agent_start → turn_start → message_start(user) →
#     message_end(user) → message_start(assistant) → text_start →
#     text_delta → text_delta → text_end → message_end(assistant) →
#     agent_settled.
#   - On a clean run, exits 0.
#
# Env-var knobs:
#   PI_PRINT_TEXT     — text to emit (default: "hello").
#   PI_PRINT_FAIL      — if "1", skip agent_settled and exit 1
#                        (tests the "exit without settled" path).
#   PI_PRINT_NO_SETTLE — if "1", emit a clean stream but no
#                        agent_settled (tests the same path
#                        without non-zero exit).
#   PI_PRINT_STDERR    — if non-empty, written to stderr before
#                        exit (tests stderr surfacing on
#                        non-zero exit).

set -eu

TEXT="${PI_PRINT_TEXT:-hello}"
PROMPT="${*:-$TEXT}"  # argv after script name is the prompt

emit_event() {
  printf '%s\n' "$1"
}

emit_session() {
  emit_event "{\"type\":\"session\",\"version\":3,\"id\":\"mock-print-session\",\"timestamp\":\"2026-01-01T00:00:00.000Z\",\"cwd\":\"$(pwd)\"}"
}

emit_agent_start() {
  emit_event '{"type":"agent_start"}'
  emit_event '{"type":"turn_start"}'
}

emit_user_msg() {
  # Mirror what pi does for the user prompt: message_start + message_end.
  emit_event "{\"type\":\"message_start\",\"message\":{\"role\":\"user\",\"content\":[{\"type\":\"text\",\"text\":\"$1\"}],\"timestamp\":1}}"
  emit_event "{\"type\":\"message_end\",\"message\":{\"role\":\"user\",\"content\":[{\"type\":\"text\",\"text\":\"$1\"}],\"timestamp\":1}}"
}

emit_assistant_msg() {
  local text="$1"
  emit_event "{\"type\":\"message_start\",\"message\":{\"role\":\"assistant\",\"content\":[{\"type\":\"text\",\"text\":\"\"}],\"api\":\"mock\",\"provider\":\"mock\",\"model\":\"mock\",\"usage\":{\"input\":0,\"output\":0,\"cacheRead\":0,\"cacheWrite\":0,\"totalTokens\":0,\"cost\":{\"input\":0,\"output\":0,\"cacheRead\":0,\"cacheWrite\":0,\"total\":0}},\"stopReason\":\"pending\",\"timestamp\":2,\"responseId\":\"mock-r1\"}}"
  emit_event "{\"type\":\"message_update\",\"assistantMessageEvent\":{\"type\":\"text_start\",\"contentIndex\":0,\"partial\":{\"role\":\"assistant\",\"content\":[{\"type\":\"text\",\"text\":\"\"}],\"api\":\"mock\",\"provider\":\"mock\",\"model\":\"mock\",\"usage\":{\"input\":0,\"output\":0,\"cacheRead\":0,\"cacheWrite\":0,\"totalTokens\":0,\"cost\":{\"input\":0,\"output\":0,\"cacheRead\":0,\"cacheWrite\":0,\"total\":0}},\"stopReason\":\"pending\",\"timestamp\":2,\"responseId\":\"mock-r1\"}},\"message\":{\"role\":\"assistant\",\"content\":[{\"type\":\"text\",\"text\":\"\"}]}}"
  emit_event "{\"type\":\"message_update\",\"assistantMessageEvent\":{\"type\":\"text_delta\",\"contentIndex\":0,\"delta\":\"$text\",\"partial\":{\"role\":\"assistant\",\"content\":[{\"type\":\"text\",\"text\":\"$text\"}]},\"api\":\"mock\",\"provider\":\"mock\",\"model\":\"mock\",\"usage\":{\"input\":0,\"output\":0,\"cacheRead\":0,\"cacheWrite\":0,\"totalTokens\":0,\"cost\":{\"input\":0,\"output\":0,\"cacheRead\":0,\"cacheWrite\":0,\"total\":0}},\"stopReason\":\"pending\",\"timestamp\":2,\"responseId\":\"mock-r1\"},\"message\":{\"role\":\"assistant\",\"content\":[{\"type\":\"text\",\"text\":\"$text\"}]}}"
  emit_event "{\"type\":\"message_update\",\"assistantMessageEvent\":{\"type\":\"text_end\",\"contentIndex\":0,\"content\":\"$text\",\"partial\":{\"role\":\"assistant\",\"content\":[{\"type\":\"text\",\"text\":\"$text\"}],\"api\":\"mock\",\"provider\":\"mock\",\"model\":\"mock\",\"usage\":{\"input\":0,\"output\":0,\"cacheRead\":0,\"cacheWrite\":0,\"totalTokens\":0,\"cost\":{\"input\":0,\"output\":0,\"cacheRead\":0,\"cacheWrite\":0,\"total\":0}},\"stopReason\":\"stop\",\"timestamp\":2,\"responseId\":\"mock-r1\"}},\"message\":{\"role\":\"assistant\",\"content\":[{\"type\":\"text\",\"text\":\"$text\"}],\"stopReason\":\"stop\"}}"
  emit_event "{\"type\":\"message_end\",\"message\":{\"role\":\"assistant\",\"stopReason\":\"stop\",\"content\":[{\"type\":\"text\",\"text\":\"$text\"}],\"usage\":{\"input\":1,\"output\":1,\"cacheRead\":0,\"cacheWrite\":0,\"totalTokens\":2,\"cost\":{\"input\":0,\"output\":0,\"total\":0}},\"model\":\"mock\",\"provider\":\"mock\"}}"
}

emit_settled() {
  emit_event '{"type":"agent_settled"}'
}

# Mode-specific behaviours.
if [ "${PI_PRINT_FAIL:-0}" = "1" ]; then
  # Mirror claudecode/print_mock.sh: emit env dump + large
  # stderr BEFORE exit 1, so the bridge's stderr goroutine
  # captures them and the wrapped error contains both.
  if [ -n "${PI_PRINT_DUMP_ENV:-}" ]; then
    for v in $(env | grep ^NIGHTME_TEST_ | sort); do
      printf '%s\n' "$v" >&2
    done
  fi
  if [ -n "${PI_PRINT_LARGE_STDERR:-}" ]; then
    N="${PI_PRINT_LARGE_STDERR}"
    i=0
    while [ "$i" -lt 10000 ]; do
      printf 's%06d.pi_stderr_marker_padding\n' "$i" >&2
      i=$((i+1))
      [ "$i" -gt "$((N / 25 + 1))" ] && break
    done
  fi
  if [ -n "${PI_PRINT_STDERR:-}" ]; then
    printf '%s\n' "$PI_PRINT_STDERR" >&2
  fi
  emit_session
  emit_agent_start
  emit_user_msg "$PROMPT"
  emit_assistant_msg "partial"
  exit 1
fi

if [ "${PI_PRINT_NO_SETTLE:-0}" = "1" ]; then
  emit_session
  emit_agent_start
  emit_user_msg "$PROMPT"
  # If NO_SETTLE_USAGE is set, emit a usage-bearing message
  # before exiting. Lets the bridge's audit-field appender
  # test (TestPrintMode_Mock_NoSettled_PreservesUsage) lock
  # in that captured usage survives the no-settled error
  # path. Usage values match the claudecode mock so the
  # expected substrings ("1234", "128") are stable across
  # both bridges.
  if [ -n "${PI_PRINT_NO_SETTLE_USAGE:-}" ]; then
    emit_event "{\"type\":\"message_end\",\"message\":{\"role\":\"assistant\",\"stopReason\":\"stop\",\"content\":[{\"type\":\"text\",\"text\":\"$TEXT\"}],\"usage\":{\"input\":1234,\"output\":56,\"cacheRead\":128,\"cacheWrite\":0,\"totalTokens\":1418,\"cost\":{\"input\":0,\"output\":0,\"cacheRead\":0,\"cacheWrite\":0,\"total\":0}},\"model\":\"mock\",\"provider\":\"mock\"}}"
  else
    emit_assistant_msg "$TEXT"
  fi
  exit 0
fi

# Default: full clean run.
emit_session
emit_agent_start
emit_user_msg "$PROMPT"
emit_assistant_msg "$TEXT"
emit_settled
exit 0

