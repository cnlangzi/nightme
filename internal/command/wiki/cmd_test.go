package wiki

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSpec_NameAndAliases(t *testing.T) {
	f := NewFactory()
	spec := f.Spec()
	if spec.Name != "wiki" {
		t.Errorf("spec.Name = %q; want %q", spec.Name, "wiki")
	}
	want := "llm-wiki"
	found := false
	for _, a := range spec.Aliases {
		if a == want {
			found = true
		}
	}
	if !found {
		t.Errorf("Aliases missing %q: %v", want, spec.Aliases)
	}
}

func TestSpec_NoSubcommands(t *testing.T) {
	// /wiki is a single command, no subcommands, after the
	// init+update merge. Pin this so a future "let's add
	// /wiki rescan" change has to update the test and
	// surface the discussion.
	f := NewFactory()
	spec := f.Spec()
	if len(spec.Subcommands) != 0 {
		t.Errorf("Spec.Subcommands should be empty after merge; got %+v", spec.Subcommands)
	}
}

func TestSpec_UsageMentionsAgentFlag(t *testing.T) {
	f := NewFactory()
	spec := f.Spec()
	if !strings.Contains(spec.Usage, "[-a <agent>]") {
		t.Errorf("Spec.Usage should mention -a flag; got:\n%s", spec.Usage)
	}
}

func TestScaffold_NoGoCode_EmitsGitkeep(t *testing.T) {
	// Empty tempdir → 0 modules → emits .gitkeep so the
	// otherwise-empty modules/ is still tracked by git.
	dir := t.TempDir()
	written, err := Scaffold(dir)
	if err != nil {
		t.Fatalf("Scaffold: %v", err)
	}

	wantSuffix := []string{
		filepath.Join("wiki", "llms.txt"),
		filepath.Join("wiki", "architecture.md"),
		filepath.Join("wiki", "glossary.md"),
		filepath.Join("wiki", "modules", ".gitkeep"),
		"wiki.yml",
	}
	if len(written) != len(wantSuffix) {
		t.Fatalf("written = %d files, want %d (got %v)", len(written), len(wantSuffix), written)
	}
	for i, w := range wantSuffix {
		if !strings.HasSuffix(written[i], w) {
			t.Errorf("written[%d] = %q, want suffix %q", i, written[i], w)
		}
	}

	for path, wantPrefix := range map[string]string{
		filepath.Join(dir, "wiki", "llms.txt"):        "# " + filepath.Base(dir) + "\n",
		filepath.Join(dir, "wiki", "architecture.md"): "# Architecture\n",
		filepath.Join(dir, "wiki", "glossary.md"):     "# Glossary\n",
		filepath.Join(dir, "wiki.yml"):                "version: 1\n",
	} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("ReadFile %s: %v", path, err)
			continue
		}
		if !strings.HasPrefix(string(data), wantPrefix) {
			end := len(data)
			if end > 64 {
				end = 64
			}
			t.Errorf("%s: prefix mismatch; got %q, want prefix %q", path, string(data)[:end], wantPrefix)
		}
	}

	// No modules → empty wiki.yml.modules array, llms.txt
	// has empty Modules section.
	yml, _ := os.ReadFile(filepath.Join(dir, "wiki.yml"))
	if !strings.Contains(string(yml), "modules: []") {
		t.Errorf("wiki.yml should have empty modules list; got:\n%s", yml)
	}
	llms, _ := os.ReadFile(filepath.Join(dir, "wiki", "llms.txt"))
	if !strings.Contains(string(llms), "## Modules") ||
		!strings.Contains(string(llms), "## Reference") {
		t.Errorf("llms.txt missing Modules or Reference section; got:\n%s", llms)
	}
	// No `- [` link lines should appear when no modules.
	for _, line := range strings.Split(string(llms), "\n") {
		if strings.HasPrefix(line, "- [") && strings.Contains(line, "./modules/") {
			t.Errorf("llms.txt has module link when modules empty: %q", line)
		}
	}
}

