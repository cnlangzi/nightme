# F-CODE-REVIEW-ADAPTER — `/code-review` slash command across bridges

> **Status:** Research report (no implementation yet).
> **Branch:** `feat-review` (no `internal/command/review/` package exists today).
> **Goal:** Design an adapter that, given a code-review slash command, calls each
> agent's native review primitive when it has one and falls back to a curated
> prompt template when it doesn't.

This document is the input to that design. It surveys the seven candidates the
project cares about (Claude Code, Codex CLI, OpenCode, Pi, ACP, Gemini CLI,
GitHub Copilot CLI / Aider / Cursor / Cody), records what each one natively
exposes for "review my diff", and proposes a unified UX flag surface plus a
fallback prompt that the adapter can use when the active agent has nothing.

> **Source caveat.** The harness for this task had no working web search
> (`DEEPSEEK_API_KEY` missing for the `web-search-deepseek` provider), so this
> report is compiled from training-time knowledge of public docs and source
> repos. Every claim that needs verification is flagged with `⟂ verify ⟂`
> and an upstream URL the implementer should sanity-check against the live
> page before shipping.

---

## 1. Summary table

| Agent | Native review primitive? | What it looks like | Adapter strategy |
| --- | --- | --- | --- |
| **Claude Code** | ✅ Built-in `/review` slash command | Runs a review pass over the staged/unstaged diff using a dedicated prompt, with optional `--target <branch>` / `--comments` flags | Call the native `/review` directly via stdin if the bridge advertises it; otherwise prompt-based fallback. |
| **OpenAI Codex CLI** | ⚠️ No slash command, but has a `codex review` subcommand family (CLI subcommand, not in-session slash) | `codex review --base <branch>` or `codex review --commit <sha>` | Invoke the CLI subcommand via the bridge's `RunOnce` path; not a slash input. |
| **OpenCode** | ❌ No native review command | — | Prompt-based fallback. The bridge already advertises an `availableCommands` list — gate the decision on that list. |
| **Pi** (`@earendil-works/pi-coding-agent`) | ❌ No native review command | — | Prompt-based fallback. |
| **ACP** (Agent Client Protocol) | ❌ No review primitive on the wire protocol itself | The protocol exposes `session/prompt`, `session/cancel`, `session/load`, etc., but no dedicated `session/review`. | ACP is a transport, not an agent — the review is delegated to whatever ACP backend is mounted (Zed, JetBrains, …). Use prompt-based fallback against the current session. |
| **Gemini CLI** | ❌ No `/review` slash command | Gemini CLI's built-in slash commands are `/about`, `/auth`, `/bug`, `/clear`, `/compress`, `/copy`, `/docs`, `/editor`, `/exit`, `/extensions`, `/help`, `/mcp`, `/memory`, `/quit`, `/stats`, `/tools`, `/theme`, `/settings`, `/vim`, `/init`. | Prompt-based fallback. |
| **GitHub Copilot CLI** | ❌ No `/review`; reviews happen on github.com via the Copilot code-review bot | — | Prompt-based fallback (CLI side). |
| **Aider** | ❌ No review primitive; "review" is a meta-task done by re-prompting the model | — | Prompt-based fallback. |
| **Cursor** | ✅ Review feature in the IDE (Composer → "Review"), but **not** in `cursor-agent` CLI's slash namespace | — | Prompt-based fallback for CLI; note for IDE users. |
| **Cody** (Sourcegraph) | ❌ No `/review`; "Review code" is a UI action in the editor | — | Prompt-based fallback. |

**Bottom line:** Of the in-session agents the project bridges (claudecode /
codex / opencode / pi / dsh / acp / pty), **only Claude Code has a true
in-session review slash command**. Codex has an equivalent but it lives at the
CLI level (`codex review`), not inside an interactive session. Everything else
needs a well-designed prompt.

---

## 2. Per-agent detail

### 2.1 Claude Code (Anthropic)

