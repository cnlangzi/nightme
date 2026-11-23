package cwd

// The Data keys this package's outcomes carry.
//
// A key names a fact, not a category: the status says which branch
// the work took. A key with no value does not appear at all.
const (
	// KeyCwd is the workspace path the chat works in.
	KeyCwd = "cwd"
)
