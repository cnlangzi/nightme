package review

// The silence threshold of a review run: the flag, the config key, and
// what the chat is told when the threshold fires.
//
// A group that timed out is a failed group — but it stays resumable:
// the environment stalled, not the group's content.

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
	"github.com/cnlangzi/nightme/internal/messages"
)

// writeReviewConfig points the config file at a temp path and writes
// the given YAML into it.
func writeReviewConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	t.Setenv("NIGHTME_CONFIG", path)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	return path
}

// TestParseReviewArgs_Timeout: the flag carries a duration, and a
// value that is not one is rejected.
func TestParseReviewArgs_Timeout(t *testing.T) {
	cases := []struct {
		name    string
		argv    []string
		want    time.Duration
		wantSet bool
		wantErr string
	}{
		{"not given", []string{}, 0, false, ""},
		{"--timeout 5m", []string{"--timeout", "5m"}, 5 * time.Minute, true, ""},
		{"-t 90s (short form)", []string{"-t", "90s"}, 90 * time.Second, true, ""},
		{"--timeout 1h", []string{"--timeout", "1h"}, time.Hour, true, ""},
		{"--timeout 0 disables it", []string{"--timeout", "0"}, 0, true, ""},
		{"--timeout 0s disables it", []string{"--timeout", "0s"}, 0, true, ""},
		{
			name:    "an unparsable value",
			argv:    []string{"--timeout", "bogus"},
			wantErr: "--timeout requires a duration like 15m",
		},
		{
			name:    "an empty value",
			argv:    []string{"--timeout", ""},
			wantErr: "--timeout requires a duration like 15m",
		},
		{
			name:    "a negative value",
			argv:    []string{"--timeout", "-1m"},
			wantErr: "--timeout must not be negative",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec, err := parseReviewArgs(tc.argv)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("parseReviewArgs(%v) = nil error, want %q", tc.argv, tc.wantErr)
				}
				if err.Error() != tc.wantErr {
					t.Fatalf("parseReviewArgs(%v) = %q, want %q", tc.argv, err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseReviewArgs(%v): %v", tc.argv, err)
			}
			if spec.Timeout != tc.want {
				t.Errorf("the timeout = %s, want %s", spec.Timeout, tc.want)
			}
			if spec.TimeoutSet != tc.wantSet {
				t.Errorf("TimeoutSet = %v, want %v", spec.TimeoutSet, tc.wantSet)
			}
		})
	}
}

// TestParseReviewArgs_TimeoutWithoutAValue: the shared lexer rejects
// a value-taking flag whose value is missing.
func TestParseReviewArgs_TimeoutWithoutAValue(t *testing.T) {
	if _, err := parseReviewArgs([]string{"--timeout"}); err == nil {
		t.Fatal("a missing value was accepted")
	}
}

// TestValidateReviewFlags_Timeout: the timeout cannot be combined with
// the flags that start no run.
func TestValidateReviewFlags_Timeout(t *testing.T) {
	cases := []struct {
		name    string
		spec    Spec
		wantErr string
	}{
		{
			name:    "timeout with abort",
			spec:    Spec{TimeoutSet: true, Timeout: 5 * time.Minute, Abort: true},
			wantErr: "--timeout and --abort are mutually exclusive",
		},
		{
			name:    "timeout with unset",
			spec:    Spec{TimeoutSet: true, Timeout: 5 * time.Minute, Unset: true},
			wantErr: "--timeout and --unset are mutually exclusive",
		},
		{"timeout alone", Spec{TimeoutSet: true, Timeout: 5 * time.Minute}, ""},
		{"timeout with a continuation", Spec{TimeoutSet: true, Timeout: 5 * time.Minute, Continue: true}, ""},
		{"timeout with the global scope", Spec{TimeoutSet: true, Timeout: 5 * time.Minute, Global: true, Agent: "pi"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateReviewFlags(tc.spec)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("validateReviewFlags(%+v) = %v, want nil", tc.spec, err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("validateReviewFlags(%+v) = nil, want %q", tc.spec, tc.wantErr)
			case tc.wantErr != "" && err.Error() != tc.wantErr:
				t.Fatalf("validateReviewFlags(%+v) = %q, want %q", tc.spec, err.Error(), tc.wantErr)
			}
		})
	}
}

