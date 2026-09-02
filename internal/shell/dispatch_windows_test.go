//go:build windows

// Windows-specific tests for the shell dispatcher:
//   - cmd /c execution path (cmd.exe, not sh)
//   - GBK / GB18030 decoding of child output
//   - decoderFor / wrapDecoder unit coverage
//
// Unix-side coverage lives in dispatch_test.go (cross-platform
// parseShell + executeShell + runShell tests, with Unix
// commands like !echo, !false, !pwd, !seq).
package shell

import (
	"context"
	"runtime"
	"strings"
	"testing"
)

// TestExecuteShell_EchoHello_CmdPath exercises the cmd /c path:
// "echo hello" on cmd.exe outputs "hello " (with trailing space,
// a known cmd.exe quirk). Asserts: exit 0 + stdout contains
// "hello". Replaces the pre-streaming TestDispatch_EchoHello
// which read r.Reply / r.Consumed (those fields were removed
// in F-shell-stream).
func TestExecuteShell_EchoHello_CmdPath(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("cmd /c only available on Windows")
	}
	r := executeShell(context.Background(), t.TempDir(), "echo hello", nil, nil)
	if r.ExitCode != 0 {
		t.Errorf("expected exit 0, got %d", r.ExitCode)
	}
	if !strings.Contains(r.Stdout, "hello") {
		t.Errorf("expected stdout to contain 'hello', got %q", r.Stdout)
	}
}

// TestExecuteShell_CmdNotFound verifies the cmd.exe equivalent
// of "command not found". cmd.exe returns exit 9009 (== 0x2331)
// for an unrecognised program; we just assert non-zero exit.
func TestExecuteShell_CmdNotFound(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("cmd /c only available on Windows")
	}
	r := executeShell(context.Background(), t.TempDir(), "definitely-not-a-real-cmd-xyzzy", nil, nil)
	if r.ExitCode == 0 {
		t.Errorf("expected non-zero exit for missing command, got %d", r.ExitCode)
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

// TestWrapDecoder_NilPassthrough verifies that nil decoder →
// raw bytes passthrough (the "non-Chinese Windows" path). The
// returned reader yields input bytes unmodified. Replaces the
// pre-streaming TestDecode_NopDecoder + TestDecode_EmptyAndNil
// + TestDecode_GBKHelloWorld tests, which exercised a `decode()`
// helper that was removed in F-shell-stream (the streaming
// path uses transform.NewReader directly inside drainWithFallback).
func TestWrapDecoder_NilPassthrough(t *testing.T) {
	got := wrapDecoder(strings.NewReader("hello"), nil)
	var sb strings.Builder
	buf := make([]byte, 64)
	for {
		n, err := got.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	if sb.String() != "hello" {
		t.Errorf("wrapDecoder with nil decoder = %q, want %q", sb.String(), "hello")
	}
}