func TestScaffold_WithGoModules_EmitsPerModuleSkeletons(t *testing.T) {
	// Fake a Go repo layout: 2 top-level modules + 1 nested.
	dir := t.TempDir()
	mustMkdir(t, filepath.Join(dir, "internal", "foo"))
	mustMkdir(t, filepath.Join(dir, "internal", "bar"))
	mustMkdir(t, filepath.Join(dir, "cmd", "demo"))
	mustWrite(t, filepath.Join(dir, "internal", "foo", "foo.go"), "package foo\n")
	mustWrite(t, filepath.Join(dir, "internal", "bar", "bar.go"), "package bar\n")
	mustWrite(t, filepath.Join(dir, "cmd", "demo", "main.go"), "package main\n")
	// A test-only dir should NOT become a module.
	mustMkdir(t, filepath.Join(dir, "internal", "testpkg"))
	mustWrite(t, filepath.Join(dir, "internal", "testpkg", "x_test.go"), "package testpkg\n")
	// A nested dir under a module should be ignored (leaf rule).
	mustMkdir(t, filepath.Join(dir, "internal", "foo", "sub"))
	mustWrite(t, filepath.Join(dir, "internal", "foo", "sub", "sub.go"), "package sub\n")
	// A hidden dir should be skipped.
	mustMkdir(t, filepath.Join(dir, "internal", ".hidden"))
	mustWrite(t, filepath.Join(dir, "internal", ".hidden", "x.go"), "package hidden\n")

	written, err := Scaffold(dir)
	if err != nil {
		t.Fatalf("Scaffold: %v", err)
	}

	// Expect: llms.txt, architecture.md, glossary.md,
	// modules/foo.md, modules/bar.md, modules/demo.md,
	// wiki.yml (7 files). No .gitkeep (modules[] non-empty).
	wantCount := 9
	if len(written) != wantCount {
		t.Fatalf("written = %d, want %d (got %v)", len(written), wantCount, written)
	}

	// Per-module files exist with expected H1. 5 modules:
	// foo, bar, demo, foo/sub (sub-package with files), and
	// testpkg (only _test.go files — still a module per
	// the no-leaf-rule policy).
	for path, wantH1 := range map[string]string{
		filepath.Join(dir, "wiki", "modules", "internal", "foo.md"):       "# foo\n",
		filepath.Join(dir, "wiki", "modules", "internal", "bar.md"):       "# bar\n",
		filepath.Join(dir, "wiki", "modules", "internal", "foo", "sub.md"): "# sub\n",
		filepath.Join(dir, "wiki", "modules", "internal", "testpkg.md"):   "# testpkg\n",
		filepath.Join(dir, "wiki", "modules", "cmd", "demo.md"):           "# demo\n",
	} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("ReadFile %s: %v", path, err)
			continue
		}
		if !strings.HasPrefix(string(data), wantH1) {
			t.Errorf("%s: missing H1 %q; got %q", path, wantH1, firstLine(data))
		}
	}

	// wiki.yml mirrors the module list with last_sha null.
	yml, _ := os.ReadFile(filepath.Join(dir, "wiki.yml"))
	for _, want := range []string{
		"modules:\n",
		"  - path: cmd/demo\n",
		"    file: cmd/demo.md\n",
		"    last_sha: null\n",
		"  - path: internal/bar\n",
		"    file: internal/bar.md\n",
		"  - path: internal/foo\n",
		"    file: internal/foo.md\n",
	} {
		if !strings.Contains(string(yml), want) {
			t.Errorf("wiki.yml missing %q; got:\n%s", want, yml)
		}
	}

	// llms.txt Modules section lists all five, sorted, with
	// the [name](./modules/<file>.md) link form.
	llms, _ := os.ReadFile(filepath.Join(dir, "wiki", "llms.txt"))
	for _, want := range []string{
		"## Modules\n",
		"- [bar](./modules/internal/bar.md)\n",
		"- [demo](./modules/cmd/demo.md)\n",
		"- [foo](./modules/internal/foo.md)\n",
	} {
		if !strings.Contains(string(llms), want) {
			t.Errorf("llms.txt missing %q; got:\n%s", want, llms)
		}
	}

	// .gitkeep must NOT be written when there are modules.
	if _, err := os.Stat(filepath.Join(dir, "wiki", "modules", ".gitkeep")); err == nil {
		t.Errorf(".gitkeep should not be created when modules[] is non-empty")
	}
}

func TestDiscoverModules_DeepWalkEveryNonEmptyDir(t *testing.T) {
	// No leaf rule: a directory with files AND a sub-package
	// with files BOTH become modules.
	dir := t.TempDir()
	mustMkdir(t, filepath.Join(dir, "internal", "foo"))
	mustWrite(t, filepath.Join(dir, "internal", "foo", "foo.go"), "package foo\n")
	mustMkdir(t, filepath.Join(dir, "internal", "foo", "sub"))
	mustWrite(t, filepath.Join(dir, "internal", "foo", "sub", "sub.go"), "package sub\n")

	mods, err := discoverModules(dir)
	if err != nil {
		t.Fatalf("discoverModules: %v", err)
	}
	wantPaths := []string{
		filepath.ToSlash(filepath.Join("internal", "foo")),
		filepath.ToSlash(filepath.Join("internal", "foo", "sub")),
	}
	if len(mods) != len(wantPaths) {
		t.Fatalf("discoverModules got %d, want %d; %+v", len(mods), len(wantPaths), mods)
	}
	for i, m := range mods {
		if m.Path != wantPaths[i] {
			t.Errorf("mods[%d].Path = %q, want %q", i, m.Path, wantPaths[i])
		}
	}
}

