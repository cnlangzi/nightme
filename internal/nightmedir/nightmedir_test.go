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

// Path: always forward-slash regardless of OS separator.
func TestPath_ForwardSlash(t *testing.T) {
	got := nightmedir.Path("/tmp/foo")
	if !strings.HasSuffix(got, "/.nightme") {
		t.Errorf("Path = %q, want suffix %q", got, "/.nightme")
	}
	if strings.Contains(got, `\`) {
		t.Errorf("Path must not contain backslash; got %q", got)
	}
}

// FilePath: joins cwd + DirName + name with forward slashes.
func TestFilePath_ForwardSlash(t *testing.T) {
	got := nightmedir.FilePath("/tmp/foo", "handoff.md")
	if !strings.HasSuffix(got, "/.nightme/handoff.md") {
		t.Errorf("FilePath = %q, want suffix %q", got, "/.nightme/handoff.md")
	}
	if strings.Contains(got, `\`) {
		t.Errorf("FilePath must not contain backslash; got %q", got)
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
