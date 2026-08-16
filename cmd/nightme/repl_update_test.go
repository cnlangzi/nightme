package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

// TestPrompt_OutdatedYes exercises the full happy path:
//   1. Checker says "newer than me"
//   2. prompt prints the question
//   3. Reader returns "y\n"
//   4. prompt echoes "y", then invokes deps.OnYes (which is
//      wired to the `nightme update check` subcommand in
//      production). With OnYes nil the prompt falls back to
//      printing the manual install hint — same as the
//      pre-subcommand-split behaviour — so the legacy contract
//      holds.
func TestPrompt_OutdatedYes(t *testing.T) {
	checker := newTestChecker(t, "v9.9.9")
	var out bytes.Buffer

	err := promptForUpdateIfOutdated(context.Background(), &PromptDeps{
		Checker: checker,
		Out:     &out,
		Reader:  func() (string, error) { return "y\n", nil },
		// OnYes intentionally nil → fallback to inline
		// install hint.
	})
	if err != nil {
		t.Fatalf("promptForUpdateIfOutdated: %v", err)
	}

	got := out.String()
	for _, want := range []string{
		"nightme 9.9.9 is available",
		"Update now?",
		"go install github.com/cnlangzi/nightme/cmd/nightme@latest",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q\n--- full output ---\n%s", want, got)
		}
	}
	if !strings.Contains(got, "y\n") {
		t.Errorf("expected echoed answer 'y' in output, got:\n%s", got)
	}
}

// TestPrompt_OnYesIsInvoked is the post-split contract:
// when the user says y AND deps.OnYes is wired, the prompt
// delegates to OnYes instead of printing the fallback hint.
// Production wires OnYes to dispatch the cobra `update check`
// subcommand; the test wires it to a counter so we can assert
// the call happened without spinning up a full cobra tree.
func TestPrompt_OnYesIsInvoked(t *testing.T) {
	checker := newTestChecker(t, "v9.9.9")
	var out bytes.Buffer
	var onYesCalls int
	var capturedOut string

	err := promptForUpdateIfOutdated(context.Background(), &PromptDeps{
		Checker: checker,
		Out:     &out,
		Reader:  func() (string, error) { return "y\n", nil },
		OnYes: func() error {
			onYesCalls++
			capturedOut = out.String()
			return nil
		},
	})
	if err != nil {
		t.Fatalf("promptForUpdateIfOutdated: %v", err)
	}
	if onYesCalls != 1 {
		t.Errorf("OnYes called %d times, want exactly 1", onYesCalls)
	}
	// The prompt header must be in the buffer by the time
	// OnYes runs (so production can route update output to
	// the same writer).
	for _, want := range []string{
		"nightme 9.9.9 is available",
		"Update now?",
	} {
		if !strings.Contains(capturedOut, want) {
			t.Errorf("OnYes ran but prompt text %q was not in writer:\n%s", want, capturedOut)
		}
	}
	// Fallback hint must NOT print when OnYes is wired —
	// OnYes is the source of truth, so duplicating the
	// manual-install text would be noise.
	if strings.Contains(out.String(), "go install github.com/cnlangzi/nightme") {
		t.Errorf("OnYes is set: fallback install hint should NOT print:\n%s", out.String())
	}
}

// TestPrompt_OnYesErrorPropagates pins that the prompt
// returns OnYes's error to the caller instead of swallowing
// it. The REPL caller (runREPLInteractive) ignores the
// returned error today, but future callers may want to log
// it; the contract is "OnYes error flows out unchanged".
func TestPrompt_OnYesErrorPropagates(t *testing.T) {
	checker := newTestChecker(t, "v9.9.9")
	sentinel := errors.New("synthetic update failure")

	err := promptForUpdateIfOutdated(context.Background(), &PromptDeps{
		Checker: checker,
		Out:     io.Discard,
		Reader:  func() (string, error) { return "y\n", nil },
		OnYes:   func() error { return sentinel },
	})
	if err == nil || !errors.Is(err, sentinel) {
		t.Fatalf("expected sentinel error, got %v", err)
	}
}

// TestPrompt_NoOnYesFallsBackToInlineHint documents the
// legacy / safety-net path: when OnYes is nil the prompt
// prints the manual install hint inline. This is what
// tests relying on the "go install" string in the buffer
// pin against.
func TestPrompt_NoOnYesFallsBackToInlineHint(t *testing.T) {
	checker := newTestChecker(t, "v9.9.9")
	var out bytes.Buffer

	err := promptForUpdateIfOutdated(context.Background(), &PromptDeps{
		Checker: checker,
		Out:     &out,
		Reader:  func() (string, error) { return "yes\n", nil },
		// OnYes nil
	})
	if err != nil {
		t.Fatalf("promptForUpdateIfOutdated: %v", err)
	}
	if !strings.Contains(out.String(), "go install") {
		t.Errorf("expected inline install hint when OnYes is nil:\n%s", out.String())
	}
}

// TestPrompt_OutdatedNo is the "user says no" path. We
// expect the prompt, the echo, and NOT the install instructions.
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
	if strings.Contains(got, "go install") {
		t.Errorf("did not expect install instructions on 'n':\n%s", got)
	}
}

// TestPrompt_OutdatedEnterOnly treats a bare newline as "no"
// (default in [y/N] convention) and must NOT print the install
// instructions.
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
	if strings.Contains(out.String(), "go install") {
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

// TestUpdate_BareIsCheck exercises the "bare `nightme update`
// delegates to `update check`" wiring. We run against an
// httptest server that returns a known newer version and
// assert the output looks exactly like `update check` would
// produce (Current version / Latest release / Status lines).
func TestUpdate_BareIsCheck(t *testing.T) {
	srv := httptest.NewServer(stubVersionHandler("9.9.9"))
	t.Cleanup(srv.Close)

	// We can't override the default endpoint URL inside the
	// production code without plumbing it through; redirect
	// the default URL via the package-level DefaultVersionURL
	// var instead (it's a var, not const, for exactly this
	// reason).
	saved := version.DefaultVersionURL
	version.DefaultVersionURL = srv.URL
	t.Cleanup(func() { version.DefaultVersionURL = saved })

	root := newTestRoot()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"update"})
	if err := root.Execute(); err != nil {
		t.Fatalf("nightme update: %v", err)
	}

	got := buf.String()
	for _, want := range []string{
		"Current version:",
		"Latest release:",
		"9.9.9",
		"newer release is available",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q\n--- full output ---\n%s", want, got)
		}
	}
}

// TestUpdate_HasCheckDownloadInstallSubcommands is a smoke
// test against the cobra tree: the three subcommands we
// promised in the commit message must all be registered, and
// the parent must NOT have a RunE that fires when subcommands
// are listed under `help update`.
func TestUpdate_HasCheckDownloadInstallSubcommands(t *testing.T) {
	root := newTestRoot()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"help", "update"})
	if err := root.Execute(); err != nil {
		t.Fatalf("help update: %v", err)
	}
	got := buf.String()
	for _, sub := range []string{"check", "download", "install"} {
		if !strings.Contains(got, sub) {
			t.Errorf("help update missing subcommand %q\n%s", sub, got)
		}
	}
}