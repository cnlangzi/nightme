# nightme Workflow — Configuration Reference

> **Status**: The workflow schema and the personal agent that runs it.
> **Scope**: The YAML schema, the plan the engine computes, the judgment points, and the runtime that executes them.

A **workflow** is a YAML file that nightme runs. It answers two questions:

1. **What** does it do? (`jobs.<name>.steps` — ordered steps)
2. **How far** may it go? (`iterations`, and the agent's budget)

The personal agent (`/me`) runs a workflow in a chat. The same file is the plan the agent follows and the bound it may not cross.

---

## Table of Contents

- [1. File location & layout](#1-file-location--layout)
  - [1.1 Installing a workflow](#11-installing-a-workflow)
  - [1.2 Uninstalling a workflow](#12-uninstalling-a-workflow)
  - [1.3 Checking a workflow](#13-checking-a-workflow)
- [2. Top-level fields](#2-top-level-fields)
  - [2.1 `requires` — the versions the workflow needs](#21-requires--the-versions-the-workflow-needs)
- [3. Triggers (`on:`)](#3-triggers-on)
  - [3.1 `schedule`](#31-schedule)
  - [3.2 `pull_request`](#32-pull_request)
  - [3.3 `branch`](#33-branch)
  - [3.4 `issue`](#34-issue)
  - [3.5 `mention`](#35-mention)
- [4. Jobs](#4-jobs)
- [5. Steps](#5-steps)
  - [5.1 `message` — talk to the chat the run works in](#51-message--talk-to-the-chat-the-run-works-in)
  - [5.2 `shell` — execute shell](#52-shell--execute-shell)
  - [5.3 `wait` — sit out a duration](#53-wait--sit-out-a-duration)
  - [5.4 `use` — invoke a registered action](#54-use--invoke-a-registered-action)
  - [5.5 `cmd` — run a builtin command](#55-cmd--run-a-builtin-command)
  - [5.6 Common step fields](#56-common-step-fields)
  - [5.7 `agents` — the chain of agents the step runs](#57-agents--the-chain-of-agents-the-step-runs)
- [6. Expressions](#6-expressions)
  - [6.1 The forms of an expression](#61-the-forms-of-an-expression)
  - [6.2 The paths](#62-the-paths)
  - [6.3 The operators](#63-the-operators)
  - [6.4 The truth of a value](#64-the-truth-of-a-value)
  - [6.5 The functions](#65-the-functions)
  - [6.6 What is refused](#66-what-is-refused)
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
~/.nightme/workflows/<name>/workflow.yml
~/.nightme/workflows/<owner>/<name>/workflow.yml
```

The directory is `<data_dir>/workflows` — `~/.nightme/workflows` with the default `data_dir`. The daemon and the `nightme workflow` CLI read the same directory.

- A workflow is a bundle: a directory under the workflows directory that holds a `workflow.yml`. A workflow's name is the path of its bundle under the workflows directory — `fix-issue/workflow.yml` is the workflow `fix-issue`, `nightme/coder/workflow.yml` is the workflow `nightme/coder`. Two files that claim the same name make that name unusable.
- Nothing else is a workflow: a `*.yml` file that lies directly in the workflows directory, and a file that sits beside a bundle's `workflow.yml`, are not workflows.
- The directory is read at any depth, except the reserved directories `actions` and `runs`.
- The workflows are read when they are asked for: the daemon reads the directory at startup and again before every `/me start`. An edited file takes effect on the next `/me start`, without a restart. A file whose size and modification time did not change is not parsed again. The fingerprint is best-effort: a change that keeps the size and lands in the same modification-time tick is noticed when the daemon restarts.
- A file that does not load stops only itself: the workflows beside it load and run as usual, and the failure is reported against the name the file would have provided.
- The action scripts live in `~/.nightme/workflows/actions/`. A run keeps no state of its own on disk.

### 1.1 Installing a workflow

`/wf install [-f] <owner>/<name>` fetches a published workflow and installs it:

```
/wf install nightme/coder
```

The workflow is written to `~/.nightme/workflows/nightme/coder/workflow.yml`, so its name keeps the address it was published with and `/me start nightme/coder` runs it. The install is shown as a checklist of three rows — the download, the validation, the write — and the rows update as the install goes. The path the rows report is home-relative and uses forward slashes, so it reads the same on every operating system.

An address that carries no owner is the project's own namespace: `coder` is the address `nightme/coder`. The short name is another way of writing the address — the same file is fetched and written under the same path.

An address that is already installed is skipped: the hub is not asked and the local file is left alone. `-f` (`--force`) installs it again — the workflow is fetched once more and written over the local file, unless its content did not change, in which case the file is not rewritten.

The file is fetched from the hub through three tiers, in order: GitHub, jsDelivr, jsdmirror. Every tier is probed before its file is fetched, so a tier that cannot be reached is skipped instead of costing a transfer timeout. The downloaded content is checked before it is written: a file that does not validate is not installed, and the workflow that is already there stays as it is.

The address is `<owner>/<name>`: the owner is the namespace the workflow is published under — lowercase letters, digits, dashes and underscores, starting with a letter or a digit — and the name is its lowercase kebab-case short name. `actions` and `runs` are reserved by the workflows directory, so they are not owners. An address that carries no owner is the project's own namespace, and the owner is filled in before the shape is checked, so the name it carries has to fit the same rules: `Coder` is refused.

The install's outcome says in one line what it came to: installed, skipped, updated, unchanged, not found, an invalid address, an unreachable tier, a file that does not validate, or a file that could not be written. A failure carries the status that says which one it was.

### 1.2 Uninstalling a workflow

`/wf uninstall <owner>/<name>` takes an installed workflow away again:

```
/wf uninstall nightme/coder
```

The bundle `~/.nightme/workflows/nightme/coder/` is removed, with every file under it. The owner directory `~/.nightme/workflows/nightme/` goes with it when nothing else is left in it; a directory that still holds another workflow — or any other file — stays as it is. The removal is shown as a checklist of one row, and the path it reports is home-relative and uses forward slashes. The short name works here too: `/wf uninstall coder` takes away the same workflow.

The uninstall's outcome says in one line what it came to: uninstalled, not installed, or the file could not be removed.

### 1.3 Checking a workflow

`nightme workflow validate` checks the workflow files and reports every problem it found:

```
nightme workflow validate [path...]
        --json    print the diagnostics as a JSON array
```

With no argument it validates the workflows dir; with arguments it validates the files and the directories it is given, and a directory contributes the `workflow.yml` of every bundle under it. An argument that is not a path on disk is looked up as a workflow name, so `nightme workflow validate nightme/coder` validates the file of that name. A name that carries no owner is the project's own namespace: `coder` is `nightme/coder`. Every problem is one line — `path:line:column: message` — and a run where every workflow is valid prints nothing. The exit code is `0` when every workflow is valid, `8` when a workflow does not validate, and `9` when a path does not exist or cannot be read — the workflows dir among them.

The engine offers the same check as an API (`pkg/wfe`):

| Function | Returns |
|---|---|
| `Validate(raw []byte) []Diagnostic` | Every problem in the given YAML bytes. |
| `ValidateFile(path string) []Diagnostic` | Every problem in the file at that path. |
| `ValidateDir(dir string) []Diagnostic` | Every problem in every workflow file of that directory. |

A `Diagnostic` carries the file, the line, the column and the message. The load path — `Parse`, `LoadFile`, `LoadDir` — reports the problems of the schema as a `*wfe.ValidationError`, which matches `wfe.ErrValidationFailed` under `errors.Is`.

The two surfaces differ in one thing: the load path ignores the keys the schema does not have, so a workflow file that carries extra fields keeps running; the validation surface reports them, because a typo changes what the workflow does without saying so.

---

## 2. Top-level fields

| Field | Type | Required | Default | Purpose |
|---|---|---|---|---|
| `title` | string | ✅ | — | What the run is for. It is what the operator reads and what the agent is given. |
| `args` | arg[] | ❌ | — | The arguments the workflow takes. A spec is a mapping with `name` and an optional `default`. |
| `instruct` | bool | ❌ | `true` | Whether the agent may send free-form instructions to the chat the run works in. |
| `workspaces` | string[] | ❌ | — | The workspaces the workflow names. The run works where the chat works. |
| `requires` | object | ❌ | — | The versions the workflow needs (see §2.1). |
| `on` | object | ❌ | — | Trigger spec (see §3). Parsed and displayed; nothing fires it. |
| `jobs` | map | ✅ | — | Named jobs, each with ordered `steps` (see §4). |

The workflow's name is **not** a field: it is the path of the bundle under the
workflows directory — the directory that holds the workflow file. `title:` says
what the workflow is for.

A workflow names **no agent**. The run uses the agent of the chat that started it — the chat's `/use` choice, or its primary agent.

A key the schema does not have is ignored: the file loads, and the key has no effect. `nightme workflow validate` reports it (see §1.1).

Minimal skeleton (`~/.nightme/workflows/my-workflow/workflow.yml`):

```yaml
title: commit the work in this chat
jobs:
  main:
    steps:
      - message: /gtw commit
```

**Full example** (every top-level field + every step kind + every trigger kind):

```yaml
title: Fix an issue and ship it as a PR.

requires:
  nightme: ">=0.10.0"               # which nightme this file needs

args:
  - name: issue
    default: "*"                          # a default makes the argument optional

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

      # 2. message step: talk to the chat the run works in
      - id: fix
        if: ${{ success() }}
        message: "/gtw fix ${{ args.issue }} -y"
        env:
          REVIEW_DEPTH: deep

      # 3. message step that repairs the step before it
      - id: review
        message: "/review"
        description: 没跑完就 /review -c；有改动就先 /gtw commit

      # 4. wait step: sit out a duration
      - id: rest
        wait: 5m

      # 5. use step: invoke a registered action
      - id: notify
        use: notify
        with:
          channel: feishu
          target: oc_xxx
          message: "issue ${{ steps.fix.outputs.data.issue }} done"

      # 6. use step: user-defined action script
      - id: cleanup
        use: cleanup-tmp-files
        with:
          paths: [/tmp/pr]
```

**Why `workspaces` is optional**: a workflow runs where the chat works, and follows the chat when it moves. The list names the workspaces the workflow is meant for; it is not a filter that decides where the run happens.

### 2.1 `requires` — the versions the workflow needs

A workflow can say which nightme it needs:

```yaml
requires:
  nightme: ">=0.10.0"          # which nightme this file needs
```

The key names the object the requirement is about; the only object is
`nightme`. The value is a version constraint:

```
constraint := comparator ("," comparator)*      # a comma means "and"
comparator := (">=" | ">" | "<=" | "<" | "=" | "!=") version
version    := major ["." minor ["." patch]]
```

An operator is required: a bare `0.10.0` would be an exact match, and a
workflow that pins an exact version stops working on the next patch release.
`^` and `~` are not supported — their meaning differs under `0.x` and `1.x`,
and a workflow is not a package.

Only the numeric trunk of a version is compared: `v0.9.1-12-gabc` is `0.9.1`,
and `0.10` is `0.10.0`.

A constraint that does not parse is a problem of the file: the workflow does
not load, and `nightme workflow validate` reports it.

The requirement is checked when the run starts. A build that is too old
refuses the run and says both versions: `this workflow needs nightme >= 0.10.0;
this nightme is 0.9.1`. A build that carries no release version — a
development build (`dev`, `0.9.1-dev`) or a CI build (`0.0.0`) — does not
check the requirement at all: it is the newest code there is.

`nightme workflow list`, `nightme workflow show` and `nightme workflow
validate` show the requirement and how this build stands against it.
`/wf install` says what the installed workflow needs; it does not refuse it.

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
| `iterations` | int | ❌ | How many rounds the job runs. Missing means one round, `0` means the job's steps run until the run ends, `N` means `N` rounds in total. `/me start -i <rounds>` overrides it for that run (see §8.9). |
| `sleep` | duration | ❌ | The gap between two rounds of the job. Missing means no gap: the next round starts at once. The value is a Go duration (`30s`, `5m`), the way the `wait` of a step is one; a value that does not parse — or one that runs backwards — is refused when the file is loaded. |
| `steps` | step[] | ✅ | Ordered steps to execute. |

A **round** is one pass through a job's step list. When a round ends, the job starts the next one: the round's step outputs are cleared — so `${{ steps.fix.outputs.data.issue }}` reads the next round's fix — and the per-step budgets start over. The job's `if:` conditions are read from the state of the round that is running.

A job that names a `sleep` waits that long between two of its rounds. The gap is waited after a round ends — whether it ended on its own or the agent ended it early — and before the next round starts. The first round of a job waits for nothing: it starts as soon as the jobs it `needs` have finished. A job whose last round ends waits for nothing either: there is no next round to wait for.

The gap is not a step: it shows up nowhere in the chat and nowhere on the card of the round. `/me stop` and the shutdown of the daemon end the wait at once.

A round in which no step ran does not repeat: a job whose every step is skipped had nothing to do, so it does not start another round. The plan is exhausted and the agent decides whether the run is over.

A round can also end **early**: the agent ends it when there is genuinely nothing left to do in it. The steps of the round that did not run are recorded as skipped, and the job starts its next round.

A job waits for the jobs it `needs` — a repeating needed job releases the waiting job after its first round, because a job that repeats until the run ends never finishes.

A workflow runs one job graph in one chat: the branches of the graph express ordering and branching, never concurrent work on separate workspaces.

---

## 5. Steps

Each step carries exactly one of five kinds. A step with two kinds, or with none, is refused when the file is loaded.

| Kind | Field | Use case | Where it runs |
|---|---|---|---|
| `message` | `message: <text>` | Talk to the chat the run works in | that chat |
| `shell` | `shell: <cmd>` | Execute shell | the chat's current directory |
| `use` | `use: <action>` + `with: <args>` | Invoke a registered action | the action registry |
| `wait` | `wait: <duration>` | Sit out a duration | the run |
| `cmd` | `cmd: {name: <builtin>}` | Run a builtin command of the engine | the engine |

`message` and `shell` are **default-supported** — they work in every workflow. `use` is the **extension point** — the agent registers which action names are valid.

### 5.1 `message` — talk to the chat the run works in

The text goes into the chat the run works in, marked with a bot sign. A `/`-prefixed text is a slash command the daemon interprets; anything else is a turn for the coding agent. Both settle with an outcome.

```yaml
# A slash command
- id: commit
  message: "/gtw commit"

# A free-form turn
- id: explain
  message: |
    Summarise the changes of the last commit in one line.
```

The decision model picks among the steps' `message:` values. A registered nightme command the agent issues is a repair: the run accepts it, records it against the step the run is on, and it never becomes that step's verdict. A command that is neither a step's message nor a registered command comes back to the agent as an error it can correct.

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

The command runs in the chat's current directory. The directory moves when a command moves it — `/gtw fix` moves the chat into the worktree it created — so a step that follows `/gtw fix` runs inside that worktree. The step's `env:` is layered on top of the daemon's own environment, so the shell finds `PATH`, `HOME` and the git/ssh configuration a real command needs.

The step's outputs carry what the command produced: `data.exit_code`, `data.stdout`, `data.stderr` and one `data.<key>` per `key=value` line of the output.

`shell` is the right tool when you need arbitrary side effects. Reuse is by writing shell scripts (e.g. under `~/.nightme/workflows/actions/`), not by importing external action packages.

### 5.3 `wait` — sit out a duration

```yaml
- id: rest
  wait: 5m
```

The step waits five minutes and then reports a success. The duration is a Go duration (`30s`, `5m`, `1h30m`); a value that does not parse is refused when the file is loaded. A wait carries no command.

A wait is a step of a round: it waits inside the round, and it runs only when the round reaches it — a wait that runs only when the queue was empty is a condition on the outcome of the step before it. The gap between two rounds of a job is the job's own `sleep` (see §4): the engine keeps it however the round ended, so the back-off of a repeating job survives the round the agent ended early.

`/me stop` ends a wait at once: the wait belongs to the run. The same goes for the gap between two rounds.

### 5.4 `use` — invoke a registered action

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

### 5.5 `cmd` — run a builtin command

`cmd` runs a command of the engine itself. There is one of them today:

```yaml
- id: quiet
  cmd:
    name: skip
```

| Field | Type | Required | Purpose |
|---|---|---|---|
| `name` | string | ❌ | The builtin command the step runs. |
| `args` | string[] | ❌ | The arguments the command takes. |

A `cmd` step may carry a `message:` — the message is what the step does in the chat, not a second kind:

```yaml
- id: quiet
  cmd:
    name: skip
  message: "/gtw back"
```

The message goes to the chat first and its outcome is the step's outcome. The command runs only when the message worked: a message that failed is the step's failure, and the agent decides what comes next.

**`skip` ends the round quietly.** The steps of the round that did not run are recorded as skipped, the job starts its next round, and the round leaves nothing behind in the report of the run: no row in the tree of the rounds, no error, no decision and no count. Its card carries the verdict and nothing else. The tokens it spent are counted — they were spent.

A `cmd` step is a step of the plan like any other: it has its place in the list of the job, it has an `if:`, and the failure of the step before it keeps it from running. It names no agents: the message it sends is the chat's own.

The name of a command is fixed: it is not an expression, and a file that holds one is refused when it is loaded.

A `cmd` step that names no command and carries no message does nothing, and it is refused when the file is loaded. So is a `cmd` step that names a command the engine does not have, or that gives a command an argument it does not take.

### 5.6 Common step fields

Every step supports:

| Field | Type | Default | Purpose |
|---|---|---|---|
| `id` | string | `<job>#<position>` | The step's name. Only a step that declares one can be named in an expression. |
| `if` | string | (always true) | Condition expression. A step whose condition does not hold is skipped and the chain continues. |
| `env` | map | — | Environment variables for the step (merged on top of the run's env and the daemon's env). |
| `continue-on-error` | bool | `false` | Whether the chain continues when this step fails. |
| `description` | string | — | What the step is, which outcome is its normal branch, and what to do when it is not. The decision model and the chat model both read it. |
| `agents` | chain[] | — | The agents the step runs, in order (see §5.7). |

### 5.7 `agents` — the chain of agents the step runs

A step may name the agents that do its work. The chain is a list of entries, and each entry has a `mode` and the `names` it applies to:

```yaml
- id: review
  message: "/review"
  agents:
    - mode: use
      names: [omp, claude]
```

The step runs the agents in the order they are declared, and the names of an entry in the order they are declared. The first agent that does not break is the step's outcome; an attempt that breaks hands over to the next one. An attempt the chain absorbs leaves no trace in the run: its error is not the step's error — the step's error is the error of the attempt the chain ends on. When every attempt broke, the step fails and the decision model is asked what happens next — the same path a single failed step takes.

| Field | Type | Required | Purpose |
|---|---|---|---|
| `mode` | string | ✅ | How the agent is applied: `args` or `use`. |
| `names` | string[] | ✅ | The agents to try, in order. Every name is non-empty. |

The names are strings of the workflow: they may hold expressions, and `${{ args.NAME }}` is expanded when the run binds its arguments.

**`args` — the agent goes on the command line.** The step's message is sent with `-a <agent>` appended, so the command itself names the agent:

```yaml
- id: review
  message: "/review"
  agents:
    - mode: args
      names: [omp, claude]
```

The message has to be a slash command: the agent goes on the command line, and only a command takes one. A message that holds an expression is accepted when the file is loaded and checked again when the step runs, because the command dispatcher sees what the expression became.

**`use` — the chat's agent is switched for the attempt.** The message goes to the chat unchanged, with the chat's selected agent switched to the attempt's agent for the length of the attempt. The chat's own agent comes back when the attempt returns — unless the operator switched the chat's agent themselves in the meantime, which wins.

```yaml
- id: fix
  message: "/gtw fix"
  agents:
    - mode: use
      names: [omp, claude]
```

The switch is process-local: the chat store keeps the agent the operator picked with `/use`, so the run never changes what the operator's own choice is. The cost of the mode is that the operator's own messages go to the switched agent while the attempt runs.

`use` is the mode for the commands that take no agent of their own (`/gtw fix`, `/gtw new`, `/gtw plan`): they dispatch to the chat's selected agent. `args` is the mode for the commands that take one (`/review`, `/gtw commit`, `/gtw pr`).

**The handover — what a broken attempt leaves behind.** A broken attempt hands the step over to the next one, and what the handover carries depends on what the broken attempt reported: an attempt that said how much of its work it did not finish — the `unfinished` count in its outcome — hands that work over, and the next agent finishes it instead of running the whole thing again. The `args` command of the next attempt is then the step's message plus `--continue` (`/review --continue -a claude`). An attempt that reported no count leaves nothing behind, so the next agent runs the whole thing again (`/review -a claude`).

A command joins the handover by reporting the count, and it must accept `--continue`: that is the flag the handover adds. A step whose message carries a flag the command refuses together with `--continue` — `/review --global`, whose `--global` and `--continue` are exclusive — breaks the handover: the attempt fails on the flag check, and the next agent runs the whole thing again.

The `use` mode has no handover: the message goes to the chat as it is, and there is no place to put the handover in it.

A workflow that names no `agents` sends its message once, unchanged.

One execution of the step is one run of it: when it concludes, it is recorded in the run's action history once, and it costs one chance of the step's budget however many agents its chain tried. The record carries the command of the attempt the chain ended on — the `args` attempt's own command (`/review -a omp`, or `/review --continue -a omp` when it took the work of the attempt before it over), or the step's message with the agent noted (`/review [agent: omp]`) in the `use` mode, because the command itself does not name the agent — and the outcome of the chain. An attempt the chain absorbed is not recorded: the operator reads the result of the chain, not the failures it swallowed.

---

## 6. Expressions

An expression reads the run: what the earlier steps produced, what the workflow was asked for, what the environment holds. A condition is an expression, and an expression may compare what it reads.

### 6.1 The forms of an expression

Any string value can embed an expression in `${{ ... }}`. The expression is evaluated when the step runs, except the `${{ args.* }}` ones, which are resolved when the run starts.

A condition may also be written bare. The two forms below are the same condition:

```yaml
- id: commit
  message: "/gtw commit"
  if: ${{ steps.fix.outputs.data.issue }}
- id: pr
  message: "/gtw pr"
  if: steps.fix.outputs.data.issue
```

A condition is one expression: two `${{ ... }}` in one value, or text around one, are refused when the file is loaded.

### 6.2 The paths

| Path | Reads |
|---|---|
| `steps.<id>.outputs.success` | Whether the step `<id>` did its job — `true` or `false`. |
| `steps.<id>.outputs.status` | The HTTP status of the step `<id>` — `200`, `409`, `429`, … |
| `steps.<id>.outputs.statusText` | The one line that says what happened to the step `<id>`. |
| `steps.<id>.outputs.data.<key>` | One of the facts the step `<id>` reported. |
| `needs.<job>.*` | The outputs of the last step of the job `<job>`. |
| `args.<name>` | The value of the argument `<name>`. |
| `env.<name>` | The value of the environment variable `<name>`. |
| `event.<key>` | One of the facts of the event that started the run. No trigger fires yet, so the event is empty. |
| `now` | The current time, in RFC 3339. |

The `outputs` segment is optional: the rest of the path is joined with `.` into one key. `${{ steps.fix.outputs.data.issue }}` and `${{ steps.fix.data.issue }}` read the same fact, and a key that is not an output falls back to the data namespace, so `${{ steps.fix.outputs.issue }}` finds the `issue` fact of the outcome.

A path that names nothing is null: the step that did not run yet, the fact it did not report, the argument the workflow does not declare.

Every fact is a fact: the plan reads the status to tell a normal result from a breakage (see §8.2), and the facts say which case it was.

### 6.3 The operators

From the loosest to the tightest:

| Operator | Means |
|---|---|
| `\|\|` | Either side is true. |
| `&&` | Both sides are true. |
| `==`, `=`, `!=` | The same, the same, not the same. |
| `<`, `<=`, `>`, `>=` | Ordered. |
| `!` | Not. |

`!` binds tighter than a comparison, so `!a == b` reads as `(!a) == b`. A comparison needs parentheses around it: `!(a == b)`.

The values compare themselves. Two strings are the same when they read the same, a string that reads as a number is compared as a number, and a comparison that cannot be made is false. One side being a bool makes the comparison a question of truth: `"true" == true`, `"false" == false` and `null == false` hold. A null is not a number and not a string: `null == 0` and `null == ''` are false, and a comparison a null takes part in is false — a path that names nothing says nothing about the order of two values.

| Comparison | Result |
|---|---|
| `"200" == 200` | true — the string reads as the number. |
| `"true" == true` | true — one side is a bool, so both are read as truth. |
| `null == 0` | false — a null is nothing else. |
| `"1000" > "400"` | true — both read as numbers. |
| `"zebra" > "m"` | true — neither reads as a number, so they are compared as text. |
| `"abc" > 3` | false — there is no way to compare them. |

### 6.4 The truth of a value

A condition holds when the value it comes to is true. A null is false, a bool is itself, a number is true when it is not zero, and a string is false when it is empty, `false` or `0`.

### 6.5 The functions

| Function | Answers |
|---|---|
| `success()` | Whether the last step that ran did its job. A run that ran no step yet has nothing that broke: the answer is true. |
| `empty(x)` | Whether `x` is nothing: null, or a string with nothing but spaces in it. |

`failure()`, `cancelled()` and `always()` are not functions of the language, and a condition that calls one is refused when the file is loaded. Write `!success()` and `steps.<id>.outputs.status == 499` instead; a step that must run whatever happens carries no condition at all.

### 6.6 What is refused

- An expression that names an argument the workflow does not declare.
- A condition that does not parse.
- A function the language does not have.

---

## 7. Complete examples

### 7.1 Fix an issue and ship it

```yaml
title: 把队列里的一个 issue 做完并推上去
args:
  - name: issue
    default: "*"
jobs:
  ship:
    iterations: 0                     # 0 = the job repeats until the run ends
    sleep: 5m                         # the gap between two rounds
    steps:
      - id: fix
        message: "/gtw fix ${{ args.issue }} -y"
        description: 从 nightme/assigned 队列里挑一个
      - id: commit
        message: "/gtw commit"
        if: ${{ steps.fix.outputs.data.issue }}
      - id: review
        message: "/review"
        description: 没跑完就 /review -c；有改动就先 /gtw commit
        if: ${{ steps.fix.outputs.data.issue }}
      - id: commit2
        message: "/gtw commit"
        if: ${{ steps.fix.outputs.data.issue }}
      - id: push
        message: "/gtw push"
        if: ${{ steps.fix.outputs.data.issue }}
      - id: close
        message: "/gtw close"
```

The guard on the steps after `fix` reads `data.issue`: a fix that picked an issue reports `issue` in its outcome, a fix that found nothing does not — so the guard means "an issue was picked" and the guarded steps of the round are skipped.

The `close` step carries no condition: it runs when the round reaches it, and it tidies up the worktree the round left behind.

The `sleep` is the gap between two rounds: the round that ends early is followed by five minutes of nothing, and the next round looks at the queue again.

### 7.2 Build, test and report

```yaml
title: build, test and report
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
title: look at the tree and fix what is broken
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
  │  the policy (pkg/wfe.Policy) bounds every       │
  │  action a run may take                         │
  └───────┬──────────────────────────┬─────────────┘
          │ "🧑‍💻: /gtw fix 42 -y"   │ the run's cards
          ▼                          ▼
  ┌──────────────────────┐   ┌──────────────────────┐
  │ internal/channel —   │   │ internal/gateway/   │
  │ the chat itself      │   │ observe — what the  │
  │                      │   │ daemon said         │
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
| Transport | `internal/channel` | The chat the run works in. `Impersonator` speaks for the run: what it says is a real message of the chat, and it comes back in through `Incoming()`. |
| The side channel | `internal/gateway/observe` | What the daemon said about the messages the run sent: the phase, the heartbeat, the prompts, the outcome. |
| The agent | `internal/me` | The run: what to do next, what the policy allows, and the sessions the run speaks in. |
| The models | `internal/llm` | Two capabilities: `Provider` (chat and tool calling) and `Decider` (typed decision questions). |
| The engine | `pkg/wfe` | The workflow schema, the plan of a run, the judgment points, and the `Policy` that bounds a run. |

The engine is a pure library: no I/O, no clock, no secrets. It parses, validates, computes the plan of a run and evaluates expressions — the runner owns the external capabilities and hands the engine their results.

The agent works in the chat the run started in. Every command it sends is a real message of that chat, posted on the platform with a bot mark in front of it, and it comes back into the daemon through `Incoming()` like any other message. The receipt, the reaction and the interactive prompt therefore anchor to that message the way they anchor to a person's message. The agent never reaches into the gateway or the chat sessions.

Two channels can be driven this way: Feishu and Telegram. On any other channel `/me start` is refused with the reason.

### 8.2 The plan: the ready set

The engine computes the plan of a run from the workflow and the progress so far.

The **ready set** is the set of unrun steps whose job's `needs` are all satisfied and whose `if:` holds. Within a job only the first such step counts — the steps of a job run in order.

| The workflow says | The plan is |
|---|---|
| one step in the ready set, and the previous outcome was a normal result | run that step |
| one step in the ready set, and the previous outcome raised an exception | a judgment point |
| more than one step in the ready set | a judgment point — the plan forks |
| no step in the ready set, and no job repeats | a judgment point — the plan is exhausted |
| no step in the ready set, and a job repeats | that job starts its next round |

The plan reads the **status** of the previous outcome to tell a normal result from a breakage. A `4xx` or a `5xx` is an **exception**: something broke, so the flow cannot go on by itself and the agent decides. A `2xx` is a **normal result**: the request was valid and the work got done or there was nothing to do, so the flow continues on its own. A `499` is an exception whatever it carries: somebody stopped the step.

A run that has not executed anything yet counts as a normal result, so the first step runs on the fast path.

A round ends when the job's steps are done. The next round starts with the job's first step, the round's step outputs are cleared and the per-step budgets start over. The ready set of the next round is computed from the state of that round. A job that names a `sleep` waits its gap before that first step runs.

A round can also end early, when the agent ends it: the steps of the round that did not run are recorded as skipped, and the round advances as it does after an ordinary round. The step whose exception the agent resolved is not a judgment point any more — the agent has already decided what happens next.

### 8.3 The judgment points

A judgment point is where the workflow cannot decide and the agent does. It is reached when:

- the previous outcome raised an exception — something broke, so the flow cannot go on by itself;
- the plan forks — the ready set holds more than one step, or more than one job is ready to start a round;
- the ready step is still running — a command waits for an answer;
- the plan is exhausted — the ready set is empty and no job repeats.

A job that repeats knows how many rounds it runs: when they are spent, the workflow is over and the run ends on its own — no model is asked what comes next. A job that runs one round has no rounds to spend: the plan is exhausted because its steps ran, and the agent still has a say about it.

The agent answers a judgment point with a **directive**:

| Directive | What it does |
|---|---|
| `resume()` | Runs the step the judgment is about again — the step that broke. |
| `next()` | Skips the step the judgment is about: the engine records it as skipped and continues with the workflow. It takes the id of a step: the steps of the round from the step the judgment is about up to the step it names are passed over. |
| `ntm_<command>` | Runs a nightme command, as the owner. One tool per registered command. |
| `agent(task)` | Asks the owner's thinking. It investigates and recommends; it changes nothing. |
| `instruct(text)` | Sends a free-form instruction to the chat. |
| `answer_choice(choice_id, option_id)` | Answers a prompt the work agent is waiting for. |
| `ask_human(question)` | Asks the user. The run waits for the answer. |
| `repo_status()` | Reads the branch and the tree facts of the chat. |
| `wait(duration)` | Waits before the model looks again — for work that is still running in the background. The duration is a Go duration (`30s`, `5m`); the wait lasts at most 10 minutes. |
| `end_round()` | Ends the round of the job the run is on: the steps of the round that did not run are skipped, and the job starts its next round. |
| `set_output(step_id, key, value)` | Writes into the output of a step that ran: one of its facts, or its verdict. |
| `finish(status, summary)` | Ends the run. `status` is `done` or `failed`. |

`resume` and `next` act on the step the judgment is about: the run knows it, and the engine decides what runs next. `resume` runs that step again — its budget for the round is checked first — and `next` records it as skipped: the facts the step produced are kept, the error goes, and the engine plans the next step. Both refuse a step that did its job already: there is nothing to repair, and nothing to skip. `next` refuses a step that is still running: the work is in flight.

`next` may name a step instead: the steps of the round from the step the judgment is about up to the step it names are passed over. The step it names has to be in the round the run is on and at or after the step the run is on — the plan of a run only goes forward. A step of another job, a step that has passed and a step that did its job already are refused, and nothing is skipped.

`set_output` writes into the output of a step that ran: a fact under its own name, or the verdict of the step. The status writes the verdict, and the success follows from it: a 2xx is a success, anything else is not. The status text replaces the line of words. A step that has no record has no output to write. The correction is work the agent causes: it costs a chance of the step the run is on, and the run's history says what it changed. The write can unblock the plan: a step that was passed over because its own condition did not hold runs again when the condition holds now.

The nightme commands are the ones the command registry holds, minus the ones that would take the run with them (`/me`, `/close`, `/new`, `/stop`, `/cwd`, `/use`, `/resume`, `/steer`, `/wiki`), the ones that only show something (`/help`, `/think`, `/tools`, `/watch`, `/format`) and the subcommands that shape the project (`/gtw config`, `/gtw plan`, `/gtw new`) or tear the run's own workspace and its command apart (`/gtw close`, `/gtw sync`, `/gtw abort`). `/gtw back` is among the commands the model may issue: it is the way out of a fix worktree the run is stuck in. It moves the chat back to the repo root and syncs main on the way, and the run follows the chat — a run that leaves its worktree works on the repo root from then on. A command that belongs to no step is a repair: it counts against the step the run is on, but it never becomes that step's verdict.

The engine owns the run state: after a directive is executed, the state holds the new outcome, the step that ran and the round it ran in. A decision the agent makes never writes state by itself.

### 8.4 The policy

The policy is the bound on what a run may do. It is a pure function of the workflow and the actions taken so far.

| Check | Reports |
|---|---|
| `CheckRun(command)` | Whether the command may run now and which step it counts against. |
| `CheckStep(index)` | Whether the step may take an action now — its budget for the current round. |
| `CheckInstruct()` | Whether a free-form instruction is allowed. |
| `CheckFinish(status)` | Whether the run may end. `done` needs the last executed step's latest outcome to be a success; `failed` is always allowed. |
| `Remaining()` | The chances left per step. |
| `Render()` | The workflow as a compact text block: the identity, the title, the steps, their descriptions and the limits. |
| `StepCommands()` | The step messages the policy accepts right now. |

A step's message belongs to that step, and a step that spent its budget leaves the list. The budget counts the actions of the current round only: the step's own execution, the agent's decision that the step is skipped, the nightme commands and the instructions issued for it, and the turns of the chat model that produced no tool call. It starts over at every round.

A judgment that names no step — the plan forks, a job starts a new round — is charged to the last step that ran in the round. A run that has done no work yet is charged to the step the plan may run now. Every turn the agent spends without acting costs a chance somewhere: a turn that costs nothing bounds nothing.

The budget is one number for every step and every round, set by the personal agent from `me.max_judgments` (50 by default). A workflow file does not carry it. When the budget of the step the run is on is spent, the run pauses and says so in the chat: the operator runs the step again with `/me resume`, and the step's budget starts over.

The registered nightme commands are not the workflow's own: they are accepted without belonging to a step, and they are recorded against the step the run is on. The step's own verdict is the one the plan reads, so a repair that succeeded does not turn a failed step into a finished one.

There is no cap on the number of model turns: a run ends when the plan is exhausted and the agent finishes it, when the agent calls `finish`, when the operator sends `/me stop`, or when the daemon shuts down. A turn of the chat model that produced no tool call and a chat model that failed end nothing: the run asks again, and the budget is what bounds it.

### 8.5 The tools

The chat model acts through the tools below. Every handler enforces the policy: a violation comes back as a tool error result, and the model corrects itself.

| Tool | Behaviour |
|---|---|
| `resume()` | Runs the step the judgment is about — the step that broke — again: its own limit for the current round is checked first, then the step runs as the workflow wrote it. It returns the outcome — status, summary, data — and the transcript of what the coding agent said. It refuses a step that did not break. |
| `next()` | Records the step the judgment is about as complete: the facts it produced are kept, the summary says the agent decided the step was done, and the engine plans the next step. It counts against the step's budget like any other action. It refuses a step that is still running and a step that did its job already. It takes the id of a step: the steps of the round from the step the judgment is about up to the step it names are passed over, and the steps it cannot move are refused. |
| `ntm_<command>` | Runs a nightme command as the owner. The tool takes the command's subcommand and its arguments; the command goes to the chat as a message of the run. |
| `agent(task)` | Asks the owner's thinking a question and returns its answer. Registered only when there is an agent to ask. |
| `instruct(text)` | Runs `Policy.CheckInstruct`, then sends a free-form prompt to the chat. It is tracked and returned like `resume`. |
| `answer_choice(choice_id, option_id)` | Answers a permission request or a question the work agent is waiting for. |
| `ask_human(question)` | Asks the question in the chat — as a reply to the run's overview — and waits. The answer resumes the run as the tool's result. |
| `repo_status()` | Reads the chat's repository: the branch, whether the tree is dirty, and how far the branch is ahead of or behind its upstream and its base branch. |
| `wait(duration)` | Blocks the agent's turn for the duration, at most 10 minutes, and hands the turn back to the model when it is over. The wait belongs to the run: stopping the run or shutting the daemon ends it at once. |
| `end_round()` | Ends the round of the job the run is on: the steps of the round that did not run are recorded as skipped, and the job starts its next round. A round whose step is still running cannot end. |
| `set_output(step_id, key, value)` | Writes into the output of a step that ran: a fact under its own name, or its verdict. The status writes the verdict, and the success follows from it — a 2xx is a success, anything else is not. The status text replaces the line of words. A step that has no record has no output to write. A step that was passed over because its own condition did not hold runs again when the condition holds now. The correction costs a chance of the step the run is on, and the run's history says what it changed. |
| `finish(status, summary)` | Runs `Policy.CheckFinish` and ends the run. The record of the session is posted when the run ends. `status` is `done` or `failed`. |

A free-form text that names a nightme command is refused: the command has a tool of its own, and a command in a free-form text would run as a turn of the chat instead of as a command of the run.

`resume` blocks while the step runs and makes no model calls. It comes back early when the work agent asks something — the model can then answer, instruct or ask — or when the command goes quiet for ten minutes. Both cases report `waiting` and the reason.

### 8.6 The two capabilities

At a judgment point, the agent asks the decision model what comes next.

**System 1 — the decision model.** One request carries the rendered workflow, the remaining budgets, the history of the run as outcomes, and — for the last action — its full outcome and the tail of its transcript. The request asks three questions:

| Question | Shape | Asked |
|---|---|---|
| `next` | choice — the steps' `message:` values, plus `done` and `escalate`; each option's description is the step's description | always |
| `needs_human` | yes/no — does the next step need the user's judgement or credentials | always |
| `step_succeeded` | yes/no — did the last step work | only when the last outcome reported no verdict |

The decision is acted on without the chat model when all of these hold:

- `next` is not `escalate`;
- the confidence of `next` is at least `min_confidence`;
- `needs_human` is below 0.5;
- the policy allows it — `Policy.CheckRun` for a command, `Policy.CheckFinish` for `done`.

The decision is recorded in the conversation as a synthetic assistant tool call, so the chat model reads a consistent history when it takes over: the decision names a step, so the call is recorded as `run_step(step_id)`. It is the decision model's own record — the chat model acts through `resume` and `next`. The model that answered is stored with the run.

**System 2 — the chat model.** The standard tool-calling shape: call the model with the tools, run the tool calls, append the results, repeat until `finish` or a pause.

The model is the owner's hands inside the workflow: what it runs executes exactly as if the owner had typed it. The engine is the master — it runs the steps and wakes the model only when a step raised an exception — and the model's job is to repair the gap and to give the control back. The system prompt is the role, the tools and the workflow — the title, the jobs, the steps, their descriptions and its limits. It does not carry the workflow's conditions, its environment or its templates: those are the engine's business, not the model's. It names the two situations that are still the model's when a step produced nothing: an outcome whose verdict nobody reported, and work that is still running in the background (wait, then look again).

The model is called with a situation report: why the run needs it, the round it is in, the state of the repository, the steps of that round and which one is ready, what every step of the round produced — its status, its summary, its error, its facts and the hint that explains its reason code — the step the judgment is about with its description, the commands the policy accepts now, the chances left for it, the turns the agent spent without acting, the directives it may take, and the transcripts of the last two actions, each cut to its last 4 KB.

The report is a user turn, and only the latest one is kept: the report is rendered again on every turn of the agent, and a conversation that holds one report per turn grows past what the chat model accepts.

Anything that needs text — an instruction, an answer to a prompt — always goes to System 2.

### 8.7 The three modes

`me` is enabled when at least one of `me.llm` and `me.decider` is configured.

| `me.llm` | `me.decider` | Behaviour |
|---|---|---|
| set | unset | System 2 only: the tool loop decides every judgment point. |
| unset | set | System 1 only: confident decisions run, everything else asks the user. |
| set | set | System 1 first, with System 2 handling the escalations. |

Without a chat model, an escalation becomes a question for the user: the agent lists the options and `/me reply <option>` picks one. An action that needs text goes to `ask_human` in that mode.

The fast path needs no model at all: a workflow whose every step ends in a success runs with zero calls to either capability.

### 8.8 The configuration

```yaml
me:
  llm:                              # the chat model (System 2)
    protocol: chat_completions      # chat_completions | messages
    base_url: https://api.openai.com/v1
    model: gpt-4.1-mini
    api_key: sk-xxxx                # the key itself, in the file
    max_tokens: 40960               # the cap on one answer (the default)
    timeout: 10m                    # the deadline of one call (the default)
    headers:                        # extra headers every request carries
      HTTP-Referer: https://nightme.dev
  decider:                          # the decision model (System 1)
    protocol: decisions             # decisions | systemone
    base_url: https://openrouter.ai/api/v1
    model: typesafe/jev-1.14
    api_key_env: OPENROUTER_API_KEY # the name of a variable that holds the key
    min_confidence: 0.8             # below this the agent escalates
  agent: claude                     # the agent that thinks for the owner
  max_judgments: 50                 # the chances the agent has at one step in one round
```

The protocol names the wire format, not the vendor — the vendor is the one the
base URL points at. Four protocols are known:

| Protocol | The endpoint it calls |
|---|---|
| `chat_completions` | `{base_url}/chat/completions` |
| `messages` | `{base_url}/messages` |
| `decisions` | `{base_url}/decisions` |
| `systemone` | `{base_url}/systemone` |

The base URL is used as it is written: the client appends the path of its own
protocol and nothing else — no version segment is added, no default endpoint
exists. Copy the address out of the vendor's documentation and it works:
`https://api.openai.com/v1` reaches `https://api.openai.com/v1/chat/completions`
and `https://api.anthropic.com/v1` reaches `https://api.anthropic.com/v1/messages`.

`timeout` is the deadline of one call — a call that runs longer is cut off. It
defaults to ten minutes for the chat model and five for the decision model.
`headers` are extra headers every request carries — the ones a gateway asks for.

The credential comes from one of two channels: `api_key` holds the key itself in
the configuration file, `api_key_env` names an environment variable that holds it.
Either channel works for either capability; set one of them, not both — `api_key`
wins when both are set, and a configuration with neither sends no authentication
header. `config.yaml` is written with mode 0600 and should be kept out of git.

The user picks the models. nightme never chooses one, and it never injects a model into the coding agents — those keep their own configuration.

`me.agent` names the agent that thinks for the owner: the run asks it about the step that failed and what to do about it, and it answers with the cause and a recommendation. It changes nothing — the run's own hands carry out every change. When it is not set, the agent of the chat that started the run is used; a run without an agent has nobody to ask.

`me.llm` and `me.decider` are independent. A model that is configured but cannot be built — an unknown protocol, a missing key — is reported in the daemon log and left out; the daemon still starts, and `/me` says why it cannot run.

`nightme me` configures `me.llm` from the terminal: it shows the current values, asks for the protocol, the endpoint, the model, the cap on an answer, the agent that thinks for the owner and the credential, and writes them into the configuration file without touching the rest of it. The wizard is the whole interface — there is no `nightme me get` and no `nightme me set`. The daemon builds its models at start-up, so a new chat model takes effect on the next `nightme restart`.

The cap on an answer defaults to 40960 tokens; a configuration that does not name one gets it.

### 8.9 The `/me` commands

| Command | Behaviour |
|---|---|
| `/me start <workflow> [args...] [-i <rounds>]` | Starts a workflow in this chat. The name is the path of the workflow's bundle under the workflows dir. The run inherits the chat's workspace and agent. A chat without a workspace is refused. A second run in the same chat is refused. `-i` sets how many rounds every job of the workflow runs in this run. The command posts no reply: the chat's own card for the `/me start` message carries the plan of the run and the acknowledgement. |
| `/me reply <text>` | Answers the question the run is waiting for. |
| `/me pause` | Stops the run after the step it is on. The step that is running is not interrupted: its outcome is recorded. The card of the `/me pause` message carries the answer of the command and the state of the run: the answer is its first entry, and the phase line says why the run stopped. |
| `/me resume [<step-id>] [note]` | Runs the step the run is on again: the step that broke. The step id names the step to run: it has to have run in the round and not have succeeded, and a step that has passed, that did its job or that has not run is refused with the reason. The step's budget for the current round starts over before the retry. The card of the `/me resume` message carries the answer of the command and the state of the run. The note is the operator's instruction; it joins the conversation the chat model reads. A step that is running, that succeeded, or that has not run yet is not run again: the answer names the step and says why. `/me resume` does not interrupt a step that is running. |
| `/me next [<step-id>] [note]` | Skips the steps of the round up to and including a step: without a step it skips the step the run is on, with one it skips every step of the round from the step the run is on up to the step the operator named. The plan goes on with the workflow, and the facts the steps produced are kept. The card of the `/me next` message carries the answer of the command and the state of the run. The note is the operator's instruction; it joins the conversation the chat model reads. A step whose command is still running is skipped: the command it sent to the chat is abandoned, and the run goes on with the next step of the job. The abandoned command is not cancelled — the chat owns it, and what it produces lands in the chat when it finishes. A step that succeeded, and a step whose work is not a chat command, are not skipped: the answer names the step and says why. |
| `/me stop` | Cancels the run and leaves the record of the session in the chat. The stop and the record share one card: the card of the `/me stop` message. The work the chat agent does on its own goes on to its end. |
| `/me status` | Shows the state of the run again: the card of the `/me status` message carries the phase of the run, the tree of the rounds and the checklist of the round. A run that has not entered a round yet shows its phase line alone. The command posts no reply of its own. |

The step the run is on is the step its last judgment was about. A run that has no judgment — the operator took the chat back, the agent was throttled — is on the step it ran last, when that step did not succeed; a run whose last step succeeded is on the first step of the plan's ready set. `/me resume` and `/me next` act on that step, and the decision rides on the run until its loop applies it.

The step id of `/me resume` and `/me next` names the step the command acts on. It is the first token of the text after the subcommand, and only when the workflow has a step with that id: a text that names no step is the note of the command, as it always was. The step has to be in the round the run is on and at or after the step the run is on — the plan of a run only goes forward. A step of another job, a step that has passed, a step that did its job already and a step whose work is in flight and is not a chat command are refused: the command says which of them it is, and nothing is skipped. `/me resume` runs the step it names again, and that step has to have run in the round and not have succeeded.

The two commands decide at command time what they do with that step, and they answer accordingly: the promise only when there is something to do, and the reason when there is not. A command that acts on nothing names the step and says what did not happen.

| The state of the run and of the step | `/me resume` | `/me next` |
|---|---|---|
| The run is waiting for an answer | refused: answer it with `/me reply` | refused: answer it with `/me reply` |
| The step's command is running | the step is not run again; the command says why | the step is skipped and the command it sent to the chat is abandoned |
| The step's work is running, and it is not a command | the step is not run again; the command says why | the step is not skipped; the command says why |
| The step succeeded | the step is not run again; the command says why | the step is not skipped; the command says why |
| The step failed or was cancelled | the step runs again | the step is skipped |
| The step has not run | the plan runs it | the step is skipped |
| The run is working | the command says why; no decision | the command says why; no decision |
| The step the command named is of another job | refused: the command says which step and why | refused: the command says which step and why |
| The step the command named has passed | refused: the command says which step and why | refused: the command says which step and why |
| The step the command named did its job | refused: the command says which step and why | refused: the command says which step and why |

Both commands hand the control back to the run: a run that waits for the operator goes on, whatever the command decided about the step. A command that decides records the decision on the run, and the run's loop applies it at its next turn — the same rule the run's own execution layer follows, so the promise the command makes and the action the run performs agree. A command that decides nothing records no decision: the plan goes on with the step it was going to run anyway.

`/me next` on a step whose command is still running is the one decision that does not wait for the run's next turn. The run waits for the command, so a decision that waited for the run's next turn would wait for the command to finish — and a command that never finishes would keep the operator stuck. The step is therefore skipped at once: it is recorded as skipped, the facts it produced are kept, and the run stops waiting for the command and goes on with the next step of the job. The command itself is not cancelled: the chat owns it, and what it produces lands in the chat when it finishes. The verdict of a command that comes back after the step was skipped does not put the step back into the plan.

A step whose work is not a chat command — a shell, a wait, an action — belongs to the run itself: the run cannot go on while it runs, so the command holds no decision and says why.

The run speaks in agent sessions, and the chat renders every session the way it renders a coding agent's turn: an identity line, the entries of the session, the tool calls as thread events and a status bar in the footer. A round also carries the steps of the round as a checklist. A run opens four kinds of session. Every one of them hangs under one message:

| Session | Its card hangs under | It carries |
|---|---|---|
| The run | The `/me start` message — the chat's own card, which the run patches in place | The plan of the run: the workflow it works through, what the workflow is for, the facts of the run and its steps with the command of each. The plan is published once and never changes; the status bar and the verdict of the run ride in the same card. |
| A job round | A card of its own: the round opens it and the chat answers with its id | The tree of the rounds of the run, the closing block of the round that ended, the steps of the round as a checklist, and the round's identity, token usage and repository facts in its footer. The checklist follows the steps of the round. |
| The state of the run | The message that asked for it — the `/me status`, `/me pause`, `/me resume` or `/me next` message, whose card the chat already holds — or a card of its own when the run stops by itself | The phase line of the run, the tree of the rounds and the checklist of the round. It is a snapshot: it is published once and never changed. |
| A decision | A card of its own: the line that says the agent takes the workflow over opens it, and the chat answers with its id | The line that opens the card, the reason the decision was taken, the conclusion of every judgment of the step, the thinking of the chat model, the tool calls of the decision as thread events under the card, one entry per reply, and the verdict of the last judgment in its footer. |
| The record of the finished run | The card of the run, or the card of the `/me stop` message when the operator stopped the run | The account of the session: how the run ended, how long it went, what it did, what went wrong and what the models decided. |

The card of a decision belongs to the step the decision is about: a step that is judged again goes on with the card it has. A judgment about another step ends the card of the step before — the conclusion of its last judgment rides in that card's footer — and opens a card of its own; a step the run comes back to opens a card of its own as well, so one card belongs to one stretch of judgments. The reason of a step is published once: a judgment that comes to the same reason again adds nothing to the card.

The card of a session is a card of the chat. A round and a decision open their card themselves: the first entry of the session goes out without an anchor, the chat answers with the id of the card it wrote, and the rest of the session lands on that card. The state of the run opens its card the same way: the phase line goes out without an anchor, the chat answers with the id of the card it wrote, and the rest of the state lands on that card. A session owns one card, and the chat never posts a second one for it. The run's own session is the exception: it posts no message and writes into the card the chat already holds for the message that started the run. The state of the run is the other exception when the operator asks for it: it writes into the card the chat already holds for the message of the command. A run that no message started — a synthetic inbound, a command without a message id — has no such card: its entries go to the chat's orphan handling. The card of a round that ended stays in the chat as the record of that round; the next round starts a new one. A run that comes back to a round it has been in before goes on with the card that round has.

The state of the run is a card of its own when the run reports it: the phase line opens that card, and the chat renders the rest of the state onto it. The card of the state is the latest card of the round: the run goes on with it, and the card it replaces keeps the content it had. A card the run opened for a stop the operator has already taken back is not the latest card of the round: the operator is answered on the card of their own command, and the run goes on with that one.

The state of the run is published whenever the operator asks for it with `/me status`, `/me pause`, `/me resume` or `/me next`, and whenever the run stops by itself — a person took the chat back, the step ran out of budget or the agent was throttled. The operator's command publishes it on the card of its own message, and the run opens a card of its own when it stops by itself. A run that stops while a card of its decision is open publishes no state of its own: that card is on the screen, the round keeps its own card, and the reason of the pause reaches the operator in the chat. The answer of the command is the first entry of the card, and the phase line follows it: `⏸ paused: <the reason the run stopped>`, `asking: <the question the run waits for>`, or `running · step: <the command the run waits for>`.

Every run that ends leaves a record of the session in the chat: the record says how the run ended, what it did, what went wrong and what the models decided. The record is posted once the run has unwound, so the step that was in flight counts as a step that did not run unless it produced an outcome. A run that ends by itself leaves a record too; a run the daemon abandons at its shutdown leaves none, because the run stops where it stands and publishes nothing. The record rides on the card of the run, unless the operator stopped the run: the record of a stopped run rides on the card of the `/me stop` message, so the stop and the record are one card. The two lines land on that card in the order they arrive: the record may come before the line that says the run was stopped.

No card of the run carries a button: the operator drives the run with `/me pause`, `/me resume`, `/me next` and `/me stop`. A question the run asks is the chat's own interactive choice card, and a click on an option is the answer — the answer never reaches the agent. A person who quotes the question instead answers it in their own words — the answer is claimed by the run before the agent sees it.

The question itself never reaches the agent either: it is a message of the chat, not a turn of the conversation. The agent sees the commands the run speaks, not the questions the run asks.

The arguments of `/me start` fill the workflow's declared arguments: a positional token fills the next argument that is still unassigned, in declaration order, and a token of the form `name=value` assigns that argument by name. An argument the workflow does not declare is refused, and an argument with neither a value nor a default is refused.

The command line is read by the shared lexer: a token that starts with `-` is a flag, and a flag the command does not know is refused. `-i <rounds>` (or `--iterations <rounds>`) sets the round count of every job of the workflow for that run — `0` means "until the run ends", the same as `iterations: 0` in the file. The count belongs to the run: the file keeps its own counts, and the next `/me start` uses them. Everything after `--` belongs to the workflow, so a value that starts with `-` reaches it.

One run is active per chat: the chat that starts a run is the chat the run works in, the chat that answers its questions, and the chat that holds its cards.

The run's lifecycle belongs to these commands alone. `/close`, `/stop` and `/new` end the chat's session, not the run: a run whose step was interrupted goes on to the next judgment point.

### 8.10 The outcome model

The run judges from facts, not from text. Every command the agent sends reports a structured outcome.

The status is an HTTP status, and its category says which branch the
step took.

| Status | Meaning |
|---|---|
| `2xx` | The step did what it was asked to do — or there was nothing to do, which counts as done. A step that was skipped is `2xx` too: it did not complete by the workflow's execution, and the plan moved past it. |
| `4xx` | The request or the world refused it: the command line was wrong, the issue is not there, the branch is in the way. |
| `5xx` | Something broke: the agent did not run, the worktree could not be read, the platform could not be reached. |
| `499` | Somebody stopped it. |
| `0` | Nobody reported a verdict. |

A `2xx` is a **normal result** and a `4xx` or a `5xx` is an **exception**: something broke, so the flow cannot go on by itself and the agent decides (see §8.2). The contract itself is in [`me.md`](./me.md).

A command that ran *because* something existed and found nothing to do is a `2xx` — nothing to commit, nothing to push, an empty queue. The **facts** say which case it was: no `issue` key, or `commits=0`.

A step that is still running has no verdict yet, and a step whose execution is gone has none either.

The outcome of a step reaches the run state and the expressions of the steps after it: the verdict as `${{ steps.<id>.outputs.success }}`, the status as `${{ steps.<id>.outputs.status }}`, the words as `${{ steps.<id>.outputs.statusText }}`, the facts under `${{ steps.<id>.outputs.data.<key> }}`.

The engine owns the outcome of a step: it is written when the step runs, and the plan reads it there. The agent may correct one: `set_output` writes a fact or the verdict of a step that ran. The plan reads the new value — a step whose verdict the agent fixed is not an exception any more, so the plan goes on instead of stopping at a judgment point, and the steps after it read the new facts. A step that was passed over because its own condition did not hold runs again when the condition holds now.

The chat sees the outcome on the card of the session it belongs to — one
message, patched in place. The card of the run is the plan of the run: it names
the workflow it works through and what the workflow is for, how big the run is,
the arguments it was given and the steps of every job. It is published once and
never changes. The footer of the card names
the run, the model and the verdict the run reached:

```
▶ [ fix-issue ] started
Fix issue #42 and ship it as a PR.

jobs:     1
steps:    5
started:  14:03:22
args:     id=42

main
├─ [ 1 ]: fix      /gtw fix 42 -y
├─ [ 2 ]: commit   /gtw commit
├─ [ 3 ]: review   /review
├─ [ 4 ]: push     /gtw push
└─ [ 5 ]: pr       /gtw pr

---
🤖: me · jev-1.14 · fix-issue-1752661200000000000 · fix-issue is complete
💰:「 4.2k / 1.1k / 4210 · 12.5% (200k) · $0.087 」
📁: code/nightme · ⎇ main · + 2 · ± 3 · − 1
```

The body of the card reads like the output of a command. The headline names
the workflow, the second line is what the workflow is for, and the facts of the
run follow: how many jobs it has, how many steps they hold, when the run
started and what it was given. The values of the facts start at the same
column: the key, its colon, and the padding to the column.

The plan follows the facts, one block per job. The header of a block says how
many rounds the job runs — `main` for one round, `main ⟳ 3 rounds` for three,
`main ⟳ until the run ends` for a job that repeats until the run ends. Every
step is a node of its own, numbered across the whole plan, with its id and the
command it runs. The ids of a job are padded, so the commands of a job all
start at the same column. A job with more than 24 steps lists its first 24 and
folds the rest into one node: `└─ [ 25 ]: … +6 more steps`.

The card of a round carries the tree of the rounds of the run: the rounds that
have finished, with their own facts, and the round that is going. The tree is
published once, when the round opens, and is never published again — a
published entry cannot be edited.

```
rounds: 4
├─ [ 1 ] v 10:33:04 · 5m25s decisions:  system-1 ×1 · operator ×1 ·  tokens - 32.4k / 8.1k
├─ [ 2 ] ! 10:38:29 · 5m14s decisions:  system-1 ×1 · operator ×1 ·  tokens - 32.4k / 8.1k
├─ [ 3 ] v 10:43:43 · 5m14s decisions:  system-1 ×1 · operator ×1 ·  tokens - 32.4k / 8.1k
└─ [ 4 ] > 10:48:57
```

The heading of the tree names the number of rounds the run is configured for:
the count of the job the round belongs to, whether the workflow declares it or
`/me start -i <rounds>` set it for that run. A job that repeats until the run
ends has no count, and the heading says so: `rounds: ⟳ until the run ends`.

The node of a finished round carries its number, the icon of its outcome, when
it started, how long it took, what decided in it and what the run spent up to
its end. The numbers are the numbers of the run over the rounds up to that
one: the tree reads as a ledger, and the last node carries the totals of the
run. The node of the round that is going carries its number, the icon `>` and
the time it started — the round has not settled, so it has no duration and no
verdict yet.

The icon of a row is the marker of the checklist without the brackets, so one
card carries one vocabulary: `v` for a round that went well, `!` for one that
went wrong, `x` for one that was stopped, `?` for one that reported no verdict
and `>` for the round that is going. Every icon is one character wide, and the
round number is as wide as the largest number the tree shows, so the icons of
the rows start at the same column.

The tree shows the last five rounds of the run: the earlier ones fold into
one node, `… +3`.

A round that produced nothing stays out of the tree: a round in which no
step produced work — every step was passed over, waited, or reported that
there was nothing to do — says nothing that the rounds around it do not
say. The card of such a round stays, with its checklist and its closing
block: the round happened, and what it did is on it. The folded rounds are
the rounds that produced work and are too old: the rounds that produced
nothing are not counted at all.

The rounds of a job are numbered from one, so a run with several jobs has rounds of the same number. The tree names the job when there is more than one — `[ 1 ] v ship · 10:33:04 · …` — and leaves it out when the run has a single job: the plan of the run says which job the rounds belong to.

When the round ends, its card carries the closing block: what the round came
to and the numbers of the run through that round.

```
ended: [ v ] the fix landed · 5m14s
decisions:  system-1 ×1 · operator ×1 ·  tokens - 32.4k / 8.1k
```

Every row of the checklist is a numbered markdown list item — `1. [v] fix (the
fix landed)` — and every marker is one ASCII character:

| Marker | State |
|---|---|
| `[ ]` | Not started. |
| `[>]` | Running. |
| `[v]` | Done. |
| `[!]` | Failed. |
| `[-]` | Skipped. |
| `[x]` | Cancelled or deleted. |

The detail of the row says what the step came to and how many chances it has
left. A step that failed never reads like a step that has not started.

The row is the same row in every channel that renders a checklist: Feishu,
Telegram and Discord all render the number, the marker, the subject and the
detail of the row in the same way. The subject of a `/me` row is the id of the
step, and its detail carries facts only — the words of the outcome and the
chances left. The branch the step worked on is not
in the detail: the card the operator read names it.

```
💭 8 · 🔧 6 · ⏱ 14:35:22

rounds: 4
├─ [ 1 ] v 10:33:04 · 5m25s decisions:  system-1 ×1 · operator ×1 ·  tokens - 32.4k / 8.1k
└─ [ 2 ] > 10:38:29
🧑‍💻: main · round 2

**📋 Tasks**
1. [v] fix (the fix landed)
2. [>] commit (3 chances left)
3. [!] review (the review is incomplete · 1 chance left)
4. [ ] push
5. [-] pr

---
🤖: me · jev-1.14 · main · round 2
```

The card of a decision shows that the agent took the workflow over, why the
decision was taken, what the models decided and what the chat model said. Its
first line is the takeover: the agent's mark, in bold, so that the takeover is
seen at once among the entries of the card.

```
**🧑‍💻: NightMe is taking over the workflow · review**

reason: the previous step "review" ended as "failed" — the review is incomplete

the decision model — /gtw fix 43 -y (p=0.91)

the review needs a second look, the diff is bigger than I expected

---
🤖: me · jev-1.14 · decision-3 · /gtw fix 43 -y (p=0.91)
```

The record of a finished session rides on the card of the run — or on the card of the `/me stop` message when the operator stopped the run:

```
■ [ fix-issue ] stopped
stopped while: /gtw fix 43 -y

duration: 42m14s
jobs:     ship × 7
actions:  63 (41 ok · 3 failed · 2 noop · 1 cancelled)
steps:    9 ran · 2 not run

rounds:
├─ [ 1 ] v 10:33:04 · 5m25s decisions:  system-1 ×1 · operator ×1 ·  tokens - 32.4k / 8.1k
└─ [ 2 ] ! 10:38:29 · 12m02s decisions:  system-1 ×2 · system-2 ×1 ·  tokens - 48.1k / 12.3k

errors (last 5 of 6):
├─ [ ! ] /gtw fix 43 -y — failed: the issue is claimed by another run (reason: issue_claimed)
└─ [ ! ] /gtw commit — failed: nothing to commit (reason: no_diff)

decisions (last 10 of 23):
├─ the decision model — /gtw fix 43 -y (p=0.91) · because: the previous step "review" ended as "failed"
└─ the chat model — run_step(step_id=review) · because: the plan is exhausted
```

The facts come from the run's own record: no model is asked to write the record. The headline names the phase the run ended in, and a run that failed says why under it — an error that never reached an action of the run leaves no row in the `errors` block. The `errors` are the actions whose outcome was a failure or a cancellation, at most the last five of them; the `decisions` are the judgments the models made at the wake-ups, at most the last ten. The tree of the rounds carries the counters of the run: the number of decisions and the tokens ride in its last row, so a run that finished no round shows neither. Its heading stays plain: the number of rounds is the state of a run that is still going, and the record describes a run that is over. A row is cut short: a shell step reports a whole build log as its summary, and a card that carries it is unreadable.

The wake log holds the model decisions only: the workflow's own fast path is not a decision a model made, so a run that goes through its workflow without asking a model holds no wake records at all. A decision model answer and a chat model turn are two records.

### 8.11 Guardrails and recovery

| Guardrail | Bound |
|---|---|
| Step budget | `me.max_judgments` chances at one step in one round, counted per round. |
| Rounds | `iterations` for a job — the number of rounds it runs. `/me start -i <rounds>` sets it for that run. |
| Compaction | Every tool result but the last keeps its status and summary; the last one keeps its transcript. |
| Rate limit | A command the agent answered 429 pauses the run; the operator continues it. A chain of agents absorbs the throttle it can — the next agent takes the work over — and only the throttle the chain cannot absorb pauses the run. |

A text-only answer — words without a tool call — does not end the run. The words stay in the conversation, the turn costs one chance of the step the judgment was about, and the run asks the agent again with a fresh situation report.

A chat model that failed or that never answered costs the same one chance. The wait before the next attempt grows: the first failure waits 30 seconds, the next waits 60, then 120, 240, and 300 seconds from then on. The wait belongs to the run — stopping the run or shutting the daemon ends it at once.

One turn of the chat model lasts at most five minutes: the chat model call carries that deadline, so the run does not wait for the client's own timeout.

A command the underlying agent answered 429 pauses the run: the agent is throttled, and running the command again makes it worse. A chain of agents absorbs the throttle it can — the next agent takes the work over and the run goes on — so the run waits for the operator only when the throttle reaches it. The operator sets it going again with `/me resume`. The pause is the run's own: the model is not asked to think about a throttle.

**What the 429 means.** The status of the outcome is the fact: a command that answers 429 is throttled, whatever its words say. The words are the fallback for the bridges that do not report a status yet — `rate_limit_error`, `too many requests` and a number in the transcript of the turn all say the same thing.

**What a throttle does to the run.** The chain of agents of the step is the fallback: the next agent of the chain takes the work over, and the run goes on. When the chain is exhausted and the status is 429, the run pauses — the agent is not woken up, because a throttle is not a judgment to make. Every other failure is a judgment point: the decision model is asked what happens next (see §8.3).

The run has no clock of its own. Every command it issues carries its own budget — the agent behind the command reports its own outcome when that budget runs out — and the run acts on that outcome. A timeout is the agent's timeout, never the run's. The run watches: it stops when the command reports, when the operator stops it, or when the daemon shuts down.

A run lives in the chat it started in and in the memory of the daemon that runs it. Nothing of it is written to disk, and a daemon restart loses it: the chat that started the run holds no run any more, `/me status` says so, and `/me resume` and `/me next` are refused with the same reason. A run that the daemon abandons at its shutdown stops where it stands and publishes nothing to the chat: the cards the run posted stay, and the last one still reads "running". A run state file left in `<DataDir>/workflows/runs/` by an older daemon stays where it is: nothing reads it.

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
| Parallel execution | One run drives one chat, so the branches of a graph express ordering, not concurrency. |
| Real `notify` / `email` / `github_*` actions | They report `"stub": true`; making them real is their own change. |
| A human-gate node kind | Human input stays the agent's `ask_human` tool at a judgment point. |

## 10. Cross-references

| Topic | Where |
|---|---|
| Trigger sources | [`feat/F-workflow-engine.md`](./feat/F-workflow-engine.md) |
| Mention parsing (git provider) | [`feat/F-workflow-engine.md`](./feat/F-workflow-engine.md) |
| Agent names | [`primary-agent-detection.md`](./primary-agent-detection.md) |
| The chat a run works in | [`../internal/channel`](../../internal/channel) |
| What the daemon said | [`../internal/gateway/observe`](../../internal/gateway/observe) |
| The `wfe` engine | [`../../pkg/wfe`](../../pkg/wfe) |
| The outcome contract | [`me.md`](./me.md) |
