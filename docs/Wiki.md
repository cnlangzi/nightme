# Wiki

`/wiki` builds an LLM-oriented repository map under `<cwd>/wiki/` and stores incremental state in `<cwd>/wiki.yml`.

The source code is authoritative. Wiki pages are generated navigation artifacts that provide a global view, summarize module responsibilities, and direct an Agent to relevant source files and symbols. Agents use the Wiki to decide what code to inspect, not as a substitute for inspecting code.

`/wiki` runs as a normal task on the ChatSession's selected long-running AgentSession. Agent selection, process spawning, resume, permissions, event delivery, cancellation, and failure recovery use the same path as ordinary chat messages.

## 1. Goals

The Wiki lets a contributor or coding Agent answer these questions with a small context budget:

- What are the repository's main areas and entry points?
- Which module is relevant to this task?
- Which source symbols and tests should be inspected first?
- How does this module connect to adjacent modules?
- Which cross-module flow should be followed when the task spans boundaries?

The generated content follows these principles:

- Code is the source of truth for behavior and structure.
- `AGENTS.md` and equivalent repository rule files are the source of truth for coding and workflow instructions.
- Every summary leads back to source paths or symbols.
- Pages are disposable and can be regenerated from the repository.
- Content is loaded progressively; an Agent does not need the complete Wiki in context.
- Generated prose describes observable structure. It does not invent design intent or normative rules.

## 2. Scope

### In scope

- A zero-argument `/wiki` slash command.
- `<cwd>/wiki/` generated content and `<cwd>/wiki.yml` incremental metadata.
- Hierarchical `llms.txt` indexes for progressive disclosure.
- A repository Quickstart for coding tasks.
- A generated architecture overview.
- One generated module page per non-empty source directory.
- Git-driven incremental planning.
- Resumable pending state.
- LLM generation through the ChatSession's selected long-running AgentSession.
- Deterministic validation and index rendering.

### Out of scope

- Agent selection flags or temporary Agent switching.
- One-shot execution through `agent.Builtins`.
- A Wiki-specific Agent, AgentSession, bridge, provider, model, or credential path.
- A replacement for source inspection, CodeGraph, language servers, or search.
- Human-authored design decisions, ADRs, coding rules, or changelogs.
- Persistent task playbooks for every possible change type.
- Complete API references or complete call graphs.
- Git commit, stash, or push operations.

## 3. Command contract

```text
/wiki
```

`/wiki` accepts no flags and no positional arguments. In particular, `-a` and `--agent` are invalid. The selected Agent is controlled exclusively by `/use <agent>`.

### 3.1 Preconditions

The command requires:

1. A selected CWD on the ChatSession.
2. A selected Agent configured through `/use`.
3. A live or resumable AgentSession returned by `cs.LookupSelectedAgentSession()`.
4. A Git repository containing the selected CWD.
5. A source-clean working tree.

Failure messages identify the corrective action:

- No CWD: `no workspace set; run /cwd <path> first`
- No Agent: `no active agent; run /use <agent> first`
- Invalid arguments: `usage: /wiki`
- Not a Git repository: `not a git repo (or git unavailable): ...`
- Dirty source tree: `working tree has source changes; commit first`

The selected AgentSession is resolved before Wiki state is written. A failure to activate the Agent does not leave a scaffold or pending plan behind.

The command resolves the project root with:

```text
git -C <selected-cwd> rev-parse --show-toplevel
```

Discovery, Git operations, `wiki/`, and `wiki.yml` use this repository root. A ChatSession CWD inside a repository does not create a partial Wiki in that subdirectory.

Source-clean means:

- Without resumable pending state, the complete working tree is clean.
- With pending state, modifications to `wiki.yml`, deterministic indexes, and generated pages named by metadata or pending entries are allowed.
- An unrecognized path under `wiki/` is an output-boundary violation rather than an implicitly trusted generated file.
- Tracked or untracked changes outside those generated paths are rejected.

This distinction allows interrupted Wiki generation to resume without permitting uncommitted source to enter generated summaries.

### 3.2 Agent path

The command uses this path:

```text
/wiki
  -> command.Handle
  -> ChatSession.LookupSelectedAgentSession
  -> ChatSession.QueueUserMessage
  -> AgentSession.Submit
  -> bridge.SendBlocks
  -> Agent CLI
  -> AgentSession readpump
  -> ChatSession AgentEventBus / PromptEndBus
```

