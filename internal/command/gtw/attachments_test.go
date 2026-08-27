package gtw

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
)

// TestDownloadAttachments_FetchesAllFiles exercises the full
// HTTP download loop via an httptest server. Confirms each
// attachment lands at destDir/<index>-<name>, MIME type is
// the server's Content-Type, and the returned ContentBlock
// slice has the right Type / Path / MediaType for the agent.
func TestDownloadAttachments_FetchesAllFiles(t *testing.T) {
	// httptest server: /img1 returns PNG bytes with
	// image/png; /file2.txt returns "hello world" with
	// text/plain.
	mux := http.NewServeMux()
	mux.HandleFunc("/img1.png", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = io.WriteString(w, "PNG-BYTES")
	})
	mux.HandleFunc("/file2.txt", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "hello world")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	destDir := filepath.Join(t.TempDir(), "attachments")

	atts := []IssueAttachment{
		{URL: srv.URL + "/img1.png", Filename: "shot.png", MIMEType: "image/png"},
		{URL: srv.URL + "/file2.txt", Filename: "notes.txt"},
	}
	blocks, err := downloadAttachments(context.Background(), atts, destDir)
	if err != nil {
		t.Fatalf("downloadAttachments: %v", err)
	}
	if len(blocks) != 2 {
		t.Fatalf("got %d blocks, want 2", len(blocks))
	}

	// File on disk + MIME refinement + block-type routing.
	// Images → ContentImage (bridge inlines pixels); non-images
	// → ContentFile (bridge emits a path annotation). Filenames
	// are prefixed with the index so same-named attachments
	// don't clobber each other.
	for i, want := range []struct {
		name, body, mime string
		wantType        agent.ContentBlockType
	}{
		{"0-shot.png", "PNG-BYTES", "image/png", agent.ContentImage},
		{"1-notes.txt", "hello world", "text/plain", agent.ContentFile}, // charset suffix stripped
	} {
		path := filepath.Join(destDir, want.name)
		got, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("[%d] read %s: %v", i, path, err)
			continue
		}
		if string(got) != want.body {
			t.Errorf("[%d] body = %q, want %q", i, string(got), want.body)
		}
		if blocks[i].Type != want.wantType {
			t.Errorf("[%d] block type = %v, want %v", i, blocks[i].Type, want.wantType)
		}
		if blocks[i].Path != path {
			t.Errorf("[%d] block path = %q, want %q", i, blocks[i].Path, path)
		}
		if !strings.HasPrefix(blocks[i].MediaType, want.mime) {
			t.Errorf("[%d] block mediaType = %q, want %q prefix", i, blocks[i].MediaType, want.mime)
		}
	}
}

// TestDownloadAttachments_RejectsHTTPError verifies a 4xx/5xx
// response surfaces as an error rather than an empty file.
// (httptest's default for unhandled paths is 404, but we
// register a deliberate 500.)
func TestDownloadAttachments_RejectsHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	destDir := filepath.Join(t.TempDir(), "att")
	_, err := downloadAttachments(context.Background(),
		[]IssueAttachment{{URL: srv.URL + "/x", Filename: "x"}},
		destDir)
	if err == nil {
		t.Fatal("want error for HTTP 500, got nil")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("err = %v, want '500' in message", err)
	}
}

// TestDownloadAttachments_NumberedFallback covers the
// filename fallback when the attachment has empty Filename.
func TestDownloadAttachments_NumberedFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "data")
	}))
	defer srv.Close()
	destDir := filepath.Join(t.TempDir(), "att")
	blocks, err := downloadAttachments(context.Background(),
		[]IssueAttachment{
			{URL: srv.URL + "/a"},
			{URL: srv.URL + "/b"},
		},
		destDir)
	if err != nil {
		t.Fatalf("downloadAttachments: %v", err)
	}
	if len(blocks) != 2 {
		t.Fatalf("blocks = %d, want 2", len(blocks))
	}
	// Filenames are index-prefixed ("<i>-<name>") to avoid
	// same-name clobbering; empty Filename falls back to
	// "attachment-<i>", so the on-disk name is "<i>-attachment-<i>".
	for i, b := range blocks {
		want := filepath.Join(destDir, fmt.Sprintf("%d-attachment-%d", i, i))
		if b.Path != want {
			t.Errorf("[%d] path = %q, want %q", i, b.Path, want)
		}
		if _, err := os.Stat(want); err != nil {
			t.Errorf("[%d] file missing: %v", i, err)
		}
	}
}

