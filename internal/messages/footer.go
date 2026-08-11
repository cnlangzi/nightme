package messages

// Footer payload types — promoted from internal/command/gtw into
// the messages package so the SessionContext struct can stay in
// this package without creating an import cycle (chatsession →
// outbound → channel → messages → gtw → chatsession would loop
// through the gtw package; moving the leaf value types here keeps
// the dependency graph acyclic).

// GitStatusSnapshot is the parsed result of a single
// `git status --porcelain --branch` invocation against a workspace.
// Pure value type (no methods) so it can be carried across package
// boundaries (SessionContext, runtime stamping) and tested without
// running git.
//
// Field semantics:
//
//	Branch         — current branch name. Empty when the workspace
//	                 is in detached HEAD state ("## HEAD (no branch)"
//	                 or "## (HEAD detached at <sha>)"). The Feishu
//	                 footer renders this as "⎇ ?" so users see
//	                 "branch unknown" without the underlying reason.
//	Uncommitted    — count of porcelain entries that are NOT
//	                 untracked or ignored. Includes modified (M),
//	                 added (A), deleted (D), renamed (R), copied
//	                 (C), and unmerged conflict entries (UU, AA,
//	                 etc.). Excludes "!!" ignored lines.
//	Untracked      — count of "??" entries (files not in the index).
//	AheadOfRemote  — number of local commits the upstream is
//	                 behind. Always 0 when HasUpstream is false.
//	HasUpstream    — true when the branch has an upstream tracking
//	                 ref ("## main...origin/main"). Detached HEAD
//	                 never has upstream; the Feishu footer omits
//	                 the "⇡ N" segment in that case.
//
// F-48 (follow-up to F-45): runtime stamps one of these on every
// OutboundMessage.SessionContext that flows to a main-chat footer
// render site. See docs/feat/F-45-session-footer.md §1.7.
type GitStatusSnapshot struct {
	Branch        string
	Uncommitted   int
	Untracked     int
	AheadOfRemote int
	HasUpstream   bool
}

// PR is the abstract cross-platform handle for a single Pull
// Request / Merge Request. Production has two impls (GitHub /
// GitLab) and the runtime caches one PR per AgentSession via
// prcache.Registry.
//
// Field semantics:
//
//	Number — platform-native PR number (GitHub: #N; GitLab: !N).
//	URL    — web URL to the PR / MR page.
//	State  — "open" / "merged" / "closed". v1 only requests
//	         PRs in the "open" state (GitHub's `pr list --state open`,
//	         GitLab's `mr list --state opened`); the field stays in
//	         the type so a future "show merged PR too" footer variant
//	         can flip the platform filter without changing the
//	         consumer.
type PR struct {
	Number int
	URL    string
	State  string
}