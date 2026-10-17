package gtw

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
)

// funcCLIRunner delegates Run to fn so a test can inspect the
// temp body file while it still exists.
type funcCLIRunner struct {
	fn func(name string, args []string) (string, string, error)
}

func (r funcCLIRunner) Run(_ context.Context, name string, args ...string) (string, string, error) {
	return r.fn(name, args)
}

func argAfter(args []string, flag string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			return args[i+1]
		}
	}
	return ""
}

func TestParseNewIssueArgs(t *testing.T) {
	cases := []struct {
		argv    []string
		yes     bool
		wantErr bool
	}{
		{argv: nil},
		{argv: []string{"-y"}, yes: true},
		{argv: []string{"--yes"}, yes: true},
		{argv: []string{"--force"}, wantErr: true},
		{argv: []string{"title"}, wantErr: true},
	}
	for _, tc := range cases {
		got, err := parseNewIssueArgs(tc.argv)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%v: want error, got nil", tc.argv)
			}
			continue
		}
		if err != nil {
			t.Errorf("%v: unexpected error: %v", tc.argv, err)
			continue
		}
		if got.Yes != tc.yes {
			t.Errorf("%v: Yes = %v, want %v", tc.argv, got.Yes, tc.yes)
		}
	}
}

func TestRenderNewIssuePrompt(t *testing.T) {
	const path = "/home/u/.nightme/issue-drafts/issue-1.md"

	p := renderNewIssuePrompt(path)
	if strings.Contains(p, "{{") {
		t.Error("unsubstituted placeholder in prompt")
	}
	if strings.HasPrefix(strings.TrimSpace(p), "/") {
		t.Error("prompt starts with '/'; agents would parse it as a slash command")
	}
	for _, want := range []string{
		path,
		"Do NOT run gh / glab",
		"STOP",
		"print the complete content of the draft file in chat",
		"run /gtw new -y to create the issue",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
}

func TestIssueDraftPath(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", os.Getenv("HOME"))

	a1, err := issueDraftPath("oc_chat/a:1")
	if err != nil {
		t.Fatal(err)
	}
	a2, _ := issueDraftPath("oc_chat/a:1")
	b, _ := issueDraftPath("oc_chat/b:2")
	if a1 != a2 {
		t.Errorf("draft path not stable per chat: %q vs %q", a1, a2)
	}
	if a1 == b {
		t.Error("different chats share a draft path")
	}
	if base := filepath.Base(a1); strings.ContainsAny(base, `/\:`) || !strings.HasPrefix(base, "issue-") {
		t.Errorf("unsafe draft file name %q", base)
	}
	if !strings.HasPrefix(a1, filepath.Join(os.Getenv("HOME"), ".nightme", issueDraftDirName)) {
		t.Errorf("draft %q not under ~/.nightme/%s", a1, issueDraftDirName)
	}
}

func TestResetIssueDraft(t *testing.T) {
	draft := filepath.Join(t.TempDir(), "drafts", "issue-x.md")

	if err := resetIssueDraft(draft); err != nil {
		t.Fatalf("missing dir: %v", err)
	}
	if err := os.WriteFile(draft, []byte("# old\nbody\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := resetIssueDraft(draft); err != nil {
		t.Fatalf("existing draft: %v", err)
	}
	if _, err := os.Stat(draft); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stale draft should be removed (stat err = %v)", err)
	}
}

func resolveTo(prov GitProvider) func() (GitProvider, string, string, error) {
	return func() (GitProvider, string, string, error) { return prov, "o", "r", nil }
}

func TestParseIssueDraft(t *testing.T) {
	cases := []struct {
		name      string
		raw       string
		title     string
		body      string
		wantError bool
	}{
		{
			name:  "plain",
			raw:   "# feat(gtw): add new\n\n## Background & Goal\nx\n",
			title: "feat(gtw): add new",
			body:  "## Background & Goal\nx\n",
		},
		{
			name:  "bom crlf and leading blank lines",
			raw:   "\ufeff\r\n\r\n# fix: y\r\n## Requirements\r\n- R1\r\n",
			title: "fix: y",
			body:  "## Requirements\n- R1\n",
		},
		{name: "no heading", raw: "feat: x\nbody", wantError: true},
		{name: "level-2 heading", raw: "## feat: x\nbody", wantError: true},
		{name: "empty title", raw: "# \nbody", wantError: true},
		{name: "empty body", raw: "# feat: x\n\n  \n", wantError: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			title, body, err := parseIssueDraft(tc.raw)
			if tc.wantError {
				if err == nil {
					t.Fatalf("want error, got title=%q body=%q", title, body)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if title != tc.title || body != tc.body {
				t.Errorf("got (%q, %q), want (%q, %q)", title, body, tc.title, tc.body)
			}
		})
	}
}

func TestCreateIssueFromDraft_NoDraft(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent.md")
	prov := newFakeGitProvider(ProviderGitHub, "github.com")

	got := createIssueFromDraft(context.Background(), missing, resolveTo(prov))
	if !strings.HasPrefix(got, "❌ no issue draft") || !strings.Contains(got, "`/gtw new`") {
		t.Errorf("reply = %q", got)
	}
	if n := len(prov.CallsByMethod("CreateIssue")); n != 0 {
		t.Errorf("CreateIssue called %d times without a draft", n)
	}
}

func TestCreateIssueFromDraft_CreatesIssueAndRemovesDraft(t *testing.T) {
	draft := filepath.Join(t.TempDir(), "issue.md")
	if err := os.WriteFile(draft, []byte("# feat(gtw): archive chats\n\n## Requirements\n- R1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	prov := newFakeGitProvider(ProviderGitHub, "github.com")
	prov.SetCreateIssueResp(&Issue{ID: 42, Title: "feat(gtw): archive chats", URL: "https://github.com/o/r/issues/42"})

	got := createIssueFromDraft(context.Background(), draft, resolveTo(prov))

	calls := prov.CallsByMethod("CreateIssue")
	if len(calls) != 1 {
		t.Fatalf("CreateIssue calls = %d, want 1", len(calls))
	}
	if c := calls[0]; c.Owner != "o" || c.Repo != "r" || c.IssueTitle != "feat(gtw): archive chats" || c.IssueBody != "## Requirements\n- R1\n\n---\n_Generated by [NightMe](https://github.com/cnlangzi/nightme)_\n" {
		t.Errorf("CreateIssue call = %+v", c)
	}
	for _, want := range []string{"✅ Issue #42 opened", "https://github.com/o/r/issues/42", "/gtw fix 42"} {
		if !strings.Contains(got, want) {
			t.Errorf("reply missing %q:\n%s", want, got)
		}
	}
	if _, err := os.Stat(draft); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("draft should be removed after the issue is created (stat err = %v)", err)
	}
}

func TestCreateIssueFromDraft_CreateFailureKeepsDraft(t *testing.T) {
	draft := filepath.Join(t.TempDir(), "issue.md")
	if err := os.WriteFile(draft, []byte("# fix: x\nbody\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	prov := newFakeGitProvider(ProviderGitHub, "github.com")
	prov.SetCreateIssueErr(errors.New("HTTP 401"))

	got := createIssueFromDraft(context.Background(), draft, resolveTo(prov))

	if !strings.Contains(got, "HTTP 401") || !strings.Contains(got, draft) {
		t.Errorf("reply = %q, want error and draft path", got)
	}
	if _, err := os.Stat(draft); err != nil {
		t.Errorf("draft should be kept on failure: %v", err)
	}
}

func TestCreateIssueFromDraft_InvalidDraft(t *testing.T) {
	draft := filepath.Join(t.TempDir(), "issue.md")
	if err := os.WriteFile(draft, []byte("no heading here\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	prov := newFakeGitProvider(ProviderGitHub, "github.com")

	got := createIssueFromDraft(context.Background(), draft, resolveTo(prov))

	if !strings.HasPrefix(got, "❌ invalid issue draft") {
		t.Errorf("reply = %q", got)
	}
	if n := len(prov.CallsByMethod("CreateIssue")); n != 0 {
		t.Errorf("CreateIssue called %d times for an invalid draft", n)
	}
}

func TestIssueIDFromURL(t *testing.T) {
	cases := map[string]int{
		"https://github.com/o/r/issues/42":            42,
		"https://github.com/o/r/issues/42/\n":         42,
		"https://gitlab.example.com/g/s/r/-/issues/7": 7,
		"https://github.com/o/r/issues/new":           0,
		"":                                            0,
	}
	for in, want := range cases {
		if got := issueIDFromURL(in); got != want {
			t.Errorf("issueIDFromURL(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestGitHubProvider_CreateIssue(t *testing.T) {
	const body = "## Requirements\n- R1\n"
	var bodyFile, gotBody string
	var gotArgs []string
	prov := &GitHubProvider{Runner: funcCLIRunner{fn: func(name string, args []string) (string, string, error) {
		if name != "gh" {
			t.Errorf("binary = %q, want gh", name)
		}
		gotArgs = args
		bodyFile = argAfter(args, "--body-file")
		b, err := os.ReadFile(bodyFile)
		if err != nil {
			t.Errorf("read body file: %v", err)
		}
		gotBody = string(b)
		return "https://github.com/o/r/issues/42\n", "", nil
	}}}

	issue, err := prov.CreateIssue(context.Background(), "o", "r", "feat: x", body)
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if issue.ID != 42 || issue.URL != "https://github.com/o/r/issues/42" || issue.Title != "feat: x" {
		t.Errorf("issue = %+v", issue)
	}
	if argAfter(gotArgs, "--repo") != "o/r" || argAfter(gotArgs, "--title") != "feat: x" {
		t.Errorf("args = %v", gotArgs)
	}
	if gotBody != body {
		t.Errorf("body file content = %q, want %q", gotBody, body)
	}
	if _, err := os.Stat(bodyFile); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("temp body file should be removed (stat err = %v)", err)
	}
}

func TestGitHubProvider_CreateIssue_Errors(t *testing.T) {
	failing := &GitHubProvider{Runner: funcCLIRunner{fn: func(string, []string) (string, string, error) {
		return "", "HTTP 403: Resource not accessible", errors.New("exit status 1")
	}}}
	if _, err := failing.CreateIssue(context.Background(), "o", "r", "t", "b"); err == nil || !strings.Contains(err.Error(), "HTTP 403") {
		t.Errorf("err = %v, want stderr preserved", err)
	}

	garbled := &GitHubProvider{Runner: funcCLIRunner{fn: func(string, []string) (string, string, error) {
		return "done\n", "", nil
	}}}
	if _, err := garbled.CreateIssue(context.Background(), "o", "r", "t", "b"); err == nil {
		t.Error("want error when gh prints no issue URL")
	}
}

func TestGitLabProvider_CreateIssue(t *testing.T) {
	const body = "## Requirements\n- R1\n"
	var gotArgs []string
	var gotBody string
	prov := &GitLabProvider{Runner: funcCLIRunner{fn: func(name string, args []string) (string, string, error) {
		if name != "glab" {
			t.Errorf("binary = %q, want glab", name)
		}
		gotArgs = args
		desc := argAfter(args, "-F")
		if !strings.HasPrefix(desc, "description=@") {
			t.Errorf("-F value = %q, want description=@<file>", desc)
		}
		b, err := os.ReadFile(strings.TrimPrefix(desc, "description=@"))
		if err != nil {
			t.Errorf("read body file: %v", err)
		}
		gotBody = string(b)
		return `{"iid": 7, "web_url": "https://gitlab.com/g/r/-/issues/7", "state": "opened"}`, "", nil
	}}}

	issue, err := prov.CreateIssue(context.Background(), "g/sub", "r", "fix: y", body)
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if issue.ID != 7 || issue.URL != "https://gitlab.com/g/r/-/issues/7" || issue.State != "open" {
		t.Errorf("issue = %+v", issue)
	}
	if !strings.Contains(strings.Join(gotArgs, " "), "projects/g%2Fsub%2Fr/issues") {
		t.Errorf("args = %v, want escaped project path", gotArgs)
	}
	if argAfter(gotArgs, "-f") != "title=fix: y" {
		t.Errorf("title field = %q", argAfter(gotArgs, "-f"))
	}
	if gotBody != body {
		t.Errorf("body file content = %q, want %q", gotBody, body)
	}
}

// newIssueRig is a chat session sitting in a real temp repo whose
// origin points at github.com/o/r, with a fake provider behind it.
type newIssueRig struct {
	f    *Factory
	cs   *chatsession.ChatSession
	ch   *recordingCh
	prov *fakeGitProvider
}

func newNewIssueRig(t *testing.T, chatID string) *newIssueRig {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	repo := initTempRepo(t)
	mustGit(t, repo, "remote", "add", "origin", "https://github.com/o/r.git")

	ch := &recordingCh{}
	cs, err := chatsession.New(chatID, "test-agent")
	if err != nil {
		t.Fatalf("chatsession.New: %v", err)
	}
	cs.WithEmitter(ch)
	if err := cs.SetSelectedCwd(repo); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
	}
	prov := newFakeGitProvider(ProviderGitHub, "github.com")
	f := NewFactoryWithDeps(NewManager(), HandlerDeps{
		Git:    ExecGitRunner{},
		Detect: fakeDetect(prov),
		Now:    time.Now,
	})
	return &newIssueRig{f: f, cs: cs, ch: ch, prov: prov}
}

func (r *newIssueRig) handle(t *testing.T, args ...string) {
	t.Helper()
	_, err := r.f.Handle(context.Background(), command.RuntimeServices{}, nil, r.cs, command.SlashInput{
		Args:      append([]string{"gtw", "new"}, args...),
		ChatID:    r.cs.ChatID,
		MessageID: "msg-1",
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
}

func (r *newIssueRig) draftPath(t *testing.T) string {
	t.Helper()
	p, err := issueDraftPath(r.cs.ChatID)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFactory_NewYes_CreatesIssueFromDraft(t *testing.T) {
	rig := newNewIssueRig(t, "chat-new-1")
	draft := rig.draftPath(t)
	if err := resetIssueDraft(draft); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(draft, []byte("# feat(gtw): archive chats\n\n## Requirements\n- R1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rig.prov.SetCreateIssueResp(&Issue{ID: 42, Title: "feat(gtw): archive chats", URL: "https://github.com/o/r/issues/42"})

	rig.handle(t, "-y")

	calls := rig.prov.CallsByMethod("CreateIssue")
	if len(calls) != 1 || calls[0].Owner != "o" || calls[0].Repo != "r" || calls[0].IssueTitle != "feat(gtw): archive chats" {
		t.Fatalf("CreateIssue calls = %+v", calls)
	}
	if got := rig.ch.lastText(); !strings.Contains(got, "✅ Issue #42 opened") {
		t.Errorf("reply = %q", got)
	}
	if _, err := os.Stat(draft); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("draft should be removed (stat err = %v)", err)
	}
}

func TestFactory_NewYes_NoDraft(t *testing.T) {
	rig := newNewIssueRig(t, "chat-new-2")

	rig.handle(t, "-y")

	if got := rig.ch.lastText(); !strings.HasPrefix(got, "❌ no issue draft") {
		t.Errorf("reply = %q", got)
	}
	if n := len(rig.prov.CallsByMethod("CreateIssue")); n != 0 {
		t.Errorf("CreateIssue called %d times", n)
	}
}

func TestFactory_NewDraft_RemovesStaleDraft(t *testing.T) {
	rig := newNewIssueRig(t, "chat-new-3")
	draft := rig.draftPath(t)
	if err := resetIssueDraft(draft); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(draft, []byte("# fix: old\nold body\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	rig.handle(t)

	if _, err := os.Stat(draft); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("/gtw new must remove the previous draft so -y cannot submit it (stat err = %v)", err)
	}
	if n := len(rig.prov.CallsByMethod("CreateIssue")); n != 0 {
		t.Errorf("drafting must not create an issue; CreateIssue called %d times", n)
	}
}

func TestFactory_NewDraft_NotInRepo(t *testing.T) {
	rig := newNewIssueRig(t, "chat-new-4")
	if err := rig.cs.SetSelectedCwd(t.TempDir()); err != nil {
		t.Fatal(err)
	}

	rig.handle(t)

	if got := rig.ch.lastText(); !strings.Contains(got, "Not in a git repository") {
		t.Errorf("reply = %q", got)
	}
}

func TestFactory_New_TakesNoRunLock(t *testing.T) {
	rig := newNewIssueRig(t, "chat-new-5")
	mu := rig.f.mgr.runLockFor(rig.cs.ChatID)
	mu.Lock()
	defer mu.Unlock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = rig.f.Handle(context.Background(), command.RuntimeServices{}, nil, rig.cs, command.SlashInput{
			Args: []string{"gtw", "new", "-y"}, ChatID: rig.cs.ChatID, MessageID: "msg-1",
		})
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("/gtw new blocked on the per-chat run lock")
	}
}
