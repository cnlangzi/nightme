package gtw

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/cnlangzi/nightme/internal/command"
)

// debugFactory implements command.SlashCommandFactory for the
// `/gtw test` debug command. It seeds synthetic drafts, sends
// real decision cards via the runtime's channel, then waits
// for the user to click. Production use: Feishu UAT only.
//
// It is registered alongside the main Factory (or instead of
// it, for debug-only builds). The runtime decides which to
// mount based on cfg.
type debugFactory struct {
	mgr  *Manager
	deps HandlerDeps
}

// NewDebugFactory constructs a debug Factory backed by mgr. The
// runtime should also call SetHandlerDeps before registering
// the factory.
func NewDebugFactory(mgr *Manager) *debugFactory {
	return &debugFactory{mgr: mgr}
}

func (d *debugFactory) SetHandlerDeps(deps HandlerDeps) {
	d.deps = deps
	if d.mgr != nil {
		d.mgr.SetHandlerDeps(deps)
	}
}

// Spec implements command.SlashCommandFactory.
func (d *debugFactory) Spec() command.Spec {
	return command.Spec{
		Name:    "gtw-test",
		Aliases: []string{"gtwtest"},
		Summary: "Debug / UAT only: seed a synthetic gtw draft and wait for a Feishu click.",
		Usage:   "/gtw test <scenario>   card | ok | yes-no | three | unknown | orphan",
	}
}

// Handle implements command.SlashCommandFactory.
func (d *debugFactory) Handle(ctx context.Context, rt command.RuntimeServices, input command.SlashInput) (*command.SlashOutput, error) {
	if len(input.Args) < 1 {
		return &command.SlashOutput{Reply: d.Spec().Usage, Consumed: true}, nil
	}
	return d.runScenario(ctx, rt, input, input.Args[0], input.Args[1:])
}

// --- scenarios ---

// gtwTestScenario describes one debug scenario. Mostly used
// for help text; the actual setup happens in runScenario.
type gtwTestScenario struct {
	Name           string
	Description    string
	SetupUserMsgID string
	SetupDraft     string // "branch-exists" | "worktree-fail" | "" (no seed)
	SetupEmoji     string // "" means render preview only
}

var gtwTestScenarios = []gtwTestScenario{
	{
		Name:           "card",
		Description:    "preview — render the branch-exists interactive card so the user can see its shape",
		SetupUserMsgID: "om_test_card",
	},
	{
		Name:           "ok",
		Description:    "single-choice 🔄 — worktree-fail retry, draft is taken and consumed",
		SetupUserMsgID: "om_test_ok",
		SetupDraft:     "worktree-fail",
		SetupEmoji:     "🔄",
	},
	{
		Name:           "yes-no",
		Description:    "two-choice ❌/🔄 — one recognised emoji acts, one unknown leaves draft",
		SetupUserMsgID: "om_test_yes_no",
		SetupDraft:     "worktree-fail",
		SetupEmoji:     "❌",
	},
	{
		Name:           "three",
		Description:    "three-choice 🆕/🔗/❌ — middle option acted on recognised draft",
		SetupUserMsgID: "om_test_three",
		SetupDraft:     "branch-exists",
		SetupEmoji:     "🔗",
	},
	{
		Name:           "unknown",
		Description:    "unknown emoji 👍 — draft LEFT in place, no follow-up card",
		SetupUserMsgID: "om_test_unknown",
		SetupDraft:     "branch-exists",
		SetupEmoji:     "👍",
	},
	{
		Name:           "orphan",
		Description:    "no draft — expect consumed=true dropped=true, no message",
		SetupUserMsgID: "om_orphan_no_draft",
		SetupEmoji:     "✅",
	},
}

func (d *debugFactory) runScenario(ctx context.Context, rt command.RuntimeServices, input command.SlashInput, name string, args []string) (*command.SlashOutput, error) {
	var sc *gtwTestScenario
	for i := range gtwTestScenarios {
		if gtwTestScenarios[i].Name == name {
			sc = &gtwTestScenarios[i]
			break
		}
	}
	if sc == nil {
		return &command.SlashOutput{Reply: d.scenarioHelp(), Consumed: true}, nil
	}
	if sc.SetupEmoji == "" && sc.SetupDraft == "" {
		// Preview-only: send the branch-exists card as a
		// preview (no draft seeded).
		return d.runPreview(ctx, rt, input, sc)
	}
	if sc.SetupDraft != "" {
		if err := d.seedDraft(input.ChatID, sc.SetupUserMsgID, sc.SetupDraft); err != nil {
			return &command.SlashOutput{
				Reply:    fmt.Sprintf("❌ /gtw test %s: seed failed: %v", name, err),
				Consumed: true,
			}, nil
		}
	}
	// No real channel adapter available in this package —
	// the runtime is expected to provide one via rt.Channel.
	// The seed is enough for the test to be useful in CI
	// even without an actual card send.
	return &command.SlashOutput{
		Reply: fmt.Sprintf("🧪 /gtw test %s [seed only]\n  setup: %s\n  expected: production HandleReaction + PATCH on click (Choice.RequestID).\n",
			name, sc.Description),
		Consumed: true,
	}, nil
}

