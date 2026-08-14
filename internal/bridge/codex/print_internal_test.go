// print_internal_test.go — unit tests for buildPrintArgs argv
// construction. No real codex binary needed; these run in CI.
//
// What we lock here:
//   - The fixed prefix layout (exec, --dangerously-bypass-...
//     -C, --skip-git-repo-check)
//   - ContentImage → -i <path> insertion (repeatable, correct
//     order)
//   - ContentText + ContentFile → positional prompt composition
//   - Empty blocks → sentinel "(see attached content)" so
//     codex exec doesn't fall back to stdin (verified bug on
//     codex 0.145.0)
//   - ContentFile → "@<path>" annotation in prompt (no file
//     flag for exec — file refs are just text)
//
// The dynamic pieces (-o <tmpfile>, --json, --, <prompt>) are
// appended by runPrintMode after buildPrintArgs returns; those
// are covered by the real-binary e2e in print_real_unix_test.go.

//go:build !windows

package codex

import (
	"reflect"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
)

func TestBuildPrintArgs_TextOnly(t *testing.T) {
	args, prompt := buildPrintArgs(
		agent.StartConfig{Workspace: "/tmp/ws"},
		[]agent.ContentBlock{
			{Type: agent.ContentText, Text: "explain this"},
		},
	)
	wantPrefix := []string{
		"exec",
		"--dangerously-bypass-approvals-and-sandbox",
		"-C", "/tmp/ws",
		"--skip-git-repo-check",
	}
	if !reflect.DeepEqual(args, wantPrefix) {
		t.Errorf("args = %v, want %v", args, wantPrefix)
	}
	if prompt != "explain this" {
		t.Errorf("prompt = %q, want %q", prompt, "explain this")
	}
}

func TestBuildPrintArgs_TextJoinsWithNewlines(t *testing.T) {
	_, prompt := buildPrintArgs(
		agent.StartConfig{Workspace: "/ws"},
		[]agent.ContentBlock{
			{Type: agent.ContentText, Text: "first"},
			{Type: agent.ContentText, Text: "second"},
			{Type: agent.ContentText, Text: ""}, // empty skipped
			{Type: agent.ContentText, Text: "third"},
		},
	)
	want := "first\nsecond\nthird"
	if prompt != want {
		t.Errorf("prompt = %q, want %q", prompt, want)
	}
}

func TestBuildPrintArgs_ImageFlagRepeated(t *testing.T) {
	args, prompt := buildPrintArgs(
		agent.StartConfig{Workspace: "/ws"},
		[]agent.ContentBlock{
			{Type: agent.ContentText, Text: "compare these"},
			{Type: agent.ContentImage, Path: "/img/a.png", MediaType: "image/png"},
			{Type: agent.ContentImage, Path: "/img/b.jpg", MediaType: "image/jpeg"},
		},
	)
	// Two -i flags appended after the fixed prefix.
	wantArgs := []string{
		"exec",
		"--dangerously-bypass-approvals-and-sandbox",
		"-C", "/ws",
		"--skip-git-repo-check",
		"-i", "/img/a.png",
		"-i", "/img/b.jpg",
	}
	if !reflect.DeepEqual(args, wantArgs) {
		t.Errorf("args = %v, want %v", args, wantArgs)
	}
	// Each ContentImage contributes a `[image]` placeholder at
	// its position in the prompt so the model sees order. The
	// actual vision content is delivered via the -i flags in
	// wantArgs above (verified: 100×100 RGBA → 17,869 vision
	// tokens via F-CODEX-PRINT-001 token-scaling check).
	wantPrompt := "compare these\n[image]\n[image]"
	if prompt != wantPrompt {
		t.Errorf("prompt = %q, want %q", prompt, wantPrompt)
	}
	if got := countImageFlags(args); got != 2 {
		t.Errorf("countImageFlags = %d, want 2", got)
	}
}

