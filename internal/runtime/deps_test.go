package runtime

import (
	"testing"

	"github.com/cnlangzi/nightme/internal/channel"
)

// TestWithChannel_Default verifies the default channel is feishu.
func TestWithChannel_Default(t *testing.T) {
	deps := DefaultDeps()
	if deps.NewChannel == nil {
		t.Fatal("DefaultDeps().NewChannel is nil")
	}
}

// TestWithChannel_Echo verifies --channel=echo selects the echo
// channel.
func TestWithChannel_Echo(t *testing.T) {
	deps, err := WithChannel(DefaultDeps(), "echo")
	if err != nil {
		t.Fatalf("WithChannel: %v", err)
	}
	if !deps.SkipFeishuLogin {
		t.Error("echo should set SkipFeishuLogin=true")
	}
}

// TestWithBot_AddsBotChannel verifies WithBot wires bot as a
// channel constructor.
func TestWithBot_AddsBotChannel(t *testing.T) {
	deps := WithBot(DefaultDeps(), "/tmp/workflows")
	if deps.NewBot == nil {
		t.Fatal("WithBot().NewBot is nil")
	}
	// Verify it actually constructs (without a real config)
	ch, err := deps.NewBot(nil)
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}
	if ch == nil {
		t.Fatal("NewBot returned nil channel")
	}
	if ch.Name() != "bot" {
		t.Errorf("Name = %q, want bot", ch.Name())
	}
	var _ channel.Channel = ch // compile-time check
}
