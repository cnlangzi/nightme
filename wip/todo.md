# Handover: fix-gtw-fix branch — mimeFromExt wiring + runtime self-containment

This document hands off the **`fix-gtw-fix`** branch to the next agent.
It supersedes the previous `wip/todo.md` (which described an unrelated
`fix-telegram-rolling-log` branch — ignore that content if you saw it).

## TL;DR for the next agent

- The big feature (download all issue attachments, route by MIME type) **landed
  in commit `6e48754` and is fully tested** — do not touch that contract.
- There are **2 uncommitted files** in the working tree (see §State). Both
  compile and pass `go test ./internal/command/gtw/`.
- One change introduces a function **`mimeFromExt` that is defined but never
  called** — it will trip a `staticcheck` `unusedfunc`/`U1000` rule on strict
  CI. **Your primary job is to wire it in** (see §Task 1).
- The other change documents a "runtime self-containment" invariant that is
  **already pinned by a passing test** — just commit it, or extend it per
  §Task 2.

## Goal of the branch

`/gtw fix <issue>` turns a GitHub issue into a self-contained agent prompt
that runs on the user's own worktree. The recent work (commits
`1ee615d` → `6e48754`) made the dispatch prompt:

1. **Plan-first** — `DispatchPlan` (default) produces a due-diligence prompt;
   `DispatchExecute` (the `-y` flag) produces a GOBL-mode edit prompt. Both
   come from one `buildIssueDispatchText` with a `mode` switch.
2. **Attachment-aware** — every `![](url)` / `[](url)` link in the issue body
   is downloaded, routed by MIME type to `ContentImage` (vision) or
   `ContentFile` (path annotation), and counted in the dispatch text so the
   agent knows attachments exist.

Reference docs:
- `docs/feat/F-gtw-fix.md` — the feature design (the §4.1/§4.2 prompts live here)
- `internal/command/gtw/fix.go:855` — `buildIssueDispatchText` (the template)
- `internal/command/gtw/dispatch_test.go` — the pinned invariants
- `internal/command/gtw/attachments.go` — the downloader + MIME router
- `internal/command/gtw/provider.go:972` — `extractGitHubAttachments`

## State

```
Branch:    fix-gtw-fix
HEAD:      6e48754 fix(gtw): download all issue attachments, route by MIME type
Working:   2 files modified, nothing staged
Build:     go build ./internal/command/gtw/   → ok (exit 0)
Tests:     go test ./internal/command/gtw/    → ok (16.3s, all pass)
```

### Uncommitted diff (the handoff payload)

**`internal/command/gtw/attachments.go`** — renamed `looksLikeImageName(name)
bool` → `mimeFromExt(name) string` and expanded the table from 7 image
extensions to ~17 MIME types:

| extension(s) | returned MIME |
|---|---|
| `.png .jpg .jpeg .gif .webp .bmp .svg` | `image/*` (→ ContentImage) |
| `.pdf` | `application/pdf` |
| `.json` | `application/json` |
| `.txt .log` | `text/plain` |
| `.xml` | `application/xml` |
| `.csv` | `text/csv` |
| `.md` | `text/markdown` |
| `.html .htm` | `text/html` |
| `.zip` | `application/zip` |
| `.gz .tgz` | `application/gzip` |
| `.tar` | `application/x-tar` |
| *(default)* | `application/octet-stream` (→ ContentFile) |

**`docs/feat/F-gtw-fix.md`** — added a blockquote "运行时自包含原则"
(Runtime self-containment principle) above the §4.1 Plan Prompt. It states
that `buildIssueDispatchText`'s §Task body runs in a standalone agent on the
user's worktree that **cannot see this repo's docs**, so the runtime text must
not leak internal section numbers (`§4`), doc filenames (`F-gtw-fix`,
`REVIEWER_INSTRUCTIONS`), or cross-mode references (`the plan above`,
`Execute (§`). The invariant is guarded by
`TestBuildIssueDispatchText_RuntimeSelfContained` (dispatch_test.go:192),
**which already exists and passes**.

## Tasks for the next agent

### Task 1 (BLOCKING) — wire `mimeFromExt` into the extractors

**This is the primary handoff item.** `mimeFromExt` is currently **dead
code**: defined at `attachments.go:173` with a doc comment claiming "the
attachment extractors use it", but no extractor calls it. A strict
`staticcheck` run (`unusedfunc` / U1000) will fail CI on this function.

