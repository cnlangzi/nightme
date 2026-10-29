package gtw

import (
	"context"
	"errors"
	"fmt"
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
	const dir = "/home/u/.nightme/issue-drafts/issue-1"

	p := renderNewIssuePrompt(dir, defaultLanguage)
	if strings.Contains(p, placeholderDraftDirAbs) {
		t.Error("unsubstituted draft directory placeholder in prompt")
	}
	if strings.HasPrefix(strings.TrimSpace(p), "/") {
		t.Error("prompt starts with '/'; agents would parse it as a slash command")
	}
	for _, want := range []string{
		dir,
		"01.md, 02.md",
		"{{ISSUE:NN}}",
		"NN.created",
		"list the directory before every rewrite",
		"Do NOT run gh / glab",
		"STOP",
		"splitting the work into several issues",
		"One issue is the default",
		"Never split by kind of work",
		"{{ISSUE:02}} inside 01.md fails",
		"must point to a lower-numbered draft of this set",
		"every {{ISSUE:NN}} points at a lower file number",
		"print the complete content of every draft file in chat",
		"run /gtw new -y to create the issues",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
}

func TestIssueDraftDir(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", os.Getenv("HOME"))

	a1, err := issueDraftDir("oc_chat/a:1")
	if err != nil {
		t.Fatal(err)
	}
	a2, _ := issueDraftDir("oc_chat/a:1")
	b, _ := issueDraftDir("oc_chat/b:2")
	if a1 != a2 {
		t.Errorf("draft dir not stable per chat: %q vs %q", a1, a2)
	}
	if a1 == b {
		t.Error("different chats share a draft dir")
	}
	if base := filepath.Base(a1); strings.ContainsAny(base, `/\:`) || !strings.HasPrefix(base, "issue-") {
		t.Errorf("unsafe draft dir name %q", base)
	}
	if !strings.HasPrefix(a1, filepath.Join(os.Getenv("HOME"), ".nightme", issueDraftDirName)) {
		t.Errorf("draft dir %q not under ~/.nightme/%s", a1, issueDraftDirName)
	}
}

func TestResetIssueDrafts(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "drafts", "issue-x")

	if err := resetIssueDrafts(dir); err != nil {
		t.Fatalf("missing dir: %v", err)
	}
	writeDrafts(t, dir, "01.created", "#42 old\n", "02.md", "# old 2\nbody\n")
	if err := resetIssueDrafts(dir); err != nil {
		t.Fatalf("existing drafts: %v", err)
	}
	if names := draftNames(t, dir); len(names) != 0 {
		t.Errorf("stale drafts should be removed, left %v", names)
	}
}

