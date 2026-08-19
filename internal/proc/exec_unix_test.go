//go:build !windows

package proc

import (
	"context"
	"testing"
)

// TestNew_SetsSysProcAttrSetsid pins the platform-specific
// SysProcAttr wiring so a future regression that drops the
// Setsid flag (and re-introduces the F-54 / stop hang on macOS)
// is caught by CI before it reaches the daemon.
func TestNew_SetsSysProcAttrSetsid(t *testing.T) {
	cmd := New(context.Background(), "/bin/true")
	if cmd.SysProcAttr == nil {
		t.Fatal("New left SysProcAttr nil; the cli will inherit the daemon TTY")
	}
	if !cmd.SysProcAttr.Setsid {
		t.Error("SysProcAttr.Setsid = false; cli will not be its own session/pg leader")
	}
}

// TestBuildTerminalShellCommand pins the shell command that
// openTerminalMac (via AppleScript) and openTerminalLinux (via
// `sh -c`) hand to the spawned terminal. Both platforms share
// this helper, so a single set of cases covers both.
//
// Failure modes this guards against:
//
//  1. A bare name (no single quotes) breaks when the install
//     path contains a space — e.g. /Users/x/My Apps/nightme
//     would be split at the space.
//  2. Dropping the keep-open suffix means the Terminal window
//     closes on any subcommand exit, hiding whatever error the
//     subcommand produced (the user's original bug report).
//
// Tested as a pure function so it doesn't need osascript,
// Terminal.app, or any of the Linux emulators installed.
func TestBuildTerminalShellCommand(t *testing.T) {
	const keepOpen = `; echo; read -p 'press enter to close'`

	cases := []struct {
		name string
		exe  string
		args []string
		want string
	}{
		{
			name: "no args",
			exe:  "/usr/local/bin/nightme",
			args: nil,
			want: "'/usr/local/bin/nightme'" + keepOpen,
		},
		{
			name: "with subcommand",
			exe:  "/usr/local/bin/nightme",
			args: []string{"logs"},
			want: "'/usr/local/bin/nightme' 'logs'" + keepOpen,
		},
		{
			name: "go install path",
			exe:  "/Users/x/go/bin/nightme",
			args: []string{"list"},
			want: "'/Users/x/go/bin/nightme' 'list'" + keepOpen,
		},
		{
			name: "path with space",
			exe:  "/Users/x/My Apps/nightme",
			args: []string{"agents"},
			want: "'/Users/x/My Apps/nightme' 'agents'" + keepOpen,
		},
		{
			name: "multiple subcommand args",
			exe:  "/opt/homebrew/bin/nightme",
			args: []string{"kill", "agent1"},
			want: "'/opt/homebrew/bin/nightme' 'kill' 'agent1'" + keepOpen,
		},
		{
			name: "homebrew linux path",
			exe:  "/home/linuxbrew/.linuxbrew/bin/nightme",
			args: []string{"status"},
			want: "'/home/linuxbrew/.linuxbrew/bin/nightme' 'status'" + keepOpen,
		},
		{
			// Apostrophe in the install path — the standard
			// `'\''` (close, literal quote, reopen) idiom
			// keeps the shell parser happy. Without this
			// escape the second `'` ends the quoted string
			// and bash reports "unexpected EOF".
			name: "path with apostrophe",
			exe:  "/Users/O'Brien/nightme",
			args: nil,
			want: "'/Users/O'\\''Brien/nightme'" + keepOpen,
		},
		{
			// Same idiom applied to an arg.
			name: "arg with apostrophe",
			exe:  "/usr/local/bin/nightme",
			args: []string{"it's"},
			want: "'/usr/local/bin/nightme' 'it'\\''s'" + keepOpen,
		},
		{
			// Backslash is literal inside single-quoted
			// shell strings — no extra escaping needed.
			name: "path with backslash",
			exe:  `/Users/test\foo/nightme`,
			args: nil,
			want: `'/Users/test\foo/nightme'` + keepOpen,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildTerminalShellCommand(tc.exe, tc.args)
			if got != tc.want {
				t.Errorf("buildTerminalShellCommand(%q, %v):\n  got  = %q\n  want = %q",
					tc.exe, tc.args, got, tc.want)
			}
		})
	}
}

