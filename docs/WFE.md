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
  - [8.1 The layering](#81-the-layering)
  - [8.2 The playbook schema](#82-the-playbook-schema)
  - [8.3 The policy](#83-the-policy)
  - [8.4 The tools](#84-the-tools)
  - [8.5 The two capabilities](#85-the-two-capabilities)
  - [8.6 The three modes](#86-the-three-modes)
  - [8.7 The configuration](#87-the-configuration)
  - [8.8 The `/me` commands](#88-the-me-commands)
  - [8.9 The outcome model](#89-the-outcome-model)
  - [8.10 Guardrails and recovery](#810-guardrails-and-recovery)
  - [8.11 The workflow service](#811-the-workflow-service)
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
| `workspaces` | string[] | ✅ | — | **List of CWDs this workflow operates on.** Each workspace is a local directory (typically a git checkout). The workflow's triggers fire only for events on these workspaces; every run executes `chdir` into one of them. |
| `agent` | string | ❌ | (nightme default) | **Default agent for this workflow's `prompt` steps.** Applied via the `/use agent <name>` slash command that bot pushes into `bot.Incoming()` at run start. Step-level `agent:` overrides this; if both are absent, the nightme primary agent (`cfg.Primary`) is used. |
| `worker` | int | ❌ | `1` | Max parallel instances when the trigger fires concurrently. |
| `on` | object | ✅ | — | Trigger spec (see §3). |
| `jobs` | map | ✅ | — | Named jobs to run in order (see §4). |

Minimal skeleton:

```yaml
name: my-workflow
workspaces: [~/work/myproject]                # ← required: which CWDs
agent: codex                                  # ← optional: workflow default agent
worker: 1
on:
  schedule:
    - cron: '0 9 * * *'
jobs:
  main:
    steps:
      - run: echo "hello"
```

**Full example** (uses every top-level field + every step kind + every trigger kind):

```yaml
name: full-demo
workspaces: [~/work/nightme]                  # which CWDs this applies to
agent: codex                                  # default agent for all prompt steps
worker: 3                                     # up to 3 parallel runs

on:                                           # multiple triggers OR'd
  schedule:
    - cron: '0 9 * * *'                      # nightly at 9 AM
  pull_request:
    branches: [main, develop]                 # PRs to main or develop
    events: [opened, synchronize]              # opened or new commit pushed
  branch:
    patterns: ['release/*']                   # release branches
    events: [pushed]
  issue:
    events: [opened, labeled]                 # new or labeled issues
  mention:                                     # @owner mention
    commands: [review, fix]                   # only review or fix mentions

jobs:
  prepare:                                     # one job
    steps:
      - id: setup
        run: echo "starting at $(date)"
        env:
          TZ: UTC

  review:                                     # another job; runs in parallel with prepare
    needs: [prepare]                          # wait for prepare
    steps:
      # 1. shell step: run a script
      - id: lint
        run: ./scripts/lint.sh
        continue-on-error: true                # don't fail the whole run on lint

      # 2. prompt step: ask the agent (inherits workflow-level agent: codex)
      - id: ai
        if: ${{ success() }}                  # skip if lint failed (due to continue-on-error)
        prompt: |
          Review this PR.
          Title: ${{ event.title }}
          Author: ${{ event.author }}
        env:
          REVIEW_DEPTH: deep

      # 3. prompt step with step-level agent override
      - id: critical-look
        if: ${{ steps.ai.outputs.verdict == 'needs-fix' }}
        prompt: "Double-check the AI's review."
        agent: claude                         # override the workflow default for this step

      # 4. use step: invoke a bot-injected action
      - id: notify
        if: ${{ always() }}
        use: notify
        with:
          channel: feishu
          target: oc_xxx
          message: "PR ${{ event.title }}: ${{ steps.ai.outputs.verdict }}"

      # 5. use step: user-defined action script
      - id: cleanup
        if: ${{ always() }}
        use: cleanup-tmp-files
        with:
          paths: [/tmp/pr-${{ event.number }}]
```

This example demonstrates every feature in one workflow:
- All 5 trigger kinds (`schedule` / `pull_request` / `branch` / `issue` / `mention`)
- Both `cron` style and `command` style for `mention`
- All 3 step kinds (`run` / `prompt` / `use`)
- Step-level overrides (agent: claude on critical-look)
- Conditional execution (`if:` expressions)
- Cross-job dependency (`needs: [prepare]`)
- Job-level env merging
- `continue-on-error` for graceful degradation

**Why `workspaces` (plural, array)**: one workflow can apply to many projects. e.g. the same `nightly-cleanup` workflow might run on every checkout under `~/work/*`. The array is the trigger filter: a trigger event for workspace X only fires workflows whose `workspaces` list contains X.

**Agent resolution priority** (highest first):

1. **Step-level** `prompt.agent` (e.g. `- prompt: "..."` + `agent: codex` on the step)
2. **Workflow-level** `agent` (this field)
3. **nightme default** (`cfg.Primary` in `nightme.yaml`)

When bot runs the workflow, only the resolved agent is set on the chat via `/use agent <name>`. The other levels are not pushed as messages (avoids redundant `/use agent` calls).

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

Fires when **the owner is @-mentioned anywhere that the underlying git provider's mention API can deliver** (sourced via `gitProvider` — this is **not** related to Feishu chat mentions). The "owner" here is the GitHub/GitLab user that the `gitProvider` credentials authenticate as — bot doesn't need a separate `owner_github_login` config; whoever the token belongs to is the owner, and the API can only see events related to that user (and repos the token has access to).

**Permissive trigger**: bot does not filter by event subtype. Whatever the gitProvider's mention API can deliver counts — issue/PR body mention, issue/PR comment mention, PR review comment mention, PR review summary mention, etc. The `gitProvider` is responsible for normalizing each platform's mention events (GitHub `mentioned`, GitLab `Note` family) into a single "mention" event for bot to consume.

**v0 supported commands**: only `@<owner> review [id]` is implemented. Other commands (`fix`, `close`, etc.) are reserved for later. The whitelist `commands: [review]` (or just `commands: review`) restricts matching to this verb.

**v0 `<id>` semantics**: when present in args, it's an explicit PR/issue number to review. When absent, the workflow reviews the PR/issue where the mention happened (from `event.mention.pr_number` or `event.mention.issue_number`).

| Field | Type | Default | Purpose |
|---|---|---|---|
| `commands` | string \| string[] | (all) | Whitelist of mention commands. Omit to fire on any mention. |

**Command format** (v0): `@<owner> review [<id>]` — the only supported command is `review`, and `<id>` is optional.

| Trigger text | `command` | `args` | Reviewed target |
|---|---|---|---|
| `@owner review` | `review` | (empty) | `event.mention.pr_number` / `event.mention.issue_number` (default) |
| `@owner review 123` | `review` | `123` | PR/issue #123 (explicit) |
| `@owner review #42` | `review` | `#42` | PR/issue #42 (`#` is stripped) |
| `@owner fix ...` | `fix` | — | ❌ not triggered (v0 unsupported) |

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

Each step is one of three shapes:

| Form | Field | Use case | Runtime call |
|---|---|---|---|
| `run` | `run: <cmd>` | Execute shell (inline or file) | `Runtime.RunShell` |
| `prompt` | `prompt: <text>` + `agent: <name>` | Send a prompt to a local AI agent | `Runtime.SendPrompt` |
| `use` | `use: <action>` + `with: <args>` | Invoke a bot-injected action (notify, email, GitHub, …) | `Runtime.RunAction` |

`run` and `prompt` are **default-supported** — they work in every workflow because they're part of the wfe runtime contract. `use` is the **extension point** — the bot subsystem registers which action names are valid; see §8.5 for the action model.

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
```

**Disambiguation rule** (matches GitHub Actions):

- Value contains a newline OR starts with `|` → multi-line inline
- Value is a path that resolves to an existing file → run that file
- Otherwise → single-line shell command

`run` is the right tool when you need arbitrary side effects. Reuse is by writing shell scripts (e.g. under `~/.nightme/workflows/scripts/`), not by importing external action packages.

### 5.2 `prompt` — invoke local agent

`prompt` is the "run for LLMs" sibling of `run`. It sends a text prompt to a local agent (Claude Code, Codex, Pi, etc.) and waits for the reply.

```yaml
# Basic
- name: AI review
  prompt: |
    Please review this PR.
    Title: ${{ event.title }}
  agent: codex                  # local agent name (see below)

# Without agent → owner's default agent
- name: Quick summary
  prompt: "Summarize ${{ event.title }} in one line"
```

| Field | Type | Required | Purpose |
|---|---|---|---|
| `prompt` | string | ✅ | The prompt text (supports `${{ ... }}` expressions). |
| `agent` | string | ❌ | Local agent name (`codex`, `claude`, `pi`, `opencode`, `dsh`). Defaults to the workflow-level `agent:` (§2) or, if absent, the nightme primary agent (`cfg.Primary`). |

**Scope note**: nightme only **invokes** the agent — it does not install, configure, or store credentials. The agent binary must be on `$PATH` and have its own environment (API keys, config) ready. This is a "reuse the local env" model, not a "manage the agent" model.

**Agent resolution** (highest priority first):

1. **Step-level** `agent:` on the prompt step
2. **Workflow-level** `agent:` (§2 top-level field)
3. **nightme default** (`cfg.Primary` in `nightme.yaml`)

When bot runs the workflow, it determines the resolved agent and pushes a single `/use agent <name>` slash command into `bot.Incoming()` at run start. The chat's active agent is set before any `prompt` step runs, so every subsequent prompt dispatches to the right agent.

**Why a workflow-level `agent:`**: the same workflow often needs the same agent across all its `prompt` steps. Setting it once at the workflow level avoids repeating `agent: codex` on every step. Step-level `agent:` is for the rare case where one step needs a different agent.

### 5.3 `use` — invoke a bot-injected action

`use` is the **extension point** for any capability beyond shell and agent. The action name is whatever the bot subsystem has registered; the YAML passes arguments via `with:` (same shape as GitHub Actions).

```yaml
# Built-in actions (bot ships with these)
- id: send-notif
  use: notify
  with:
    channel: feishu
    target: oc_xxx
    message: "PR ${{ event.pr.number }} done"

- id: post-pr
  use: github_pr_comment
  with:
    pr: ${{ event.pr.number }}
    body: "Auto-reviewed by nightme"

# User-defined action (script in ~/.nightme/workflows/actions/)
- id: deploy
  use: deploy-staging          # bot wraps scripts/<name>.sh as an action
  with:
    env: production
    tag: ${{ env.RELEASE_TAG }}
```

| Field | Type | Required | Purpose |
|---|---|---|---|
| `use` | string | ✅ | Action name. Resolved by bot against its ActionRegistry. |
| `with` | map | ❌ | Action-specific arguments. Values may embed `${{ ... }}` expressions. |

**Available actions depend on the bot**:

- **Built-in** (registered in Go by the bot subsystem): `notify`, `email`, `github_pr_comment`, `github_issue`, `slack`, `webhook`, …
- **User-defined** (shell scripts under `~/.nightme/workflows/actions/<name>.<ext>`): auto-registered as actions at bot startup. stdout must be JSON `{"outputs": {...}}` for typed outputs, otherwise outputs are empty.

Unknown `use:` names fail the run with a clear error: `unknown action: "foo" — registered: [notify, email, github_pr_comment, …]`.

**Why three forms, not one**: `run` and `prompt` are the universal "do arbitrary work" tools (most workflows only need these two). `use` exists for **structured, repeatable side effects** that benefit from bot-side wiring (typed outputs, retries, secret isolation, multi-channel abstractions) instead of every workflow reinventing them with shell.

### 5.4 Common step fields

Every step (all three forms) supports:

| Field | Type | Default | Purpose |
|---|---|---|---|
| `name` | string | (derived) | Human-readable name for logs / notifications. |
| `id` | string | — | Step ID for output reference: `${{ steps.<id>.outputs.<key> }}`. |
| `if` | string | `success()` | Condition expression. Failure of a previous step skips dependents unless `continue-on-error: true`. |
| `env` | map | — | Environment variables for the step (merged on top of the workflow + bot env). |
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
| `mention` | `event.mention.text` (full comment body), `event.mention.command` (first word after `@owner`), `event.mention.args` (everything after the command), `event.mention.source` (`pr` / `issue`), `event.mention.pr_number` or `event.mention.issue_number`, `event.mention.comment_id`, `event.mention.author`, `event.mention.url` |

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
workspaces: [~/work/nightme]               # which project(s) this workflow runs in
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

### 7.2 Mention-driven review (v0)

```yaml
name: pr-reviewer
workspaces: [~/work/nightme, ~/work/foo]   # applies to both repos

on:
  mention:
    commands: review                       # only `@owner review [id]` triggers this workflow

jobs:
  review:
    steps:
      - name: Determine target id
        id: target
        run: |
          # 如果 args 提供 id，用 args；否则用 mention 环境里的 PR/issue
          if [ -n "$MENTION_ARGS" ]; then
            echo "id=$MENTION_ARGS" >> "$GITHUB_OUTPUT"
          else
            case "$MENTION_SOURCE" in
              pr)     echo "id=$MENTION_PR_NUMBER" >> "$GITHUB_OUTPUT" ;;
              issue)  echo "id=$MENTION_ISSUE_NUMBER" >> "$GITHUB_OUTPUT" ;;
            esac
          fi
        env:
          MENTION_ARGS:        ${{ event.mention.args }}
          MENTION_SOURCE:      ${{ event.mention.source }}
          MENTION_PR_NUMBER:   ${{ event.mention.pr_number }}
          MENTION_ISSUE_NUMBER: ${{ event.mention.issue_number }}

      - name: AI review
        id: ai
        prompt: |
          Review ${{ steps.target.outputs.id }}:
          URL:    ${{ event.mention.url }}
          Author: ${{ event.mention.author }}
        agent: codex

      - name: Post review comment
        use: github_pr_comment
        with:
          pr: ${{ steps.target.outputs.id }}
          body: "Auto-review by nightme:\n\n${{ steps.ai.outputs.summary }}"
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

This section answers "where do runs actually execute, and how do the pieces fit?". Sections §1–§7 define the YAML schema; this one defines the runtime that executes it.

Two kinds of YAML live in the same directory:

- a **workflow** — a cron- or event-driven DAG of steps (§1–§7). It is executed by the workflow service.
- a **playbook** — an ordered list of steps plus the constraints around them. It is executed by the personal agent, `me`.

A file with top-level `steps:` is a playbook; a file with top-level `jobs:` is a workflow. The two are never mixed in one file.

### 8.1 The layering

```
  ┌─ a chat ───────────────┐
  │  /me start fix-issue 42 │
  └──────────┬─────────────┘
             │
             ▼
  ┌────────────────────────────────────────────────┐
  │  internal/me — the personal agent               │
  │                                                │
  │  System 1 (decision model) — decides the next   │
  │  step when it is sure                          │
  │  System 2 (chat model + tools) — decides when   │
  │  System 1 is not, and whenever text is needed   │
  │                                                │
  │  the policy (internal/wfe.Policy) bounds every   │
  │  action a run may take                         │
  └───────┬──────────────────────────┬─────────────┘
          │ commands                 │ questions, progress
          ▼                          ▼
  ┌──────────────────────┐   ┌──────────────────────┐
  │ internal/channel/    │   │ runtime ChatSender   │
  │ loopback — transport │   │ → the starting chat  │
  └──────────┬───────────┘   └──────────────────────┘
             │
             ▼
  ┌────────────────────────────────────────────────┐
  │  gateway → chatsession → agentsession → bridge  │
  │  → the coding agent that does the work         │
  └────────────────────────────────────────────────┘
```

| Layer | Package | Owns |
|---|---|---|
| Transport | `internal/channel/loopback` | Sending a command into a chat, tracking it, and reporting its outcome. Knows nothing about playbooks. |
| The agent | `internal/me` | The run: what to do next, what the policy allows, what the observer is told. |
| The models | `internal/llm` | Two capabilities: `Provider` (chat and tool calling) and `Decider` (typed decision questions). |
| The schema | `internal/wfe` | The playbook and workflow schemas, and the `Policy` that bounds a run. |

The agent drives the daemon the way a person drives it through Feishu: every command is a real message with a real id, and every answer comes back as a structured outcome. It never reaches into the gateway or the chat sessions.

### 8.2 The playbook schema

A playbook is a human-written, ordered list of steps that bounds what the agent loop may do. It carries no workspace and no trigger: the working directory comes from the chat that starts the run.

```yaml
name: fix-issue
args: [id]
goal: Fix issue #${{ args.id }} and ship it as a PR.
steps:
  - /gtw fix ${{ args.id }} -y
  - /gtw commit
  - run: /review
    note: If it fails or does not finish, continue with /review -c. If it passes and left changes, /gtw commit first.
    allow: [/review -c, /gtw commit]
    max: 5
  - /gtw push
  - /gtw pr
```

| Field | Meaning |
|---|---|
| `name` | The playbook's name. `/me start <name>` picks it. |
| `args` | The names of the arguments. `/me start fix-issue 42` maps `42` onto `id`. |
| `goal` | What the run is for. It is part of the model's instructions. |
| `max_actions` | The action budget of the whole playbook. Default 30. |
| `instruct` | Whether the agent may send free-form instructions. Default true. |
| `steps` | The ordered steps. |

A step is either a plain string — shorthand for `run` — or a mapping:

| Field | Meaning |
|---|---|
| `run` | The command the step performs. |
| `note` | Why the step is there, and when the extra commands apply. It is the description the decision model reads. |
| `allow` | The extra commands the agent may use inside the step. |
| `max` | The number of actions the step may take. 0 means no limit of its own. |

Only `${{ args.NAME }}` is supported, and only for a declared argument. Any other expression is rejected when the playbook is loaded.

### 8.3 The policy

The policy is the bound on what a run may do. It is a pure function of the playbook and the actions taken so far.

| Check | Reports |
|---|---|
| `CheckRun(command)` | Whether the command may run now, and which step it belongs to. |
| `CheckInstruct()` | Whether a free-form instruction is allowed. |
| `CheckFinish(status)` | Whether the run may end. `done` needs the last step's latest outcome to be `ok` or `noop`; `failed` is always allowed. |
| `Remaining()` | The action budget left, overall and per step. |
| `Render()` | The playbook as a compact text block: the goal, the steps, the notes, the allowed commands and the limits. |
| `AllowedCommands()` | The commands the policy accepts right now. |

A command belongs to the first step that lists it, and a step that spent its limit takes its commands out of the list.

### 8.4 The tools

The chat model acts through six tools. Every handler enforces the policy: a violation comes back as a tool error result, and the model corrects itself.

| Tool | Behaviour |
|---|---|
| `run_step(command)` | Runs `Policy.CheckRun`, then sends the command to the work chat. It returns the outcome — status, summary, data — and the transcript of what the work agent said. |
| `instruct(text)` | Runs `Policy.CheckInstruct`, then sends a free-form prompt to the work chat. It is tracked and returned like `run_step`. |
| `answer_choice(choice_id, option_id)` | Answers a permission request or a question the work agent is waiting for. |
| `ask_human(question)` | Posts the question to the observer chat and suspends the run. `/me reply <text>` resumes it with the reply as the tool's result. |
| `repo_status()` | Reads the work chat's repository: the branch, whether the tree is dirty, and how far the branch is ahead of or behind its upstream and its base branch. |
| `finish(status, summary)` | Runs `Policy.CheckFinish`, ends the run and posts the summary. `status` is `done` or `failed`. |

`run_step` blocks while the command runs and makes no model calls. It comes back early when the work agent asks something — the model can then answer, instruct or ask — or when the command goes quiet for ten minutes. Both cases report `waiting` and the reason.

### 8.5 The two capabilities

After every step settles, the agent asks the decision model what comes next.

**System 1 — the decision model.** One request carries the rendered playbook, the remaining budgets, the history of the run as outcomes, and — for the last action — its full outcome and the tail of its transcript. The request asks three questions:

| Question | Shape | Asked |
|---|---|---|
| `next` | choice — the commands the policy accepts now, plus `done` and `escalate`; each option's description is the step's note | always |
| `needs_human` | yes/no — does the next step need the user's judgement or credentials | always |
| `step_succeeded` | yes/no — did the last step work | only when the last outcome is `unknown` |

The decision is acted on without the chat model when all of these hold:

- `next` is not `escalate`;
- the confidence of `next` is at least `min_confidence`;
- `needs_human` is below 0.5;
- the policy allows it — `Policy.CheckRun` for a command, `Policy.CheckFinish` for `done`.

The decision is recorded in the conversation as a synthetic assistant tool call with a note (`decided by jev-1.14.0, p=0.93`), so the chat model reads a consistent history when it takes over. The model that answered is stored with the run.

**System 2 — the chat model.** The standard tool-calling shape: call the model with the tools, run the tool calls, append the results, repeat until `finish`, a pause or a budget limit. The system prompt is the playbook: the role, `Policy.Render()`, and how to read an outcome — the status is the primary evidence, the transcript is context.

Anything that needs text — an instruction, an answer to a prompt — always goes to System 2.

### 8.6 The three modes

`me` is enabled when at least one of `me.llm` and `me.decider` is configured.

| `me.llm` | `me.decider` | Behaviour |
|---|---|---|
| set | unset | System 2 only: the tool loop decides every step. |
| unset | set | System 1 only: confident decisions run, everything else asks the user. |
| set | set | System 1 first, with System 2 handling the escalations. |

Without a chat model, an escalation becomes a question for the user: the agent lists the options and `/me reply <option>` picks one. An action that needs text goes to `ask_human` in that mode.

### 8.7 The configuration

```yaml
me:
  llm:
    provider: openai              # a registered chat provider: openai | anthropic
    base_url: https://api.openai.com/v1
    model: gpt-4.1-mini
    api_key_env: OPENAI_API_KEY   # the key is read from the environment, never stored in the config
    max_tokens: 4096
  decider:
    protocol: decisions           # decisions | systemone
    base_url: https://openrouter.ai/api/v1
    model: typesafe/jev-1.14
    api_key_env: OPENROUTER_API_KEY
    min_confidence: 0.8           # below this the agent escalates
```

The decider's base is the root of the API; the version segment is added
when the base does not carry it. The two protocols live at
`{base}/v1/decisions` and `{base}/v1/systemone` — `https://api.openai.com`
and `https://api.typesafe.ai` are the roots of the two APIs.

The user picks the models. nightme never chooses one, and it never injects a model into the coding agents — those keep their own configuration.

`me.llm` and `me.decider` are independent. A model that is configured but cannot be built — an unknown provider, a missing key — is reported in the daemon log and left out; the daemon still starts, and `/me` says why it cannot run.

### 8.8 The `/me` commands

| Command | Behaviour |
|---|---|
| `/me start <playbook> [args...]` | Starts a playbook in this chat. The positional arguments map onto the playbook's declared arguments, in order. The run inherits the chat's workspace and agent. A chat without a workspace is refused. A second run in the same chat is refused. |
| `/me reply <text>` | Answers the question the run is waiting for. |
| `/me stop` | Cancels the run and stops the work chat. |
| `/me status` | Reports the current step, the phase and progress of the running command, the remaining budget, and what each capability decided and spent. |

One run is active per chat: the chat that starts a run is the chat that receives the progress and answers the questions.

### 8.9 The outcome model

The run judges from facts, not from text. Every command the agent sends reports a structured outcome.

| Status | Meaning |
|---|---|
| `ok` | The step did what it was asked to do. |
| `noop` | There was nothing to do — the request was valid, the world already satisfied it. |
| `incomplete` | The step partly worked; the rest is still open. |
| `failed` | The step did not work. |
| `cancelled` | Somebody stopped it. |
| `timeout` | It ran out of time. |
| `unknown` | Nobody reported a verdict. |

A step that is still running reports `waiting`, and a step whose execution is gone reports `interrupted`.

The progress the observer receives is built from the outcome:

```
▶ /gtw fix 42 -y
✅ /gtw fix 42 -y
▶ /review
⚠️ /review incomplete · 1/3 groups unfinished
✅ issue 42 is on its way · 6 actions · system-1: 0 decisions, 0 tokens · system-2: 8 decisions, 4210 tokens
```

### 8.10 Guardrails and recovery

| Guardrail | Bound |
|---|---|
| Loop iterations | Three times the playbook's action budget, unless the configuration says otherwise. |
| Token budget | The tokens one run may spend on the chat model. |
| Step timeout | Thirty minutes per command. |
| Stall detection | Ten minutes without a heartbeat. |
| Action budget | `max_actions` for the run, `max` for a step. |
| Compaction | Every tool result but the last keeps its status and summary; the last one keeps its transcript. |

A text-only answer — words without a tool call — is nudged once to call a tool. The second time, the run ends as a failure.

The run is persisted after every tool result: the bound playbook, the policy's history, the whole conversation and the paused state. On startup, running runs resume:

- a command that was in flight is resolved through the loopback; if its execution is gone, it reports `interrupted`;
- a suspended run keeps waiting for `/me reply`.

Recovery is silent. The observer is told only when a run cannot go on — an authentication error from the model, for instance.

### 8.11 The workflow service

The cron- and event-driven workflows of §1–§7 run in `internal/me/workflow`. It is a runtime service, not a channel: it never receives inbound messages and it renders nothing. It drives the daemon through the loopback channel's client, the way a person drives it through Feishu.

Its run state lives in `<DataDir>/workflows/state/<run-id>.json`, and the work chat of a run is a loopback chat with the `lb_` prefix.

The workflow service and the personal agent share the workflows directory and the loopback channel. They do not share a run: the workflow service's runs are triggered by cron and git events, the personal agent's runs are started by `/me start` in a chat.

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
| Hot-reload of user-defined action scripts | Bot restart picks them up; live reload is a follow-up. |

---

## 10. Cross-references

- **Trigger source**: GitHub / GitLab webhook payloads (provider-agnostic; the GitProvider abstraction normalizes them).
- **Mention parsing**: `@owner <command> ...` text is parsed by bot itself (no feishu involvement). The first word after `@owner` is the command.
- **Agent names** (`codex`, `claude`, `pi`, `opencode`, `dsh`): come from the `agents:` block of `~/.nightme/config.yaml`.
- **`bot` channel**: `internal/channel/bot/`, peer of `feishu` and `echo`. Implements the `Channel` interface *and* the `wfe.Runtime` interface; no new control plane.
- **`wfe` library**: `internal/wfe/`. Pure Go, no I/O, no clock, no secrets. Single entry point: `Tick(state, runtime) → state'`. Bot is its only caller.
- **Implementation details** (state machine, run state serialisation, event store, ActionRegistry design, gw client integration, rollout phases): [`feat/F-workflow-engine.md`](./feat/F-workflow-engine.md).
