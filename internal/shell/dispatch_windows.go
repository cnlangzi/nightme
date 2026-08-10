//go:build windows

// Windows shell executor — runs the user command via `cmd /c`.
// `cmd.exe` is on every Windows install since NT 4.0
// (C:\Windows\System32\cmd.exe, PATH-default), so no shell
// detection chain is needed.
//
// The Windows-specific gotcha is encoding: cmd.exe writes
// stdout/stderr in the system's active ANSI/OEM code page
// (CP_ACP). On a Simplified Chinese Windows this is CP936
// (GBK), and Go's os/exec returns raw bytes — so a user
// running `!ipconfig /all` on a Chinese-locale box would see
// garbled text. We decode via golang.org/x/text/encoding
// using GB18030 (a GBK superset) when GetACP() returns the
// Chinese code page; otherwise we fall through to UTF-8
// (which works for cmd /u /c invocations and modern PowerShell
// pipes — though we don't invoke those here, leaving the door
// open for future variants).
package shell

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"
)

// cpACPChinese is the Windows ANSI code page for Simplified
// Chinese. GetACP() returns 936 on zh-CN systems; Big5 (950)
// and Shift-JIS (932) are out of scope for this MVP — they
// would need their own decoder branches.
const cpACPChinese = 936

// executeShell runs cmd via `cmd /c` in cwd with a clean env.
// stdout/stderr are captured as raw bytes and decoded based
// on the system's active code page (see file header).
//
// The cmd /c quoting rules differ from POSIX (no single-quote
// blocks; `^` is the escape char). For MVP we pass cmd as a
// single argument and let cmd.exe handle its own parsing —
// this matches how Windows users actually type commands in
// cmd.exe interactively.
func executeShell(ctx context.Context, cwd, cmd string) (*Result, error) {
	start := time.Now()

	c := exec.CommandContext(ctx, "cmd", "/c", cmd)
	c.Dir = cwd
	c.Env = os.Environ()

	var stdoutRaw, stderrRaw bytes.Buffer
	c.Stdout = &stdoutRaw
	c.Stderr = &stderrRaw

	// On Windows, child handles can be inherited — when we kill
	// the parent context, the child should die too. Default
	// behaviour for CommandContext is fine here.
	runErr := c.Run()
	dur := time.Since(start)

	exitCode := 0
	if runErr != nil {
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			exitCode = ee.ExitCode()
		} else {
			exitCode = -1
		}
	}

	dec := decoderFor(windows.GetACP())

	r := &Result{
		Consumed: true,
		Stdout:   decode(stdoutRaw.Bytes(), dec),
		Stderr:   decode(stderrRaw.Bytes(), dec),
		ExitCode: exitCode,
		Duration: dur,
		Cmd:      cmd,
		Cwd:      cwd,
	}
	r.Reply = renderSummary(r)
	return r, nil
}

// decoderFor picks the right decoder for the given ANSI code
// page. Unknown code pages fall back to identity (raw bytes)
// so we never silently corrupt output — a user on a non-
// Chinese system will see whatever cmd.exe wrote, which is
// what they'd see if they ran the command manually in cmd.
func decoderFor(acp uint32) *encoding.Decoder {
	if acp == cpACPChinese {
		return simplifiedchinese.GB18030.NewDecoder()
	}
	return nil
}

// decode runs the raw child bytes through the given decoder.
// Uses x/text/transform with a small buffer; output that fits
// in a single chunk is returned as a string, otherwise we
// drain until done.
func decode(raw []byte, dec *encoding.Decoder) string {
	if len(raw) == 0 {
		return ""
	}
	if dec == nil {
		return string(raw)
	}
	r := transform.NewReader(bytes.NewReader(raw), dec)
	out := &bytes.Buffer{}
	if _, err := out.ReadFrom(r); err != nil {
		// Decoder errors are rare (invalid byte sequences); we
		// fall back to the raw bytes so the user at least sees
		// something rather than an empty string.
		return string(raw)
	}
	return out.String()
}