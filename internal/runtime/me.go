// me.go — the helpers the personal agent needs from the runtime.
//
// The personal agent works inside the chat the run started in. It
// owns the workflow catalogue, the action registry and the per-run
// state; the channel owns the transport.
//
// The agent stops before the channels do: the runs that are still
// going are cancelled and given a moment to write their final state
// while their chat is still up.

package runtime

import (
	"errors"
	"path/filepath"

	"github.com/cnlangzi/nightme/internal/channel"
	"github.com/cnlangzi/nightme/internal/config"
)

// workflowsDir returns <DataDir>/workflows — the directory the
// workflow YAML files live in.
func workflowsDir(cfg *config.Config) (string, error) {
	if cfg == nil || cfg.Paths.DataDir == "" {
		return "", errors.New("cfg.Paths.DataDir is empty")
	}
	base, err := filepath.Abs(cfg.Paths.DataDir)
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "workflows"), nil
}

// countIMChannels counts the channels a person can talk to.
func countIMChannels(chs []channel.Channel) int {
	n := 0
	for _, ch := range chs {
		if ch == nil {
			continue
		}
		n++
	}
	return n
}
