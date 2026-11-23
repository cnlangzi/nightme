package me

// The Data keys the outcomes this package produces and reads carry.
//
// A key names a fact, not a category: the status says which branch
// the work took. A key with no value does not appear at all.
//
// A count is a decimal string and a boolean is "true" or "false".
const (
	// KeyAheadOfBase is how many commits the branch is ahead of its
	// base.
	KeyAheadOfBase = "ahead_of_base"
	// KeyAheadOfUpstream is how many commits the branch is ahead of
	// its tracking ref.
	KeyAheadOfUpstream = "ahead_of_upstream"
	// KeyAnswer is what the user answered.
	KeyAnswer = "answer"
	// KeyBase is the branch the work started from.
	KeyBase = "base"
	// KeyBehindBase is how many commits the branch is behind its
	// base.
	KeyBehindBase = "behind_base"
	// KeyBehindUpstream is how many commits the branch is behind its
	// tracking ref.
	KeyBehindUpstream = "behind_upstream"
	// KeyBranch is the branch the chat is on.
	KeyBranch = "branch"
	// KeyCwd is the directory the chat works in.
	KeyCwd = "cwd"
	// KeyDirty says whether the working tree carries changes.
	BoolKeyDirty = "dirty"
	// KeyExitCode is the exit code a shell command left.
	KeyExitCode = "exit_code"
	// KeyModel is the model that answered.
	KeyModel = "model"
	// KeyStderr is what a shell command wrote to its error output.
	KeyStderr = "stderr"
	// KeyStdout is what a shell command wrote to its output.
	KeyStdout = "stdout"
	// KeyUnfinished is the number of items a command left open. The
	// command that produces it declares the key in its own package;
	// the same name stands for the same fact here.
	KeyUnfinished = "unfinished"
	// KeyUpstream is the branch's tracking ref, when it has one.
	KeyUpstream = "upstream"
)
