package main

import (
	"fmt"
	"os"
	"runtime/debug"

	"github.com/spf13/cobra"

	nmerrors "github.com/cnlangzi/nightme/internal/errors"
)

// panicMsg is the single source of truth for the panic-recovery
// banner.
const panicMsg = "[nightme] internal panic recovered"

// Recover wraps rootCmd so a panic anywhere inside cobra's Run
// pipeline is captured, logged with a stack trace, and converted
// to nmerrors.CodeGenericError. Run returns the CodedError as a
// regular RunE error so the existing Execute() flow handles the
// exit code without further branching.
//
// Recover is idempotent: calling it twice on the same root only
// installs one panic guard.
func Recover(rootCmd *cobra.Command) {
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

	// Guard the entire RunE chain. Wrapping rather than replacing
	// preserves existing command logic and lets tests still call
	// newRootCmd() without the panic guard when they prefer.
	prevRunE := rootCmd.RunE
	rootCmd.RunE = func(cmd *cobra.Command, args []string) (err error) {
		defer func() {
			if r := recover(); r != nil {
				stack := debug.Stack()
				fmt.Fprintf(os.Stderr, "%s: %v\n", panicMsg, r)
				fmt.Fprintf(os.Stderr, "%s\n", stack)
				err = nmerrors.Wrap(
					nmerrors.CodeGenericError,
					fmt.Sprintf("panic: %v", r),
					nil,
				)
			}
		}()
		if prevRunE != nil {
			return prevRunE(cmd, args)
		}
		return nil
	}
}

// panicGuardKey is the cobra.Annotations key used to detect prior
// Recover() calls. Kept short to avoid Annotation bloat.
const panicGuardKey = "nightme.panic-guard"