func TestDiscoverModules_TestFilesCountAsFiles(t *testing.T) {
	// A directory with ONLY _test.go files is still a module
	// (test files go into git; the directory is a real unit).
	dir := t.TempDir()
	mustMkdir(t, filepath.Join(dir, "internal", "good"))
	mustWrite(t, filepath.Join(dir, "internal", "good", "g.go"), "package good\n")
	mustMkdir(t, filepath.Join(dir, "internal", "testonly"))
	mustWrite(t, filepath.Join(dir, "internal", "testonly", "x_test.go"), "package testonly\n")
	mustMkdir(t, filepath.Join(dir, "internal", "h"))
	mustWrite(t, filepath.Join(dir, "internal", "h", "h.go"), "package h\n")

	mods, err := discoverModules(dir)
	if err != nil {
		t.Fatalf("discoverModules: %v", err)
	}
	wantPaths := []string{
		"internal/good",
		"internal/h",
		"internal/testonly",
	}
	if len(mods) != len(wantPaths) {
		t.Fatalf("got %d modules, want %d; got=%+v", len(mods), len(wantPaths), mods)
	}
	for i, m := range mods {
		if m.Path != wantPaths[i] {
			t.Errorf("mods[%d].Path = %q, want %q", i, m.Path, wantPaths[i])
		}
	}
}

func TestDiscoverModules_SortedOutput(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"zebra", "alpha", "mango"} {
		p := filepath.Join(dir, "internal", name)
		mustMkdir(t, p)
		mustWrite(t, filepath.Join(p, name+".go"), "package "+name+"\n")
	}
	mods, err := discoverModules(dir)
	if err != nil {
		t.Fatalf("discoverModules: %v", err)
	}
	if len(mods) != 3 {
		t.Fatalf("got %d modules", len(mods))
	}
	want := []string{
		filepath.ToSlash(filepath.Join("internal", "alpha")),
		filepath.ToSlash(filepath.Join("internal", "mango")),
		filepath.ToSlash(filepath.Join("internal", "zebra")),
	}
	for i, m := range mods {
		if m.Path != want[i] {
			t.Errorf("mods[%d].Path = %q, want %q", i, m.Path, want[i])
		}
	}
}

func TestScaffold_RefusesNonEmptyWikiDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "wiki"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "wiki", "existing.md"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := Scaffold(dir)
	if !errors.Is(err, ErrWikiAlreadyExists) {
		t.Fatalf("Scaffold err = %v; want ErrWikiAlreadyExists", err)
	}
}

func TestScaffold_RefusesWikiDirAsFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "wiki"), []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Scaffold(dir)
	if !errors.Is(err, ErrWikiAlreadyExists) {
		t.Fatalf("Scaffold err = %v; want ErrWikiAlreadyExists", err)
	}
}

func TestScaffold_RefusesExistingWikiYml(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "wiki.yml"), []byte("version: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Scaffold(dir)
	if !errors.Is(err, ErrWikiAlreadyExists) {
		t.Fatalf("Scaffold err = %v; want ErrWikiAlreadyExists", err)
	}
}

func TestScaffold_AllowsEmptyWikiDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "wiki"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Empty wiki/ is OK; Scaffold fills it.
	if _, err := Scaffold(dir); err != nil {
		t.Fatalf("Scaffold on empty wiki/: %v", err)
	}
}

func TestScaffold_NoTempLeftoverOnSuccess(t *testing.T) {
	dir := t.TempDir()
	if _, err := Scaffold(dir); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "wiki"))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".wiki-init-") {
			t.Errorf("temp file leaked: %s", e.Name())
		}
	}
}

func TestLlmsTxtSkeleton_StableShape(t *testing.T) {
	out := llmsTxtSkeletonWithModules("demo", nil)
	for _, want := range []string{
		"# demo\n",
		"## Architecture",
		"- [Architecture Overview](./architecture.md)",
		"## Modules",
		"## Reference",
		"- [Glossary](./glossary.md)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("llmsTxtSkeleton missing %q; got:\n%s", want, out)
		}
	}
}

