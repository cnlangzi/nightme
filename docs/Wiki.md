# Wiki

`/wiki init` establishes a wiki for the repository at `<cwd>`. The wiki is a set of plain Markdown files under `<cwd>/wiki/modules/`, one per concept. The structure is the durable documentation asset the project uses to onboard new contributors and to ground future Agent sessions.

The source code is authoritative. The wiki is generated from the source tree by the Agent.

## 1. Goals

The wiki answers these questions for a contributor or Agent that has just arrived at the repository:

- What are the major conceptual areas of this project?
- For each concept, what is it responsible for and where does one start reading?

## 2. Scope

`/wiki init` covers:

- A zero-argument slash command.
- A single Agent invocation per `/wiki init` call.
- Per-module Markdown files under `<cwd>/wiki/modules/`.
- A module's identity is its filename.

`/wiki init` does not cover:

- A central index file. No `wiki.yml`, no `llms.txt`. The file system is the index: a directory listing of `wiki/modules/` is the table of contents.
- A re-extraction or refresh command.
- Aggregate pages such as `architecture.md` or `quickstart.md`.
- A bounded-prompt orchestration. Init runs as one Agent invocation; per-module file production is part of that single invocation.
- Module identity naming rules beyond kebab-case uniqueness. The Agent chooses the concept boundaries and the names.

## 3. Command contract

```text
/wiki init
```

`/wiki init` takes no flags and no arguments. The Agent is selected exclusively by `/use <agent>`; init does not accept `-a` or `--agent`.

### 3.1 Preconditions

The command requires:

1. A selected CWD on the ChatSession.
2. A selected Agent configured through `/use`.
3. A live or resumable AgentSession returned by `cs.LookupSelectedAgentSession()`.
4. A Git repository containing the selected CWD.
5. A source-clean working tree.
6. The path `<cwd>/wiki/modules/` does not already exist. Init refuses to overwrite an existing wiki.

Failure messages identify the corrective action:

- No CWD: `no workspace set; run /cwd <path> first`
- No Agent: `no active agent; run /use <agent> first`
- Invalid arguments: `usage: /wiki init`
- Not a Git repository: `not a git repo (or git unavailable): ...`
- Dirty source tree: `working tree has source changes; commit first`
- Wiki already exists: `wiki/modules/ already exists; refusing to overwrite`

### 3.2 Agent path

The command uses the standard ChatSession path:

```text
/wiki init
  -> command.Handle
  -> ChatSession.LookupSelectedAgentSession
  -> ChatSession.QueueUserMessage (MessageKindQueue)
  -> AgentSession.Submit
  -> bridge.SendBlocks
  -> Agent CLI
  -> Agent readpump
  -> ChatSession AgentEventBus / PromptEndBus
```

Init submits exactly one Agent prompt. The prompt instructs the Agent to read the codebase, design a module structure, and write per-module Markdown files directly. The Agent returns a short summary; the runtime does not parse a framed result for init.

### 3.3 Module ownership

Init runs as a one-shot command. There is no Job registry, no pending queue, and no incremental state file. The wiki is established or it is not; the runtime does not track progress across invocations.

## 4. Module file format

Every module file is a plain Markdown file. The module's identity is its filename: `<cwd>/wiki/modules/<name>.md` where `<name>` is the kebab-case ID the Agent chose.

The file content is exactly:

```markdown
# <Display Name>

> <One-line purpose — what this concept IS responsible for>

<body>
```

Three rules:

- The file's basename (without `.md`) is kebab-case: lowercase letters, digits, and hyphens; 1-5 hyphen-separated words; must start and end with a letter or digit.
- The first non-empty line is the H1 heading (`# <something>`). The H1 text need not match the filename; the filename is the identity.
- The next non-empty line is a one-line blockquote (`> <something>`) stating the module's purpose in one sentence.

There is no frontmatter. There is no metadata block. The runtime only parses three things — filename, H1, blockquote — and otherwise trusts the body.

### 4.1 Body

Everything below the blockquote is the Agent's call. The Agent decides whether the module warrants:

- A state table.
- A flow description.
- Entry points and their source anchors.
- A list of related modules with links.
- Free-form paragraphs.

The body is the documentation; the filename plus the H1 plus the blockquote are the contract.

### 4.2 Cross-module references

