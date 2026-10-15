package gtw

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
	"github.com/cnlangzi/nightme/internal/nightmedir"
	"github.com/cnlangzi/nightme/internal/pathutil"
)

// issueDraftDirName is the per-user directory under
// $HOME/.nightme where /gtw new drafts land. Per-user rather than
// per-cwd: a draft inside the repo would show up as an untracked
// file and trip the dirty-worktree gates of /gtw sync and /gtw fix.
const issueDraftDirName = "issue-drafts"

const placeholderDraftFileAbs = "{{DRAFT_FILE_ABS}}"

// newIssueArgs is the parsed argv tail of `/gtw new`.
type newIssueArgs struct {
	Yes bool // -y / --yes: create the issue from the reviewed draft
}

var newCmdSpec = command.CmdSpec{
	Name:  "/gtw new",
	Usage: "/gtw new [-y|--yes]",
	Flags: map[string]command.FlagSpec{
		"-y":    {Name: "yes"},
		"--yes": {Name: "yes"},
	},
	MinArgs: 0,
	MaxArgs: 0,
}

func parseNewIssueArgs(argv []string) (newIssueArgs, error) {
	parsed, err := command.ParseCmdArgs(argv, newCmdSpec)
	if err != nil {
		return newIssueArgs{}, err
	}
	return newIssueArgs{Yes: parsed.Bool("yes")}, nil
}

// RunNewDraft is the first half of /gtw new: it queues the archive
// prompt into the chat's CURRENT agent session — not a one-shot
// agent like /gtw commit or /gtw pr — because the discussion only
// exists in that session's context. The agent either asks its
// blocking questions or writes the chat's draft file and shows the
// full document in chat. The command returns as soon as the prompt
// is queued; it never waits on the agent.
//
// Any previous draft of this chat is removed first so that a run
// ending in questions cannot leave an older document behind for
// /gtw new -y to submit.
func RunNewDraft(
	ctx context.Context,
	cs *chatsession.ChatSession,
	deps HandlerDeps,
	chatID, messageID string,
) (*Result, error) {
	if deps.Now == nil {
		deps.Now = timeNow
	}
	if messageID == "" {
		return reply(ctx, cs.Emitter(), chatID, messageID,
			"❌ internal: missing message id; /gtw new did not enqueue"), nil
	}
	cwd := cs.SelectedCwd()

	// Resolve the provider up front: finding out that the repo has
	// no usable origin only at /gtw new -y would waste the whole
	// archive turn.
	repoRoot, err := RepoRoot(ctx, cwd, deps.Git)
	if err != nil {
		return reply(ctx, cs.Emitter(), chatID, messageID,
			"❌ Not in a git repository. Run /cwd <inside a repo> first."), nil
	}
	if _, _, _, err := resolveProvider(ctx, Context{RepoRoot: repoRoot, Worktree: cwd}, deps); err != nil {
		return reply(ctx, cs.Emitter(), chatID, messageID, "❌ "+err.Error()), nil
	}

	draftPath, err := issueDraftPath(chatID)
	if err != nil {
		return reply(ctx, cs.Emitter(), chatID, messageID,
			fmt.Sprintf("❌ prepare issue draft: %v", err)), nil
	}
	if err := resetIssueDraft(draftPath); err != nil {
		return reply(ctx, cs.Emitter(), chatID, messageID,
			fmt.Sprintf("❌ prepare issue draft: %v", err)), nil
	}

	blocks := []agent.ContentBlock{{Type: agent.ContentText, Text: renderNewIssuePrompt(draftPath)}}
	if err := dispatchIssueToChatSession(ctx, cs, deps.Now, chatID, messageID, blocks); err != nil {
		return reply(ctx, cs.Emitter(), chatID, messageID,
			fmt.Sprintf("❌ dispatch to agent: %v\nhint: /gtw new archives the discussion held with the current agent", err)), nil
	}
	return &Result{Consumed: true}, nil
}

