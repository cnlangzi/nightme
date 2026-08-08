package gtw

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/chatsession"
)

// ContextSlot is the gtw-package view of one Manager's per-chat
// context slot. Production: gtw.Manager.GetContext / SetContext.
// Tests can pass a small adapter that wraps those methods.
//
// The slot is value-typed: Load returns a copy, Store accepts a
// copy. The gtw package never holds a live pointer to the stored
// value, which keeps the reader/writer race surface to zero.
// Pass the zero Context{} to Store to clear.
type ContextSlot interface {
	Load() Context
	Store(c Context)
}

// DraftsMap is the gtw-package view of one ChatSession's
// gtwDrafts map. Production: gtwDraftsMap from
// internal/gateway. Tests: a map[string]*Draft with closures.
type DraftsMap interface {
	Store(userMsgID string, d *Draft)
	Take(userMsgID string) *Draft
	Lookup(userMsgID string) *Draft
}

// HandlerDeps wires the side effects RunFix / HandleAction need.
// All fields are required; pass an instance constructed in the
// runtime's startup code (cmd/nightme/run.go).
type HandlerDeps struct {
	// Send is the IM-side send callback. Production: wraps
	// gateway.Channel.Send; tests: appends to a slice.
	Send SendFunc
	// SendCard (F-46) is the IM-side card send callback. Returns
	// the bot-side message id assigned by the channel so the
	// dispatcher can store it on the draft for later PATCH.
	// Production: wraps gateway.Channel.SendCard; tests can
	// inject a fake or leave nil (legacy fallback uses Send +
	// discards the id, action handler emits no PATCH).
	SendCard SendCardFunc
	// Git wraps the local git binary. Tests inject a fake.
	Git GitRunner
	// Prober is the HTTPProber for Detect's Stage B API probe
	// (used only when the URL hint is ambiguous). nil → production
	// uses ExecHTTPProber{} with 3s default timeout. Tests inject
	// a fake that returns canned JSON for fixture-driven cases.
	Prober HTTPProber
	// Detect is the provider-detection function. nil → production
	// uses package-level Detect (URL hint + API probe). Tests
	// override to inject a fakeProvider without running real
	// Detect logic (see F-50 §1.4 for the injection pattern).
	Detect func(ctx context.Context, remoteURL string, prober HTTPProber) (GitProvider, error)
	// Now is the clock. Tests override for deterministic drafts.
	Now func() time.Time
}

// Result is the gtw-package view of a command's outcome. Mirrors
// gateway.CommandResult without taking a dependency on gateway.
// The runtime layer converts to *gateway.CommandResult before
// returning from the slash-command handler.
type Result struct {
	Consumed bool
	Dropped  bool
}

// RunFix is the exported entry point for the /gtw fix command.
// Called from cmd/nightme/run.go via gtw.Factory.runFix.
//
// F-XX: takes *chatsession.ChatSession directly (no longer through
// the Sender interface) and accepts a Mode parameter to route
// between the issue (remote) flow and the local branch flow.
//
//	args is the user-supplied argv tail AFTER the "fix"
//	subcommand. For ModeRemote: args[0] is the issue id.
//	For ModeLocal: args[0] is the literal branch name.
//	Pass args[1:] (not args[0]) into ModeLocal/Remote since
//	the "fix" subcommand token has already been consumed by
//	Factory.runFix.
func RunFix(
	ctx context.Context,
	mode Mode,
	cs *chatsession.ChatSession,
	slot ContextSlot,
	drafts DraftsMap,
	deps HandlerDeps,
	chatID, messageID string,
	args []string,
) (*Result, error) {
	if deps.Now == nil {
		deps.Now = timeNow
	}
	if len(args) < 1 {
		return reply(ctx, deps.Send, chatID, messageID,
			"Usage: /gtw fix <issue-id>  |  /gtw fix --name <branch>"), nil
	}

	// --- preflight: ActiveCwd must be set for both modes --------
	if cs == nil || cs.ActiveCwd() == "" {
		return reply(ctx, deps.Send, chatID, messageID,
			"❌ No active workspace. Send /cwd <path> first."), nil
	}
	if cur := slot.Load(); cur != (Context{}) {
		return reply(ctx, deps.Send, chatID, messageID,
			"⚠️ Already inside a /gtw fix. Finish or cancel it first."), nil
	}

	switch mode {
	case ModeLocal:
		return runFixLocal(ctx, cs, slot, drafts, deps, chatID, messageID, args[0])
	default:
		// ModeRemote (and the empty/zero legacy default).
		return runFixRemote(ctx, cs, slot, drafts, deps, chatID, messageID, args[0])
	}
}

