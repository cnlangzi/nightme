package gtw

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/chatsession"
)

// sandboxHome points os.UserHomeDir at a scratch directory so the
// user-level gtw.yml read / write paths never touch the developer's
// real config. USERPROFILE is set too — os.UserHomeDir reads it on
// Windows.
func sandboxHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return home
}

// userYmlPath returns the path of the user-level gtw.yml inside the
// sandboxed home.
func userYmlPath(home string) string {
	return filepath.Join(home, ".nightme", "gtw.yml")
}

// writeUserYml drops a gtw.yml with the given content into the
// sandboxed home, creating ~/.nightme on the way.
func writeUserYml(t *testing.T, home, content string) string {
	t.Helper()
	path := userYmlPath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir .nightme: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write gtw.yml: %v", err)
	}
	return path
}

// originGit is a GitRunner that answers `git remote get-url origin`
// with a canned URL and rejects every other command.
type originGit struct {
	url string
	err error
}

func (g *originGit) Run(_ context.Context, _ string, args ...string) (string, string, error) {
	if len(args) >= 3 && args[0] == "remote" && args[1] == "get-url" {
		if g.err != nil {
			return "", "error: No such remote 'origin'", g.err
		}
		return g.url, "", nil
	}
	return "", "", errors.New("unexpected git call: " + strings.Join(args, " "))
}

// -----------------------------------------------------------------------------
// repoKey
// -----------------------------------------------------------------------------

func TestRepoKey_ParsesOriginURL(t *testing.T) {
	cases := []struct {
		name string
		url  string
		want string
	}{
		{"scp style", "git@github.com:cnlangzi/nightme.git", "github.com/cnlangzi/nightme"},
		{"https", "https://github.com/cnlangzi/nightme.git", "github.com/cnlangzi/nightme"},
		{"https without .git", "https://github.com/cnlangzi/nightme", "github.com/cnlangzi/nightme"},
		{"ssh url", "ssh://git@github.com/cnlangzi/nightme.git", "github.com/cnlangzi/nightme"},
		{"gitlab", "https://gitlab.com/myteam/myrepo.git", "gitlab.com/myteam/myrepo"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			git := &originGit{url: tc.url}
			got, err := repoKey(context.Background(), "/w", git)
			if err != nil {
				t.Fatalf("repoKey(%q): %v", tc.url, err)
			}
			if got != tc.want {
				t.Errorf("repoKey(%q) = %q, want %q", tc.url, got, tc.want)
			}
		})
	}
}

func TestRepoKey_NoOriginRemote(t *testing.T) {
	git := &originGit{err: errors.New("no origin")}
	got, err := repoKey(context.Background(), "/w", git)
	if err == nil {
		t.Fatalf("expected an error when there is no origin remote, got %q", got)
	}
	if got != "" {
		t.Errorf("repoKey returned %q alongside an error, want empty", got)
	}
}

// -----------------------------------------------------------------------------
// resolveLanguage
// -----------------------------------------------------------------------------

func TestResolveLanguage_ScopePrecedence(t *testing.T) {
	cfg := Config{
		Lang: "简体中文",
		Repos: map[string]RepoConfig{
			"github.com/cnlangzi/nightme": {Lang: "zh-CN"},
		},
	}
	git := &originGit{url: "git@github.com:cnlangzi/nightme.git"}

	got := resolveLanguage(context.Background(), cfg, "/w", git)
	if got != "zh-CN" {
		t.Errorf("repo scope should win, got %q", got)
	}

	lang, scope := resolveLanguageScope(context.Background(), cfg, "/w", git)
	if lang != "zh-CN" || scope != scopeRepo {
		t.Errorf("got (%q, %q), want (%q, %q)", lang, scope, "zh-CN", scopeRepo)
	}

	// No repo-scope entry → the user scope applies.
	cfg.Repos = map[string]RepoConfig{"gitlab.com/other/repo": {Lang: "fr"}}
	lang, scope = resolveLanguageScope(context.Background(), cfg, "/w", git)
	if lang != "简体中文" || scope != scopeGlobal {
		t.Errorf("got (%q, %q), want (%q, %q)", lang, scope, "简体中文", scopeGlobal)
	}

	// Neither scope set → the default.
	cfg.Lang = ""
	lang, scope = resolveLanguageScope(context.Background(), cfg, "/w", git)
	if lang != defaultLanguage || scope != scopeDefault {
		t.Errorf("got (%q, %q), want (%q, %q)", lang, scope, defaultLanguage, scopeDefault)
	}
}

