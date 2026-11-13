// me.go — the helpers the personal agent needs from the runtime.
//
// The personal agent drives the daemon through the loopback channel,
// the way a person drives it through Feishu. It owns the workflow
// catalogue, the action registry and the per-run state; the loopback
// channel owns the transport.
//
// The agent starts once the loopback channel is attached — its
// commands need a chat to land in — and stops before the channels
// do: the runs that are still going are cancelled and given a
// moment to write their final state while their chat is still up.

package runtime

import (
	"errors"
	"path/filepath"

	"github.com/cnlangzi/nightme/internal/channel"
	"github.com/cnlangzi/nightme/internal/channel/loopback"
	"github.com/cnlangzi/nightme/internal/config"
)

// findLoopback returns the loopback channel among the started
// channels, or nil when there is none.
func findLoopback(chs []channel.Channel) *loopback.Channel {
	for _, ch := range chs {
		if lb, ok := ch.(*loopback.Channel); ok {
			return lb
		}
	}
	return nil
}

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

// countIMChannels counts the channels a person can talk to. The
// loopback channel is the daemon's own chat: it is always there, so
// it must not hide the fact that no IM channel is configured.
func countIMChannels(chs []channel.Channel) int {
	n := 0
	for _, ch := range chs {
		if ch == nil {
			continue
		}
		if _, ok := ch.(*loopback.Channel); ok {
			continue
		}
		n++
	}
	return n
}