// --- parseWikiArgs ---

func TestParseWikiArgs_AcceptsShortAgentFlag(t *testing.T) {
	got, err := parseWikiArgs([]string{"-a", "claude"})
	if err != nil {
		t.Fatalf("parseWikiArgs: %v", err)
	}
	if got.Agent != "claude" {
		t.Errorf("Agent = %q; want %q", got.Agent, "claude")
	}
}

func TestParseWikiArgs_AcceptsLongAgentFlag(t *testing.T) {
	got, err := parseWikiArgs([]string{"--agent", "codex"})
	if err != nil {
		t.Fatalf("parseWikiArgs: %v", err)
	}
	if got.Agent != "codex" {
		t.Errorf("Agent = %q; want %q", got.Agent, "codex")
	}
}

func TestParseWikiArgs_NoFlagsEmptyAgent(t *testing.T) {
	got, err := parseWikiArgs(nil)
	if err != nil {
		t.Fatalf("parseWikiArgs: %v", err)
	}
	if got.Agent != "" {
		t.Errorf("Agent = %q; want empty", got.Agent)
	}
}

func TestParseWikiArgs_RejectsUnknownFlag(t *testing.T) {
	_, err := parseWikiArgs([]string{"--force"})
	if err == nil {
		t.Errorf("parseWikiArgs accepted --force; want error")
	}
	if !strings.Contains(err.Error(), "unknown flag") {
		t.Errorf("err = %q; want \"unknown flag\" wording", err)
	}
}

func TestParseWikiArgs_RejectsStrayPositional(t *testing.T) {
	_, err := parseWikiArgs([]string{"extra"})
	if err == nil {
		t.Errorf("parseWikiArgs accepted stray positional; want error")
	}
	if !strings.Contains(err.Error(), "unexpected positional") {
		t.Errorf("err = %q; want \"unexpected positional\" wording", err)
	}
}

func TestParseWikiArgs_RejectsMissingAgentValue(t *testing.T) {
	_, err := parseWikiArgs([]string{"-a"})
	if err == nil {
		t.Errorf("parseWikiArgs accepted -a with no value; want error")
	}
	if !strings.Contains(err.Error(), "missing value") {
		t.Errorf("err = %q; want \"missing value\" wording", err)
	}
}

// --- Scaffold default state ---

func TestScaffold_AlwaysWritesAgentNull(t *testing.T) {
	// /wiki init takes no flags, so wiki.yml.agent is always
	// YAML null at init time. The first /wiki update records
	// the agent it ran with into this field.
	dir := t.TempDir()
	if _, err := Scaffold(dir); err != nil {
		t.Fatalf("Scaffold: %v", err)
	}
	yml, err := os.ReadFile(filepath.Join(dir, "wiki.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(yml), "agent: null\n") {
		t.Errorf("wiki.yml should contain agent: null after init; got:\n%s", yml)
	}
	if !strings.Contains(string(yml), "include: []\n") {
		t.Errorf("wiki.yml should contain include: [] after init; got:\n%s", yml)
	}
}

// --- ignore filter ---

func TestIgnoreFilter_BuiltinWikiSkipped(t *testing.T) {
	dir := t.TempDir()
	mustMkdir(t, filepath.Join(dir, "wiki"))
	mustWrite(t, filepath.Join(dir, "wiki", "x.md"), "# x\n")
	mustMkdir(t, filepath.Join(dir, "internal", "foo"))
	mustWrite(t, filepath.Join(dir, "internal", "foo", "foo.go"), "package foo\n")

	mods, err := discoverModules(dir)
	if err != nil {
		t.Fatalf("discoverModules: %v", err)
	}
	if len(mods) != 1 || mods[0].Path != filepath.ToSlash(filepath.Join("internal", "foo")) {
		t.Errorf("expected only internal/foo; got %+v", mods)
	}
}

func TestIgnoreFilter_GitignorePatternsApply(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, ".gitignore"), "node_modules\n*.tmp\n")
	mustMkdir(t, filepath.Join(dir, "node_modules"))
	mustWrite(t, filepath.Join(dir, "node_modules", "x.go"), "package nm\n")
	mustMkdir(t, filepath.Join(dir, "internal", "foo"))
	mustWrite(t, filepath.Join(dir, "internal", "foo", "foo.go"), "package foo\n")
	mustWrite(t, filepath.Join(dir, "scratch.tmp"), "junk")

	mods, err := discoverModules(dir)
	if err != nil {
		t.Fatalf("discoverModules: %v", err)
	}
	if len(mods) != 1 || mods[0].Path != filepath.ToSlash(filepath.Join("internal", "foo")) {
		t.Errorf("expected only internal/foo; got %+v", mods)
	}
}