func TestResolveLanguage_UnreadableOriginFallsBack(t *testing.T) {
	cfg := Config{Lang: "简体中文"}
	git := &originGit{err: errors.New("git missing")}
	lang, scope := resolveLanguageScope(context.Background(), cfg, "/w", git)
	if lang != "简体中文" || scope != scopeGlobal {
		t.Errorf("got (%q, %q), want (%q, %q)", lang, scope, "简体中文", scopeGlobal)
	}
}

func TestResolveLanguage_EmptyValueCountsAsUnset(t *testing.T) {
	cfg := Config{
		Lang: "   ",
		Repos: map[string]RepoConfig{
			"github.com/cnlangzi/nightme": {Lang: ""},
		},
	}
	git := &originGit{url: "git@github.com:cnlangzi/nightme.git"}
	lang, scope := resolveLanguageScope(context.Background(), cfg, "/w", git)
	if lang != defaultLanguage || scope != scopeDefault {
		t.Errorf("got (%q, %q), want (%q, %q)", lang, scope, defaultLanguage, scopeDefault)
	}
}

// -----------------------------------------------------------------------------
// applyLanguage
// -----------------------------------------------------------------------------

func TestApplyLanguage_ReplacesPlaceholder(t *testing.T) {
	out := applyLanguage("write it in "+placeholderLanguage, "简体中文")
	if strings.Contains(out, placeholderLanguage) {
		t.Errorf("placeholder survived: %q", out)
	}
	if !strings.Contains(out, "write it in 简体中文") {
		t.Errorf("language not applied: %q", out)
	}
}

func TestApplyLanguage_EmptyLanguageUsesDefault(t *testing.T) {
	out := applyLanguage("write it in "+placeholderLanguage, "")
	if !strings.Contains(out, "write it in "+defaultLanguage) {
		t.Errorf("expected the default language, got %q", out)
	}
}

// -----------------------------------------------------------------------------
// gtw.yml write path
// -----------------------------------------------------------------------------

func TestSetGlobalLang_CreatesFile(t *testing.T) {
	home := sandboxHome(t)
	if err := setGlobalLang("简体中文"); err != nil {
		t.Fatalf("setGlobalLang: %v", err)
	}
	data, err := os.ReadFile(userYmlPath(home))
	if err != nil {
		t.Fatalf("read gtw.yml: %v", err)
	}
	if !strings.Contains(string(data), "lang: 简体中文") {
		t.Errorf("unexpected content:\n%s", data)
	}
}