// TestResolveReviewTimeout: the flag wins over the config value, the
// config value wins over the built-in default, and a config value that
// is not a duration warns and falls back to the default.
func TestResolveReviewTimeout(t *testing.T) {
	t.Run("the flag wins over the config", func(t *testing.T) {
		writeReviewConfig(t, "review:\n  timeout: 5m\n")
		got, notice := resolveReviewTimeout(Spec{TimeoutSet: true, Timeout: 90 * time.Second})
		if got != 90*time.Second {
			t.Errorf("the threshold = %s, want 90s", got)
		}
		if notice != "" {
			t.Errorf("unexpected notice: %q", notice)
		}
	})

	t.Run("the config value is used when the flag is absent", func(t *testing.T) {
		writeReviewConfig(t, "review:\n  timeout: 5m\n")
		got, notice := resolveReviewTimeout(Spec{})
		if got != 5*time.Minute {
			t.Errorf("the threshold = %s, want 5m", got)
		}
		if notice != "" {
			t.Errorf("unexpected notice: %q", notice)
		}
	})

	t.Run("the config value may disable the timeout", func(t *testing.T) {
		writeReviewConfig(t, "review:\n  timeout: 0\n")
		got, notice := resolveReviewTimeout(Spec{})
		if got != 0 {
			t.Errorf("the threshold = %s, want 0", got)
		}
		if notice != "" {
			t.Errorf("unexpected notice: %q", notice)
		}
	})

	t.Run("the built-in default is used when neither says", func(t *testing.T) {
		writeReviewConfig(t, "primary: cc\n")
		got, notice := resolveReviewTimeout(Spec{})
		if got != agent.DefaultReviewGroupTimeout {
			t.Errorf("the threshold = %s, want %s", got, agent.DefaultReviewGroupTimeout)
		}
		if notice != "" {
			t.Errorf("unexpected notice: %q", notice)
		}
	})

	t.Run("a config value that is not a duration warns", func(t *testing.T) {
		writeReviewConfig(t, "review:\n  timeout: soon\n")
		got, notice := resolveReviewTimeout(Spec{})
		if got != agent.DefaultReviewGroupTimeout {
			t.Errorf("the threshold = %s, want %s", got, agent.DefaultReviewGroupTimeout)
		}
		if !strings.Contains(notice, `review.timeout "soon" is not a duration`) {
			t.Errorf("the notice does not name the bad value: %q", notice)
		}
		if !strings.Contains(notice, agent.DefaultReviewGroupTimeout.String()) {
			t.Errorf("the notice does not name the fallback: %q", notice)
		}
	})

	t.Run("a negative config value warns", func(t *testing.T) {
		// A negative threshold would silence the watchdog without
		// saying so: the flag path rejects it, so the config path
		// must not accept it either.
		writeReviewConfig(t, "review:\n  timeout: -5m\n")
		got, notice := resolveReviewTimeout(Spec{})
		if got != agent.DefaultReviewGroupTimeout {
			t.Errorf("the threshold = %s, want %s", got, agent.DefaultReviewGroupTimeout)
		}
		if !strings.Contains(notice, `review.timeout "-5m" must not be negative`) {
			t.Errorf("the notice does not name the bad value: %q", notice)
		}
	})

	t.Run("a config file that cannot be read warns", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.yaml")
		t.Setenv("NIGHTME_CONFIG", path)
		if err := os.WriteFile(path, []byte("review: [oops\n"), 0o600); err != nil {
			t.Fatalf("seed config: %v", err)
		}
		got, notice := resolveReviewTimeout(Spec{})
		if got != agent.DefaultReviewGroupTimeout {
			t.Errorf("the threshold = %s, want %s", got, agent.DefaultReviewGroupTimeout)
		}
		if !strings.Contains(notice, "cannot read review.timeout from") {
			t.Errorf("the notice does not report the unreadable file: %q", notice)
		}
	})
}

