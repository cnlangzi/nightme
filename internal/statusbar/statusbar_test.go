package statusbar

import (
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/agentsession"
	"github.com/cnlangzi/nightme/internal/messages"
)

// TestNewRuntimeSource_NilLookup verifies that a nil lookup
// function is handled defensively — production always provides
// one, but tests or future misconfigurations shouldn't panic.
// Returning nil matches the documented contract.
func TestNewRuntimeSource_NilLookup(t *testing.T) {
	source := NewRuntimeSource(nil, Deps{})
	if got := source("any-chat"); got != nil {
		t.Errorf("nil lookup should produce nil StatusBar, got %+v", got)
	}
}

// TestNewRuntimeSource_UnknownChatID verifies the lookup-driven
// path: a chatID the runtime doesn't know about yields zero
// ChatInfo, which the Source treats as "skip" rather than
// panicking.
func TestNewRuntimeSource_UnknownChatID(t *testing.T) {
	// Stub lookup: unknown chat → zero ChatInfo.
	lookup := func(chatID string) ChatInfo { return ChatInfo{} }
	source := NewRuntimeSource(lookup, Deps{})
	if got := source("never-created"); got != nil {
		t.Errorf("unknown chatID should produce nil StatusBar, got %+v", got)
	}
}

// TestNewRuntimeSource_NoActiveAS_StillProducesGitBar covers
// the GitBar-always-present rule: a chat with no selected AS
// but with a SelectedCwd still gets a StatusBar with GitBar
// populated (the user should always see what worktree they're
// on, even before any agent spawns).
//
// Uses a stub ChatLookupFunc instead of *chatsession.Manager so
// the test stays in package statusbar's dependency footprint
// (chatsession → outbound → statusbar would otherwise cycle).
func TestNewRuntimeSource_NoActiveAS_StillProducesGitBar(t *testing.T) {
	tmp := t.TempDir()
	stub := func(chatID string) ChatInfo {
		if chatID == "oc_no_as" {
			return ChatInfo{Cwd: tmp} // no AS, but has workspace
		}
		return ChatInfo{}
	}
	source := NewRuntimeSource(stub, Deps{})
	got := source("oc_no_as")
	if got == nil {
		t.Fatal("chat with workspace should produce StatusBar (GitBar fallback), got nil")
	}
	if got.GitBar == nil {
		t.Error("GitBar should be populated even when no AS is selected")
	}
	if got.GitBar.Workspace != tmp {
		t.Errorf("GitBar.Workspace = %q, want %q", got.GitBar.Workspace, tmp)
	}
	// Non-git workspace → GitStatus is nil (no CollectGit
	// registered in Deps); the renderer drops Line 3 in that
	// case. The StatusBar itself is still populated, which is
	// the contract this test pins.
	if got.GitBar.GitStatus != nil {
		t.Errorf("missing CollectGit should produce GitStatus=nil, got %+v", got.GitBar.GitStatus)
	}
	if got.AgentBar != nil {
		t.Error("AgentBar should be nil when no AS is selected (the 'AgentBar 没有 AS 则忽略' rule)")
	}
}

// TestBuild_AllIdentityPresentPopulatesAllSubBars verifies the
// happy path: an AS with all identity fields set produces a
// StatusBar with both GitBar and AgentBar populated.
func TestBuild_AllIdentityPresentPopulatesAllSubBars(t *testing.T) {
	as := agentsession.NewAgentSession("as_x", "cs_x", "claude", t.TempDir(), nil)
	as.SetModel("opus-4-5")
	usage := &agent.UsageInfo{InputTokens: 5, OutputTokens: 7}

	got := Build(as, usage, Deps{})
	if got == nil {
		t.Fatal("AS with identity should produce StatusBar")
	}
	if got.AgentBar == nil {
		t.Error("AgentBar should be populated when Agent/Model set")
	}
	if got.AgentBar.Agent != "claude" || got.AgentBar.Model != "opus-4-5" {
		t.Errorf("AgentBar = %+v, want Agent=claude Model=opus-4-5", got.AgentBar)
	}
	if got.GitBar == nil {
		t.Error("GitBar should always be populated when AS has a Cwd")
	}
	if got.UsageBar == nil || got.UsageBar.UsageInfo != usage {
		t.Error("UsageBar should be populated when usage is passed")
	}
}

// TestBuild_UsageOnlyReturnsPopulatedStatusBar covers the
// "event has usage but no AS state yet" case (e.g. usage
// arrives before the AS reports its model). Usage alone is
// enough to materialize a StatusBar in the new structure (it
// populates UsageBar).
func TestBuild_UsageOnlyReturnsPopulatedStatusBar(t *testing.T) {
	as := agentsession.NewAgentSession("as_x", "cs_x", "", t.TempDir(), nil)
	usage := &agent.UsageInfo{InputTokens: 5, OutputTokens: 7}
	got := Build(as, usage, Deps{})
	if got == nil {
		t.Fatal("usage should be sufficient to materialize StatusBar")
	}
	if got.UsageBar == nil || got.UsageBar.UsageInfo != usage {
		t.Errorf("UsageBar.UsageInfo = %+v, want %+v", got.UsageBar.UsageInfo, usage)
	}
}

// TestStampFromAS_LegacyGate verifies StampFromAS still works
// for the MessageStateBus path (which pre-stamps explicitly so
// the emitter's source doesn't double-attach).
func TestStampFromAS_LegacyGate(t *testing.T) {
	as := agentsession.NewAgentSession("as_legacy", "cs_l", "claude", t.TempDir(), nil)
	msg := &messages.OutboundMessage{
		Kind:   messages.OutMessageState,
		ChatID: "oc_l",
		Usage:  &agent.UsageInfo{InputTokens: 1, OutputTokens: 1},
	}
	StampFromAS(msg, as, Deps{})
	if msg.StatusBar == nil {
		t.Fatal("MessageSubmitted-style path must populate StatusBar")
	}
	if msg.StatusBar.UsageBar == nil {
		t.Error("Usage should be co-located on StatusBar.UsageBar for legacy gate path")
	}
}

