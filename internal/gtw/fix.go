package gtw

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Sender is the small surface the gtw package needs from
// ChatSession. Production callers pass an adapter backed by
// *chatsession.ChatSession (see internal/gateway/handlers_gtw.go).
// Tests can pass a fake to avoid spinning up a real one.
type Sender interface {
	ActiveCwd() string
	SetActiveCwd(cwd string) error
}

// ContextSlot is the gtw-package view of one ChatSession's
// gtwContext slot. Production: gtwContextSlot from
// internal/gateway. Tests: a tiny struct with closures.
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
	// Git wraps the local git binary. Tests inject a fake.
	Git GitRunner
	// NewPlatform picks a PlatformClient based on the remote URL.
	// Production: NewPlatformClient. Tests: returns a fake.
	NewPlatform func(kind PlatformKind) (PlatformClient, error)
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
// Called from internal/gateway/handlers_gtw.go.
func RunFix(
	ctx context.Context,
	cs Sender,
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
		return reply(ctx, deps.Send, chatID, messageID, "Usage: /gtw fix <issue-id>"), nil
	}
	issueID, err := parseIssueID(args[0])
	if err != nil {
		return reply(ctx, deps.Send, chatID, messageID, fmt.Sprintf("❌ %v", err)), nil
	}

	// --- preflight (§5.2.①) ---------------------------------------
	if cs.ActiveCwd() == "" {
		return reply(ctx, deps.Send, chatID, messageID,
			"❌ No active workspace. Send /cwd <path> first."), nil
	}
	if cur := slot.Load(); cur != (Context{}) {
		return reply(ctx, deps.Send, chatID, messageID,
			"⚠️ Already inside a /gtw fix. Finish or cancel it first."), nil
	}

	// F-45 §5.7: daemon-recovery. If the in-memory gtwContext was
	// lost (daemon restart), the cwd may still be inside a worktree
	// holding a `fix/<id>-*` branch. Rebuild it transparently so
	// the user doesn't lose their fix state.
	if rebuilt := RebuildContext(ctx, cs, deps.Git, deps.NewPlatform); rebuilt != (Context{}) {
		slot.Store(rebuilt)
		_ = deps.Send(ctx, OutMsg{
			ChatID:  chatID,
			ReplyTo: messageID,
			Text: fmt.Sprintf(
				"♻️ Recovered /gtw fix #%d (state: %s).\n  branch: %s\n  worktree: %s\n  Re-run `/gtw fix %d` to continue, or use a different command.",
				rebuilt.Issue, rebuilt.State, rebuilt.Branch, rebuilt.Worktree, rebuilt.Issue),
		})
		return &Result{Consumed: true}, nil
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
	platformKind, err := DetectPlatform(remoteURL)
	if err != nil {
		return reply(ctx, deps.Send, chatID, messageID,
			"❌ 暂不支持的 Git 平台 (only github.com / gitlab.com are supported in v1)."), nil
	}
	owner, repo, err := ParseRepoOwner(remoteURL)
	if err != nil {
		return reply(ctx, deps.Send, chatID, messageID,
			fmt.Sprintf("❌ Cannot parse owner/repo from remote URL %q.", remoteURL)), nil
	}
	platform, err := deps.NewPlatform(platformKind)
	if err != nil {
		return reply(ctx, deps.Send, chatID, messageID, fmt.Sprintf("❌ %v", err)), nil
	}

	// --- fetch issue + derive branch + slug (§5.2.②) -------------
	issue, err := platform.GetIssue(ctx, owner, repo, issueID)
	if err != nil {
		if errors.Is(err, ErrIssueNotFound) {
			return reply(ctx, deps.Send, chatID, messageID,
				fmt.Sprintf("❌ Issue #%d not found in %s/%s.", issueID, owner, repo)), nil
		}
		return reply(ctx, deps.Send, chatID, messageID,
			fmt.Sprintf("❌ Failed to fetch issue: %v", err)), nil
	}
	branch := DeriveBranch(issueID, issue.Title)
	// The worktree directory name carries the issue-id prefix so
	// multiple worktrees under the same repo don't collide on
	// similar titles. DeriveSlug returns just the title-derived
	// component; we compose the full worktree slug here.
	worktreeSlug := fmt.Sprintf("%d-%s", issueID, DeriveSlug(issueID, issue.Title))
	worktreePath := WorktreePath(repoRoot, worktreeSlug)

	// --- branch-exists decision (§5.3.1) --------------------------
	exists, err := BranchExists(ctx, repoRoot, branch, deps.Git)
	if err != nil {
		return reply(ctx, deps.Send, chatID, messageID,
			fmt.Sprintf("❌ git show-ref failed: %v", err)), nil
	}
	if exists {
		existingPath, _ := WorktreeListPath(ctx, repoRoot, branch, deps.Git)
		return emitBranchExistsDraft(ctx, deps, chatID, messageID, messageID, drafts, FixDraftPayload{
			IssueID:  issueID,
			Title:    issue.Title,
			Branch:   branch,
			Slug:     worktreeSlug,
			Repo:     owner + "/" + repo,
			Platform: string(platformKind),
			ChatID:   chatID,
		}, existingPath)
	}

	// --- label the issue (§5.2.② cont.) ---------------------------
	labelAdded := false
	if err := platform.AddLabel(ctx, owner, repo, issueID, LabelWIP); err != nil {
		_ = deps.Send(ctx, OutMsg{
			ChatID:  chatID,
			ReplyTo: messageID,
			Text:    fmt.Sprintf("⚠️ Could not add label %q: %v\n(proceeding with local worktree)", LabelWIP, err),
		})
	} else {
		labelAdded = true
	}

	// --- create the worktree (§5.2.③) -----------------------------
	if err := WorktreeAdd(ctx, repoRoot, branch, worktreePath, "HEAD", deps.Git); err != nil {
		// Roll back the label we just added (§5.3.3 §5.4).
		if labelAdded {
			_ = platform.RemoveLabel(context.Background(), owner, repo, issueID, LabelWIP)
		}
		return emitWorktreeFailDraft(ctx, deps, chatID, messageID, messageID, drafts, FixDraftPayload{
			IssueID:    issueID,
			Title:      issue.Title,
			Branch:     branch,
			Slug:       worktreeSlug,
			Repo:       owner + "/" + repo,
			Platform:   string(platformKind),
			GitError:   tailLines(stderrFromWorktreeErr(err), 10),
			LabelAdded: labelAdded,
			ChatID:     chatID,
		})
	}

	// --- switch cwd (§5.2.④) --------------------------------------
	if err := cs.SetActiveCwd(worktreePath); err != nil {
		return reply(ctx, deps.Send, chatID, messageID,
			fmt.Sprintf("❌ SetActiveCwd failed: %v", err)), nil
	}

	// --- write gtwContext (§5.2.⑤) --------------------------------
	now := deps.Now()
	slot.Store(Context{
		Issue:     issueID,
		Branch:    branch,
		Worktree:  worktreePath,
		State:     StateFixing,
		UpdatedAt: now,
	})

	// --- render the success card (§5.2.⑥) -------------------------
	card := renderFixSuccessCard(issue, branch, worktreePath, owner+"/"+repo)
	return reply(ctx, deps.Send, chatID, messageID, card), nil
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
	card := renderBranchExistsCard(payload, existingPath)
	return sendDraft(ctx, deps, chatID, messageID, userMsgID, card, drafts, DraftFixBranchExists, payload)
}