// RunNewCreate is the second half of /gtw new: after the user has
// reviewed the draft in chat, `/gtw new -y` creates the issue from
// the draft file. No agent is involved — whether and when the issue
// is created is the user's call, and what gets submitted is exactly
// the file the agent wrote.
func RunNewCreate(
	ctx context.Context,
	cs *chatsession.ChatSession,
	deps HandlerDeps,
	chatID, messageID string,
) (*Result, error) {
	draftPath, err := issueDraftPath(chatID)
	if err != nil {
		return reply(ctx, cs.Emitter(), chatID, messageID,
			fmt.Sprintf("❌ locate issue draft: %v", err)), nil
	}
	cwd := cs.SelectedCwd()
	repoRoot, err := RepoRoot(ctx, cwd, deps.Git)
	if err != nil {
		return reply(ctx, cs.Emitter(), chatID, messageID,
			"❌ Not in a git repository. Run /cwd <inside a repo> first."), nil
	}
	resolve := func() (GitProvider, string, string, error) {
		return resolveProvider(ctx, Context{RepoRoot: repoRoot, Worktree: cwd}, deps)
	}
	return reply(ctx, cs.Emitter(), chatID, messageID,
		createIssueFromDraft(ctx, draftPath, resolve)), nil
}

// createIssueFromDraft turns the draft into an issue and returns
// the reply text. The draft is removed once the issue exists and
// kept on failure so the user can retry or create the issue by hand.
func createIssueFromDraft(ctx context.Context, draftPath string, resolve func() (GitProvider, string, string, error)) string {
	raw, err := os.ReadFile(draftPath)
	if errors.Is(err, os.ErrNotExist) {
		return "❌ no issue draft for this chat\n" +
			"↳ run `/gtw new` first — the agent writes the draft once its questions are answered"
	}
	if err != nil {
		return fmt.Sprintf("❌ read issue draft %s: %v", draftPath, err)
	}

	title, body, err := parseIssueDraft(string(raw))
	if err != nil {
		return fmt.Sprintf("❌ invalid issue draft %s: %v\n↳ ask the agent to fix the draft, then `/gtw new -y` again", draftPath, err)
	}

	provider, owner, repo, err := resolve()
	if err != nil {
		return "❌ " + err.Error()
	}
	issue, err := provider.CreateIssue(ctx, owner, repo, title, body)
	if err != nil {
		return fmt.Sprintf("❌ create issue: %v\n→ draft: %s (kept)\n↳ `/gtw new -y` to retry", err, draftPath)
	}
	_ = os.Remove(draftPath)
	return renderIssueOpenedCard(owner, repo, issue)
}

// parseIssueDraft splits a draft into title and body. The first
// non-blank line must be a level-1 heading (`# <title>`); the rest
// of the file is the issue body.
func parseIssueDraft(raw string) (title, body string, err error) {
	text := strings.TrimPrefix(strings.ReplaceAll(raw, "\r\n", "\n"), "\ufeff")
	text = strings.TrimLeft(text, " \t\n")
	first, rest, _ := strings.Cut(text, "\n")
	first = strings.TrimSpace(first)
	if !strings.HasPrefix(first, "# ") {
		return "", "", errors.New("first line must be `# <issue title>`")
	}
	title = strings.TrimSpace(strings.TrimPrefix(first, "# "))
	if title == "" {
		return "", "", errors.New("issue title is empty")
	}
	body = strings.TrimSpace(rest)
	if body == "" {
		return "", "", errors.New("issue body is empty")
	}
	return title, body + "\n", nil
}

func renderIssueOpenedCard(owner, repo string, issue *Issue) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "✅ Issue #%d opened\n", issue.ID)
	fmt.Fprintf(&sb, "→ title: %s\n", issue.Title)
	fmt.Fprintf(&sb, "→ repo:  %s/%s\n", owner, repo)
	if issue.URL != "" {
		fmt.Fprintf(&sb, "→ url:   %s\n", issue.URL)
	}
	fmt.Fprintf(&sb, "↳ `/gtw fix %d` to plan it · or `/gtw fix %d -y` to implement directly", issue.ID, issue.ID)
	return sb.String()
}

// issueDraftPath returns the chat's draft file:
// $HOME/.nightme/issue-drafts/issue-<hash(chatID)>.md. One draft per
// chat — a chat holds one discussion at a time — and the name is
// stable so /gtw new -y finds what /gtw new asked the agent to
// write. Chat IDs are hashed because channel IDs may contain
// characters that are not safe in file names.
func issueDraftPath(chatID string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home: %w", err)
	}
	sum := sha256.Sum256([]byte(chatID))
	name := "issue-" + hex.EncodeToString(sum[:8]) + ".md"
	return pathutil.Join(home, nightmedir.DirName, issueDraftDirName, name), nil
}

