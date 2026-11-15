//go:build windows

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestTheReportIsReplacedWhileItIsRead pins the Windows sharing rule
// the startup report depends on.
//
// A file another process holds open cannot be replaced unless the
// opener allowed FILE_SHARE_DELETE. The parent polls the report while
// the daemon rewrites it, so the parent's handle has to allow the
// delete — otherwise the daemon's rename fails with "Access is
// denied", the report keeps the version it had before the channels
// were tried, and the operator never sees which channel came up.
func TestTheReportIsReplacedWhileItIsRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, daemonStartupReportName)
	if err := os.WriteFile(path, []byte(`{"pid":1}`), 0o600); err != nil {
		t.Fatalf("write the report: %v", err)
	}

	f, err := openReportFile(path)
	if err != nil {
		t.Fatalf("open the report: %v", err)
	}
	defer func() { _ = f.Close() }()

	tmp := filepath.Join(dir, ".daemon-startup-test.json.tmp")
	if err := os.WriteFile(tmp, []byte(`{"pid":2}`), 0o600); err != nil {
		t.Fatalf("write the temp file: %v", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatalf("the report was not replaced while it was open: %v", err)
	}
}