func emitWorktreeFailDraft(
	ctx context.Context,
	deps HandlerDeps,
	chatID, messageID, userMsgID string,
	drafts DraftsMap,
	payload FixDraftPayload,
) (*Result, error) {
	card := renderWorktreeFailCard(payload)
	return sendDraft(ctx, deps, chatID, messageID, userMsgID, card, drafts, DraftFixWorktreeFail, payload)
}

func sendDraft(
	ctx context.Context,
	deps HandlerDeps,
	chatID, messageID, userMsgID, card string,
	drafts DraftsMap,
	kind DraftKind,
	payload FixDraftPayload,
) (*Result, error) {
	drafts.Store(userMsgID, &Draft{
		Kind:      kind,
		Payload:   payload,
		CreatedAt: deps.Now(),
	})
	return reply(ctx, deps.Send, chatID, messageID, card), nil
}

// reply is the single-send-and-ack helper. The reply is threaded
// under the user's /gtw fix message (ReplyTo = messageID) so the
// channel can render it as a thread reply rather than a fresh top-
// level bubble.
func reply(ctx context.Context, send SendFunc, chatID, messageID, text string) *Result {
	_ = send(ctx, OutMsg{ChatID: chatID, ReplyTo: messageID, Text: text})
	return &Result{Consumed: true}
}
