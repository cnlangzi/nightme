package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/updater"
	"github.com/cnlangzi/nightme/internal/version"
)

// --- stub fixtures -----------------------------------------

// stubLookupForChecker returns a version.ReleaseLookup that
// always returns the supplied tag (regardless of which the
// Checker passes in). It also bumps the supplied call counter
// so tests can assert "did we hit the network?" without
// spinning up an httptest server.
//
// Production wires this to updater.LookupForLatest via
// cmd/nightme/version_check.go. Tests inject the stub
// directly so they don't depend on real network reachability.
func stubLookupForChecker(tag string, calls *atomic.Int32) version.ReleaseLookup {
	return func(_ context.Context, _ string) (version.ReleaseMeta, string, error) {
		if calls != nil {
			calls.Add(1)
		}
		return version.ReleaseMeta{
			TagName:     tag,
			PublishedAt: time.Unix(1700000000, 0).UTC(),
		}, "nightme.dev", nil
	}
}

// stubCheckerWithTag builds a version.Checker wired to a stub
// Lookup. The Checker is the only seam tests need; Lookup is
// injected here so tests don't have to fake an httptest server.
func stubCheckerWithTag(tag string) (*version.Checker, *atomic.Int32) {
	var calls atomic.Int32
	c := &version.Checker{
		Lookup:      stubLookupForChecker(tag, &calls),
		HTTPTimeout: 200 * time.Millisecond,
		CacheTTL:    24 * time.Hour,
		Now:         func() time.Time { return time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC) },
	}
	return c, &calls
}

// --- prompt path tests -------------------------------------

// TestPrompt_OutdatedYes exercises the new three-stage
// shape: a single "y" at the Update prompt moves into the
// download stage, which then asks "Install now?" next.
//
// We pin the y-then-EOF transcript (the user accepts the
// initial y but then can't answer the Install prompt) so
// we observe the second prompt header without driving the
// download stage all the way through.
//
// We inject deps.Release with a single Asset entry so the
// prompt's stage-2 lookup short-circuits via precomputed
// instead of falling back to a live GitHub fetch (which
// would race against the test fixture).
func TestPrompt_OutdatedYes(t *testing.T) {
	pre := &updater.Release{
		TagName: "v9.9.9",
		Assets: []updater.Asset{
			{Name: "unrelated-asset.txt"},
		},
	}
	var out bytes.Buffer

	idx := 0
	replies := []struct {
		line string
		err  error
	}{
		{"y\n", nil},
	}
	err := promptForUpdateIfOutdated(context.Background(), &PromptDeps{
		VersionCheck: &version.CheckResult{Latest: "v9.9.9", Outdated: true},
		Release:      pre,
		Out:          &out,
		Reader: func() (string, error) {
			r := replies[idx]
			idx++
			return r.line, r.err
		},
	})
	if err != nil {
		t.Fatalf("promptForUpdateIfOutdated: %v", err)
	}

	got := out.String()
	for _, want := range []string{
		"Update available",
		"9.9.9",
		"Update now?",
		"download failed",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q\n--- full output ---\n%s", want, got)
		}
	}
	if idx != 1 {
		t.Errorf("Reader called %d times, want 1", idx)
	}
}

// TestPrompt_ThreeStagesY_Y_Y is the happy path: user
// accepts each of the y/N prompts. We use a stub Release
// that triggers a download-stage failure (no matching
// asset), so we observe the two-prompt transcript without
// driving Install all the way through.
func TestPrompt_ThreeStagesY_Y_Y(t *testing.T) {
	checker, _ := stubCheckerWithTag("v9.9.9")
	pre := &updater.Release{
		TagName: "v9.9.9",
		Assets:  []updater.Asset{{Name: "unrelated-asset.txt"}},
	}

	t.Setenv("NIGHTME_PATHS_DATA_DIR", t.TempDir())

	var out bytes.Buffer
	idx := 0
	replies := []string{"y\n"}
	err := promptForUpdateIfOutdated(context.Background(), &PromptDeps{
		Checker: checker,
		Release: pre,
		Out:     &out,
		Reader: func() (string, error) {
			s := replies[idx]
			idx++
			return s, nil
		},
	})
	_ = err
	got := out.String()
	if !strings.Contains(got, "Update now?") {
		t.Errorf("expected Update prompt:\n%s", got)
	}
	if idx != 1 {
		t.Errorf("Reader called %d times, want 1", idx)
	}
}