func TestSetGlobalLang_KeepsHooksCommentsAndUnknownKeys(t *testing.T) {
	home := sandboxHome(t)
	src := "# my hooks\nfix:\n  hooks:\n    before:\n      - run: codegraph init\nunknown_key: keep-me\n"
	writeUserYml(t, home, src)

	if err := setGlobalLang("简体中文"); err != nil {
		t.Fatalf("setGlobalLang: %v", err)
	}
	data, err := os.ReadFile(userYmlPath(home))
	if err != nil {
		t.Fatalf("read gtw.yml: %v", err)
	}
	got := string(data)
	for _, want := range []string{"# my hooks", "codegraph init", "unknown_key: keep-me", "lang: 简体中文"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestSetRepoLang_CreatesReposSection(t *testing.T) {
	home := sandboxHome(t)
	if err := setRepoLang("github.com/cnlangzi/nightme", "zh-CN"); err != nil {
		t.Fatalf("setRepoLang: %v", err)
	}
	data, err := os.ReadFile(userYmlPath(home))
	if err != nil {
		t.Fatalf("read gtw.yml: %v", err)
	}
	got := string(data)
	if !strings.Contains(got, "repos:") {
		t.Errorf("missing repos section:\n%s", got)
	}
	if !strings.Contains(got, "github.com/cnlangzi/nightme:") {
		t.Errorf("missing repo key:\n%s", got)
	}
	if !strings.Contains(got, "lang: zh-CN") {
		t.Errorf("missing repo language:\n%s", got)
	}
}

func TestUnsetGlobalLang_RemovesEntry(t *testing.T) {
	home := sandboxHome(t)
	writeUserYml(t, home, "lang: 简体中文\nfix:\n  hooks:\n    before:\n      - run: x\n")

	if err := unsetGlobalLang(); err != nil {
		t.Fatalf("unsetGlobalLang: %v", err)
	}
	data, _ := os.ReadFile(userYmlPath(home))
	got := string(data)
	if strings.Contains(got, "lang:") {
		t.Errorf("lang entry survived:\n%s", got)
	}
	if !strings.Contains(got, "codegraph") && !strings.Contains(got, "run: x") {
		t.Errorf("hooks were dropped:\n%s", got)
	}
}

func TestUnsetRepoLang_RemovesEntry(t *testing.T) {
	home := sandboxHome(t)
	writeUserYml(t, home, "repos:\n  github.com/cnlangzi/nightme:\n    lang: zh-CN\n")

	if err := unsetRepoLang("github.com/cnlangzi/nightme"); err != nil {
		t.Fatalf("unsetRepoLang: %v", err)
	}
	data, _ := os.ReadFile(userYmlPath(home))
	got := string(data)
	if strings.Contains(got, "lang:") || strings.Contains(got, "github.com/cnlangzi/nightme") {
		t.Errorf("repo entry survived:\n%s", got)
	}
}

func TestWriteUserConfig_RefusesMalformedFile(t *testing.T) {
	home := sandboxHome(t)
	bad := "lang: [unclosed\n"
	writeUserYml(t, home, bad)

	if err := setGlobalLang("zh"); err == nil {
		t.Fatalf("expected an error for a malformed gtw.yml")
	}
	data, _ := os.ReadFile(userYmlPath(home))
	if string(data) != bad {
		t.Fatalf("malformed file was modified:\n%s", data)
	}
}

// TestSetGlobalLang_CommentOnlyFile covers a gtw.yml that holds
// nothing but comments — a valid YAML document with no root. The
// write path must treat it as an empty config, not as a broken
// one.
func TestSetGlobalLang_CommentOnlyFile(t *testing.T) {
	home := sandboxHome(t)
	writeUserYml(t, home, "# only a comment\n")

	if err := setGlobalLang("zh"); err != nil {
		t.Fatalf("setGlobalLang on a comments-only file: %v", err)
	}
	data, err := os.ReadFile(userYmlPath(home))
	if err != nil {
		t.Fatalf("read gtw.yml: %v", err)
	}
	got := string(data)
	if !strings.Contains(got, "lang: zh") {
		t.Errorf("language not written:\n%s", got)
	}
	if !strings.Contains(got, "# only a comment") {
		t.Errorf("comment dropped:\n%s", got)
	}
}

// TestWriteUserConfig_ConcurrentWritesDoNotLoseUpdates covers two
// chats writing the shared gtw.yml at the same time. Without
// serialisation the read → mutate → rename cycles interleave and
// one of the two updates is silently lost.
func TestWriteUserConfig_ConcurrentWritesDoNotLoseUpdates(t *testing.T) {
	sandboxHome(t)

	const writers = 8
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := setRepoLang(fmt.Sprintf("github.com/o/repo-%d", i), "x"); err != nil {
				t.Errorf("setRepoLang: %v", err)
			}
		}(i)
	}
	wg.Wait()

	cfg, notes := Load()
	if notes.HasWarnings() {
		t.Fatalf("unexpected load warnings: %v", notes.Warnings)
	}
	for i := 0; i < writers; i++ {
		key := fmt.Sprintf("github.com/o/repo-%d", i)
		if _, ok := cfg.Repos[key]; !ok {
			t.Errorf("lost update: %s is missing from %v", key, cfg.Repos)
		}
	}
}

// TestSetGlobalLang_EmptyDocument covers a gtw.yml that holds no
// content at all — a bare `---` — which is an empty config, not a
// broken one.
func TestSetGlobalLang_EmptyDocument(t *testing.T) {
	home := sandboxHome(t)
	writeUserYml(t, home, "---\n")

	if err := setGlobalLang("zh"); err != nil {
		t.Fatalf("setGlobalLang on an empty document: %v", err)
	}
	data, err := os.ReadFile(userYmlPath(home))
	if err != nil {
		t.Fatalf("read gtw.yml: %v", err)
	}
	if !strings.Contains(string(data), "lang: zh") {
		t.Errorf("language not written:\n%s", data)
	}
}