// runFixRemote implements the F-45 / F-XX ID-mode flow:
//
//	/gtw fix <issue-id>
//
// Steps:
//
//  1. RebuildContext (daemon-recovery; §5.7).
//  2. RepoRoot → RemoteOriginURL → Detect → GetIssue.
//  3. Branch name = DeriveBranchFromTitle(issue.Title, id).
//  4. PreflightWorktreeCreate → catches path / branch / parent
//     errors before WorktreeAdd.
//  5. BranchExists? → DraftFixBranchExists card.
//  6. AddLabel(LabelWIP); on WorktreeAdd failure RemoveLabel
//     and emit DraftFixWorktreeFail card.
//  7. SetActiveCwd → slot.Store(ModeRemote).
//  8. Render success card.
//  9. Dispatch issue body to ChatSession.QueueUserMessage so
//     the agent picks it up. Failure here does NOT roll back
//     the worktree — the user can re-trigger manually.
func runFixRemote(
	ctx context.Context,
	cs *chatsession.ChatSession,
	slot ContextSlot,
	drafts DraftsMap,
	deps HandlerDeps,
	chatID, messageID string,
	rawID string,
) (*Result, error) {
	issueID, err := parseIssueID(rawID)
	if err != nil {
		return reply(ctx, deps.Send, chatID, messageID, fmt.Sprintf("❌ %v", err)), nil
	}

	// F-XX: daemon-recovery via the worktree's git branch was
	// removed — the F-45 RebuildContext path relied on a
	// `fix/<id>-*` branch prefix which the new naming
	// convention no longer uses. Recovery now flows through the
	// BranchExists → existingPath == worktreePath check below:
	// when the user re-runs /gtw fix after a daemon restart
	// while the worktree + branch still exist, we hit the same-
	// path branch and skip creation. No separate RebuildContext
	// step is needed.

	prober := deps.Prober
	if prober == nil {
		prober = &ExecHTTPProber{}
	}

	// --- locate repo + remote (§5.2.② prep) -----------------------
	repoRoot, err := RepoRoot(ctx, cs.ActiveCwd(), deps.Git)
	if err != nil {
		return reply(ctx, deps.Send, chatID, messageID,
			"❌ Not in a git repository. Run /cwd <inside a repo> first."), nil
	}
	remoteURL, err := RemoteOriginURL(ctx, repoRoot, deps.Git)
	if err != nil || remoteURL == "" {
		return reply(ctx, deps.Send, chatID, messageID,
			"❌ No `origin` remote. Add one with `git remote add origin <url>`."), nil
	}
	detect := deps.Detect
	if detect == nil {
		detect = Detect
	}
	provider, err := detect(ctx, remoteURL, prober)
	if err != nil {
		// D3 split: distinguish "URL is malformed" (user error)
		// from "host not recognised as GitHub/GitLab". Never
		// echo the raw remoteURL — it may carry userinfo.
		redacted := redactForDisplay(remoteURL)
		switch {
		case errors.Is(err, ErrInvalidRemoteURL):
			if redacted == "" {
				return reply(ctx, deps.Send, chatID, messageID,
					"❌ 无效的 remote URL（凭证已脱敏）\n  Expected: https://github.com/<owner>/<repo>.git, git@github.com:<owner>/<repo>.git, ssh://git@<host>/path, git://<host>/path, etc."), nil
			}
			return reply(ctx, deps.Send, chatID, messageID,
				fmt.Sprintf("❌ 无效的 remote URL: %s\n  Expected: https://github.com/<owner>/<repo>.git, git@github.com:<owner>/<repo>.git, ssh://git@<host>/path, git://<host>/path, etc.", redacted)), nil
		default:
			return reply(ctx, deps.Send, chatID, messageID,
				fmt.Sprintf("❌ 暂不支持的 Git 平台 (host: %s — neither github.com/gitlab.com URL hint nor /api/v3/meta or /api/v4/version probe recognised it).", redacted)), nil
		}
	}
	if provider == nil {
		return reply(ctx, deps.Send, chatID, messageID,
			"❌ Provider detection returned no result (deps.Detect override bug)."), nil
	}
	providerKind := provider.Kind()
	owner, repo, err := ParseRepoOwner(remoteURL)
	if err != nil {
		return reply(ctx, deps.Send, chatID, messageID,
			fmt.Sprintf("❌ Cannot parse owner/repo from remote URL %s.", redactForDisplay(remoteURL))), nil
	}

	// --- fetch issue + derive branch (§5.2.②) --------------------
	issue, err := provider.GetIssue(ctx, owner, repo, issueID)
	if err != nil {
		if errors.Is(err, ErrIssueNotFound) {
			return reply(ctx, deps.Send, chatID, messageID,
				fmt.Sprintf("❌ Issue #%d not found in %s/%s.", issueID, owner, repo)), nil
		}
		return reply(ctx, deps.Send, chatID, messageID,
			fmt.Sprintf("❌ Failed to fetch issue: %v", err)), nil
	}
	branch := DeriveBranchFromTitle(issue.Title, issueID)
	worktreePath := WorktreePath(repoRoot, branch)

	// --- branch-exists decision (§5.3.1) -------------------------
	// Done BEFORE the preflight path-exists check: when the
	// branch is attached at exactly the target worktree path,
	// this is the daemon-recovery path (user re-runs /gtw fix
	// after a restart) and the worktree directory is expected
	// to exist. Preflight's "path occupied" check would
	// otherwise block the recovery. The branch-exists card
	// handles the "branch occupied elsewhere" case (where
	// preflight would also fail).
	exists, err := BranchExists(ctx, repoRoot, branch, deps.Git)
	if err != nil {
		return reply(ctx, deps.Send, chatID, messageID,
			fmt.Sprintf("❌ git show-ref failed: %v", err)), nil
	}
	if exists {
		existingPath, _ := WorktreeListPath(ctx, repoRoot, branch, deps.Git)
		// Normalize both sides: git porcelain paths are absolute but
		// may carry trailing slashes or ./ components depending on
		// platform; WorktreePath returns a Clean result. Compare
		// through filepath.Clean to keep the recovery check in
		// lockstep with PreflightWorktreeCreate.
		if existingPath != "" && filepath.Clean(existingPath) == filepath.Clean(worktreePath) {
			return completeFixAndDispatch(ctx, cs, slot, deps, chatID, messageID,
				branch, worktreePath, owner+"/"+repo, ModeRemote, issueID, issue, true /* skipDispatch */)
		}
		return emitBranchExistsDraft(ctx, deps, chatID, messageID, messageID, drafts, FixDraftPayload{
			IssueID:  issueID,
			Title:    issue.Title,
			Branch:   branch,
			Slug:     branch,
			Repo:     owner + "/" + repo,
			Provider: string(providerKind),
			ChatID:   chatID,
		}, existingPath)
	}

	// --- preflight (path / branch / parent) ----------------------
	if err := PreflightWorktreeCreate(ctx, repoRoot, branch, worktreePath, deps.Git); err != nil {
		return reply(ctx, deps.Send, chatID, messageID, err.Error()), nil
	}

	// --- label the issue (§5.2.② cont.) -------------------------
	labelAdded := false
	if err := provider.AddLabel(ctx, owner, repo, issueID, LabelWIP); err != nil {
		_ = deps.Send(ctx, OutMsg{
			ChatID:  chatID,
			ReplyTo: messageID,
			Text:    fmt.Sprintf("⚠️ Could not add label %q: %v\n(proceeding with local worktree)", LabelWIP, err),
		})
	} else {
		labelAdded = true
	}

	// --- create the worktree (§5.2.③) ---------------------------
	if err := WorktreeAdd(ctx, repoRoot, branch, worktreePath, "HEAD", deps.Git); err != nil {
		if labelAdded {
			_ = provider.RemoveLabel(ctx, owner, repo, issueID, LabelWIP)
		}
		return emitWorktreeFailDraft(ctx, deps, chatID, messageID, messageID, drafts, FixDraftPayload{
			IssueID:    issueID,
			Title:      issue.Title,
			Branch:     branch,
			Slug:       branch,
			Repo:       owner + "/" + repo,
			Provider:   string(providerKind),
			GitError:   tailLines(stderrFromWorktreeErr(err), 10),
			LabelAdded: labelAdded,
			ChatID:     chatID,
		})
	}

	// --- switch cwd + write context + render + dispatch ----------
	return completeFixAndDispatch(ctx, cs, slot, deps, chatID, messageID,
		branch, worktreePath, owner+"/"+repo, ModeRemote, issueID, issue, false)
}

