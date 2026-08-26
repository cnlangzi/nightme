package gtw

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
)

// TestFactory_Spec covers the Factory.Spec() contract: the
// returned Spec is what command.Registry uses to build the
// dispatch table. Name + Aliases are the keys; Summary + Usage
// surface in /help.
func TestFactory_Spec(t *testing.T) {
	f := NewFactory(NewManager())
	s := f.Spec()
	if s.Name != "gtw" {
		t.Errorf("expected Name=gtw, got %q", s.Name)
	}
	if !contains(s.Aliases, "team") {
		t.Errorf("expected alias 'team' in %v", s.Aliases)
	}
	if s.Summary == "" {
		t.Errorf("expected non-empty Summary, got empty")
	}
	if !strings.Contains(s.Usage, "fix") {
		t.Errorf("expected Usage to mention 'fix' subcommand, got %q", s.Usage)
	}
}

// TestFactory_Handle_NoArgs covers the "no subcommand given"
// path: the Factory returns a usage hint (Consumed=true) so
// the user sees feedback instead of falling through to the
// agent loop.
//
// F-51 argv convention: commander.Dispatch prefixes Args
// with the command name, so production callers see
// Args = ["gtw", ...]. Args[1] is the subcommand slot —
// when only "gtw" is present (Args[1] out of range), the
// Factory returns the usage hint.
func TestFactory_Handle_NoArgs(t *testing.T) {
	cs := &chatsession.ChatSession{}
	f := NewFactory(NewManager())
	got, err := f.Handle(context.Background(),
		command.RuntimeServices{},
		nil, cs,
		command.SlashInput{Text: "/gtw", Args: []string{"gtw"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.Consumed {
		t.Errorf("expected Consumed=true for empty /gtw, got %+v", got)
	}
	if !strings.Contains(got.Reply, "fix") {
		t.Errorf("expected Reply to mention 'fix' (usage hint), got %q", got.Reply)
	}
}

// TestFactory_Handle_List covers the /gtw list subcommand
// with no drafts. Should reply with "(none in this chat)".
// (list / reset subcommands removed — see wip/gtw.md step 37.
// Manager.ListDrafts / Manager.Reset / Manager.DraftCount are
// still used by cmd/nightme/debug.go for the CLI debug
// interface.)

// TestFactory_Handle_Fix_NoArgs covers /gtw fix without an
// issue id. Should reply with a usage hint.
//
// F-51 argv convention: commander.Dispatch prefixes Args
// with the command name, so production callers see
// Args = ["gtw", "fix", "<id>", ...]. The subcommand lives
// at Args[1] and the subcommand's args start at Args[2].
func TestFactory_Handle_Fix_NoArgs(t *testing.T) {
	cs := &chatsession.ChatSession{}
	f := NewFactory(NewManager())
	got, _ := f.Handle(context.Background(), command.RuntimeServices{},
		nil, cs,
	command.SlashInput{Text: "/gtw fix", Args: []string{"gtw", "fix"}})
	if !got.Consumed {
		t.Errorf("expected Consumed, got %+v", got)
	}
	if !strings.Contains(got.Reply, "fix") {
		t.Errorf("expected Reply to mention 'fix' (usage), got %q", got.Reply)
	}
}

// TestFactory_Handle_Fix_BadIssueID covers /gtw fix with a
// non-numeric id. Should reply with a hint.
func TestFactory_Handle_Fix_BadIssueID(t *testing.T) {
	cs := &chatsession.ChatSession{}
	f := NewFactory(NewManager())
	got, _ := f.Handle(context.Background(), command.RuntimeServices{},
		nil, cs,
	command.SlashInput{Text: "/gtw fix abc", Args: []string{"gtw", "fix", "abc"}})
	if !got.Consumed {
		t.Errorf("expected Consumed, got %+v", got)
	}
	if !strings.Contains(got.Reply, "abc") {
		t.Errorf("expected Reply to mention 'abc' in error, got %q", got.Reply)
	}
}

// TestFactory_Handle_UnknownSubcommand covers the
// "/gtw bogus" path. Commander passes Args[0]="gtw" (the
// command name); the factory must look at Args[1] for the
// subcommand so the unknown-subcommand reply quotes "bogus"
// (not "gtw").
func TestFactory_Handle_UnknownSubcommand(t *testing.T) {
	cs := &chatsession.ChatSession{}
	f := NewFactory(NewManager())
	got, _ := f.Handle(context.Background(), command.RuntimeServices{},
		nil, cs,
	command.SlashInput{Text: "/gtw bogus", Args: []string{"gtw", "bogus"}})
	if !got.Consumed {
		t.Errorf("expected Consumed, got %+v", got)
	}
	if !strings.Contains(got.Reply, "bogus") {
		t.Errorf("expected Reply to mention 'bogus', got %q", got.Reply)
	}
}

// contains is a tiny helper (Go 1.21+ has slices.Contains,
// but we want to be explicit and avoid the import).
func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
// (parseFixMode was the pre-F-XX helper that parsed mode + rawArg
// from a boolean-flag-stripped argv. F-XX rewrote parseFixArgs
// as a proper CLI lexer and deleted parseFixMode. The bare-id,
// --name/-n, missing-value, and empty-argv cases are all
// covered by the stricter TestParseFixArgs_* tests in
// parse_fix_args_test.go — see TestParseFixArgs_YesFlag's
// "default plan" / "yes with local" sub-cases, etc.)

// --- F-XX run-lock integration tests ---
//
// These tests verify the per-chat run lock is actually acquired
// at the top of Factory.Handle (the F-59 serialisation seam):
// two Handle calls for the SAME chatID must run strictly
// serially, but Handle for DIFFERENT chatIDs run concurrently.
//
// We use /gtw fix with no issue id (`Args=["gtw","fix"]`) as the
// payload because runFix returns immediately on len<3 — no deps
// required, no I/O, no panics. The lock holding is observable
// purely from goroutine scheduling.
//
// The pre-acquire pattern (lock taken in the test goroutine,
// then Handle called from a worker goroutine that must wait)
// avoids any flake from the subcommand running "too fast to
// observe". The lock IS the observable.

// TestFactory_Handle_SameChatSerializes verifies that two Handle
// calls for the same chatID are strictly serialised: the second
// worker blocks on the per-chat lock until the first releases.
//
// Approach:
//  1. Pre-acquire chat-A's lock from the test goroutine.
//  2. Spawn a worker calling Handle for chat-A — it must block
//     on the Lock inside Handle.
//  3. Assert the worker has NOT returned while we still hold
//     the lock.
//  4. Release the lock; the worker must now complete quickly.
//
// If the lock were accidentally removed (regression), step 3
// would fail because the worker would return immediately.
func TestFactory_Handle_SameChatSerializes(t *testing.T) {
	m := newTestManager()
	f := NewFactory(m)

	const chatID = "chat-A"
	mu := m.runLockFor(chatID)
	if mu == nil {
		t.Fatalf("runLockFor(%q) returned nil", chatID)
	}
	mu.Lock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = f.Handle(context.Background(),
			command.RuntimeServices{},
			nil,
			&chatsession.ChatSession{},
			command.SlashInput{
				Text:   "/gtw fix",
				Args:   []string{"gtw", "fix"},
				ChatID: chatID,
			})
	}()

	// Give the worker time to enter Handle and reach the Lock
	// acquisition. 50ms is generous for a no-op path; if the
	// worker is still running after this, it MUST be blocked
	// on the lock (it's the only blocking point).
	time.Sleep(50 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("Handle returned while external goroutine held chatID's lock; " +
			"the per-chat run lock is not being acquired at Handle entry")
	default:
	}

	releaseStart := time.Now()
	mu.Unlock()

	select {
	case <-done:
		elapsed := time.Since(releaseStart)
		// runFix returns immediately on len<3; Handle's only
		// remaining work is the deferred Unlock. Anything past
		// 200ms indicates scheduling pathology (CI under load),
		// not a code regression.
		if elapsed > 200*time.Millisecond {
			t.Errorf("Handle took %v after lock release; expected < 200ms", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Handle did not return within 2s after lock release; possible deadlock")
	}
}

// TestFactory_Handle_DifferentChatIndependent is the dual of
// SameChatSerializes: chat-A's lock must NOT block chat-B. A
// worker calling Handle for chat-B must complete even while the
// test goroutine still holds chat-A's lock.
//
// Regression target: anyone who accidentally widens the lock
// scope (e.g. Manager.mu.Lock instead of the per-chat mutex,
// or a single shared mutex) would fail this test.
func TestFactory_Handle_DifferentChatIndependent(t *testing.T) {
	m := newTestManager()
	f := NewFactory(m)

	const chatA = "chat-A"
	const chatB = "chat-B"

	// Pre-acquire chat-A's lock. chat-B's lock must remain free.
	muA := m.runLockFor(chatA)
	if muA == nil {
		t.Fatalf("runLockFor(%q) returned nil", chatA)
	}
	muA.Lock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = f.Handle(context.Background(),
			command.RuntimeServices{},
			nil,
			&chatsession.ChatSession{},
			command.SlashInput{
				Text:   "/gtw fix",
				Args:   []string{"gtw", "fix"},
				ChatID: chatB,
			})
	}()

	// chat-B must complete without us releasing chat-A's lock.
	// 200ms is generous for a no-op subcommand; any longer wait
	// means chat-B is being incorrectly blocked by chat-A.
	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("chat-B Handle did not complete within 200ms; chat-A's lock " +
			"leaked across chatIDs (locks are NOT per-chat)")
	}

	// Clean up: release chat-A's lock.
	muA.Unlock()
}

// TestFactory_Handle_UnknownSubcommandReleasesLock guards the
// default case of the subcommand switch: when Args[1] is not
// "fix" / "close" / etc., Handle falls through to the
// "Unknown subcommand: ..." reply. The defer Unlock must still
// fire for that path.
//
// Regression target: a future refactor that pulls the lock
// acquisition INTO each subcommand case (instead of covering
// the whole switch) would leave the unknown-subcommand path
// without lock coverage and this test would catch it.
func TestFactory_Handle_UnknownSubcommandReleasesLock(t *testing.T) {
	m := newTestManager()
	f := NewFactory(m)

	const chatID = "chat-A"
	mu := m.runLockFor(chatID)
	if mu == nil {
		t.Fatalf("runLockFor(%q) returned nil", chatID)
	}

	// First Handle call: unknown subcommand, must complete
	// (releasing the lock) without ever seeing mu held
	// externally.
	done := make(chan struct{})
	go func() {
		defer close(done)
		got, err := f.Handle(context.Background(),
			command.RuntimeServices{},
			nil,
			&chatsession.ChatSession{},
			command.SlashInput{
				Text:   "/gtw bogus",
				Args:   []string{"gtw", "bogus"},
				ChatID: chatID,
			})
		if err != nil {
			t.Errorf("Handle returned unexpected err: %v", err)
		}
		if !got.Consumed {
			t.Errorf("expected Consumed=true for unknown subcommand reply, got %+v", got)
		}
		if !strings.Contains(got.Reply, "bogus") {
			t.Errorf("expected Reply to mention 'bogus', got %q", got.Reply)
		}
	}()

	// Must complete quickly — no external lock held, no I/O.
	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("Handle did not complete within 200ms; lock likely leaked on the default case")
	}

	// Sanity: the lock is now unheld. A second worker calling
	// Handle must proceed without blocking.
	mu.Lock() // confirms prior Unlock happened
	mu.Unlock()
}
