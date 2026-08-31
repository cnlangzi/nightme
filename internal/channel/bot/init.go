package bot

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/cnlangzi/nightme/internal/channel"
	"github.com/cnlangzi/nightme/internal/config"
	"github.com/cnlangzi/nightme/internal/wfe"
)

// init registers the bot channel with the channel registry so
// channel.BuildAll picks it up automatically (v1.3+ multi-channel).
//
// bot drives workflow runs keyed on workspace paths and does not
// surface per-chat sessions in chat_sessions.json; the prefix is
// still declared (as "bt_") so the chatstore validation knows it
// belongs to the bot family and skips bot chatIDs without warning
// if they happen to appear on disk.
//
// bot is enabled when the workflows dir exists (or can be created)
// under the user's nightme data dir. If not, the builder returns
// an error and BuildAll silently skips bot — no user-facing error.
func init() {
	channel.Register("bot", "bt_", botBuilder)
}

// botBuilder constructs a bot channel. The cfg-derived workflows
// dir is `<DataDir>/workflows` (default `~/.nightme/workflows`).
// If the dir has no `*.yaml` files, we return an error (BuildAll
// treats this as "skip bot").
//
// v0: cron-only — no git event source is constructed here. The
// trigger pipeline runs scheduled workflows and drops any other
// event kind until git event support is reintroduced.
func botBuilder(cfg *config.Config) (channel.Channel, error) {
	dir, err := workflowsDir(cfg)
	if err != nil {
		return nil, err
	}
	workflows, err := wfe.LoadDir(dir)
	if err != nil || len(workflows) == 0 {
		return nil, fmt.Errorf("no workflows found in %s", dir)
	}
	// Build the workspace→repo map (used by future git event
	// dispatch; cheap even when not used in v0). botBuilder
	// is invoked once during daemon startup; the caller
	// doesn't yet have a parent ctx, so context.Background()
	// is the right scope — gitOrigin inside caps its own
	// short timeout via timeouts.CLI.
	wsMap, err := buildWorkspaceRepoMap(context.Background(), workflows)
	if err != nil {
		return nil, err
	}
	_ = wsMap
	return New(Config{
		WorkflowsDir: dir,
		ActionsDir:   filepath.Join(dir, "actions"),
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