// runFixLocal implements the F-XX local-mode flow:
//
//	/gtw fix --name <branch>
//
// Steps:
//
//  1. DeriveBranchFromName(rawName) — slugifies user input; errors
//     out if the result is empty (no remote-issue id to fall
//     back to).
//  2. RepoRoot → no origin required.
//  3. PreflightWorktreeCreate.
//  4. BranchExists? → DraftFixBranchExists card (no LabelAdded
//     payload — local mode has no remote state to roll back).
//  5. WorktreeAdd; on failure emit DraftFixWorktreeFail card.
//  6. SetActiveCwd → slot.Store(ModeLocal, Issue=-1).
//  7. Render the simplified local success card.
//
// Local mode does NOT call provider.GetIssue / AddLabel /
// QueueUserMessage — the user is opting into a no-remote flow
// that should work even in repos without an `origin`.
func runFixLocal(
	ctx context.Context,
	cs *chatsession.ChatSession,
	slot ContextSlot,
	drafts DraftsMap,
	deps HandlerDeps,
	chatID, messageID string,
	rawName string,
) (*Result, error) {
	branch, err := DeriveBranchFromName(rawName)
	if err != nil {
		return reply(ctx, deps.Send, chatID, messageID, "❌ "+err.Error()), nil
	}

	repoRoot, err := RepoRoot(ctx, cs.ActiveCwd(), deps.Git)
	if err != nil {
		return reply(ctx, deps.Send, chatID, messageID,
			"❌ Not in a git repository. Run /cwd <inside a repo> first."), nil
	}
	worktreePath := WorktreePath(repoRoot, branch)

	// --- branch-exists decision (BEFORE preflight; see ID-mode
	// runFixRemote for the rationale — preflight's "path occupied"
	// check would block the daemon-recovery path where the branch
	// is already attached at the target worktree path).
	exists, err := BranchExists(ctx, repoRoot, branch, deps.Git)
	if err != nil {
		return reply(ctx, deps.Send, chatID, messageID,
			fmt.Sprintf("❌ git show-ref failed: %v", err)), nil
	}
	if exists {
		existingPath, _ := WorktreeListPath(ctx, repoRoot, branch, deps.Git)
		// See runFixRemote: normalize both sides so porcelain's
		// sometimes-dirty paths compare equal to WorktreePath.
		if existingPath != "" && filepath.Clean(existingPath) == filepath.Clean(worktreePath) {
			return completeFixAndDispatch(ctx, cs, slot, deps, chatID, messageID,
				branch, worktreePath, "", ModeLocal, -1, nil, true /* skipDispatch */)
		}
		return emitBranchExistsDraft(ctx, deps, chatID, messageID, messageID, drafts, FixDraftPayload{
			IssueID: -1,
			Title:   "(local branch)",
			Branch:  branch,
			Slug:    branch,
			Repo:    "",
			ChatID:  chatID,
		}, existingPath)
	}

	if err := PreflightWorktreeCreate(ctx, repoRoot, branch, worktreePath, deps.Git); err != nil {
		return reply(ctx, deps.Send, chatID, messageID, err.Error()), nil
	}

	if err := WorktreeAdd(ctx, repoRoot, branch, worktreePath, "HEAD", deps.Git); err != nil {
		return emitWorktreeFailDraft(ctx, deps, chatID, messageID, messageID, drafts, FixDraftPayload{
			IssueID:  -1,
			Title:    "(local branch)",
			Branch:   branch,
			Slug:     branch,
			Repo:     "",
			GitError: tailLines(stderrFromWorktreeErr(err), 10),
			ChatID:   chatID,
		})
	}

	return completeFixAndDispatch(ctx, cs, slot, deps, chatID, messageID,
		branch, worktreePath, "", ModeLocal, -1, nil, false)
}

