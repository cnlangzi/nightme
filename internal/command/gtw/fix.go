package gtw

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Sender is the full outbound surface gtw uses. Defined in
// types.go (canonical location for shared types); the gtw
// package uses gtw.Sender everywhere instead of the previous
// minimal ActiveCwd/SetActiveCwd subset.

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

	// F-50 §5.7 + F-45 reaction ingest: daemon-recovery. If the
	// in-memory gtwContext was lost (daemon restart), the cwd may
	// still be inside a worktree holding a `fix/<id>-*` branch.
	// Rebuild it transparently so the user doesn't lose their
	// fix state. F-50 review fix: forward the caller's prober to
	// avoid a second ExecHTTPProber instantiation in Detect.
	//
	// The prober is created here (before the early rebuild call)
	// so both the rebuild path and the main Detect path share the
	// same instance — never two independent timeouts.
	prober := deps.Prober
	if prober == nil {
		prober = &ExecHTTPProber{}
	}
	if rebuilt := RebuildContext(ctx, cs, deps.Git, deps.Detect, prober); rebuilt != (Context{}) {
		slot.Store(rebuilt)
		_ = reply(ctx, deps.Send, chatID, messageID, fmt.Sprintf(
			"♻️ Recovered /gtw fix #%d (state: %s) after daemon restart.\n  branch: %s\n  worktree: %s\n  Continue working in the worktree, or `/cwd` back to the repo before starting a new fix.",
			rebuilt.Issue, rebuilt.State, rebuilt.Branch, rebuilt.Worktree))
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
	// Two-stage provider detection (F-50 §1.2): URL hint first
	// (zero network), API endpoint probe fallback for self-hosted
	// GitHub Enterprise / GitLab on custom domains. Returns a
	// GitProvider already bound to host + version. Tests override
	// via deps.Detect to inject a fakeProvider.
	detect := deps.Detect
	if detect == nil {
		detect = Detect
	}
	// prober is the same instance created earlier for
	// RebuildContext — reused here so the two Detect calls
	// (rebuild + main) share the 3s timeout budget rather
	// than doubling.
	provider, err := detect(ctx, remoteURL, prober)
	if err != nil {
		// D3 split: distinguish "URL is malformed" (user error —
		// the remote URL itself doesn't parse) from "host not
		// recognised as GitHub/GitLab" (no provider implementation
		// for that host yet). The two need different remediation
		// hints in the IM reply.
		//
		// Security: never echo the raw remoteURL — it may carry
		// userinfo (PAT / oauth2:token) that would leak to the
		// IM channel. We use redactForDisplay() which strips
		// credentials + caps length. If redaction fails (truly
		// unparseable input), we fall back to a generic hint
		// without any URL echo at all.
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
			// ErrUnsupportedProvider (or wrapped): both stages
			// failed. URL hint did not match github.com / gitlab.com,
			// AND the API probe (when Stage A was ambiguous) did
			// not recognise the host either. Self-hosted GitHub
			// Enterprise / GitLab instances are tried first via
			// /api/v3/meta or /api/v4/version; only true unknowns
			// (Bitbucket / Gitea — not yet supported in v1) land here.
			return reply(ctx, deps.Send, chatID, messageID,
				fmt.Sprintf("❌ 暂不支持的 Git 平台 (host: %s — neither github.com/gitlab.com URL hint nor /api/v3/meta or /api/v4/version probe recognised it).", redacted)), nil
		}
	}
	// Custom deps.Detect may legally return (nil, nil) (e.g. test
	// fakes for negative paths). Production Detect never does; the
	// guard prevents a panic in the .Kind() call below.
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

	// --- fetch issue + derive branch + slug (§5.2.②) -------------
	issue, err := provider.GetIssue(ctx, owner, repo, issueID)
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
			Provider: string(providerKind),
			ChatID:   chatID,
		}, existingPath)
	}

	// --- label the issue (§5.2.② cont.) ---------------------------
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

	// --- create the worktree (§5.2.③) -----------------------------
	if err := WorktreeAdd(ctx, repoRoot, branch, worktreePath, "HEAD", deps.Git); err != nil {
		// Roll back the label we just added (§5.3.3 §5.4).
		// We use the caller's ctx (not context.Background()) so
		// a /kill or client disconnect can abort the rollback
		// the same way it would the original AddLabel. The
		// pre-fix version detached the rollback from caller
		// cancellation, which could leak a slow `glab` child
		// past daemon shutdown.
		if labelAdded {
			_ = provider.RemoveLabel(ctx, owner, repo, issueID, LabelWIP)
		}
		return emitWorktreeFailDraft(ctx, deps, chatID, messageID, messageID, drafts, FixDraftPayload{
			IssueID:    issueID,
			Title:      issue.Title,
			Branch:     branch,
			Slug:       worktreeSlug,
			Repo:       owner + "/" + repo,
			Provider:   string(providerKind),
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
func reply(ctx context.Context, send SendFunc, chatID, messageID, text string) *Result {
	_ = send(ctx, OutMsg{ChatID: chatID, ReplyTo: messageID, Text: text})
	return &Result{Consumed: true}
}