The Wiki prompt is a `MessageKindQueue` message so it forms a standalone Prompt and does not merge with ordinary user messages.

The Wiki message follows the normal ChatSession queue semantics. If it is still queued, `/use` may change which selected AgentSession consumes it. Once `AgentSession.Submit` accepts the Prompt, its AgentSession ID is fixed; a later `/use` does not cancel or retarget the in-flight Prompt.

The queued `Message.ID` is the channel-native ID of the user's `/wiki` message, so ordinary Agent output can attach to a real message. A separate internal Job ID identifies the Wiki run. Events are correlated by Job ID, AgentSession ID, Prompt ID, and channel message ID; an internal identifier is never used as a Channel reply anchor.

The Agent uses its existing tools and permissions to inspect source files and write generated Wiki pages. `/wiki` does not add tools or capabilities to the Agent.

### 3.3 Job ownership

Apply runs under a command-layer Wiki Job coordinator. This coordinator changes no Agent interface and owns only orchestration around the normal ChatSession path.

The process-wide Job key is the canonical repository root. The Job record stores its owning ChatSession ID. Only one active Job may update a repository, including calls from different chats bound to the same CWD. A concurrent `/wiki` for the same repository returns:

```text
wiki job already running
```

The Job context derives from `cs.Context()`, not from the slash-command handler context. It therefore survives after `Handle` returns its acknowledgement and is cancelled when the ChatSession shuts down.

The coordinator owns:

- EventBus subscriptions and unsubscription.
- Prompt and Job correlation.
- Output allowlist snapshots.
- Page validation and metadata finalization.
- Final status delivery through the ChatSession emitter.

Daemon termination clears the in-memory Job registry. Persistent pending state remains the recovery source for the next invocation.

## 4. Progressive disclosure

The Wiki exposes five levels of context:

```text
AGENTS.md
  -> wiki/llms.txt
    -> wiki/modules/<area>/llms.txt
      -> wiki/modules/<source-path>/index.md
        -> source files, symbols, and tests
```

Each level contains only enough information to choose the next level.

### Level 0: repository rules

`AGENTS.md` remains the always-loaded instruction layer. A repository may add this pointer:

```markdown
For unfamiliar tasks, read `wiki/llms.txt`; load only relevant linked pages.
```

`/wiki` may report that the pointer is absent, but it does not edit `AGENTS.md`.

### Level 1: root index

`wiki/llms.txt` is the discovery entry point. It contains:

- Project name and one-sentence description.
- A link to Quickstart.
- A link to Architecture, marked for cross-module tasks.
- Links to top-level source areas.
- Common executable or service entry points.
- Links to repository rule files.

It does not enumerate every module in a large repository.

### Level 2: directory indexes

Each source area has a local `llms.txt` that lists only its direct child modules and child areas. Each link includes a one-sentence description that lets the Agent decide whether to open it.

An index that exceeds its budget delegates entries to deeper indexes. It does not truncate modules.

### Level 3: module pages

A module page gives the shortest useful map from a responsibility to source code. Related modules are linked, not embedded.

### Level 4: architecture overview

`architecture.md` is optional for local changes. It is read when:

- A task crosses module boundaries.
- The data or event flow is unclear.
- An extension point must be identified.
- Several module pages appear relevant.

### Level 5: source

The final step is always source inspection. The Agent verifies Wiki claims against source, callers, tests, generated code, build tags, and configuration before editing.

### 4.1 Context budgets

- Root `llms.txt`: 300–500 tokens.
- Directory `llms.txt`: 200–500 tokens.
- Module page: 300–600 tokens.
- Quickstart: 500–800 tokens.
- Architecture: 800–1,200 tokens.

Budgets are soft limits. Content is compressed by field priority:

1. Purpose
2. Entry Points
3. Main Flows
4. Source Anchors
5. Where to Change
6. Related Modules

Required fields are not truncated mid-section.

## 5. File layout