// completeFixAndDispatch handles the common tail of both modes:
// switch cwd → store Context → render success card → dispatch
// the issue body to the agent (ID mode only). Centralising this
// means both modes share the same "after worktree is created"
// logic; the mode-specific bits stay in runFixRemote /
// runFixLocal.
//
// issue is non-nil only in ID mode; local mode passes nil (the
// dispatcher check at the bottom skips it). skipDispatch is true
// when the caller already decided we shouldn't dispatch (e.g.
// the branch was already attached at the target path — a re-entry
// after daemon recovery).
func completeFixAndDispatch(
	ctx context.Context,
	cs *chatsession.ChatSession,
	slot ContextSlot,
	deps HandlerDeps,
	chatID, messageID, branch, worktreePath, repo string,
	mode Mode,
	issueID int,
	issue *Issue,
	skipDispatch bool,
) (*Result, error) {
	// --- switch cwd (§5.2.④) -------------------------------------
	if err := cs.SetActiveCwd(worktreePath); err != nil {
		return reply(ctx, deps.Send, chatID, messageID,
			fmt.Sprintf("❌ SetActiveCwd failed: %v", err)), nil
	}

	// --- write gtwContext (§5.2.⑤) -------------------------------
	now := deps.Now()
	slot.Store(Context{
		Mode:      mode,
		Issue:     issueID,
		Branch:    branch,
		Worktree:  worktreePath,
		State:     StateFixing,
		UpdatedAt: now,
	})

	// --- render the success card (§5.2.⑥) ------------------------
	var card string
	if mode == ModeLocal {
		card = renderFixLocalSuccessCard(branch, worktreePath)
	} else {
		// ID mode. Callers (runFixRemote) guarantee issue is
		// non-nil; a nil here would be a programming error.
		card = renderFixSuccessCard(issue, branch, worktreePath, repo)
	}
	result := reply(ctx, deps.Send, chatID, messageID, card)

	// --- dispatch issue to agent (ID mode only) -------------------
	// We do this AFTER the reply so the user sees the success
	// card immediately even if dispatch stalls / fails. Failures
	// here are warn-only — the worktree is the durable side
	// effect; a failed dispatch can be retried by the user
	// re-running /gtw fix or by manually sending the issue
	// reference to the agent.
	if mode == ModeRemote && !skipDispatch && issue != nil {
		if err := dispatchIssueToChatSession(ctx, cs, deps.Now, chatID, messageID, issue, branch, repo); err != nil {
			slog.Default().Warn("gtw: dispatch issue to agent failed",
				"issue_id", issue.ID,
				"chat_id", chatID,
				"err", err)
			// Append a single-line warning so the user knows
			// the agent didn't receive the dispatch. We do not
			// roll back the worktree — see comment above.
			_ = deps.Send(ctx, OutMsg{
				ChatID:  chatID,
				ReplyTo: messageID,
				Text:    fmt.Sprintf("⚠️ Could not dispatch issue #%d to agent: %v\nThe worktree is ready; you can /cwd into %s and tell the agent to fix #%d.", issue.ID, err, worktreePath, issue.ID),
			})
		}
	}
	return result, nil
}

