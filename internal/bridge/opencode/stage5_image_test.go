
// Tests for stage 5: image base64 inline.
//
// Behaviour we verify:
//   - ContentImage with a small file: produces a data:<mime>;base64,...
//     URL in the prompt part (rather than file://).
//   - ContentFile (non-image) still uses file:// URL.
//   - ContentImage with bytes > maxImageBytes returns ErrImageTooLarge
//     BEFORE making any HTTP request.
//   - ContentImage with a missing file returns a clear "opencode: read image ..."
//     error rather than panicking.
//   - Errors release pendingTurnActive so the next SendBlocks can proceed.
package opencode

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
)

// captureBodyServer is a tiny httptest helper that captures the
// incoming request body and replies with a canned response so we
// can inspect the prompt payload from the test.
type captureBodyServer struct {
	t        *testing.T
	calls    atomic.Int32
	captured *string
	body     string
	code     int
}

func (s *captureBodyServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/session/", func(w http.ResponseWriter, r *http.Request) {
		s.calls.Add(1)
		buf, _ := io.ReadAll(r.Body)
		*s.captured = string(buf)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(s.code)
		_, _ = w.Write([]byte(s.body))
	})
	return mux
}

func makeAgent(t *testing.T, srvURL string) *driver {
	t.Helper()
	return &driver{
		name:        "opencode",
		workspace:   "/tmp",
		server:      &serverProc{baseURL: srvURL},
		events:      make(chan agent.AgentEvent, 16),
		closed:      make(chan struct{}),
		stopDeliver: make(chan struct{}),
		exitDone:    make(chan struct{}),
		sessionID:   "ses_1",
		trans:       newTranslator(stubDeliver2(), "opencode", "/tmp", "main", "ses_1", ""),
	}
}

