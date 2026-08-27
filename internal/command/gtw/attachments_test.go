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

// TestAttachmentsFromBody_GitHub verifies the GitHub provider's
// per-body attachment extraction: finds `![alt](url)` image syntax
// AND `[label](url)` plain-link syntax, ignores non-http URLs and
// data: URIs, strips query strings from filenames, and seeds
// MIMEType from the filename extension via mimeFromExt.
//
// The plain-link case (`[](shot.png)` without the `!` prefix) is
// the v1 fix from cnlangzi/nightme#294 — prior to that, the
// package-level parser's `body[i] != '!'` guard dropped every
// plain link, so a `[](crash.log)` reference to a log file was
// silently invisible to the dispatched agent. The new behaviour
// routes such links through mimeFromExt so `[](shot.png)` lands
// on ContentImage and `[](crash.log)` on ContentFile.
func TestAttachmentsFromBody_GitHub(t *testing.T) {
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

	prov := &GitHubProvider{}
	got := prov.attachmentsFromBody(body)
	if len(got) != 3 {
		t.Fatalf("got %d attachments, want 3 (image-form, image-form-with-query, plain-link-form): %+v", len(got), got)
	}
	if got[0].URL != "https://user-images.githubusercontent.com/abc.png" {
		t.Errorf("[0] URL = %q", got[0].URL)
	}
	if got[0].Filename != "abc.png" {
		t.Errorf("[0] Filename = %q, want abc.png", got[0].Filename)
	}
	if got[0].MIMEType != "image/png" {
		t.Errorf("[0] MIMEType = %q, want image/png (seeded from .png ext)", got[0].MIMEType)
	}
	if got[1].URL != "https://example.com/path/image.jpg?token=xyz" {
		t.Errorf("[1] URL = %q", got[1].URL)
	}
	// Filename should have query string stripped.
	if got[1].Filename != "image.jpg" {
		t.Errorf("[1] Filename = %q, want image.jpg (query stripped)", got[1].Filename)
	}
	if got[1].MIMEType != "image/jpeg" {
		t.Errorf("[1] MIMEType = %q, want image/jpeg", got[1].MIMEType)
	}
	// [link](https://example.com) — plain link, no `!` prefix.
	// Pre-#294 this was silently dropped; post-fix it's picked up
	// with filename = "example.com" and mimeFromExt falling back
	// to application/octet-stream. downloadAttachments will route
	// that to ContentFile (the agent can still read it as text).
	if got[2].URL != "https://example.com" {
		t.Errorf("[2] URL = %q, want https://example.com (plain link picked up)", got[2].URL)
	}
	if got[2].Filename != "example.com" {
		t.Errorf("[2] Filename = %q, want example.com (URL last segment)", got[2].Filename)
	}
	if got[2].MIMEType != "application/octet-stream" {
		t.Errorf("[2] MIMEType = %q, want application/octet-stream (unknown ext fallback)", got[2].MIMEType)
	}
}

// TestAttachmentsFromBody_GitHub_PlainLinkToImage pins the v1
// fix from #294 explicitly: a `[](shot.png)` reference (no `!`
// prefix) to an image file must be picked up, NOT silently dropped
// the way the pre-#294 `!`-guarded parser used to. The MIME hint
// from mimeFromExt routes it to ContentImage at dispatch time.
func TestAttachmentsFromBody_GitHub_PlainLinkToImage(t *testing.T) {
	body := "see [screenshot](https://example.com/uploads/shot.png)"

	prov := &GitHubProvider{}
	got := prov.attachmentsFromBody(body)
	if len(got) != 1 {
		t.Fatalf("got %d attachments, want 1: %+v", len(got), got)
	}
	if got[0].URL != "https://example.com/uploads/shot.png" {
		t.Errorf("URL = %q", got[0].URL)
	}
	if got[0].Filename != "shot.png" {
		t.Errorf("Filename = %q, want shot.png", got[0].Filename)
	}
	if got[0].MIMEType != "image/png" {
		t.Errorf("MIMEType = %q, want image/png (plain link to image → routed as image)", got[0].MIMEType)
	}
}

