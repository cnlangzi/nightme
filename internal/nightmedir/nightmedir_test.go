// Tests for the nightmedir package. Covers the three guarantees
// the package makes: directory creation is idempotent, .gitignore
// is appended without clobbering existing content, and Path /
// FilePath return forward-slash forms regardless of platform.
package nightmedir_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/nightmedir"
)

// EnsureDir: missing dir → created with 0o755.
func TestEnsureDir_CreatesMissing(t *testing.T) {
	cwd := t.TempDir()
	if err := nightmedir.EnsureDir(cwd); err != nil {
		t.Fatalf("EnsureDir: %v", err)
	}
	info, err := os.Stat(filepath.Join(cwd, ".nightme"))
	if err != nil {
		t.Fatalf("expected .nightme to exist: %v", err)
	}
	if !info.IsDir() {
		t.Errorf(".nightme is not a directory")
	}
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != 0o755 {
			t.Errorf(".nightme perm = %o, want 0755", perm)
		}
	}
}

// EnsureDir: already exists → no error, perm unchanged.
func TestEnsureDir_Idempotent(t *testing.T) {
	cwd := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cwd, ".nightme"), 0o700); err != nil {
		t.Fatalf("setup MkdirAll: %v", err)
	}
	if err := nightmedir.EnsureDir(cwd); err != nil {
		t.Fatalf("EnsureDir on existing dir: %v", err)
	}
	info, err := os.Stat(filepath.Join(cwd, ".nightme"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != 0o700 {
			t.Errorf("EnsureDir must not change existing perm; got %o, want 0700", perm)
		}
	}
}

// EnsureDir: cwd parent missing → does NOT create intermediate
// dirs. The contract is narrow — callers that pass a cwd whose
// parents don't exist should get an error rather than a surprise
// tree.
func TestEnsureDir_DoesNotCreateParent(t *testing.T) {
	parent := t.TempDir()
	bogus := filepath.Join(parent, "does", "not", "exist")
	if err := nightmedir.EnsureDir(bogus); err == nil {
		t.Fatalf("EnsureDir must fail when cwd parents are missing")
	}
}

// Path: platform-canonical join of cwd + DirName. Forward-slash
// is not the contract here — that's RelPath's. The contract is
// "ends in DirName, separator matches platform" so callers can
// pass the result to os.Stat / os.WriteFile.
func TestPath_EndsInDirName(t *testing.T) {
	got := nightmedir.Path("/tmp/foo")
	if path := filepath.Join("/tmp/foo", nightmedir.DirName); got != path {
		t.Errorf("Path = %q, want platform-canonical %q", got, path)
	}
}

// FilePath: platform-canonical join of cwd + DirName + name.
// Forward-slash is not the contract here — that's RelPath's.
func TestFilePath_EndsInFile(t *testing.T) {
	got := nightmedir.FilePath("/tmp/foo", "handoff.md")
	if path := filepath.Join("/tmp/foo", nightmedir.DirName, "handoff.md"); got != path {
		t.Errorf("FilePath = %q, want platform-canonical %q", got, path)
	}
}

