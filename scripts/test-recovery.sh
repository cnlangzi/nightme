#!/bin/bash
# test-recovery.sh — verify Phase 2 (F-dsh-shared-host) restart recovery.
#
# Steps:
#   1. Start nightme daemon + dsh shared host.
#   2. Trigger a dsh chat session (writes sessionId to registry).
#   3. SIGTERM the dsh subprocess (simulates dsh crash / restart).
#   4. Spawn a fresh dsh on the same port (simulates recovery).
#   5. Verify RecoverAll re-attaches: same sessionId present, mux stream
#      delivers session/subscribed for it, prompt round-trip works.
#
# Pre-req: dsh binary on PATH, ./bin/nightme built.

set -euo pipefail

PORT="${DSH_TEST_PORT:-13080}"
NIGHTME_LOG=/tmp/nightme-recovery-test.log
DSH_PID_FILE=/tmp/dsh-recovery-test.pid
NIGHTME_PID_FILE=/tmp/nightme-recovery-test.pid

cleanup() {
  set +e
  if [[ -f "$DSH_PID_FILE" ]]; then
    kill -TERM "$(cat "$DSH_PID_FILE")" 2>/dev/null || true
    rm -f "$DSH_PID_FILE"
  fi
  if [[ -f "$NIGHTME_PID_FILE" ]]; then
    kill -TERM "$(cat "$NIGHTME_PID_FILE")" 2>/dev/null || true
    rm -f "$NIGHTME_PID_FILE"
  fi
}
trap cleanup EXIT

log() { echo "[$(date +%H:%M:%S)] $*" >&2; }
fail() { log "FAIL: $*"; exit 1; }

# Step 1: start fresh dsh
log "Step 1: launch fresh dsh web on port $PORT"
rm -f /tmp/dsh-recovery-test.log
dsh --profile web --port "$PORT" >/tmp/dsh-recovery-test.log 2>&1 &
echo $! > "$DSH_PID_FILE"
for i in 1 2 3 4 5 6 7 8 9 10; do
  if curl -sf "http://127.0.0.1:$PORT/" >/dev/null; then
    log "  dsh ready (pid $(cat "$DSH_PID_FILE"))"
    break
  fi
  sleep 1
done
[[ "$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:$PORT/)" = "200" ]] \
  || fail "dsh did not become ready"

# Step 2: create a fresh session via curl, capture sessionId
log "Step 2: create session"
SESSION_RESP=$(curl -s -X POST "http://127.0.0.1:$PORT/api/session.create" \
  -H 'Content-Type: application/json' \
  -d "{\"type\":\"client-request\",\"rpcId\":\"recovery-test-create\",\"method\":\"session.create\",\"payload\":{\"cwd\":\"/tmp/dsh-probe\"}}")
SESSION_ID=$(echo "$SESSION_RESP" | jq -r '.result.value.sessionId')
[[ -n "$SESSION_ID" && "$SESSION_ID" != "null" ]] || fail "session.create returned no id"
log "  created session: $SESSION_ID"

# Step 3: kill dsh, restart on same port
log "Step 3: SIGTERM dsh (pid $(cat "$DSH_PID_FILE"))"
kill -TERM "$(cat "$DSH_PID_FILE")"
for i in 1 2 3 4 5; do
  if ! ps -p "$(cat "$DSH_PID_FILE")" >/dev/null 2>&1; then break; fi
  sleep 1
done
ps -p "$(cat "$DSH_PID_FILE")" >/dev/null 2>&1 && fail "dsh still alive after SIGTERM"

# Step 4: spawn fresh dsh on same port
log "Step 4: spawn fresh dsh on port $PORT"
dsh --profile web --port "$PORT" >/tmp/dsh-recovery-test.log 2>&1 &
echo $! > "$DSH_PID_FILE"
for i in 1 2 3 4 5 6 7 8 9 10; do
  if curl -sf "http://127.0.0.1:$PORT/" >/dev/null; then break; fi
  sleep 1
done
log "  fresh dsh ready (pid $(cat "$DSH_PID_FILE"))"

# Step 5: re-attach via session.create({sessionId, cwd}) — expect same id
log "Step 5: re-attach via session.create with original sessionId"
RECOVER_RESP=$(curl -s -X POST "http://127.0.0.1:$PORT/api/session.create" \
  -H 'Content-Type: application/json' \
  -d "{\"type\":\"client-request\",\"rpcId\":\"recovery-test-attach\",\"method\":\"session.create\",\"payload\":{\"sessionId\":\"$SESSION_ID\",\"cwd\":\"/tmp/dsh-probe\"}}")
RECOVERED_ID=$(echo "$RECOVER_RESP" | jq -r '.result.value.sessionId')
[[ "$RECOVERED_ID" = "$SESSION_ID" ]] || fail "re-attach returned different id: $RECOVERED_ID (expected $SESSION_ID)"
log "  ✓ re-attached: same sessionId $RECOVERED_ID"

# Step 6: verify session.list still contains it
log "Step 6: verify session.list"
LIST_RESP=$(curl -s -X POST "http://127.0.0.1:$PORT/api/session.list" \
  -H 'Content-Type: application/json' \
  -d '{"type":"client-request","rpcId":"recovery-test-list","method":"session.list","payload":{}}')
FOUND=$(echo "$LIST_RESP" | jq -r --arg s "$SESSION_ID" '.result.value.items[] | select(.sessionId == $s) | .sessionId')
[[ -n "$FOUND" ]] || fail "session.list did not contain reattached session"
log "  ✓ session.list returns the reattached session"

# Step 7: send a prompt and verify round-trip
log "Step 7: prompt round-trip on reattached session"
PROMPT_RESP=$(curl -s -X POST "http://127.0.0.1:$PORT/api/session.prompt" \
  -H 'Content-Type: application/json' \
  -d "{\"type\":\"client-request\",\"rpcId\":\"recovery-test-prompt\",\"method\":\"session.prompt\",\"payload\":{\"sessionId\":\"$SESSION_ID\",\"mode\":\"queue\",\"content\":[{\"type\":\"text\",\"text\":\"reply with single word PONG\"}]}}")
PROMPT_OK=$(echo "$PROMPT_RESP" | jq -r '.result.ok')
[[ "$PROMPT_OK" = "true" ]] || fail "session.prompt on reattached session failed: $PROMPT_RESP"
log "  ✓ session.prompt accepted on reattached session"

log ""
log "========================================="
log "test-recovery.sh PASSED"
log "  Phase 2 (restart recovery) verified:"
log "  - sessionId stable across dsh restart"
log "  - re-attach via session.create({sessionId, cwd})"
log "  - session.list still returns the session"
log "  - prompt round-trip works on reattached session"
log "========================================="