func TestIgnoreFilter_GitignoreNegationUnignores(t *testing.T) {
	// Standard gitignore: "*.log" ignored, "!keep.log" un-ignores.
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, ".gitignore"), "*.log\n!keep.log\n")
	mustWrite(t, filepath.Join(dir, "junk.log"), "junk")
	mustWrite(t, filepath.Join(dir, "keep.log"), "keep me")
	mustMkdir(t, filepath.Join(dir, "internal", "foo"))
	mustWrite(t, filepath.Join(dir, "internal", "foo", "foo.go"), "package foo\n")

	mods, err := discoverModules(dir)
	if err != nil {
		t.Fatalf("discoverModules: %v", err)
	}
	// expected modules: foo (Go pkg), keep.log (the !negation
	// un-ignores the file, but it's not a directory so does
	// not become a module — the walker only checks dirs).
	// junk.log: ignored, so its parent dir (cwd) is a
	// container, no Go files at top level → no module from it.
	if len(mods) != 1 {
		t.Errorf("expected 1 module; got %d: %+v", len(mods), mods)
	}
}

func TestIgnoreFilter_DotPrefixAlwaysHidden(t *testing.T) {
	dir := t.TempDir()
	// .gitignore does NOT mention .vscode — the dot-prefix
	// rule still hides it.
	mustMkdir(t, filepath.Join(dir, ".vscode"))
	mustWrite(t, filepath.Join(dir, ".vscode", "settings.json"), "{}")
	mustMkdir(t, filepath.Join(dir, "internal", "foo"))
	mustWrite(t, filepath.Join(dir, "internal", "foo", "foo.go"), "package foo\n")

	mods, err := discoverModules(dir)
	if err != nil {
		t.Fatalf("discoverModules: %v", err)
	}
	for _, m := range mods {
		if strings.HasPrefix(m.Path, ".") {
			t.Errorf("dot-prefix dir leaked into modules: %q", m.Path)
		}
	}
}

func TestIgnoreFilter_IncludeExemptsDotDir(t *testing.T) {
	dir := t.TempDir()
	// include: [.github] exempts .github from dot-prefix
	mustWrite(t, filepath.Join(dir, "wiki.yml"), "version: 1\ninclude:\n  - .github\n")
	mustMkdir(t, filepath.Join(dir, ".github", "workflows"))
	mustWrite(t, filepath.Join(dir, ".github", "workflows", "ci.yml"), "name: ci\n")
	mustMkdir(t, filepath.Join(dir, "internal", "foo"))
	mustWrite(t, filepath.Join(dir, "internal", "foo", "foo.go"), "package foo\n")

	mods, err := discoverModules(dir)
	if err != nil {
		t.Fatalf("discoverModules: %v", err)
	}
	var sawGithub bool
	for _, m := range mods {
		if m.Path == ".github/workflows" {
			sawGithub = true
		}
	}
	if !sawGithub {
		t.Errorf(".github/workflows should be included; got %+v", mods)
	}
}

func TestIgnoreFilter_IncludeExactOnlyNotRecursive(t *testing.T) {
	// include: [.github] exempts .github itself but NOT its
	// hidden children (.github/.private stays hidden).
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "wiki.yml"), "version: 1\ninclude:\n  - .github\n")
	mustMkdir(t, filepath.Join(dir, ".github"))
	mustWrite(t, filepath.Join(dir, ".github", "README.md"), "hi")
	mustMkdir(t, filepath.Join(dir, ".github", ".private"))
	mustWrite(t, filepath.Join(dir, ".github", ".private", "secret.go"), "package secret\n")

	mods, err := discoverModules(dir)
	if err != nil {
		t.Fatalf("discoverModules: %v", err)
	}
	for _, m := range mods {
		if m.Path == ".github/.private" {
			t.Errorf(".github/.private should stay hidden (one-level include); got %+v", mods)
		}
	}
}

func TestIgnoreFilter_MissingGitignoreOK(t *testing.T) {
	dir := t.TempDir()
	mustMkdir(t, filepath.Join(dir, "internal", "foo"))
	mustWrite(t, filepath.Join(dir, "internal", "foo", "foo.go"), "package foo\n")

	mods, err := discoverModules(dir)
	if err != nil {
		t.Fatalf("discoverModules with no .gitignore: %v", err)
	}
	if len(mods) != 1 {
		t.Errorf("expected 1 module; got %d", len(mods))
	}
}

