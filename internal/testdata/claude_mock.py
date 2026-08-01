#!/usr/bin/env python3
"""Mock Claude Code CLI that mimics stream-json output.

Reads one JSON envelope per line from stdin and emits:
  1. A user-role echo event (simulates --replay-user-messages)
  2. An assistant text event (the agent's response)
  3. A terminal result event (signals turn completion)

Logs each step to stderr so the test can capture the bridge's
normal-flow transcripts. Exit cleanly on stdin EOF.
"""
import json
import sys


def extract_text(envelope: str) -> str:
    needle = '"text":"'
    idx = envelope.find(needle)
    if idx < 0:
        return "empty"
    start = idx + len(needle)
    end = envelope.find('"', start)
    if end < 0:
        return "empty"
    raw = envelope[start:end]
    return raw.replace('\\"', '"').replace('\\\\', '\\')


def log(msg: str) -> None:
    sys.stderr.write(f"[mock] {msg}\n")
    sys.stderr.flush()


def emit(envelope: str) -> None:
    text = extract_text(envelope)
    log(f"got envelope, text={text!r}")
    # 1. user-role echo
    sys.stdout.write(json.dumps({
        "type": "user",
        "message": {
            "role": "user",
            "content": [{"type": "text", "text": "[replay] " + text}],
        },
    }) + "\n")
    sys.stdout.flush()
    log("emitted user replay")
    # 2. assistant text
    sys.stdout.write(json.dumps({
        "type": "assistant",
        "message": {
            "role": "assistant",
            "content": [{"type": "text", "text": "got: " + text}],
        },
    }) + "\n")
    sys.stdout.flush()
    log("emitted assistant")
    # 3. terminal result
    sys.stdout.write(json.dumps({
        "type": "result",
        "subtype": "success",
        "is_error": False,
        "duration_ms": 1,
        "duration_api_ms": 1,
        "num_turns": 1,
        "result": "done",
    }) + "\n")
    sys.stdout.flush()
    log("emitted result")
    log("flushed all events")


def main() -> int:
    log("started")
    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        emit(line)
    log("EOF")
    return 0


if __name__ == "__main__":
    sys.exit(main())
