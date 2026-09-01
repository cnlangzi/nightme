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
type stubChannel struct {
	name string
}

func (s *stubChannel) Name() string { return s.name }
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
func snapshot() map[string]entry {
	original := map[string]entry{}
	mu.RLock()
	for n, e := range reg {
		original[n] = e
	}
	mu.RUnlock()
	return original
}

func restore(s map[string]entry) {
	mu.Lock()
	defer mu.Unlock()
	reg = map[string]entry{}
	for n, e := range s {
		reg[n] = e
	}
}

// TestBuildAll_IteratesInAlphabeticalOrder verifies that
// BuildAll iterates channels in the same order as Available()
// (alphabetical) and skips builders that return an error.
func TestBuildAll_IteratesInAlphabeticalOrder(t *testing.T) {
	original := snapshot()
	defer restore(original)

	mu.Lock()
	reg = map[string]entry{}
	mu.Unlock()

	Register("alpha", "a_", makeStubBuilder("alpha", nil))
	Register("beta", "b_", makeStubBuilder("beta", nil))
	Register("gamma", "g_", makeStubBuilder("gamma", errors.New("no creds")))

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
	reg = map[string]entry{}
	mu.Unlock()

	Register("only-failing", "o_", makeStubBuilder("only-failing", errors.New("no creds")))

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
	reg = map[string]entry{}
	mu.Unlock()

	// Register out of alphabetical order; Available must sort.
	Register("zulu", "z_", makeStubBuilder("zulu", nil))
	Register("alpha", "a_", makeStubBuilder("alpha", nil))
	Register("mike", "m_", makeStubBuilder("mike", nil))

	got := Available()
	want := []string{"alpha", "mike", "zulu"}
	if len(got) != len(want) {
		t.Fatalf("Available: got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("Available[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestChatIDPrefixes_SkipsEmpty verifies that channels declaring
// no prefix (e.g. the bot workflows engine) do NOT contribute an
// "" to ChatIDPrefixes — an empty prefix would falsely accept
// every key. Pins the safety contract chatstore.New relies on.
func TestChatIDPrefixes_SkipsEmpty(t *testing.T) {
	original := snapshot()
	defer restore(original)

	mu.Lock()
	reg = map[string]entry{}
	mu.Unlock()

	Register("feishu", "oc_", makeStubBuilder("feishu", nil))
	Register("telegram", "tg_", makeStubBuilder("telegram", nil))
	Register("bot", "", makeStubBuilder("bot", nil))

	got := ChatIDPrefixes()
	want := []string{"oc_", "tg_"}
	if len(got) != len(want) {
		t.Fatalf("ChatIDPrefixes: got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("ChatIDPrefixes[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestChatIDPrefix_Lookup verifies the single-channel prefix
// accessor used by tests / diagnostics.
func TestChatIDPrefix_Lookup(t *testing.T) {
	original := snapshot()
	defer restore(original)

	mu.Lock()
	reg = map[string]entry{}
	mu.Unlock()

	Register("slack", "sl_", makeStubBuilder("slack", nil))

	if got := ChatIDPrefix("slack"); got != "sl_" {
		t.Errorf("ChatIDPrefix(slack) = %q, want %q", got, "sl_")
	}
	if got := ChatIDPrefix("unknown"); got != "" {
		t.Errorf("ChatIDPrefix(unknown) = %q, want \"\"", got)
	}
	if got := ChatIDPrefix("bot"); got != "" {
		t.Errorf("ChatIDPrefix(bot) = %q, want \"\" (bot has no prefix)", got)
	}
}

// TestRegister_RejectsReservedCharsInPrefix verifies the prefix
// guard. '/' is reserved because the user-facing login / config
// flows treat it as a path separator. ':' is permitted — see
// the package doc — so this test does NOT exercise a ':' prefix.
func TestRegister_RejectsReservedCharsInPrefix(t *testing.T) {
	original := snapshot()
	defer restore(original)

	for _, bad := range []string{"o/c", "foo/bar"} {
		bad := bad
		t.Run(bad, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("Register with prefix %q should panic", bad)
				}
			}()
			Register("test", bad, makeStubBuilder("test", nil))
		})
	}
}

// TestRegister_AcceptsColonPrefix pins the policy that ':' is a
// legal prefix character. Bot relies on it ("bot:") and so does
// telegram's "<prefix><chat>:<thread>" shape, although the colon
// there is between chatid and thread (not inside the prefix).
func TestRegister_AcceptsColonPrefix(t *testing.T) {
	original := snapshot()
	defer restore(original)

	defer func() {
		if r := recover(); r != nil {
			t.Errorf("Register with ':' prefix should not panic, got %v", r)
		}
	}()
	Register("test-colon", "bot:", makeStubBuilder("test-colon", nil))
	if got := ChatIDPrefix("test-colon"); got != "bot:" {
		t.Errorf("ChatIDPrefix(test-colon) = %q, want %q", got, "bot:")
	}
}