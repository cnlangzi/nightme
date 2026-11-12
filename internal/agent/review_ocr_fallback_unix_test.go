//go:build !windows

package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeOcrOnPath puts an `ocr` executable that always fails on PATH.
// The real ocr, if installed, is shadowed by it.
func fakeOcrOnPath(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "ocr")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write fake ocr: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestReview_OcrPreviewFails_FallsBackToTheGoFileList covers a broken
// ocr: the Go-side enumeration supplies the file list and the review
// continues. A failing ocr must not read as "there is nothing to
// review".
func TestReview_OcrPreviewFails_FallsBackToTheGoFileList(t *testing.T) {
	fakeOcrOnPath(t)

	dir := gitRepoWithOrigin(t)
	gitCommitFile(t, dir, "a.go", "package a\n+changed\n", "change")

	var mu sync.Mutex
	var prompts []string
	fs := &sessionFakeStarter{
		name: "test-agent",
		newDriver: func(StartConfig) *sessionFakeDriver {
			return newSessionFakeDriver(func(blocks []ContentBlock) []AgentEvent {
				mu.Lock()
				if len(blocks) > 0 {
					prompts = append(prompts, blocks[0].Text)
				}
				mu.Unlock()
				return groupReply("ok")
			})
		},
	}

	res, err := ReviewWithOcr(context.Background(), fs, StartConfig{Workspace: dir})
	if err != nil {
		t.Fatalf("review failed although only ocr is broken: %v", err)
	}
	if !strings.Contains(res.Text, "Reviewed") {
		t.Errorf("review result carries no findings:\n%s", res.Text)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(prompts) == 0 {
		t.Fatal("no agent turn ran — the broken ocr read as an empty diff")
	}
	sawFile := false
	for _, p := range prompts {
		if strings.Contains(p, "a.go") {
			sawFile = true
		}
	}
	if !sawFile {
		t.Errorf("no prompt covers the committed change:\n%s", strings.Join(prompts, "\n---\n"))
	}
}