func TestLoadWikiInclude_InlineArray(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "wiki.yml"), "version: 1\ninclude: [.github, .config]\n")
	got, err := loadWikiInclude(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{".github", ".config"}
	if len(got) != len(want) {
		t.Fatalf("got %v; want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("[%d] = %q; want %q", i, got[i], want[i])
		}
	}
}

func TestLoadGitignore_CommentsAndBlanks(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, ".gitignore"), "# header comment\n\nnode_modules\n# trailing\n*.log\n")
	p, err := loadGitignore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if p == nil {
		t.Fatal("parser is nil")
	}
	if got := len(p.patterns); got != 2 {
		t.Errorf("expected 2 patterns (comments/blanks stripped); got %d", got)
	}
}

func TestShouldSkip_BuiltinWikiCannotBeOverridden(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "wiki.yml"), "version: 1\ninclude:\n  - wiki\n")
	filter, err := loadIgnoreFilter(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !filter.shouldSkip("wiki", true) {
		t.Errorf("wiki/ should be skipped even when in include; the self-recursion guard overrides everything")
	}
}

// --- /wiki sync flow ---

// mockGit is the test fake. HEAD is fixed; IsClean returns
// true; ChangedFiles returns whatever changes map is set for
// the path filter.
type mockGit struct {
	head    string
	clean   bool
	changes map[string][]string
}

func (m mockGit) Head(_ string) (string, error) { return m.head, nil }
func (m mockGit) IsClean(_ string) (bool, error) { return m.clean, nil }
func (m mockGit) ChangedFiles(_, _, pathFilter string) ([]string, error) {
	return m.changes[pathFilter], nil
}

func cleanGit() mockGit {
	return mockGit{head: "deadbeef", clean: true, changes: map[string][]string{}}
}