**Slash command:** `/review` (interactive session) or the `--review` style
flag set when invoked non-interactively. Sources: Anthropic's Claude Code
documentation and the `claude-code` source.

**What it does internally:**
1. Claude Code resolves the current git state — base branch is auto-detected
   from the upstream tracking branch (typically `main`), with overrides
   available via `--target <ref>` / `-t <ref>`.
2. It runs a dedicated review prompt that is **not the same as the main
   coding prompt**; it asks the model to inspect the diff, comment on each
   change, and emit findings. The review prompt is shipped inside the Claude
   Code binary (closed source); we can't read the exact wording, but the
   community has reproduced equivalent prompts (see §4).
3. The output is delivered as an assistant turn with a structured layout —
   typically one paragraph of overall verdict followed by per-file /
   per-hunk findings. Claude Code's review pass does **not** spawn a
   sub-agent by default — it uses the same primary agent with a different
   system prompt. (⟂ verify ⟂ — earlier public discussion of `Task` tool
   usage; as of late 2025 the `/review` path is single-agent.)
4. The result is shown in the TUI; it does not post to GitHub automatically.
   To post a PR comment you'd use `gh pr review` or a separate integration.

**Flags** (as of Claude Code ≥ 1.0):
- `/review` — review uncommitted + un-staged changes against the current branch's upstream.
- `/review <branch>` or `/review --target <branch>` — review changes against `<branch>` (⟂ verify exact CLI form ⟂).
- `/review --base <branch>` — alternate spelling (⟂ verify ⟂).
- `/review --comments` — emit inline-style file:line comments rather than a prose report (⟂ verify ⟂).

**Output categories** the community reports from `/review` runs:
- Correctness bugs
- Security / secret leaks
- Performance regressions
- Style / idiomatic violations
- Missing tests
- API contract / breaking-change risks

There is **no formal severity ladder** in the default output — Claude Code
free-proses findings, which is one reason a fallback prompt with explicit
severity buckets (§4) is useful when you want to feed findings back into a
tooling pipeline.

**Bridge implication for nightme:** `internal/bridge/claudecode/stream.go`
already handles writing raw slash input to `claude`'s stdin. Sending
`/review\n` from `/code-review` will go straight through. We should *not*
do that blindly, though — the runtime's own slash-command registry must
continue to claim `/code-review` (it lives in nightme, not in claude),
otherwise the runtime will fall through to claude with the literal text
"/code-review" and claude will treat it as an unknown slash. The right
shape is: nightme's `/code-review` handler detects the active bridge;
if it's `claudecode`, it writes `/review` (plus any flags we want to pass)
to the stdin pipe of the running claude session.

> **Citations (verify before shipping):**
> - Claude Code docs landing: <https://docs.claude.com/en/docs/claude-code>
> - `/review` announcement / changelog: <https://docs.claude.com/en/docs/claude-code/changelog>
> - Anthropic blog on Claude Code review: <https://www.anthropic.com/news/claude-code>

---

### 2.2 OpenAI Codex CLI

**Slash command:** None. Codex CLI's interactive slash commands are `/model`,
`/approvals`, `/reasoning`, `/new`, `/quit`, `/help`, etc. — no `/review`.

**Codex does, however, expose review as a CLI subcommand** (not a slash
input). The published subcommands include (⟂ verify against current CLI
help output ⟂):
- `codex review --base <branch>` — review working-tree changes vs `<branch>`.
- `codex review --commit <sha>` — review the diff of a single commit.
- `codex review --title <text>` — pre-fill the PR title (used together with
  `gh pr create` style flows).

Internally, `codex review` reuses the same agent loop but with a
review-tuned prompt. The output is a markdown report with optional
inline comments in `file:line: <comment>` format that can be piped into
`gh pr review --comment-file` for GitHub posting.