// TestAttachmentsFromBody_GitHub_EmptyAltLink pins the parser's
// empty-alt handling. With the `!` guard removed (#294), the
// closeBracket loop must start at j = i+1 so `[](url)` (empty
// alt) is matched. Pre-fix regression: the loop started at i+2
// (safe only because the old `!` guard pinned body[i+1] as `[`),
// which silently dropped every `[](url)` reference. This test
// would have failed against that bug.
func TestAttachmentsFromBody_GitHub_EmptyAltLink(t *testing.T) {
	body := "log dump: [](https://example.com/crash.log)"

	prov := &GitHubProvider{}
	got := prov.attachmentsFromBody(body)
	if len(got) != 1 {
		t.Fatalf("got %d attachments, want 1 (empty-alt [] must match): %+v", len(got), got)
	}
	if got[0].URL != "https://example.com/crash.log" {
		t.Errorf("URL = %q", got[0].URL)
	}
	if got[0].Filename != "crash.log" {
		t.Errorf("Filename = %q, want crash.log", got[0].Filename)
	}
	if got[0].MIMEType != "text/plain" {
		t.Errorf("MIMEType = %q, want text/plain (.log extension)", got[0].MIMEType)
	}
}

// TestAttachmentsFromBody_GitHub_EmptyAndNoMatches covers the
// no-op cases: empty body → nil, body without any http(s) link
// → nil. downloadAttachments treats nil as "no work to do".
func TestAttachmentsFromBody_GitHub_EmptyAndNoMatches(t *testing.T) {
	prov := &GitHubProvider{}

	if got := prov.attachmentsFromBody(""); got != nil {
		t.Errorf("empty body: got %+v, want nil", got)
	}
	if got := prov.attachmentsFromBody("just text, no links here"); got != nil {
		t.Errorf("no links: got %+v, want nil", got)
	}
	// data: / file: / mailto: URIs are skipped — the parser
	// rejects anything that isn't http(s).
	if got := prov.attachmentsFromBody(
		"see [data](data:image/png;base64,xxx) or [local](file:///etc/passwd)",
	); got != nil {
		t.Errorf("non-http URIs: got %+v, want nil", got)
	}
}

// TestAttachmentsFromBody_GitLab verifies the GitLab provider's
// attachment extraction. v1 (cnlangzi/nightme#294) reuses the
// same body parser as GitHub — body markdown is the same
// convention, and GitLab users previously got ZERO attachments
// dispatched because GitLabProvider.GetIssue returned
// Attachments: nil. After this refactor GitLab users get the
// same body-derived dispatch as GitHub.
//
// The future native `glab api … attachment_links` migration is
// tracked as a TODO inside (*GitLabProvider).attachmentsFromBody;
// when that lands the assertions here will tighten (host
// allowlist for `/uploads/...`, richer metadata).
func TestAttachmentsFromBody_GitLab(t *testing.T) {
	body := "" +
		"![screenshot](https://gitlab.com/uploads/screenshot.png)\n" +
		"\n" +
		"log dump: [crash.log](https://gitlab.example.com/path/crash.log)\n" +
		"\n" +
		"unrelated [docs](https://docs.example.com)\n" +
		"\n"

	prov := &GitLabProvider{}
	got := prov.attachmentsFromBody(body)
	if len(got) != 3 {
		t.Fatalf("got %d attachments, want 3: %+v", len(got), got)
	}
	if got[0].URL != "https://gitlab.com/uploads/screenshot.png" {
		t.Errorf("[0] URL = %q", got[0].URL)
	}
	if got[0].MIMEType != "image/png" {
		t.Errorf("[0] MIMEType = %q, want image/png", got[0].MIMEType)
	}
	if got[1].URL != "https://gitlab.example.com/path/crash.log" {
		t.Errorf("[1] URL = %q", got[1].URL)
	}
	if got[1].MIMEType != "text/plain" {
		t.Errorf("[1] MIMEType = %q, want text/plain (.log extension)", got[1].MIMEType)
	}
	if got[2].URL != "https://docs.example.com" {
		t.Errorf("[2] URL = %q", got[2].URL)
	}
}

// TestAttachmentsFromBody_GitLab_Empty covers the no-op case for
// the GitLab path: empty body → nil. The download pipeline treats
// nil as "no work to do" and falls through to text-only dispatch.
func TestAttachmentsFromBody_GitLab_Empty(t *testing.T) {
	prov := &GitLabProvider{}
	if got := prov.attachmentsFromBody(""); got != nil {
		t.Errorf("empty body: got %+v, want nil", got)
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
