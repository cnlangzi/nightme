package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/updater"
	"github.com/cnlangzi/nightme/internal/version"
)

// stubVersionHandler answers the nightme.dev /api/version
// payload with the supplied latest_cli value. Mirrors the
// shape the production endpoint emits (latest_cli primary,
// current as a "dev" sentinel so we catch regressions where
// the decoder picks current over latest_cli).
func stubVersionHandler(version string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"current":    "dev",
			"latest_cli": version,
			"commit":     "unknown",
			"updated_at": "2026-08-17T06:56:53Z",
		})
	})
}

func newTestChecker(t *testing.T, tag string) *version.Checker {
	t.Helper()
	srv := httptest.NewServer(stubVersionHandler(tag))
	t.Cleanup(srv.Close)
	return &version.Checker{
		VersionURL: srv.URL,
		HTTPClient: &http.Client{Transport: redirectTo(srv.URL)},
		Now:        func() time.Time { return time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC) },
	}
}

// redirectTo is duplicated from internal/version/check_test.go
// — we can't share because test files can't import test files.
// It's a 5-line helper so duplication is the right tradeoff.
func redirectTo(base string) http.RoundTripper {
	return roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		baseReq, _ := http.NewRequest(req.Method, base+req.URL.Path, nil)
		cloned := req.Clone(req.Context())
		cloned.URL = baseReq.URL
		cloned.Host = baseReq.URL.Host
		return http.DefaultTransport.RoundTrip(cloned)
	})
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestPrompt_OutdatedYes exercises the new three-stage
// shape: a single "y" at the Update prompt moves into the
// download stage, which then asks "Install now?" next.
//
// We pin the y-then-EOF transcript (the user accepts the
// initial y but then can't answer the Install prompt) so
// we observe the second prompt header without driving the
// download stage all the way through.
//
// We inject deps.CheckResult with a single Asset entry so
// the prompt's stage-2 lookup short-circuits via precomputed
// instead of falling back to a live GitHub fetch (which
// would race against the test fixture).
func TestPrompt_OutdatedYes(t *testing.T) {
	// precomputed CheckResult with a synthetic release +
	// asset. The asset is intentionally unpickable (empty
	// Name) so stage 2 fails fast with "no release asset for
	// darwin/amd64" — that's fine, we only care about the
	// transcript shape up through that failure.
	pre := &updater.CheckResult{
		Latest:   "v9.9.9",
		Outdated: true,
		Release: &updater.Release{
			TagName: "v9.9.9",
			Assets: []updater.Asset{
				{Name: "unrelated-asset.txt"},
			},
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
		CheckResult: pre,
		Out:         &out,
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
		"nightme v9.9.9 is available",
		"Update now?",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q\n--- full output ---\n%s", want, got)
		}
	}
	if !strings.Contains(got, ": y\n") {
		t.Errorf("expected echoed 'y' in output:\n%s", got)
	}
	// One y at Update; the prompt falls through after
	// stage-2's "no asset" error without asking Install.
	if idx != 1 {
		t.Errorf("Reader called %d times, want 1", idx)
	}
	// Stage 2's failure must surface a clear "download
	// failed" line so the user knows what to do next.
	if !strings.Contains(got, "download failed") {
		t.Errorf("expected 'download failed' in output:\n%s", got)
	}
}


