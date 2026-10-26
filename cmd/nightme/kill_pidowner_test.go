package main

import "testing"

// TestSameCommandFold pins the Windows comparison rules: compare
// base names, drop a trailing ".exe", ignore case.
//
// The cases below are deliberately platform-independent — the rules
// are Windows-only in production, but the comparison must answer the
// same way wherever the test runs, so one file covers linux and
// windows. The absolute-path case needs Windows path semantics and
// therefore lives in kill_windows_test.go.
func TestSameCommandFold(t *testing.T) {
	cases := []struct {
		actual string
		want   string
		match  bool
	}{
		{"codex.exe", "codex", true},
		{"codex.exe", "codex.exe", true},
		{"codex", "codex.exe", true},
		{"claude.exe", "Claude", true},
		{"CODEX.EXE", "codex", true},
		{"codex.exe", "claude", false},
		{"claude.exe", "codex.exe", false},
		{"", "codex", false},
		{"codex.exe", "", false},
	}
	for _, c := range cases {
		if got := sameCommandFold(c.actual, c.want); got != c.match {
			t.Errorf("sameCommandFold(%q, %q) = %v, want %v", c.actual, c.want, got, c.match)
		}
	}
}

// TestStripExeSuffix pins the ".exe" trimming rule on its own.
func TestStripExeSuffix(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"codex.exe", "codex"},
		{"codex.EXE", "codex"},
		{"codex.Exe", "codex"},
		{"codex.exe.exe", "codex.exe"},
		{"codex", "codex"},
		{"exe", "exe"},
		{".exe", ".exe"},
		{"", ""},
	}
	for _, c := range cases {
		if got := stripExeSuffix(c.in); got != c.want {
			t.Errorf("stripExeSuffix(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestSameCommand pins the unix rules now that they live in a
// build-tag-free file: compare base names, case-SENSITIVELY, and
// accept a name truncated to commTruncLen characters.
func TestSameCommand(t *testing.T) {
	cases := []struct {
		actual string
		want   string
		match  bool
	}{
		{"claude", "claude", true},
		{"/opt/homebrew/bin/claude", "claude", true},
		{"Claude", "claude", false},
		{"/usr/bin/vim", "claude", false},
		{"claude.exe", "claude", false},
		{"my-long-agent-r", "my-long-agent-runner", true},
	}
	for _, c := range cases {
		if got := sameCommand(c.actual, c.want); got != c.match {
			t.Errorf("sameCommand(%q, %q) = %v, want %v", c.actual, c.want, got, c.match)
		}
	}
}
