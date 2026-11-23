package gtw

// The Data keys this package's outcomes carry.
//
// A key names a fact, not a category: the status says which branch
// the work took. A key with no value does not appear at all.
//
// A count is a decimal string, an enum is lower-case snake, and a
// boolean is "true" or "false".
const (
	// KeyAction is what /gtw assign did: added or removed.
	KeyAction = "action"
	// KeyAgent is the name of the agent that ran.
	KeyAgent = "agent"
	// KeyAttempts is how many times the agent was asked for a pull
	// request.
	KeyAttempts = "attempts"
	// KeyBaseBranch is the branch a pull request merges into.
	KeyBaseBranch = "base_branch"
	// KeyBlocked is how many candidates were skipped because a
	// dependency had not landed.
	KeyBlocked = "blocked"
	// KeyBranch is the branch name.
	KeyBranch = "branch"
	// KeyBranchExists is how many candidates were skipped because the
	// branch was already there.
	KeyBranchExists = "branch_exists"
	// KeyCandidates is how many candidate issues were looked at.
	KeyCandidates = "candidates"
	// KeyChanges is how many changes the agent left behind.
	KeyChanges = "changes"
	// KeyCommitLog is the commits, one "<sha> <subject>" per line.
	KeyCommitLog = "commit_log"
	// KeyCommits is how many commits there are.
	KeyCommits = "commits"
	// KeyConflict says whether the pull request has a conflict.
	BoolKeyConflict = "conflict"
	// KeyDeps is how many dependency relations were created.
	KeyDeps = "deps"
	// KeyDepsFailed is how many dependency relations failed.
	KeyDepsFailed = "deps_failed"
	// KeyElapsed is how long the cancelled command had run.
	KeyElapsed = "elapsed"
	// KeyFormat says why the agent's reply did not carry the format
	// a pull request needs.
	KeyFormat = "format"
	// KeyHead is the sha the synced branch points at.
	KeyHead = "head"
	// KeyHeld is how many candidates another agent was holding.
	KeyHeld = "held"
	// KeyIssue is the issue number the work was about.
	KeyIssue = "issue"
	// KeyIssueURL is the issue's link on the platform.
	KeyIssueURL = "issue_url"
	// KeyIssues is the issue numbers that were opened, comma
	// separated.
	KeyIssues = "issues"
	// KeyKept is how many drafts were not turned into issues.
	KeyKept = "kept"
	// KeyLabel is the state label that blocked the fix.
	KeyLabel = "label"
	// KeyLang is the output language.
	KeyLang = "lang"
	// KeyMergeState is the platform-neutral merge verdict: mergeable,
	// conflicting, behind, unknown or blocked.
	KeyMergeState = "merge_state"
	// KeyMode is the mode /gtw fix ran in: remote or local.
	KeyMode = "mode"
	// KeyPaths is how many paths changed.
	KeyPaths = "paths"
	// KeyPRNumber is the pull request number.
	KeyPRNumber = "pr_number"
	// KeyPROpen is how many candidates were skipped because a pull
	// request was already open.
	KeyPROpen = "pr_open"
	// KeyPRState is the state of the pull request: open, closed or
	// merged.
	KeyPRState = "pr_state"
	// KeyPRURL is the pull request's link.
	KeyPRURL = "pr_url"
	// KeyRemoteHead is the sha origin/<branch> points at.
	KeyRemoteHead = "remote_head"
	// KeyRepo is the repository, owner/repo.
	KeyRepo = "repo"
	// KeyRepoRoot is the root of the main repository.
	KeyRepoRoot = "repo_root"
	// KeyScope is the scope a setting was written in: global, repo or
	// default.
	KeyScope = "scope"
	// KeySelectedBy says how the issue was selected.
	KeySelectedBy = "selected_by"
	// KeySHARange is the commit range, "<before>..HEAD".
	KeySHARange = "sha_range"
	// KeyStaged is how many changes are in the index.
	KeyStaged = "staged"
	// KeySubcommand is the /gtw subcommand that was cancelled.
	KeySubcommand = "subcommand"
	// KeyTopic is the topic /gtw plan works on.
	KeyTopic = "topic"
	// KeyUnset says whether the setting was removed.
	BoolKeyUnset = "unset"
	// KeyUnstaged is how many changes are not in the index.
	KeyUnstaged = "unstaged"
	// KeyUntracked is how many paths git does not track.
	KeyUntracked = "untracked"
	// KeyWakeAfter is how long to wait before looking for work again.
	KeyWakeAfter = "wake_after"
	// KeyWorktree is the worktree path.
	KeyWorktree = "worktree"
	// KeyWorktreeCandidates are the worktree branches the chat could
	// have jumped to, comma separated.
	KeyWorktreeCandidates = "worktree_candidates"
)