// writeDrafts writes name/content pairs into dir.
func writeDrafts(t *testing.T, dir string, nameContent ...string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i+1 < len(nameContent); i += 2 {
		if err := os.WriteFile(filepath.Join(dir, nameContent[i]), []byte(nameContent[i+1]), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func draftNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func readDraft(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func resolveTo(prov GitProvider) func() (GitProvider, string, string, error) {
	return func() (GitProvider, string, string, error) { return prov, "o", "r", nil }
}

// numberIssuesFrom makes CreateIssue hand out consecutive issue
// numbers starting at first, failing the calls listed in fail
// (1-based).
func numberIssuesFrom(prov *fakeGitProvider, first int, fail ...int) {
	call := 0
	next := first
	prov.SetCreateIssueFunc(func(title, _ string) (*Issue, error) {
		call++
		for _, f := range fail {
			if f == call {
				return nil, errors.New("HTTP 401")
			}
		}
		id := next
		next++
		return &Issue{ID: id, Title: title, URL: fmt.Sprintf("https://github.com/o/r/issues/%d", id)}, nil
	})
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

func TestCreateIssuesFromDrafts_NoDraft(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent")
	empty := t.TempDir()
	for name, dir := range map[string]string{"missing dir": missing, "empty dir": empty} {
		t.Run(name, func(t *testing.T) {
			prov := newFakeGitProvider(ProviderGitHub, "github.com")

			got := createIssuesFromDrafts(context.Background(), dir, resolveTo(prov))

			if !strings.HasPrefix(got, "❌ no issue draft") || !strings.Contains(got, dir) || !strings.Contains(got, "`/gtw new`") {
				t.Errorf("reply = %q", got)
			}
			if n := len(prov.CallsByMethod("CreateIssue")); n != 0 {
				t.Errorf("CreateIssue called %d times without a draft", n)
			}
		})
	}
}

func TestCreateIssuesFromDrafts_SingleDraft(t *testing.T) {
	dir := t.TempDir()
	writeDrafts(t, dir, "01.md", "# feat(gtw): archive chats\n\n## Requirements\n- R1\n")
	prov := newFakeGitProvider(ProviderGitHub, "github.com")
	prov.SetCreateIssueResp(&Issue{ID: 42, Title: "feat(gtw): archive chats", URL: "https://github.com/o/r/issues/42"})

	got := createIssuesFromDrafts(context.Background(), dir, resolveTo(prov))

	calls := prov.CallsByMethod("CreateIssue")
	if len(calls) != 1 {
		t.Fatalf("CreateIssue calls = %d, want 1", len(calls))
	}
	if c := calls[0]; c.Owner != "o" || c.Repo != "r" || c.IssueTitle != "feat(gtw): archive chats" || c.IssueBody != "## Requirements\n- R1\n\n---\n_Generated by [NightMe](https://nightme.dev)_\n" {
		t.Errorf("CreateIssue call = %+v", c)
	}
	for _, want := range []string{"✅ Issue #42 opened", "https://github.com/o/r/issues/42", "/gtw fix 42"} {
		if !strings.Contains(got, want) {
			t.Errorf("reply missing %q:\n%s", want, got)
		}
	}
	if names := draftNames(t, dir); strings.Join(names, ",") != "01.created" {
		t.Errorf("draft should be replaced by its record, dir holds %v", names)
	}
	if rec := readDraft(t, dir, "01.created"); rec != "#42 feat(gtw): archive chats\nhttps://github.com/o/r/issues/42\n" {
		t.Errorf("record = %q", rec)
	}
}

func TestCreateIssuesFromDrafts_MultipleDraftsInOrderWithRefs(t *testing.T) {
	dir := t.TempDir()
	writeDrafts(t, dir,
		"03.md", "# feat(c): third\n\n- depends on {{ISSUE:01}} and {{ISSUE:02}}\n",
		"01.md", "# feat(a): first\n\nbody a\n",
		"02.md", "# feat(b): second\n\n- depends on {{ISSUE:01}}\n",
	)
	prov := newFakeGitProvider(ProviderGitHub, "github.com")
	numberIssuesFrom(prov, 42)

	got := createIssuesFromDrafts(context.Background(), dir, resolveTo(prov))

	calls := prov.CallsByMethod("CreateIssue")
	if len(calls) != 3 {
		t.Fatalf("CreateIssue calls = %d, want 3", len(calls))
	}
	for i, title := range []string{"feat(a): first", "feat(b): second", "feat(c): third"} {
		if calls[i].IssueTitle != title {
			t.Errorf("call %d title = %q, want %q", i, calls[i].IssueTitle, title)
		}
		if strings.Contains(calls[i].IssueBody, "{{ISSUE:") {
			t.Errorf("call %d body has an unresolved reference:\n%s", i, calls[i].IssueBody)
		}
	}
	if !strings.Contains(calls[1].IssueBody, "depends on #42\n") {
		t.Errorf("second body = %q, want reference to #42", calls[1].IssueBody)
	}
	if !strings.Contains(calls[2].IssueBody, "depends on #42 and #43\n") {
		t.Errorf("third body = %q, want references to #42 and #43", calls[2].IssueBody)
	}
	want := "✅ 3 issues opened\n" +
		"→ repo: o/r\n" +
		"→ #42:  feat(a): first · https://github.com/o/r/issues/42\n" +
		"→ #43:  feat(b): second · https://github.com/o/r/issues/43\n" +
		"→ #44:  feat(c): third · https://github.com/o/r/issues/44\n" +
		"↳ `/gtw fix <id>` to assess one · or `/gtw fix <id> -y` to implement it directly"
	if got != want {
		t.Errorf("reply =\n%s\nwant\n%s", got, want)
	}
	if names := draftNames(t, dir); strings.Join(names, ",") != "01.created,02.created,03.created" {
		t.Errorf("drafts should be replaced by their records, dir holds %v", names)
	}
}

func TestCreateIssuesFromDrafts_AllCreated(t *testing.T) {
	dir := t.TempDir()
	writeDrafts(t, dir, "01.created", "#42 feat: a\n", "02.created", "#43 feat: b\n")
	prov := newFakeGitProvider(ProviderGitHub, "github.com")

	got := createIssuesFromDrafts(context.Background(), dir, resolveTo(prov))

	if want := "✨ every issue draft of this chat is already an issue\n→ issues: #42, #43\n↳ `/gtw new` to draft from a new discussion"; got != want {
		t.Errorf("reply =\n%s\nwant\n%s", got, want)
	}
	if n := len(prov.CallsByMethod("CreateIssue")); n != 0 {
		t.Errorf("CreateIssue called %d times with nothing pending", n)
	}
}

func TestCreateIssuesFromDrafts_PartialFailureThenRetry(t *testing.T) {
	dir := t.TempDir()
	writeDrafts(t, dir,
		"01.md", "# feat(a): first\n\nbody a\n",
		"02.md", "# feat(b): second\n\n- depends on {{ISSUE:01}}\n",
		"03.md", "# feat(c): third\n\n- depends on {{ISSUE:02}}\n",
	)
	prov := newFakeGitProvider(ProviderGitHub, "github.com")
	numberIssuesFrom(prov, 42, 2)

	got := createIssuesFromDrafts(context.Background(), dir, resolveTo(prov))

	for _, want := range []string{"⚠️ 1 of 3 issues opened", "→ #42:", "02.md", "HTTP 401", dir + " (2 kept)", "`/gtw new -y` to create the rest"} {
		if !strings.Contains(got, want) {
			t.Errorf("reply missing %q:\n%s", want, got)
		}
	}
	if names := draftNames(t, dir); strings.Join(names, ",") != "01.created,02.md,03.md" {
		t.Fatalf("dir holds %v, want the record of 01 and the two drafts not yet created", names)
	}

	retry := newFakeGitProvider(ProviderGitHub, "github.com")
	numberIssuesFrom(retry, 50)
	got = createIssuesFromDrafts(context.Background(), dir, resolveTo(retry))

	calls := retry.CallsByMethod("CreateIssue")
	if len(calls) != 2 || calls[0].IssueTitle != "feat(b): second" || calls[1].IssueTitle != "feat(c): third" {
		t.Fatalf("retry CreateIssue calls = %+v, want only the remaining two", calls)
	}
	if !strings.Contains(calls[0].IssueBody, "depends on #42\n") || !strings.Contains(calls[1].IssueBody, "depends on #50\n") {
		t.Errorf("retry bodies = %q / %q", calls[0].IssueBody, calls[1].IssueBody)
	}
	if !strings.HasPrefix(got, "✅ 2 issues opened") {
		t.Errorf("retry reply = %q", got)
	}
}

func TestCreateIssuesFromDrafts_FirstFailureKeepsDrafts(t *testing.T) {
	dir := t.TempDir()
	writeDrafts(t, dir, "01.md", "# fix: x\nbody\n")
	prov := newFakeGitProvider(ProviderGitHub, "github.com")
	prov.SetCreateIssueErr(errors.New("HTTP 401"))

	got := createIssuesFromDrafts(context.Background(), dir, resolveTo(prov))

	if want := "❌ create issue from 01.md: HTTP 401\n→ drafts: " + dir + " (1 kept)\n↳ `/gtw new -y` to retry"; got != want {
		t.Errorf("reply =\n%s\nwant\n%s", got, want)
	}
	if names := draftNames(t, dir); len(names) != 1 {
		t.Errorf("draft should be kept on failure, left %v", names)
	}
}

func TestCreateIssuesFromDrafts_InvalidSetCreatesNothing(t *testing.T) {
	cases := []struct {
		name  string
		files []string
		want  string
	}{
		{
			name:  "bad heading in a later draft",
			files: []string{"01.md", "# feat: a\nbody\n", "02.md", "no heading here\n"},
			want:  "❌ invalid issue draft 02.md: first line must be",
		},
		{
			name:  "reference to a later draft",
			files: []string{"01.md", "# feat: a\nsee {{ISSUE:02}}\n", "02.md", "# feat: b\nbody\n"},
			want:  "❌ invalid issue draft 01.md: {{ISSUE:02}} must point to a lower-numbered draft",
		},
		{
			name:  "reference to a missing draft",
			files: []string{"01.md", "# feat: a\nbody\n", "02.md", "# feat: b\nsee {{ISSUE:07}}\n"},
			want:  "❌ invalid issue draft 02.md: {{ISSUE:07}}",
		},
		{
			name:  "self reference",
			files: []string{"01.md", "# feat: a\nsee {{ISSUE:01}}\n"},
			want:  "❌ invalid issue draft 01.md: {{ISSUE:01}}",
		},
		{
			name:  "draft reusing the number of a created issue",
			files: []string{"01.created", "#42 feat: a\n", "01.md", "# feat: a again\nbody\n"},
			want:  "❌ invalid issue draft 01.md: draft 01 is already issue #42",
		},
		{
			name:  "stray file",
			files: []string{"01.md", "# feat: a\nbody\n", "issue-1.md", "# feat: b\nbody\n"},
			want:  `❌ read issue drafts`,
		},
		{
			name:  "unreadable record",
			files: []string{"01.created", "created\n", "02.md", "# feat: b\nbody\n"},
			want:  `❌ read issue drafts`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeDrafts(t, dir, tc.files...)
			prov := newFakeGitProvider(ProviderGitHub, "github.com")
			prov.SetCreateIssueResp(&Issue{ID: 1})

			got := createIssuesFromDrafts(context.Background(), dir, resolveTo(prov))

			if !strings.HasPrefix(got, tc.want) {
				t.Errorf("reply = %q, want prefix %q", got, tc.want)
			}
			if n := len(prov.CallsByMethod("CreateIssue")); n != 0 {
				t.Errorf("CreateIssue called %d times for an invalid draft set", n)
			}
			if names := draftNames(t, dir); len(names) != len(tc.files)/2 {
				t.Errorf("drafts must be kept, left %v", names)
			}
		})
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

func (r *newIssueRig) draftDir(t *testing.T) string {
	t.Helper()
	dir, err := issueDraftDir(r.cs.ChatID)
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestFactory_NewYes_CreatesIssuesFromDrafts(t *testing.T) {
	rig := newNewIssueRig(t, "chat-new-1")
	dir := rig.draftDir(t)
	writeDrafts(t, dir,
		"01.md", "# feat(gtw): archive chats\n\n## Requirements\n- R1\n",
		"02.md", "# feat(gtw): split drafts\n\n- depends on {{ISSUE:01}}\n",
	)
	numberIssuesFrom(rig.prov, 42)

	rig.handle(t, "-y")

	calls := rig.prov.CallsByMethod("CreateIssue")
	if len(calls) != 2 || calls[0].Owner != "o" || calls[0].Repo != "r" ||
		calls[0].IssueTitle != "feat(gtw): archive chats" || !strings.Contains(calls[1].IssueBody, "depends on #42") {
		t.Fatalf("CreateIssue calls = %+v", calls)
	}
	if got := rig.ch.lastText(); !strings.Contains(got, "✅ 2 issues opened") {
		t.Errorf("reply = %q", got)
	}
	if names := draftNames(t, dir); strings.Join(names, ",") != "01.created,02.created" {
		t.Errorf("drafts should be replaced by their records, dir holds %v", names)
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

func TestFactory_NewDraft_RemovesStaleDrafts(t *testing.T) {
	rig := newNewIssueRig(t, "chat-new-3")
	dir := rig.draftDir(t)
	writeDrafts(t, dir, "01.md", "# fix: old\nold body\n", "02.md", "# fix: old 2\nold body\n")

	rig.handle(t)

	if names := draftNames(t, dir); len(names) != 0 {
		t.Errorf("/gtw new must remove the previous drafts so -y cannot submit them, left %v", names)
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
