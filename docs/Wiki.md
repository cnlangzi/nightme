# Wiki

`/wiki init` establishes a wiki module structure for the repository at `<cwd>`. The wiki is a set of per-module Markdown files under `<cwd>/wiki/modules/`. The structure is the durable documentation asset the project uses to onboard new contributors and to ground future Agent sessions.

The source code is authoritative. The wiki is generated from the source tree by the Agent, then maintained incrementally as the source evolves.

## 1. Goals

The wiki answers these questions for a contributor or Agent that has just arrived at the repository:

- What are the major conceptual areas of this project?
- For each concept, what source files are involved and where does one start reading?
- How do concepts connect to one another?

## 2. Scope

`/wiki init` covers:

- A zero-argument slash command.
- A single Agent invocation per `/wiki init` call.
- Per-module Markdown files under `<cwd>/wiki/modules/<name>.md`.
- A YAML frontmatter on each module file that records the module's name, last-regenerated source HEAD, prompt version, and the source paths the module covers.

`/wiki init` does not cover:

- A central index file (no `wiki.yml`, no `llms.txt` for now). The wiki is fully de-centralized: the file system is the index.
- A re-extraction or refresh command. The wiki is established once per project; later edits to source files are handled by separate mechanisms not defined here.
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

Every module file at `<cwd>/wiki/modules/<name>.md` has this structure:

```markdown
---
name: <kebab-case-name>
last_sha: null
prompt_version: 1
covers:
  - <repo-relative-path-1>
  - <repo-relative-path-2>
  - ...
---

# <Display Name>

> <One-line purpose>

<body>
```

### 4.1 Frontmatter

The frontmatter is the file's metadata. Four fields:

- `name`: kebab-case identifier, 1-5 words, unique across all modules. Used as the module's stable ID in cross-module references.
- `last_sha`: source HEAD at the time the page was last regenerated. `null` when init first writes the file.
- `prompt_version`: prompt contract version applied to the page. Set to `1` for init.
- `covers`: list of repo-relative paths the module covers. Test files belong to the module whose non-test code is alongside them. Every source file in the repo must appear in exactly one module's `covers:`.

The frontmatter is owned by the runtime, not by the Agent. The Agent writes the initial values; later regeneration steps (not defined here) update `last_sha` and may update `covers` when the Agent discovers a new file the module depends on.

### 4.2 Body

The H1 (`# <Display Name>`) and the one-line blockquote (`> <purpose>`) are required. The blockquote should state what the module is responsible for, in one sentence.

Everything below the blockquote is the Agent's call. The Agent decides whether the module warrants:

- A state table.
- A flow description.
- Entry points and their source anchors.
- A list of related modules with links.
- Free-form paragraphs.

The body is the documentation; the frontmatter is the contract.

### 4.3 Cross-module references

When one module's body refers to another module, the link uses the other module's `name` as written in its frontmatter:

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

For each module, write `<REPO_ROOT>/wiki/modules/<name>.md`.

Required file structure:

  ---
  name: <kebab-case-name>
  last_sha: null
  prompt_version: 1
  covers:
    - <path>
    ...
  ---

  # <Display Name>

  > <One-line purpose>

  <body>

  Required:
    - name: kebab-case, 1-5 words, unique
    - covers: real repo paths; every source file in the repo
      appears in exactly one module's covers

  The frontmatter fields, H1, and the one-line blockquote are
  required. The body is yours.

## When done

Reply with a one-paragraph summary of the modules you created.
```

The runtime does not require a framed result for init. The Agent's chat reply is the only feedback the runtime uses to decide whether init succeeded.

## 6. Validation

The runtime validates the Agent's output by inspecting the file system after the Agent's response:

- The directory `<cwd>/wiki/modules/` exists and contains at least one `.md` file.
- Every `.md` file in `wiki/modules/` has parseable YAML frontmatter with the four required fields.
- The `name` field of each file is kebab-case and unique across the set.
- Every path listed in any `covers:` array exists in the repository.
- The union of all `covers:` arrays covers every tracked source file in the repository. (Files excluded by `.gitignore` or by the Agent's own filtering — such as build artifacts or vendored dependencies — are not counted as uncovered.)

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
├── cmd.go          slash command registration, zero-argument validation, ChatSession handoff
├── init.go         the init prompt, Agent submission, post-write validation
├── frontmatter.go  read and write the YAML frontmatter block
└── validate.go     frontmatter schema, file existence, coverage check
```

The package does not contain an Agent provider abstraction, a one-shot runner, a Job registry, a pending queue, a batched Apply loop, or stubs.

## 9. Verification contract

Tests cover:

- `/wiki init` rejects every argument, including `-a` and `--agent`.
- Missing CWD and missing selected Agent fail before filesystem writes.
- `LookupSelectedAgentSession` is used so detached sessions resume.
- A nested ChatSession CWD resolves to the Git worktree root.
- Source-clean rejects non-Wiki changes and permits resumable generated changes.
- Dirty and non-Git repositories are rejected.
- An existing `wiki/modules/` directory causes init to refuse.
- Frontmatter is parseable; missing fields are reported.
- `name` kebab-case and uniqueness are enforced.
- `covers:` paths are validated against the filesystem.
- Coverage is computed against `git ls-files`, not against the working tree.
- No `wiki.yml` or `llms.txt` is written by init.

Repository verification follows `AGENTS.md`: formatting, tests, lint, and build must pass.

## 10. References

- [AGENTS.md](https://agents.md) — repository instructions for coding Agents.
- [YAML 1.2 frontmatter convention](https://jekyllrb.com/docs/front-matter/) — precedent for the per-file metadata block.
- [Aider Repository Map](https://aider.chat/docs/repomap.html) — token-budgeted source-derived repository maps.