// RelPath: always slash-form ".nightme/<name>", independent of
// platform. Used for user-visible reply text where IM cards render
// forward slashes regardless of host OS.
func TestRelPath_AlwaysSlash(t *testing.T) {
	got := nightmedir.RelPath("handoff.md")
	if got != ".nightme/handoff.md" {
		t.Errorf("RelPath = %q, want .nightme/handoff.md", got)
	}
	if strings.Contains(got, `\`) {
		t.Errorf("RelPath must not contain backslash; got %q", got)
	}
}

// DirName is the public contract — anyone reading the codebase
// should see the literal `.nightme/` and confirm it matches.
func TestDirName_Literal(t *testing.T) {
	if nightmedir.DirName != ".nightme" {
		t.Errorf("DirName = %q, want .nightme", nightmedir.DirName)
	}
}

// EnsureGitignoreEntry: missing .gitignore → created with the
// entry. Idempotent on re-run.
func TestEnsureGitignoreEntry_CreatesMissing(t *testing.T) {
	cwd := t.TempDir()
	if err := nightmedir.EnsureGitignoreEntry(cwd); err != nil {
		t.Fatalf("EnsureGitignoreEntry: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(cwd, ".gitignore"))
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	if !strings.Contains(string(body), ".nightme/") {
		t.Errorf(".gitignore missing .nightme/ entry; got:\n%s", body)
	}
	if !strings.HasSuffix(string(body), "\n") {
		t.Errorf(".gitignore should end with newline; got %q", body)
	}

	// Re-run: still idempotent.
	if err := nightmedir.EnsureGitignoreEntry(cwd); err != nil {
		t.Fatalf("EnsureGitignoreEntry on existing entry: %v", err)
	}
	body2, _ := os.ReadFile(filepath.Join(cwd, ".gitignore"))
	if strings.Count(string(body2), ".nightme/") != 1 {
		t.Errorf("expected exactly one .nightme/ entry; got:\n%s", body2)
	}
}

// EnsureGitignoreEntry: existing entry (.nightme/ form) → no-op,
// content unchanged.
func TestEnsureGitignoreEntry_DirectoryForm_NoOp(t *testing.T) {
	cwd := t.TempDir()
	original := "build/\n.nightme/\nnode_modules/\n"
	if err := os.WriteFile(filepath.Join(cwd, ".gitignore"), []byte(original), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := nightmedir.EnsureGitignoreEntry(cwd); err != nil {
		t.Fatalf("EnsureGitignoreEntry: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(cwd, ".gitignore"))
	if string(got) != original {
		t.Errorf("content changed on no-op: got %q, want %q", got, original)
	}
}

// EnsureGitignoreEntry: existing entry (.nightme/<file> form) →
// no-op, content unchanged. Manual edits that name a specific
// file inside the directory should still be recognized.
func TestEnsureGitignoreEntry_ExplicitFileForm_NoOp(t *testing.T) {
	cwd := t.TempDir()
	original := "build/\n.nightme/handoff.md\nnode_modules/\n"
	if err := os.WriteFile(filepath.Join(cwd, ".gitignore"), []byte(original), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := nightmedir.EnsureGitignoreEntry(cwd); err != nil {
		t.Fatalf("EnsureGitignoreEntry: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(cwd, ".gitignore"))
	if string(got) != original {
		t.Errorf("content changed on no-op: got %q, want %q", got, original)
	}
}

// EnsureGitignoreEntry: existing .gitignore without entry →
// entry appended, leading newline inserted when existing content
// doesn't end with one. Original content preserved byte-for-byte
// up to the separator newline.
func TestEnsureGitignoreEntry_AppendsPreservingContent(t *testing.T) {
	cwd := t.TempDir()
	original := "build/\nnode_modules/" // intentionally no trailing newline
	if err := os.WriteFile(filepath.Join(cwd, ".gitignore"), []byte(original), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := nightmedir.EnsureGitignoreEntry(cwd); err != nil {
		t.Fatalf("EnsureGitignoreEntry: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(cwd, ".gitignore"))
	want := "build/\nnode_modules/\n.nightme/\n"
	if string(got) != want {
		t.Errorf("appended content mismatch:\ngot:  %q\nwant: %q", got, want)
	}
}

// EnsureGitignoreEntry: existing .gitignore with trailing newline
// → entry appended directly (no double newline).
func TestEnsureGitignoreEntry_AppendsAfterTrailingNewline(t *testing.T) {
	cwd := t.TempDir()
	original := "build/\nnode_modules/\n"
	if err := os.WriteFile(filepath.Join(cwd, ".gitignore"), []byte(original), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := nightmedir.EnsureGitignoreEntry(cwd); err != nil {
		t.Fatalf("EnsureGitignoreEntry: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(cwd, ".gitignore"))
	want := "build/\nnode_modules/\n.nightme/\n"
	if string(got) != want {
		t.Errorf("appended content mismatch:\ngot:  %q\nwant: %q", got, want)
	}
}

// EnsureGitignoreEntry: empty .gitignore → entry written.
func TestEnsureGitignoreEntry_EmptyFile(t *testing.T) {
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, ".gitignore"), nil, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := nightmedir.EnsureGitignoreEntry(cwd); err != nil {
		t.Fatalf("EnsureGitignoreEntry: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(cwd, ".gitignore"))
	if string(got) != ".nightme/\n" {
		t.Errorf("empty file should get single entry; got %q", got)
	}
}

// --- per-user handoff helpers ---

// homeDir sandboxes $HOME (and USERPROFILE on Windows) so the
// per-user helpers resolve inside the test sandbox. Mirrors
// the helper in command/{handoff,resume}/cmd_test.go.
func homeDir(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", home)
	}
	return home
}

// HandoffDirName / HandoffFileSuffix / DirName: literal
// contract — the slash-form composition downstream relies on
// these being exactly these strings.
func TestHandoff_Literals(t *testing.T) {
	if nightmedir.HandoffDirName != "handoff" {
		t.Errorf("HandoffDirName = %q, want handoff", nightmedir.HandoffDirName)
	}
	if nightmedir.HandoffFileSuffix != ".md" {
		t.Errorf("HandoffFileSuffix = %q, want .md", nightmedir.HandoffFileSuffix)
	}
}

// HandoffDir: returns $HOME/.nightme/handoff regardless of the
// caller's cwd. Forward-slash-vs-platform separator comes from
// pathutil.Join.
func TestHandoffDir_Path(t *testing.T) {
	home := homeDir(t)
	got, err := nightmedir.HandoffDir()
	if err != nil {
		t.Fatalf("HandoffDir: %v", err)
	}
	want := filepath.Join(home, ".nightme", "handoff")
	if got != want {
		t.Errorf("HandoffDir = %q, want %q", got, want)
	}
}

// HandoffFilePath: joins HandoffDir with <name>.md.
func TestHandoffFilePath_Path(t *testing.T) {
	home := homeDir(t)
	got, err := nightmedir.HandoffFilePath("demo")
	if err != nil {
		t.Fatalf("HandoffFilePath: %v", err)
	}
	want := filepath.Join(home, ".nightme", "handoff", "demo.md")
	if got != want {
		t.Errorf("HandoffFilePath = %q, want %q", got, want)
	}
}

// EnsureHandoffDir: missing dir → created (lazily creating
// $HOME/.nightme too — the per-user .nightme may not exist on
// a first-run sandbox).
func TestEnsureHandoffDir_CreatesMissing(t *testing.T) {
	home := homeDir(t)
	if err := nightmedir.EnsureHandoffDir(); err != nil {
		t.Fatalf("EnsureHandoffDir: %v", err)
	}
	info, err := os.Stat(filepath.Join(home, ".nightme", "handoff"))
	if err != nil {
		t.Fatalf("expected handoff dir to exist: %v", err)
	}
	if !info.IsDir() {
		t.Errorf("handoff is not a directory")
	}
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != 0o700 {
			t.Errorf("handoff perm = %o, want 0700", perm)
		}
	}
}

// EnsureHandoffDir: already exists → no error, perm unchanged.
func TestEnsureHandoffDir_Idempotent(t *testing.T) {
	home := homeDir(t)
	dir := filepath.Join(home, ".nightme", "handoff")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("setup MkdirAll: %v", err)
	}
	if err := nightmedir.EnsureHandoffDir(); err != nil {
		t.Fatalf("EnsureHandoffDir on existing dir: %v", err)
	}
	info, _ := os.Stat(dir)
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != 0o755 {
			t.Errorf("EnsureHandoffDir must not change existing perm; got %o, want 0755", perm)
		}
	}
}

// EnsureHandoffDir: a pre-existing sibling file inside the
// directory must not be removed or modified.
func TestEnsureHandoffDir_PreservesExistingFiles(t *testing.T) {
	home := homeDir(t)
	dir := filepath.Join(home, ".nightme", "handoff")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("setup MkdirAll: %v", err)
	}
	sentinel := filepath.Join(dir, "demo.md")
	if err := os.WriteFile(sentinel, []byte("existing"), 0o644); err != nil {
		t.Fatalf("setup write sentinel: %v", err)
	}
	if err := nightmedir.EnsureHandoffDir(); err != nil {
		t.Fatalf("EnsureHandoffDir: %v", err)
	}
	body, _ := os.ReadFile(sentinel)
	if string(body) != "existing" {
		t.Errorf("sentinel file was disturbed: %q", body)
	}
}

// HandoffRelPath: always slash-form, independent of host
// platform — used in user-visible reply text where IM cards
// render forward slashes regardless of OS. Includes the `~/`
// prefix so the user knows where to find the file without
// knowing the .nightme convention.
func TestHandoffRelPath_AlwaysSlash(t *testing.T) {
	got := nightmedir.HandoffRelPath("demo")
	if got != "~/.nightme/handoff/demo.md" {
		t.Errorf("HandoffRelPath = %q, want ~/.nightme/handoff/demo.md", got)
	}
	if strings.Contains(got, `\`) {
		t.Errorf("HandoffRelPath must not contain backslash; got %q", got)
	}
}

// ValidateHandoffName: comprehensive character set coverage.
// One test instead of a table so the failure scenarios are
// named individually.
func TestValidateHandoffName(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantErr string
	}{
		{"empty", "", "must not be empty"},
		{"dot", ".", `reserved`},
		{"dotdot", "..", `reserved`},
		{"leading_dot", ".hidden", "must not start with '.'"},
		{"leading_dash", "-flag", "must not start with '-'"},
		{"slash", "foo/bar", "invalid character"},
		{"backslash", `foo\bar`, "invalid character"},
		{"colon", "foo:bar", "invalid character"},
		{"space", "foo bar", "invalid character"},
		{"unicode", "foö", "invalid character"},
		{"too_long", strings.Repeat("a", 65), "at most 64"},
		{"exactly_64", strings.Repeat("a", 64), ""},
		{"letters", "Demo", ""},
		{"digits", "123", ""},
		{"underscore", "demo_task", ""},
		{"dash_inside", "demo-task", ""},
		{"mixed", "Demo-1_task", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := nightmedir.ValidateHandoffName(tc.input)
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("ValidateHandoffName(%q) = %v, want nil", tc.input, err)
				}
				return
			}
			if err == nil {
				t.Errorf("ValidateHandoffName(%q) = nil, want error containing %q", tc.input, tc.wantErr)
				return
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("ValidateHandoffName(%q) = %v, want error containing %q", tc.input, err, tc.wantErr)
			}
		})
	}
}