func TestSync_FreshScaffoldMarksFreshTrue(t *testing.T) {
	dir := t.TempDir()
	mustMkdir(t, filepath.Join(dir, "internal", "foo"))
	mustWrite(t, filepath.Join(dir, "internal", "foo", "foo.go"), "package foo\n")
	res, err := SyncWith(dir, "", cleanGit(), stubDispatcher{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Fresh {
		t.Errorf("Fresh = false; want true on first run")
	}
	// Core files exist
	for _, p := range []string{"wiki.yml", "wiki/llms.txt", "wiki/architecture.md", "wiki/glossary.md", "wiki/modules/internal/foo.md"} {
		if _, err := os.Stat(filepath.Join(dir, p)); err != nil {
			t.Errorf("expected %s after fresh sync: %v", p, err)
		}
	}
}

func TestSync_ReconcileAddsNewModule(t *testing.T) {
	dir := t.TempDir()
	mustMkdir(t, filepath.Join(dir, "internal", "foo"))
	mustWrite(t, filepath.Join(dir, "internal", "foo", "foo.go"), "package foo\n")
	if _, err := SyncWith(dir, "", cleanGit(), stubDispatcher{}); err != nil {
		t.Fatal(err)
	}
	// Add a new module after first sync.
	mustMkdir(t, filepath.Join(dir, "internal", "bar"))
	mustWrite(t, filepath.Join(dir, "internal", "bar", "bar.go"), "package bar\n")

	res, err := SyncWith(dir, "", cleanGit(), stubDispatcher{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Fresh {
		t.Errorf("Fresh = true; want false on second run")
	}
	if len(res.Added) != 1 || res.Added[0] != "internal/bar" {
		t.Errorf("Added = %v; want [internal/bar]", res.Added)
	}
	if len(res.Removed) != 0 {
		t.Errorf("Removed = %v; want empty", res.Removed)
	}
	// New module's wiki file should exist.
	if _, err := os.Stat(filepath.Join(dir, "wiki/modules/internal/bar.md")); err != nil {
		t.Errorf("expected wiki/modules/internal/bar.md: %v", err)
	}
}

func TestSync_ReconcileMarksRemoved(t *testing.T) {
	dir := t.TempDir()
	mustMkdir(t, filepath.Join(dir, "internal", "foo"))
	mustMkdir(t, filepath.Join(dir, "internal", "bar"))
	mustWrite(t, filepath.Join(dir, "internal", "foo", "foo.go"), "package foo\n")
	mustWrite(t, filepath.Join(dir, "internal", "bar", "bar.go"), "package bar\n")
	if _, err := SyncWith(dir, "", cleanGit(), stubDispatcher{}); err != nil {
		t.Fatal(err)
	}
	// Delete one module.
	if err := os.RemoveAll(filepath.Join(dir, "internal", "bar")); err != nil {
		t.Fatal(err)
	}

	res, err := SyncWith(dir, "", cleanGit(), stubDispatcher{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Removed) != 1 || res.Removed[0] != "internal/bar" {
		t.Errorf("Removed = %v; want [internal/bar]", res.Removed)
	}
	// yml should mark it removed.
	ymlData, _ := os.ReadFile(filepath.Join(dir, "wiki.yml"))
	yml, _ := parseWikiYml(ymlData)
	var foundRemoved bool
	for _, m := range yml.Modules {
		if m.Path == "internal/bar" && m.Removed {
			foundRemoved = true
		}
	}
	if !foundRemoved {
		t.Errorf("wiki.yml should mark internal/bar as removed; modules:\n%s", ymlData)
	}
	// Wiki file DELETED (decision: removed modules' files are
	// deleted; yml entry stays for audit).
	if _, err := os.Stat(filepath.Join(dir, "wiki/modules/internal/bar.md")); err == nil {
		t.Errorf("wiki/modules/internal/bar.md should be deleted; still on disk")
	}
	// llms.txt should NOT list removed module.
	llmsData, _ := os.ReadFile(filepath.Join(dir, "wiki/llms.txt"))
	if strings.Contains(string(llmsData), "bar") {
		t.Errorf("llms.txt should NOT list removed module bar; got:\n%s", llmsData)
	}
}

func TestSync_PreservesLLMContent(t *testing.T) {
	// Module whose last_sha is non-null should NOT be
	// overwritten by stub regeneration.
	dir := t.TempDir()
	mustMkdir(t, filepath.Join(dir, "internal", "foo"))
	mustWrite(t, filepath.Join(dir, "internal", "foo", "foo.go"), "package foo\n")
	if _, err := SyncWith(dir, "", cleanGit(), stubDispatcher{}); err != nil {
		t.Fatal(err)
	}
	// Hand-edit wiki.yml to mark foo as having LLM content.
	sha := "abc123"
	ymlData, _ := os.ReadFile(filepath.Join(dir, "wiki.yml"))
	yml, _ := parseWikiYml(ymlData)
	for i := range yml.Modules {
		if yml.Modules[i].Path == "internal/foo" {
			yml.Modules[i].LastSHA = &sha
		}
	}
	encoded, _ := encodeWikiYml(yml)
	atomicWrite(filepath.Join(dir, "wiki.yml"), string(encoded))
	// Hand-write a "real" content into foo.md (NOT a stub).
	realContent := "# foo\n\n## Public Surface\n\nLLM-WRITTEN CONTENT DO NOT OVERWRITE\n"
	atomicWrite(filepath.Join(dir, "wiki/modules/internal/foo.md"), realContent)

	res, err := SyncWith(dir, "", cleanGit(), stubDispatcher{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Preserved) != 1 || res.Preserved[0] != "internal/foo.md" {
		t.Errorf("Preserved = %v; want [internal/foo.md]", res.Preserved)
	}
	if len(res.Written) != 0 {
		t.Errorf("Written = %v; want empty (foo is preserved)", res.Written)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "wiki/modules/internal/foo.md"))
	if !strings.Contains(string(data), "LLM-WRITTEN CONTENT") {
		t.Errorf("LLM-written content was overwritten:\n%s", data)
	}
}

func TestSync_HalfStateRecovers(t *testing.T) {
	// Old behavior refused half-state; new behavior recovers
	// (see TestSync_RecoversFromMissingWikiDir / ...Yml).
	// This test was previously named HalfStateRefuses; it
	// stays here only as a marker so a future "let's
	// re-add hard refusal" change surfaces.
}

func TestSync_ReconcileRestoresRemovedModule(t *testing.T) {
	// A module that was removed and then re-introduced in
	// source should be marked as added (not stay "removed").
	dir := t.TempDir()
	mustMkdir(t, filepath.Join(dir, "internal", "foo"))
	mustWrite(t, filepath.Join(dir, "internal", "foo", "foo.go"), "package foo\n")
	if _, err := SyncWith(dir, "", cleanGit(), stubDispatcher{}); err != nil {
		t.Fatal(err)
	}
	// Remove module.
	if err := os.RemoveAll(filepath.Join(dir, "internal", "foo")); err != nil {
		t.Fatal(err)
	}
	if _, err := SyncWith(dir, "", cleanGit(), stubDispatcher{}); err != nil {
		t.Fatal(err)
	}
	// Re-introduce module.
	mustMkdir(t, filepath.Join(dir, "internal", "foo"))
	mustWrite(t, filepath.Join(dir, "internal", "foo", "foo.go"), "package foo\n")
	res, err := SyncWith(dir, "", cleanGit(), stubDispatcher{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Added) != 1 || res.Added[0] != "internal/foo" {
		t.Errorf("Added = %v; want [internal/foo] (re-introduced)", res.Added)
	}
	// yml should NOT have removed:true anymore.
	ymlData, _ := os.ReadFile(filepath.Join(dir, "wiki.yml"))
	if strings.Contains(string(ymlData), "removed: true") {
		t.Errorf("yml should not have removed:true after re-introduction:\n=== full yml ===\n%s\n=== end ===", ymlData)
	}
}

func TestSync_StubRegeneratesFileLayout(t *testing.T) {
	dir := t.TempDir()
	mustMkdir(t, filepath.Join(dir, "internal", "foo"))
	mustWrite(t, filepath.Join(dir, "internal", "foo", "a.go"), "package foo\n\nfunc A() {}\n")
	mustWrite(t, filepath.Join(dir, "internal", "foo", "b.go"), "package foo\n\nfunc B() {\n  return\n}\n")
	mustWrite(t, filepath.Join(dir, "internal", "foo", "a_test.go"), "package foo\n// test\n")
	if _, err := SyncWith(dir, "", cleanGit(), stubDispatcher{}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "wiki/modules/internal/foo.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"# foo\n",
		"## File Layout\n",
		"| `a.go` |",
		"| `b.go` |",
		"<!-- sources -->",
		"- internal/foo/a.go",
		"- internal/foo/b.go",
	} {
		if !strings.Contains(string(data), want) {
			t.Errorf("stub missing %q; got:\n%s", want, data)
		}
	}
	if strings.Contains(string(data), "_test.go") {
		t.Errorf("stub should NOT list test files; got:\n%s", data)
	}
}

func TestParseWikiYml_RoundTrip(t *testing.T) {
	src := `version: 1
last_commit: null
agent: null
include: [.github]
modules:
  - path: internal/command/gtw
    file: gtw.md
    last_sha: null
  - path: internal/agent
    file: agent.md
    last_sha: null
`
	parsed, err := parseWikiYml([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if parsed.Version != 1 {
		t.Errorf("Version = %d", parsed.Version)
	}
	if parsed.LastCommit != nil {
		t.Errorf("LastCommit should be nil")
	}
	if parsed.Agent != nil {
		t.Errorf("Agent should be nil")
	}
	if len(parsed.Include) != 1 || parsed.Include[0] != ".github" {
		t.Errorf("Include = %v", parsed.Include)
	}
	if len(parsed.Modules) != 2 {
		t.Fatalf("Modules len = %d", len(parsed.Modules))
	}
	if parsed.Modules[0].Path != "internal/command/gtw" {
		t.Errorf("Modules[0].Path = %q", parsed.Modules[0].Path)
	}
	if parsed.Modules[0].LastSHA != nil {
		t.Errorf("Modules[0].LastSHA should be nil")
	}
}

func TestReadSourceFiles_SortedAndFiltered(t *testing.T) {
	dir := t.TempDir()
	mustMkdir(t, filepath.Join(dir, "internal", "foo"))
	mustWrite(t, filepath.Join(dir, "internal", "foo", "z.go"), "package foo\n")
	mustWrite(t, filepath.Join(dir, "internal", "foo", "a.go"), "package foo\n// 2 lines\n")
	mustWrite(t, filepath.Join(dir, "internal", "foo", "z_test.go"), "package foo\n")
	mustWrite(t, filepath.Join(dir, "internal", "foo", ".hidden"), "junk")

	sources, err := readSourceFiles(dir, "internal/foo")
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 2 {
		t.Fatalf("len(sources) = %d, want 2; got %+v", len(sources), sources)
	}
	if sources[0].Name != "a.go" || sources[1].Name != "z.go" {
		t.Errorf("order: got %s, %s; want a.go, z.go", sources[0].Name, sources[1].Name)
	}
	for _, s := range sources {
		if strings.HasSuffix(s.Name, "_test.go") {
			t.Errorf("test file leaked: %s", s.Name)
		}
		if strings.HasPrefix(s.Name, ".") {
			t.Errorf("hidden file leaked: %s", s.Name)
		}
	}
}

// --- helpers ---

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func firstLine(data []byte) string {
	if i := strings.IndexByte(string(data), '\n'); i >= 0 {
		return string(data[:i+1])
	}
	return string(data)
}
