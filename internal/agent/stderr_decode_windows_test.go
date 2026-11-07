//go:build windows

package agent

import (
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/codepage"
)

// TestStderrRingBuffer_DecodesConsoleCodePage: on a zh-CN host the
// child's stderr carries CP936 bytes, and the ring buffer hands the
// bridge diagnostic readable text instead of raw GBK.
//
// The decode is gated on the host's ANSI code page, so this only
// asserts anything on a Chinese Windows install; the rule itself is
// covered by internal/codepage's tests on every platform.
func TestStderrRingBuffer_DecodesConsoleCodePage(t *testing.T) {
	if codepage.LocalACP() != codepage.ChineseACP {
		t.Skipf("the console code page is %d, not CP936; the decode is a no-op on this host", codepage.LocalACP())
	}

	raw := []byte{
		0xc3, 0xfc, 0xc1, 0xee, 0xd0, 0xd0, 0xcc, 0xab,
		0xb3, 0xa4, 0xa1, 0xa3, 0x0d, 0x0a,
	}
	r := NewStderrRingBuffer(StderrTailBytes)
	if _, err := r.Write(raw); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got := r.String()
	if !strings.Contains(got, "命令行太长。") {
		t.Errorf("String() = %q, want the decoded CP936 message", got)
	}
}