func TestBuildPrintArgs_FileAsAtRef(t *testing.T) {
	_, prompt := buildPrintArgs(
		agent.StartConfig{Workspace: "/ws"},
		[]agent.ContentBlock{
			{Type: agent.ContentText, Text: "review"},
			{Type: agent.ContentFile, Path: "/src/main.go"},
			{Type: agent.ContentFile, Path: ""}, // empty skipped
		},
	)
	want := "review\n@/src/main.go"
	if prompt != want {
		t.Errorf("prompt = %q, want %q", prompt, want)
	}
}

func TestBuildPrintArgs_AllImagesFallsBackToSentinel(t *testing.T) {
	// All-image blocks: no text content. codex exec requires
	// SOMETHING as the positional arg, else it falls back to
	// stdin. Use a benign sentinel.
	args, prompt := buildPrintArgs(
		agent.StartConfig{Workspace: "/ws"},
		[]agent.ContentBlock{
			{Type: agent.ContentImage, Path: "/img/x.png"},
		},
	)
	wantArgs := []string{
		"exec",
		"--dangerously-bypass-approvals-and-sandbox",
		"-C", "/ws",
		"--skip-git-repo-check",
		"-i", "/img/x.png",
	}
	if !reflect.DeepEqual(args, wantArgs) {
		t.Errorf("args = %v, want %v", args, wantArgs)
	}
	if prompt != "[image]" {
		t.Errorf("prompt = %q, want %q", prompt, "[image]")
	}
}

func TestBuildPrintArgs_EmptyBlocksFallsBackToSentinel(t *testing.T) {
	_, prompt := buildPrintArgs(
		agent.StartConfig{Workspace: "/ws"},
		nil,
	)
	if prompt != "(see attached content)" {
		t.Errorf("prompt = %q, want %q", prompt, "(see attached content)")
	}
}

func TestBuildPrintArgs_SkipEmptyPaths(t *testing.T) {
	// Empty Path on Image/File → silently dropped (matches the
	// long-lived bridge's behavior; verified in agent.go's
	// SendBlocks path).
	args, prompt := buildPrintArgs(
		agent.StartConfig{Workspace: "/ws"},
		[]agent.ContentBlock{
			{Type: agent.ContentImage, Path: ""},
			{Type: agent.ContentFile, Path: ""},
		},
	)
	if got := countImageFlags(args); got != 0 {
		t.Errorf("countImageFlags = %d, want 0", got)
	}
	if !strings.Contains(prompt, "(see attached content)") {
		t.Errorf("prompt = %q, want sentinel (no usable blocks)", prompt)
	}
}

// TestBuildPrintArgs_PreservesBlockOrder — the F-CODEX-PRINT-001
// "faithful forwarding" rule requires that text/image/file
// block order is preserved in argv + prompt. codex exec CLI
// can't natively interleave image bytes with text (only -i
// flag for images), so the implementation uses a `[image]`
// placeholder at each image's position in the prompt. This
// test pins the exact layout.
func TestBuildPrintArgs_PreservesBlockOrder(t *testing.T) {
	args, prompt := buildPrintArgs(
		agent.StartConfig{Workspace: "/ws"},
		[]agent.ContentBlock{
			{Type: agent.ContentText, Text: "first text"},
			{Type: agent.ContentImage, Path: "/img.png"},
			{Type: agent.ContentText, Text: "second text"},
			{Type: agent.ContentFile, Path: "/file.txt"},
			{Type: agent.ContentText, Text: "third text"},
		},
	)
	wantPrompt := "first text\n[image]\nsecond text\n@/file.txt\nthird text"
	if prompt != wantPrompt {
		t.Errorf("prompt = %q, want %q", prompt, wantPrompt)
	}
	wantArgs := []string{
		"exec",
		"--dangerously-bypass-approvals-and-sandbox",
		"-C", "/ws",
		"--skip-git-repo-check",
		"-i", "/img.png",
	}
	if !reflect.DeepEqual(args, wantArgs) {
		t.Errorf("args = %v, want %v", args, wantArgs)
	}
}