**Bridge implication for nightme:** The `codex` bridge is built around
`codex app-server` JSON-RPC. The interactive session does not have a
review RPC; we can't trigger `/review` from inside a session. The
adapter should spawn a one-shot `codex review --base <base>` as a
separate `RunOnce` call and stream its output back to the chat as a
slash-command reply.

> **Citations:**
> - Codex CLI source: <https://github.com/openai/codex>
> - Codex CLI subcommand reference: <https://github.com/openai/codex/blob/main/docs/cli.md>
> - OpenAI Cookbook examples: <https://cookbook.openai.com/>

---

### 2.3 OpenCode (`@opencode-ai/opencode`)

**Slash command:** None with the name `/review`. OpenCode's interactive
slash commands include `/help`, `/commands`, `/models`, `/agents`,
`/sessions`, `/share`, `/new`, `/init`, `/compact`, etc. (⟂ verify
against current version ⟂).

**Bridge implication for nightme:** The `opencode` bridge already has
`AvailableBuiltinCommands()` and `IsBuiltinCommand()` in
`internal/bridge/opencode/translate.go:312-343`. We can poll those and
confirm "review" is not in the list at runtime, then fall through to
the prompt-based path. There's no fast path — OpenCode goes through
the fallback.

> **Citations:**
> - OpenCode source: <https://github.com/sst/opencode>
> - OpenCode docs: <https://opencode.ai/docs/>

---

### 2.4 Pi (`@earendil-works/pi-coding-agent`)

**Slash command:** No native `/review`. Pi's interactive command surface
includes things like `/save`, `/load`, `/tree`, `/undo`, `/exit`, etc.

**Tool agent:** Pi does have a tool-calling agent harness (model-driven
tool use) that *could* be re-purposed for review, but there is no
out-of-the-box "review this diff" entry point. The community pattern
for Pi + review is: send a "review the diff" prompt as a regular
message; the agent uses its file/shell tools to read the diff and
emit a verdict.

**Bridge implication for nightme:** Pi is wired via JSON-RPC in
`internal/bridge/pi`. We send plain text prompts to it; the
fallback review prompt (§4) is the right answer.

> **Citations:**
> - Pi source: <https://github.com/earendil-works/pi-coding-agent>

---

### 2.5 ACP (Agent Client Protocol)

**Protocol-level primitive:** None. ACP defines `session/new`,
`session/prompt`, `session/cancel`, `session/load`, `session/*` for
status / history, plus file-system / terminal capabilities. There is
no `session/review` message type as of ACP 0.x (⟂ verify against
the current schema ⟂).

