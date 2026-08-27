# Reviewer strict-grounding rules (drop into `/review` prompt)

> For `dsh /review` on `/Users/geax/code/geax/github.com/cnlangzi/nightme.nightme/fix-gtw-fix`
> (gtw F-XX branch, fix-gtw-fix).

A finding is ONLY valid when all three are true:

1. **The code actually behaves this way today.** Trace the
   execution path from a real caller to the cited line.
   If you cite "function X calls function Y with arg Z", verify
   X actually exists, Y actually exists, and the call site
   actually passes Z. A single grep that doesn't match means
   the finding is wrong — fix the finding, not the code.

2. **The bug is observable from the current code path, not a
   hypothetical future one.** Do NOT raise findings like
   "if X is reintroduced, then Y would need to handle Z" —
   that's a design opinion, not a bug. Only flag behavior the
   current code produces.

3. **The fix or cleanup you suggest is observable in the
   current diff/PR.** "This dead code might confuse a future
   reader" is a style opinion; "this code path is unreachable
   today because no caller exists" is a fact.

## Forbidden patterns

- "Should be X" — design opinions, not bugs.
- "If the user does X, Y might happen" — hypotheticals.
- "This is dead code because it has no callers" — VERIFY the
  caller list first; if there IS a caller, it's not dead.
- "The constant has the wrong emoji" — only flag if the wrong
  emoji is actually rendered or compared somewhere; dead
  constants with wrong values are style issues, not bugs
  (and should be deleted instead).
- "I would clean this up" — if the current code works, it's
  not a finding.

## Required evidence per finding

Every finding must cite:

- Exact file:line for the bug site.
- Exact file:line for the caller that triggers it (if
  applicable).
- The concrete runtime behavior (e.g. "user sees '�' /
  'âĜ' / empty string", "this branch never executes", "this
  function returns X").

If you can't produce these, the finding is wrong and should
be dropped, not weakened.

## Severity calibration

- **Critical / High**: bug that produces wrong user-visible
  output, corrupts state, or breaks a documented contract.
  Must cite a real execution path that is reachable today.
- **Medium**: bug with non-trivial consequences, e.g. wrong
  emoji in a non-rendered constant that another tool could
  string-compare; or `git show-ref` reply that displays
  garbage instead of ❌.
- **Low**: nits / style / dead code without observable
  consequence / comments that lie but don't mislead runtime.

Don't promote dead-code-with-no-caller to "Medium bug" — it's
a Low-style cleanup, not a Medium bug.

## Worked examples

### Good finding (grounded)

> Severity: High
>
> Path: `internal/command/gtw/fix.go:503`
> Content: `runFixLocal` returns `fmt.Sprintf("âĜ git show-ref failed: %v", err)` to the
> user when `BranchExists(ctx, repoRoot, branch, deps.Git)` fails
> in local mode. The bytes `c3 a2 c2 9d c2 8c` are mojibake for
> ❌ (correct UTF-8 `e2 9d 8c`). The user sees `âĜ git show-ref
> failed: …` instead of `❌ git show-ref failed: …`.
>
> Comparison: `runFixRemote` at `fix.go:309` uses the correct
> bytes, so this is a one-line copy-paste slip.

### Bad finding (not grounded)

> ❌ Severity: Medium
> Path: `internal/command/gtw/render.go` (general)
> Content: "The `default` branch in the switch should be a
> `panic` so future enum drift fails loud."

This is a design opinion about how enum drift should be
handled. The current code does a silent fallback that tests
will catch; no user is impacted. Don't raise.

### Borderline (mark Low + cleanup)

> Severity: Low (style / cleanup)
> Path: `internal/command/gtw/slug.go:183-184`
> Content: Two trailing blank lines after the closing brace.
> `gofmt -d internal/command/gtw/slug.go` flags this.

This is a real observation (gofmt confirms), but it's a style
nit — mark Low.