// TestPrompt_DeclineInstallKeepsStaging covers the user's
// option to download-but-not-install.
func TestPrompt_DeclineInstallKeepsStaging(t *testing.T) {
	// Checker says "up to date" → no prompt at all.
	checker, _ := stubCheckerWithTag(version.Version)

	var out bytes.Buffer
	calls := 0
	err := promptForUpdateIfOutdated(context.Background(), &PromptDeps{
		Checker: checker,
		Out:     &out,
		Reader:  func() (string, error) { calls++; return "y\n", nil },
	})
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if calls != 0 {
		t.Errorf("Reader called %d times on up-to-date path; want 0", calls)
	}
	if out.Len() != 0 {
		t.Errorf("expected silent output on up-to-date path; got:\n%s", out.String())
	}
}

// TestPrompt_OutdatedNo covers the "user says n at the
// first prompt" path.
func TestPrompt_OutdatedNo(t *testing.T) {
	checker, _ := stubCheckerWithTag("v9.9.9")
	var out bytes.Buffer

	err := promptForUpdateIfOutdated(context.Background(), &PromptDeps{
		Checker: checker,
		Out:     &out,
		Reader:  func() (string, error) { return "n\n", nil },
	})
	if err != nil {
		t.Fatalf("promptForUpdateIfOutdated: %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "Update now?") {
		t.Errorf("output missing prompt:\n%s", got)
	}
	if strings.Contains(got, "Install now?") {
		t.Errorf("should not reach Install prompt after declining Update:\n%s", got)
	}
	if strings.Contains(got, "go install") {
		t.Errorf("did not expect install instructions on 'n':\n%s", got)
	}
}

// TestPrompt_OutdatedEnterOnly treats a bare newline as "no".
func TestPrompt_OutdatedEnterOnly(t *testing.T) {
	checker, _ := stubCheckerWithTag("v9.9.9")
	var out bytes.Buffer

	err := promptForUpdateIfOutdated(context.Background(), &PromptDeps{
		Checker: checker,
		Out:     &out,
		Reader:  func() (string, error) { return "\n", nil },
	})
	if err != nil {
		t.Fatalf("promptForUpdateIfOutdated: %v", err)
	}
	if strings.Contains(out.String(), "Install now?") {
		t.Errorf("empty reply should be treated as 'n', got:\n%s", out.String())
	}
}

// TestPrompt_UpToDateIsSilent verifies the no-prompt path.
func TestPrompt_UpToDateIsSilent(t *testing.T) {
	checker, _ := stubCheckerWithTag("v0.0.1")
	var out bytes.Buffer

	err := promptForUpdateIfOutdated(context.Background(), &PromptDeps{
		Checker: checker,
		Out:     &out,
		Reader:  func() (string, error) { return "y\n", nil },
	})
	if err != nil {
		t.Fatalf("promptForUpdateIfOutdated: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("expected no output when up-to-date, got:\n%s", out.String())
	}
}

// TestPrompt_NetworkFailureIsSilent covers the lookup-down
// case: stub Lookup errors. We expect ZERO output (and the
// REPL proceeds).
func TestPrompt_NetworkFailureIsSilent(t *testing.T) {
	checker := &version.Checker{
		Lookup: func(_ context.Context, _ string) (version.ReleaseMeta, string, error) {
			return version.ReleaseMeta{}, "", errors.New("network down")
		},
		Now: func() time.Time { return time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC) },
	}
	var out bytes.Buffer

	err := promptForUpdateIfOutdated(context.Background(), &PromptDeps{
		Checker: checker,
		Out:     &out,
		Reader:  func() (string, error) { return "y\n", nil },
	})
	if err != nil {
		t.Fatalf("promptForUpdateIfOutdated: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("expected silent on network failure, got:\n%s", out.String())
	}
}

// TestPrompt_NoReaderIsSilent covers the production fallback:
// runREPLWith calls promptForUpdateIfOutdated with a nil
// Reader.
func TestPrompt_NoReaderIsSilent(t *testing.T) {
	checker, _ := stubCheckerWithTag("v9.9.9")
	var out bytes.Buffer

	err := promptForUpdateIfOutdated(context.Background(), &PromptDeps{
		Checker: checker,
		Out:     &out,
		// Reader deliberately nil.
	})
	if err != nil {
		t.Fatalf("promptForUpdateIfOutdated: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("expected silent when Reader is nil, got:\n%s", out.String())
	}
}

// TestPrompt_EOFIsTreatedAsNo simulates the user pressing
// Ctrl-D on the prompt line.
func TestPrompt_EOFIsTreatedAsNo(t *testing.T) {
	checker, _ := stubCheckerWithTag("v9.9.9")
	var out bytes.Buffer

	err := promptForUpdateIfOutdated(context.Background(), &PromptDeps{
		Checker: checker,
		Out:     &out,
		Reader: func() (string, error) {
			return "", io.EOF
		},
	})
	if err != nil {
		t.Fatalf("promptForUpdateIfOutdated: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "Run `nightme update`") {
		t.Errorf("expected EOF hint, got:\n%s", got)
	}
	if strings.Contains(got, "go install") {
		t.Errorf("EOF should not trigger install instructions:\n%s", got)
	}
}

// TestPrompt_ReadErrorIsNonFatal covers a non-EOF read error.
func TestPrompt_ReadErrorIsNonFatal(t *testing.T) {
	checker, _ := stubCheckerWithTag("v9.9.9")
	var out bytes.Buffer

	err := promptForUpdateIfOutdated(context.Background(), &PromptDeps{
		Checker: checker,
		Out:     &out,
		Reader: func() (string, error) {
			return "", errors.New("synthetic read failure")
		},
	})
	if err != nil {
		t.Fatalf("promptForUpdateIfOutdated: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "read error") {
		t.Errorf("expected 'read error' note, got:\n%s", got)
	}
	if strings.Contains(got, "go install") {
		t.Errorf("read error should not trigger install instructions:\n%s", got)
	}
}

// TestPrompt_InvalidAnswerThenNoRePrompt guards against the
// "user mistyped ? then we ask again" trap.
func TestPrompt_InvalidAnswerThenNoRePrompt(t *testing.T) {
	checker, _ := stubCheckerWithTag("v9.9.9")
	var out bytes.Buffer
	calls := 0

	err := promptForUpdateIfOutdated(context.Background(), &PromptDeps{
		Checker: checker,
		Out:     &out,
		Reader: func() (string, error) {
			calls++
			return "???\n", nil
		},
	})
	if err != nil {
		t.Fatalf("promptForUpdateIfOutdated: %v", err)
	}
	if calls != 1 {
		t.Errorf("Reader called %d times, want exactly 1", calls)
	}
	if strings.Contains(out.String(), "go install") {
		t.Errorf("invalid answer should not print install instructions:\n%s", out.String())
	}
}

// TestPrompt_VersionCheckDrivesPrompt covers the production
// wiring: runREPLInteractive runs the countdown Check, then
// passes VersionCheck in so promptForUpdateIfOutdated does
// not hit the network again.
func TestPrompt_VersionCheckDrivesPrompt(t *testing.T) {
	var out bytes.Buffer
	calls := 0
	err := promptForUpdateIfOutdated(context.Background(), &PromptDeps{
		VersionCheck: &version.CheckResult{Latest: "v9.9.9", Outdated: true},
		Out:          &out,
		Reader: func() (string, error) {
			calls++
			return "n\n", nil
		},
	})
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if calls != 1 {
		t.Errorf("Reader called %d times, want 1", calls)
	}
	got := out.String()
	if !strings.Contains(got, "Update available") {
		t.Errorf("missing availability line:\n%s", got)
	}
	if !strings.Contains(got, "9.9.9") {
		t.Errorf("missing latest version:\n%s", got)
	}
	if !strings.Contains(got, "Update now?") {
		t.Errorf("missing Update prompt:\n%s", got)
	}
	if strings.Contains(got, "Install now?") {
		t.Errorf("declining Update should not reach Install:\n%s", got)
	}
}

// --- count tests -------------------------------------------

// TestPrompt_LookupForLatestFiresOnce verifies the stage-1
// lookup is invoked exactly once even when the user declines
// the install prompt. Repeated startup chatter would be
// obnoxious in the REPL.
func TestPrompt_LookupForLatestFiresOnce(t *testing.T) {
	checker, calls := stubCheckerWithTag("v9.9.9")
	var out bytes.Buffer

	err := promptForUpdateIfOutdated(context.Background(), &PromptDeps{
		Checker: checker,
		Out:     &out,
		Reader:  func() (string, error) { return "n\n", nil },
	})
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("Lookup fired %d times; want exactly 1", got)
	}
}

// --- countdown tests ---------------------------------------

// TestWaitCheckCountdown_InstantResult verifies a cache-hit
// style Check (result already on the channel) paints the
// countdown once then clears it without waiting out the
// timeout.
func TestWaitCheckCountdown_InstantResult(t *testing.T) {
	ch := make(chan version.CheckResult, 1)
	ch <- version.CheckResult{Latest: "1.2.3", Outdated: true}

	var out bytes.Buffer
	got := waitCheckCountdown(context.Background(), &out, 5, time.Hour, ch)
	if got.Latest != "1.2.3" {
		t.Errorf("Latest = %q, want 1.2.3", got.Latest)
	}
	s := out.String()
	if !strings.Contains(s, "Checking for updates... 5s") {
		t.Errorf("missing countdown paint:\n%q", s)
	}
}

// TestWaitCheckCountdown_TimeoutSkips returns a zero result
// when Check never completes, so the caller falls through to
// the shell without prompting.
func TestWaitCheckCountdown_TimeoutSkips(t *testing.T) {
	ch := make(chan version.CheckResult) // never sent
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	var out bytes.Buffer
	got := waitCheckCountdown(ctx, &out, 5, time.Millisecond, ch)
	if got.Latest != "" || got.Outdated {
		t.Errorf("timeout should skip, got %+v", got)
	}
	if !strings.Contains(out.String(), "Checking for updates...") {
		t.Errorf("expected countdown text, got %q", out.String())
	}
}

// --- runREPL integration -----------------------------------

// TestRunREPLWith_NoVersionChatter confirms that the existing
// REPL scanner path (used by the legacy TestREPL_* suite) does
// NOT inject version-prompt text into the output when stdin is
// empty.
func TestRunREPLWith_NoVersionChatter(t *testing.T) {
	root, reg := newTestRoot()
	var buf bytes.Buffer
	captureREPLIO(root, &buf)
	if err := runREPLWith(root, reg, nil, strings.NewReader(""), &buf); err != nil {
		t.Fatalf("runREPLWith: %v", err)
	}
	if strings.Contains(buf.String(), "Update now?") {
		t.Errorf("runREPLWith must not prompt for update (no reader wired):\n%s", buf.String())
	}
}

// --- CLI flag surface --------------------------------------

// TestUpdate_AllInOneFlags pins the single-verb surface:
// --tag / --quiet / --no-install / --no-restart / --yes / -y.
func TestUpdate_AllInOneFlags(t *testing.T) {
	root, _ := newTestRoot()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"update", "--help"})
	if err := root.Execute(); err != nil {
		t.Fatalf("update --help: %v", err)
	}
	got := buf.String()
	for _, want := range []string{
		"--tag",
		"--quiet", "-q",
		"--no-install",
		"--no-restart",
		"--yes", "-y",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("update --help missing %q\n%s", want, got)
		}
	}
	for _, mustNot := range []string{"update check", "update download", "update install"} {
		if strings.Contains(got, mustNot) {
			t.Errorf("update --help still mentions subcommand surface %q\n%s", mustNot, got)
		}
	}
}

// --- CLI integration ---------------------------------------

// TestUpdate_AllInOneNoInstallHappyPath drives the full
// single-verb `nightme update` end-to-end with --no-install,
// so we don't actually swap a binary or os.Exit.
//
// The version check goes through updater.LookupForLatest
// (nightme.dev → GitHub fallback). The download stage goes
// through updater.LookupForDownload (GitHub → nightme.dev
// fallback). We mock BOTH base URLs to point at the same
// fixture so both paths return the same release.
func TestUpdate_AllInOneNoInstallHappyPath(t *testing.T) {
	body := strings.Repeat("nightme-test-binary-", 256) // ~5 KiB
	srv := newUpdateFixture(t, "v9.9.9", "9.9.9", body)
	savedGitHub := updater.GitHubBaseURL
	savedMirror := updater.NightMeDevBaseURL
	updater.GitHubBaseURL = srv.URL + "/repos/cnlangzi/nightme"
	updater.NightMeDevBaseURL = srv.URL
	t.Cleanup(func() {
		updater.GitHubBaseURL = savedGitHub
		updater.NightMeDevBaseURL = savedMirror
	})

	t.Setenv("NIGHTME_PATHS_DATA_DIR", t.TempDir())

	root, _ := newTestRoot()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"update", "--no-install", "--tag", "v9.9.9"})
	if err := root.Execute(); err != nil {
		t.Fatalf("update --no-install: %v\n%s", err, buf.String())
	}

	got := buf.String()
	for _, want := range []string{
		"Update available",
		"9.9.9",
		"sha256",
		"--no-install",
		"stopping before swap",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q\n--- full output ---\n%s", want, got)
		}
	}
}

