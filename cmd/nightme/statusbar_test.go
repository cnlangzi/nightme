package main

import (
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/agentsession"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command/gtw"
	"github.com/cnlangzi/nightme/internal/gateway"
	"github.com/cnlangzi/nightme/internal/prcache"
)

// TestNewRuntimeStatusBar_NilMgr verifies that a nil Manager is
// handled defensively — production always provides one, but tests
// or future misconfigurations shouldn't panic. Returning nil
// matches the contract documented on newRuntimeStatusBar.
func TestNewRuntimeStatusBar_NilMgr(t *testing.T) {
	source := newRuntimeStatusBar(nil, &prcache.Registry{}, gtw.HandlerDeps{})
	if got := source("any-chat"); got != nil {
		t.Errorf("nil mgr should produce nil StatusBar, got %+v", got)
	}
}

// TestNewRuntimeStatusBar_UnknownChatID verifies the source looks
// up chat by id — a chat that was never created yields nil rather
// than panicking on a missing map entry.
func TestNewRuntimeStatusBar_UnknownChatID(t *testing.T) {
	mgr := chatsession.NewManager()
	source := newRuntimeStatusBar(mgr, &prcache.Registry{}, gtw.HandlerDeps{})
	if got := source("never-created"); got != nil {
		t.Errorf("unknown chatID should produce nil StatusBar, got %+v", got)
	}
}

// TestNewRuntimeStatusBar_NoActiveAS_StillProducesGitBar covers
// the GitBar-always-present rule: a chat with no selected AS
// but with a SelectedCwd still gets a StatusBar with GitBar
// populated (the user should always see what worktree they're
// on, even before any agent spawns). Pre-rename this same
// scenario produced nil — the GitBar fallback is a new
// behaviour shipped alongside the rename.
func TestNewRuntimeStatusBar_NoActiveAS_StillProducesGitBar(t *testing.T) {
	mgr := chatsession.NewManager()
	_, _ = mgr.GetOrCreate("oc_no_as", "claude")
	// Simulate the user having set /cwd: cs.SelectedCwd is now
	// non-empty even though no AS has been selected. Use a
	// t.TempDir() so the workspace is guaranteed non-git on any
	// developer machine (CI, fresh clones, etc.) — testing with
	// /tmp is environment-dependent because /tmp itself can be
	// a git worktree in some setups.
	cs, _ := mgr.GetOrCreate("oc_no_as", "claude")
	tmp := t.TempDir()
	_ = cs.SetSelectedCwd(tmp)

	source := newRuntimeStatusBar(mgr, &prcache.Registry{}, gtw.HandlerDeps{})
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
	// Non-git workspace → GitStatus is nil; the renderer drops
	// Line 3 in that case. The StatusBar itself is still
	// populated, which is the contract this test pins.
	if got.GitBar.GitStatus != nil {
		t.Errorf("non-git workspace should produce GitStatus=nil, got %+v", got.GitBar.GitStatus)
	}
	if got.AgentBar != nil {
		t.Error("AgentBar should be nil when no AS is selected (the 'AgentBar 没有 AS 则忽略' rule)")
	}
}

// TestBuildStatusBar_AllIdentityPresentPopulatesAllSubBars
// verifies the happy path: an AS with all identity fields set
// produces a StatusBar with both GitBar and AgentBar populated.
func TestBuildStatusBar_AllIdentityPresentPopulatesAllSubBars(t *testing.T) {
	as := agentsession.NewAgentSession("as_x", "cs_x", "claude", "/tmp", nil)
	as.SetModel("opus-4-5")
	usage := &agent.UsageInfo{InputTokens: 5, OutputTokens: 7}

	got := buildStatusBar(as, usage, &prcache.Registry{}, gtw.HandlerDeps{})
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

// TestBuildStatusBar_UsageOnlyReturnsPopulatedStatusBar covers
// the "event has usage but no AS state yet" case (e.g. usage
// arrives before the AS reports its model). Usage alone is
// enough to materialize a StatusBar in the new structure (it
// populates UsageBar).
func TestBuildStatusBar_UsageOnlyReturnsPopulatedStatusBar(t *testing.T) {
	as := agentsession.NewAgentSession("as_x", "cs_x", "", "/tmp", nil)
	usage := &agent.UsageInfo{InputTokens: 5, OutputTokens: 7}
	got := buildStatusBar(as, usage, &prcache.Registry{}, gtw.HandlerDeps{})
	if got == nil {
		t.Fatal("usage should be sufficient to materialize StatusBar")
	}
	if got.UsageBar == nil || got.UsageBar.UsageInfo != usage {
		t.Errorf("UsageBar.UsageInfo = %+v, want %+v", got.UsageBar.UsageInfo, usage)
	}
}

// TestStampFromAS_LegacyGate verifies stampFromAS still works
// for the MessageStateBus path (which pre-stamps explicitly so
// the emitter's source doesn't double-attach). Pre-rename this
// was `sessionContextInto`; the test keeps its name for
// bisect-ability.
func TestStampFromAS_LegacyGate(t *testing.T) {
	as := agentsession.NewAgentSession("as_legacy", "cs_l", "claude", "/tmp", nil)
	msg := &gateway.OutboundMessage{
		Kind:   gateway.OutMessageState,
		ChatID: "oc_l",
		Usage:  &agent.UsageInfo{InputTokens: 1, OutputTokens: 1},
	}
	stampFromAS(msg, as, &prcache.Registry{}, gtw.HandlerDeps{})
	if msg.StatusBar == nil {
		t.Fatal("MessageSubmitted-style path must populate StatusBar")
	}
	if msg.StatusBar.UsageBar == nil {
		t.Error("Usage should be co-located on StatusBar.UsageBar for legacy gate path")
	}
}
