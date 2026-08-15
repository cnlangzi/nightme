// Regression tests for the claudecode Stop path that run on ALL
// platforms (Unix + Windows).
//
// fix-stop reverse-engineered the actual interrupt mechanism by
// spawning `claude --print --input-format stream-json
// --output-format stream-json --verbose` and observing the wire.
// The CLI's init event advertises `interrupt_receipt_v1`, and
// the corresponding stdin input is a control_request:
//
//	stdin:  {"type":"control_request","request":{"subtype":"interrupt"}}
//	stdout: {"type":"control_response","response":{"subtype":"success","response":{"still_queued":[]}}}
//	stdout: {"type":"result","is_error":true,"subtype":"error_during_execution",...}
//
// The CLI STAYS ALIVE after the interrupt and can accept a
// follow-up user message on the same session_id. Same shape
// as codex's `turn/interrupt` and acp's `session/cancel` —
// structured in-band interrupt that keeps the process alive,
// so the chat layer's TryFlush picks up the next queued
// prompt on the SAME session (no `--resume`, no respawn, no
// ghost turn).
//
// The Unix-only tests that actually spawn a real subprocess
// and observe the wire live (TestStop_WritesControlRequestInterruptOnStdin,
// TestStop_DoesNotSendSIGINT, TestStop_FallsBackToSIGINTWhenStdinBroken)
// live in agent_interrupt_unix_test.go — they need
// `syscall.SysProcAttr.Setpgid` and `os/exec` process-group
// semantics that only exist on Unix.

package claudecode

import (
	"context"
	"errors"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
)

// interruptPayload is the canonical stdin payload that
// reverse-engineered test runs confirmed Claude Code 2.1.220
// accepts as an in-flight turn interrupt. Same shape as
// codex turn/interrupt and acp session/cancel — a single
// message carrying an action verb.
const interruptPayload = `{"type":"control_request","request":{"subtype":"interrupt"}}`

// TestStop_NoCmdReturnsErrNotSupported covers the pre-Start /
// post-Close contract. The driver has no cmd — Stop must
// return ErrNotSupported and not panic. Cross-platform: no
// subprocess, no signals, no pipes.
func TestStop_NoCmdReturnsErrNotSupported(t *testing.T) {
	d := &driver{}
	err := d.Stop(context.Background())
	if !errors.Is(err, agent.ErrNotSupported) {
		t.Errorf("Stop before Start = %v, want ErrNotSupported", err)
	}
}