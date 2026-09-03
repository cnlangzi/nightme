// Package gtw implements the nightme team-workflow command family
// (F-45: `/gtw fix <id>` and the underlying state machine).
//
// Scope (v1):
//
//	/gtw fix <issue-id>     claim an issue, create a worktree, label it
//
// Design constraints:
//
//   - State lives at <worktree>/.nightme/gtw.yml — the cwd-scoped
//     on-disk snapshot, removed with the worktree on close. There
//     is no in-memory "active fix" copy: the yml is the source of
//     truth for everything /gtw does. Provider-side labels
//     (GitHub / GitLab `nightme/wip`) are an additional state
//     surface for cross-process visibility.
//   - Zero new OutboundKind: all output is plain text (the caller wraps
//     it into whatever OutboundKind the channel wants).
//   - Credentials are borrowed from `gh auth token` / `glab auth status`.
//     nightme never persists its own tokens.
//
// Provider abstraction (GitProvider):
//
//   - GitProvider is the /gtw-facing interface to a git hosting
//     platform's issue tracker. Production has two implementations:
//     GitHubProvider (wraps `gh` CLI) and GitLabProvider (wraps
//     `glab` CLI). Future hosts (Gitea, Bitbucket) plug in by
//     satisfying the same interface — no caller change needed.
//   - Detection is two-stage (see Detect in provider.go): URL hint
//     first (zero network), API endpoint probe fallback for
//     self-hosted GitHub Enterprise / GitLab on custom domains.
//     Probed via HTTPProber (mirrors CLIRunner / GitRunner injection
//     pattern); nil → ExecHTTPProber{} with 3s default timeout.
//
// All public surfaces are safe to use from a single goroutine.
// The gtw package is stateless beyond the per-process Manager
// (which holds a per-chat run lock — see internal/command/gtw/
// manager.go doc). No global maps, no package-level mutable
// state outside Manager.
//
// The gtw package is gateway-agnostic on purpose: it does not import
// internal/gateway. The runtime wraps the IM channel into a messages.Emitter
// (a single function value), which keeps the dependency graph a
// tree (gtw → chatsession; gateway → gtw → chatsession; no cycles).
package gtw