// TestShellQuoteAndAppleScriptEscape covers the two quoting
// helpers that wrap user-controlled paths before they're handed
// to a shell or to AppleScript. Both are necessary because
// the shell-quoting rule and the AppleScript-quoting rule are
// different (single-quoted shell strings can't contain `'`,
// AppleScript double-quoted strings can't contain unescaped
// `"` or `\`).
func TestShellQuoteAndAppleScriptEscape(t *testing.T) {
	// shellQuote cases. The interesting ones are the
	// apostrophe (must close, literal quote, reopen) and
	// backslash (literal inside single-quoted shell strings).
	shellCases := []struct {
		in, want string
	}{
		{"/usr/local/bin/nightme", "'/usr/local/bin/nightme'"},
		{"/Users/x/My Apps/nightme", "'/Users/x/My Apps/nightme'"},
		{"/Users/O'Brien/nightme", "'/Users/O'\\''Brien/nightme'"},
		{`/Users/test\foo/nightme`, `'/Users/test\foo/nightme'`},
		{"it's", "'it'\\''s'"},
		{"", "''"},
	}
	for _, c := range shellCases {
		t.Run("shell/"+c.in, func(t *testing.T) {
			if got := shellQuote(c.in); got != c.want {
				t.Errorf("shellQuote(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}

	// escapeAppleScriptString cases. The interesting ones
	// are the literal double-quote (AppleScript would treat
	// the inner `"` as the closing delimiter) and backslash
	// (AppleScript treats `\` as an escape introducer).
	asCases := []struct {
		in, want string
	}{
		{"/usr/local/bin/nightme", "/usr/local/bin/nightme"},
		{`/Users/x/"test"/nightme`, `/Users/x/\"test\"/nightme`},
		{`/Users/test\foo/nightme`, `/Users/test\\foo/nightme`},
		{`"`, `\"`},
		{`\`, `\\`},
	}
	for _, c := range asCases {
		t.Run("applescript/"+c.in, func(t *testing.T) {
			if got := escapeAppleScriptString(c.in); got != c.want {
				t.Errorf("escapeAppleScriptString(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestLinuxProbePrefixes pins the probe list AND the argv
// shape that openTerminalLinux hands to gnome-terminal /
// konsole / etc. on Linux: each probe is the emulator binary
// plus its "run a command" prefix, then `sh -c`, then the
// shell command from buildTerminalShellCommand. This is the
// contract the `sh -c` wrapper relies on — if a future
// refactor drops the wrapper, every emulator falls back to
// spawning the bare command, which is exactly the failure
// mode this test was added to prevent.
func TestLinuxProbePrefixes(t *testing.T) {
	// Each entry is the emulator binary that owns the probe
	// plus the full argv the daemon hands to it. The argv
	// shape MUST end in `sh -c <shellCmd>` — that uniform
	// suffix is what guarantees every emulator receives a
	// shell-interpretable command regardless of its own
	// "run a command" syntax.
	wantArgvs := map[string][]string{
		"gnome-terminal": {"gnome-terminal", "--", "sh", "-c", "<shellCmd>"},
		"konsole":        {"konsole", "-e", "sh", "-c", "<shellCmd>"},
		"alacritty":      {"alacritty", "-e", "sh", "-c", "<shellCmd>"},
		"kitty":          {"kitty", "sh", "-c", "<shellCmd>"},
		"xfce4-terminal": {"xfce4-terminal", "-e", "sh", "-c", "<shellCmd>"},
		"lxterminal":     {"lxterminal", "-e", "sh", "-c", "<shellCmd>"},
		"mate-terminal":  {"mate-terminal", "-e", "sh", "-c", "<shellCmd>"},
		"xterm":          {"xterm", "-e", "sh", "-c", "<shellCmd>"},
		"foot":           {"foot", "sh", "-c", "<shellCmd>"},
		"wezterm":        {"wezterm", "start", "--", "sh", "-c", "<shellCmd>"},
	}

	// Index LinuxTerminalProbes by binary so the test fails
	// fast if a probe is added or removed without updating
	// the expected map (and vice versa).
	gotBins := make(map[string][]string, len(LinuxTerminalProbes))
	for _, p := range LinuxTerminalProbes {
		bin := p[0]
		gotBins[bin] = linuxProbeArgv(p, "<shellCmd>")
	}

	if len(gotBins) != len(wantArgvs) {
		t.Errorf("LinuxTerminalProbes has %d entries, want %d (probes=%v expected=%v)",
			len(gotBins), len(wantArgvs), keys(gotBins), keys(wantArgvs))
	}
	for bin, want := range wantArgvs {
		got, ok := gotBins[bin]
		if !ok {
			t.Errorf("missing probe for %q in LinuxTerminalProbes", bin)
			continue
		}
		if !equalSlices(got, want) {
			t.Errorf("argv for %q = %v, want %v", bin, got, want)
		}
	}
	for bin := range gotBins {
		if _, ok := wantArgvs[bin]; !ok {
			t.Errorf("unexpected probe %q in LinuxTerminalProbes", bin)
		}
	}
}

func keys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
