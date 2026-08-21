package gtw

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/pathutil"
)

// downloadHTTPClient is the http.Client used by
// downloadAttachments. Wired as a package var so tests can
// swap in an httptest server. Production uses the default
// client with a sane timeout — GitHub user-images and GitLab
// uploads both respond in well under 30s in normal conditions.
var downloadHTTPClient = &http.Client{
	Timeout: 30 * time.Second,
}

// downloadAttachments fetches each IssueAttachment from its
// URL and writes it under destDir/<index>-<filename>. The
// returned []agent.ContentBlock is suitable for inlining
// directly into a chatsession.Message.Blocks — one ContentFile
// block per downloaded file, in URL order.
//
// On any per-attachment failure the function aborts and
// returns the partial slice it has built so far plus the
// error. The caller decides whether to swallow (best-effort)
// or surface to the user.
//
// We don't try to be clever about MIME detection: the
// attachment's MIMEType field is the provider's best guess,
// refined by the HTTP Content-Type response header when
// available. Unknown → "application/octet-stream".
func downloadAttachments(ctx context.Context, atts []IssueAttachment, destDir string) ([]agent.ContentBlock, error) {
	if len(atts) == 0 {
		return nil, nil
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir attachments dir %s: %w", destDir, err)
	}
	blocks := make([]agent.ContentBlock, 0, len(atts))
	for i, att := range atts {
		name := att.Filename
		if name == "" {
			name = fmt.Sprintf("attachment-%d", i)
		}
		// Defensive: make sure filename can't escape destDir.
		// Strip any path components (slashes, "..") that a
		// malicious provider response could inject.
		name = pathutil.Base(name)
		if name == "." || name == "/" || name == "" {
			name = fmt.Sprintf("attachment-%d", i)
		}
		dest := pathutil.Join(destDir, name)

		mimeType, body, err := fetchAttachment(ctx, att.URL)
		if err != nil {
			return blocks, fmt.Errorf("download %s: %w", att.URL, err)
		}
		if err := os.WriteFile(dest, body, 0o644); err != nil {
			return blocks, fmt.Errorf("write %s: %w", dest, err)
		}
		// Refine MIMEType from the response when the
		// provider's hint was empty or "image/png" (the
		// GitHub placeholder).
		final := att.MIMEType
		if mimeType != "" && (final == "" || final == "image/png") {
			final = mimeType
		}
		if final == "" {
			final = "application/octet-stream"
		}
		blocks = append(blocks, agent.ContentBlock{
			Type:      agent.ContentFile,
			Path:      dest,
			MediaType: final,
		})
	}
	return blocks, nil
}

// fetchAttachment does one HTTP GET and returns the body's
// bytes plus the Content-Type header (lowercased, with
// parameters stripped). Returns an error on non-2xx responses
// with the status code surfaced.
func fetchAttachment(ctx context.Context, url string) (string, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", nil, fmt.Errorf("build request: %w", err)
	}
	// GitHub user-images specifically: these URLs don't
	// require auth (they're public CDN), but GitLab
	// /uploads/ usually needs the glab token. We don't pass
	// any auth header here — the http.Client is bare. If the
	// URL is auth-required, the server returns 401/403 and we
	// surface that as an error. Future: route through the
	// same CLIRunner the providers use so auth "just works".
	resp, err := downloadHTTPClient.Do(req)
	if err != nil {
		return "", nil, fmt.Errorf("GET: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", nil, fmt.Errorf("read body: %w", err)
	}
	mime := strings.TrimSpace(resp.Header.Get("Content-Type"))
	if i := strings.Index(mime, ";"); i >= 0 {
		mime = mime[:i]
	}
	mime = strings.ToLower(strings.TrimSpace(mime))
	return mime, body, nil
}

// attachmentsDir is the conventional location for downloaded
// issue attachments inside a worktree. Kept inside .nightme/
// so it's already covered by the gitignore EnsureGitignore
// writes — we don't want attachments showing up in `git
// status` and tripping the /gtw close dirty check.
func attachmentsDir(worktreePath string, issueID int) string {
	return pathutil.Join(worktreePath, nightmeDirName, "attachments",
		fmt.Sprintf("issue-%d", issueID))
}

// errDownload is the sentinel for "download failed but the
// user-visible flow can continue without attachments". Used
// by completeFixAndDispatch to log+continue rather than fail
// the whole fix.
var errDownload = errors.New("attachment download failed")

// downloadAttachmentsBestEffort is the variant called from
// completeFixAndDispatch: on download failure it logs a
// warning and returns whatever blocks succeeded. The dispatch
// then proceeds with text-only blocks (or partial file
// blocks) — the agent gets a slightly degraded prompt rather
// than no prompt at all.
func downloadAttachmentsBestEffort(ctx context.Context, atts []IssueAttachment, destDir string) []agent.ContentBlock {
	if len(atts) == 0 {
		return nil
	}
	blocks, err := downloadAttachments(ctx, atts, destDir)
	if err != nil {
		slog.Default().Warn("gtw: attachment download failed; dispatching with partial / text-only",
			"dest_dir", destDir,
			"attachments_total", len(atts),
			"blocks_so_far", len(blocks),
			"err", err)
		// Best-effort: return what we have. Wrapping the
		// underlying error in errDownload would let callers
		// errors.Is, but for v1 we just log and continue.
		_ = errDownload
	}
	return blocks
}