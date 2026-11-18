package runtime

import (
	"testing"

	"github.com/cnlangzi/nightme/internal/command"
)

// TestTheWfCommandIsRegistered — the daemon imports the /wf command
// package, so the command is in the registry the runtime dispatches.
func TestTheWfCommandIsRegistered(t *testing.T) {
	command.SetDeps(command.Deps{})
	found := false
	for _, spec := range command.Default().Specs() {
		if spec.Name == "wf" {
			found = true
		}
	}
	if !found {
		t.Errorf("the /wf command is not registered: %v", command.Default().Specs())
	}
}
