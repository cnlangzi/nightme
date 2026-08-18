# nightme Workflow — Configuration Reference

> **Status**: Design draft (v0, pre-implementation). Schema may shift as the engine lands.
> **Scope**: YAML schema, trigger semantics, step grammar, expressions. Engine internals (scheduler, runner, store) live elsewhere.

A **workflow** is a YAML file that nightme watches, schedules, and runs. It answers three questions:

1. **When** does it run? (`on:` — trigger)
2. **How many** parallel instances? (`worker:`)
3. **What** does it do? (`jobs.<name>.steps` — ordered steps)

Conceptually a workflow is "GitHub Actions for your local dev loop" — declarative, idempotent, single-binary.

---

## Table of Contents

- [1. File location & layout](#1-file-location--layout)
- [2. Top-level fields](#2-top-level-fields)
- [3. Triggers (`on:`)](#3-triggers-on)
  - [3.1 `schedule`](#31-schedule)
  - [3.2 `pull_request`](#32-pull_request)
  - [3.3 `branch`](#33-branch)
  - [3.4 `issue`](#34-issue)
  - [3.5 `mention`](#35-mention)
- [4. Jobs](#4-jobs)
- [5. Steps](#5-steps)
  - [5.1 `run` — execute shell](#51-run--execute-shell)
  - [5.2 `prompt` — invoke local agent](#52-prompt--invoke-local-agent)
  - [5.3 Common step fields](#53-common-step-fields)
- [6. Expressions (`${{ ... }}`)](#6-expressions--)
- [7. Complete examples](#7-complete-examples)
- [8. Runtime architecture](#8-runtime-architecture)
  - [8.1 The `bot` channel](#81-the-bot-channel)
  - [8.2 Architecture diagram](#82-architecture-diagram)
  - [8.3 Trigger → virtual chat mapping](#83-trigger--virtual-chat-mapping)
  - [8.4 Synthetic messages through Gateway](#84-synthetic-messages-through-gateway)
  - [8.5 Run state &amp; persistence](#85-run-state--persistence)
  - [8.6 Outbound: notifications vs chat replies](#86-outbound-notifications-vs-chat-replies)
- [9. Out of scope (v0)](#9-out-of-scope-v0)
- [10. Cross-references](#10-cross-references)

---

## 1. File location & layout

Workflows are read from:

```
~/.nightme/workflows/*.yaml
```

- One workflow per file; filename is informational (not used as the identifier).
- The `name:` field is the canonical identifier; duplicates cause a load error.
- Hot-reload on file change (no daemon restart needed).

---

## 2. Top-level fields

| Field | Type | Required | Default | Purpose |
|---|---|---|---|---|
| `name` | string | ✅ | — | Unique workflow identifier; surfaces in logs and notifications. |
| `worker` | int | ❌ | `1` | Max parallel instances when the trigger fires concurrently. |
| `on` | object | ✅ | — | Trigger spec (see §3). |
| `jobs` | map | ✅ | — | Named jobs to run in order (see §4). |

Minimal skeleton:

```yaml
name: my-workflow
worker: 1
on:
  schedule:
    - cron: '0 9 * * *'
jobs:
  main:
    steps:
      - run: echo "hello"
```

---

## 3. Triggers (`on:`)

A workflow can listen to **multiple** trigger sources in one file — all conditions are OR'd. The trigger block is one of five shapes:

### 3.1 `schedule`

Cron list. Standard 5-field cron (`minute hour dom mon dow`), evaluated in the owner's local timezone.

```yaml
on:
  schedule:
    - cron: '0 9 * * *'        # every day at 09:00
    - cron: '0 18 * * 5'       # every Friday at 18:00
```

### 3.2 `pull_request`

Fires when a PR event arrives (GitHub / GitLab webhook).

| Field | Type | Default | Purpose |
|---|---|---|---|
| `branches` | string[] | `[]` (all) | Target branch filter (e.g. `main`, `develop`). |
| `events` | string[] | `[]` (all) | Sub-events to listen to: `opened`, `reopened`, `synchronize`, `closed`, `labeled`, … (per provider's webhook vocabulary). |

```yaml
on:
  pull_request:
    branches: [main, develop]
    events: [opened, reopened, synchronize]
```

### 3.3 `branch`

Fires when a branch-level event happens (push, create, delete). Useful for release / protection flows.

| Field | Type | Default | Purpose |
|---|---|---|---|
| `patterns` | string[] | `[]` (all) | Glob match on branch name (e.g. `feature/*`, `release/*`). |
| `events` | string[] | `[]` (all) | Sub-events: `pushed`, `created`, `deleted`. |

```yaml
on:
  branch:
    patterns: ['feature/*', 'release/*']
    events: [pushed, created, deleted]
```

### 3.4 `issue`

Fires when an issue event arrives.

| Field | Type | Default | Purpose |
|---|---|---|---|
| `events` | string[] | `[]` (all) | Sub-events: `opened`, `commented`, `labeled`, `closed`. |

```yaml
on:
  issue:
    events: [opened, commented, labeled, closed]
```

### 3.5 `mention`

Fires when **the owner is @-mentioned** in any inbound notification (PR comment, issue comment, IM message). The owner account is read from nightme config — no `target` field needed (single-user model).

| Field | Type | Default | Purpose |
|---|---|---|---|
| `commands` | string \| string[] | (all) | Whitelist of mention commands. Omit to fire on any mention. |

**Command format**: the first word after `@owner` is the command. `@owner review this PR` → `commands: review`.

| Mention text | `commands` | Triggers? |
|---|---|---|
| `@owner review` | `review` | ✅ |
| `@owner review` | `[fix, close]` | ❌ |
| `@owner please review` | `review` | ❌ (first word is "please", not "review") |
| `@owner review` | (omitted) | ✅ |

```yaml
# Single command
on:
  mention:
    commands: review

# Multiple commands, same workflow
on:
  mention:
    commands: [fix, close]

# Any mention
on:
  mention: {}
```

---

## 4. Jobs

`jobs` is a map of named job definitions. Each job runs its `steps` in order. Jobs run **in parallel by default**; use `needs` to express dependencies.

```yaml
jobs:
  review:
    steps: [ ... ]                # runs first

  auto-fix:
    needs: review                 # waits for review to finish
    steps: [ ... ]
```

| Field | Type | Required | Purpose |
|---|---|---|---|
| `needs` | string \| string[] | ❌ | Other job(s) this one waits for. |
| `if` | string | ❌ | Condition expression (see §6). |
| `steps` | step[] | ✅ | Ordered steps to execute. |

**No per-job concurrency knob.** Concurrency is workflow-level (`worker:`). Each worker instance runs the full job graph.

---

## 5. Steps

Each step is one of two shapes:

| Form | Use case |
|---|---|
| `run` | Execute shell (inline or file) |
| `prompt` | Send a prompt to a local AI agent |

### 5.1 `run` — execute shell

Supports both **inline** and **file** modes (mirrors GitHub Actions `run`):

```yaml
# 1. Inline single-line
- name: Lint
  run: npm run lint

# 2. Inline multi-line (literal block)
- name: Build
  run: |
    set -euo pipefail
    echo "Building..."
    make build

# 3. Script file (path must exist; resolved relative to workflow dir)
- name: Checkout
  run: ./scripts/checkout.sh

- name: Notify feishu
  run: ./scripts/notify-feishu.sh oc_xxx "PR review done"
```

**Disambiguation rule** (matches GitHub Actions):

- Value contains a newline OR starts with `|` → multi-line inline
- Value is a path that resolves to an existing file → run that file
- Otherwise → single-line shell command

Reuse is achieved by writing shell scripts (e.g. under `~/.nightme/workflows/scripts/`), not by importing external action packages.

### 5.2 `prompt` — invoke local agent

`prompt` is the "run for LLMs" sibling of `run`. It sends a text prompt to a local agent (Claude Code, Codex, Pi, etc.).

```yaml
# Basic
- name: AI review
  prompt: |
    Please review this PR.
    Title: ${{ event.pr.title }}
  agent: codex                  # local agent name (see below)

# Without agent → owner's default agent
- name: Quick summary
  prompt: "Summarize ${{ event.pr.title }} in one line"
```

| Field | Type | Required | Purpose |
|---|---|---|---|
| `prompt` | string | ✅ | The prompt text (supports `${{ ... }}` expressions). |
| `agent` | string | ❌ | Local agent name (`codex`, `claude`, `pi`, `opencode`, `dsh`). Defaults to the owner's configured primary agent. |

**Scope note**: nightme only **invokes** the agent — it does not install, configure, or store credentials. The agent binary must be on `$PATH` and have its own environment (API keys, config) ready. This is a "reuse the local env" model, not a "manage the agent" model.

### 5.3 Common step fields

Every step (both `run` and `prompt`) supports:

| Field | Type | Default | Purpose |
|---|---|---|---|
| `name` | string | (derived) | Human-readable name for logs / notifications. |
| `id` | string | — | Step ID for output reference: `${{ steps.<id>.outputs.<key> }}`. |
| `if` | string | `success()` | Condition expression. Failure of a previous step skips dependents unless `continue-on-error: true`. |
| `env` | map | — | Environment variables for the step. |
| `continue-on-error` | bool | `false` | Don't fail the job if this step errors. |
| `shell` | string | (host default) | `run` only: which shell interpreter to use (`bash`, `sh`, `zsh`, `pwsh`, …). |

---

## 6. Expressions (`${{ ... }}`)

Any string value can embed expressions. Expressions are evaluated **at step start** (not at workflow load).

### 6.1 Context roots

| Context | When | What's in it |
|---|---|---|
| `event.*` | always | The trigger event payload. Schema depends on `on.<trigger>`. |
| `steps.<id>.outputs.*` | after that step finishes | Output keys the step declared. |
| `needs.<job>.outputs.*` | after that job finishes | Aggregated outputs of that job's last step. |
| `secrets.*` | always | Secrets loaded from nightme config. |

### 6.2 Event payload shapes (v0)

| Trigger | `event.*` keys |
|---|---|
| `schedule` | `event.cron`, `event.scheduled_at` |
| `pull_request` | `event.pr.number`, `event.pr.title`, `event.pr.body`, `event.pr.author`, `event.pr.branch`, `event.pr.base`, `event.pr.changed_files`, `event.action` |
| `branch` | `event.branch.name`, `event.action` |
| `issue` | `event.issue.number`, `event.issue.title`, `event.issue.body`, `event.issue.author`, `event.action` |
| `mention` | `event.mention.text` (full message), `event.mention.command` (first word), `event.mention.thread`, `event.mention.source` (`pr` / `issue` / `im`) |

### 6.3 Functions

| Function | Meaning |
|---|---|
| `success()` | True if the previous step (or all `needs` for jobs) succeeded. |
| `failure()` | True if the previous step failed. |
| `always()` | Always true (use sparingly). |
| `cancelled()` | True if the run was cancelled. |

### 6.4 Examples

```yaml
# Conditional notify
- name: Notify
  if: ${{ steps.ai.outputs.has_issue == 'true' }}
  run: ./scripts/notify-feishu.sh oc_xxx "PR ${{ event.pr.number }} has issues"

# Cross-step output
- name: AI review
  id: ai
  prompt: "Review ${{ event.pr.title }}"
  agent: codex

- name: Apply fix
  if: ${{ steps.ai.outputs.verdict == 'needs-fix' }}
  prompt: "Fix: ${{ steps.ai.outputs.verdict_detail }}"

# Cross-job output
- name: Final report
  prompt: "Based on review: ${{ needs.review.outputs.ai.verdict }}"
```

---

## 7. Complete examples

### 7.1 PR review workflow

```yaml
name: pr-reviewer
worker: 3                                  # up to 3 PRs reviewed in parallel

on:
  pull_request:
    branches: [main, develop]
    events: [opened, reopened, synchronize]
  mention:
    commands: review                      # also: "@owner review this"

jobs:
  review:
    steps:
      - name: Checkout
        run: ./scripts/checkout.sh

      - name: Lint
        run: npm run lint
        continue-on-error: true

      - name: AI review
        id: ai
        prompt: |
          Review PR:
          Title: ${{ event.pr.title }}
          Author: ${{ event.pr.author }}
          Files:  ${{ event.pr.changed_files }}
        agent: codex

      - name: Notify
        if: ${{ steps.ai.outputs.has_issue == 'true' }}
        run: ./scripts/notify-feishu.sh oc_xxx \
          "PR ${{ event.pr.number }} has issues"
```

### 7.2 Mention-driven auto-fix

```yaml
name: auto-fix

on:
  mention:
    commands: [fix, auto-fix]              # "@owner fix issue #42" or "@owner auto-fix ..."

jobs:
  fix:
    steps:
      - name: Parse mention
        run: ./scripts/parse-mention.sh

      - name: Apply fix
        prompt: |
          Source: ${{ event.mention.source }}
          Issue:  ${{ event.mention.text }}
          Thread: ${{ event.mention.thread }}
        agent: claude

      - name: Notify
        run: ./scripts/notify-feishu.sh oc_xxx "Processed: ${{ event.mention.command }}"
```

### 7.3 Nightly batch

```yaml
name: nightly-cleanup
worker: 1                                  # serial — only one instance at a time

on:
  schedule:
    - cron: '0 3 * * *'                    # 3am every day

jobs:
  cleanup:
    steps:
      - run: ./scripts/rotate-logs.sh
      - run: ./scripts/prune-caches.sh
        continue-on-error: true
      - run: ./scripts/notify-feishu.sh oc_xxx "Nightly cleanup done"
```

### 7.4 Branch-protection (release flow)

```yaml
name: release-watch

on:
  branch:
    patterns: ['release/*']
    events: [pushed]

jobs:
  validate:
    steps:
      - run: ./scripts/check-semver.sh ${{ event.branch.name }}
      - run: ./scripts/run-e2e.sh
      - name: Notify
        run: ./scripts/notify-feishu.sh oc_xxx "Release ${{ event.branch.name }} validated"
```

---

## 8. Runtime architecture

This section answers "where do workflow runs actually execute?" — the previous sections defined the YAML schema, this one defines how the engine fits into the existing nightme runtime.

### 8.1 The `bot` channel

Workflows are run by a dedicated Channel implementation: **`internal/channel/bot/`** (the *bot* channel). It is a peer of `feishu` and `echo`, not a new control plane.

| Channel | Backing event source | "User" |
|---|---|---|
| `feishu` | Feishu webhook | A real person typing in chat |
| `echo` | (none — test) | Test harness |
| **`bot`** | cron / PR webhook / branch event / issue event / mention | The workflow YAML scheduler |

**Iron rule:** the `bot` channel is an *event source*. It does not call agents directly. It produces synthetic inbound messages and routes them through the same Gateway every other channel uses.

### 8.2 Architecture diagram

```
  ┌─ trigger sources ─────────────┐
  │  cron   PR webhook            │
  │  branch issue  mention        │
  └──────────────┬────────────────┘
                 │
                 ▼
  ┌────────────────────────────────────┐
  │      internal/channel/bot         │
  │                                    │
  │  ┌────────────────────────────┐   │
  │  │  workflow 引擎             │   │
  │  │  - 解析 ~/.nightme/workflows/│   │
  │  │  - 调度 / 事件匹配 / 状态   │   │
  │  │  - 步骤执行                │   │
  │  └────────────────────────────┘   │
  │             │                     │
  │  对接基建:  ▼                     │
  │   shell / file / secrets          │
  │   / GitHub API / 飞书 API / ...   │
  └────────────────┬───────────────────┘
                   │
                   │  合成"消息"按 Channel 契约发出
                   │  （跟 feishu 用户敲键盘一样）
                   ▼
          ┌─────────────────┐
          │    Gateway      │  ← 唯一入口
          │  router+dispatch│
          └────────┬────────┘
                   ▼
          ┌─────────────────┐
          │   ChatSession   │  ← 虚拟 chat
          └────────┬────────┘
                   ▼
          ┌─────────────────┐
          │  AgentSession   │  ← 跑 agent
          └─────────────────┘
```

The dashed-box path (`run:` steps) does **not** go through Gateway — `run` is plain shell, executed by the bot engine itself. The solid-box path (`prompt:` steps) does go through Gateway.

### 8.3 Trigger → virtual chat mapping

Every workflow run lives inside exactly one ChatSession. The `bot` channel picks the ChatSession by trigger type:

| Trigger | Virtual ChatSession source |
|---|---|
| `schedule` | A workflow-generated "system" chat owned by the bot. One per workflow. |
| `pull_request` | The chat that *opened* the PR thread, if any. Otherwise a system chat. |
| `branch` | A system chat bound to the repo. (No real chat exists for branch events.) |
| `issue` | The chat that *opened* the issue thread, if any. Otherwise a system chat. |
| `mention` | The chat where the mention occurred (e.g. the Feishu group). |

System chats are persistent — a workflow can carry its CWD, its active agent, and its session memory across runs. The bot owns them under the hood; user configures only the workflow YAML, not the chats.

### 8.4 Synthetic messages through Gateway

A `prompt:` step is, at runtime, a synthetic inbound message:

```yaml
- prompt: "Review ${{ event.pr.title }}"
  agent: codex
```

…is realized as:

> `bot` channel → synthesizes an inbound envelope
> → Gateway dispatch (same path as a Feishu message)
> → routed to the workflow's ChatSession
> → routed to the active AgentSession for `codex` in that chat's CWD
> → agent processes; output flows back to `bot`
> → `bot` records the output under `steps.<id>.outputs.*`
> → workflow advances to next step

The AgentSession pool semantics are unchanged: a `prompt` step in a given chat+agent+CWD triple **reuses** an existing live session if one is running, just like a human typing in that chat. Cold start only when no live session exists for that triple.

### 8.5 Run state & persistence

A workflow run is durable. The bot persists per-run state at:

```
~/.nightme/workflows/state/<run-id>.json
```

Where `<run-id>` is `<workflow-name>:<trigger-key>:<started-at>` (deterministic per trigger, so re-fires coalesce).

State shape (sketch):

```json
{
  "run_id": "pr-reviewer:pr-42:2026-08-18T09:00:00Z",
  "workflow": "pr-reviewer",
  "trigger": { "kind": "pull_request", "event": { "pr": { ... } } },
  "chat_id": "system:pr-reviewer:42",
  "current_job": "review",
  "current_step": "ai",
  "step_outputs": {
    "ai": { "verdict": "needs-fix", "has_issue": "true" }
  },
  "status": "running"          // running | succeeded | failed | cancelled
}
```

- **Daemon restart**: in-flight runs are reloaded from disk; the bot resumes from `current_step` (re-evaluates `if:` conditions; re-runs idempotent steps; `prompt` resumes a live session if still alive).
- **Failure recovery**: a failed step leaves the run in `failed`; the user (or the next trigger) can decide whether to retry.
- **Worker pool** (§2 `worker: N`): the bot caps concurrent runs of the same workflow at N. Excess triggers queue (FIFO) and run when a worker frees up.

### 8.6 Outbound: notifications vs chat replies

There are two ways a workflow can talk back to the outside world, and they have different semantics:

| Need | Tool | Path |
|---|---|---|
| Send a notification (Feishu, Slack, email, webhook) | `run:` with `curl` (or a helper script under `scripts/`) | Bot → shell → external API directly. **Does not** go through Gateway. |
| Reply into the virtual chat (visible in status output) | `prompt:` step's output | Bot → synthetic message → Gateway → renders to the chat's outbound surface. |

`run:` is for *side effects*; `prompt:` is for *agent interaction*. Mixing the two is the most common workflow mistake — pick the right one for the job.

---

## 9. Out of scope (v0)

These are deliberately **not** in v0; they belong in a later iteration.

| Not in v0 | Why |
|---|---|
| External action marketplace (`uses: owner/repo@v1`) | Reuse is via shell scripts; the project's stack stays single-binary. |
| Cron expressions with seconds / `@reboot` | Standard 5-field cron is enough; expansion is a follow-up. |
| Job-level matrix / fan-out | Worker pool at workflow level covers the common case. |
| Per-step `timeout-minutes` | Inherits the host process timeout for now. |
| Webhook signature verification (beyond what `git provider` already does) | Provider abstraction (see F-50) handles this. |
| Cross-workflow triggers (`workflow_run`) | Compose via mention + schedule. |
| Secret injection from external vaults | Reads from `~/.nightme/config.yaml`; HashiCorp Vault / 1Password are follow-ups. |
| Reusable action library (`./actions/xxx`) | Same reason as the marketplace point — shell is enough. |

---

## 10. Cross-references

- Trigger source: GitHub / GitLab webhook payloads (provider-agnostic; the GitProvider abstraction normalizes them).
- Mention parsing reuses `internal/channel/feishu/mention.go` (`computeHasMention`, `stripMentionPrefix`).
- Agent names (`codex`, `claude`, `pi`, `opencode`, `dsh`) come from the `agents:` block of `~/.nightme/config.yaml`.
- The `bot` channel lives at `internal/channel/bot/`, alongside `feishu` and `echo`. It implements the same `Channel` interface; no new control plane.
- This doc describes the **YAML schema and runtime architecture**. Engine internals (scheduler, runner, event store, run state machine) are tracked in a separate `feat/F-XX-workflow-engine.md` design doc.