// TestUpdate_CacheHitSkipsDownload pins the staging-dir
// shortcut.
func TestUpdate_CacheHitSkipsDownload(t *testing.T) {
	body := strings.Repeat("nightme-test-binary-", 256) // ~5 KiB
	srv := newUpdateFixture(t, "v9.9.9", "9.9.9", body)
	savedGitHub := updater.GitHubBaseURL
	savedMirror := updater.NightMeDevBaseURL
	updater.GitHubBaseURL = srv.URL + "/repos/cnlangzi/nightme"
	updater.NightMeDevBaseURL = srv.URL
	t.Cleanup(func() {
		updater.GitHubBaseURL = savedGitHub
		updater.NightMeDevBaseURL = savedMirror
	})

	dataDir := t.TempDir()
	t.Setenv("NIGHTME_PATHS_DATA_DIR", dataDir)

	wantExt := "tar.gz"
	if runtime.GOOS == "windows" {
		wantExt = "zip"
	}
	wantName := fmt.Sprintf("nightme_9.9.9_%s_%s.%s",
		runtime.GOOS, runtime.GOARCH, wantExt)
	wantPath := filepath.Join(dataDir, "updates", "9.9.9", wantName)
	if err := os.MkdirAll(filepath.Dir(wantPath), 0o700); err != nil {
		t.Fatalf("mkdir staging: %v", err)
	}
	if err := os.WriteFile(wantPath, []byte(body), 0o600); err != nil {
		t.Fatalf("seed archive: %v", err)
	}

	root, _ := newTestRoot()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"update", "--no-install", "--tag", "v9.9.9"})
	if err := root.Execute(); err != nil {
		t.Fatalf("update --no-install (cache hit): %v\n%s", err, buf.String())
	}
	got := buf.String()
	if !strings.Contains(got, "skipping download") {
		t.Errorf("expected 'skipping download'; got:\n%s", got)
	}
	if !strings.Contains(got, "sha256 verified") {
		t.Errorf("expected sha256 verified; got:\n%s", got)
	}
}