// TestReviewTimeoutNotice: the notice names the groups that went
// silent and points at the command that picks them up.
func TestReviewTimeoutNotice(t *testing.T) {
	outcomes := []reviewOutcome{
		{pattern: "**/*.go"},
		{
			pattern: agent.PatternBuiltin,
			err: &agent.GroupTimeoutError{
				Group:   "nightme code reviewer",
				Timeout: 15 * time.Minute,
			},
		},
	}
	got := reviewTimeoutNotice(outcomes, 15*time.Minute)
	want := "⏱️ 1 of 2 subagents timed out — no heartbeat for 15m0s (nightme code reviewer) → /review -c"
	if got != want {
		t.Errorf("the notice = %q, want %q", got, want)
	}
}

// TestReviewTimeoutNotice_NamesEveryGroup: several timed-out groups
// are named together.
func TestReviewTimeoutNotice_NamesEveryGroup(t *testing.T) {
	outcomes := []reviewOutcome{
		{pattern: "**/*.go", err: &agent.GroupTimeoutError{Group: "**/*.go", Timeout: time.Minute}},
		{pattern: "**/*.ts", err: &agent.GroupTimeoutError{Group: "**/*.ts", Timeout: time.Minute}},
	}
	got := reviewTimeoutNotice(outcomes, time.Minute)
	for _, want := range []string{"2 of 2 subagents timed out", "`**/*.go`, `**/*.ts`"} {
		if !strings.Contains(got, want) {
			t.Errorf("the notice does not carry %q: %s", want, got)
		}
	}
}

// TestReviewTimeoutNotice_NoTimeout: a run without a timed-out group
// sends no notice.
func TestReviewTimeoutNotice_NoTimeout(t *testing.T) {
	outcomes := []reviewOutcome{{pattern: "**/*.go"}}
	if got := reviewTimeoutNotice(outcomes, 15*time.Minute); got != "" {
		t.Errorf("unexpected notice: %q", got)
	}
}

// TestReviewVerdict_MarksTheTimedOutGroup: the verdict says the group
// timed out, and still offers the continuation — the group is
// resumable.
func TestReviewVerdict_MarksTheTimedOutGroup(t *testing.T) {
	outcomes := []reviewOutcome{
		{
			pattern:   "**/*.go",
			source:    "system",
			sessionID: "sess-go",
			err: &agent.GroupTimeoutError{
				Group:   "system / **/*.go",
				Timeout: 15 * time.Minute,
			},
		},
	}
	got := newVerdict(testRecord(outcomes, nil, nil), true).line()
	want := "⚠️ review · 0 of 1 subagent done · 0 findings so far\n\n" +
		"❌ **1 of 1 subagent failed**\n" +
		"- `system / **/*.go` (timed out)\n\n" +
		"💡 hint: run `/review --continue` to continue the failed groups"
	if got != want {
		t.Errorf("the verdict is:\n%s\nwant:\n%s", got, want)
	}
}

// TestSpentResults_KeepsTheTimedOutGroup: a timeout does not spend
// the group's recovery attempt — the environment stalled, not the
// group's content.
func TestSpentResults_KeepsTheTimedOutGroup(t *testing.T) {
	c := &outcomeCollector{}
	c.observe(agent.ReviewJob{Pattern: "**/*.go", SessionID: "sess-go"},
		&agent.GroupTimeoutError{Group: "**/*.go", Timeout: 15 * time.Minute})
	c.observe(agent.ReviewJob{Pattern: "**/*.ts", SessionID: "sess-ts"},
		errors.New("boom"))

	out := c.spentResults()
	for _, o := range out {
		switch o.pattern {
		case "**/*.go":
			if o.spent {
				t.Error("a timed-out group spent its recovery attempt")
			}
		case "**/*.ts":
			if !o.spent {
				t.Error("a failed group kept its recovery attempt")
			}
		}
	}
}

