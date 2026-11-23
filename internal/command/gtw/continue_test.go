package gtw

// Tests for the three scope flags /gtw commit and /gtw pr gained
// from /review: --continue (pick the last failed run of this chat
// back up), -g/--global (persist the agent as the subcommand's
// default) and --unset (remove it again).

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
)

// -----------------------------------------------------------------------------
// Flag parsing
// -----------------------------------------------------------------------------

// TestParseCommitArgs_ScopeFlags pins the three flags the shared
// lexer must accept on /gtw commit.
func TestParseCommitArgs_ScopeFlags(t *testing.T) {
	cases := []struct {
		argv []string
		want commitArgs
	}{
		{[]string{"-c"}, commitArgs{Continue: true}},
		{[]string{"--continue"}, commitArgs{Continue: true}},
		{[]string{"-g", "-a", "codex"}, commitArgs{Agent: "codex", Global: true}},
		{[]string{"--global", "--agent", "codex"}, commitArgs{Agent: "codex", Global: true}},
		{[]string{"--unset"}, commitArgs{Unset: true}},
	}
	for _, tc := range cases {
		got, err := parseCommitArgs(tc.argv)
		if err != nil {
			t.Fatalf("parseCommitArgs(%v): %v", tc.argv, err)
		}
		if got != tc.want {
			t.Errorf("parseCommitArgs(%v) = %+v, want %+v", tc.argv, got, tc.want)
		}
	}
}

// TestParsePRArgs_ScopeFlags pins the same three flags on /gtw pr,
// next to the --ready flag it already had.
func TestParsePRArgs_ScopeFlags(t *testing.T) {
	cases := []struct {
		argv []string
		want prArgs
	}{
		{[]string{"-c"}, prArgs{Continue: true}},
		{[]string{"--continue", "--ready"}, prArgs{Continue: true, Ready: true}},
		{[]string{"-g", "-a", "codex"}, prArgs{Agent: "codex", Global: true}},
		{[]string{"--global", "--agent", "codex", "--ready"}, prArgs{Agent: "codex", Global: true, Ready: true}},
		{[]string{"--unset"}, prArgs{Unset: true}},
	}
	for _, tc := range cases {
		got, err := parsePRArgs(tc.argv)
		if err != nil {
			t.Fatalf("parsePRArgs(%v): %v", tc.argv, err)
		}
		if got != tc.want {
			t.Errorf("parsePRArgs(%v) = %+v, want %+v", tc.argv, got, tc.want)
		}
	}
}

// TestParseArgs_UnknownFlagRejected pins the "unknown flag"
// contract for the new flags' neighbours: a flag nobody declared
// is still refused.
func TestParseArgs_UnknownFlagRejected(t *testing.T) {
	for _, argv := range [][]string{{"--contine"}, {"--globals"}, {"-x"}} {
		if _, err := parseCommitArgs(argv); err == nil {
			t.Errorf("parseCommitArgs(%v) returned no error; want 'unknown flag'", argv)
		} else if !strings.Contains(err.Error(), "unknown flag") {
			t.Errorf("parseCommitArgs(%v) error lacks 'unknown flag'; got %q", argv, err.Error())
		}
		if _, err := parsePRArgs(argv); err == nil {
			t.Errorf("parsePRArgs(%v) returned no error; want 'unknown flag'", argv)
		} else if !strings.Contains(err.Error(), "unknown flag") {
			t.Errorf("parsePRArgs(%v) error lacks 'unknown flag'; got %q", argv, err.Error())
		}
	}
}