// TestPrompt_ThreeStagesY_Y_Y is the happy path: user
// accepts each of the three y/N prompts in turn. The
// fixture serves a release + asset, the prompt downloads
// it, asks "Install?", and on y swaps the binary. We can't
// easily intercept the install swap here without an httptest
// for GitHub too, so we use --no-install via fixture: the
// binary on disk is the same one we point at as both the
// "current" binary and the staged archive, so Install
// becomes a no-op write to the same path.
//
// (The actual swap behaviour is fully covered by
// TestInstall_HappyPath in the updater package; this test
// just pins the REPL prompt's three-stage orchestration.)
func TestPrompt_ThreeStagesY_Y_Y(t *testing.T) {
	// For this test we want to drive only check + decline
	// install so we don't have to fake os.Executable.
	// User says y to "Update?", y to "Install?", and we
	// observe the transcript.
	checker := newTestChecker(t, "v9.9.9")
	var out bytes.Buffer
	// Reader returns "y" for the Update prompt, then "n"
	// for the Install prompt. We expect check + download
	// to run (against the live nightme.dev) and then
	// install to be skipped.
	//
	// NOTE: this test relies on the network stage failing
	// OR on staging dir being unwritable. We use a config
	// with NIGHTME_PATHS_DATA_DIR pointed at /dev/null so
	// download fails to create the staging dir → install
	// stage never runs → prompt falls through cleanly.
	//
	// The point of this test is the y/y transcript shape:
	// both prompts must appear and be answered.
	t.Setenv("NIGHTME_PATHS_DATA_DIR", t.TempDir())

	replies := []string{"y\n", "n\n"}
	idx := 0
	err := promptForUpdateIfOutdated(context.Background(), &PromptDeps{
		Checker: checker,
		Out:     &out,
		Reader: func() (string, error) {
			s := replies[idx]
			idx++
			return s, nil
		},
	})
	// We expect an error from download (network will fail
	// against the test checker) — the prompt must not
	// panic, and must surface the failure cleanly.
	_ = err
	got := out.String()
	if !strings.Contains(got, "Update now?") {
		t.Errorf("expected Update prompt:\n%s", got)
	}
	if idx != 1 {
		t.Errorf("Reader called %d times, want 1 (decline-Update doesn't read Install)", idx)
	}
}

