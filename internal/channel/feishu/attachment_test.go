package feishu

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/channel"
)

// downloadTestRig wires DownloadAttachments to a test inbox
// directory and restores InboxBaseDir on cleanup.
type downloadTestRig struct {
	origInbox func() (string, error)
	tmpDir    string
}

func newDownloadTestRig(t *testing.T) *downloadTestRig {
	t.Helper()
	dir := t.TempDir()
	rig := &downloadTestRig{
		origInbox: InboxBaseDir,
		tmpDir:    dir,
	}
	InboxBaseDir = func() (string, error) { return dir, nil }
	t.Cleanup(func() { InboxBaseDir = rig.origInbox })
	return rig
}

// --- tests ---

func TestDownloadInboxDir_CreatesPerSessionDir(t *testing.T) {
	_ = newDownloadTestRig(t)

	dir, err := inboxDirForSession("sess_abc")
	if err != nil {
		t.Fatalf("inboxDirForSession: %v", err)
	}
	wantSuffix := filepath.Join("sess_abc")
	if !strings.HasSuffix(dir, wantSuffix) {
		t.Errorf("inbox dir = %q, want suffix %q", dir, wantSuffix)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat inbox dir: %v", err)
	}
	if !info.IsDir() {
		t.Errorf("inbox path is not a directory: %s", dir)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("inbox dir mode = %#o, want 0700", perm)
	}
}

func TestDownloadInboxDir_EmptySessionID(t *testing.T) {
	if _, err := inboxDirForSession(""); err == nil {
		t.Error("inboxDirForSession(\"\") returned nil error; want error")
	}
}

func TestDownloadAttachments_EmptyInput(t *testing.T) {
	_ = newDownloadTestRig(t)
	res := DownloadAttachments(context.Background(), nil, "om_xxx", nil, "sess_1")
	if res.HasAttachments {
		t.Errorf("HasAttachments = true, want false for empty input")
	}
	if res.AllFailed {
		t.Errorf("AllFailed = true, want false for empty input")
	}
	if len(res.Atts) != 0 || len(res.FailureKeys) != 0 {
		t.Errorf("expected zero-length Atts / FailureKeys for empty input, got %+v", res)
	}
}

func TestBuildForwardedText_PreservesAttachmentOrder(t *testing.T) {
	atts := []channel.Attachment{
		{Type: "image", LocalPath: "/tmp/a.png"},
		{Type: "file", LocalPath: "/tmp/b.pdf"},
		{Type: "audio", LocalPath: "/tmp/c.m4a"},
	}
	got := BuildForwardedText("", atts)
	want := "attachment (image): /tmp/a.png\nattachment (file): /tmp/b.pdf\nattachment (audio): /tmp/c.m4a"
	if got != want {
		t.Errorf("BuildForwardedText = %q, want %q", got, want)
	}
}

func TestBuildForwardedText_NoTrailingNewline(t *testing.T) {
	got := BuildForwardedText("caption", []channel.Attachment{
		{Type: "image", LocalPath: "/tmp/a.png"},
	})
	if strings.HasSuffix(got, "\n") {
		t.Errorf("BuildForwardedText has trailing newline: %q", got)
	}
}

// --- uniquePath ---

func TestUniquePath_FirstSlotFree(t *testing.T) {
	dir := t.TempDir()
	got, err := uniquePath(dir, "test.png")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(got) != "test.png" {
		t.Errorf("first slot should be %q, got %q", "test.png", filepath.Base(got))
	}
}

func TestUniquePath_AppendsSuffix(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "test.png"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := uniquePath(dir, "test.png")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(got) != "test_2.png" {
		t.Errorf("expected test_2.png, got %q", filepath.Base(got))
	}
}

func TestUniquePath_PreservesExtension(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "report.pdf"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "report_2.pdf"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := uniquePath(dir, "report.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(got) != "report_3.pdf" {
		t.Errorf("expected report_3.pdf, got %q", filepath.Base(got))
	}
}

func TestUniquePath_NoExtension(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Makefile"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := uniquePath(dir, "Makefile")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(got) != "Makefile_2" {
		t.Errorf("expected Makefile_2, got %q", filepath.Base(got))
	}
}

// --- nextBackoff schedule ---

func TestNextBackoff_DoublesUntilCap(t *testing.T) {
	if got := nextBackoff(initialBackoff); got != initialBackoff*backoffMultiplier {
		t.Errorf("first nextBackoff = %v, want %v", got, initialBackoff*backoffMultiplier)
	}
	// After enough doublings we hit the cap.
	v := initialBackoff
	for i := 0; i < 20; i++ {
		v = nextBackoff(v)
	}
	if v != maxBackoffDuration {
		t.Errorf("nextBackoff did not converge to cap; final = %v, want %v", v, maxBackoffDuration)
	}
}