// TestValidateGTWFlags pins the flag combinations the two commands
// refuse — the /review rule set without --abort and --timeout.
func TestValidateGTWFlags(t *testing.T) {
	cases := []struct {
		name    string
		agent   string
		global  bool
		unset   bool
		cont    bool
		wantErr string
	}{
		{"continue and global", "", true, false, true, "--continue and --global are mutually exclusive"},
		{"continue and unset", "", false, true, true, "--continue and --unset are mutually exclusive"},
		{"unset and agent", "codex", false, true, false, "--unset and --agent are mutually exclusive"},
		{"unset without global", "", false, true, false, "--unset requires --global"},
		{"global without agent", "", true, false, false, "--global requires --agent <name>"},
		{"continue with agent is a takeover", "codex", false, false, true, ""},
		{"continue with ready is orthogonal", "", false, false, true, ""},
		{"global with unset removes the default", "", true, true, false, ""},
		{"no flags", "", false, false, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateGTWFlags(tc.agent, tc.global, tc.unset, tc.cont)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("validateGTWFlags = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validateGTWFlags = nil, want %q", tc.wantErr)
			}
			if err.Error() != tc.wantErr {
				t.Errorf("validateGTWFlags = %q, want %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// The global scope: ~/.nightme/gtw.yml
// -----------------------------------------------------------------------------

// TestSetGlobalAgent_WritesTheSection covers the write path: the
// subcommand's section is created and carries the agent.
func TestSetGlobalAgent_WritesTheSection(t *testing.T) {
	home := sandboxHome(t)
	if err := setGlobalAgent(subCommit, "codex"); err != nil {
		t.Fatalf("setGlobalAgent: %v", err)
	}
	data, err := os.ReadFile(userYmlPath(home))
	if err != nil {
		t.Fatalf("read gtw.yml: %v", err)
	}
	got := string(data)
	if !strings.Contains(got, "commit:") {
		t.Errorf("missing commit section:\n%s", got)
	}
	if !strings.Contains(got, "agent: codex") {
		t.Errorf("missing agent key:\n%s", got)
	}
}

// TestSetGlobalAgent_KeepsHooksAndUnknownKeys covers the write
// path's promise: the node-level edit leaves hand-written hooks and
// unknown keys alone.
func TestSetGlobalAgent_KeepsHooksAndUnknownKeys(t *testing.T) {
	home := sandboxHome(t)
	writeUserYml(t, home, "commit:\n  hooks:\n    before:\n      - run: codegraph init\nunknown_key: keep-me\n")

	if err := setGlobalAgent(subCommit, "codex"); err != nil {
		t.Fatalf("setGlobalAgent: %v", err)
	}
	data, _ := os.ReadFile(userYmlPath(home))
	got := string(data)
	for _, want := range []string{"codegraph init", "unknown_key: keep-me", "agent: codex"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

// TestUnsetGlobalAgent_RemovesTheKey covers the delete path: the
// key goes, and a section left empty goes with it.
func TestUnsetGlobalAgent_RemovesTheKey(t *testing.T) {
	home := sandboxHome(t)
	writeUserYml(t, home, "commit:\n  agent: codex\nlang: English\n")

	if err := unsetGlobalAgent(subCommit); err != nil {
		t.Fatalf("unsetGlobalAgent: %v", err)
	}
	data, _ := os.ReadFile(userYmlPath(home))
	got := string(data)
	if strings.Contains(got, "agent:") {
		t.Errorf("agent key survived:\n%s", got)
	}
	if strings.Contains(got, "commit:") {
		t.Errorf("empty commit section survived:\n%s", got)
	}
	if !strings.Contains(got, "lang: English") {
		t.Errorf("unrelated key was dropped:\n%s", got)
	}
}

// TestUnsetGlobalAgent_KeepsTheHooks covers the delete path's
// other half: a section that still carries hooks stays.
func TestUnsetGlobalAgent_KeepsTheHooks(t *testing.T) {
	home := sandboxHome(t)
	writeUserYml(t, home, "commit:\n  agent: codex\n  hooks:\n    before:\n      - run: codegraph init\n")

	if err := unsetGlobalAgent(subCommit); err != nil {
		t.Fatalf("unsetGlobalAgent: %v", err)
	}
	data, _ := os.ReadFile(userYmlPath(home))
	got := string(data)
	if strings.Contains(got, "agent:") {
		t.Errorf("agent key survived:\n%s", got)
	}
	if !strings.Contains(got, "codegraph init") {
		t.Errorf("hooks were dropped:\n%s", got)
	}
}

// TestApplyGlobalScope_UnknownAgentIsRejected covers the name
// check: a name no registered starter answers to is refused and
// nothing is written.
func TestApplyGlobalScope_UnknownAgentIsRejected(t *testing.T) {
	home := sandboxHome(t)
	withAgent(t, &recordingAgent{name: "claude"})

	_, _, _, err := applyGlobalScope(subCommit, "nosuchagent", true, false, "committing")
	if err == nil {
		t.Fatalf("applyGlobalScope accepted an unregistered agent")
	}
	if !strings.Contains(err.Error(), "unknown agent") {
		t.Errorf("error = %q, want it to name the unknown agent", err.Error())
	}
	if _, statErr := os.Stat(userYmlPath(home)); statErr == nil {
		t.Errorf("the config file was written for an unregistered agent")
	}
}

// -----------------------------------------------------------------------------
// The global scope: the command surface
// -----------------------------------------------------------------------------

// TestRunCommit_GlobalPersistsTheAgent covers `/gtw commit -a
// codex -g`: the agent is written to ~/.nightme/gtw.yml and the
// command still runs.
func TestRunCommit_GlobalPersistsTheAgent(t *testing.T) {
	home := sandboxHome(t)
	codex := &recordingAgent{name: "codex", runOnceText: "committed"}
	withAgent(t, codex)

	git := newPushGit()
	git.onArgs(statusCmd, "## wt\n M foo.go\n", "", nil)
	git.onArgs([]string{"rev-parse", "HEAD"}, "aaaa\n", "", nil)

	withCwd(t, t.TempDir())
	writeYml(t, mustPwd(t), Context{Worktree: mustPwd(t), Branch: "wt", RepoRoot: mustPwd(t)})
	cs := newPushChatSession(t)
	s := captureCh(t, cs)

	f := NewFactoryWithDeps(NewManager(), HandlerDeps{Git: git})
	_, err := f.Handle(context.Background(), command.RuntimeServices{}, nil, cs, command.SlashInput{
		ChatID:    "chat",
		MessageID: "msg",
		Text:      "/gtw commit -a codex -g",
		Args:      []string{"gtw", "commit", "-a", "codex", "-g"},
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	data, readErr := os.ReadFile(userYmlPath(home))
	if readErr != nil {
		t.Fatalf("read gtw.yml: %v", readErr)
	}
	if !strings.Contains(string(data), "agent: codex") {
		t.Errorf("commit.agent was not persisted:\n%s", data)
	}
	if !strings.Contains(allText(s), "commit.agent = codex (global)") {
		t.Errorf("the reply does not confirm the write: %q", allText(s))
	}
}

// allText returns the text of every message the emitter received.
func allText(ch *recordingCh) string {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	var sb strings.Builder
	for _, m := range ch.sends {
		sb.WriteString(m.Text)
		sb.WriteString("\n")
	}
	return sb.String()
}

// TestRunCommit_UnsetRemovesTheAgent covers `/gtw commit -g
// --unset`: the key goes, the command does not run.
func TestRunCommit_UnsetRemovesTheAgent(t *testing.T) {
	home := sandboxHome(t)
	writeUserYml(t, home, "commit:\n  agent: codex\n")
	codex := &recordingAgent{name: "codex", runOnceText: "committed"}
	withAgent(t, codex)

	f := NewFactoryWithDeps(NewManager(), HandlerDeps{})
	_, err := f.Handle(context.Background(), command.RuntimeServices{}, nil, &chatsession.ChatSession{}, command.SlashInput{
		ChatID:    "chat",
		MessageID: "msg",
		Text:      "/gtw commit -g --unset",
		Args:      []string{"gtw", "commit", "-g", "--unset"},
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	data, _ := os.ReadFile(userYmlPath(home))
	if strings.Contains(string(data), "agent:") {
		t.Errorf("commit.agent survived the unset:\n%s", data)
	}
	if len(codex.calls) != 0 {
		t.Errorf("the unset path ran the agent %d time(s); want 0", len(codex.calls))
	}
}

// TestRunCommit_GlobalWithUnknownAgentIsRejected covers `-a
// nosuchagent -g`: the name is refused, nothing is written, and
// the command does not run.
func TestRunCommit_GlobalWithUnknownAgentIsRejected(t *testing.T) {
	home := sandboxHome(t)
	withAgent(t, &recordingAgent{name: "claude"})

	p := newOutcomeProbe(t)
	f := NewFactoryWithDeps(NewManager(), HandlerDeps{})
	_, err := f.Handle(p.ctx, command.RuntimeServices{}, nil, p.cs, command.SlashInput{
		ChatID:    "chat",
		MessageID: "msg-1",
		Text:      "/gtw commit -a nosuchagent -g",
		Args:      []string{"gtw", "commit", "-a", "nosuchagent", "-g"},
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got := p.settle(t)
	assertOutcome(t, got, http.StatusNotFound, nil)
	if _, statErr := os.Stat(userYmlPath(home)); statErr == nil {
		t.Errorf("the config file was written for an unregistered agent")
	}
}

// TestRunCommit_GlobalThenPlainRunUsesTheAgent covers the promise
// of `-g`: a later run without `-a` uses the persisted agent, not
// the chat's own.
func TestRunCommit_GlobalThenPlainRunUsesTheAgent(t *testing.T) {
	sandboxHome(t)
	codex := &recordingAgent{name: "codex", runOnceText: "committed"}
	claude := &recordingAgent{name: "claude", runOnceText: "should-not-be-called"}
	withAgent(t, claude, codex)

	git := newPushGit()
	git.onArgsSeq(statusCmd,
		pushGitResp{"## wt\n M foo.go\n", "", nil},
		pushGitResp{"## wt\n", "", nil},
		pushGitResp{"## wt\n M foo.go\n", "", nil},
		pushGitResp{"## wt\n", "", nil},
	)
	git.onArgsSeq([]string{"rev-parse", "HEAD"},
		pushGitResp{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n", "", nil},
		pushGitResp{"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\n", "", nil},
		pushGitResp{"cccccccccccccccccccccccccccccccccccccccc\n", "", nil},
		pushGitResp{"dddddddddddddddddddddddddddddddddddddddd\n", "", nil},
	)
	git.onArgs([]string{"--no-optional-locks", "status", "--porcelain"}, "", "", nil)
	git.onArgs([]string{"rev-parse", "--abbrev-ref", "HEAD"}, "wt\n", "", nil)
	git.on("log", "dddddddd feat: the agent did the thing\n", "", nil)

	withCwd(t, t.TempDir())
	writeYml(t, mustPwd(t), Context{Worktree: mustPwd(t), Branch: "wt", RepoRoot: mustPwd(t)})
	cs := newPushChatSession(t)
	_ = cs.SetSelectedAgent("claude")

	f := NewFactoryWithDeps(NewManager(), HandlerDeps{Git: git})
	for _, argv := range [][]string{
		{"gtw", "commit", "-a", "codex", "-g"},
		{"gtw", "commit"},
	} {
		if _, err := f.Handle(context.Background(), command.RuntimeServices{}, nil, cs, command.SlashInput{
			ChatID:    "chat",
			MessageID: "msg",
			Text:      "/gtw commit",
			Args:      argv,
		}); err != nil {
			t.Fatalf("Handle(%v): %v", argv, err)
		}
	}

	if len(claude.calls) != 0 {
		t.Errorf("the chat's agent ran %d time(s); want 0 (the persisted agent wins)", len(claude.calls))
	}
	if len(codex.calls) != 2 {
		t.Errorf("the persisted agent ran %d time(s), want 2", len(codex.calls))
	}
}

// TestRunCommit_FlagCombinationIsRefused covers the usage gate:
// the combinations the commands cannot act on are refused before
// anything else happens.
func TestRunCommit_FlagCombinationIsRefused(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		want string
	}{
		{"continue and global", []string{"gtw", "commit", "-c", "-g", "-a", "codex"}, "--continue and --global are mutually exclusive"},
		{"continue and unset", []string{"gtw", "commit", "-c", "--unset"}, "--continue and --unset are mutually exclusive"},
		{"unset and agent", []string{"gtw", "commit", "--unset", "-g", "-a", "codex"}, "--unset and --agent are mutually exclusive"},
		{"unset without global", []string{"gtw", "commit", "--unset"}, "--unset requires --global"},
		{"global without agent", []string{"gtw", "commit", "-g"}, "--global requires --agent <name>"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newOutcomeProbe(t)
			f := NewFactoryWithDeps(NewManager(), HandlerDeps{})
			out, err := f.Handle(p.ctx, command.RuntimeServices{}, nil, p.cs, command.SlashInput{
				ChatID:    "chat",
				MessageID: "msg-1",
				Text:      "/gtw commit",
				Args:      tc.argv,
			})
			if err != nil {
				t.Fatalf("Handle: %v", err)
			}
			if !strings.Contains(out.Reply, tc.want) {
				t.Errorf("reply = %q, want it to name the refused combination (%q)", out.Reply, tc.want)
			}
			got := p.settle(t)
			assertOutcome(t, got, http.StatusBadRequest, nil)
		})
	}
}

// TestRunCommit_GlobalWriteFailureStillRuns covers the write
// failure: the command runs anyway, the warning rides along, and
// the outcome says the persistence did not happen.
func TestRunCommit_GlobalWriteFailureStillRuns(t *testing.T) {
	home := sandboxHome(t)
	// A malformed yml is never rewritten — the write fails.
	writeUserYml(t, home, "lang: [unclosed\n")
	codex := &recordingAgent{name: "codex", runOnceText: "committed"}
	withAgent(t, codex)

	git := commitContinueRig(t, "wt-write-fail")
	withCwd(t, t.TempDir())
	writeYml(t, mustPwd(t), Context{Worktree: mustPwd(t), Branch: "wt-write-fail", RepoRoot: mustPwd(t)})
	cs := newPushChatSession(t)
	s := captureCh(t, cs)

	p := newOutcomeProbe(t)
	f := NewFactoryWithDeps(NewManager(), HandlerDeps{Git: git})
	_, err := f.Handle(p.ctx, command.RuntimeServices{}, nil, cs, command.SlashInput{
		ChatID:    "chat",
		MessageID: "msg",
		Text:      "/gtw commit -a codex -g",
		Args:      []string{"gtw", "commit", "-a", "codex", "-g"},
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(codex.calls) != 1 {
		t.Errorf("the agent ran %d time(s), want 1 (the command runs anyway)", len(codex.calls))
	}
	if !strings.Contains(allText(s), "failed to save commit.agent") {
		t.Errorf("the warning is missing from the reply:\n%s", allText(s))
	}
	got := p.settle(t)
	assertOutcome(t, got, http.StatusInternalServerError, nil)
}

// -----------------------------------------------------------------------------
// The continuation: /gtw commit
// -----------------------------------------------------------------------------

// commitContinueRig wires the git surface a continuation needs:
// a dirty worktree, a HEAD that advances, and a clean tree
// afterwards.
func commitContinueRig(t *testing.T, branch string) *pushGit {
	t.Helper()
	git := newPushGit()
	git.onArgsSeq(statusCmd,
		pushGitResp{"## " + branch + "...origin/" + branch + "\n M foo.go\n", "", nil},
		pushGitResp{"## " + branch + "...origin/" + branch + "\n", "", nil},
	)
	git.onArgsSeq([]string{"rev-parse", "HEAD"},
		pushGitResp{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n", "", nil},
		pushGitResp{"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\n", "", nil},
	)
	git.onArgs([]string{"--no-optional-locks", "status", "--porcelain"}, "", "", nil)
	git.onArgs([]string{"rev-parse", "--abbrev-ref", "HEAD"}, branch+"\n", "", nil)
	git.on("log", "bbbbbbbb feat: the agent did the thing\n", "", nil)
	return git
}

// TestDispatchCommit_ContinueWithoutARecord covers the "nothing to
// continue" path: the chat has no failed commit to pick up.
func TestDispatchCommit_ContinueWithoutARecord(t *testing.T) {
	p := newOutcomeProbe(t)
	git := commitContinueRig(t, "wt-continue-none")
	withAgent(t, &recordingAgent{name: "claude", runOnceText: "committed"})

	_, err := dispatchCommit(p.ctx, p.cs,
		HandlerDeps{Git: git, records: newRecordStore()},
		"chat", "msg-1", commitArgs{Continue: true}, Config{})
	if err != nil {
		t.Fatalf("dispatchCommit: %v", err)
	}
	if !strings.Contains(p.ch.lastText(), "no failed /gtw commit to continue") {
		t.Errorf("reply = %q, want the no-continue refusal", p.ch.lastText())
	}
	got := p.settle(t)
	assertOutcome(t, got, http.StatusNotFound, nil)
}

// TestDispatchCommit_ContinueAfterSuccess covers the "the last one
// completed" path: the record is there, but it holds no failure.
func TestDispatchCommit_ContinueAfterSuccess(t *testing.T) {
	p := newOutcomeProbe(t)
	git := commitContinueRig(t, "wt-continue-done")
	withAgent(t, &recordingAgent{name: "claude", runOnceText: "committed"})

	records := newRecordStore()
	records.Set("chat", subCommit, gtwRecord{agent: "claude", ctx: Context{Branch: "wt-continue-done"}})

	_, err := dispatchCommit(p.ctx, p.cs,
		HandlerDeps{Git: git, records: records},
		"chat", "msg-1", commitArgs{Continue: true}, Config{})
	if err != nil {
		t.Fatalf("dispatchCommit: %v", err)
	}
	if !strings.Contains(p.ch.lastText(), "the last /gtw commit completed") {
		t.Errorf("reply = %q, want the nothing-to-continue notice", p.ch.lastText())
	}
	got := p.settle(t)
	assertOutcome(t, got, http.StatusOK, nil)
}

// TestDispatchCommit_FailureIsRecorded covers the write side: an
// agent that never ran leaves a record a continuation can pick up.
func TestDispatchCommit_FailureIsRecorded(t *testing.T) {
	p := newOutcomeProbe(t)
	git := newPushGit()
	git.onArgs(statusCmd, "## wt-fail...origin/wt-fail\n M foo.go\n", "", nil)
	git.onArgs([]string{"rev-parse", "HEAD"}, "aaaa\n", "", nil)

	claude := &recordingAgent{name: "claude", runOnceErr: errBoom}
	withAgent(t, claude)
	if err := p.cs.SetSelectedAgent("claude"); err != nil {
		t.Fatalf("SetSelectedAgent: %v", err)
	}

	withCwd(t, t.TempDir())
	writeYml(t, mustPwd(t), Context{Worktree: mustPwd(t), Branch: "wt-fail", RepoRoot: mustPwd(t)})
	if err := p.cs.SetSelectedCwd(mustPwd(t)); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
	}

	records := newRecordStore()
	_, err := dispatchCommit(p.ctx, p.cs,
		HandlerDeps{Git: git, records: records},
		"chat", "msg-1", commitArgs{}, Config{})
	if err != nil {
		t.Fatalf("dispatchCommit: %v", err)
	}
	rec, ok := records.Lookup("chat", subCommit)
	if !ok {
		t.Fatalf("the failed run left no record")
	}
	if !rec.failed {
		t.Errorf("the record does not mark the run as failed: %+v", rec)
	}
	if rec.agent != "claude" {
		t.Errorf("record agent = %q, want claude", rec.agent)
	}
	if !strings.Contains(rec.prompt, "You are a release engineer") {
		t.Errorf("the record kept no prompt to replay:\n%s", rec.prompt)
	}
	if rec.sessionID != "" {
		t.Errorf("record session id = %q, want empty (the agent never ran)", rec.sessionID)
	}
}

// TestDispatchCommit_ContinueReplaysThePrompt covers the "the
// attempt never reached the agent" case: there is no conversation
// to continue, so the recorded prompt is replayed.
func TestDispatchCommit_ContinueReplaysThePrompt(t *testing.T) {
	p := newOutcomeProbe(t)
	git := commitContinueRig(t, "wt-replay")
	claude := &recordingAgent{name: "claude", runOnceText: "committed"}
	withAgent(t, claude)

	records := newRecordStore()
	records.Set("chat", subCommit, gtwRecord{
		agent:  "claude",
		ctx:    Context{Worktree: "/w", Branch: "wt-replay"},
		prompt: "the recorded prompt",
		failed: true,
	})

	_, err := dispatchCommit(p.ctx, p.cs,
		HandlerDeps{Git: git, records: records},
		"chat", "msg-1", commitArgs{Continue: true}, Config{})
	if err != nil {
		t.Fatalf("dispatchCommit: %v", err)
	}
	claude.mu.Lock()
	calls := len(claude.calls)
	text := ""
	if calls == 1 && len(claude.calls[0].blocks) > 0 {
		text = claude.calls[0].blocks[0].Text
	}
	claude.mu.Unlock()
	if calls != 1 {
		t.Fatalf("the agent ran %d time(s), want 1", calls)
	}
	if text != "the recorded prompt" {
		t.Errorf("the agent received %q, want the recorded prompt", text)
	}
	got := p.settle(t)
	assertOutcome(t, got, http.StatusOK, nil)
}

// TestDispatchCommit_ContinueResumesTheSession covers the "the
// attempt reached the agent" case: the session is reopened and sent
// the text "continue".
func TestDispatchCommit_ContinueResumesTheSession(t *testing.T) {
	p := newOutcomeProbe(t)
	git := commitContinueRig(t, "wt-resume")

	starter := &sessionStarter{name: "claude", text: "committed", sessionID: "sess-1"}
	withStarters(t, starter)

	records := newRecordStore()
	records.Set("chat", subCommit, gtwRecord{
		agent:     "claude",
		ctx:       Context{Worktree: "/w", Branch: "wt-resume"},
		prompt:    "the recorded prompt",
		sessionID: "sess-1",
		failed:    true,
	})

	_, err := dispatchCommit(p.ctx, p.cs,
		HandlerDeps{Git: git, records: records},
		"chat", "msg-1", commitArgs{Continue: true}, Config{})
	if err != nil {
		t.Fatalf("dispatchCommit: %v", err)
	}
	starts := starter.startedConfigs()
	if len(starts) != 1 {
		t.Fatalf("the agent was started %d time(s), want 1", len(starts))
	}
	if starts[0].SessionID != "sess-1" {
		t.Errorf("the session was not reopened: SessionID = %q, want sess-1", starts[0].SessionID)
	}
	sent := starter.sentBlocks()
	if len(sent) != 1 {
		t.Fatalf("the session received %d block(s), want 1", len(sent))
	}
	if sent[0].Text != continueText {
		t.Errorf("the session received %q, want %q", sent[0].Text, continueText)
	}
}

// TestDispatchCommit_ContinueTakeoverReplaysOnTheNewAgent covers
// `--continue -a codex`: a session belongs to the agent that ran
// it, so the new agent gets the recorded prompt instead.
func TestDispatchCommit_ContinueTakeoverReplaysOnTheNewAgent(t *testing.T) {
	p := newOutcomeProbe(t)
	git := commitContinueRig(t, "wt-takeover")

	claude := &recordingAgent{name: "claude", runOnceText: "should-not-be-called"}
	codex := &recordingAgent{name: "codex", runOnceText: "committed"}
	withAgent(t, claude, codex)

	records := newRecordStore()
	records.Set("chat", subCommit, gtwRecord{
		agent:     "claude",
		ctx:       Context{Worktree: "/w", Branch: "wt-takeover"},
		prompt:    "the recorded prompt",
		sessionID: "sess-1",
		failed:    true,
	})

	_, err := dispatchCommit(p.ctx, p.cs,
		HandlerDeps{Git: git, records: records},
		"chat", "msg-1", commitArgs{Continue: true, Agent: "codex"}, Config{})
	if err != nil {
		t.Fatalf("dispatchCommit: %v", err)
	}
	if len(claude.calls) != 0 {
		t.Errorf("the recorded agent ran %d time(s); want 0 (the session cannot move)", len(claude.calls))
	}
	if len(codex.calls) != 1 {
		t.Fatalf("the takeover agent ran %d time(s), want 1", len(codex.calls))
	}
	codex.mu.Lock()
	text := ""
	if len(codex.calls) == 1 && len(codex.calls[0].blocks) > 0 {
		text = codex.calls[0].blocks[0].Text
	}
	codex.mu.Unlock()
	if text != "the recorded prompt" {
		t.Errorf("the takeover agent received %q, want the recorded prompt", text)
	}
}

// TestDispatchCommit_ContinueUsesTheRecordedWorktree covers the
// workspace contract: the continuation works where the failed
// attempt worked, not where the chat's cwd points now.
func TestDispatchCommit_ContinueUsesTheRecordedWorktree(t *testing.T) {
	p := newOutcomeProbe(t)
	git := commitContinueRig(t, "wt-frozen")
	withAgent(t, &recordingAgent{name: "claude", runOnceText: "committed"})

	records := newRecordStore()
	records.Set("chat", subCommit, gtwRecord{
		agent:  "claude",
		ctx:    Context{Worktree: "/frozen/worktree", Branch: "wt-frozen"},
		prompt: "the recorded prompt",
		failed: true,
	})

	// The chat's cwd points somewhere else entirely.
	if err := p.cs.SetSelectedCwd("/somewhere/else"); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
	}

	_, err := dispatchCommit(p.ctx, p.cs,
		HandlerDeps{Git: git, records: records},
		"chat", "msg-1", commitArgs{Continue: true}, Config{})
	if err != nil {
		t.Fatalf("dispatchCommit: %v", err)
	}
	for _, c := range git.calls {
		if c.dir != "/frozen/worktree" {
			t.Errorf("git ran in %q, want the recorded worktree %q", c.dir, "/frozen/worktree")
			break
		}
	}
	got := p.settle(t)
	assertOutcome(t, got, http.StatusOK, nil)
}

// -----------------------------------------------------------------------------
// The continuation: /gtw pr
// -----------------------------------------------------------------------------

// TestDispatchPR_ContinueWithoutARecord covers the "nothing to
// continue" path of /gtw pr.
func TestDispatchPR_ContinueWithoutARecord(t *testing.T) {
	p := newOutcomeProbe(t)
	rig := newPRTestRig(t)
	rig.installDeps()
	rig.deps.records = newRecordStore()

	_, err := dispatchPR(p.ctx, p.cs, rig.deps,
		"chat", "msg-1", prArgs{Continue: true}, Config{})
	if err != nil {
		t.Fatalf("dispatchPR: %v", err)
	}
	if !strings.Contains(p.ch.lastText(), "no failed /gtw pr to continue") {
		t.Errorf("reply = %q, want the no-continue refusal", p.ch.lastText())
	}
	got := p.settle(t)
	assertOutcome(t, got, http.StatusNotFound, nil)
}

// TestDispatchPR_FormatFailureIsRecorded covers the write side of
// the PR flow: a reply that fails the format gate leaves a record
// that keeps the correction prompt — the prompt a takeover
// replays.
func TestDispatchPR_FormatFailureIsRecorded(t *testing.T) {
	p := newOutcomeProbe(t)
	rig := newPRTestRig(t)
	rig.installDeps()
	rig.deps.records = newRecordStore()
	dir := setupPRWorktree(t, rig, Context{Branch: "wt-pr-fail"})
	if err := p.cs.SetSelectedCwd(dir); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
	}
	if err := p.cs.SetSelectedAgent("claude"); err != nil {
		t.Fatalf("SetSelectedAgent: %v", err)
	}
	setupPRGit(rig, "wt-pr-fail", 0)

	// The agent answers with prose — no title, no summary heading.
	rig.agent.runOnceText = "Here is a summary of the change."

	_, err := dispatchPR(p.ctx, p.cs, rig.deps,
		"chat", "msg-1", prArgs{}, Config{})
	if err != nil {
		t.Fatalf("dispatchPR: %v", err)
	}
	rec, ok := rig.deps.records.Lookup("chat", subPR)
	if !ok {
		t.Fatalf("the failed run left no record")
	}
	if !rec.failed {
		t.Errorf("the record does not mark the run as failed: %+v", rec)
	}
	if !strings.Contains(rec.prompt, "## Correction") {
		t.Errorf("the record kept no correction prompt:\n%s", rec.prompt)
	}
	if !strings.Contains(rec.prompt, "Here is a summary of the change.") {
		t.Errorf("the correction prompt does not quote the rejected reply:\n%s", rec.prompt)
	}
}

// TestDispatchPR_ContinueReplaysTheStoredPrompt covers the PR
// continuation: the stored prompt — the one the format gate's
// correction round built — is replayed, and the pull request opens
// when the reply passes the gate.
func TestDispatchPR_ContinueReplaysTheStoredPrompt(t *testing.T) {
	p := newOutcomeProbe(t)
	rig := newPRTestRig(t)
	rig.installDeps()
	rig.deps.records = newRecordStore()
	dir := setupPRWorktree(t, rig, Context{Branch: "wt-pr-continue"})
	if err := p.cs.SetSelectedCwd(dir); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
	}
	if err := p.cs.SetSelectedAgent("claude"); err != nil {
		t.Fatalf("SetSelectedAgent: %v", err)
	}
	setupPRGit(rig, "wt-pr-continue", 0)

	// The stored prompt is what the failed attempt was given.
	rig.deps.records.Set("chat", subPR, gtwRecord{
		agent:  "claude",
		ctx:    Context{Worktree: dir, Branch: "wt-pr-continue", RepoRoot: dir},
		prompt: "the stored prompt",
		failed: true,
	})

	_, err := dispatchPR(p.ctx, p.cs, rig.deps,
		"chat", "msg-1", prArgs{Continue: true}, Config{})
	if err != nil {
		t.Fatalf("dispatchPR: %v", err)
	}
	rig.agent.mu.Lock()
	calls := len(rig.agent.calls)
	text := ""
	if calls >= 1 && len(rig.agent.calls[0].blocks) > 0 {
		text = rig.agent.calls[0].blocks[0].Text
	}
	rig.agent.mu.Unlock()
	if calls != 1 {
		t.Fatalf("the agent ran %d time(s), want 1", calls)
	}
	if text != "the stored prompt" {
		t.Errorf("the agent received %q, want the stored prompt", text)
	}
	if !strings.Contains(p.ch.lastText(), "PR opened") {
		t.Errorf("the pull request was not opened: %q", p.ch.lastText())
	}
	got := p.settle(t)
	assertOutcome(t, got, http.StatusOK, nil)
}

// TestDispatchCommit_ContinueUpdatesTheRecord covers the chain: a
// continuation that fails again leaves a record the next
// continuation can pick up.
func TestDispatchCommit_ContinueUpdatesTheRecord(t *testing.T) {
	p := newOutcomeProbe(t)
	git := newPushGit()
	git.onArgs(statusCmd, "## wt-chain...origin/wt-chain\n M foo.go\n", "", nil)
	git.onArgs([]string{"rev-parse", "HEAD"}, "aaaa\n", "", nil)
	withAgent(t, &recordingAgent{name: "claude", runOnceErr: errBoom})

	records := newRecordStore()
	records.Set("chat", subCommit, gtwRecord{
		agent:  "claude",
		ctx:    Context{Worktree: "/w", Branch: "wt-chain"},
		prompt: "the recorded prompt",
		failed: true,
	})

	_, err := dispatchCommit(p.ctx, p.cs,
		HandlerDeps{Git: git, records: records},
		"chat", "msg-1", commitArgs{Continue: true}, Config{})
	if err != nil {
		t.Fatalf("dispatchCommit: %v", err)
	}
	rec, ok := records.Lookup("chat", subCommit)
	if !ok {
		t.Fatalf("the failed continuation left no record")
	}
	if !rec.failed {
		t.Errorf("the record does not mark the continuation as failed: %+v", rec)
	}
	if rec.prompt != "the recorded prompt" {
		t.Errorf("the record kept %q, want the prompt the work was asked with", rec.prompt)
	}
}

// TestDispatchCommit_AbortedRunKeepsTheRecord covers the abort
// rule: a run the user stopped is not a failure — it neither
// writes a record nor clears the one that is there.
func TestDispatchCommit_AbortedRunKeepsTheRecord(t *testing.T) {
	p := newOutcomeProbe(t)
	git := commitContinueRig(t, "wt-abort")
	withAgent(t, &recordingAgent{name: "claude", runOnceText: "committed"})

	records := newRecordStore()
	records.Set("chat", subCommit, gtwRecord{
		agent:  "claude",
		ctx:    Context{Worktree: "/w", Branch: "wt-abort"},
		prompt: "the recorded prompt",
		failed: true,
	})

	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(ErrAborted)

	_, err := dispatchCommit(ctx, p.cs,
		HandlerDeps{Git: git, records: records},
		"chat", "msg-1", commitArgs{}, Config{})
	if err != nil {
		t.Fatalf("dispatchCommit: %v", err)
	}
	rec, ok := records.Lookup("chat", subCommit)
	if !ok {
		t.Fatalf("the aborted run dropped the record")
	}
	if !rec.failed {
		t.Errorf("the aborted run cleared the record: %+v", rec)
	}
	if rec.prompt != "the recorded prompt" {
		t.Errorf("the aborted run overwrote the record: %+v", rec)
	}
}

// TestDispatchCommit_ContinueStopsWhenTheWorkIsDone covers the
// stale record of a failed attempt whose work the user finished by
// hand: the continuation re-runs the readiness gate first, so it
// reports the finished work instead of resuming the old session.
func TestDispatchCommit_ContinueStopsWhenTheWorkIsDone(t *testing.T) {
	p := newOutcomeProbe(t)
	// A clean worktree: the user committed by hand.
	git := newPushGit()
	git.onArgs(statusCmd, "## wt-done...origin/wt-done\n", "", nil)
	claude := &recordingAgent{name: "claude", runOnceText: "should-not-be-called"}
	withAgent(t, claude)

	records := newRecordStore()
	records.Set("chat", subCommit, gtwRecord{
		agent:     "claude",
		ctx:       Context{Worktree: "/w", Branch: "wt-done"},
		prompt:    "the recorded prompt",
		sessionID: "sess-1",
		failed:    true,
	})

	_, err := dispatchCommit(p.ctx, p.cs,
		HandlerDeps{Git: git, records: records},
		"chat", "msg-1", commitArgs{Continue: true}, Config{})
	if err != nil {
		t.Fatalf("dispatchCommit: %v", err)
	}
	if len(claude.calls) != 0 {
		t.Errorf("the agent ran %d time(s); want 0 (the work is already done)", len(claude.calls))
	}
	if !strings.Contains(p.ch.lastText(), "nothing to commit") {
		t.Errorf("reply = %q, want it to report the finished work", p.ch.lastText())
	}
}

// TestDispatchCommit_ContinueWithNoRecordedAgentResolvesOne covers
// the failed attempt that named no agent: there was none to run, so
// the record carries no name. The continuation resolves one now —
// the tiers decide, exactly as a fresh run does — instead of
// refusing with an empty name.
func TestDispatchCommit_ContinueWithNoRecordedAgentResolvesOne(t *testing.T) {
	p := newOutcomeProbe(t)
	git := commitContinueRig(t, "wt-no-agent")
	claude := &recordingAgent{name: "claude", runOnceText: "committed"}
	withAgent(t, claude)
	if err := p.cs.SetSelectedAgent("claude"); err != nil {
		t.Fatalf("SetSelectedAgent: %v", err)
	}

	records := newRecordStore()
	records.Set("chat", subCommit, gtwRecord{
		ctx:    Context{Worktree: "/w", Branch: "wt-no-agent"},
		prompt: "the recorded prompt",
		failed: true,
	})

	_, err := dispatchCommit(p.ctx, p.cs,
		HandlerDeps{Git: git, records: records},
		"chat", "msg-1", commitArgs{Continue: true}, Config{})
	if err != nil {
		t.Fatalf("dispatchCommit: %v", err)
	}
	claude.mu.Lock()
	calls := len(claude.calls)
	claude.mu.Unlock()
	if calls != 1 {
		t.Fatalf("the agent ran %d time(s), want 1", calls)
	}
	if strings.Contains(p.ch.lastText(), "unknown agent") {
		t.Errorf("the continuation refused with an unknown agent: %q", p.ch.lastText())
	}
	got := p.settle(t)
	assertOutcome(t, got, http.StatusOK, nil)
}

// TestRunCommit_ContinueHooksSeeTheRecordedWorkspace covers the
// hook context of a continuation: the before- and the after-hook
// both see the worktree the continuation works in — not the chat's
// current cwd.
func TestRunCommit_ContinueHooksSeeTheRecordedWorkspace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the hook uses sh-style env expansion")
	}
	home := sandboxHome(t)
	marker := filepath.Join(t.TempDir(), "hooks.txt")
	writeUserYml(t, home, fmt.Sprintf(
		"commit:\n  hooks:\n    before:\n      - run: 'echo before $GTW_WORKTREE >> %s'\n    after:\n      - run: 'echo after $GTW_WORKTREE >> %s'\n",
		marker, marker))

	withAgent(t, &recordingAgent{name: "claude", runOnceText: "committed"})
	git := newPushGit() // no response: the continuation stops at the readiness gate

	// The chat's cwd is a workspace of its own; the continuation
	// works in the recorded one.
	withCwd(t, t.TempDir())
	cs := newPushChatSession(t)

	records := newRecordStore()
	records.Set("chat", subCommit, gtwRecord{
		agent:  "claude",
		ctx:    Context{Worktree: "/recorded/worktree", Branch: "wt-recorded"},
		prompt: "the recorded prompt",
		failed: true,
	})

	f := NewFactoryWithDeps(NewManager(), HandlerDeps{Git: git, records: records})
	if _, err := f.Handle(context.Background(), command.RuntimeServices{}, nil, cs, command.SlashInput{
		ChatID:    "chat",
		MessageID: "msg",
		Text:      "/gtw commit --continue",
		Args:      []string{"gtw", "commit", "--continue"},
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("read the hook marker: %v", err)
	}
	got := string(data)
	for _, want := range []string{"before /recorded/worktree", "after /recorded/worktree"} {
		if !strings.Contains(got, want) {
			t.Errorf("the hook output lacks %q:\n%s", want, got)
		}
	}
}

// -----------------------------------------------------------------------------
// Test doubles
// -----------------------------------------------------------------------------

// withStarters swaps agent.Builtins for a local registry holding
// the supplied starters. withAgent covers the recordingAgent fake;
// this one takes any starter.
func withStarters(t *testing.T, starters ...agent.Starter) {
	t.Helper()
	orig := agent.Builtins
	clean := agent.New()
	for _, s := range starters {
		clean.Register(s)
	}
	agent.Builtins = clean
	t.Cleanup(func() { agent.Builtins = orig })
}

// sessionStarter is a Starter whose Start opens a live agent
// session — the shape the continuation's resume path needs (a
// one-shot turn cannot be resumed).
type sessionStarter struct {
	name      string
	text      string
	sessionID string

	mu     sync.Mutex
	starts []agent.StartConfig
	sent   []agent.ContentBlock
}

// startedConfigs returns the StartConfig each Start call received.
func (s *sessionStarter) startedConfigs() []agent.StartConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]agent.StartConfig(nil), s.starts...)
}

// sentBlocks returns the blocks the session received.
func (s *sessionStarter) sentBlocks() []agent.ContentBlock {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]agent.ContentBlock(nil), s.sent...)
}

func (s *sessionStarter) Info() agent.Info {
	return agent.NewInfo(s.name, agent.ModeJSONIO, "fake-"+s.name, nil, nil)
}
func (s *sessionStarter) Detect() error { return nil }
func (s *sessionStarter) Init(string)   {}
func (s *sessionStarter) Clone() agent.Starter {
	return &sessionStarter{name: s.name, text: s.text, sessionID: s.sessionID}
}
func (s *sessionStarter) Start(_ context.Context, cfg agent.StartConfig) (*agent.Agent, error) {
	s.mu.Lock()
	s.starts = append(s.starts, cfg)
	s.mu.Unlock()
	events := make(chan agent.AgentEvent, 8)
	driver := &sessionDriver{
		events: events,
		onSend: func(blocks []agent.ContentBlock) {
			s.mu.Lock()
			s.sent = append(s.sent, blocks...)
			s.mu.Unlock()
		},
	}
	return agent.NewAgent(s.Info(), 0, events, driver), nil
}
func (s *sessionStarter) RunOnce(ctx context.Context, cfg agent.StartConfig, blocks []agent.ContentBlock, opts ...agent.RunOnceOption) (agent.RunResult, error) {
	return agent.RunSession(ctx, s, cfg, blocks, opts...)
}
func (s *sessionStarter) Review(_ context.Context, _ agent.StartConfig, _ ...agent.RunOnceOption) (agent.RunResult, error) {
	return agent.RunResult{}, agent.ErrReviewNotSupported
}

// sessionDriver replays the session's event sequence once the
// prompt has been sent.
type sessionDriver struct {
	events chan agent.AgentEvent
	onSend func([]agent.ContentBlock)
}

func (d *sessionDriver) SendBlocks(_ context.Context, blocks []agent.ContentBlock) error {
	if d.onSend != nil {
		d.onSend(blocks)
	}
	go func() {
		d.events <- agent.AgentEvent{Kind: agent.EventAgentReady}
		d.events <- agent.AgentEvent{
			Kind:   agent.EventAgentResult,
			Result: &agent.AgentResultEvent{Text: "committed"},
		}
		close(d.events)
	}()
	return nil
}
func (d *sessionDriver) SendPermission(string) error { return nil }
func (d *sessionDriver) Reset(context.Context) error { return nil }
func (d *sessionDriver) Close() error                { return nil }
func (d *sessionDriver) Stop(context.Context) error  { return agent.ErrNotSupported }
func (d *sessionDriver) Keepalive(context.Context, func(context.Context) error) error {
	return nil
}

// Ping answers the daemon's liveness probe. The fake is
// responsive unless the test says otherwise.
func (d *sessionDriver) Ping(context.Context) error { return nil }