// TestAttachIfMissing_HappyPath verifies that AttachIfMissing
// populates msg.StatusBar from a non-nil Source when msg has no
// StatusBar yet.
func TestAttachIfMissing_HappyPath(t *testing.T) {
	called := false
	src := func(chatID string) *messages.StatusBar {
		called = true
		return &messages.StatusBar{
			GitBar: &messages.GitStatusBar{Workspace: chatID},
		}
	}
	msg := &messages.OutboundMessage{ChatID: "oc_attach"}
	AttachIfMissing(msg, src)
	if !called {
		t.Fatal("Source should have been invoked when msg.StatusBar was nil")
	}
	if msg.StatusBar == nil {
		t.Fatal("StatusBar should be attached")
	}
	if msg.StatusBar.GitBar == nil || msg.StatusBar.GitBar.Workspace != "oc_attach" {
		t.Errorf("GitBar.Workspace = %+v, want oc_attach", msg.StatusBar.GitBar)
	}
}

// TestAttachIfMissing_PreservesExisting verifies the "missing"
// gate: a pre-filled StatusBar wins over the Source. Callers
// that explicitly stamped (e.g. StampFromAS for the
// MessageStateBus MessageSubmitted path) must not be overwritten.
func TestAttachIfMissing_PreservesExisting(t *testing.T) {
	pre := &messages.StatusBar{
		AgentBar: &messages.AgentStatusBar{Agent: "claude"},
	}
	src := func(chatID string) *messages.StatusBar {
		t.Fatalf("Source must not be invoked when msg.StatusBar is already set")
		return nil
	}
	msg := &messages.OutboundMessage{ChatID: "oc_attach", StatusBar: pre}
	AttachIfMissing(msg, src)
	if msg.StatusBar != pre {
		t.Errorf("StatusBar should be unchanged, got %+v want %+v", msg.StatusBar, pre)
	}
}

// TestAttachIfMissing_NilSourceNoOp verifies the nil-Source
// path: no panic, no attach. Matches the production behaviour
// where the Emitter is constructed with a zero Options
// (Source == nil) and every Send is a passthrough.
func TestAttachIfMissing_NilSourceNoOp(t *testing.T) {
	msg := &messages.OutboundMessage{ChatID: "oc_attach"}
	AttachIfMissing(msg, nil)
	if msg.StatusBar != nil {
		t.Errorf("nil Source should leave StatusBar nil, got %+v", msg.StatusBar)
	}
}

// TestAttachIfMissing_SourceReturnsNil verifies that a Source
// returning nil is treated as "skip this turn" — no attach, no
// error, msg.StatusBar stays nil.
func TestAttachIfMissing_SourceReturnsNil(t *testing.T) {
	src := func(chatID string) *messages.StatusBar { return nil }
	msg := &messages.OutboundMessage{ChatID: "oc_attach"}
	AttachIfMissing(msg, src)
	if msg.StatusBar != nil {
		t.Errorf("Source returning nil should leave StatusBar nil, got %+v", msg.StatusBar)
	}
}

// TestAttachIfMissing_CoLocatesUsage pins F-55: when Source
// produced a StatusBar without a UsageBar but msg carries
// Usage, copy it across. The channel footer reads
// sb.UsageBar (not msg.Usage), so a missing co-located value
// would silently drop Line 2 of the footer.
func TestAttachIfMissing_CoLocatesUsage(t *testing.T) {
	usage := &agent.UsageInfo{InputTokens: 5, OutputTokens: 7}
	src := func(chatID string) *messages.StatusBar {
		// Note: no UsageBar set on the returned StatusBar.
		return &messages.StatusBar{
			GitBar: &messages.GitStatusBar{Workspace: chatID},
		}
	}
	msg := &messages.OutboundMessage{ChatID: "oc_attach", Usage: usage}
	AttachIfMissing(msg, src)
	if msg.StatusBar == nil {
		t.Fatal("StatusBar should be attached")
	}
	if msg.StatusBar.UsageBar == nil {
		t.Fatal("UsageBar should be co-located from msg.Usage")
	}
	if msg.StatusBar.UsageBar.UsageInfo != usage {
		t.Errorf("UsageBar.UsageInfo = %+v, want %+v", msg.StatusBar.UsageBar.UsageInfo, usage)
	}
}

// TestAttachIfMissing_DoesNotOverwriteUsageBar verifies the
// "UsageBar already present" branch: if Source populated
// UsageBar, the co-location path must not clobber it.
func TestAttachIfMissing_DoesNotOverwriteUsageBar(t *testing.T) {
	srcUsage := &agent.UsageInfo{InputTokens: 1, OutputTokens: 1}
	msgUsage := &agent.UsageInfo{InputTokens: 99, OutputTokens: 99}
	src := func(chatID string) *messages.StatusBar {
		return &messages.StatusBar{
			UsageBar: &messages.UsageStatusBar{UsageInfo: srcUsage},
		}
	}
	msg := &messages.OutboundMessage{ChatID: "oc_attach", Usage: msgUsage}
	AttachIfMissing(msg, src)
	if msg.StatusBar.UsageBar.UsageInfo != srcUsage {
		t.Errorf("UsageBar.UsageInfo = %+v, want Source-set value %+v", msg.StatusBar.UsageBar.UsageInfo, srcUsage)
	}
}
