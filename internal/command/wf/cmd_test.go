package wf

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/pkg/outcome"
)

// captureEmitter records the outbound messages of a chat.
type captureEmitter struct {
	mu   sync.Mutex
	msgs []messages.OutboundMessage
}

// Send implements messages.Emitter.
func (e *captureEmitter) Send(_ context.Context, msg messages.OutboundMessage) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.msgs = append(e.msgs, msg)
	return "", nil
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
func (e *captureEmitter) settled() *outcome.Outcome {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, msg := range e.msgs {
		if msg.Kind == messages.OutMessageSettled {
			return msg.Outcome
		}
	}
	return nil
}

// lastSettled returns the outcome of the last settled message.
func (e *captureEmitter) lastSettled() *outcome.Outcome {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out *outcome.Outcome
	for _, msg := range e.msgs {
		if msg.Kind == messages.OutMessageSettled {
			out = msg.Outcome
		}
	}
	return out
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

// TestTheUsageNamesTheShortName — the usage lines tell the operator
// that a name without an owner is another way of writing the address.
func TestTheUsageNamesTheShortName(t *testing.T) {
	f := testFactory(t.TempDir(), nil, nil)
	spec := f.Spec()
	for _, sub := range spec.Subcommands {
		if !strings.Contains(sub.Usage, "| <name>") {
			t.Errorf("the usage of %s is %q, want it to name the short form", sub.Name, sub.Usage)
		}
	}
	if !strings.Contains(spec.Usage, "| <name>") {
		t.Errorf("the usage of the command is %q, want it to name the short form", spec.Usage)
	}
	if !strings.Contains(spec.Usage, "\n") {
		t.Errorf("the usage of the command is %q, want a line per subcommand", spec.Usage)
	}
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
	if _, err := os.Stat(filepath.Join(dir, "nightme", "coder", "workflow.yml")); err != nil {
		t.Fatalf("the workflow was not written: %v", err)
	}

	work.Release()
	outcome := em.settled()
	if outcome == nil {
		t.Fatal("the message settled without an outcome")
	}
	if !strings.Contains(outcome.StatusText, "is installed") {
		t.Errorf("the words are %q, want them to name the case", outcome.StatusText)
	}
	if outcome.Status != http.StatusOK {
		t.Errorf("the status is %d, want %d", outcome.Status, http.StatusOK)
	}
	if outcome.Data["path"] != filepath.Join(dir, "nightme", "coder", "workflow.yml") {
		t.Errorf("the path is %q, want the absolute path of the file", outcome.Data["path"])
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
}

// TestHandle_TheCommandLineIsChecked — the shared lexer refuses a
// command line that does not fit.
func TestHandle_TheCommandLineIsChecked(t *testing.T) {
	dir := t.TempDir()
	cs, _ := newChat(t)
	f := testFactory(dir, nil, nil)
	ctx := context.Background()

	for _, args := range [][]string{
		{"wf"},
		{"wf", "bogus"},
		{"wf", "install"},
		{"wf", "install", "a", "b"},
		{"wf", "install", "--bogus", "a/b"},
		{"wf", "install", "--force"},
		{"wf", "uninstall"},
		{"wf", "uninstall", "-f", "a/b"},
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
			continue
		}
		if !strings.HasPrefix(out.Reply, "❌") {
			t.Errorf("the command line %v was answered with %q", args, out.Reply)
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
			names := make([]string, 0, len(spec.Subcommands))
			for _, sub := range spec.Subcommands {
				names = append(names, sub.Name)
			}
			if len(names) != 2 || names[0] != "install" || names[1] != "uninstall" {
				t.Errorf("the subcommands are %v, want install and uninstall", names)
			}
		}
	}
	if !found {
		t.Errorf("the command is not registered: %v", command.Default().Specs())
	}
}

// TestHandle_TheReservedOwnersAreRefused — an owner that names a
// reserved directory would write a workflow the loader never sees.
func TestHandle_TheReservedOwnersAreRefused(t *testing.T) {
	dir := t.TempDir()
	gets := 0
	srv := newCountingServer(&gets, validWorkflow)
	defer srv.Close()

	cs, em := newChat(t)
	f := testFactory(dir, []Source{{Name: "github", BaseURL: srv.URL + "/"}}, nil)
	work := cs.Hold("msg-3")
	ctx := chatsession.WithWork(context.Background(), work)

	for _, address := range []string{"actions/foo", "runs/foo"} {
		out, err := f.Handle(ctx, command.RuntimeServices{}, nil, cs, command.SlashInput{
			ChatID:    "chat-1",
			MessageID: "msg-3",
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
}

// TestHandle_TheChecklistIsAnchoredToTheMessage — every snapshot
// carries the message the operator sent, so the channel updates one
// card instead of posting one per snapshot.
func TestHandle_TheChecklistIsAnchoredToTheMessage(t *testing.T) {
	dir := t.TempDir()
	tier, srv := servingTier("github", validWorkflow)
	defer srv.Close()

	cs, em := newChat(t)
	f := testFactory(dir, []Source{tier}, nil)
	ctx := context.Background()

	if _, err := f.Handle(ctx, command.RuntimeServices{}, nil, cs, command.SlashInput{
		ChatID:    "chat-1",
		MessageID: "msg-4",
		Args:      []string{"wf", "install", "nightme/coder"},
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	tasks := em.tasks()
	if len(tasks) == 0 {
		t.Fatal("the checklist was never published")
	}
	for _, msg := range tasks {
		if msg.ReplyTo != "msg-4" {
			t.Errorf("a snapshot is anchored to %q, want msg-4", msg.ReplyTo)
		}
	}
}

// TestHandle_AnUnchangedInstallIsANoop — the request was valid and
// there was nothing to write.
func TestHandle_AnUnchangedInstallIsANoop(t *testing.T) {
	dir := t.TempDir()
	tier, srv := servingTier("github", validWorkflow)
	defer srv.Close()

	// The workflow is already installed and current.
	path := filepath.Join(dir, "nightme", "coder", "workflow.yml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(validWorkflow), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	cs, em := newChat(t)
	f := testFactory(dir, []Source{tier}, nil)
	work := cs.Hold("msg-5")
	ctx := chatsession.WithWork(context.Background(), work)

	if _, err := f.Handle(ctx, command.RuntimeServices{}, nil, cs, command.SlashInput{
		ChatID:    "chat-1",
		MessageID: "msg-5",
		Args:      []string{"wf", "install", "-f", "nightme/coder"},
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	work.Release()
	outcome := em.settled()
	if outcome == nil {
		t.Fatal("the message settled without an outcome")
	}
	if outcome.Status != http.StatusOK {
		t.Errorf("the status is %d, want %d", outcome.Status, http.StatusOK)
	}
	if !strings.Contains(outcome.StatusText, "is already current") {
		t.Errorf("the words are %q, want them to name the case", outcome.StatusText)
	}
}

// TestHandle_AnInstalledWorkflowIsSkippedUnlessForced — the workflow
// that is already there is left alone, and only a forced install goes
// to the hub and writes over it.
func TestHandle_AnInstalledWorkflowIsSkippedUnlessForced(t *testing.T) {
	dir := t.TempDir()
	tier, srv := servingTier("github", validWorkflow)
	defer srv.Close()

	path := filepath.Join(dir, "nightme", "coder", "workflow.yml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	local := "title: local\njobs:\n  main:\n    steps: [{message: hi}]\n"
	if err := os.WriteFile(path, []byte(local), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	cs, em := newChat(t)
	f := testFactory(dir, []Source{tier}, nil)

	work := cs.Hold("msg-6")
	ctx := chatsession.WithWork(context.Background(), work)
	if _, err := f.Handle(ctx, command.RuntimeServices{}, nil, cs, command.SlashInput{
		ChatID:    "chat-1",
		MessageID: "msg-6",
		Args:      []string{"wf", "install", "nightme/coder"},
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	work.Release()

	outcome := em.settled()
	if outcome == nil {
		t.Fatal("the message settled without an outcome")
	}
	if !strings.Contains(outcome.StatusText, "is already installed") {
		t.Errorf("the words are %q, want them to name the case", outcome.StatusText)
	}
	if outcome.Status != http.StatusOK {
		t.Errorf("the status is %d, want %d", outcome.Status, http.StatusOK)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(content) != local {
		t.Errorf("the file holds %q, want the local one", content)
	}

	work = cs.Hold("msg-7")
	ctx = chatsession.WithWork(context.Background(), work)
	if _, err := f.Handle(ctx, command.RuntimeServices{}, nil, cs, command.SlashInput{
		ChatID:    "chat-1",
		MessageID: "msg-7",
		Args:      []string{"wf", "install", "-f", "nightme/coder"},
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	work.Release()

	outcome = em.lastSettled()
	if outcome == nil {
		t.Fatal("the message settled without an outcome")
	}
	if !strings.Contains(outcome.StatusText, "is updated") {
		t.Errorf("the words are %q, want them to name the case", outcome.StatusText)
	}
	content, err = os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(content) != validWorkflow {
		t.Errorf("the file holds %q, want the hub's", content)
	}
}

// TestHandle_UninstallRemovesTheWorkflow — the uninstall takes the
// workflow away, and a second one finds nothing to remove.
func TestHandle_UninstallRemovesTheWorkflow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nightme", "coder", "workflow.yml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(validWorkflow), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	cs, em := newChat(t)
	f := testFactory(dir, nil, nil)

	work := cs.Hold("msg-8")
	ctx := chatsession.WithWork(context.Background(), work)
	if _, err := f.Handle(ctx, command.RuntimeServices{}, nil, cs, command.SlashInput{
		ChatID:    "chat-1",
		MessageID: "msg-8",
		Args:      []string{"wf", "uninstall", "nightme/coder"},
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	work.Release()

	outcome := em.settled()
	if outcome == nil {
		t.Fatal("the message settled without an outcome")
	}
	if !strings.Contains(outcome.StatusText, "is uninstalled") {
		t.Errorf("the words are %q, want them to name the case", outcome.StatusText)
	}
	if outcome.Status != http.StatusOK {
		t.Errorf("the status is %d, want %d", outcome.Status, http.StatusOK)
	}
	if outcome.Data["path"] != filepath.Join(dir, "nightme", "coder", "workflow.yml") {
		t.Errorf("the path is %q, want the absolute path of the file", outcome.Data["path"])
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the workflow is still there: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Errorf("the bundle is still there: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(filepath.Dir(path))); !os.IsNotExist(err) {
		t.Errorf("the owner directory is still there: %v", err)
	}

	tasks := em.tasks()
	if len(tasks) == 0 {
		t.Fatal("the checklist was never published")
	}
	items := tasks[0].TaskList.Items
	if len(items) != 1 {
		t.Fatalf("the checklist holds %d rows, want one", len(items))
	}
	if items[0].ID != "remove" {
		t.Errorf("the row is %q, want remove", items[0].ID)
	}
	if items[0].Subject != "remove nightme/coder" {
		t.Errorf("the row says %q", items[0].Subject)
	}

	work = cs.Hold("msg-9")
	ctx = chatsession.WithWork(context.Background(), work)
	if _, err := f.Handle(ctx, command.RuntimeServices{}, nil, cs, command.SlashInput{
		ChatID:    "chat-1",
		MessageID: "msg-9",
		Args:      []string{"wf", "uninstall", "nightme/coder"},
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	work.Release()

	outcome = em.lastSettled()
	if outcome == nil {
		t.Fatal("the message settled without an outcome")
	}
	if !strings.Contains(outcome.StatusText, "is not installed") {
		t.Errorf("the words are %q, want them to name the case", outcome.StatusText)
	}
	if outcome.Status != http.StatusOK {
		t.Errorf("the status is %d, want %d", outcome.Status, http.StatusOK)
	}
}

// TestHandle_UninstallRefusesAnAddressThatDoesNotFit — the address is
// checked before anything is removed.
func TestHandle_UninstallRefusesAnAddressThatDoesNotFit(t *testing.T) {
	dir := t.TempDir()
	cs, em := newChat(t)
	f := testFactory(dir, nil, nil)
	work := cs.Hold("msg-10")
	ctx := chatsession.WithWork(context.Background(), work)

	out, err := f.Handle(ctx, command.RuntimeServices{}, nil, cs, command.SlashInput{
		ChatID:    "chat-1",
		MessageID: "msg-10",
		Args:      []string{"wf", "uninstall", "actions/foo"},
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if out.Reply != "" {
		t.Errorf("the reply is %q, want none", out.Reply)
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
}

// TestHandle_ANameWithoutAnOwnerInstallsTheSameWorkflow — "coder"
// installs the workflow the full address installs, under the same
// path.
func TestHandle_ANameWithoutAnOwnerInstallsTheSameWorkflow(t *testing.T) {
	dir := t.TempDir()
	tier, srv := servingTier("github", validWorkflow)
	defer srv.Close()

	cs, em := newChat(t)
	f := testFactory(dir, []Source{tier}, nil)
	work := cs.Hold("msg-11")
	ctx := chatsession.WithWork(context.Background(), work)

	if _, err := f.Handle(ctx, command.RuntimeServices{}, nil, cs, command.SlashInput{
		ChatID:    "chat-1",
		MessageID: "msg-11",
		Args:      []string{"wf", "install", "coder"},
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	path := filepath.Join(dir, "nightme", "coder", "workflow.yml")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the workflow was not written: %v", err)
	}

	work.Release()
	outcome := em.settled()
	if outcome == nil {
		t.Fatal("the message settled without an outcome")
	}
	if !strings.Contains(outcome.StatusText, "is installed") {
		t.Errorf("the words are %q, want them to name the case", outcome.StatusText)
	}
	if outcome.Data["path"] != path {
		t.Errorf("the path is %q, want %q", outcome.Data["path"], path)
	}

	tasks := em.tasks()
	if len(tasks) == 0 {
		t.Fatal("the checklist was never published")
	}
	if subject := tasks[0].TaskList.Items[0].Subject; subject != "download nightme/coder" {
		t.Errorf("the first row says %q, want the full address", subject)
	}
}

// TestHandle_ANameWithoutAnOwnerAsksTheHubForTheSameFile — the short
// name fetches the file of the full address.
func TestHandle_ANameWithoutAnOwnerAsksTheHubForTheSameFile(t *testing.T) {
	dir := t.TempDir()
	var mu sync.Mutex
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodHead, http.MethodOptions:
			w.WriteHeader(http.StatusOK)
		default:
			mu.Lock()
			asked = append(asked, r.URL.Path)
			mu.Unlock()
			_, _ = w.Write([]byte(validWorkflow))
		}
	}))
	defer srv.Close()

	cs, _ := newChat(t)
	f := testFactory(dir, []Source{{Name: "github", BaseURL: srv.URL + "/"}}, nil)
	for _, address := range []string{"nightme/coder", "coder"} {
		work := cs.Hold("msg-12")
		ctx := chatsession.WithWork(context.Background(), work)
		if _, err := f.Handle(ctx, command.RuntimeServices{}, nil, cs, command.SlashInput{
			ChatID:    "chat-1",
			MessageID: "msg-12",
			Args:      []string{"wf", "install", "-f", address},
		}); err != nil {
			t.Fatalf("Handle: %v", err)
		}
		work.Release()
	}

	mu.Lock()
	defer mu.Unlock()
	if len(asked) != 2 {
		t.Fatalf("the hub was asked for %d files, want two", len(asked))
	}
	if asked[0] != asked[1] {
		t.Errorf("the hub was asked for %q and %q, want the same file", asked[0], asked[1])
	}
}

// TestHandle_ANameWithoutAnOwnerRemovesTheSameWorkflow — "coder"
// takes away the workflow the full address takes away.
func TestHandle_ANameWithoutAnOwnerRemovesTheSameWorkflow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nightme", "coder", "workflow.yml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(validWorkflow), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	cs, em := newChat(t)
	f := testFactory(dir, nil, nil)
	work := cs.Hold("msg-13")
	ctx := chatsession.WithWork(context.Background(), work)

	if _, err := f.Handle(ctx, command.RuntimeServices{}, nil, cs, command.SlashInput{
		ChatID:    "chat-1",
		MessageID: "msg-13",
		Args:      []string{"wf", "uninstall", "coder"},
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	work.Release()

	outcome := em.settled()
	if outcome == nil {
		t.Fatal("the message settled without an outcome")
	}
	if !strings.Contains(outcome.StatusText, "is uninstalled") {
		t.Errorf("the words are %q, want them to name the case", outcome.StatusText)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the workflow is still there: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(filepath.Dir(path))); !os.IsNotExist(err) {
		t.Errorf("the owner directory is still there: %v", err)
	}
}

// TestHandle_TheAddressThatDoesNotFitIsEchoedAsItWasTyped — an
// address that does not fit the hub's shape is refused, and the
// checklist and the outcome carry the address as it was typed.
func TestHandle_TheAddressThatDoesNotFitIsEchoedAsItWasTyped(t *testing.T) {
	dir := t.TempDir()
	gets := 0
	srv := newCountingServer(&gets, validWorkflow)
	defer srv.Close()

	cs, em := newChat(t)
	f := testFactory(dir, []Source{{Name: "github", BaseURL: srv.URL + "/"}}, nil)
	work := cs.Hold("msg-14")
	ctx := chatsession.WithWork(context.Background(), work)

	out, err := f.Handle(ctx, command.RuntimeServices{}, nil, cs, command.SlashInput{
		ChatID:    "chat-1",
		MessageID: "msg-14",
		Args:      []string{"wf", "install", "Coder"},
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if out.Reply != "" {
		t.Errorf("the reply is %q, want none", out.Reply)
	}
	if gets != 0 {
		t.Errorf("the hub was asked %d times, want none", gets)
	}

	work.Release()
	outcome := em.settled()
	if outcome == nil {
		t.Fatal("the message settled without an outcome")
	}
	if outcome.Status != http.StatusBadRequest {
		t.Errorf("the status is %d, want %d", outcome.Status, http.StatusBadRequest)
	}
	if outcome.Data["address"] != "Coder" {
		t.Errorf("the address is %q, want the one that was typed", outcome.Data["address"])
	}
	if !strings.Contains(outcome.StatusText, "kebab-case") {
		t.Errorf("the words are %q, want them to name the shape of the name", outcome.StatusText)
	}

	tasks := em.tasks()
	if len(tasks) == 0 {
		t.Fatal("the checklist was never published")
	}
	if subject := tasks[0].TaskList.Items[0].Subject; subject != "download Coder" {
		t.Errorf("the first row says %q, want the address that was typed", subject)
	}
}
