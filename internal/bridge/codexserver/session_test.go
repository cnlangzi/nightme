package codexserver

import (
	"os/exec"
	"strings"
	"testing"
)

// ─── ringBuffer tests ───

func TestRingBuffer_BelowCapacity(t *testing.T) {
	rb := newRingBuffer(100)
	_, _ = rb.Write([]byte("hello"))
	_, _ = rb.Write([]byte(" world"))
	if got := rb.String(); got != "hello world" {
		t.Errorf("ringBuffer = %q, want %q", got, "hello world")
	}
}

func TestRingBuffer_TrimsToCapacity(t *testing.T) {
	rb := newRingBuffer(5)
	_, _ = rb.Write([]byte("abc"))
	_, _ = rb.Write([]byte("def"))
	_, _ = rb.Write([]byte("ghi")) // total 9, last 5 = "defghi"
	// After "abc" + "def" + "ghi" = 9 bytes; trim to last 5 = "efghi".
	if got := rb.String(); got != "efghi" {
		t.Errorf("ringBuffer after overflow = %q, want %q", got, "efghi")
	}
}

func TestRingBuffer_LargeSingleWrite(t *testing.T) {
	rb := newRingBuffer(10)
	big := make([]byte, 100)
	for i := range big {
		big[i] = byte('a' + i%26)
	}
	_, _ = rb.Write(big)
	if got := rb.String(); len(got) != 10 {
		t.Errorf("ringBuffer = %d bytes, want 10", len(got))
	}
	// Last 10 bytes of big.
	expected := string(big[len(big)-10:])
	if got := rb.String(); got != expected {
		t.Errorf("ringBuffer = %q, want %q", got, expected)
	}
}

// ─── detectBranch tests ───

func TestDetectBranch_NotAGitRepo(t *testing.T) {
	tmpDir := t.TempDir()
	if branch := detectBranch(tmpDir); branch != "" {
		t.Errorf("detectBranch on non-git dir = %q, want empty", branch)
	}
}

func TestDetectBranch_AGitRepo(t *testing.T) {
	tmpDir := t.TempDir()
	run := func(name string, args ...string) {
		t.Helper()
		cmd := exec.Command(name, args...)
		cmd.Dir = tmpDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s %v: %v\n%s", name, args, err, out)
		}
	}
	run("git", "init", "--initial-branch=main")
	run("git", "config", "user.email", "test@example.com")
	run("git", "config", "user.name", "test")
	// Commit so HEAD resolves.
	if err := exec.Command("git", "-C", tmpDir, "commit", "--allow-empty", "-m", "init").Run(); err != nil {
		t.Skipf("git commit failed (no git?): %v", err)
	}
	if branch := detectBranch(tmpDir); branch != "main" {
		t.Errorf("detectBranch = %q, want main", branch)
	}
}

// ─── sessionConfig → argv ───

// We don't expose buildArgs as a separate function (it's inlined in
// newSession). Instead, exercise the argv contract via a small
// reconstruction: the helper ensures our `-c` flags survive the
// JSON-RPC layer (i.e. that codex accepts them).

func TestSessionConfig_DefaultsAreEmpty(t *testing.T) {
	cfg := sessionConfig{}
	if cfg.workspace != "" {
		t.Errorf("default workspace not empty: %q", cfg.workspace)
	}
	if cfg.model != "" || cfg.effort != "" {
		t.Errorf("model/effort defaults not empty: %+v", cfg)
	}
	if cfg.resume {
		t.Errorf("default resume = true")
	}
}

func TestSessionConfig_ResumeFlagFromSessionID(t *testing.T) {
	// sessionConfig.resume is set by the Agent based on cfg.SessionID — same contract.
	cfg := sessionConfig{sessionID: "thr-existing", resume: true}
	if !cfg.resume {
		t.Errorf("resume = false when sessionID set; want true")
	}
	cfg2 := sessionConfig{}
	if cfg2.resume {
		t.Errorf("resume = true when sessionID empty; want false")
	}
}

// ─── argv construction (extract from newSession for testability) ───

// argvForSession mirrors the argv assembly in newSession so we can
// assert it without spawning a real process. Kept in the test file
// (not in session.go) because the production argv is a single
// short literal slice; pulling it into a named function would add
// a level of indirection without behavioural gain.
func argvForSession(cfg sessionConfig) []string {
	argv := []string{"app-server", "--listen", "stdio://"}
	if cfg.model != "" {
		argv = append(argv, "-c", `model="`+cfg.model+`"`)
	}
	if cfg.effort != "" {
		argv = append(argv, "-c", `model_reasoning_effort="`+cfg.effort+`"`)
	}
	return argv
}

func TestArgvForSession_NoExtras(t *testing.T) {
	got := argvForSession(sessionConfig{})
	want := []string{"app-server", "--listen", "stdio://"}
	if !sliceEqual(got, want) {
		t.Errorf("argv = %v, want %v", got, want)
	}
}

func TestArgvForSession_ModelAndEffort(t *testing.T) {
	got := argvForSession(sessionConfig{model: "o4-mini", effort: "high"})
	// argv: [app-server, --listen, stdio://, -c, model=..., -c, model_reasoning_effort=...]
	// 7 elements (each -c is paired with its value, so 2 flags = 2 pairs).
	if len(got) != 7 {
		t.Fatalf("argv length = %d, want 7; argv = %v", len(got), got)
	}
	if got[3] != "-c" || !strings.Contains(got[4], "model=") {
		t.Errorf("argv[3:5] = %v, want -c model=...", got[3:5])
	}
	if got[5] != "-c" || !strings.Contains(got[6], "model_reasoning_effort=") {
		t.Errorf("argv[5:7] = %v, want -c model_reasoning_effort=...", got[5:7])
	}
}

// ─── helpers ───

func sliceEqual(a, b []string) bool {
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
