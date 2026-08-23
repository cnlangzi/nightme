// Package gtw implements the nightme team-workflow command family
// (F-45: `/gtw fix <id>` and the underlying state machine).
//
// Scope (v1):
//
//	/gtw fix <issue-id>     claim an issue, create a worktree, label it
//
// Design constraints:
//
//   - Zero new per-repo / per-user files: state lives in ChatSession memory
//     (gtwContext / gtwDrafts) and on the provider (GitHub / GitLab labels).
//   - Zero new OutboundKind: all output is plain text (the caller wraps
//     it into whatever OutboundKind the channel wants).
//   - The reaction-routing entry point is one extra branch in
//     ChatSession.HandleAction (gtwDrafts checked before the F-31 FSM).
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
// All public surfaces are safe to use from a single goroutine. The
// ChatSession-owned fields (gtwContext / gtwDrafts) are guarded by
// ChatSession.mu; the gtw package itself is stateless.
//
// The gtw package is gateway-agnostic on purpose: it does not import
// internal/gateway. The runtime wraps the IM channel into a messages.Emitter
// (a single function value), which keeps the dependency graph a
// tree (gtw → chatsession; gateway → gtw → chatsession; no cycles).
package gtw
