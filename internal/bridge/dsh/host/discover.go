// discover.go — probe an existing dsh web instance.
//
// When nightme starts, it checks whether a `dsh --profile web` is
// already running on the default port (3080). If yes, nightme
// attaches to it as just another client — no new subprocess is
// spawned, the user's browser session keeps working, and the
// existing dsh's session log stays authoritative. If no, nightme
// spawns a fresh dsh itself (the legacy path).
//
// Why this matters: users often keep `dsh web` open in their browser
// to manage the same sessions the chat bridge drives. Spawning a
// second dsh on a different port would split their sessions across
// two instances; reusing the existing one keeps everything unified.

package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/cnlangzi/nightme/internal/httpclient"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// defaultDSHPort is the port both `dsh web` (browser) and our shared
// host default to. Discovery probes this single port — if the user's
// dsh runs on a non-default port, nightme falls back to spawning its
// own (the legacy path).
const defaultDSHPort = 3080

// ErrNotRunning means the probe found nothing on the target port.
// Not a hard error — StartSharedHost uses this to decide whether
// to spawn a fresh dsh.
var ErrNotRunning = errors.New("dsh.host: nothing listening on the target port")

// ErrNotDSH means something IS listening on the target port but it
// doesn't speak the dsh wire protocol (it might be a different web
// service). StartSharedHost surfaces this so the user sees a clear
// error instead of mysterious RPC failures.
var ErrNotDSH = errors.New("dsh.host: target port responds but doesn't look like dsh")

// probeTimeout bounds the whole discovery attempt (TCP dial +
// probe RPC). Real-machine dial is sub-millisecond on loopback;
// 2s leaves slack for slow CI without holding up boot on a
// legitimate "nothing here" answer.
const probeTimeout = 2 * time.Second

// DiscoverExisting probes the given port for an already-running dsh
// web instance. On success it returns a *Client rooted at the
// discovered URL — ready to subscribe / RPC / etc. — without owning
// any subprocess. The caller is responsible for closing the Client
// when done; the dsh subprocess is the user's and we do NOT signal it.
//
// Failure modes:
//   - nothing listening → ErrNotRunning (caller should spawn)
//   - TCP connects but protocol probe fails → ErrNotDSH (caller should
//     surface; spawning on top of another web service is a footgun)
//
// The probe uses POST /api/host.describe — a real dsh method (see
// dsh-api.md §2.3.1) that's safe to call on any session-less /api.
// We send a fresh clientRequest with a probe-specific rpcId; the
// response's rpcId echoes back, validating it's actually dsh and
// not just any HTTP server.
func DiscoverExisting(ctx context.Context, port int) (*Client, error) {
	if port <= 0 || port > 65535 {
		return nil, fmt.Errorf("dsh.host: discover: invalid port %d", port)
	}

	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	baseURL, err := probeLive(probeCtx, port)
	if err != nil {
		return nil, err
	}

	// Probe RPC. probeDSHManifest takes baseURL (not *Client) because
	// it bypasses Client.Post's envelope-wrapping — the probe only
	// needs the HTTP transport.
	if err := probeDSHManifest(probeCtx, baseURL); err != nil {
		return nil, err
	}

	// Probe succeeded — build the full Client (Hub + Router) at
	// the discovered URL. The hub's WS pumps don't connect
	// synchronously (they retry with backoff) so this is cheap.
	full := New(baseURL, nil)
	return full, nil
}

// probeLive does the TCP-level dial + builds the candidate URL.
// Returns ErrNotRunning if nothing is bound, or a wrapped error.
func probeLive(ctx context.Context, port int) (string, error) {
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	d := net.Dialer{}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		// Connection refused / timeout / etc → not running.
		return "", fmt.Errorf("%w: %v", ErrNotRunning, err)
	}
	_ = conn.Close()

	u := &url.URL{
		Scheme: "http",
		Host:   net.JoinHostPort("127.0.0.1", strconv.Itoa(port)),
	}
	return u.String(), nil
}

// probeDSHManifest hits the web app manifest at /manifest.webmanifest
// and verifies the body carries dsh-specific identity strings.
//
// Why this endpoint (vs an RPC like host.describe):
//   - Unauthenticated. dsh serves this path without the dsh-auth
//     cookie, so the probe doesn't need a token from the user's
//     dashboard session.
//   - HTTP GET, no envelope. No JSON-RPC body to construct, no
//     rpcId echo to verify.
//   - Stable identity. The manifest always carries `"name":"DeepSeek
//     Harness"` + `"short_name":"DSH"` — these are the dsh product
//     strings, unique to this server, not coincidentally mockable
//     by another web service on the same port.
//
// Verified 2026-09-10 against dsh 0.1.2-rc.1 (returns 200 with the
// expected body). The previous RPC probe (host.describe) was a
// regression risk because it 404s on this dsh release.
func probeDSHManifest(ctx context.Context, baseURL string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		baseURL+"/manifest.webmanifest", nil)
	if err != nil {
		return errors.Join(ErrNotDSH, fmt.Errorf("build req: %w", err))
	}

	httpClient := httpclient.DefaultWithTimeout(probeTimeout)
	resp, err := httpClient.Do(req)
	if err != nil {
		return errors.Join(ErrNotDSH, fmt.Errorf("do: %w", err))
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return errors.Join(ErrNotDSH, fmt.Errorf("HTTP %d", resp.StatusCode))
	}
	// Browsers look for application/manifest+json; dsh honors that.
	// Reject anything else (e.g. text/html from a generic 404 page)
	// to keep the fingerprint tight.
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/manifest+json") {
		return errors.Join(ErrNotDSH, fmt.Errorf("content-type=%q", ct))
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return errors.Join(ErrNotDSH, fmt.Errorf("read body: %w", err))
	}

	var manifest struct {
		Name      string `json:"name"`
		ShortName string `json:"short_name"`
	}
	if err := json.Unmarshal(body, &manifest); err != nil {
		return errors.Join(ErrNotDSH, fmt.Errorf("decode: %w", err))
	}
	if manifest.Name != "DeepSeek Harness" {
		return errors.Join(ErrNotDSH, fmt.Errorf("name=%q (want \"DeepSeek Harness\")", manifest.Name))
	}
	if manifest.ShortName != "DSH" {
		return errors.Join(ErrNotDSH, fmt.Errorf("short_name=%q (want \"DSH\")", manifest.ShortName))
	}
	return nil
}
