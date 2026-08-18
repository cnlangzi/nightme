package gateway

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
	commandServices "github.com/cnlangzi/nightme/internal/command/services"
	"github.com/cnlangzi/nightme/internal/gateway/inbound"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/internal/shell"
)

// stubDispatcher is a minimal CommandDispatcher stand-in that
// returns the canned Reply we want to observe routed through
// dispatchLoop → Emitter.Send.
type stubDispatcher struct {
	reply string
}

func (s *stubDispatcher) Dispatch(ctx context.Context, rt command.RuntimeServices, mgr *chatsession.Manager, cs *chatsession.ChatSession, input command.SlashInput) (*command.SlashOutput, bool, error) {
	return &command.SlashOutput{Consumed: true, Reply: s.reply}, true, nil
}

// Match implements command.Commander — always true so the
// slash branch always claims in this regression test (the
// test's whole point is to verify the reply path runs end-to-end).
func (s *stubDispatcher) Match(_ string) (string, bool) { return "", true }

// stubMessageHandler returns a real ChatSession so the dispatcher's
// tryCommandDispatch path can succeed end-to-end.
type stubMessageHandler struct{}

func (stubMessageHandler) HandleInbound(ctx context.Context, msg *messages.InboundMessage) error {
	return nil
}
func (stubMessageHandler) GetOrCreate(chatID, primaryAgent string) (*chatsession.ChatSession, error) {
	cs, err := chatsession.New(chatID, primaryAgent)
	if err != nil {
		return nil, err
	}
	cs.SetSelectedCwd("/tmp")
	cs.SetSelectedAgent(primaryAgent)
	return cs, nil
}

// stubShell always falls through (returns nil, false) so the
// slash-command path is exercised without shell handling.
// Mirrors the new shell.Dispatcher.Handle signature introduced
// in F-XX (Sender→Emitter refactor; cs parameter for ⏳→✅
// framework emissions).
type stubShell struct{}

func (stubShell) Handle(_ *chatsession.Manager, _ *chatsession.ChatSession, _ shell.InboundRequest) (*shell.ShellOutput, bool) {
	return nil, false
}

// stubAction never fires.
type stubAction struct{}

func (stubAction) Handle(ctx context.Context, chatID string, ev commandServices.ReactionEvent) bool {
	return false
}

// recordingEmitter captures every OutboundMessage handed to Send
// so the test can assert the slash reply made it (with the
// user's MessageID stamped as ReplyTo).
type recordingEmitter struct {
	mu  sync.Mutex
	out []messages.OutboundMessage
}

func (r *recordingEmitter) Send(ctx context.Context, msg messages.OutboundMessage) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.out = append(r.out, msg)
	return nil
}
func (r *recordingEmitter) Record() []messages.OutboundMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]messages.OutboundMessage(nil), r.out...)
}

// runDispatchLoop is the regression-equivalent of the
// production dispatchLoop: it reads from a channel, dispatches
// through the wired Router. We isolate it so the test does not
// need to drive the real channel pumps (which spawned
// goroutines that blocked indefinitely against the un-wired
// test channels).
//
// F-59: the reply is now emitted asynchronously from inside
// runCommand via the wired Emitter — the F-58 placeholder
// CommandResult always has an empty Reply, so this loop no
// longer forwards result.Reply. We still drive DispatchInbound
// here so the goroutine actually fires (the test polls the
// emitter for the async reply).
func runDispatchLoop(ctx context.Context, r *Router, in <-chan messages.InboundMessage) {
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-in:
			if !ok {
				return
			}
			if _, err := r.DispatchInbound(ctx, &msg); err != nil {
				continue
			}
		}
	}
}

// TestDispatchLoop_ForwardsSlashReply is the regression for the
// F-54 bug: the gateway's dispatchLoop used to drop
// CommandResult.Reply on the floor after the F-58 dispatcher
// rewrite. Every slash command (the /use / /new / /close / /gtw
// family) silently lost its bot-side reply — the user would see
// the green-check ✓ on the inbound message but nothing else.
func TestDispatchLoop_ForwardsSlashReply(t *testing.T) {
	em := &recordingEmitter{}
	want := "Now using pi (pid=12345, cwd=/tmp, source=spawn)"

	r := New(inbound.New(chatsession.NewManager(), &stubDispatcher{reply: want}, stubShell{}, stubAction{}, em, "claude"))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Mirror the dispatch loop's logic in a goroutine so we
	// don't need to drive the channel pumps. F-59: the reply is
	// emitted asynchronously from inside runCommand, so we poll
	// the emitter to detect it.
	in := make(chan messages.InboundMessage, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		runDispatchLoop(ctx, r, in)
	}()

	// Synthesize a slash command inbound.
	in <- messages.InboundMessage{
		ChatID:    "oc_test",
		MessageID: "om_test_1",
		UserID:    "u_test",
		Text:      "/use pi",
	}

	// Poll for the recording emitter to see the reply.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got := em.Record()
		for _, m := range got {
			if m.Text == want {
				if m.ReplyTo != "om_test_1" {
					t.Errorf("ReplyTo = %q, want om_test_1", m.ReplyTo)
				}
				if m.Kind != messages.OutReply {
					t.Errorf("Kind = %s, want OutReply", m.Kind)
				}
				cancel()
				wg.Wait()
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	wg.Wait()
	t.Fatalf("emitter never received the slash reply; got %d messages",
		len(em.Record()))
}