```text
<cwd>/
├── wiki.yml
└── wiki/
    ├── llms.txt
    ├── quickstart.md
    ├── architecture.md
    └── modules/
        ├── llms.txt
        ├── cmd/
        │   ├── llms.txt
        │   └── nightme/
        │       └── index.md
        └── internal/
            ├── llms.txt
            ├── chatsession/
            │   └── index.md
            ├── gateway/
            │   ├── index.md
            │   └── llms.txt
            └── bridge/
                ├── index.md
                ├── llms.txt
                └── claudecode/
                    └── index.md
```

A source directory maps to a Wiki directory:

```text
internal/bridge/claudecode/
  -> wiki/modules/internal/bridge/claudecode/index.md
```

This mapping gives every module a stable identity without asking the LLM to design page names or semantic clusters.

A module directory receives `llms.txt` when it has child module directories. `wiki/modules/llms.txt` indexes the top-level source areas.

## 6. Page formats

### 6.1 Root `llms.txt`

The root index follows the `llms.txt` link format:

```markdown
# NightMe

> Remote-pair agent runtime connecting chat platforms to coding agents.

## Start

- [Quickstart](./quickstart.md): Locate code and run repository verification.
- [Architecture](./architecture.md): Cross-module components and main flows.

## Rules

- [Agent Instructions](../AGENTS.md): Build, test, style, and runtime constraints.

## Areas

- [Commands](./modules/internal/command/llms.txt): Slash-command implementations.
- [Bridges](./modules/internal/bridge/llms.txt): Agent CLI transports.
- [Channels](./modules/internal/channel/llms.txt): Chat-platform adapters.
- [Runtime](./modules/internal/llms.txt): Sessions, routing, persistence, and workflows.
```

Links use descriptions, not bare basenames. Lower-priority references may appear under `## Optional`.

### 6.2 Directory `llms.txt`

```markdown
# internal/bridge

> Agent transport implementations and shared bridge primitives.

## This module

- [Overview](./index.md): Shared bridge contracts and registration points.

## Children

- [Claude Code](./claudecode/index.md): Claude CLI transport and event parsing.
- [Codex](./codex/index.md): Codex process and protocol transport.
- [ACP](./acp/index.md): Shared Agent Client Protocol transport.
```

The `This module` section is omitted when the corresponding source directory contains no direct source files.

### 6.3 Module page

Every module page uses these sections in order:

```markdown
# internal/bridge/claudecode

> Claude Code CLI transport and event translation.

## Purpose

## Entry Points

## Main Flows

## Related Modules

## Where to Change

## Source Anchors
```

Section rules:

- `Purpose`: one short paragraph describing observable responsibility.
- `Entry Points`: the smallest set of types, functions, commands, registries, or factories worth reading first.
- `Main Flows`: no more than three concise paths from entry to output.
- `Related Modules`: direct relationships with links to module pages.
- `Where to Change`: common change targets and their source or test areas.
- `Source Anchors`: repository-relative file paths and symbol names.

Module pages do not contain:

- A complete exported API list.
- A complete file list or line-count table.
- Long code excerpts.
- Unsupported design motivations.
- Normative statements inferred from implementation.
- Changelogs, generation logs, or task history.
- Duplicate coding rules from `AGENTS.md`.

### 6.4 Architecture

`wiki/architecture.md` uses:

```markdown
# Architecture

## Components

## Entry Points

## Main Flows

## Dependency Direction

## Extension Points

## Cross-cutting Concerns

## Source Anchors
```

It is a generated overview of observable code structure:

- Components groups major responsibilities without redefining module identity.
- Entry Points identifies binaries, servers, workers, commands, and plugin registries.
- Main Flows contains a small number of end-to-end paths.
- Dependency Direction describes major dependencies present in code.
- Extension Points identifies interfaces, registration functions, factories, and adapters.
- Cross-cutting Concerns points to error handling, persistence, concurrency, platform splits, and configuration hotspots.

Architecture does not claim why a design exists unless a repository source such as an ADR explicitly states it.

### 6.5 Quickstart

`wiki/quickstart.md` is a coding-task Quickstart:

```markdown
# Quickstart

## Repository Rules

## Development Setup

## Find the Relevant Code

## Make a Focused Change

## Verify
```

It derives setup and verification commands from repository files such as `AGENTS.md`, `Makefile`, package manifests, CI configuration, and development scripts. Product installation and end-user tutorials remain in the project's normal README or documentation.

## 7. Module discovery

`discoverModules(cwd)` walks the source tree. A directory is a module when:

- It survives the ignore filter.
- It contains at least one non-hidden regular file.

The walk continues into child directories after emitting a parent module. Test-only directories are modules.

The ignore order is:

1. Built-in exclusion of `wiki/` and `wiki.yml`.
2. Repository `.gitignore`.
3. Dot-prefixed path components.
4. Exact `wiki.yml.include` opt-ins for paths intentionally restored from exclusions.

An include is exact, not recursive. Nested hidden directories require their own include entry.

The generated directory hierarchy follows source paths deterministically. The LLM does not rename, merge, split, or relocate modules.

## 8. Metadata

`wiki.yml` stores generation state, not repository knowledge:

```yaml
version: 1
last_commit: null
plan_sha: abc123
include: []
aggregates:
  architecture:
    file: architecture.md
    last_sha: abc123
    prompt_version: 1
    dirty: false
    status: done
    # before_hash: "sha256:..."
    # error: "<message>"
  quickstart:
    file: quickstart.md
    last_sha: abc123
    prompt_version: 1
    dirty: false
    status: done
    # before_hash: "sha256:..."
    # error: "<message>"
modules:
  - path: internal/bridge/claudecode
    file: internal/bridge/claudecode/index.md
    purpose: "Claude Code CLI transport and event translation."
    last_sha: abc123
    prompt_version: 1
    # removed: true
pending:
  - path: internal/bridge/claudecode
    action: regenerate
    reason: "source changed since abc123"
    files_changed:
      - internal/bridge/claudecode/session.go
    status: pending
    # before_hash: "sha256:..."
    # error: "<message>"
```

Fields:

- `version`: metadata schema version.
- `last_commit`: source HEAD of the last state where pending is empty and every aggregate is clean.
- `plan_sha`: source HEAD used to construct the pending plan.
- `include`: exact path opt-ins for discovery.
- `aggregates`: independent state for Architecture and Quickstart.
- `aggregates.*.last_sha`: source HEAD associated with the validated aggregate.
- `aggregates.*.prompt_version`: prompt contract applied to the aggregate.
- `aggregates.*.dirty`: the aggregate requires generation after module finalization.
- `aggregates.*.status`: `pending`, `in_progress`, `done`, or `failed`.
- `aggregates.*.before_hash`: aggregate content hash captured when Apply starts.
- `aggregates.*.error`: the last generation or validation failure.
- `modules[].path`: source module directory.
- `modules[].file`: module page relative to `wiki/modules/`.
- `modules[].purpose`: short generated description used by parent indexes.
- `modules[].last_sha`: source HEAD associated with the validated module page.
- `modules[].prompt_version`: prompt contract applied to the module page.
- `modules[].removed`: source directory is absent.
- `pending`: persistent Plan output consumed by Apply.
- `pending[].before_hash`: target content hash captured when Apply starts; absent means the target did not exist.

There is no Agent field. Agent selection is ChatSession state owned by `/use`.

Pending actions:

- `new`: generate a page for a discovered module without metadata.
- `regenerate`: refresh a page whose source or prompt contract changed.
- `delete`: delete generated content for a removed module.

Pending statuses:

- `pending`: not processed.
- `in_progress`: generation started without a validated completion.
- `done`: output passed validation.
- `failed`: generation or validation failed.

Done entries are removed from `pending`. Failed and interrupted entries remain retryable.

Prompt versions are applied per page. A partially completed run never stamps the new version onto failed pages. The implementation's prompt constants are compared with each page's stored version during Plan.

## 9. State reconciliation

`/wiki` supports these repository states:

| `wiki.yml` | `wiki/` | Behavior |
|---|---|---|
| absent | absent | Create metadata and generated directory structure |
| present | present | Reconcile and update |
| present | absent | Recreate generated content from metadata and source |
| absent | present | Reconstruct metadata from generated paths and source |

Generated content never overrides repository rules or source files.

## 10. Plan

Plan is deterministic and performs no LLM work:

1. Resolve the repository root and Git HEAD.
2. Load metadata and verify source-clean state.
3. Pre-validate files left by an interrupted run. An `in_progress` module or aggregate whose content differs from `before_hash` and passes validation may be finalized without regeneration.
4. Discover modules and their directly owned files.
5. Reconcile discovered modules with `wiki.yml.modules`.
6. Add `new` for discovered modules without metadata.
7. Add `regenerate` when:
   - `last_sha` is absent.
   - A directly owned file changed between `last_sha` and HEAD.
   - The stored page `prompt_version` differs from the implementation's module prompt version.
   - The generated module page is absent or structurally invalid.