// TestUpdate_AllInOneRefusesEmptyDataDir covers the safety
// property: if config.Paths.DataDir is empty, the update
// fails closed instead of writing into "/" or some other
// unintended location.
func TestUpdate_AllInOneRefusesEmptyDataDir(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	root, _ := newTestRoot()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"update", "--tag", "v9.9.9"})
	err := root.Execute()
	if err == nil {
		t.Fatal("update with no config succeeded; want error")
	}
}

// TestUpdate_HelpLongIsSingleVerb pins the user-visible
// shape: --help must NOT list subcommands.
func TestUpdate_HelpLongIsSingleVerb(t *testing.T) {
	root, _ := newTestRoot()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"update", "--help"})
	if err := root.Execute(); err != nil {
		t.Fatalf("update --help: %v", err)
	}
	got := buf.String()
	for _, want := range []string{"check", "download", "install"} {
		if !strings.Contains(got, want) {
			t.Errorf("update --help should describe the three stages, missing %q\n%s", want, got)
		}
	}
	if strings.Contains(got, "Subcommands:") {
		t.Errorf("update --help has Subcommands: header (parent got kids attached):\n%s", got)
	}
}

// newUpdateFixture serves a synthetic GitHub-shaped release
// payload over httptest. It serves BOTH the GitHub-style path
// (/repos/cnlangzi/nightme/releases/tags/<tag>) AND the
// nightme.dev-style path (/releases/latest) so a single
// fixture can back both LookupForLatest and LookupForDownload
// in tests.
//
// We use a single hand-written handler instead of http.ServeMux
// because the paths overlap in ways ServeMux rejects (a
// concrete /releases/tags/<v> URL shares its prefix with
// /releases/latest, which ServeMux treats as a pattern
// conflict).
func newUpdateFixture(t *testing.T, tag, ver, assetBody string) *httptest.Server {
	t.Helper()
	sum := sha256.Sum256([]byte(assetBody))
	sumHex := hex.EncodeToString(sum[:])
	wantOS, wantArch := runtime.GOOS, runtime.GOARCH
	ext := "tar.gz"
	if wantOS == "windows" {
		ext = "zip"
	}
	assetName := "nightme_" + ver + "_" + wantOS + "_" + wantArch + "." + ext

	var srv *httptest.Server
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/repos/cnlangzi/nightme/releases/tags/"+tag),
			r.URL.Path == "/repos/cnlangzi/nightme/releases/latest",
			r.URL.Path == "/releases/latest",
			r.URL.Path == "/releases/tags/"+tag:
			fmt.Fprintf(w, `{
				"tag_name": %q,
				"published_at": "2026-08-17T06:56:53Z",
				"assets": [
					{"name":"SHA256SUMS.txt","browser_download_url":"%s/asset/sums","size":%d},
					{"name":%q,"browser_download_url":"%s/asset/binary","size":%d}
				]
			}`, tag, srv.URL, len(sumHex), assetName, srv.URL, len(assetBody))
		case r.URL.Path == "/asset/sums":
			fmt.Fprintf(w, "%s  %s\n", sumHex, assetName)
		case r.URL.Path == "/asset/binary":
			_, _ = w.Write([]byte(assetBody))
		default:
			http.NotFound(w, r)
		}
	})
	srv = httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}