**Backend-specific behavior:** Some ACP backends (Zed's agent,
JetBrains' "Junie", …) implement review as either a UI action or a
"special prompt" but **the protocol does not standardize it**, so the
adapter cannot rely on it.

**Bridge implication for nightme:** ACP is a transport; the bridge
treats it like a generic LLM. Always go through the fallback prompt.

> **Citations:**
> - ACP spec: <https://agentclientprotocol.com/>
> - ACP repo: <https://github.com/zed-industries/agent-client-protocol>

---

### 2.6 Gemini CLI

**Slash command:** No `/review`. Gemini CLI's published slash commands
include `/about`, `/auth`, `/bug`, `/clear`, `/compress`, `/copy`,
`/docs`, `/editor`, `/exit`, `/extensions`, `/help`, `/mcp`, `/memory`,
`/quit`, `/settings`, `/stats`, `/theme`, `/tools`, `/vim`, `/init`.

**Bridge implication for nightme:** Gemini CLI is not currently
bridged by nightme. If it is added later, the adapter should use the
fallback prompt path.

> **Citations:**
> - Gemini CLI docs: <https://github.com/google-gemini/gemini-cli/blob/main/docs/index.md>

---

### 2.7 GitHub Copilot CLI / Aider / Cursor / Cody

- **GitHub Copilot CLI:** No `/review`. Reviews happen on github.com via
  the Copilot code-review bot (`@copilot pull request review`) or in
  the IDE; the CLI exposes prompts only. <https://github.com/features/copilot/cli>
- **Aider:** No review primitive; "review" is a meta-task you do by
  re-prompting the model. <https://aider.chat/docs/>
- **Cursor (IDE):** Has a "Review" feature in the Composer pane
  (`⌘⇧R` / "Composer → Review") which is essentially a structured
  diff-review prompt with a fixed output schema. Not exposed via the
  `cursor-agent` CLI. <https://docs.cursor.com/>
- **Cody (Sourcegraph):** Has a "Review code" command in the IDE;
  not exposed as a slash in Cody CLI. <https://sourcegraph.com/docs/cody>

**Bridge implication:** None of these are bridged by nightme today,
but the adapter's fallback prompt (§4) is what they'd get if they
were.

---

## 3. Are there `Agent` / `Task` tools specifically for review?

**Claude Code:** Has a `Task` tool (sub-agent launcher) and a
`general-purpose` sub-agent. The `/review` slash command does **not**
launch a sub-agent by default — it runs in the main context. You
*can* manually call the `Task` tool to spawn a `general-purpose`
reviewer sub-agent, but that's a user-driven power-user pattern, not
what `/review` does.

**Codex CLI:** No "review agent" tool. `codex review` is a separate
binary path, not a sub-agent invocation.

**Pi:** Has a tool-calling harness; no named "review" tool. You can
theoretically ask Pi to spawn a "review-only" agent in its YAML
config, but the upstream Pi repo doesn't ship one.

**OpenCode / ACP / Gemini / Copilot / Aider / Cursor / Cody:**
No review-specific agent tools.

**Practical consequence for nightme's adapter:** Don't try to be
clever about sub-agents — for every agent other than Claude Code,
the review will live in the main conversation context with a
review-tuned system prompt. For Claude Code, prefer the native
`/review` and let claude manage its own sub-agents internally.

---

## 4. Recommended fallback prompt template

This is the prompt the adapter should send to the agent when no native
review slash command exists. It's a synthesis of:

- Anthropic's published guidance for "code review" prompts (their
  docs recommend a role + scope + structured output).
- CodeRabbit's documented review prompt style (severity buckets,
  file:line refs, "no findings is a valid answer").
- The prompt patterns nightme already uses in `internal/command/gtw/commit.go`
  (role → tool floor → four dimensions → anti-patterns → self-check) and
  `internal/command/gtw/pr.go` (mandatory tool inspection, rubric, anti-modal-pattern).
- Conventional wisdom from the broader LLM-review literature
  ("Don't paraphrase the diff", "Group by file", "Severity, not vibes").

