// Package main is the nightme daemon entrypoint.
package main

import (
	"fmt"
	"os"

	"github.com/cnlangzi/nightme/internal/config"
	"github.com/cnlangzi/nightme/internal/logging"

	// Register built-in agents. The blank import triggers each
	// agent package's init(), which adds the agent to
	// internal/agent.Builtins. Adding a new agent = new package
	// + blank import here; no dispatch table to keep in sync.
	_ "github.com/cnlangzi/nightme/internal/bridge/claudecode"
)

func main() {
	cfg, err := config.LoadDefault()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
	logger, err := logging.New(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
	defer func() { _ = logging.Close(logger) }()
	Execute(logger)
}
