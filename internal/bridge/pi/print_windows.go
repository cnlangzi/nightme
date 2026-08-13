//go:build windows

// Windows stub for the print-mode spawn. The real implementation
// lives in print_unix.go; the bridge has no Windows production
// use today (claudecode is the dominant bridge on Windows per
// F-32 / F-54), but we keep the build tag balanced so the
// package compiles on both platforms and a future Windows
// port has an obvious landing spot.
//
// If / when print-mode is needed on Windows, the changes from
// print_unix.go are: pipe creation uses syscall.CreatePipe
// instead of os/exec's defaults, and process group signals
// need SysProcAttr.CreationFlags with CREATE_NEW_PROCESS_GROUP
// (the Setsid Unix call has no Windows equivalent). See
// exec_windows.go in this package for the per-bridge pattern
// nightme already uses.

package pi

import (
	"context"
	"fmt"

	"github.com/cnlangzi/nightme/internal/agent"
)

func runPrintMode(ctx context.Context, command, prompt, workspace string) (string, error) {
	_ = ctx
	_ = command
	_ = prompt
	return "", fmt.Errorf("pi: print mode not yet implemented on Windows (open an issue if needed)")
}

// Keep the agent import referenced even though we don't use
// it on Windows yet — it's the type the unix implementation
// takes via the translate helper, and matching signatures
// across platforms avoids a future cross-port footgun.
var _ agent.Info
