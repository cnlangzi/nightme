# F-37 thread-routing probe tools (experimental)

Two throwaway binaries used **once** during the F-37 implementation
to verify Feishu's reply API behaviour against the real production
bot on the Frtpilot-Xiage group (2026-08-04). They are NOT
production code, NOT part of the nightme binary, NOT registered in
any `Makefile` target. They are kept here as a reference artifact so
that:

1. The exact `reply_in_thread` payload shapes the SDK sends are
   recoverable (no need to re-derive if a future change regresses
   them).
2. The real-Feishu response shape (root_id / parent_id / thread_id
   assignment rules) is reproducible without re-running the ops
   walkthrough.

## Files

| File | Purpose | Output |
|---|---|---|
| `feishu_thread_probe.go` | Mock-server probe: 8 combinations sent through a local `httptest.Server` that pretends to be lark. Captures the **outbound JSON request bytes** for each combo. | Run with `go run ./cmd/_probe` (note: shared `package main` with `send_one/`, but `_probe` is a different directory) |
| `send_one/main.go` | Real-server sender: one-shot CLI that fires **one** real message to the real Feishu bot. Reads AppID/AppSecret from `~/.config/nightme/config.yaml`. | Run with `go run ./cmd/_probe/send_one -h` |

## 2026-08-04 experiment record

The canonical findings (request bodies, response IDs, UI
verification) are in **`docs/feat/F-37-tool-thread-routing.md` §7.5`**.
The probe tools here are just the **how we verified it** artifact.

## When to delete

- When F-37 has been in production for a release cycle and no one
  needs to re-verify the SDK payload shape.
- When upgrading `larksuite/oapi-sdk-go` past v3.9.9 (the snapshot
  we ran against) — at that point the mock probe should be
  re-checked against the new SDK and either updated or removed.

## Safety

These tools hit the real Feishu production bot if you supply real
credentials. The send_one CLI **does not** have a dry-run mode. If
you keep them, be careful who has shell access to a machine with
a valid `~/.config/nightme/config.yaml`.
