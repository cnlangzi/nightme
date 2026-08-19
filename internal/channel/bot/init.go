package bot

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/cnlangzi/nightme/internal/channel"
	"github.com/cnlangzi/nightme/internal/command/gtw"
	"github.com/cnlangzi/nightme/internal/config"
	"github.com/cnlangzi/nightme/internal/wfe"
)

// init registers the bot channel with the channel registry so
// channel.BuildAll picks it up automatically (v1.3+ multi-channel).
//
// bot is enabled when the workflows dir exists (or can be created)
// under the user's nightme data dir. If not, the builder returns
// an error and BuildAll silently skips bot — no user-facing error.
func init() {
	channel.Register("bot", botBuilder)
}

// botBuilder constructs a bot channel. The cfg-derived workflows
// dir is `<DataDir>/workflows` (default `~/.nightme/workflows`).
// If the dir has no `*.yaml` files, we return an error (BuildAll
// treats this as "skip bot").
func botBuilder(cfg *config.Config) (channel.Channel, error) {
	dir, err := workflowsDir(cfg)
	if err != nil {
		return nil, err
	}
	workflows, err := wfe.LoadDir(dir)
	if err != nil || len(workflows) == 0 {
		return nil, fmt.Errorf("no workflows found in %s", dir)
	}
	// Build a default polling event source. v0 always uses
	// polling (no webhook server). The source polls `gh api` for
	// every workspace's remote origin.
	poller := newPollingSource(workflows)
	return New(Config{
		WorkflowsDir: dir,
		ActionsDir:   filepath.Join(dir, "actions"),
		EventSource:  poller,
	}), nil
}

// workflowsDir returns <DataDir>/workflows. Errors if DataDir
// can't be resolved (e.g. $HOME is unset).
func workflowsDir(cfg *config.Config) (string, error) {
	if cfg == nil || cfg.Paths.DataDir == "" {
		return "", fmt.Errorf("bot: cfg.Paths.DataDir is empty")
	}
	base, err := filepath.Abs(cfg.Paths.DataDir)
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "workflows"), nil
}

// newPollingSource constructs a default polling event source for
// the given workflows. Each workspace's git remote is added to
// the repos being polled. The owner login is empty in v0 (poller
// doesn't filter, bot's workspace filter takes over).
func newPollingSource(workflows []*wfe.Workflow) *gtw.PollingEventSource {
	seen := map[string]bool{}
	var repos []string
	for _, wf := range workflows {
		for _, ws := range wf.Workspaces {
			repo := gitOrigin(ws)
			if repo == "" {
				continue
			}
			// Normalize SSH/HTTPS to owner/repo.
			repo = strings.TrimSuffix(repo, ".git")
			if !seen[repo] {
				seen[repo] = true
				repos = append(repos, repo)
			}
		}
	}
	return &gtw.PollingEventSource{
		Repos:    repos,
		Provider: "github",
		Interval: 0, // default 30s
		State:    &gtw.PollingState{},
	}
}