// TestHandle_ReviewReportsTheTimedOutGroup: the chat is told which
// group timed out, and the outcome data carries the count.
func TestHandle_ReviewReportsTheTimedOutGroup(t *testing.T) {
	agents := newTestAgents(t)
	starter := &planReviewStarter{
		name: "review-timeout-agent",
		groups: []agent.ReviewPlanGroup{
			{Pattern: "**/*.go", Label: "**/*.go", Files: []string{"a.go"}},
			{Pattern: "**/*.ts", Label: "**/*.ts", Files: []string{"c.ts"}},
		},
		timedOut: 1,
		texts:    []string{"the go findings"},
	}
	agents.registry.Register(starter)

	cs, em := newOutcomeChat(t, "review-timeout-outcome", starter.name)
	f := NewFactory()
	hold := cs.Hold("msg-1")
	ctx := chatsession.WithWork(context.Background(), hold)
	out, err := f.Handle(ctx, command.RuntimeServices{}, nil, cs, command.SlashInput{
		ChatID:    cs.ChatID,
		MessageID: "msg-1",
		Text:      "/review --timeout 5m",
		Args:      []string{"review", "--timeout", "5m"},
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if out == nil {
		t.Fatal("Handle returned nil SlashOutput")
	}
	hold.Release()
	got := waitOutcome(t, em, 5*time.Second)
	assertOutcome(t, got, http.StatusInternalServerError, map[string]string{
		"groups":           "2",
		"unfinished":       "1",
		"timed_out_groups": "1",
		"agent":            starter.name,
	})

	var notice string
	for _, msg := range em.messages() {
		if msg.Kind == messages.OutReply && strings.Contains(msg.Text, "⏱️") {
			notice = msg.Text
		}
	}
	if notice == "" {
		t.Fatalf("the chat was not told about the timeout: %+v", em.messages())
	}
	for _, want := range []string{"1 of 2 subagents timed out", "→ /review -c"} {
		if !strings.Contains(notice, want) {
			t.Errorf("the notice does not carry %q: %s", want, notice)
		}
	}
}

// TestHandle_ContinueResumesTheTimedOutGroup: a group that timed out
// keeps its recovery attempt, so the continuation picks it up again.
func TestHandle_ContinueResumesTheTimedOutGroup(t *testing.T) {
	agents := newTestAgents(t)
	recorded := &reviewSessionStarter{name: "review-timeout-recorded-agent", reply: "resumed findings"}
	chatRunner := &reviewSessionStarter{name: "review-timeout-chat-agent", reply: "must not run"}
	agents.registry.Register(recorded)
	agents.registry.Register(chatRunner)

	em := newRecordingEmitter()
	const chatID = "review-timeout-continue-chat"
	cs, _ := newContinueChatSession(t, chatID, chatRunner.name, t.TempDir(), em)
	enableThink(t, cs)

	f := NewFactory()
	f.record(chatID, recorded.name, "/recorded/ws", reviewRun{outcomes: []reviewOutcome{
		{
			pattern:   "**/*.ts",
			files:     []string{"c.ts"},
			sessionID: "sess-ts",
			err: &agent.GroupTimeoutError{
				Group:   "**/*.ts",
				Timeout: 15 * time.Minute,
			},
		},
	}})

	out := dispatchContinue(t, f, cs, chatID, "review", "-c", "--timeout", "5m")
	if out.Reply != "" {
		t.Fatalf("unexpected reply: %q", out.Reply)
	}

	em.waitForOutThinking(t, "resumed findings")

	cfgs := recorded.startedConfigs()
	if len(cfgs) != 1 {
		t.Fatalf("resumed %d sessions, want 1 (the timed-out group)", len(cfgs))
	}
	if cfgs[0].SessionID != "sess-ts" {
		t.Errorf("resumed session id = %q, want %q", cfgs[0].SessionID, "sess-ts")
	}
}

// TestHandle_TimeoutFlagWithTheGlobalScope: the threshold may travel
// with the flags that persist the runner — the review still runs.
func TestHandle_TimeoutFlagWithTheGlobalScope(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	t.Setenv("NIGHTME_CONFIG", path)

	starter := newTestAgents(t).register("review-timeout-global-agent")
	cs := newReviewChatSession(t, starter.name, dir)

	out := dispatchReview(t, cs, "review", "-a", starter.name, "-g", "--timeout", "5m")
	if !strings.Contains(out.Reply, "✅ review.agent = "+starter.name+" (global)") {
		t.Fatalf("the reply does not confirm the write: %q", out.Reply)
	}
	if strings.Contains(out.Reply, "❌") {
		t.Fatalf("the flag combination was rejected: %q", out.Reply)
	}
}
