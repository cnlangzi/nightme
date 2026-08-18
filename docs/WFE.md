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
  - [8.1 The `bot` subsystem — host](#81-the-bot-subsystem--host)
  - [8.2 The `wfe` library — pure](#82-the-wfe-library--pure)
  - [8.3 Architecture diagram](#83-architecture-diagram)
  - [8.4 Runtime interface — the injection point](#84-runtime-interface--the-injection-point)
  - [8.5 Action injection model](#85-action-injection-model)
  - [8.6 Trigger detection — bot's own concern (3-stage filter pipeline)](#86-trigger-detection--bots-own-concern-3-stage-filter-pipeline)
  - [8.7 `prompt` step via the nightme channel](#87-prompt-step-via-the-nightme-channel)
  - [8.8 Run state &amp; persistence](#88-run-state--persistence)
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

This section answers "where do workflow runs actually execute, and how do the pieces fit?" Sections §1–§7 defined the YAML schema; this one defines how the engine is wired into the existing nightme runtime.

### 8.1 bot is a Channel

> **🔒 Design invariant (locked)**: bot is a channel, full stop. bot does **not** hold any reference to nightme's internal types (`chatsession.Manager`, `ChatSession`, `AgentSession`, `Gateway` internals, `/gtw fix` Go API). bot's **only** path into nightme is `bot.Incoming() → gateway → inbound.Dispatch → ChatSession → … → outbound.Emitter → bot.Send()`. Anything bot wants nightme to do must be expressed as a message (a plain text prompt, or a slash command like `/cwd`, `/use agent`, `/gtw fix`).
>
> **Outgoing side-effects** (notify, email, github action) are bot's **own** resource channels — they call external APIs (feishu HTTP, SMTP, GitHub REST) directly, not nightme internals. This is fine; the constraint only covers bot → nightme runtime.

`bot` lives at **`internal/channel/bot/`** and **implements the `channel.Channel` interface** (the same interface as `feishu` and `echo`). bot is registered in `gateway.AttachChannels(bot, feishu, echo, …)` and goes through the exact same channel pipeline as any other channel.

```
person → types in feishu
  → feishu channel → gateway → ChatSession → agent
  → reply flows back via feishu.Send → user sees it

workflow fires
  → bot synthesizes a message
  → bot channel → gateway → ChatSession → agent       ← SAME PATH
  → reply flows back via bot.Send → workflow captures it
```

**bot replaces a person in front of a chat platform.** That's the entire conceptual model. The gateway doesn't know or care whether a message came from feishu, echo, or bot — it just dispatches.

| Channel | "User" | Message source |
|---|---|---|
| `feishu` | Real person typing in feishu | feishu webhook |
| `echo` | Test harness | `echo.Inject` (in-process) |
| **`bot`** | **The workflow YAML scheduler** | **bot synthesizes and pushes into its own `Incoming()`** |

**Why bot must be a Channel (not a subsystem)**:

- The gateway's `dispatchLoop` reads from every channel's `Incoming()` and routes via `inbound.Dispatch`. There's no "system trigger" branch — only channels.
- Slash commands (`/cwd`, `/use agent`, `/gtw fix`) are dispatched by `tryCommandDispatch`. A workflow that wants to invoke `/gtw fix` must do so by sending a `/gtw fix …` message through the channel pipeline.
- Agent replies flow back via `outbound.Emitter → channel.Send`. bot's `Send` is the only natural way to capture them.

If bot tried to bypass the channel and call `ChatSession` directly, it would:

- Skip `inbound.Dispatch` — `/gtw fix` and other slash commands wouldn't work.
- Have no `outbound.Emitter → Send` callback for replies — it would have to invent a separate reply path.
- Diverge from the architectural model that `feishu`/`echo` already follow.

**Bot's responsibilities**:

1. **Load workflows** from `~/.nightme/workflows/*.yaml` and validate.
2. **Run its own trigger manager** (cron timers, git event subscriptions). Triggers are bot's private concern — nightme doesn't see them.
3. **When a trigger fires**, derive a fresh chatID, register a per-run reply channel, and synthesize the first message (`/cwd <workspace>`) into `bot.Incoming()`.
4. **Drive `wfe.Tick`** for each run in a goroutine. For `prompt` steps, push a message via `bot.Incoming()` and wait on the per-run reply channel. For `run` steps, `os/exec`. For `use` steps, `ActionRegistry`.
5. **Capture agent replies** delivered via `bot.Send` (called by the gateway's `outbound.Emitter`) and deliver them to the waiting `wfe.Tick` goroutine through the per-run reply channel.
6. **Persist run state** to `~/.nightme/workflows/state/<run-id>.json` and recover after restart.
7. **Register action channels** (built-in + user-defined scripts).

**Iron rule**: bot never holds a `chatsession.Manager` reference, never calls `ChatSession.*` directly, never talks to the gateway's `dispatchInbound` outside the channel protocol. Everything goes through `bot.Incoming()` and `bot.Send()`.

### 8.2 The `wfe` library — pure

`internal/wfe/` is a **pure Go library**. It has zero knowledge of gw, chatsession, agentsession, the file system beyond the YAML it parses, the network, secrets, or even the current time. Every external dependency is injected through a `Runtime` interface that the bot provides.

What wfe owns:

- YAML parsing, schema validation, type definitions (`Workflow`, `Step`, `RunState`, `Event`).
- Event → workflow matching (pure function).
- The state machine: `Tick(state, runtime) → (state', error)`. One step at a time, fully pure.
- Expression evaluation: `${{ event.x }}`, `${{ steps.y.outputs.z }}`, `${{ env.K }}`.
- Step dispatch: maps each step kind to a `Runtime` call.

What wfe does **not** own:

- Clocks. `Runtime.Now()` is the only way to read time.
- Secrets. They're just env vars, resolved by the bot before the run starts.
- I/O of any kind. No file system, no network, no subprocess.
- The trigger source. The bot decides when to call `Tick`.

This split is what makes wfe trivially testable: pass a mock `Runtime`, pass a `RunState`, observe the new `RunState`.

### 8.3 Architecture diagram

```
  ┌─ bot's own trigger sources ────────────────┐
  │  cron timer (robfig/cron)                  │
  │  PR / issue / branch / mention (git events)│
  │  ── all internal to bot ──                 │
  └─────────────────┬──────────────────────────┘
                    │ trigger events
                    ▼
  ┌──────────────────────────────────────────────────┐
  │   internal/channel/bot  (a Channel, peer of      │
  │                          feishu, echo)           │
  │                                                  │
  │   implements channel.Channel interface:          │
  │     Name() / Start() / Stop()                    │
  │     Incoming()  ← bot pushes synthesized msgs     │
  │     Send()      ← gateway delivers agent reply   │
  │                                                  │
  │   internal state:                                │
  │     workflows []*wfe.Workflow                    │
  │     triggers  (cron + git events)                │
  │     actions   ActionRegistry                     │
  │     stateStore                                    │
  │     runsByChatID map[chatID]*botRun             │
  │       each botRun has its own reply channel       │
  │                                                  │
  │   per-run lifecycle:                             │
  │     trigger fires                                │
  │       → derive chatID + runID                    │
  │       → spawn botRun (with reply channel)         │
  │       → go driveRun(r):                          │
  │            for !r.state.Done() {                  │
  │                r.state = wfe.Tick(state, wf, rt)   │
  │                save(state)                        │
  │            }                                      │
  │       → cleanup botRun                           │
  │                                                  │
  │   runtime 4 methods:                             │
  │     RunShell:   os/exec (cwd = run.workspace)    │
  │     SendPrompt: push msg → bot.Incoming(),       │
  │                 wait on r.reply chan               │
  │     RunAction:  ActionRegistry                   │
  │     Now:        time.Now                          │
  └────────────────────┬─────────────────────────────┘
                       │ bot.Incoming() / bot.Send()
                       │ (跟 feishu 完全一样的 channel 协议)
                       ▼
  ┌──────────────────────────────────────────────────┐
  │   nightme gateway (完全复用)                       │
  │                                                  │
  │   AttachChannels(bot, feishu, echo, ...)         │
  │   pumpInbound: 读每个 channel.Incoming()           │
  │   dispatchLoop:                                  │
  │     channelCh → inbound.Dispatch                  │
  │     → tryActionDispatch                          │
  │     → tryCommandDispatch  (e.g. /cwd /use /gtw fix)│
  │     → tryShellDispatch                            │
  │     → tryMessageDispatch                         │
  │   → ChatSession(chatID)                          │
  │   → /cwd 设 cwd (第一步)                          │
  │   → /use agent 设 agent                          │
  │   → /gtw fix 创建 worktree (off main)             │
  │   → AgentSession → agent → reply                │
  │   reply: outbound.Emitter → channel.Send          │
  │     → bot.Send(msg) 收                           │
  │     → bot 查 runsByChatID[msg.ChatID]             │
  │     → 把 msg.Text 投到 botRun.reply               │
  │     → workflow 的 SendPrompt 等到 reply, 继续 Tick  │
  └──────────────────────────────────────────────────┘
```

Solid lines are the path a single workflow run takes. bot.Incoming → gateway → ChatSession → agent → reply → bot.Send → botRun.reply → wfe.Tick. This path is **identical to the path a feishu-typed message takes**; the only difference is the source (synthesized by bot vs. typed by a human) and the sink (bot's reply chan vs. feishu's API).

### 8.4 Resource channels — the injection points

wfe is a pure library. It has no resources of its own — to run anything, it calls out through a `Runtime` interface that the bot injects. **Each method on `Runtime` is a channel from bot to a specific resource.** nightme is just one of those resources.

| Method | Channel | Resource |
|---|---|---|
| `RunShell` | shell channel | `os/exec` (direct subprocess) |
| `SendPrompt` | **nightme channel** | nightme runtime — bot pushes msg to `bot.Incoming()`; gateway dispatches to ChatSession; reply flows back via `bot.Send` (same shape as feishu user typing) |
| `RunAction: notify` | notify channel | feishu / slack / webhook IM (via bot's `ActionRegistry`) |
| `RunAction: email` | email channel | SMTP |
| `RunAction: github_pr_comment` | github channel | GitHub API (via `gitProvider` abstraction) |
| `RunAction: <user-script>` | user-script channel | `~/.nightme/workflows/actions/<name>.<ext>` |
| `Now` | (clock — not a channel) | `time.Now` |

The conceptual symmetry with nightme's own `Channel` interface is intentional:

- nightme has `Channel` implementations (`feishu`, `echo`) for **receiving** messages from user-facing sources.
- bot has "resource channels" for **sending** task-execution messages to backend resources.

The `Runtime` interface in Go is just a typed union of the channels the workflow step grammar can dispatch to:

```go
// internal/wfe/runtime.go
type Runtime interface {
    // shell channel
    RunShell(ctx, ShellSpec) (*ShellResult, error)
    
    // nightme channel (agent task execution)
    SendPrompt(ctx, PromptSpec) (*Reply, error)
    
    // action channels (one method, many backends resolved by name)
    RunAction(ctx, ActionSpec) (*ActionResult, error)
    
    // clock (injected so wfe has no time dependency)
    Now() time.Time
}
```

The bot provides one implementation. The implementation is what wires each method to its resource:

| Method | Backed by |
|---|---|
| `RunShell` | `os/exec` in the bot process |
| `SendPrompt` | Push msg into `bot.Incoming()`; block on per-run reply channel; reply comes via `bot.Send` (see §8.7) |
| `RunAction` | `ActionRegistry.Run` (resolves name → action implementation) |
| `Now` | `time.Now` |

**Why nightme is one channel among many**: workflow steps don't only need agents. They need to `run` arbitrary shell, `notify` users, post to `github`, send `email`, etc. The nightme channel is privileged (it can invoke an agent), but it's not special in the type system — it's one entry in a fixed list of channels that the step grammar knows about. Adding a new "always available" capability means adding a new method to `Runtime` (and a new step kind); adding a new "nameable" capability means registering a new entry in `ActionRegistry`.

The `Runtime` is what the bot hands to wfe when calling `Tick`. wfe holds it for the duration of one step, then returns the new state. The next step can see a different `Runtime` (in practice, never, but the type system permits it).

### 8.5 Action resource channels

`use:` is the **extension point** beyond `run` and `prompt`. While `run` and `prompt` are first-class channels (their step kind is enough to dispatch), `use` is **name-dispatched** — wfe doesn't know what actions exist, it just calls `Runtime.RunAction(spec)` with a name. The bot's `ActionRegistry` is what resolves the name to a concrete action channel.

In channel terms: **`run`, `prompt`, `use` are all resource channels**. The first two have fixed method names in `Runtime`; the third is a multiplexer over a dynamic name set.

```go
// internal/channel/bot/action.go
type Action interface {
    Name() string                                  // channel name (matches `use: <name>`)
    Execute(ctx, args map[string]any, env map[string]string) (*ActionResult, error)
}

type ActionRegistry struct {
    actions map[string]Action
}
```

**Built-in action channels** (registered in Go at bot startup):

| Action name (`use:`) | Channel to | Backed by |
|---|---|---|
| `notify` | notify channel (feishu / slack / webhook IM) | Bot's channel registry + HTTP |
| `email` | email channel | SMTP client |
| `github_pr_comment` | github channel (PR comments) | `gitProvider` abstraction (F-50) |
| `github_issue` | github channel (issues) | Same |
| `slack` | slack channel | Slack API |
| `webhook` | webhook channel | `http.Client` |

**User-defined action channels** (auto-discovered at bot startup):

```
~/.nightme/workflows/actions/
├── deploy-staging.sh        # auto-registered as channel "deploy-staging"
├── rotate-secrets.py        # auto-registered as channel "rotate-secrets"
└── notify-custom.sh         # auto-registered as channel "notify-custom"
```

The bot wraps each script as a `ShellAction` (a script-backed channel):

```go
type ShellAction struct {
    ScriptPath string
}

func (a *ShellAction) Name() string {
    return strings.TrimSuffix(filepath.Base(a.ScriptPath), filepath.Ext(a.ScriptPath))
}

func (a *ShellAction) Execute(ctx, args, env) (*ActionResult, error) {
    // args serialised to env (KEY_UPPER=json) for shell-side consumption
    // + JSON file for complex consumption
    // stdout is expected to be JSON {"outputs": {...}}; non-JSON → empty outputs
    return parseActionOutput(stdout)
}
```

This means **adding a new action channel doesn't require changing wfe or rebuilding the bot binary** — drop a script in the directory and restart the bot (or hot-reload, see open questions). The Go-level built-ins (`notify`, `email`, `github_*`, etc.) ship with the bot; user-scripts are the open extension surface.

Unknown `use:` names fail the run with a clear error: `unknown action: "foo" — registered: [notify, email, github_pr_comment, …]`.

### 8.6 Trigger detection — bot's own concern (3-stage filter pipeline)

Triggers are **not** a nightme feature. The five trigger kinds in `on:` (`schedule`, `pull_request`, `branch`, `issue`, `mention`) are bot's private input sources — nightme's `Channel` / `Gateway` / `ChatSession` subsystems are not involved in detecting them.

The flow has three explicit stages: **receive → filter → trigger**.

#### 8.6.1 Stage 1 — Receive (passive)

bot subscribes once to `gitProvider` and gets **all events** for repos the token can see. No per-workflow subscription.

| Trigger kind | How bot receives it |
|---|---|
| `schedule` | bot runs `robfig/cron` internally, ticks evaluate each workflow's cron list |
| `pull_request` / `issue` / `branch` / `mention` | bot subscribes to `gitProvider` once; receives **all** PR/issue/branch/mention events the API delivers |

#### 8.6.2 Stage 2 — Filter (active, per workflow's `workspaces`)

For every event, bot filters through each workflow's `workspaces` list before triggering:

```
for each event:
    if event.kind == "schedule":
        workspace = nil   # cron 无 event.repo
    else:
        workspace = wsMap.byRepo[event.repo]    # ~/.git/config 反查
        if workspace == "": return              # 事件 repo 不在 bot 的 workspaces 里，丢弃

    for each workflow:
        if workspace != nil and !contains(workflow.workspaces, workspace):
            continue    # 这个 workflow 不覆盖此 workspace
        if !wfe.Match(workflow, event):
            continue    # 这个 workflow 的 on: 不匹配此 event 类型
        fireWorkflow(workflow, event, workspace)
```

`wsMap` is built at bot startup by reading `git -C <workspace> remote get-url origin` for every workspace mentioned in any workflow's `workspaces` list, and normalizing the URL to `owner/repo`. Cached; refreshed on workflow reload.

#### 8.6.3 Stage 3 — Trigger (execute)

For each (workflow, event, workspace) match, bot starts a fresh run (new `runID` + new `chatID` + new worktree via `/gtw fix`).

This means:

- The `Channel` interface is **not** implemented by bot. There's no `Incoming()` channel on bot that anyone pumps.
- The `Gateway.dispatchLoop` never sees a trigger event.
- The `inbound.Dispatch` chain is unchanged. No "system trigger" branch.
- bot has no `Send()` outbound — `notify` and other side-effect actions reach channels via bot's own `ActionRegistry`, not via the gateway.

This isolation is what lets bot ship independently of nightme's dispatch chain evolution. If nightme later refactors gateway, bot is unaffected. If bot adds a new trigger kind, nothing in nightme changes.

### 8.7 `prompt` step via the nightme channel

A `prompt:` step is, at runtime, **bot submitting a task-execution message through the nightme channel**. The message says "execute this prompt in this chat with this agent, give me the reply". This is the same shape as `/gtw fix` asking nightme to spawn an agent in a chat:

```yaml
- prompt: "Review ${{ event.title }}"
  agent: codex
```

…is realized as:

> `botRuntime.SendPrompt` synthesizes an inbound message (chatID, body, meta) and pushes it into `bot.Incoming()`
> → gateway's `pumpInbound` reads it → `channelCh`
> → `dispatchLoop` calls `DispatchInbound(msg)`
> → `inbound.Dispatch` → `tryMessageDispatch` → `ChatSession(chatID)` (which has already had `/cwd` and `/use agent` applied at run start)
> → `AgentSession(codex)` (already running in the worktree) processes the prompt
> → agent reply flows back: `AgentSession` → `outbound.Emitter` → `bot.Send(msg)`
> → `bot.Send` looks up `runsByChatID[msg.ChatID]` and delivers `msg.Text` to the per-run reply channel
> → `botRuntime.SendPrompt` unblocks with `&wfe.Reply{Text: msg.Text}`, wfe records the output under `steps.<id>.outputs.*`
> → wfe returns the advanced state to the bot
> → bot saves the new state to disk

The AgentSession pool semantics are unchanged: a `prompt` step in a given chat+agent+CWD triple **reuses** an existing live session if one is running, just like a `/gtw fix` or a human typing in that chat. Cold start only when no live session exists for that triple.

**`run` and `use` steps do not use the nightme channel.** They go through their own channels:

- `run` → shell channel (direct `os/exec` inside bot's process)
- `use` → the named action channel (resolved by `ActionRegistry`)

Only `prompt` reaches the nightme channel, because only `prompt` needs an LLM agent to think and reply. Everything else is more direct: shell runs locally, github hits GitHub's API directly, notify hits the IM API directly, etc. The nightme channel is the heaviest of the channels because it spans two processes and an LLM — but architecturally it's a peer of the others.

### 8.8 Run state & persistence

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

- **Daemon restart**: in-flight runs are reloaded from disk; the bot resumes the `Tick` loop from `current_step` (re-evaluates `if:` conditions; re-runs idempotent steps; `prompt` resumes a live session if still alive).
- **Failure recovery**: a failed step leaves the run in `failed`; the user (or the next trigger) can decide whether to retry.
- **Worker pool** (§2 `worker: N`): the bot caps concurrent runs of the same workflow at N. Excess triggers queue (FIFO) and run when a worker frees up.

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
| Hot-reload of user-defined action scripts | Bot restart picks them up; live reload is a follow-up. |

---

## 10. Cross-references

- **Trigger source**: GitHub / GitLab webhook payloads (provider-agnostic; the GitProvider abstraction normalizes them).
- **Mention parsing**: `@owner <command> ...` text is parsed by bot itself (no feishu involvement). The first word after `@owner` is the command.
- **Agent names** (`codex`, `claude`, `pi`, `opencode`, `dsh`): come from the `agents:` block of `~/.nightme/config.yaml`.
- **`bot` channel**: `internal/channel/bot/`, peer of `feishu` and `echo`. Implements the `Channel` interface *and* the `wfe.Runtime` interface; no new control plane.
- **`wfe` library**: `internal/wfe/`. Pure Go, no I/O, no clock, no secrets. Single entry point: `Tick(state, runtime) → state'`. Bot is its only caller.
- **Implementation details** (state machine, run state serialisation, event store, ActionRegistry design, gw client integration, rollout phases): [`feat/F-workflow-engine.md`](./feat/F-workflow-engine.md).
