//go:build liveupdate && !windows

package main

// The tests in this file reach the operator's machine.
//
// The update prompt has no seam: it loads the real config,
// downloads a release into the real data directory, installs it
// over the running binary and restarts the daemon. A test that
// accepts the offer therefore stops the developer's daemon — the
// install step spawns `nightme restart`, which asks the daemon to
// stop over its control socket.
//
// The build tag keeps these tests out of `make test`. CI runs them
// with `-tags liveupdate`, where no daemon is running and the data
// directory is a temporary one:
//
//	go test -tags liveupdate ./cmd/nightme/
//
// The verdict the checker returns is injected, so the tests do not
// depend on the build's own version: a build that names no release
// compares as older than every release, and a test that leaned on
// that would change behaviour with the version default.
//
// Windows is excluded because it cannot replace a running binary:
// the file is locked by the process that runs it.

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/updater"
	"github.com/cnlangzi/nightme/internal/version"
)

// liveLatestTag asks the release feed which tag is current.
//
// The download stage resolves the tag on its own; this call only
// makes the transcript show the real version instead of a made-up
// one.
func liveLatestTag(ctx context.Context, t *testing.T) string {
	t.Helper()
	tag, _, err := updater.LookupLatestTag(ctx)
	if err != nil {
		t.Skipf("the release feed did not answer: %v", err)
	}
	return tag
}

// skipOnDownloadFailure skips the test when the release asset could
// not be fetched.
//
// The release asset is ~19 MB and the download client gives up after
// 45 seconds, so a link slower than ~400 KB/s fails the download.
// That is the runner's network, not the code: the chain itself is
// covered hermetically by cmd/nightme/update_integration_test.go,
// which serves the same three stages from fixture servers. A
// download that succeeds but installs nothing is a different
// matter and fails the test.
func skipOnDownloadFailure(t *testing.T, transcript string) {
	t.Helper()
	if strings.Contains(transcript, "download failed") {
		t.Skipf("the release asset could not be fetched (network):\n%s", tail(transcript, 400))
	}
}

// tail returns the last n characters of s, for a failure message
// that does not carry the whole progress-bar transcript.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

// TestPrompt_LiveUpdate_DeclinesAtInstall accepts the update offer
// and declines the install: the release is downloaded and staged,
// and the binary is left alone.
//
// On a developer machine the staging directory is the real one, so
// this test is the one that fills it with a release archive.
func TestPrompt_LiveUpdate_DeclinesAtInstall(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	replies := []string{"y\n", "n\n"}
	idx := 0
	var out bytes.Buffer

	err := promptForUpdateIfOutdated(ctx, &PromptDeps{
		VersionCheck: &version.CheckResult{Latest: liveLatestTag(ctx, t), Outdated: true},
		Out:          &out,
		Reader: func() (string, error) {
			if idx >= len(replies) {
				return "n\n", nil
			}
			reply := replies[idx]
			idx++
			return reply, nil
		},
	})
	if err != nil {
		t.Fatalf("promptForUpdateIfOutdated: %v", err)
	}

	got := out.String()
	skipOnDownloadFailure(t, got)
	for _, want := range []string{"Update available", "Update now?", "Staged"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q\n--- full output ---\n%s", want, got)
		}
	}
	if strings.Contains(got, "installed") {
		t.Errorf("declining the install must not install anything:\n%s", got)
	}
}

// TestPrompt_LiveUpdate_InstallsTheRelease accepts both prompts:
// the release is downloaded and installed over the running binary.
//
// The install target is the running test binary (os.Executable),
// so the test replaces itself with the release binary — the same
// swap `nightme update` performs on a real nightme. The daemon is
// left alone: the restart only happens when a daemon is running,
// and CI has none.
func TestPrompt_LiveUpdate_InstallsTheRelease(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	before, err := os.Stat(exe)
	if err != nil {
		t.Fatalf("stat %s: %v", exe, err)
	}

	var out bytes.Buffer
	err = promptForUpdateIfOutdated(ctx, &PromptDeps{
		VersionCheck: &version.CheckResult{Latest: liveLatestTag(ctx, t), Outdated: true},
		Out:          &out,
		Reader:       func() (string, error) { return "y\n", nil },
	})
	if err != nil {
		t.Fatalf("promptForUpdateIfOutdated: %v", err)
	}

	got := out.String()
	skipOnDownloadFailure(t, got)
	for _, want := range []string{"Update available", "Update now?", "Install now?", "Staged", "installed"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q\n--- full output ---\n%s", want, got)
		}
	}

	after, err := os.Stat(exe)
	if err != nil {
		t.Fatalf("stat %s: %v", exe, err)
	}
	if after.Size() == before.Size() && after.ModTime().Equal(before.ModTime()) {
		t.Errorf("the running binary was not replaced: size=%d mtime=%s", after.Size(), after.ModTime())
	}
}
