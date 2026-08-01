package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// newTestRoot returns a fresh cobra root wired up with the same
// subcommands the binary exposes. Each test gets its own root so
// previous SetArgs/SetContext calls do not leak between cases.
//
// The caller is responsible for routing Out/Err to a buffer (see
// the helper below). We intentionally do not call SetOut here so
// tests never accidentally split output across two writers.
func newTestRoot() *cobra.Command {
	return newRootCmd()
}

// captureREPLIO wires root's output streams to buf so the REPL and
// any dispatched subcommand write into the same buffer.
func captureREPLIO(root *cobra.Command, buf *bytes.Buffer) {
	root.SetOut(buf)
	root.SetErr(buf)
}

// TestREPL_EOF verifies a clean Ctrl-D (empty stdin) exits without
// error and prints the banner + a trailing newline so the host
// shell prompt starts on its own line.
func TestREPL_EOF(t *testing.T) {
	root := newTestRoot()
	var buf bytes.Buffer
	captureREPLIO(root, &buf)
	if err := runREPLWith(root, nil, strings.NewReader("")); err != nil {
		t.Fatalf("runREPLWith: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"nightme ", "Interactive shell", "nightme> "} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n%s", want, out)
		}
	}
	// EOF should not print "bye" (that signals explicit exit/quit).
	if strings.Contains(out, "bye") {
		t.Errorf("EOF should not print bye: %q", out)
	}
}

// TestREPL_Exit covers the typed `exit` keyword — banner + a
// friendly farewell, then clean return.
func TestREPL_Exit(t *testing.T) {
	root := newTestRoot()
	var buf bytes.Buffer
	captureREPLIO(root, &buf)
	if err := runREPLWith(root, nil, strings.NewReader("exit\n")); err != nil {
		t.Fatalf("runREPLWith: %v", err)
	}
	if !strings.Contains(buf.String(), "bye") {
		t.Errorf("exit should print bye: %q", buf.String())
	}
}

// TestREPL_Quit mirrors TestREPL_Exit for the `quit` alias.
func TestREPL_Quit(t *testing.T) {
	root := newTestRoot()
	var buf bytes.Buffer
	captureREPLIO(root, &buf)
	if err := runREPLWith(root, nil, strings.NewReader("quit\n")); err != nil {
		t.Fatalf("runREPLWith: %v", err)
	}
	if !strings.Contains(buf.String(), "bye") {
		t.Errorf("quit should print bye: %q", buf.String())
	}
}

// TestREPL_EmptyLine_Noop confirms that an empty line (Enter on a
// blank prompt) re-prompts instead of dispatching an empty argv.
func TestREPL_EmptyLine_Noop(t *testing.T) {
	root := newTestRoot()
	var buf bytes.Buffer
	captureREPLIO(root, &buf)
	in := strings.NewReader("\n\n")
	if err := runREPLWith(root, nil, in); err != nil {
		t.Fatalf("runREPLWith: %v", err)
	}
	count := strings.Count(buf.String(), "nightme> ")
	if count < 3 {
		t.Errorf("expected at least 3 prompts (banner + 2 re-prompts), got %d:\n%s",
			count, buf.String())
	}
	if strings.Contains(buf.String(), "Error:") {
		t.Errorf("empty input should not surface an error: %q", buf.String())
	}
}

// TestREPL_TrimWhitespace exercises the strings.TrimSpace path so
// leading/trailing spaces around a command do not break dispatch.
func TestREPL_TrimWhitespace(t *testing.T) {
	root := newTestRoot()
	var buf bytes.Buffer
	captureREPLIO(root, &buf)
	if err := runREPLWith(root, nil, strings.NewReader("   version   \n")); err != nil {
		t.Fatalf("runREPLWith: %v", err)
	}
	if !strings.Contains(buf.String(), "nightme version") {
		t.Errorf("whitespace-trimmed dispatch did not run 'version': %q", buf.String())
	}
}

// TestREPL_UnknownCommand confirms an unknown subcommand surfaces a
// human-readable error and the REPL continues rather than dying.
func TestREPL_UnknownCommand(t *testing.T) {
	root := newTestRoot()
	var buf bytes.Buffer
	captureREPLIO(root, &buf)
	in := strings.NewReader("not-a-real-command\nexit\n")
	if err := runREPLWith(root, nil, in); err != nil {
		t.Fatalf("runREPLWith: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "Error:") {
		t.Errorf("unknown command should print Error: line: %q", out)
	}
	if !strings.Contains(out, "bye") {
		t.Errorf("REPL should continue after error and reach exit: %q", out)
	}
}

// TestREPL_DispatchesVersion confirms a real command runs and its
// output is visible in the buffer. Integration check that the
// loop wires cobra correctly.
func TestREPL_DispatchesVersion(t *testing.T) {
	root := newTestRoot()
	var buf bytes.Buffer
	captureREPLIO(root, &buf)
	if err := runREPLWith(root, nil, strings.NewReader("version\n")); err != nil {
		t.Fatalf("runREPLWith: %v", err)
	}
	if !strings.Contains(buf.String(), "nightme version") {
		t.Errorf("version output missing: %q", buf.String())
	}
}

// TestREPL_BannerHasVersion covers the banner header substitution:
// the literal template placeholder gets replaced with the build's
// version metadata.
func TestREPL_BannerHasVersion(t *testing.T) {
	root := newTestRoot()
	var buf bytes.Buffer
	captureREPLIO(root, &buf)
	if err := runREPLWith(root, nil, strings.NewReader("")); err != nil {
		t.Fatalf("runREPLWith: %v", err)
	}
	if strings.Contains(buf.String(), "%!s(MISSING)") {
		t.Errorf("banner has unfilled placeholder: %q", buf.String())
	}
}

// TestREPL_PromptAfterCommand checks that the prompt is reprinted
// after a successful command so the user can keep typing.
func TestREPL_PromptAfterCommand(t *testing.T) {
	root := newTestRoot()
	var buf bytes.Buffer
	captureREPLIO(root, &buf)
	in := strings.NewReader("version\nexit\n")
	if err := runREPLWith(root, nil, in); err != nil {
		t.Fatalf("runREPLWith: %v", err)
	}
	// We expect: banner prompt + version output + re-prompt + bye.
	count := strings.Count(buf.String(), "nightme> ")
	if count < 2 {
		t.Errorf("expected prompt before and after command, got %d:\n%s",
			count, buf.String())
	}
}