// TestRunNew_DraftSurfacesLoadNotes covers the /gtw new draft
// path: a malformed gtw.yml must not silently fall back to the
// default output language.
func TestRunNew_DraftSurfacesLoadNotes(t *testing.T) {
	rig := newNewIssueRig(t, "chat-new-load-notes")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".nightme"), 0o700); err != nil {
		t.Fatalf("mkdir .nightme: %v", err)
	}
	if err := os.WriteFile(filepath.Join(home, ".nightme", "gtw.yml"),
		[]byte("lang: [unclosed\n"), 0o600); err != nil {
		t.Fatalf("write gtw.yml: %v", err)
	}

	rig.handle(t)

	var sent strings.Builder
	for _, m := range rig.ch.sends {
		sent.WriteString(m.Text)
		sent.WriteString("\n")
	}
	if !strings.Contains(sent.String(), "parse") {
		t.Fatalf("expected a parse warning in the reply, got:\n%s", sent.String())
	}
}

func TestLoad_ReadsLanguageScopes(t *testing.T) {
	home := sandboxHome(t)
	writeUserYml(t, home, "lang: 简体中文\nrepos:\n  github.com/cnlangzi/nightme:\n    lang: zh-CN\n")

	cfg, notes := Load()
	if notes.HasWarnings() {
		t.Fatalf("unexpected load warnings: %v", notes.Warnings)
	}
	if cfg.Lang != "简体中文" {
		t.Errorf("cfg.Lang = %q, want 简体中文", cfg.Lang)
	}
	repo, ok := cfg.Repos["github.com/cnlangzi/nightme"]
	if !ok {
		t.Fatalf("repo entry missing: %v", cfg.Repos)
	}
	if repo.Lang != "zh-CN" {
		t.Errorf("repo lang = %q, want zh-CN", repo.Lang)
	}
}

// TestLoad_ReadsFlowStyleRepos covers the flow-style spelling of
// the repos section — a user may write it either way.
func TestLoad_ReadsFlowStyleRepos(t *testing.T) {
	home := sandboxHome(t)
	writeUserYml(t, home, "lang: 简体中文\nrepos: {\"github.com/cnlangzi/nightme\": {lang: zh-CN}}\n")

	cfg, notes := Load()
	if notes.HasWarnings() {
		t.Fatalf("unexpected load warnings: %v", notes.Warnings)
	}
	if cfg.Lang != "简体中文" {
		t.Errorf("cfg.Lang = %q, want 简体中文", cfg.Lang)
	}
	repo, ok := cfg.Repos["github.com/cnlangzi/nightme"]
	if !ok {
		t.Fatalf("repo entry missing: %v", cfg.Repos)
	}
	if repo.Lang != "zh-CN" {
		t.Errorf("repo lang = %q, want zh-CN", repo.Lang)
	}
}

func TestLoad_NoLanguageScopesReadsEmpty(t *testing.T) {
	home := sandboxHome(t)
	writeUserYml(t, home, "fix:\n  hooks:\n    before:\n      - run: x\n")

	cfg, notes := Load()
	if notes.HasWarnings() {
		t.Fatalf("unexpected load warnings: %v", notes.Warnings)
	}
	if cfg.Lang != "" {
		t.Errorf("cfg.Lang = %q, want empty", cfg.Lang)
	}
	if len(cfg.Repos) != 0 {
		t.Errorf("cfg.Repos = %v, want empty", cfg.Repos)
	}
}

// -----------------------------------------------------------------------------
// /gtw config
// -----------------------------------------------------------------------------

// configChatSession builds a chat session whose cwd is dir, plus
// the recorder that captures its replies. A ChatSession accepts
// one emitter for its lifetime, so the recorder is created once
// and reused across the calls of a single test.
func configChatSession(t *testing.T, dir string) (*chatsession.ChatSession, *recordingCh) {
	t.Helper()
	cs := &chatsession.ChatSession{}
	_ = cs.SetSelectedCwd(dir)
	return cs, captureCh(t, cs)
}

// runConfigCommand drives RunConfig and returns the reply text.
func runConfigCommand(t *testing.T, cs *chatsession.ChatSession, ch *recordingCh, git GitRunner, argv ...string) string {
	t.Helper()
	if _, err := RunConfig(context.Background(), cs, HandlerDeps{Git: git}, "chat", "msg", argv); err != nil {
		t.Fatalf("RunConfig: %v", err)
	}
	return ch.lastText()
}

