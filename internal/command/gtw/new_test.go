package gtw

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/chatsession"
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

	ask := renderNewIssuePrompt(path, false)
	defaults := renderNewIssuePrompt(path, true)
	for name, p := range map[string]string{"ask": ask, "defaults": defaults} {
		if strings.Contains(p, "{{") {
			t.Errorf("%s: unsubstituted placeholder in prompt", name)
		}
		if !strings.Contains(p, path) {
			t.Errorf("%s: prompt does not name the draft path", name)
		}
		if !strings.Contains(p, "Do NOT run gh / glab") {
			t.Errorf("%s: prompt must forbid the agent from creating the issue", name)
		}
	}
	if !strings.Contains(ask, "STOP") {
		t.Error("ask mode must tell the agent to stop after asking")
	}
	if strings.Contains(defaults, "STOP") || !strings.Contains(defaults, "Do not ask the user anything") {
		t.Error("-y mode must take defaults instead of stopping")
	}
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

func TestFinishNewIssue_NoDraft(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent.md")
	prov := newFakeGitProvider(ProviderGitHub, "github.com")

	if got := finishNewIssue(context.Background(), prov, "o", "r", missing, false); !strings.Contains(got, "waiting for your answers") || !strings.Contains(got, "/gtw new -y") {
		t.Errorf("ask mode reply = %q", got)
	}
	if got := finishNewIssue(context.Background(), prov, "o", "r", missing, true); !strings.HasPrefix(got, "❌") {
		t.Errorf("-y mode reply = %q, want ❌", got)
	}
	if n := len(prov.CallsByMethod("CreateIssue")); n != 0 {
		t.Errorf("CreateIssue called %d times without a draft", n)
	}
}

func TestFinishNewIssue_CreatesIssueAndRemovesDraft(t *testing.T) {
	draft := filepath.Join(t.TempDir(), "issue.md")
	if err := os.WriteFile(draft, []byte("# feat(gtw): archive chats\n\n## Requirements\n- R1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	prov := newFakeGitProvider(ProviderGitHub, "github.com")
	prov.SetCreateIssueResp(&Issue{ID: 42, Title: "feat(gtw): archive chats", URL: "https://github.com/o/r/issues/42"})

	got := finishNewIssue(context.Background(), prov, "o", "r", draft, false)

	calls := prov.CallsByMethod("CreateIssue")
	if len(calls) != 1 {
		t.Fatalf("CreateIssue calls = %d, want 1", len(calls))
	}
	if c := calls[0]; c.Owner != "o" || c.Repo != "r" || c.IssueTitle != "feat(gtw): archive chats" || c.IssueBody != "## Requirements\n- R1\n" {
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

func TestFinishNewIssue_CreateFailureKeepsDraft(t *testing.T) {
	draft := filepath.Join(t.TempDir(), "issue.md")
	if err := os.WriteFile(draft, []byte("# fix: x\nbody\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	prov := newFakeGitProvider(ProviderGitHub, "github.com")
	prov.SetCreateIssueErr(errors.New("HTTP 401"))

	got := finishNewIssue(context.Background(), prov, "o", "r", draft, false)

	if !strings.Contains(got, "HTTP 401") || !strings.Contains(got, draft) {
		t.Errorf("reply = %q, want error and draft path", got)
	}
	if _, err := os.Stat(draft); err != nil {
		t.Errorf("draft should be kept on failure: %v", err)
	}
}

func TestFinishNewIssue_InvalidDraft(t *testing.T) {
	draft := filepath.Join(t.TempDir(), "issue.md")
	if err := os.WriteFile(draft, []byte("no heading here\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	prov := newFakeGitProvider(ProviderGitHub, "github.com")

	got := finishNewIssue(context.Background(), prov, "o", "r", draft, false)

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

func TestAwaitPromptEnd(t *testing.T) {
	ch := make(chan chatsession.PromptEndedEvent, 1)
	ch <- chatsession.PromptEndedEvent{Reason: agent.PromptEndClean}
	if err := awaitPromptEnd(context.Background(), ch, time.Second); err != nil {
		t.Errorf("clean end: %v", err)
	}

	ch <- chatsession.PromptEndedEvent{Reason: agent.PromptEndUserStopped}
	if err := awaitPromptEnd(context.Background(), ch, time.Second); err == nil {
		t.Error("stopped turn: want error")
	}

	if err := awaitPromptEnd(context.Background(), make(chan chatsession.PromptEndedEvent), 10*time.Millisecond); err == nil {
		t.Error("timeout: want error")
	}
}