// dispatchIssueToChatSession packages the issue into a fixed-
// template text block and pushes it into the chat session's
// input queue. The queue's TryFlush() will spawn (or reuse) an
// AgentSession and submit the prompt.
//
// The synthesised Message reuses the GTW command's inbound
// messageID so the agent's reply is anchored as a thread reply
// under the user's original /gtw fix command — IM UX continuity.
//
// Kind is MessageKindQueue so the message forms its own Prompt
// batch and isn't merged with subsequent user messages (a user
// typing "thanks" right after the /gtw fix shouldn't be batched
// into the same prompt as "fix issue #42").
//
// now is the clock dependency; callers pass deps.Now (which is
// already used by completeFixAndDispatch for the Context
// timestamp) so tests can pin time deterministically.
func dispatchIssueToChatSession(
	ctx context.Context,
	cs *chatsession.ChatSession,
	now func() time.Time,
	chatID, messageID string,
	issue *Issue,
	branch, repo string,
) error {
	body := buildIssueDispatchText(issue, branch, repo)
	// F-54 timing: the runtime dispatcher (cmd/nightme/run.go)
	// emits MessageQueued BEFORE QueueUserMessage so channels
	// can render the ⏳ indicator while the agent spawns. We
	// honour the same contract here: emit first, then queue.
	cs.EmitMessageState(messageID, agent.MessageQueued)
	return cs.QueueUserMessage(chatsession.Message{
		ID:         messageID,
		ChatID:     chatID,
		Blocks:     []agent.ContentBlock{{Type: agent.ContentText, Text: body}},
		ReceivedAt: now(),
		Kind:       chatsession.MessageKindQueue,
	})
}