8. Add `delete` for metadata modules absent from source.
9. Merge matching `failed` and `in_progress` entries into the plan instead of discarding their status and error context.
10. Mark Architecture dirty when any module action exists.
11. Mark Quickstart dirty when its tracked source set or prompt version changes.
12. Set `plan_sha` to HEAD and persist `pending` and aggregate state.

### 10.1 Direct-file ownership

A module owns regular files directly inside its source directory. It does not own descendant module files.

Changed files are assigned to their containing source directory:

```text
internal/bridge/session.go
  -> internal/bridge

internal/bridge/claudecode/session.go
  -> internal/bridge/claudecode
```

Plan may compute one or more Git diffs and then filter by exact containing directory. It must not use a recursive path filter as the final module-change decision:

```text
git diff <sha>..HEAD -- internal/bridge
```

The recursive form includes descendants and would regenerate every ancestor page after a leaf change.

When a direct file is deleted and its directory no longer qualifies as a module, reconciliation emits `delete`. Child module changes rebuild ancestor indexes deterministically but do not regenerate unchanged ancestor module pages.

### 10.2 Replanning

When `plan_sha` differs from HEAD, Plan recomputes changed files against each page's `last_sha` and merges the resulting actions with retryable pending entries. Generated pages associated only with the older plan are not accepted without validation against the new HEAD.

An unreachable stored SHA causes full regeneration of the affected page. It is not treated as “no change.”

Pending merge is keyed by `(path, action)`. A recomputed action replaces stale `reason` and `files_changed`; existing failure information remains available until Apply retries the entry. A conflicting action for the same path is discarded, such as `regenerate` becoming `delete`.

## 11. Apply

Apply uses the selected long-running AgentSession.

### 11.1 Processing order

1. Retryable `in_progress` and `failed` live modules.
2. Other live modules, deepest source path first.
3. Parent modules.
4. Removed modules, processed mechanically.
5. Architecture and Quickstart after no live module entry remains pending.
6. Deterministic hierarchical indexes after every successful batch.

Child pages therefore exist before parent indexes and aggregate pages are rendered.

### 11.2 Bounded batch

One `/wiki` invocation submits at most one Agent Prompt. Its generation batch contains at most eight target pages or 64 directly owned source files across those targets, whichever limit is reached first. A single module is never split across batches, even when that module alone exceeds the source-file limit.

Delete actions do not consume Agent batch capacity. Apply removes their generated paths mechanically and records filesystem failures.

When no live module entry remains pending, Apply completes delete actions before selecting aggregate targets. Architecture therefore observes the module set after successful deletion. A failed deletion remains pending and prevents Architecture from being marked clean.

When pending work exceeds the batch limit, finalization reports the remaining count. A subsequent `/wiki` uses the long-running AgentSession selected through `/use` at that invocation; `/wiki` creates no Agent or conversation.

Architecture and Quickstart enter a batch only after all live module pages they depend on validate. Root and directory indexes include only validated pages that exist, so a partially generated Wiki remains navigable.

This bounded model provides a checkpoint at every invocation and avoids an unbounded Prompt whose partial progress cannot be correlated reliably.

### 11.3 Agent task

The Wiki task is submitted through `cs.QueueUserMessage` as one standalone `MessageKindQueue` Prompt. The prompt contains:

- The resolved repository root.
- The page contracts and context budgets.
- The path to `wiki.yml.pending`.
- The exact ordered target-page allowlist for this batch.
- The instruction to treat code as authoritative.
- The instruction to use existing source-reading and file-editing tools.
- The instruction not to edit `wiki.yml` or generated `llms.txt` indexes.
- The required framed final result.

Before queueing, Apply stores each target's `before_hash`, changes its status to `in_progress`, and atomically writes `wiki.yml`. If queueing fails, those targets return to `pending` with the queue error and the Job key is released.

The Agent writes target Markdown files directly. Its ordinary tool and permission events continue through the ChatSession event pipeline. The final response stays concise and does not reproduce generated pages in chat.

The final response uses:

```text
WIKI_RESULT_BEGIN
{"job_id":"wiki-...","completed":["wiki/modules/internal/gateway/index.md"],"failed":[]}
WIKI_RESULT_END
```

Only allowlisted paths are accepted. A failure item has `path` and `error` string fields. The JSON object contains no generated page content.

The command observes `AgentEventBus` and `PromptEndBus` using the Job, Prompt, AgentSession, and channel message identifiers. While the message is queued, the coordinator matches the channel message ID. The first event for its submitted Prompt binds the owning AgentSession ID and Prompt ID; subsequent events must match both. This preserves normal `/use` queue semantics without mixing another AgentSession's output.

The collector accumulates both `EventAgentText` and `EventAgentResult`, because not every bridge emits a result event. `PromptEndBus` terminates collection but does not by itself indicate successful generation.

This observation coordinates completion; it does not create a separate Agent execution path or suppress normal ChatSession events.

### 11.4 Finalization

After Prompt completion, `/wiki`:

1. Extracts the last complete framed result matching the Job ID.
2. Rejects result paths outside the batch allowlist.
3. Validates every target reported completed.
4. Sets `last_sha = plan_sha`, stores the applied prompt version, and refreshes `purpose` for validated pages.
5. Marks reported failures, missing files, and invalid targets failed.
6. Generates directory `llms.txt` files from source paths and validated module purposes.
7. Generates root `llms.txt`.
8. Updates aggregate state and `last_commit` when aggregate pages validate.
9. Removes done pending entries and persists failures.

If framing is absent or malformed, files whose content changed during the batch and passes validation may be accepted. An unchanged pre-existing file is not accepted without a matching completed result. Remaining targets stay failed or pending for retry.

If the Agent process exits, the Prompt is stopped, or the daemon terminates before finalization, `in_progress` entries remain. The next `/wiki` retries them.

### 11.5 Output boundary

Before submitting the Prompt, the coordinator records:

- Git status.
- Content hashes for allowlisted target pages in `pending[].before_hash`.
- Existing generated paths.

After Prompt completion, only these Agent-written paths are permitted:

- The exact target-page allowlist.

The command itself may write `wiki.yml` and generated `llms.txt` indexes.

Generated files and their parent directories must not be symbolic links. Containment checks use cleaned, resolved paths under the repository's physical `wiki/` directory rather than lexical prefix checks alone.

Any other tracked or untracked change is an output-boundary violation. Apply:

- Does not delete or revert the unexpected change.
- Does not finalize the affected batch.
- Lists the unexpected repository-relative paths in the final error.
- Leaves pending state retryable.

An Agent modification to `wiki.yml` or an `llms.txt` file is also a boundary violation; these files are command-owned.

## 12. Validation

Validation is mechanical:

- Required headings are present and ordered.
- Page size is within a bounded tolerance.
- Source paths are repository-relative and exist.
- Source anchors name a file and, when supplied, a symbol.
- Module and index links resolve.
- Every index link has a description.
- A module has Purpose, Entry Points, and Source Anchors.
- No `[TBD]` placeholders remain in a completed page.
- Generated files stay under `wiki/`.
- Generated files and parent directories are not symbolic links.
- `wiki.yml` follows the supported schema.
- Framed results contain the active Job ID and allowlisted paths only.
- Agent-written paths stay inside the batch output allowlist.

File existence, path containment, Markdown shape, result framing, and link resolution are hard validation failures. Symbol validation is best-effort and language-aware when a parser is available; an unsupported language does not fail solely because a symbol cannot be mechanically resolved. The file anchor remains mandatory.

Semantic correctness is not inferred from successful validation. Agents still verify summaries against code before editing.

## 13. Incremental aggregation

Module pages regenerate only from changes under their source directory or a page-contract change.

Architecture regenerates whenever a module page is added, regenerated, or deleted. This deterministic fan-out avoids asking the planner to infer whether a local code change has architectural impact.

Quickstart regenerates when it is absent, its prompt contract changes, or its source files change. Its source set includes repository rules, build files, package manifests, CI configuration, development scripts, and executable entry points.

All generated pages regenerate when the page contract or generation prompt changes. Removing a module's `last_sha` requests regeneration of that module and the architecture aggregate.

Directory indexes are rendered from direct child metadata. A parent index does not copy descendant page content.