// TestDownloadAttachments_EmptyInput is the no-op case:
// passing zero attachments yields nil blocks / nil error.
func TestDownloadAttachments_EmptyInput(t *testing.T) {
	blocks, err := downloadAttachments(context.Background(), nil, t.TempDir())
	if err != nil {
		t.Errorf("err = %v, want nil", err)
	}
	if blocks != nil {
		t.Errorf("blocks = %v, want nil", blocks)
	}
}

// TestExtractGitHubAttachments verifies the markdown-image
// parser finds images in `![alt](url)` syntax, ignores
// non-http URLs, and handles multi-line bodies.
func TestExtractGitHubAttachments(t *testing.T) {
	body := "" +
		"Some text.\n" +
		"\n" +
		"![first](https://user-images.githubusercontent.com/abc.png)\n" +
		"\n" +
		"More text.\n" +
		"\n" +
		"![second](https://example.com/path/image.jpg?token=xyz)\n" +
		"\n" +
		"Not an image: [link](https://example.com)\n" +
		"\n" +
		"data: ![bad](data:image/png;base64,xxx)\n" +
		"\n"

	got := extractGitHubAttachments(body)
	if len(got) != 2 {
		t.Fatalf("got %d attachments, want 2: %+v", len(got), got)
	}
	if got[0].URL != "https://user-images.githubusercontent.com/abc.png" {
		t.Errorf("[0] URL = %q", got[0].URL)
	}
	if got[0].Filename != "abc.png" {
		t.Errorf("[0] Filename = %q, want abc.png", got[0].Filename)
	}
	if got[1].URL != "https://example.com/path/image.jpg?token=xyz" {
		t.Errorf("[1] URL = %q", got[1].URL)
	}
	// Filename should have query string stripped.
	if got[1].Filename != "image.jpg" {
		t.Errorf("[1] Filename = %q, want image.jpg (query stripped)", got[1].Filename)
	}
}

// TestBuildIssueDispatchBlocks confirms the assembled blocks
// slice is [ContentText, ContentFile, ContentFile] when
// attachmentBlocks has two entries.
func TestBuildIssueDispatchBlocks(t *testing.T) {
	issue := &Issue{
		ID:    7,
		Title: "Test",
		Body:  "body",
		URL:   "https://x",
	}
	attachments := []agent.ContentBlock{
		{Type: agent.ContentFile, Path: "/a", MediaType: "image/png"},
		{Type: agent.ContentFile, Path: "/b", MediaType: "text/plain"},
	}
	blocks := buildIssueDispatchBlocks(issue, attachments, "branch-x", "owner/repo", DispatchPlan)
	if len(blocks) != 3 {
		t.Fatalf("blocks = %d, want 3", len(blocks))
	}
	if blocks[0].Type != agent.ContentText {
		t.Errorf("[0] type = %v, want ContentText", blocks[0].Type)
	}
	if !strings.Contains(blocks[0].Text, "issue #7") {
		t.Errorf("[0] text missing 'issue #7':\n%s", blocks[0].Text)
	}
	for i := 1; i < 3; i++ {
		if blocks[i].Type != agent.ContentFile {
			t.Errorf("[%d] type = %v, want ContentFile", i, blocks[i].Type)
		}
	}
}

// TestAttachmentsDir verifies the conventional path used by
// completeFixAndDispatch: <worktree>/.nightme/attachments/issue-<id>.
func TestAttachmentsDir(t *testing.T) {
	got := attachmentsDir("/worktree", 42)
	// attachmentsDir uses filepath.Join which produces native
	// separators (\\ on Windows, / on Unix). Build the expected
	// via filepath.FromSlash so the assertion holds on both.
	want := filepath.FromSlash("/worktree/.nightme/attachments/issue-42")
	if got != want {
		t.Errorf("attachmentsDir = %q, want %q", got, want)
	}
}