// buildIssueDispatchText formats the issue as a fixed-template
// markdown block. The template lives here (rather than in the
// renderer) because it's the "agent prompt" not the "user-facing
// success card". The user never sees this body directly — they
// see the success card; the agent sees this block.
//
// Section order is stable so agent prompts can rely on it:
// header / metadata / body / closing.
func buildIssueDispatchText(issue *Issue, branch, repo string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "📥 GitHub issue #%d — %s\n\n", issue.ID, issue.Title)
	fmt.Fprintf(&b, "## Metadata\n")
	fmt.Fprintf(&b, "- repo: %s\n", repo)
	fmt.Fprintf(&b, "- branch: %s\n", branch)
	fmt.Fprintf(&b, "- url: %s\n\n", issue.URL)
	if strings.TrimSpace(issue.Body) != "" {
		b.WriteString("## Description\n")
		b.WriteString(issue.Body)
		b.WriteString("\n\n")
	}
	b.WriteString("## Task\n")
	b.WriteString("Please investigate the issue above and implement a fix on the branch noted. ")
	b.WriteString("The worktree is already prepared; reply when you have a plan, then proceed.")
	return b.String()
}

// parseIssueID accepts a string like "42" or "#42" and returns the
// int. Anything else is an error.
func parseIssueID(raw string) (int, error) {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "#")
	if raw == "" {
		return 0, errors.New("empty issue id")
	}
	n := 0
	for _, r := range raw {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("invalid issue id %q (digits only)", raw)
		}
		n = n*10 + int(r-'0')
	}
	if n == 0 {
		return 0, errors.New("issue id cannot be 0")
	}
	return n, nil
}

// tailLines returns the last n non-empty lines of s, joined with \n.
func tailLines(s string, n int) string {
	if s == "" {
		return ""
	}
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) <= n {
		return strings.Join(lines, "\n")
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}

// stderrFromWorktreeErr extracts the captured stderr from a
// *WorktreeError. Returns "" for other error kinds.
func stderrFromWorktreeErr(err error) string {
	if werr, ok := errors.AsType[*WorktreeError](err); ok {
		return werr.Stderr
	}
	return ""
}

func emitBranchExistsDraft(
	ctx context.Context,
	deps HandlerDeps,
	chatID, messageID, userMsgID string,
	drafts DraftsMap,
	payload FixDraftPayload,
	existingPath string,
) (*Result, error) {
		card := BranchExistsCard(payload, existingPath)
		return sendDraft(ctx, deps, chatID, messageID, userMsgID, card, drafts, DraftFixBranchExists, payload)
}