Architecture remains dirty until every live module pending entry completes and the aggregate itself validates. Quickstart tracks its own source set and prompt version independently. A successful module batch cannot accidentally mark either aggregate fresh.

Deterministic root and directory indexes rebuild on every `/wiki` finalization, including no-op finalization. They require no LLM call or freshness metadata.

## 14. Reply behavior

The slash command returns an immediate acknowledgement after the Wiki Prompt is queued:

```text
✅ /wiki queued

12 module page(s) to generate
2 module page(s) to delete
Queued in the ChatSession for /workspace/nightme
```

A no-op or purely mechanical cleanup invocation does not queue an Agent Prompt and returns the final result directly. Module deletion marks Architecture dirty, so a deletion may still require an aggregate-generation Prompt after the file is removed. If `QueueUserMessage` rejects the Wiki message, the Job releases its key and leaves the planned entries pending.

The long-running Agent uses the normal chat event stream for progress and permissions.

Finalization sends a compact result:

```text
✅ /wiki

Generated 12 module page(s).
Deleted 2 removed module page(s).
Rebuilt 8 index file(s).
Failed 1 module page(s); it remains pending.
9 module page(s) remain; run /wiki again to continue.
```

Raw generated page content and full source file lists are not included in command replies.

## 15. Package responsibilities

```text
internal/command/wiki/
├── cmd.go        command registration, zero-argument validation, ChatSession handoff
├── sync.go       preflight and Plan/Apply orchestration
├── plan.go       Git-driven reconciliation and pending ordering
├── apply.go      bounded Prompt coordination, Job ownership, and finalization
├── prompt.go     Wiki task prompt construction
├── validate.go   page, framed result, and output-boundary validation
├── discover.go   deterministic source module discovery
├── ignore.go     ignore and include rules
├── skeleton.go   empty recovery scaffolds and deterministic index rendering
├── storage.go    atomic metadata and generated-file writes
└── yaml.go       metadata schema and canonical encoding
```

The package does not contain an Agent provider abstraction or one-shot runner.

## 16. Verification contract

Tests cover:

- `/wiki` rejects every argument, including `-a` and `--agent`.
- Missing CWD and missing selected Agent fail before filesystem writes.
- `LookupSelectedAgentSession` is used so detached sessions resume.
- A nested ChatSession CWD resolves to the Git worktree root.
- Source-clean rejects non-Wiki changes and permits resumable generated changes.
- Concurrent Jobs for the same repository are rejected across ChatSessions.
- The Job survives the slash-command handler context and stops with `cs.Context()`.
- The Wiki Prompt is a standalone `MessageKindQueue`.
- The channel-native slash message ID is the output anchor; Job ID remains internal.
- Prompt completion is correlated to the Job, AgentSession, Prompt, and channel message.
- A queued Wiki message follows `/use`, while an already-submitted Wiki Prompt remains on its owning AgentSession.
- Each invocation respects the page and changed-file batch limits.
- Remaining pending entries survive for the next invocation.
- Dirty and non-Git repositories are rejected.
- Discovery and ignore behavior are deterministic.
- A leaf change does not regenerate ancestor module pages.
- Unreachable SHAs trigger regeneration.
- A changed `plan_sha` causes replanning against the new HEAD.
- Interrupted and failed work remains pending.
- Module paths map to `wiki/modules/<source-path>/index.md`.
- Hierarchical indexes list direct children only.
- Page validation rejects missing headings, invalid anchors, broken links, and placeholders.
- Text-only and Result-producing bridges both support framed result collection.
- Malformed or out-of-allowlist result paths are rejected.
- Unexpected Agent writes outside the target allowlist fail without destructive rollback.
- Prompt versions are tracked independently per module and aggregate.
- No Agent name is persisted in `wiki.yml`.
- No one-shot Agent path is invoked.

Repository verification follows `AGENTS.md`: formatting, tests, lint, and build must pass.

## 17. References

- [llms.txt](https://llmstxt.org) — LLM-oriented documentation indexes.
- [AGENTS.md](https://agents.md) — repository instructions for coding Agents.
- [Agent Skills](https://agentskills.io/specification) — progressive disclosure through metadata, activation, and on-demand references.
- [Aider Repository Map](https://aider.chat/docs/repomap.html) — token-budgeted source-derived repository maps.