The old `looksLikeImageName` was *also* dead at the time of the rename (it
was introduced in commit `6e48754`'s attachments rewrite but never invoked),
so this gap predates the WIP — the rename just made it more capable and more
obviously orphaned.

**Where to wire it:** `internal/command/gtw/provider.go:972`
`extractGitHubAttachments`. Today it:

- only matches `![alt](url)` (image syntax, the `!` prefix) — see the
  `body[i] != '!' || body[i+1] != '['` guard at provider.go:982;
- hardcodes `MIMEType: "image/png"` for every match (provider.go:1024),
  with a comment "best guess; downloadAttachments refines from HTTP
  response".

The intended behaviour (per `mimeFromExt`'s doc comment) is:

1. Replace the hardcoded `"image/png"` with `mimeFromExt(fn)` so the
   pre-classification is accurate **before** the HTTP response refines it.
   The `downloadAttachments` MIME-refinement priority (attachments.go:123:
   HTTP `Content-Type` wins over `att.MIMEType` hint) still holds —
   `mimeFromExt` only seeds the hint.
2. *(Optional, larger scope)* Extend the extractor to also match plain
   `[](url)` links (no `!` prefix) so a log/dump/PDF referenced as a plain
   link becomes an attachment too. Today only `![](url)` images are picked
   up; a `[](crash.log)` link is invisible to the dispatch. If you do this,
   the `!` guard becomes a "is-image-link" heuristic and `mimeFromExt`
   decides the block type.

**Symmetry check:** there appears to be only one extractor
(`extractGitHubAttachments`). GitLab uploads (`/uploads/...`) are inlined as
`![](url)` per provider.go:48 comment and likely reuse the same extractor —
confirm by grepping `func extract.*ttachment` before assuming a second
extractor exists.

**Test expectations:** `attachments_test.go` (line 41) feeds a fixture with
`MIMEType: "image/png"` directly, so it won't break from the wiring change.
But `dispatch_test.go::TestBuildIssueDispatchText_AttachmentsSection` (line
301) asserts the attachment-count text — verify it still passes after
wiring (it should: the count is taken from `imageCount`/`fileCount` params,
not from `len(issue.Attachments)`, per commit `6e48754`).

**Definition of done:** `mimeFromExt` has ≥1 non-test caller, `staticcheck
./internal/command/gtw/` is clean, and `go test ./internal/command/gtw/`
still passes.

### Task 2 (non-blocking) — commit the runtime self-containment invariant

The `F-gtw-fix.md` blockquote + the `TestBuildIssueDispatchText_RuntimeSelfContained`
test are a matched pair that already passes. You can commit them as-is in a
single `docs(gtw): pin runtime self-containment invariant` commit.

Optional hardening: the test's leak-list (`§4`, `F-gtw-fix`,
`REVIEWER_INSTRUCTIONS`, `Execute (§`, `the plan above`) is a denylist. If
you want belt-and-suspenders, also assert that the prompt contains NO
backtick-quoted doc filename at all (regex-scan for `` `[^`]+\.md` `` and
fail if any matches a file in `docs/` or the repo root). This is optional —
the denylist already covers the known leak vectors.

### Task 3 (housekeeping) — decide the WIP commit shape

Two reasonable shapes; pick one:

- **One commit** — `refactor(gtw): mimeFromExt pre-classifies attachments by
  extension` covering both files (the `attachments.go` rename + the doc
  principle). They're conceptually related (both make the dispatch prompt
  more self-contained / accurate before download). Then do Task 1's wiring
  in a follow-up commit on top.
- **Two commits** — `docs(gtw): runtime self-containment invariant` (just
  the .md) then `refactor(gtw): mimeFromExt replaces looksLikeImageName`
  (just attachments.go). Cleaner history; the .md is already
  self-consistent (its test exists and passes), so it can land first.

Either way, **do not commit `mimeFromExt` without either wiring it in or
adding a caller** — a green local `go test` hides the dead-code problem
because tests don't run `staticcheck`.

## Lessons learned (don't repeat these)

1. **`staticcheck` is not part of `go test`.** A function can compile, have
   a thorough doc comment, and pass all tests while being completely
   uncalled. The WIP's `mimeFromExt` is exactly this case — it *looks*
   wired-in (the comment says "the extractors use it") but isn't. Always
   cross-check with `grep -rn <fn> --include=*.go` for non-test callers
   before considering a refactor done.

2. **Aspirational doc comments are a trap.** The `mimeFromExt` comment
   describes the *intended* contract, not the current state. When you read
   "the extractors use it", verify the call site exists — if it doesn't,
   that's the work item, not a finished description.

3. **The `looksLikeImageName` → `mimeFromExt` rename was the right call.**
   The old bool function could only answer "is this an image?"; the dispatch
   text needs the actual MIME type to split image vs file counts
   accurately *before* download (when only the filename is known). A
   `string`-returning function is the correct shape — just finish wiring it.

4. **The runtime self-containment principle is load-bearing.** The dispatch
   prompt runs on the user's worktree, not in this repo. Any internal
   reference (§4.1, F-gtw-fix.md, "the plan above") is a broken pointer for
   the runtime agent. The `TestBuildIssueDispatchText_RuntimeSelfContained`
   denylist is the guardrail — keep it green and extend it when you add new
   cross-references to the prompts.

## Key files to know

| File | Lines | Purpose |
|---|---|---|
| `internal/command/gtw/fix.go` | ~855+ | `buildIssueDispatchText` — the prompt template (NO HTML, just markdown) |
| `internal/command/gtw/attachments.go` | ~295 | `downloadAttachments`, `isImageMIME`, **`mimeFromExt` (dead, needs wiring)** |
| `internal/command/gtw/provider.go` | ~972+ | `extractGitHubAttachments` — the `![](url)` parser (hardcodes `image/png`) |
| `internal/command/gtw/dispatch_test.go` | ~340 | pinned invariants incl. `RuntimeSelfContained`, `AttachmentsSection` |
| `internal/command/gtw/attachments_test.go` | ~41+ | download/routing fixtures |
| `docs/feat/F-gtw-fix.md` | §4.1/§4.2 | the Plan/Execute prompt specs + runtime self-containment principle |

## DO NOT regress (locked in by `6e48754` + tests)

- **Every attachment downloads.** No skip-on-type. A 302-to-HTML-login still
  becomes a `ContentFile` so the agent sees what arrived. Pinned by
  `attachments_test.go`.
- **10MB size guard** (`maxAttachmentBytes`) skips pathological non-images
  *without writing to disk*; the dispatch text still surfaces the URL.
- **Index-prefixed filenames** (`<i>-<name>`) so same-named attachments don't
  clobber. Pinned by the download fixtures.
- **Dispatch counts come from `imageCount`/`fileCount` params**, not
  `len(issue.Attachments)`. Pinned by
  `TestBuildIssueDispatchText_AttachmentsSection`.
- **HTTP `Content-Type` wins over the provider's `MIMEType` hint**
  (attachments.go:123). `mimeFromExt` only seeds the hint; it must NOT
  override the response.
