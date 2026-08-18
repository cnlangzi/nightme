// registry_test.go — v1.3+ multi-channel: channel Registry is
// the OCP registration point. Adding a new channel = 1
// channel.<name>/init.go file that calls channel.Register.
// This test pins the registry's contract: BuildAll returns
// every channel whose builder doesn't error, in alphabetical
// order, and Available lists registered names.
package channel

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/config"
	"github.com/cnlangzi/nightme/internal/messages"
)

// stubChannel is a minimal Channel implementation used by the
// registry tests. The tests verify registry / iteration
// semantics, not channel behaviour; this stub satisfies the
// interface without depending on any real adapter (which would
// introduce an import cycle: real adapters import this package
// to register, so we can't import them here).
type stubChannel struct{ name string }

func (s *stubChannel) Name() string  { return s.name }
func (s *stubChannel) Start(_ context.Context) error { return nil }
func (s *stubChannel) Stop(_ context.Context) error  { return nil }
func (s *stubChannel) Incoming() <-chan messages.InboundMessage {
	ch := make(chan messages.InboundMessage)
	close(ch)
	return ch
}
func (s *stubChannel) Send(_ context.Context, _ messages.OutboundMessage) error {
	return nil
}
func (s *stubChannel) OnPromptEnded(_ context.Context, _, _ string) {}
func (s *stubChannel) HealthSnapshot() (string, json.RawMessage, error) {
	return s.name, json.RawMessage("{}"), nil
}
func (s *stubChannel) SetLogger(_ *slog.Logger) {}
func (s *stubChannel) BuildBlocks(_ string, _ []messages.Attachment) []agent.ContentBlock {
	return nil
}

func makeStubBuilder(name string, wantErr error) Builder {
	return func(*config.Config) (Channel, error) {
		if wantErr != nil {
			return nil, wantErr
		}
		return &stubChannel{name: name}, nil
	}
}

// snapshot/restore is used to give the test a clean registry.
// Other tests in the repo (e.g. feishu/init_test.go) may have
// already registered their own builders; we wipe + restore.
func snapshot() map[string]Builder {
	original := map[string]Builder{}
	for _, n := range Available() {
		original[n] = GetBuilder(n)
	}
	return original
}

func restore(s map[string]Builder) {
	mu.Lock()
	defer mu.Unlock()
	reg = map[string]Builder{}
	for n, b := range s {
		reg[n] = b
	}
}

// TestBuildAll_IteratesInAlphabeticalOrder verifies that
// BuildAll iterates channels in the same order as Available()
// (alphabetical) and skips builders that return an error.
func TestBuildAll_IteratesInAlphabeticalOrder(t *testing.T) {
	original := snapshot()
	defer restore(original)

	mu.Lock()
	reg = map[string]Builder{}
	mu.Unlock()

	Register("alpha", makeStubBuilder("alpha", nil))
	Register("beta", makeStubBuilder("beta", nil))
	Register("gamma", makeStubBuilder("gamma", errors.New("no creds")))

	// Available is alphabetical.
	got := Available()
	want := []string{"alpha", "beta", "gamma"}
	if len(got) != len(want) {
		t.Fatalf("Available: got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("Available[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	// BuildAll returns alpha + beta (gamma errors → skipped).
	chs, err := BuildAll(&config.Config{})
	if err != nil {
		t.Fatalf("BuildAll: %v", err)
	}
	if len(chs) != 2 {
		t.Fatalf("BuildAll returned %d channels, want 2 (alpha + beta; gamma error is skipped)", len(chs))
	}
}

// TestBuildAll_AllBuildersErrorReturnsAggregate verifies that
// when every registered builder fails (no usable credentials),
// BuildAll returns a clear error so the runtime can surface
// "run `nightme login <channel>` first".
func TestBuildAll_AllBuildersErrorReturnsAggregate(t *testing.T) {
	original := snapshot()
	defer restore(original)

	mu.Lock()
	reg = map[string]Builder{}
	mu.Unlock()

	Register("only-failing", makeStubBuilder("only-failing", errors.New("no creds")))

	_, err := BuildAll(&config.Config{})
	if err == nil {
		t.Fatal("BuildAll: want error (all builders fail), got nil")
	}
}

// TestAvailable_Alphabetical is a regression guard for the
// "Available" sort order. The runtime relies on a stable
// order when iterating multiple channels.
func TestAvailable_Alphabetical(t *testing.T) {
	original := snapshot()
	defer restore(original)

	mu.Lock()
	reg = map[string]Builder{}
	mu.Unlock()

	// Register out of alphabetical order; Available must sort.
	Register("zulu", makeStubBuilder("zulu", nil))
	Register("alpha", makeStubBuilder("alpha", nil))
	Register("mike", makeStubBuilder("mike", nil))

	got := Available()
	want := []string{"alpha", "mike", "zulu"}
	if len(got) != 3 {
		t.Fatalf("Available: got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("Available[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
