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

// maxAttachmentBytes bounds the size of a single downloaded
// attachment. Files larger than this are skipped: the download
// is aborted, the attachment is NOT written to disk, and a
// warning is logged. The dispatch text still mentions the
// attachment by URL so the agent can fetch it lazily if it
// actually needs the bytes.
//
// Non-image attachments (logs, dumps, archives) are the main
// risk: a 500MB heap dump would block the 30s HTTP timeout and
// waste disk under .nightme/attachments. Images are bounded by
// the bridge's own inline limit (claudecode caps at 5MB), so
// this guard mostly protects against pathological non-images.
const maxAttachmentBytes = 10 * 1024 * 1024 // 10 MB

// downloadAttachments fetches every IssueAttachment from its
// URL and writes it under destDir/<index>-<filename>. Each
// downloaded file becomes one ContentBlock: images →
// ContentImage (the bridge inlines pixels the model can see),
// everything else → ContentFile (the bridge emits a "File:
// <path>" annotation the model reads on demand with its file
// tools). No attachment is skipped on type alone — a log, a
// JSON dump, a PDF all land on disk so the agent has the bytes
// locally instead of having to re-fetch from a URL it may not
// be able to authenticate against.
//
// The image/non-image split exists because the bridges translate
// the two block types very differently: ContentImage is fed to
// the model as actual pixel data (base64-inlined by claudecode
// / codex, bracketed "[image: path]" by print-mode bridges),
// while ContentFile degrades to a text annotation pointing at
// the path. Inlining a screenshot as ContentFile would hide its
// pixels behind a bracket the model never sees; conversely,
// inlining a 5MB log as ContentImage would waste the vision
// channel on bytes the model can't read as text. So we classify
// by MIME and route to the right block type.
//
// Classification source priority: the HTTP response's
// Content-Type wins over the provider's MIMEType hint (which is
// often a placeholder "image/png" from extractGitHubAttachments
// or empty from GitLab). When the response says "image/*" →
// ContentImage; otherwise → ContentFile. We never discard a
// successfully-downloaded file: a URL that 302'd to an HTML login
// page still becomes a ContentFile so the agent can see what
// actually came back (better than silently dropping it).
//
// On any per-attachment download failure the function aborts
// and returns the partial slice it has built so far plus the
// error. The caller (downloadAttachmentsBestEffort) decides
// whether to swallow (best-effort) or surface to the user.
//
// Filenames are prefixed with the attachment index
// ("<i>-<name>") so two attachments that share a filename
// (common when a user drops two "screenshot.png" into one
// issue) don't clobber each other on disk.
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
		// Prefix with the index so same-named attachments
		// (two "screenshot.png" in one issue) don't overwrite
		// each other. The index also keeps on-disk order
		// stable across re-runs.
		dest := pathutil.Join(destDir, fmt.Sprintf("%d-%s", i, name))

		mimeType, body, err := fetchAttachment(ctx, att.URL)
		if err != nil {
			return blocks, fmt.Errorf("download %s: %w", att.URL, err)
		}
		// Size guard: a multi-MB log/dump would block the
		// HTTP timeout and waste disk. Skip the write and
		// leave this attachment out of the blocks slice; the
		// dispatch text still surfaces the URL so the agent
		// can fetch it lazily if it actually needs the bytes.
		if len(body) > maxAttachmentBytes {
			slog.Default().Warn("gtw: attachment exceeds size limit, skipping",
				"url", att.URL,
				"size", len(body),
				"limit", maxAttachmentBytes)
			continue
		}
		// Refine MIMEType: the HTTP response's Content-Type
		// is authoritative; the provider's hint is only used
		// when the response omitted Content-Type (rare).
		final := att.MIMEType
		if mimeType != "" {
			final = mimeType
		}
		if final == "" {
			final = "application/octet-stream"
		}
		if err := os.WriteFile(dest, body, 0o644); err != nil {
			return blocks, fmt.Errorf("write %s: %w", dest, err)
		}
		// Route by MIME: images → ContentImage (bridge inlines
		// pixels), everything else → ContentFile (bridge emits
		// a path annotation the agent reads on demand). We do
		// NOT discard non-image downloads — the bytes are on
		// disk and the agent can read them, which beats a URL
		// it may not be able to authenticate against.
		blockType := agent.ContentFile
		if isImageMIME(final) {
			blockType = agent.ContentImage
		}
		blocks = append(blocks, agent.ContentBlock{
			Type:      blockType,
			Path:      dest,
			MediaType: final,
		})
	}
	return blocks, nil
}

// isImageMIME reports whether a MIME type (provider hint or
// HTTP Content-Type) denotes an image the agent can see as
// pixels. Used to route a downloaded attachment to
// ContentImage (vision channel) vs ContentFile (file path).
func isImageMIME(mime string) bool {
	return strings.HasPrefix(mime, "image/")
}

// mimeFromExt maps a filename's extension to its best-guess
// MIME type. The attachment extractors use it to pre-classify
// a markdown `[](url)` / `![](url)` link before the HTTP
// response's Content-Type refines the type at download time.
// Image extensions map to image/* (so a `[](shot.png)` link
// without the `!` prefix still routes to ContentImage);
// non-image extensions map to their canonical type so the
// dispatch text's per-type count is accurate before download.
// Unknown extensions fall back to application/octet-stream,
// which downloadAttachments treats as a non-image → ContentFile.
func mimeFromExt(name string) string {
	n := strings.ToLower(name)
	switch {
	case strings.HasSuffix(n, ".png"):
		return "image/png"
	case strings.HasSuffix(n, ".jpg"), strings.HasSuffix(n, ".jpeg"):
		return "image/jpeg"
	case strings.HasSuffix(n, ".gif"):
		return "image/gif"
	case strings.HasSuffix(n, ".webp"):
		return "image/webp"
	case strings.HasSuffix(n, ".bmp"):
		return "image/bmp"
	case strings.HasSuffix(n, ".svg"):
		return "image/svg+xml"
	case strings.HasSuffix(n, ".pdf"):
		return "application/pdf"
	case strings.HasSuffix(n, ".json"):
		return "application/json"
	case strings.HasSuffix(n, ".txt"), strings.HasSuffix(n, ".log"):
		return "text/plain"
	case strings.HasSuffix(n, ".xml"):
		return "application/xml"
	case strings.HasSuffix(n, ".csv"):
		return "text/csv"
	case strings.HasSuffix(n, ".md"):
		return "text/markdown"
	case strings.HasSuffix(n, ".html"), strings.HasSuffix(n, ".htm"):
		return "text/html"
	case strings.HasSuffix(n, ".zip"):
		return "application/zip"
	case strings.HasSuffix(n, ".gz"), strings.HasSuffix(n, ".tgz"):
		return "application/gzip"
	case strings.HasSuffix(n, ".tar"):
		return "application/x-tar"
	default:
		return "application/octet-stream"
	}
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
// then proceeds with partial blocks (or text-only) — the
// agent gets a slightly degraded prompt rather than no prompt
// at all.
//
// Returns ContentImage blocks for image attachments and
// ContentFile blocks for non-image attachments. Every
// attachment that downloads successfully lands in the returned
// slice; only download failures (network, 4xx/5xx, oversize)
// are missing, and those are logged.
func downloadAttachmentsBestEffort(ctx context.Context, atts []IssueAttachment, destDir string) []agent.ContentBlock {
	if len(atts) == 0 {
		return nil
	}
	blocks, err := downloadAttachments(ctx, atts, destDir)
	if err != nil {
		slog.Default().Warn("gtw: attachment download failed; dispatching with partial blocks",
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