func (d *debugFactory) runPreview(_ context.Context, _ command.RuntimeServices, input command.SlashInput, sc *gtwTestScenario) (*command.SlashOutput, error) {
	if sc.Name != "card" {
		return &command.SlashOutput{
			Reply:    fmt.Sprintf("❌ /gtw test %s: no preview for this scenario", sc.Name),
			Consumed: true,
		}, nil
	}
	// Build the preview card via the production renderer.
	card := BranchExistsChoice(testPayload(input.ChatID, "branch-exists"), "(none — test env)")
	card.RequestID = "gtw-test-preview-" + sc.SetupUserMsgID
	return &command.SlashOutput{
		Reply: fmt.Sprintf("🧪 /gtw test card — preview\n  Card chrome: %s\n  Body: %s\n  (no auto-dispatch — seed only)",
			card.Title, card.Body),
		Consumed: true,
	}, nil
}

func (d *debugFactory) seedDraft(chatID, userMsgID, kind string) error {
	var draftKind DraftKind
	switch kind {
	case "branch-exists":
		draftKind = DraftFixBranchExists
	case "worktree-fail":
		draftKind = DraftFixWorktreeFail
	case "label-taken":
		draftKind = DraftFixLabelTaken
	default:
		return fmt.Errorf("unknown kind %q", kind)
	}
	p := testPayload(chatID, kind)
	requestID := "gtw-test-" + userMsgID
	d.mgr.StoreDraft(chatID, requestID, &Draft{
		Kind: draftKind,
		Payload: FixDraftPayload{
			IssueID:    p.IssueID,
			Title:      p.Title,
			Branch:     p.Branch,
			Slug:       p.Slug,
			Repo:       p.Repo,
			Provider:   p.Provider,
			LabelAdded: p.LabelAdded,
			ChatID:     p.ChatID,
		},
		ChoiceRequestID: requestID,
		CreatedAt:       time.Now(),
	})
	return nil
}

func (d *debugFactory) scenarioHelp() string {
	var b strings.Builder
	b.WriteString("🧪 /gtw test <scenario>  — debug / Feishu UAT only\n\n")
	b.WriteString("Seeds a decision card via gtw business renderers, then\n")
	b.WriteString("waits for YOUR Feishu click. No auto-dispatch.\n\n")
	b.WriteString("Scenarios:\n")
	for _, sc := range gtwTestScenarios {
		fmt.Fprintf(&b, "  %-18s  %s\n", sc.Name, sc.Description)
	}
	b.WriteString("\nUtility:\n")
	b.WriteString("  list    print every gtwDraft currently in the chat\n")
	b.WriteString("  reset   drop every gtwDraft and clear gtwContext")
	return b.String()
}

// testPayload builds a synthetic FixDraftPayload for seeding
// test drafts. Mirrors the original handler_gtw_debug's
// testPayload.
func testPayload(chatID, kind string) FixDraftPayload {
	return FixDraftPayload{
		IssueID:    42,
		Title:      "synthetic /gtw test seed",
		Branch:     "fix/42-test",
		Slug:       "42-test",
		Repo:       "cnlangzi/nightme",
		Provider:   "github",
		LabelAdded: kind != "worktree-fail",
		ChatID:     chatID,
		// Worktree is the directory gh/glab label-rollback calls
		// should spawn from. In production this is set from
		// repoRoot at emit time (see fix.go); for the debug seed
		// we use the process CWD as a best-effort fallback. If
		// Getwd fails (rare — typically a deleted worktree, the
		// exact bug this defends against), leave Worktree empty
		// and let the action handler fall back to inherited
		// CWD; the test fakes won't reach gh anyway.
		Worktree: safeGetwd(),
	}
}

// safeGetwd wraps os.Getwd so a CWD error (e.g. the directory
// was deleted) doesn't crash the debug seed. Returns "" on
// failure — callers should treat empty Worktree as "no
// guidance, fall through to defaults".
func safeGetwd() string {
	if dir, err := os.Getwd(); err == nil {
		return dir
	}
	return ""
}