When one module's body refers to another module, the link uses the other module's filename as written:

```markdown
See [other-name](other-name.md) for the prompt submission path.
```

The runtime does not validate that the link target exists; this is a documentation convention the Agent maintains.

## 5. The init prompt

The runtime submits this prompt as the Agent's task. `<REPO_ROOT>` is the resolved repository root; the Agent's file tools operate relative to it.

```markdown
# /wiki init — establish the wiki module structure

You are the lead architect of the repository at <REPO_ROOT>.
You know this codebase deeply. You are creating a durable
project documentation asset that the team will rely on
for years.

## Tasks

### 1. Read the codebase

Use your file tools to understand this project — its purpose,
its major conceptual areas, the patterns it uses, and what
a new contributor needs to know. Read enough to ground your
design.

### 2. Design the module structure

A module is a CONCEPT — a unit of coherent responsibility.
Modules may span directories; one directory may contain
multiple concepts. Design the partitioning that makes the
most sense to a new contributor who will read this wiki
next month.

### 3. Write per-module files

For each module, write one file at:

  <REPO_ROOT>/wiki/modules/<name>.md

The filename <name> is the module's identity. Use kebab-case
(lowercase letters, digits, and hyphens; 1-5 words). Do not
include any extension other than .md.

Each file MUST be plain Markdown with exactly this shape:

  # <Display Name>

  > <One-line purpose — what this concept IS responsible for>

  <body — your call>

That's it. No frontmatter. No covers list. No metadata block.
The runtime only reads three things: the filename, the H1,
and the blockquote. Everything else is yours.

## When done

Reply with a one-paragraph summary of the modules you created.
```

The runtime does not require a framed result for init. The Agent's chat reply is the only feedback the runtime uses to decide whether init succeeded.

## 6. Validation

The runtime validates the Agent's output by inspecting the file system after the Agent's response:

- The directory `<cwd>/wiki/modules/` exists and contains at least one `.md` file.
- Every `.md` file's basename matches the kebab-case pattern.
- Every `.md` file has a unique basename across `wiki/modules/`.
- Every `.md` file begins with an H1 line.
- Every `.md` file has a one-line blockquote immediately after the H1.

If any check fails, the runtime returns the failure to the user. The Agent's chat summary may name modules that the runtime failed to find on disk; those modules are reported in the failure reply so the user can ask the Agent to write them.

## 7. Command reply

On success, the runtime replies:

```text
✅ /wiki init

N module(s) created under wiki/modules/.
Read the summary above for module names.
```

On validation failure:

```text
❌ /wiki init: <reason>

Modules written but rejected:
- <name>: <reason>
- ...
```

The Agent's own chat reply is preserved as the leading content of the runtime's reply, so the user sees the Agent's module summary alongside the validation result.

## 8. Source code layout

```text
internal/command/wiki/
├── cmd.go        slash command registration, zero-argument validation, ChatSession handoff
├── init.go       the init prompt, Agent submission, post-write validation
├── validate.go   filename identity, H1 + blockquote check
└── git.go        git preflight (RepoRoot + IsClean)
```

The package does not contain an Agent provider abstraction, a one-shot runner, a Job registry, a pending queue, a batched Apply loop, stubs, or frontmatter parsing.

## 9. Verification contract

Tests cover:

- `/wiki init` rejects every argument, including `-a` and `--agent`.
- Missing CWD and missing selected Agent fail before filesystem writes.
- `LookupSelectedAgentSession` is used so detached sessions resume.
- A nested ChatSession CWD resolves to the Git worktree root.
- Source-clean rejects non-Wiki changes and permits resumable generated changes.
- Dirty and non-Git repositories are rejected.
- An existing `wiki/modules/` directory causes init to refuse.
- Filenames are validated as kebab-case and unique.
- Files beginning with an H1 line and a one-line blockquote are accepted.
- Files missing the H1 or blockquote are reported with the specific reason.
- No `wiki.yml` or `llms.txt` is written by init.

Repository verification follows `AGENTS.md`: formatting, tests, lint, and build must pass.

## 10. References

- [AGENTS.md](https://agents.md) — repository instructions for coding Agents.
- [Aider Repository Map](https://aider.chat/docs/repomap.html) — token-budgeted source-derived repository maps.