```
You are a staff-engineer reviewer. Review the diff below and produce a
single review report. Be terse, specific, and actionable. No preamble,
no "sure, here's the review", no closing pleasantries. Output ONLY the
report.

## Before you write — tool floor
You MUST inspect the diff with these commands before writing any text:

  git rev-parse --abbrev-ref HEAD        # current branch
  git log --oneline <BASE>..HEAD         # commit list on this branch
  git diff <BASE>...HEAD --stat          # per-file footprint
  git diff <BASE>...HEAD -- <path>       # full content of at least one
                                         # file you intend to call out

Substitute `<BASE>` with the base branch the caller provides (default:
the upstream tracking branch, usually `main` or `master`). If no base
is provided, run `git merge-base HEAD origin/HEAD` and use that.

Read the entire diff before composing findings. If your report is
shorter than the raw `git diff` output, you've written too little — go
back and read more of the diff.

## Scope
- Correctness — does the code do what it claims?
- Security — secrets, injection, auth, deserialization, SSRF.
- Performance — N+1, unbounded loops, missing indexes, hot-path
  allocations.
- API contract — breaking changes, signature drift, wire-format
  changes, error-handling asymmetry.
- Tests — is the new behavior actually tested? Are the failure paths
  covered?
- Style — only call out style issues that are *not* handled by an
  auto-formatter on this repo (no nitpicks).
- Docs / comments — drift between doc and code, missing WHY on a
  non-obvious decision.

## Output format (strict)

For each finding, emit exactly one bullet:

  - **[severity]** `path/to/file.ext:Lstart-Lend` — one-sentence
    problem statement. Concrete fix in the next sentence. Cite a
    specific symbol/line; do not paraphrase the diff.

Severity is one of:

  - **CRITICAL** — must fix before merge (correctness bug, secret
    leak, data loss, security).
  - **HIGH** — strong default-to-fix (perf regression, missing test
    for new behavior, broken contract).
  - **MEDIUM** — should fix or file an issue (style drift, weak
    error message, missing log).
  - **LOW** — informational (naming, doc typo, optional refactor).

Group findings by file. Order files by severity within each group
(CRITICAL first). If a file has no findings, omit it.

After the per-file bullets, add a final section:

  ## Verdict
  One of: `Approve`, `Request changes`, `Comment`. One sentence of
  rationale.

  ## Summary of intent
  - **What changed** — 1 sentence.
  - **Why** — 1 sentence.
  - **Blast radius** — files / packages / wire formats touched.

## Do NOT

- Do NOT paraphrase the diff in bullets ("updated X", "refactored Y").
  Name the file and the consequence.
- Do NOT invent findings to look thorough. An empty report is a valid
  report.
- Do NOT call out formatting that the repo's formatter would auto-fix.
- Do NOT post anything to GitHub / GitLab / Bitbucket. Report only.
- Do NOT run `git push`, `git commit`, `gh pr`, or `glab mr`. This is
  a read-only review.

## Context (filled by the adapter at call time)

  Repository: <repo URL or "resolve from `git remote get-url origin`">
  Branch (head): <branch>
  Base branch: <base>
  Working dir: <worktree-absolute-path>
  Reviewer requested by: <user-id, channel-id>
```

The adapter is responsible for substituting `<BASE>`, branch, and
worktree before sending. If the agent supports structured output,
append a fenced JSON envelope on a final line so downstream tooling
can parse findings:

```
\`\`\`review-json
{"verdict":"...", "findings":[{"severity":"...","file":"...",
 "start":1,"end":2,"problem":"...","fix":"..."}]}
\`\`\`
```

(Strip the backslash escapes in the actual prompt — they're shown
escaped here only because this document is itself Markdown.)

---

## 5. Cross-agent UX consistency

The adapter's user-visible slash command should be **`/code-review`**
(consistent with the project's existing references to `/code-review`
in `internal/command/gtw/commit_realpi_unix_test.go:394-396` and the
`gtw` namespace convention seen in `internal/command/gtw/cmd.go:236-253`).

The flag surface the adapter should expose (independent of the
underlying agent):

| Flag | Meaning | Maps to |
| --- | --- | --- |
| `--base <branch>` (alias `-b`) | Review changes against `<branch>` instead of auto-detected upstream | Claude Code `--target`, Codex `--base`, fallback `<BASE>` |
| `--staged` | Review only `git diff --staged` | fallback filter |
| `--unstaged` | Review only `git diff` (working tree) | fallback filter |
| `--commit <sha>` | Review a single commit | Codex `--commit`, fallback `git show <sha>` |
| `--agent <name>` (alias `-a`) | Override which agent runs the review (default = currently selected) | runtime `cs.LookupSelectedAgentSession` |
| `--json` | Ask the agent to emit the `review-json` envelope alongside prose | append to fallback prompt |
| `--post` | After the agent replies, post findings as a PR review comment via `gh pr review` (only on gh repos) | adapter-side orchestration |

The `gtw pr` command in `internal/command/gtw/pr.go:236` already
exposes `-a claude` for "override which agent runs the one-shot",
so `--agent` is the obvious flag name.

**Dispatch matrix the adapter should implement:**

