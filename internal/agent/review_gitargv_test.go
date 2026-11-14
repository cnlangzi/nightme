package agent

import "testing"

// TestGitArgv_NoOptionalLocksFirst pins the argv shape: the flag has
// to precede the subcommand for git to accept it, and it precedes
// `-C` so the flag is the first argument after `git`.
func TestGitArgv_NoOptionalLocksFirst(t *testing.T) {
	args := gitArgv("/ws", "diff", "--name-only")
	if len(args) != 5 {
		t.Fatalf("args = %v, want 5 elements", args)
	}
	if args[0] != "--no-optional-locks" {
		t.Errorf("args[0] = %q, want --no-optional-locks", args[0])
	}
	if args[1] != "-C" || args[2] != "/ws" {
		t.Errorf("args = %v, want -C /ws right after the flag", args)
	}
	if args[3] != "diff" || args[4] != "--name-only" {
		t.Errorf("args = %v, want the subcommand last", args)
	}
}