func TestRunConfig_QueryDefaultsToEnglish(t *testing.T) {
	sandboxHome(t)
	cs, ch := configChatSession(t, t.TempDir())
	got := runConfigCommand(t, cs, ch, &originGit{url: "git@github.com:cnlangzi/nightme.git"}, "lang")
	if !strings.Contains(got, "gtw.lang = English (default)") {
		t.Errorf("unexpected reply:\n%s", got)
	}
}

func TestRunConfig_GlobalWriteThenQuery(t *testing.T) {
	sandboxHome(t)
	cs, ch := configChatSession(t, t.TempDir())
	git := &originGit{url: "git@github.com:cnlangzi/nightme.git"}

	got := runConfigCommand(t, cs, ch, git, "-g", "lang", "简体中文")
	if !strings.Contains(got, "gtw.lang = 简体中文 (global scope)") {
		t.Errorf("unexpected write reply:\n%s", got)
	}
	if !strings.Contains(got, "~/.nightme/gtw.yml") {
		t.Errorf("write reply does not name the file:\n%s", got)
	}

	got = runConfigCommand(t, cs, ch, git, "lang")
	if !strings.Contains(got, "gtw.lang = 简体中文 (global scope)") {
		t.Errorf("unexpected query reply:\n%s", got)
	}
}

func TestRunConfig_RepoWriteBeatsGlobal(t *testing.T) {
	sandboxHome(t)
	cs, ch := configChatSession(t, t.TempDir())
	git := &originGit{url: "git@github.com:cnlangzi/nightme.git"}

	runConfigCommand(t, cs, ch, git, "-g", "lang", "简体中文")

	got := runConfigCommand(t, cs, ch, git, "lang", "zh-CN")
	if !strings.Contains(got, "gtw.lang = zh-CN (repo scope)") {
		t.Errorf("unexpected write reply:\n%s", got)
	}
	if !strings.Contains(got, "github.com/cnlangzi/nightme") {
		t.Errorf("write reply does not name the repo key:\n%s", got)
	}

	got = runConfigCommand(t, cs, ch, git, "lang")
	if !strings.Contains(got, "gtw.lang = zh-CN (repo scope)") {
		t.Errorf("repo scope should win over the global scope:\n%s", got)
	}
}

func TestRunConfig_UnsetFallsBackToDefault(t *testing.T) {
	sandboxHome(t)
	cs, ch := configChatSession(t, t.TempDir())
	git := &originGit{url: "git@github.com:cnlangzi/nightme.git"}

	runConfigCommand(t, cs, ch, git, "lang", "zh-CN")
	got := runConfigCommand(t, cs, ch, git, "--unset", "lang")
	if !strings.Contains(got, "gtw.lang unset (repo scope)") {
		t.Errorf("unexpected unset reply:\n%s", got)
	}

	got = runConfigCommand(t, cs, ch, git, "lang")
	if !strings.Contains(got, "gtw.lang = English (default)") {
		t.Errorf("expected the default after unset:\n%s", got)
	}
}

func TestRunConfig_UnsetGlobalScope(t *testing.T) {
	sandboxHome(t)
	cs, ch := configChatSession(t, t.TempDir())
	git := &originGit{url: "git@github.com:cnlangzi/nightme.git"}

	runConfigCommand(t, cs, ch, git, "-g", "lang", "简体中文")
	runConfigCommand(t, cs, ch, git, "--unset", "-g", "lang")

	got := runConfigCommand(t, cs, ch, git, "lang")
	if !strings.Contains(got, "gtw.lang = English (default)") {
		t.Errorf("expected the default after unset:\n%s", got)
	}
}

func TestRunConfig_UnknownKey(t *testing.T) {
	sandboxHome(t)
	cs, ch := configChatSession(t, t.TempDir())
	got := runConfigCommand(t, cs, ch, &originGit{}, "foo")
	if !strings.Contains(got, "unknown config key") {
		t.Errorf("unexpected reply:\n%s", got)
	}
	if !strings.Contains(got, "known keys: lang") {
		t.Errorf("reply does not name the known key:\n%s", got)
	}
}

func TestRunConfig_RepoScopeWithoutOrigin(t *testing.T) {
	sandboxHome(t)
	cs, ch := configChatSession(t, t.TempDir())
	got := runConfigCommand(t, cs, ch, &originGit{err: errors.New("no origin")}, "lang", "zh")
	if !strings.Contains(got, "-g") {
		t.Errorf("reply does not point at the global scope:\n%s", got)
	}
}