```
nightme:/code-review
   │
   ▼
active bridge ── claudecode ───► write "/review [--target X]\n" to claude stdin
              ├ codex ────────► spawn `codex review --base X` via RunOnce
              ├ opencode ─────► no native → fallback prompt
              ├ pi ───────────► no native → fallback prompt
              ├ acp ──────────► no native → fallback prompt
              ├ dsh ──────────► no native → fallback prompt
              └ pty ──────────► no native → fallback prompt
```

The bridge can advertise "I have a native review" by exposing a
`HasNativeReview` capability alongside the existing
`AvailableBuiltinCommands()`. Two of the seven bridges do
(claudecode, codex); the rest go through the fallback.

**Output handling.** The slash command reply should preserve the
agent's raw markdown (reviewers want the prose). When `--json` is
set, the runtime should also parse the `review-json` envelope,
materialize each finding as a `ReceiptCard`, and emit one
per-finding ⏳/✅ pair so chat UX gets per-finding receipts (same
pattern the receipt FSM in `internal/agent/message_state.go` already
implements for tool calls).

---

## 6. Open questions for the implementer

1. **Where does `/code-review` live in the slash registry?** Top-level
   (`/code-review`) or under the `gtw` namespace (`/gtw review`)?
   `gtw` is the established git-workflow namespace; review is a
   git-workflow-adjacent task. Recommendation: **top-level
   `/code-review`**, because review can be run against any branch /
   any worktree, not just the one managed by `gtw`. The existing
   `gtw pr` step can call `/code-review` as an internal sub-step
   before posting.

2. **How does the runtime know the bridge has a native review?**
   Add `NativeReview bool` to the `agent.BridgeCaps` struct (or a
   new `agent.BridgeReviewCaps` struct with `Available bool`,
   `SlashName string`, `BaseFlag string`). `claudecode` and `codex`
   set `Available=true`. `internal/bridge/opencode` already has the
   `availableCommands` cache but the runtime should not depend on
   the bridge to expose review semantics — it should be explicit.

3. **Should the fallback prompt live in the codebase or be
   templated config?** Strong preference for **a single Go string
   constant** in `internal/command/review/prompt.go` so it's
   version-controlled and reviewed in-tree (mirrors the
   `buildAgentPrompt` and `buildPRPrompt` functions in
   `internal/command/gtw/`). Future iterations of the prompt go
   through normal PR review.

4. **Should the adapter refuse to run on a dirty tree?** No —
   review is most useful *because* the tree is dirty. If
   `--staged` / `--unstaged` aren't passed, default to
   `git diff <BASE>...HEAD` (the entire branch).

5. **What happens if the user invokes `/code-review` on a bridge
   that has no AgentSession yet?** Spawn one first (matching the
   `/use <agent>` semantics in `internal/command/use/cmd.go`); then
   send the review prompt. The ⏳ receipt covers the spawn latency.

---

## 7. Verification checklist before merge

- [ ] `internal/bridge/claudecode` actually accepts `/review` over
      its stdin and produces a review turn (integration test with
      the mock script in `internal/testdata/claude_print_mock.sh`).
- [ ] `codex review --base <branch>` is invokable as a one-shot and
      streams a result event back (integration test).
- [ ] `AvailableBuiltinCommands()` for opencode / pi / acp does not
      contain "review" in current upstream versions.
- [ ] Fallback prompt produces findings on a representative diff
      (snapshot the output as a golden file).
- [ ] `--json` envelope round-trips through the runtime's reply
      parser without breaking the prose path.
- [ ] Slash registry doesn't shadow native claudecode `/review`
      (i.e. `/review` alone — without `/code-` prefix — still
      reaches claude).

---

## 8. Out of scope (deliberately)

- Posting findings as PR comments (`gh pr review --comment-file`).
  Easy to add later, but lives behind `--post` and should be a
  follow-up PR.
- Cross-agent review (e.g. "claude reviews codex's output"). The
  fallback prompt works for this; we don't need a special path.
- Severity calibration against real PR corpora. The severity
  ladder in §4 is a starting point; tune after seeing real output.