// resetIssueDraft creates the draft directory and removes any
// existing draft at path.
func resetIssueDraft(path string) error {
	if err := os.MkdirAll(pathutil.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func renderNewIssuePrompt(draftPath string) string {
	return strings.ReplaceAll(newIssuePrompt, placeholderDraftFileAbs, draftPath)
}

// newIssuePrompt is the archive task queued into the current agent
// session. It runs inside the user's own agent, which cannot see
// this repository's docs, so it is fully self-contained.
const newIssuePrompt = `/gtw new — archive the requirement discussion in THIS conversation into an issue draft.

The issue will be handed to a developer or a coding agent that has NOT seen this conversation and can only read the issue. It must be implementable from the issue alone.

Draft file (absolute path):
{{DRAFT_FILE_ABS}}

The user reviews the draft in chat; when they approve, they run /gtw new -y and nightme creates the issue from that file. Do NOT run gh / glab or create the issue yourself. Do NOT modify any file other than the draft file.

## Source of truth
- Use only this conversation, plus code and files you have actually read in it. You may read more of the repository to resolve names, paths, and interfaces precisely.
- If the beginning of the discussion is no longer visible to you, say so in chat and stop without writing the draft.
- Never invent requirements, decisions, numbers, interfaces, or file paths. When completeness forces you to add something the discussion did not state, mark it inline with [inferred].

## Workflow
1. Extract (do not print). List every meaningful point of the discussion as an atomic, verifiable statement, typed as: GOAL, REQUIREMENT, CONSTRAINT, DECISION (with rationale), REJECTED (with reason), EDGE CASE, INTERFACE (API, data shape, config key, command, path), NON-GOAL, OPEN QUESTION, TERM. Note for each whether it is final, superseded (by which point), or uncertain.
2. Reconcile (do not print). For conflicting points, the later and more explicit statement wins. If a later remark was casual and contradicts an explicit earlier decision, do not decide — turn it into an open question. A superseded DECISION becomes REJECTED and keeps its reason. Also list implicit assumptions you had to make and the information a developer would ask for before starting.
3. Confirm. If step 2 produced questions whose answers change scope, behavior, interfaces, or acceptance criteria, ask them now: one chat message, numbered, each with your recommended default and a one-line reason. Then STOP — do not write the draft file in this turn, and end with: "Answer the questions above, then run /gtw new again."
   If this conversation already contains your earlier /gtw new questions and the user's answers, use those answers and do not ask again. Questions that do not block implementation are not asked — they go under "Open Questions". If nothing is blocking, continue.
4. Write the draft file following the template below.
5. Review the draft against the conversation and fix it before finishing: requirements, constraints, or edge cases that were agreed but are missing; statements that differ from what was agreed; content with no basis in the discussion and no [inferred] mark; rejected options appearing anywhere except "Decisions"; items a developer could not act on (missing parameters, ambiguous interfaces, unverifiable acceptance criteria).
6. Show the draft: print the complete content of the draft file in chat, verbatim, then end with: "Reply with changes, or run /gtw new -y to create the issue."

If the user asks for changes later in this conversation, rewrite the draft file, then show the complete updated content again the same way.

## Writing rules
- Describe what the system must be, not how the discussion went. Never write phrases like "we discussed", "initially", "later changed to", "as requested".
- Every requirement is verifiable. Quantify vague words ("fast", "friendly", "reasonable") or drop them.
- Be concrete: field names, types, defaults, error messages, file paths, commands, config keys — verbatim when they appeared in the discussion or code.
- Define each term on first use and use it consistently.
- Write the body in the language the user predominantly used. Keep the section headings below as given.
- Write "None" under a section that has no entries.

## Template
The file starts with the title line. The title follows Conventional Commits — "<type>(<scope>): <summary>", type and scope in lowercase English, summary in English, at most 72 characters — so /gtw fix can derive a readable branch name from it.

# <type>(<scope>): <summary>

## Background & Goal
<the problem, who it affects, why it matters, and what success looks like>

## Scope
### In scope
- <item>
### Out of scope
- <item>

## Glossary
- <term>: <definition>

## Requirements
- R1: <requirement>
  - Acceptance: <Given / When / Then, or an executable check>

## Design
<approach, components and responsibilities, key flows (Mermaid if it helps), data structures, interfaces, configuration>

## Edge Cases & Error Handling
| Scenario | Expected behavior |
|---|---|
| <scenario> | <behavior> |

## Constraints
- <technical, compatibility, performance, security constraints; things that must not change>

## Decisions
- <decision> — why: <rationale> — rejected: <alternative> (<reason>)

## Implementation Plan
1. <independently verifiable task> — done when: <check> — depends on: <task numbers or none>

## Open Questions
- <question> — impact: <what it affects> — interim: <how to proceed until answered>`