func TestRunConfig_QueryWithoutOriginFallsBack(t *testing.T) {
	sandboxHome(t)
	cs, ch := configChatSession(t, t.TempDir())
	got := runConfigCommand(t, cs, ch, &originGit{err: errors.New("no origin")}, "lang")
	if !strings.Contains(got, "gtw.lang = English (default)") {
		t.Errorf("unexpected reply:\n%s", got)
	}
}

func TestRunConfig_UnknownFlag(t *testing.T) {
	sandboxHome(t)
	cs, ch := configChatSession(t, t.TempDir())
	got := runConfigCommand(t, cs, ch, &originGit{}, "--typo", "lang")
	if !strings.Contains(got, "unknown flag") {
		t.Errorf("unexpected reply:\n%s", got)
	}
}

// TestRunConfig_QueryWithoutCwdHonoursGlobalScope covers a chat
// with no workspace bound: there is no repo scope to consult, but
// the user scope still applies.
func TestRunConfig_QueryWithoutCwdHonoursGlobalScope(t *testing.T) {
	sandboxHome(t)
	cs := &chatsession.ChatSession{}
	ch := captureCh(t, cs)

	runConfigCommand(t, cs, ch, &originGit{}, "-g", "lang", "简体中文")

	got := runConfigCommand(t, cs, ch, &originGit{}, "lang")
	if !strings.Contains(got, "gtw.lang = 简体中文 (global scope)") {
		t.Errorf("unexpected reply:\n%s", got)
	}
}

// TestRunConfig_MultiWordValue covers a language name with a
// space. The slash-command pipeline splits argv on whitespace
// without quote awareness, so the tail after the key is one
// value.
func TestRunConfig_MultiWordValue(t *testing.T) {
	home := sandboxHome(t)
	cs, ch := configChatSession(t, t.TempDir())
	git := &originGit{url: "git@github.com:cnlangzi/nightme.git"}

	got := runConfigCommand(t, cs, ch, git, "-g", "lang", "Simplified", "Chinese")
	if !strings.Contains(got, "gtw.lang = Simplified Chinese (global scope)") {
		t.Errorf("unexpected write reply:\n%s", got)
	}
	data, err := os.ReadFile(userYmlPath(home))
	if err != nil {
		t.Fatalf("read gtw.yml: %v", err)
	}
	if !strings.Contains(string(data), "Simplified Chinese") {
		t.Errorf("value not stored verbatim:\n%s", data)
	}
}

// TestRunConfig_QuotedValue covers a quoted value — the only way
// to express an empty one, since argv splitting keeps the quotes.
func TestRunConfig_QuotedValue(t *testing.T) {
	sandboxHome(t)
	cs, ch := configChatSession(t, t.TempDir())
	git := &originGit{url: "git@github.com:cnlangzi/nightme.git"}

	runConfigCommand(t, cs, ch, git, "-g", "lang", "简体中文")
	got := runConfigCommand(t, cs, ch, git, "-g", "lang", "\"\"")
	if !strings.Contains(got, "gtw.lang unset (global scope)") {
		t.Errorf("an empty value should unset the entry:\n%s", got)
	}
}

// -----------------------------------------------------------------------------
// Prompt wiring
// -----------------------------------------------------------------------------

func TestBuildAgentPrompt_CarriesOutputLanguage(t *testing.T) {
	p := buildAgentPrompt(Context{Worktree: "/w", Branch: "x", Issue: -1}, "简体中文")
	mustContain(t, p, "- Write the subject and the body in 简体中文. Keep the Conventional Commits type keyword (`feat`, `fix`, `chore`, ...) and the `Issue: #<id>` trailer in English.")
	if strings.Contains(p, placeholderLanguage) {
		t.Errorf("placeholder survived:\n%s", p)
	}
}

func TestBuildPRPrompt_CarriesOutputLanguage(t *testing.T) {
	p := buildPRPrompt(Context{Worktree: "/w", Branch: "x"}, "main", "简体中文")
	mustContain(t, p, "## Output language")
	mustContain(t, p, "Write the PR title subject and the body prose in 简体中文.")
	mustContain(t, p, "Keep these exactly in English, whatever the output language: the `## Summary by NightMe` heading, the category labels (`New Features:`, `Bug Fixes:`, `Enhancements:`, `Tests:`, `Documentation:`, `Chore / Build / CI:`), the `Risk: <low|medium|high> — <reason>` line, and the Conventional Commits type keyword.")
	if strings.Contains(p, placeholderLanguage) {
		t.Errorf("placeholder survived:\n%s", p)
	}
	// The structural markers the wrapper parses must stay in the
	// prompt unchanged.
	mustContain(t, p, "Risk: <low|medium|high> — <concise reason>")
}

