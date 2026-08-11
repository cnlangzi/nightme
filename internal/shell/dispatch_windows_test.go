//go:build windows

// Windows-specific tests for the shell dispatcher:
//   - cmd /c execution path (cmd.exe, not sh)
//   - GBK / GB18030 decoding of child output
//   - decoderFor / decode unit coverage
//
// Unix-side coverage lives in dispatch_test.go (cross-platform
// parseShell + renderSummary + dispatch tests, with Unix
// commands like !echo, !false, !pwd, !seq).
package shell

import (
	"context"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/simplifiedchinese"
)

// TestDispatch_EchoHello_CmdPath exercises the cmd /c path:
// "echo hello" on cmd.exe outputs "hello " (with trailing space,
// a known cmd.exe quirk). We assert the summary card contains
// the command and ✅.
func TestDispatch_EchoHello_CmdPath(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("cmd /c only available on Windows")
	}
	r := dispatch(context.Background(), Request{Text: "!echo hello", Cwd: t.TempDir()})
	if !r.Consumed {
		t.Fatal("expected Consumed=true")
	}
	if r.ExitCode != 0 {
		t.Errorf("expected exit 0, got %d", r.ExitCode)
	}
	if !strings.Contains(r.Stdout, "hello") {
		t.Errorf("expected stdout to contain 'hello', got %q", r.Stdout)
	}
	if !strings.Contains(r.Reply, "echo hello") {
		t.Errorf("expected summary to include command, got %q", r.Reply)
	}
	if !strings.Contains(r.Reply, "✅") {
		t.Errorf("expected summary to have ✅, got %q", r.Reply)
	}
}

// TestDispatch_CmdNotFound verifies the cmd.exe equivalent of
// "command not found". cmd.exe returns exit 9009 (== 0x2331)
// for an unrecognised program; we just assert non-zero exit.
func TestDispatch_CmdNotFound(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("cmd /c only available on Windows")
	}
	r := dispatch(context.Background(), Request{Text: "!definitely-not-a-real-cmd-xyzzy", Cwd: t.TempDir()})
	if !r.Consumed {
		t.Fatal("expected Consumed=true")
	}
	if r.ExitCode == 0 {
		t.Errorf("expected non-zero exit for missing command, got %d", r.ExitCode)
	}
	if !strings.Contains(r.Reply, "❌") {
		t.Errorf("expected summary to have ❌, got %q", r.Reply)
	}
}

// TestDecoderFor_ChineseACP returns a non-nil decoder when ACP
// is 936 (Simplified Chinese). For non-Chinese ACPs (Western,
// Japanese, etc.) the result is nil — caller passes through raw.
func TestDecoderFor_ChineseACP(t *testing.T) {
	if got := decoderFor(936); got == nil {
		t.Error("decoderFor(936): want non-nil GB18030 decoder, got nil")
	}
	for _, acp := range []uint32{1252, 932, 949, 950} {
		if got := decoderFor(acp); got != nil {
			t.Errorf("decoderFor(%d): want nil (raw passthrough), got %v", acp, got)
		}
	}
}

// TestDecode_GBKHelloWorld is the unit-level round-trip for the
// GBK decoder path. GBK encoding of "你好" is the byte sequence
// {0xC4, 0xE3, 0xBA, 0xC3}; decoding via GB18030 should produce
// the original UTF-8 string. This catches off-by-one buffer
// bugs and replacement-character mishandling in decode().
func TestDecode_GBKHelloWorld(t *testing.T) {
	dec := simplifiedchinese.GB18030.NewDecoder()
	got := decode([]byte{0xC4, 0xE3, 0xBA, 0xC3}, dec)
	if got != "你好" {
		t.Errorf("decode(GBK '你好') = %q, want %q", got, "你好")
	}
}

// TestDecode_EmptyAndNil verifies the empty / nil-input edge
// cases of decode(). decode(nil, ...) should never panic and
// should return "".
func TestDecode_EmptyAndNil(t *testing.T) {
	if got := decode(nil, nil); got != "" {
		t.Errorf("decode(nil, nil) = %q, want \"\"", got)
	}
	if got := decode([]byte{}, nil); got != "" {
		t.Errorf("decode([], nil) = %q, want \"\"", got)
	}
	// With a real decoder but empty input — should still be "".
	dec := simplifiedchinese.GB18030.NewDecoder()
	if got := decode(nil, dec); got != "" {
		t.Errorf("decode(nil, dec) = %q, want \"\"", got)
	}
}

// TestDecode_NopDecoder verifies that nil decoder → raw bytes
// passthrough (the "non-Chinese Windows" path). The bytes come
// out unmodified, regardless of whether they're valid UTF-8.
func TestDecode_NopDecoder(t *testing.T) {
	got := decode([]byte("hello"), nil)
	if got != "hello" {
		t.Errorf("decode ASCII with nil decoder = %q, want %q", got, "hello")
	}
}

// Suppress unused-import warnings if encoding isn't referenced
// after edits to this file.
var _ = encoding.Nop