package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestRunRestartInline_SpawnsPassedPath pins the contract this
// fix restored: runRestartInline spawns the path it is GIVEN,
// not a fresh os.Executable() inside the function.
//
// Why this matters: pre-fix, runRestartInline did
//
//	exe, _ := os.Executable()
//	proc.New(ctx, exe, "restart")
//
// inside the function. updater.Install renames the running
// binary aside (targetPath -> targetPath.old) before writing
// the new binary. On Linux, os.Executable() reads /proc/self/exe,
// which follows the inode — so after Install it returns
// targetPath.old, and the spawned `nightme restart` was the
// OLD binary restarting the daemon with the OLD binary. The
// fix moved os.Executable() to the call site (BEFORE Install)
// and threads the resulting string through here.
//
// The test catches any regression that re-introduces an
// os.Executable() call inside runRestartInline, and also pins
// the new parametrised signature against accidental removal
// (a no-arg runRestartInline call would no longer compile).
func TestRunRestartInline_SpawnsPassedPath(t *testing.T) {
	dir := t.TempDir()

	// Stub binary: writes its argv[0] to a known marker file
	// and exits 0. We pin the marker file path via the source
	// so runRestartInline's "restart" argument (the only arg it
	// passes) doesn't get re-purposed as the marker path.
	argvFile := filepath.Join(dir, "argv.txt")
	stubSrc := filepath.Join(dir, "stub.go")
	stubBin := filepath.Join(dir, "stub")
	if runtime.GOOS == "windows" {
		stubBin += ".exe"
	}
	src := fmt.Sprintf(`package main

import "os"

func main() {
	_ = os.WriteFile(%q, []byte(os.Args[0]), 0o644)
}
`, argvFile)
	if err := os.WriteFile(stubSrc, []byte(src), 0o644); err != nil {
		t.Fatalf("write stub source: %v", err)
	}
	build := exec.Command("go", "build", "-o", stubBin, stubSrc)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build stub: %v\n%s", err, out)
	}

	// runRestartInline spawns <targetPath> <args...>; the stub
	// is the binary at <targetPath> and writes its argv[0] to
	// argvFile. After it returns, argvFile holds the spawned
	// path's argv[0] — which, if the contract holds, equals
	// stubBin verbatim.
	if err := runRestartInline(io.Discard, stubBin); err != nil {
		t.Fatalf("runRestartInline: %v", err)
	}

	got, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("read argv marker: %v", err)
	}
	gotStr := strings.TrimRight(string(got), "\r\n")
	if gotStr != stubBin {
		t.Errorf("runRestartInline did not spawn the path it was given:\n  got:  %q\n  want: %q",
			gotStr, stubBin)
	}
}