func TestRenderNewIssuePrompt_CarriesOutputLanguage(t *testing.T) {
	p := renderNewIssuePrompt("/tmp/drafts", "简体中文")
	mustContain(t, p, "- Write the body prose in 简体中文. Keep the section headings below exactly as given, in English.")
	if strings.Contains(p, placeholderLanguage) {
		t.Errorf("placeholder survived:\n%s", p)
	}
	// The issue title stays English/ASCII — the branch name is
	// derived from it.
	mustContain(t, p, "type and scope in lowercase English, summary in English")
}

// TestPRPromptLanguage_KeepsRiskLineParseable pins the contract
// between the language rule and extractRiskLevel: the Risk marker
// the prompt asks for is the one the parser reads back.
func TestPRPromptLanguage_KeepsRiskLineParseable(t *testing.T) {
	p := buildPRPrompt(Context{Worktree: "/w", Branch: "x"}, "main", "简体中文")
	body := "## Summary by NightMe\n\n做了一些事情。\n\nRisk: low — 只是文档改动\n"
	level, reason := extractRiskLevel(body)
	if level != "low" {
		t.Fatalf("extractRiskLevel level = %q, want low", level)
	}
	if reason == "" {
		t.Fatalf("extractRiskLevel returned no reason")
	}
	if !strings.Contains(p, "Risk:") {
		t.Errorf("prompt lost the Risk marker:\n%s", p)
	}
}

// -----------------------------------------------------------------------------
// Dispatch wiring
// -----------------------------------------------------------------------------

// blocksText joins the text of the content blocks an agent run
// received.
func blocksText(blocks []agent.ContentBlock) string {
	var sb strings.Builder
	for _, b := range blocks {
		sb.WriteString(b.Text)
	}
	return sb.String()
}

// TestDispatchCommit_UsesConfiguredLanguage pins the wiring: the
// prompt the agent receives carries the resolved language, and the
// repo scope beats the global scope.
func TestDispatchCommit_UsesConfiguredLanguage(t *testing.T) {
	git := newPushGit()
	git.onArgsSeq(statusCmd,
		pushGitResp{"## wt-lang...origin/wt-lang\n M foo.go\n", "", nil},
		pushGitResp{"## wt-lang...origin/wt-lang\n", "", nil},
	)
	git.onArgsSeq([]string{"rev-parse", "HEAD"},
		pushGitResp{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n", "", nil},
		pushGitResp{"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\n", "", nil},
	)
	git.onArgs([]string{"status", "--porcelain"}, "", "", nil)
	git.onArgs([]string{"rev-parse", "--abbrev-ref", "HEAD"}, "wt-lang\n", "", nil)
	git.on("log", "bbbbbbbb feat: agent did the thing\n", "", nil)
	git.on("remote", "git@github.com:cnlangzi/nightme.git", "", nil)

	pi := &recordingAgent{name: "pi"}
	withAgent(t, pi)
	withCwd(t, t.TempDir())
	writeYml(t, mustPwd(t), Context{
		Worktree: mustPwd(t),
		Branch:   "wt-lang",
		RepoRoot: mustPwd(t),
	})
	cs := newPushChatSession(t)
	_ = cs.SetSelectedAgent("pi")
	captureCh(t, cs)

	cfg := Config{
		Lang:  "简体中文",
		Repos: map[string]RepoConfig{"github.com/cnlangzi/nightme": {Lang: "zh-CN"}},
	}
	if _, err := dispatchCommit(context.Background(), cs,
		HandlerDeps{Git: git}, "chat", "msg", commitArgs{}, cfg); err != nil {
		t.Fatalf("dispatchCommit: %v", err)
	}

	pi.mu.Lock()
	defer pi.mu.Unlock()
	if len(pi.calls) != 1 {
		t.Fatalf("agent.RunOnce called %d times, want 1", len(pi.calls))
	}
	text := blocksText(pi.calls[0].blocks)
	if !strings.Contains(text, "- Write the subject and the body in zh-CN.") {
		t.Errorf("agent prompt does not carry the resolved language:\n%s", text)
	}
	if strings.Contains(text, placeholderLanguage) {
		t.Errorf("placeholder survived into the agent prompt")
	}
}