// TestSendBlocks_ImageInlineBase64 verifies ContentImage uses
// data:<mime>;base64,.... URL.
func TestSendBlocks_ImageInlineBase64(t *testing.T) {
	dir := t.TempDir()
	imgPath := filepath.Join(dir, "img.png")
	payload := []byte("\x89PNG\r\n\x1a\nfake-png-bytes")
	if err := os.WriteFile(imgPath, payload, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cap := &captureBodyServer{t: t, body: `{"info":{}}`, code: 200, captured: new(string)}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()

	a := makeAgent(t, srv.URL)
	a.client = newClient(&serverProc{baseURL: srv.URL}, "/tmp")

	if err := a.SendBlocks(context.Background(), []agent.ContentBlock{
		{Type: agent.ContentImage, Path: imgPath, MediaType: "image/png"},
	}); err != nil {
		t.Fatalf("SendBlocks: %v", err)
	}

	want := "data:image/png;base64," + base64.StdEncoding.EncodeToString(payload)
	if !strings.Contains(*cap.captured, want) {
		t.Errorf("body does not contain expected data URL; got:\n%s", truncate(*cap.captured, 300))
	}
	if strings.Contains(*cap.captured, "file://") {
		t.Errorf("body still uses file:// URL — stage 5 not applied")
	}
	if cap.calls.Load() == 0 {
		t.Errorf("no HTTP request was made")
	}
}

// TestSendBlocks_FileStillUsesFileURL verifies ContentFile (non-image)
// keeps file:// reference.
func TestSendBlocks_FileStillUsesFileURL(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "doc.txt")
	if err := os.WriteFile(filePath, []byte("hello"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cap := &captureBodyServer{t: t, body: `{"info":{}}`, code: 200, captured: new(string)}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()

	a := makeAgent(t, srv.URL)
	a.client = newClient(&serverProc{baseURL: srv.URL}, "/tmp")

	if err := a.SendBlocks(context.Background(), []agent.ContentBlock{
		{Type: agent.ContentFile, Path: filePath, MediaType: "text/plain"},
	}); err != nil {
		t.Fatalf("SendBlocks: %v", err)
	}
	// filePath is a Windows path on Windows hosts (with `\`
	// separators). The bridge passes it through as a file:// URL
	// to opencode's wire format, which is JSON — but the
	// opencode wire layer normalises path separators to `/` for
	// the `file://` URL prefix (URLs mandate forward slashes per
	// RFC 3986). So the captured body contains a `/`-style URL
	// regardless of the host path style. We assert against the
	// `filepath.ToSlash`-normalised form so the test passes on
	// every host.
	want := "file://" + filepath.ToSlash(filePath)
	if !strings.Contains(*cap.captured, want) {
		t.Errorf("file block did not use file:// URL: missing %s", want)
	}
}

// TestSendBlocks_ImageTooLarge verifies oversized images are
// rejected before any HTTP request is made.
func TestSendBlocks_ImageTooLarge(t *testing.T) {
	dir := t.TempDir()
	bigPath := filepath.Join(dir, "big.png")
	f, err := os.Create(bigPath)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := f.Write(make([]byte, maxImageBytes+1)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	_ = f.Close()

	cap := &captureBodyServer{t: t, body: `{"info":{}}`, code: 200, captured: new(string)}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()

	a := makeAgent(t, srv.URL)
	a.client = newClient(&serverProc{baseURL: srv.URL}, "/tmp")

	err = a.SendBlocks(context.Background(), []agent.ContentBlock{
		{Type: agent.ContentImage, Path: bigPath, MediaType: "image/png"},
	})
	if err == nil {
		t.Fatalf("SendBlocks with oversized image = nil, want error")
	}
	if !errors.Is(err, ErrImageTooLarge) {
		t.Errorf("error = %v, want ErrImageTooLarge", err)
	}
	if cap.calls.Load() != 0 {
		t.Errorf("HTTP request was made despite size error")
	}
}

// TestSendBlocks_ImageMissingPath verifies missing files produce
// a clear error and release pendingTurnActive.
func TestSendBlocks_ImageMissingPath(t *testing.T) {
	cap := &captureBodyServer{t: t, body: `{"info":{}}`, code: 200, captured: new(string)}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()

	a := makeAgent(t, srv.URL)
	a.client = newClient(&serverProc{baseURL: srv.URL}, "/tmp")

	err := a.SendBlocks(context.Background(), []agent.ContentBlock{
		{Type: agent.ContentImage, Path: "/tmp/definitely-not-here-12345.png", MediaType: "image/png"},
	})
	if err == nil {
		t.Fatalf("SendBlocks with missing image = nil, want error")
	}
	if !strings.Contains(err.Error(), "read image") {
		t.Errorf("error = %v, want contains 'read image'", err)
	}
	a.pendingMu.Lock()
	if a.pendingTurnActive {
		t.Errorf("pendingTurnActive held after error")
	}
	a.pendingMu.Unlock()
}

// TestSendBlocks_ImageEmptySkipped verifies empty image paths drop
// the block (no error) and don't trigger anything.
func TestSendBlocks_ImageEmptySkipped(t *testing.T) {
	cap := &captureBodyServer{t: t, body: `{"info":{}}`, code: 200, captured: new(string)}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()

	a := makeAgent(t, srv.URL)
	a.client = newClient(&serverProc{baseURL: srv.URL}, "/tmp")

	if err := a.SendBlocks(context.Background(), []agent.ContentBlock{
		{Type: agent.ContentImage, Path: "", MediaType: "image/png"},
	}); err != nil {
		t.Errorf("SendBlocks with empty image path = %v, want nil", err)
	}
	a.pendingMu.Lock()
	if a.pendingTurnActive {
		t.Errorf("pendingTurnActive set on no-op")
	}
	a.pendingMu.Unlock()
}

// TestSendBlocks_ImageDefaultMime verifies missing MediaType
// defaults to image/png.
func TestSendBlocks_ImageDefaultMime(t *testing.T) {
	dir := t.TempDir()
	imgPath := filepath.Join(dir, "blob")
	if err := os.WriteFile(imgPath, []byte("data"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	cap := &captureBodyServer{t: t, body: `{"info":{}}`, code: 200, captured: new(string)}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()

	a := makeAgent(t, srv.URL)
	a.client = newClient(&serverProc{baseURL: srv.URL}, "/tmp")

	if err := a.SendBlocks(context.Background(), []agent.ContentBlock{
		{Type: agent.ContentImage, Path: imgPath, MediaType: ""},
	}); err != nil {
		t.Fatalf("SendBlocks: %v", err)
	}
	if !strings.Contains(*cap.captured, "data:image/png;base64,") {
		t.Errorf("body missing default mime 'image/png'; got: %s", truncate(*cap.captured, 200))
	}
}

// ─── helpers ─────────────────────────────────────────────────────

// stubDeliver2 is a no-op deliver for the translator.
func stubDeliver2() func(agent.AgentEvent) agent.AgentEvent {
	return func(ev agent.AgentEvent) agent.AgentEvent { return ev }
}

// truncate returns the first n bytes of s with an ellipsis if cut.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}