func emitWorktreeFailDraft(
	ctx context.Context,
	deps HandlerDeps,
	chatID, messageID, userMsgID string,
	drafts DraftsMap,
	payload FixDraftPayload,
) (*Result, error) {
		card := WorktreeFailCard(payload)
		return sendDraft(ctx, deps, chatID, messageID, userMsgID, card, drafts, DraftFixWorktreeFail, payload)
}

func sendDraft(
	ctx context.Context,
	deps HandlerDeps,
	chatID, messageID, userMsgID string,
	card Card,
	drafts DraftsMap,
	kind DraftKind,
	payload FixDraftPayload,
) (*Result, error) {
	requestID := "gtw-fix-" + userMsgID
	if requestID == "" {
		requestID = "gtw-fix-" + payload.Branch
	}
	card.RequestID = requestID

	var botMsgID string
	if deps.SendCard != nil {
		id, err := deps.SendCard(ctx, OutCardMsg{
			ChatID:  chatID,
			ReplyTo: messageID,
			Card:    card,
		})
		if err == nil {
			botMsgID = id
		}
		// On error: fall through to text Send as a best-effort so
		// the user still sees the decision content even if the
		// channel's card path is unavailable.
	}
	if deps.SendCard == nil || botMsgID == "" {
		// Legacy / fallback: render the card as plain markdown and
		// send via deps.Send. The dispatcher still stores the
		// draft so the reaction pipeline works; the action handler
		// just emits plain text follow-ups (no PATCH) when the
		// bot message id is empty.
		_ = deps.Send(ctx, OutMsg{
			ChatID:  chatID,
			ReplyTo: messageID,
			Text:    renderCardMarkdown(card),
		})
	}

	drafts.Store(userMsgID, &Draft{
		Kind:          kind,
		Payload:       payload,
		CreatedAt:     deps.Now(),
		BotMessageID:  botMsgID,
		CardTitle:     card.Title,
		CardBody:      card.Body,
		CardChoices:   card.Choices,
		CardRequestID: requestID,
	})
	return &Result{Consumed: true}, nil
}

// toChatsessionCardChoices was removed in F-51: the gtw package
// now owns CardChoice directly (no chatsession alias needed).
// The renderer stores card.Choices verbatim on the draft.

// renderCardMarkdown flattens a Card back to plain markdown for
// legacy channels that don't support interactive cards (Feishu
// Web in some configs, Slack, etc.). The shape mirrors the F-45
// plain-text decision cards so the user's view is unchanged.
func renderCardMarkdown(c Card) string {
	var b strings.Builder
	if c.Title != "" {
		b.WriteString(c.Title)
		b.WriteString("\n")
	}
	if c.Body != "" {
		b.WriteString(c.Body)
		b.WriteString("\n")
	}
	if len(c.Choices) > 0 {
		b.WriteString("\n选择操作(反应对应 emoji):\n")
		for _, ch := range c.Choices {
			label := ch.Label
			if ch.Emoji != "" {
				label = ch.Emoji + " " + label
			}
			b.WriteString("  ")
			b.WriteString(label)
			b.WriteString("\n")
		}
	}
	return b.String()
}

// reply is the single-send-and-ack helper. The reply is threaded
// under the user's /gtw fix message (ReplyTo = messageID) so the
// channel can render it as a thread reply rather than a fresh top-
// level bubble.
//
// F-XX: nil-safe on `send`. The runtime historically left
// HandlerDeps.Send nil (chatSessionSender carried the actual
// channel adapter); if a future wiring change regresses that,
// calling a nil SendFunc would panic. The defensive `if send
// != nil` keeps the helper safe and degrades to "reply
// consumed but no text sent" rather than crashing the daemon.
func reply(ctx context.Context, send SendFunc, chatID, messageID, text string) *Result {
	if send != nil {
		_ = send(ctx, OutMsg{ChatID: chatID, ReplyTo: messageID, Text: text})
	}
	return &Result{Consumed: true}
}