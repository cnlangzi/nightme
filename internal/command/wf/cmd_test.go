package wf

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
	"github.com/cnlangzi/nightme/internal/messages"
)

// captureEmitter records the outbound messages of a chat.
type captureEmitter struct {
	mu   sync.Mutex
	msgs []messages.OutboundMessage
}

// Send implements messages.Emitter.
func (e *captureEmitter) Send(_ context.Context, msg messages.OutboundMessage) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.msgs = append(e.msgs, msg)
	return nil
}

// tasks returns the checklists the emitter was given.
func (e *captureEmitter) tasks() []messages.OutboundMessage {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []messages.OutboundMessage
	for _, msg := range e.msgs {
		if msg.Kind == messages.OutTaskCreate || msg.Kind == messages.OutTaskUpdate {
			out = append(out, msg)
		}
	}
	return out
}

// settled returns the outcome of the settled message.
func (e *captureEmitter) settled() *messages.Outcome {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, msg := range e.msgs {
		if msg.Kind == messages.OutMessageSettled {
			return msg.Outcome
		}
	}
	return nil
}

// newChat builds a chat session with a capturing emitter.
func newChat(t *testing.T) (*chatsession.ChatSession, *captureEmitter) {
	t.Helper()
	cs, err := chatsession.New("chat-1", "claude")
	if err != nil {
		t.Fatalf("chatsession.New: %v", err)
	}
	em := &captureEmitter{}
	cs.WithEmitter(em)
	return cs, em
}

// TestHandle_InstallsTheWorkflowAndCarriesTheReason — the command
// installs the workflow, the checklist carries the progress, and the
// settled message carries the reason.
func TestHandle_InstallsTheWorkflowAndCarriesTheReason(t *testing.T) {
	dir := t.TempDir()
	tier, srv := servingTier("github", validWorkflow)
	defer srv.Close()

	cs, em := newChat(t)
	f := testFactory(dir, []Source{tier}, nil)
	work := cs.Hold("msg-1")
	ctx := chatsession.WithWork(context.Background(), work)

	out, err := f.Handle(ctx, command.RuntimeServices{}, nil, cs, command.SlashInput{
		ChatID:    "chat-1",
		MessageID: "msg-1",
		Args:      []string{"wf", "install", "nightme/coder"},
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if out.Reply != "" {
		t.Errorf("the reply is %q, want none", out.Reply)
	}
	if !out.Consumed {
		t.Error("the command did not consume the message")
	}
	if _, err := os.Stat(filepath.Join(dir, "nightme", "coder.yml")); err != nil {
		t.Fatalf("the workflow was not written: %v", err)
	}

	work.Release()
	outcome := em.settled()
	if outcome == nil {
		t.Fatal("the message settled without an outcome")
	}
	if outcome.Data["reason"] != ReasonInstalled {
		t.Errorf("the reason is %q, want %q", outcome.Data["reason"], ReasonInstalled)
	}
	if outcome.Status != messages.OutcomeOK {
		t.Errorf("the status is %q, want %q", outcome.Status, messages.OutcomeOK)
	}

	tasks := em.tasks()
	if len(tasks) == 0 {
		t.Fatal("the checklist was never published")
	}
	first := tasks[0].TaskList.Items
	if len(first) != 3 {
		t.Fatalf("the checklist holds %d rows, want three", len(first))
	}
	if first[0].Subject != "download nightme/coder" {
		t.Errorf("the first row says %q", first[0].Subject)
	}
}

// TestHandle_AnAddressThatDoesNotFitIsRefused — the address is checked
// before anything is fetched, and nothing is written.
func TestHandle_AnAddressThatDoesNotFitIsRefused(t *testing.T) {
	dir := t.TempDir()
	gets := 0
	srv := newCountingServer(&gets, validWorkflow)
	defer srv.Close()

	cs, em := newChat(t)
	f := testFactory(dir, []Source{{Name: "github", BaseURL: srv.URL + "/"}}, nil)
	work := cs.Hold("msg-2")
	ctx := chatsession.WithWork(context.Background(), work)

	for _, address := range []string{"../etc/passwd", "Nightme/Coder"} {
		out, err := f.Handle(ctx, command.RuntimeServices{}, nil, cs, command.SlashInput{
			ChatID:    "chat-1",
			MessageID: "msg-2",
			Args:      []string{"wf", "install", address},
		})
		if err != nil {
			t.Fatalf("Handle: %v", err)
		}
		if out.Reply != "" {
			t.Errorf("the reply is %q, want none", out.Reply)
		}
	}
	if gets != 0 {
		t.Errorf("the hub was asked %d times, want none", gets)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("the workflows dir holds %d files, want none", len(entries))
	}

	work.Release()
	outcome := em.settled()
	if outcome == nil {
		t.Fatal("the message settled without an outcome")
	}
	if outcome.Data["reason"] != ReasonInvalidAddress {
		t.Errorf("the reason is %q, want %q", outcome.Data["reason"], ReasonInvalidAddress)
	}
}

// TestHandle_TheCommandLineIsChecked — the shared lexer refuses a
// command line that does not fit.
func TestHandle_TheCommandLineIsChecked(t *testing.T) {
	dir := t.TempDir()
	cs, _ := newChat(t)
	f := testFactory(dir, nil, nil)
	ctx := context.Background()

	for _, args := range [][]string{
		{"wf", "install"},
		{"wf", "install", "a", "b"},
		{"wf", "install", "--force"},
	} {
		out, err := f.Handle(ctx, command.RuntimeServices{}, nil, cs, command.SlashInput{
			ChatID: "chat-1",
			Args:   args,
		})
		if err != nil {
			t.Fatalf("Handle: %v", err)
		}
		if !out.Consumed {
			t.Errorf("the command did not consume %v", args)
		}
		if out.Reply == "" {
			t.Errorf("the command line %v was accepted without a word", args)
		}
	}
}

// newCountingServer returns a server that counts the GETs it got.
func newCountingServer(gets *int, body string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodHead, http.MethodOptions:
			w.WriteHeader(http.StatusOK)
		default:
			*gets++
			_, _ = w.Write([]byte(body))
		}
	}))
}

// TestTheCommandIsRegistered — the package registers itself the way
// every other command package does.
func TestTheCommandIsRegistered(t *testing.T) {
	command.SetDeps(command.Deps{WorkflowsDir: t.TempDir()})
	found := false
	for _, spec := range command.Default().Specs() {
		if spec.Name == "wf" {
			found = true
			if len(spec.Subcommands) != 1 || spec.Subcommands[0].Name != "install" {
				t.Errorf("the subcommands are %v, want install", spec.Subcommands)
			}
		}
	}
	if !found {
		t.Errorf("the command is not registered: %v", command.Default().Specs())
	}
}
