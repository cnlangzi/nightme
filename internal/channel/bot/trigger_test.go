package bot

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/command/gtw"
	"github.com/cnlangzi/nightme/internal/wfe"
)

// fakeEventSource is a controllable EventSource for tests.
type fakeEventSource struct {
	events chan gtw.Event
}

func newFakeEventSource(bufSize int) *fakeEventSource {
	return &fakeEventSource{events: make(chan gtw.Event, bufSize)}
}

func (f *fakeEventSource) Subscribe(_ context.Context) (<-chan gtw.Event, error) {
	return f.events, nil
}

func (f *fakeEventSource) push(ev gtw.Event) {
	f.events <- ev
}

func TestTranslateEvent(t *testing.T) {
	tests := []struct {
		name string
		in   gtw.Event
		want wfe.Event
	}{
		{
			"PR opened",
			gtw.Event{
				Kind: "pull_request", Repo: "foo/bar", Action: "opened",
				PR: 42, Author: "alice", Time: time.Unix(0, 0),
			},
			wfe.Event{
				Kind: "pull_request", Time: time.Unix(0, 0),
				Data: map[string]any{
					"repo": "foo/bar", "action": "opened",
					"pr_number": 42, "author": "alice", "source": "pull_request",
				},
			},
		},
		{
			"branch push",
			gtw.Event{
				Kind: "branch", Repo: "foo/bar", Action: "pushed",
				Branch: "main", Author: "bob", Time: time.Unix(0, 0),
			},
			wfe.Event{
				Kind: "branch", Time: time.Unix(0, 0),
				Data: map[string]any{
					"repo": "foo/bar", "action": "pushed",
					"name": "main", "author": "bob", "source": "branch",
				},
			},
		},
		{
			"mention with command",
			gtw.Event{
				Kind: "mention", Repo: "foo/bar", Action: "commented",
				PR: 42, Author: "carol", CommentBody: "@owner review this",
				Command: "review", URL: "https://example.com/comment/1",
				Time: time.Unix(0, 0),
			},
			wfe.Event{
				Kind: "mention", Time: time.Unix(0, 0),
				Data: map[string]any{
					"repo": "foo/bar", "action": "commented",
					"pr_number": 42, "author": "carol",
					"text": "@owner review this", "command": "review",
					"url": "https://example.com/comment/1",
					"source": "mention",
				},
			},
		},
		{
			"issue opened",
			gtw.Event{
				Kind: "issue", Repo: "foo/bar", Action: "opened",
				Issue: 100, Author: "dave", Time: time.Unix(0, 0),
			},
			wfe.Event{
				Kind: "issue", Time: time.Unix(0, 0),
				Data: map[string]any{
					"repo": "foo/bar", "action": "opened",
					"issue_number": 100, "author": "dave", "source": "issue",
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := translateEvent(tt.in)
			if got.Kind != tt.want.Kind {
				t.Errorf("Kind = %q, want %q", got.Kind, tt.want.Kind)
			}
			if got.Time != tt.want.Time {
				t.Errorf("Time = %v, want %v", got.Time, tt.want.Time)
			}
			if len(got.Data) != len(tt.want.Data) {
				t.Errorf("Data len = %d, want %d (got: %v, want: %v)", len(got.Data), len(tt.want.Data), got.Data, tt.want.Data)
			}
			for k, want := range tt.want.Data {
				if got.Data[k] != want {
					t.Errorf("Data[%q] = %v, want %v", k, got.Data[k], want)
				}
			}
		})
	}
}

func TestExtractCommandLocal(t *testing.T) {
	// extractCommand lives in the gtw package (polling.go). We
	// re-define it here for the bot package's wire-up test scope
	// (the bot doesn't actually call extractCommand directly — it's
	// the poller's responsibility — but we mirror the logic for
	// sanity).
	localExtract := func(text, ownerLogin string) string {
		if ownerLogin == "" {
			return ""
		}
		lower := strings.ToLower(text)
		ownerLower := strings.ToLower(ownerLogin)
		idx := strings.Index(lower, "@"+ownerLower)
		if idx < 0 {
			return ""
		}
		rest := text[idx+1+len(ownerLogin):]
		if strings.HasPrefix(rest, "/") {
			slash := strings.Index(rest, " ")
			if slash < 0 {
				return ""
			}
			rest = rest[slash+1:]
		}
		// strip leading whitespace
		rest = strings.TrimLeft(rest, " \t")
		end := strings.IndexAny(rest, " \t\n")
		if end < 0 {
			end = len(rest)
		}
		return rest[:end]
	}
	tests := []struct {
		body, owner, want string
	}{
		{"@owner review this", "owner", "review"},
		{"@owner fix issue #42", "owner", "fix"},
		{"hello @owner review", "owner", "review"},
		{"@someone else", "owner", ""},
		{"@owner/bot review", "owner", "review"},
	}
	for _, tt := range tests {
		t.Run(tt.body, func(t *testing.T) {
			if got := localExtract(tt.body, tt.owner); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// TestTriggerManager_GitEventsFlow verifies that events pushed
// into a configured EventSource flow through to bot.onTrigger.
func TestTriggerManager_GitEventsFlow(t *testing.T) {
	wf, err := wfe.Parse([]byte(`
name: pr-reviewer
workspaces: [a]
on:
  pull_request:
    events: [opened]
jobs:
  main:
    steps: [{id: s, run: x}]
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	// Build a workspaceRepoMap directly (skip the git-remote lookup
	// since the test workspace isn't a real git repo).
	wsMap := &workspaceRepoMap{
		byRepo: map[string]string{"foo/bar": "a"},
		byPath: map[string]string{"a": "foo/bar"},
	}

	src := newFakeEventSource(4)
	var gotWf *wfe.Workflow
	var gotWorkspace string
	var gotEvent wfe.Event
	called := make(chan struct{}, 1)
	onTrigger := func(_ context.Context, w *wfe.Workflow, ev wfe.Event, workspace string) {
		gotWf = w
		gotEvent = ev
		gotWorkspace = workspace
		select {
		case called <- struct{}{}:
		default:
		}
	}

	tm := newTriggerManager([]*wfe.Workflow{wf}, wsMap, onTrigger,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	tm.setEventSource(src)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := tm.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer tm.Stop()

	// Give the consume goroutine a moment to subscribe.
	time.Sleep(50 * time.Millisecond)

	// Push a matching event.
	src.push(gtw.Event{
		Kind: "pull_request", Repo: "foo/bar", Action: "opened",
		Branch: "main", PR: 42, Author: "alice", Time: time.Now(),
	})

	select {
	case <-called:
		// got event
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for onTrigger")
	}

	if gotWf == nil || gotWf.Name != "pr-reviewer" {
		t.Errorf("workflow = %v, want pr-reviewer", gotWf)
	}
	if gotWorkspace != "a" {
		t.Errorf("workspace = %q, want 'a'", gotWorkspace)
	}
	if gotEvent.Kind != "pull_request" {
		t.Errorf("event kind = %q, want pull_request", gotEvent.Kind)
	}
}
