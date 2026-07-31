package main

import (
	"fmt"
	"log/slog"
	"runtime/debug"

	"github.com/spf13/cobra"

	nmerrors "github.com/cnlangzi/nightme/internal/errors"
)

const panicMsg = "[nightme] internal panic recovered"

// Recover installs an idempotent panic guard. The optional logger keeps
// existing callers source-compatible while allowing runtime error tracking.
func Recover(rootCmd *cobra.Command, loggers ...*slog.Logger) {
	if rootCmd == nil {
		return
	}
	if _, ok := rootCmd.Annotations[panicGuardKey]; ok {
		return
	}
	if rootCmd.Annotations == nil {
		rootCmd.Annotations = map[string]string{}
	}
	rootCmd.Annotations[panicGuardKey] = "1"
	logger := loggerFromContext(rootCmd.Context())
	if len(loggers) > 0 && loggers[0] != nil {
		logger = loggers[0]
	}
	prevRunE := rootCmd.RunE
	rootCmd.RunE = func(cmd *cobra.Command, args []string) (err error) {
		defer func() {
			if r := recover(); r != nil {
				stack := debug.Stack()
				logger.Error("panic recovered", "err", r, "stack", string(stack))
				err = nmerrors.Wrap(nmerrors.CodeGenericError, fmt.Sprintf("panic: %v", r), nil)
			}
		}()
		if prevRunE != nil {
			return prevRunE(cmd, args)
		}
		return nil
	}
}

const panicGuardKey = "nightme.panic-guard"
