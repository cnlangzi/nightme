# nightme Workflow — Configuration Reference

> **Status**: The workflow schema and the personal agent that runs it.
> **Scope**: The YAML schema, the plan the engine computes, the judgment points, and the runtime that executes them.

A **workflow** is a YAML file that nightme runs. It answers two questions:

1. **What** does it do? (`jobs.<name>.steps` — ordered steps)
2. **How far** may it go? (`max_actions`, `max`, `allow`)

The personal agent (`/me`) runs a workflow in a chat. The same file is the plan the agent follows and the bound it may not cross.

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
  - [5.1 `message` — talk to the work chat](#51-message--talk-to-the-work-chat)
  - [5.2 `shell` — execute shell](#52-shell--execute-shell)
  - [5.3 `use` — invoke a registered action](#53-use--invoke-a-registered-action)
  - [5.4 Common step fields](#54-common-step-fields)
- [6. Expressions (`${{ ... }}`)](#6-expressions--)
- [7. Complete examples](#7-complete-examples)
- [8. Runtime architecture](#8-runtime-architecture)
  - [8.1 The layering](#81-the-layering)
  - [8.2 The plan: the ready set](#82-the-plan-the-ready-set)
  - [8.3 The judgment points](#83-the-judgment-points)
  - [8.4 The policy](#84-the-policy)
  - [8.5 The tools](#85-the-tools)
  - [8.6 The two capabilities](#86-the-two-capabilities)
  - [8.7 The three modes](#87-the-three-modes)
  - [8.8 The configuration](#88-the-configuration)
  - [8.9 The `/me` commands](#89-the-me-commands)
  - [8.10 The outcome model](#810-the-outcome-model)
  - [8.11 Guardrails and recovery](#811-guardrails-and-recovery)
- [9. Out of scope](#9-out-of-scope)
- [10. Cross-references](#10-cross-references)

---

## 1. File location & layout

Workflows are read from:

```
~/.nightme/workflows/*.yaml
```

- One workflow per file; the filename is informational — the `name:` field is the identifier, and duplicates are a load error.
- The workflows are read when the daemon starts. After you add or change one, run `nightme restart`.
- The action scripts live in `~/.nightme/workflows/actions/`, and the run state in `~/.nightme/workflows/runs/`.

---

## 2. Top-level fields

| Field | Type | Required | Default | Purpose |
|---|---|---|---|---|
| `name` | string | ✅ | — | Unique workflow identifier; surfaces in logs and notifications. |
| `args` | arg[] | ❌ | — | The arguments the workflow takes. A spec is a mapping with `name` and an optional `default`. |
| `goal` | string | ❌ | — | What the run is for. It is part of the model's instructions. |
| `max_actions` | int | ❌ | `30` | The action budget of the whole run. |
| `instruct` | bool | ❌ | `true` | Whether the agent may send free-form instructions to the work chat. |
| `workspaces` | string[] | ❌ | — | The workspaces the workflow names. The run takes the workspace of the chat that starts it. |
| `on` | object | ❌ | — | Trigger spec (see §3). Parsed and displayed; nothing fires it. |
| `jobs` | map | ✅ | — | Named jobs, each with ordered `steps` (see §4). |

A workflow names **no agent**. The run uses the agent of the chat that started it — the chat's `/use` choice, or its primary agent.

Minimal skeleton:

```yaml
name: my-workflow
jobs:
  main:
    steps:
      - message: /gtw commit
```

**Full example** (every top-level field + every step kind + every trigger kind):

```yaml
name: full-demo
args:
  - name: issue
    default: "*"                          # a default makes the argument optional
goal: Fix an issue and ship it as a PR.

workspaces: [~/work/nightme]              # optional: the workspaces this names

on:                                        # parsed and displayed; nothing fires it
  schedule:
    - cron: '0 9 * * *'                    # nightly at 9 AM
  pull_request:
    branches: [main, develop]
    events: [opened, synchronize]
  branch:
    patterns: ['release/*']
    events: [pushed]
  issue:
    events: [opened, labeled]
  mention:
    commands: [review, fix]

jobs:
  prepare:                                  # one job
    steps:
      - id: setup
        shell: echo "starting at $(date)"
        env:
          TZ: UTC

  ship:                                     # another job; waits for prepare
    needs: [prepare]
    steps:
      # 1. shell step: run a script
      - id: lint
        shell: ./scripts/lint.sh
        continue-on-error: true             # the chain continues when the step fails

      # 2. message step: talk to the work chat
      - id: fix
        if: ${{ success() }}
        message: "/gtw fix ${{ args.issue }} -y"
        env:
          REVIEW_DEPTH: deep

      # 3. message step that repairs the step before it
      - id: review
        message: "/review"
        note: 没跑完就 /review -c；有改动就先 /gtw commit
        allow: ["/review -c", "/gtw commit"]
        max: 5

      # 4. use step: invoke a registered action
      - id: notify
        if: ${{ always() }}
        use: notify
        with:
          channel: feishu
          target: oc_xxx
          message: "issue ${{ steps.fix.outputs.data.issue }} done"

      # 5. use step: user-defined action script
      - id: cleanup
        if: ${{ always() }}
        use: cleanup-tmp-files
        with:
          paths: [/tmp/pr]
```

**Why `workspaces` is optional**: a workflow runs in the workspace of the chat that starts it. The list names the workspaces the workflow is meant for; it is not a filter that decides where the run happens.

---

## 3. Triggers (`on:`)

The trigger block is one of five shapes. It is parsed and displayed — `nightme workflow show <name>` prints it — and nothing fires it: a workflow is started by a person, with `/me start <name>` in a chat.

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

Fires when **the owner is @-mentioned anywhere that the underlying git provider's mention API can deliver** (sourced via `gitProvider` — this is **not** related to Feishu chat mentions). The "owner" is the GitHub/GitLab user that the `gitProvider` credentials authenticate as.

| Field | Type | Default | Purpose |
|---|---|---|---|
| `commands` | string \| string[] | (all) | Whitelist of mention commands. Omit to fire on any mention. |

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

`jobs` is a map of named job definitions. Each job runs its `steps` in order. A job waits for the jobs it `needs`.

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
| `if` | string | ❌ | Condition expression (see §6). A job whose condition does not hold is skipped. |
| `steps` | step[] | ✅ | Ordered steps to execute. |

A workflow runs one job graph in one work chat: the branches of the graph express ordering and branching, never concurrent work on separate workspaces.

---

## 5. Steps

Each step carries exactly one of three kinds. A step with two kinds, or with none, is refused when the file is loaded.

| Kind | Field | Use case | Where it runs |
|---|---|---|---|
| `message` | `message: <text>` | Talk to the work chat | the work chat |
| `shell` | `shell: <cmd>` | Execute shell | the workspace |
| `use` | `use: <action>` + `with: <args>` | Invoke a registered action | the action registry |

`message` and `shell` are **default-supported** — they work in every workflow. `use` is the **extension point** — the agent registers which action names are valid.

### 5.1 `message` — talk to the work chat

The text goes into the work chat. A `/`-prefixed text is a slash command the daemon interprets; anything else is a turn for the work agent. Both settle with an outcome.

```yaml
# A slash command
- id: commit
  message: "/gtw commit"

# A free-form turn
- id: explain
  message: |
    Summarise the changes of the last commit in one line.
```

The agent's own commands — the ones it may pick at a judgment point — are the steps' `message:` values and the `allow:` entries. A command that is neither comes back to the agent as an error it can correct.

### 5.2 `shell` — execute shell

```yaml
# Inline single-line
- id: lint
  shell: npm run lint

# Inline multi-line (literal block)
- id: build
  shell: |
    set -euo pipefail
    echo "Building..."
    make build

# Script file
- id: checkout
  shell: ./scripts/checkout.sh
```

The command runs in the workspace of the run. The step's `env:` is layered on top of the daemon's own environment, so the shell finds `PATH`, `HOME` and the git/ssh configuration a real command needs.

`shell` is the right tool when you need arbitrary side effects. Reuse is by writing shell scripts (e.g. under `~/.nightme/workflows/actions/`), not by importing external action packages.

### 5.3 `use` — invoke a registered action

`use` is the **extension point** for any capability beyond shell and chat. The YAML passes arguments via `with:`.

```yaml
# Built-in actions
- id: send-notif
  use: notify
  with:
    channel: feishu
    target: oc_xxx
    message: "PR done"

- id: post-pr
  use: github_pr_comment
  with:
    pr: 123
    body: "Auto-reviewed by nightme"

# User-defined action (script in ~/.nightme/workflows/actions/)
- id: deploy
  use: deploy-staging
  with:
    env: production
```

| Field | Type | Required | Purpose |
|---|---|---|---|
| `use` | string | ✅ | Action name. Resolved against the action registry. |
| `with` | map | ❌ | Action-specific arguments. Values may embed `${{ ... }}` expressions. |

**Available actions**:

- **Built-in**: `notify`, `email`, `github_pr_comment`, `github_issue`.
- **User-defined** (executable files under `~/.nightme/workflows/actions/<name>.<ext>`): auto-registered as actions when the agent starts. stdout must be JSON `{"outputs": {...}}` for typed outputs, otherwise outputs are empty.

An unknown `use:` name fails the step with a clear error: `unknown action: "foo" (registered: [notify, email, github_pr_comment, github_issue])`.

### 5.4 Common step fields

Every step supports:

| Field | Type | Default | Purpose |
|---|---|---|---|
| `id` | string | `<job>#<position>` | The step's name. Only a step that declares one can be named in an expression. |
| `if` | string | (always true) | Condition expression. A step whose condition does not hold is skipped and the chain continues. |
| `env` | map | — | Environment variables for the step (merged on top of the run's env and the daemon's env). |
| `continue-on-error` | bool | `false` | Whether the chain continues when this step fails. |
| `note` | string | — | Why the step is there, and when the extra commands apply. It is the description the decision model reads. |
| `allow` | string[] | — | The extra commands the agent may use inside the step. |
| `max` | int | (no limit) | The number of actions the step may take. `max: 0` is refused. |

---

## 6. Expressions (`${{ ... }}`)

Any string value can embed expressions. Expressions are evaluated when the step runs, except the `${{ args.* }}` ones, which are resolved when the run starts.

| Expression | Resolves to |
|---|---|
| `${{ args.NAME }}` | The value of the argument `NAME`. |
| `${{ steps.<id>.outputs.status }}` | The status of the step `<id>` — `ok`, `noop`, `incomplete`, `failed`, `cancelled`, `timeout`, `unknown`. |
| `${{ steps.<id>.outputs.summary }}` | The one-line summary of the step `<id>`. |
| `${{ steps.<id>.outputs.data.<key> }}` | One of the facts the step `<id>` reported. |
| `${{ needs.<job>.* }}` | The outputs of the last step of the job `<job>`. |
| `${{ env.NAME }}` | The value of the environment variable `NAME`. |
| `${{ now }}` | The current time. |
| `${{ success() }}` | `true`. |
| `${{ failure() }}` | `false`. |
| `${{ always() }}` | `true`. |
| `${{ cancelled() }}` | `false`. |

An expression that names an argument the workflow does not declare is refused when the file is loaded.

---

## 7. Complete examples

### 7.1 Fix an issue and ship it

```yaml
name: fix-issue
args:
  - name: issue
    default: "*"
goal: 把队列里的一个 issue 做完并推上去
max_actions: 30
jobs:
  ship:
    steps:
      - id: fix
        message: "/gtw fix ${{ args.issue }} -y"
        note: 从 nightme/assigned 队列里挑一个
        allow: ["/gtw abort"]
        max: 3
      - id: commit
        message: "/gtw commit"
      - id: review
        message: "/review"
        note: 没跑完就 /review -c；有改动就先 /gtw commit
        allow: ["/review -c", "/gtw commit"]
        max: 5
      - id: commit2
        message: "/gtw commit"
      - id: push
        message: "/gtw push"
```

### 7.2 Build, test and report

```yaml
name: ci
workspaces: [~/work/nightme]
on:
  schedule:
    - cron: '0 3 * * *'
jobs:
  build:
    steps:
      - id: compile
        shell: make build
      - id: test
        shell: make test
        continue-on-error: true
  report:
    needs: [build]
    steps:
      - id: notify
        use: notify
        with:
          channel: feishu
          target: oc_xxx
          message: "build ${{ steps.build.outputs.status }} on ${{ now }}"
```

### 7.3 A workflow with two branches

```yaml
name: triage
jobs:
  inspect:
    steps:
      - id: status
        message: "/gtw status"
  fix:
    needs: [inspect]
    steps:
      - id: commit
        message: "/gtw commit"
  notify:
    needs: [inspect]
    steps:
      - id: say
        use: notify
        with:
          channel: feishu
          target: oc_xxx
          message: "the tree was inspected"
```

---

## 8. Runtime architecture

This section answers "where do runs actually execute, and how do the pieces fit?". Sections §1–§7 define the YAML schema; this one defines the runtime that executes it.

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
  │  the fast path: the workflow says what comes    │
  │  next, so the step runs as it is — no model     │
  │                                                │
  │  a judgment point: the agent decides — the      │
  │  decision model (System 1) first, the chat      │
  │  model (System 2) when it is not sure           │
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
| Transport | `internal/channel/loopback` | Sending a command into a chat, tracking it, and reporting its outcome. Knows nothing about workflows. |
| The agent | `internal/me` | The run: what to do next, what the policy allows, what the observer is told. |
| The models | `internal/llm` | Two capabilities: `Provider` (chat and tool calling) and `Decider` (typed decision questions). |
| The engine | `internal/wfe` | The workflow schema, the plan of a run, the judgment points, and the `Policy` that bounds a run. |

The engine is a pure library: no I/O, no clock, no secrets. It parses, validates, computes the plan of a run and evaluates expressions — the runner owns the external capabilities and hands the engine their results.

The agent drives the daemon the way a person drives it through Feishu: every command is a real message with a real id, and every answer comes back as a structured outcome. It never reaches into the gateway or the chat sessions.

### 8.2 The plan: the ready set

The engine computes the plan of a run from the workflow and the progress so far.

The **ready set** is the set of unrun steps whose job's `needs` are all satisfied and whose `if:` holds. Within a job only the first such step counts — the steps of a job run in order.

| The workflow says | The plan is |
|---|---|
| one step in the ready set, and the previous outcome was `ok` or `noop` | run that step |
| one step in the ready set, and the previous outcome was anything else | a judgment point |
| more than one step in the ready set | a judgment point — the plan forks |
| no step in the ready set | a judgment point — the plan is exhausted |

A run that has not executed anything yet counts as `ok`, so the first step runs on the fast path.

### 8.3 The judgment points

A judgment point is where the workflow cannot decide and the agent does. It is reached when:

- the ready set is empty — the plan is exhausted;
- the ready set holds more than one step — the plan forks;
- the previous outcome was not `ok` or `noop` — the last step did not finish its work;
- the step itself failed;
- a command is still running and waiting for an answer.

The agent answers a judgment point with a **directive**:

| Directive | What it does |
|---|---|
| `run_step(command)` | Runs a command of the workflow in the work chat. |
| `instruct(text)` | Sends a free-form instruction to the work chat. |
| `answer_choice(choice_id, option_id)` | Answers a prompt the work agent is waiting for. |
| `ask_human(question)` | Asks the user. The run waits for the answer. |
| `repo_status()` | Reads the branch and the tree facts of the work chat. |
| `finish(status, summary)` | Ends the run. `status` is `done` or `failed`. |

A command the agent names has to be a step's `message:` or one of a step's `allow:` entries, compared with whitespace normalised away. Anything else comes back to the agent as an error it can correct. A command the deterministic path would not have run is allowed, and it is recorded against the step that owns it.

The engine owns the run state: after a directive is executed, the state holds the new outcome, the step that ran and the remaining budgets. A decision the agent makes never writes state by itself.

### 8.4 The policy

The policy is the bound on what a run may do. It is a pure function of the workflow and the actions taken so far.

| Check | Reports |
|---|---|
| `CheckRun(command)` | Whether the command may run now, and which step it belongs to. |
| `CheckStep(index)` | Whether the step may take an action now — its own limit and the run's budget. |
| `CheckInstruct()` | Whether a free-form instruction is allowed. |
| `CheckFinish(status)` | Whether the run may end. `done` needs the last executed step's latest outcome to be `ok` or `noop`; `failed` is always allowed. |
| `Remaining()` | The action budget left, overall and per step. |
| `Render()` | The workflow as a compact text block: the goal, the steps, the notes, the allowed commands and the limits. |
| `AllowedCommands()` | The commands the policy accepts right now. |

A command belongs to the first step that lists it, and a step that spent its limit takes its commands out of the list.

### 8.5 The tools

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

### 8.6 The two capabilities

At a judgment point, the agent asks the decision model what comes next.

**System 1 — the decision model.** One request carries the rendered workflow, the remaining budgets, the history of the run as outcomes, and — for the last action — its full outcome and the tail of its transcript. The request asks three questions:

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

**System 2 — the chat model.** The standard tool-calling shape: call the model with the tools, run the tool calls, append the results, repeat until `finish`, a pause or a budget limit. The system prompt is the workflow: the role, `Policy.Render()`, and how to read an outcome — the status is the primary evidence, the transcript is context.

Anything that needs text — an instruction, an answer to a prompt — always goes to System 2.

### 8.7 The three modes

`me` is enabled when at least one of `me.llm` and `me.decider` is configured.

| `me.llm` | `me.decider` | Behaviour |
|---|---|---|
| set | unset | System 2 only: the tool loop decides every judgment point. |
| unset | set | System 1 only: confident decisions run, everything else asks the user. |
| set | set | System 1 first, with System 2 handling the escalations. |

Without a chat model, an escalation becomes a question for the user: the agent lists the options and `/me reply <option>` picks one. An action that needs text goes to `ask_human` in that mode.

The fast path needs no model at all: a workflow whose every step ends `ok` runs with zero calls to either capability.

### 8.8 The configuration

```yaml
me:
  llm:
    provider: openai              # a registered chat provider: openai | anthropic
    base_url: https://api.openai.com/v1
    model: gpt-4.1-mini
    api_key: sk-xxxx              # the key itself, in the file (optional)
    api_key_env: OPENAI_API_KEY   # the name of a variable holding the key (optional)
    max_tokens: 4096
  decider:
    protocol: decisions           # decisions | systemone
    base_url: https://openrouter.ai/api/v1
    model: typesafe/jev-1.14
    api_key: sk-yyyy
    api_key_env: OPENROUTER_API_KEY
    min_confidence: 0.8           # below this the agent escalates
```

The decider's base is the root of the API; the version segment is added
when the base does not carry it. The two protocols live at
`{base}/v1/decisions` and `{base}/v1/systemone` — `https://api.openai.com`
and `https://api.typesafe.ai` are the roots of the two APIs.

The credential comes from one of two channels: `api_key` holds the key itself in
the configuration file, `api_key_env` names an environment variable that holds it.
Either one is enough — `api_key` wins when both are set, and a configuration with
neither sends no authentication header. `config.yaml` is written with mode 0600 and
should be kept out of git.

The user picks the models. nightme never chooses one, and it never injects a model into the coding agents — those keep their own configuration.

`me.llm` and `me.decider` are independent. A model that is configured but cannot be built — an unknown provider, a missing key — is reported in the daemon log and left out; the daemon still starts, and `/me` says why it cannot run.

### 8.9 The `/me` commands

| Command | Behaviour |
|---|---|
| `/me start <workflow> [args...]` | Starts a workflow in this chat. The run inherits the chat's workspace and agent. A chat without a workspace is refused. A second run in the same chat is refused. |
| `/me reply <text>` | Answers the question the run is waiting for. |
| `/me stop` | Cancels the run and stops the work chat. |
| `/me status` | Reports the current step, the phase and progress of the running command, the remaining budget, and what each capability decided and spent. |

The arguments of `/me start` fill the workflow's declared arguments: a positional token fills the next argument that is still unassigned, in declaration order, and a token of the form `name=value` assigns that argument by name. An argument the workflow does not declare is refused, and an argument with neither a value nor a default is refused.

One run is active per chat: the chat that starts a run is the chat that receives the progress and answers the questions.

### 8.10 The outcome model

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

The outcome of a step reaches the run state and the expressions of the steps after it: the status as `${{ steps.<id>.outputs.status }}`, the facts under `${{ steps.<id>.outputs.data.<key> }}`.

The progress the observer receives is built from the outcome:

```
▶ /gtw fix 42 -y
✅ /gtw fix 42 -y
▶ /review
⚠️ /review incomplete · 1/3 groups unfinished
✅ issue 42 is on its way · 6 actions · system-1: 0 decisions, 0 tokens · system-2: 8 decisions, 4210 tokens
```

### 8.11 Guardrails and recovery

| Guardrail | Bound |
|---|---|
| Loop iterations | Three times the workflow's action budget, unless the configuration says otherwise. |
| Token budget | The tokens one run may spend on the chat model. |
| Step timeout | Thirty minutes per command. |
| Stall detection | Ten minutes without a heartbeat. |
| Action budget | `max_actions` for the run, `max` for a step. |
| Compaction | Every tool result but the last keeps its status and summary; the last one keeps its transcript. |

A text-only answer — words without a tool call — is nudged once to call a tool. The second time, the run ends as a failure.

The run is persisted after every tool result: one file in `<DataDir>/workflows/runs/<run-id>.json` holds the DAG progress, the conversation, the judgment history and the paused state. On startup, running runs resume:

- a command that was in flight is resolved through the loopback; if its execution is gone, it reports `interrupted`;
- a suspended run keeps waiting for `/me reply`.

Recovery is silent. The observer is told only when a run cannot go on — an authentication error from the model, for instance.

## 9. Out of scope

These are deliberately **not** in the workflow engine; they belong in a later iteration.

| Not in scope | Why |
|---|---|
| A timer scheduler | The `on:` block is parsed and displayed; a scheduler that starts a workflow belongs in its own change. |
| External action marketplace (`uses: owner/repo@v1`) | Reuse is via shell scripts; the project's stack stays single-binary. |
| Cron expressions with seconds / `@reboot` | Standard 5-field cron is enough; expansion is a follow-up. |
| Per-step `timeout-minutes` | Inherits the host process timeout for now. |
| Webhook signature verification (beyond what `git provider` already does) | Provider abstraction (see F-50) handles this. |
| Cross-workflow triggers (`workflow_run`) | Compose via mention + schedule. |
| Parallel execution | One run drives one work chat, so the branches of a graph express ordering, not concurrency. |
| Real `notify` / `email` / `github_*` actions | They report `"stub": true`; making them real is their own change. |
| A human-gate node kind | Human input stays the agent's `ask_human` tool at a judgment point. |

## 10. Cross-references

| Topic | Where |
|---|---|
| Trigger sources | [`feat/F-workflow-engine.md`](./feat/F-workflow-engine.md) |
| Mention parsing (git provider) | [`feat/F-workflow-engine.md`](./feat/F-workflow-engine.md) |
| Agent names | [`primary-agent-detection.md`](./primary-agent-detection.md) |
| Loopback channel | [`../internal/channel/loopback`](../../internal/channel/loopback) |
| The `wfe` engine | [`../../internal/wfe`](../../internal/wfe) |
| The outcome contract | [`me.md`](./me.md) |