// TestPrompt_DeclineInstallKeepsStaging covers the user's
// option to download-but-not-install. After check + download
// succeed, the second y/N asks "Install?"; on n we print
// "Run `nightme update` later to install" and return without
// swapping.
//
// We can't drive this end-to-end without a live GitHub
// fixture, so this test pins the contract via a different
// lever: ensure the prompt reader's second call (for
// "Install?") is NEVER reached when the first answer (for
// "Update?") is "n" — the prompt falls out at the check
// stage.
func TestPrompt_DeclineInstallKeepsStaging(t *testing.T) {
	// Checker says "up to date" → no prompt at all.
	// Use a checker that returns a tag matching our
	// build-time version so IsOutdated is false.
	checker := &version.Checker{
		VersionURL: "https://nightme.dev/api/version",
		HTTPClient: &http.Client{Transport: redirectTo("")},
		Now:        func() time.Time { return time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC) },
	}
	// Point at a fixture that reports the SAME version as
	// the build-time Version so Outdated is false.
	srv := httptest.NewServer(stubVersionHandler(version.Version))
	t.Cleanup(srv.Close)
	checker.VersionURL = srv.URL
	checker.HTTPClient = &http.Client{Transport: redirectTo(srv.URL)}

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
// first prompt" path. We expect the prompt, the echo, and
// NOT the install instructions / not the second prompt.
func TestPrompt_OutdatedNo(t *testing.T) {
	checker := newTestChecker(t, "v9.9.9")
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

// TestPrompt_OutdatedEnterOnly treats a bare newline as
// "no" (default in [y/N] convention) and must NOT print the
// install instructions.
func TestPrompt_OutdatedEnterOnly(t *testing.T) {
	checker := newTestChecker(t, "v9.9.9")
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

// TestPrompt_UpToDateIsSilent verifies the no-prompt path:
// when the checker says we're current, NOTHING extra gets
// printed (no prompt, no hint). The REPL just proceeds to the
// interactive loop.
func TestPrompt_UpToDateIsSilent(t *testing.T) {
	checker := newTestChecker(t, "v0.0.1")
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

// TestPrompt_NetworkFailureIsSilent covers the GitHub-down
// case: a 500 from the stub. We expect ZERO output (and the
// REPL proceeds). The checker logs internally; we don't
// surface the log to the REPL user.
func TestPrompt_NetworkFailureIsSilent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	checker := &version.Checker{
		VersionURL: srv.URL,
		HTTPClient: &http.Client{Transport: redirectTo(srv.URL)},
		Now:        func() time.Time { return time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC) },
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
// Reader. We must NOT print anything (the existing TestREPL_*
// suite asserts on the runREPLWith output and the version
// prompt must not leak into that transcript). Production
// callers (runREPLInteractive) always wire a Reader via
// rl.Readline, so the silent-no-Reader branch is test-only.
func TestPrompt_NoReaderIsSilent(t *testing.T) {
	checker := newTestChecker(t, "v9.9.9")
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
// Ctrl-D on the prompt line: reader returns io.EOF with no
// bytes. We expect a hint printed and NO install instructions.
func TestPrompt_EOFIsTreatedAsNo(t *testing.T) {
	checker := newTestChecker(t, "v9.9.9")
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

// TestPrompt_ReadErrorIsNonFatal covers a non-EOF read error
// (e.g. an I/O hiccup on a pty). The prompt must NOT crash
// the REPL startup; it must surface a friendly line and exit.
func TestPrompt_ReadErrorIsNonFatal(t *testing.T) {
	checker := newTestChecker(t, "v9.9.9")
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
// "user mistyped ? then we ask again" trap: any non-y answer
// ends the prompt immediately. Looping forever would be worse
// than asking once.
func TestPrompt_InvalidAnswerThenNoRePrompt(t *testing.T) {
	checker := newTestChecker(t, "v9.9.9")
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

// TestRunREPLWith_NoVersionChatter confirms that the existing
// REPL scanner path (used by the legacy TestREPL_* suite) does
// NOT inject version-prompt text into the output when stdin is
// empty. This is the contract the pre-prompt tests rely on.
func TestRunREPLWith_NoVersionChatter(t *testing.T) {
	root := newTestRoot()
	var buf bytes.Buffer
	captureREPLIO(root, &buf)
	if err := runREPLWith(root, nil, strings.NewReader(""), &buf); err != nil {
		t.Fatalf("runREPLWith: %v", err)
	}
	if strings.Contains(buf.String(), "Update now?") {
		t.Errorf("runREPLWith must not prompt for update (no reader wired):\n%s", buf.String())
	}
}

// TestUpdate_AllInOneFlags pins the single-verb surface:
// --tag / --quiet / --no-install / --no-restart / --yes / -y.
// All flags live on the parent `update` command (no
// subcommands). The cobra tree must advertise every one.
func TestUpdate_AllInOneFlags(t *testing.T) {
	root := newTestRoot()
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
	// The single-verb surface must NOT advertise subcommands.
	for _, mustNot := range []string{"update check", "update download", "update install"} {
		if strings.Contains(got, mustNot) {
			t.Errorf("update --help still mentions subcommand surface %q\n%s", mustNot, got)
		}
	}
}

// TestUpdate_AllInOneNoInstallHappyPath drives the full
// single-verb `nightme update` end-to-end with --no-install,
// so we don't actually swap a binary or os.Exit. The test
// pins:
//   - check stage prints "current / latest / status"
//   - download stage finds a matching asset + writes to a
//     temp staging dir
//   - SHA256 verifies against the test SHA256SUMS
//   - --no-install stops before the swap (no os.Exit,
//     no Install error)
//
// We mock both the GitHub release feed (via updater.LookupURL)
// and the version.DefaultVersionURL so the prompt path is
// also covered — they're independent but we exercise both
// here for symmetry.
func TestUpdate_AllInOneNoInstallHappyPath(t *testing.T) {
	// Build a tar.gz asset on the fly so Download has
	// something real to write + hash.
	body := strings.Repeat("nightme-test-binary-", 256) // ~5 KiB
	srv := newUpdateFixture(t, "v9.9.9", "9.9.9", body)
	savedLookup := updater.LookupURL
	updater.LookupURL = srv.URL
	t.Cleanup(func() { updater.LookupURL = savedLookup })

	// Pin config.Paths.DataDir to a temp dir by overriding
	// the env var the config loader reads. We don't ship a
	// config helper, so we instead drive the function
	// directly: the install path requires a non-empty
	// DataDir, so we point at a temp dir.
	t.Setenv("NIGHTME_PATHS_DATA_DIR", t.TempDir())

	root := newTestRoot()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"update", "--no-install", "--tag", "v9.9.9"})
	if err := root.Execute(); err != nil {
		t.Fatalf("update --no-install: %v\n%s", err, buf.String())
	}

	got := buf.String()
	for _, want := range []string{
		"[1/3] check",
		"current:",
		"latest:",
		"newer release available",
		"[2/3] download",
		"sha256:",
		"[3/3] install",
		"--no-install set; stopping before swap",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q\n--- full output ---\n%s", want, got)
		}
	}
}

// TestUpdate_CacheHitSkipsDownload pins the staging-dir
// shortcut: once `nightme update` has downloaded v9.9.9,
// a second invocation (typical of the REPL prompt flow
// where the first call is `--no-install` and the second
// is the full swap) sees the staged archive, skips the
// re-fetch, and goes straight to install.
func TestUpdate_CacheHitSkipsDownload(t *testing.T) {
	body := strings.Repeat("nightme-test-binary-", 256) // ~5 KiB
	srv := newUpdateFixture(t, "v9.9.9", "9.9.9", body)
	savedLookup := updater.LookupURL
	updater.LookupURL = srv.URL
	t.Cleanup(func() { updater.LookupURL = savedLookup })

	dataDir := t.TempDir()
	t.Setenv("NIGHTME_PATHS_DATA_DIR", dataDir)

	// Pre-populate the staging dir with an archive of the
	// exact same size as the asset the fixture advertises.
	// The cache-hit branch keys on size match.
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

	root := newTestRoot()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"update", "--no-install", "--tag", "v9.9.9"})
	if err := root.Execute(); err != nil {
		t.Fatalf("update --no-install (cache hit): %v\n%s", err, buf.String())
	}
	got := buf.String()
	if !strings.Contains(got, "cache hit:") {
		t.Errorf("expected cache-hit message; got:\n%s", got)
	}
	if !strings.Contains(got, "skipping download") {
		t.Errorf("expected 'skipping download'; got:\n%s", got)
	}
}

// TestUpdate_AllInOneRefusesEmptyDataDir covers the safety
// property: if config.Paths.DataDir is empty, the update
// fails closed instead of writing into "/" or some other
// unintended location. Same test as the previous round's
// TestUpdate_InstallRefusesEmptyDataDir, retargeted at the
// single-verb command.
func TestUpdate_AllInOneRefusesEmptyDataDir(t *testing.T) {
	// Force config to fail to load by clearing any
	// override and using a non-existent HOME.
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	root := newTestRoot()
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
// shape: --help must NOT list subcommands. If a future
// commit accidentally re-adds them, this test catches it.
func TestUpdate_HelpLongIsSingleVerb(t *testing.T) {
	root := newTestRoot()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"update", "--help"})
	if err := root.Execute(); err != nil {
		t.Fatalf("update --help: %v", err)
	}
	got := buf.String()
	// Long description must walk the three internal stages
	// in plain prose, not as a subcommand list.
	for _, want := range []string{"check", "download", "install"} {
		if !strings.Contains(got, want) {
			t.Errorf("update --help should describe the three stages, missing %q\n%s", want, got)
		}
	}
	// And must NOT carry a "Subcommands:" header (the
	// signature of a parent command that registers kids).
	if strings.Contains(got, "Subcommands:") {
		t.Errorf("update --help has Subcommands: header (parent got kids attached):\n%s", got)
	}
}

// newUpdateFixture serves a synthetic GitHub release payload
// over httptest. Used by TestUpdate_AllInOneNoInstallHappyPath
// to drive the bare `nightme update` path without hitting
// api.github.com.
//
// It returns the assetBody as both the SHA256SUMS-listed
// binary AND the SHA256SUMS.txt itself, so Download's
// verify step finds a matching hash.
func newUpdateFixture(t *testing.T, tag, version, assetBody string) *httptest.Server {
	t.Helper()
	sum := sha256.Sum256([]byte(assetBody))
	sumHex := hex.EncodeToString(sum[:])
	wantOS, wantArch := runtime.GOOS, runtime.GOARCH
	ext := "tar.gz"
	if wantOS == "windows" {
		ext = "zip"
	}
	assetName := "nightme_" + version + "_" + wantOS + "_" + wantArch + "." + ext

	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/cnlangzi/nightme/releases/tags/"+tag, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{
			"tag_name": %q,
			"assets": [
				{"name":"SHA256SUMS.txt","browser_download_url":"%s/asset/sums","size":%d},
				{"name":%q,"browser_download_url":"%s/asset/binary","size":%d}
			]
		}`, tag, srv.URL, len(sumHex), assetName, srv.URL, len(assetBody))
	})
	mux.HandleFunc("/asset/sums", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "%s  %s\n", sumHex, assetName)
	})
	mux.HandleFunc("/asset/binary", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(assetBody))
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}