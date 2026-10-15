package gtw

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
	"github.com/cnlangzi/nightme/internal/nightmedir"
	"github.com/cnlangzi/nightme/internal/pathutil"
	"github.com/cnlangzi/nightme/internal/timeouts"
)

// issueDraftDirName is the per-user directory under
// $HOME/.nightme where /gtw new drafts land. Per-user rather than
// per-cwd: a draft inside the repo would show up as an untracked
// file and trip the dirty-worktree gates of /gtw sync and /gtw fix.
const issueDraftDirName = "issue-drafts"

const placeholderDraftFileAbs = "{{DRAFT_FILE_ABS}}"
const placeholderConfirmRule = "{{CONFIRM_RULE}}"

// newIssueArgs is the parsed argv tail of `/gtw new`.
type newIssueArgs struct {
	Yes bool // -y / --yes: skip the confirmation stop, take recommended defaults
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

// RunNew archives the requirement discussion held in the chat's
// current agent session into a new GitHub / GitLab issue.
//
// The archive prompt is queued into the CURRENT agent session —
// not a one-shot agent like /gtw commit or /gtw pr — because the
// discussion only exists in that session's context. The agent
// writes the finished document to a per-invocation draft file;
// when its turn ends nightme reads the draft and creates the issue
// through the provider, so issue creation never depends on the
// agent driving gh / glab correctly.
//
// A turn that ends without a draft means the agent stopped to ask
// confirmation questions: the user answers in chat and re-runs
// /gtw new (the prompt tells the agent to reuse those answers), or
// runs /gtw new -y to accept the agent's recommended defaults.
func RunNew(
	ctx context.Context,
	cs *chatsession.ChatSession,
	deps HandlerDeps,
	chatID, messageID string,
	args newIssueArgs,
) (*Result, error) {
	if deps.Now == nil {
		deps.Now = timeNow
	}
	if messageID == "" {
		return reply(ctx, cs.Emitter(), chatID, messageID,
			"❌ internal: missing message id; /gtw new did not enqueue"), nil
	}
	cwd := cs.SelectedCwd()
	if _, err := cs.LookupSelectedAgentSession(); err != nil {
		return reply(ctx, cs.Emitter(), chatID, messageID,
			"❌ "+err.Error()+"\nhint: /gtw new archives the discussion held with the current agent — `/use <agent>` first"), nil
	}

	repoRoot, err := RepoRoot(ctx, cwd, deps.Git)
	if err != nil {
		return reply(ctx, cs.Emitter(), chatID, messageID,
			"❌ Not in a git repository. Run /cwd <inside a repo> first."), nil
	}
	provider, owner, repo, err := resolveProvider(ctx, Context{RepoRoot: repoRoot, Worktree: cwd}, deps)
	if err != nil {
		return reply(ctx, cs.Emitter(), chatID, messageID, "❌ "+err.Error()), nil
	}

	draftPath, err := prepareIssueDraftPath(deps.Now())
	if err != nil {
		return reply(ctx, cs.Emitter(), chatID, messageID,
			fmt.Sprintf("❌ prepare issue draft: %v", err)), nil
	}

	ended, unsub, err := subscribePromptEnd(cs, messageID)
	if err != nil {
		return reply(ctx, cs.Emitter(), chatID, messageID, "❌ "+err.Error()), nil
	}
	defer unsub()

	blocks := []agent.ContentBlock{{Type: agent.ContentText, Text: renderNewIssuePrompt(draftPath, args.Yes)}}
	if err := dispatchIssueToChatSession(ctx, cs, deps.Now, chatID, messageID, blocks); err != nil {
		return reply(ctx, cs.Emitter(), chatID, messageID,
			fmt.Sprintf("❌ dispatch to agent: %v", err)), nil
	}

	if err := awaitPromptEnd(ctx, ended, timeouts.Agent); err != nil {
		return reply(ctx, cs.Emitter(), chatID, messageID,
			fmt.Sprintf("❌ /gtw new: %v", err)), nil
	}
	return reply(ctx, cs.Emitter(), chatID, messageID,
		finishNewIssue(ctx, provider, owner, repo, draftPath, args.Yes)), nil
}

// finishNewIssue turns the agent's draft into an issue and returns
// the reply text. The draft is removed once the issue exists and
// kept on failure so the user can create the issue by hand.
func finishNewIssue(ctx context.Context, provider GitProvider, owner, repo, draftPath string, yes bool) string {
	raw, err := os.ReadFile(draftPath)
	if errors.Is(err, os.ErrNotExist) {
		if yes {
			return "❌ agent finished without writing the issue draft\n↳ re-run `/gtw new -y`"
		}
		return "🤖 agent is waiting for your answers\n" +
			"↳ answer in chat, then `/gtw new` again · or `/gtw new -y` to accept its recommended defaults"
	}
	if err != nil {
		return fmt.Sprintf("❌ read issue draft %s: %v", draftPath, err)
	}

	title, body, err := parseIssueDraft(string(raw))
	if err != nil {
		return fmt.Sprintf("❌ invalid issue draft %s: %v\n↳ fix the file and create the issue manually, or re-run `/gtw new`", draftPath, err)
	}

	issue, err := provider.CreateIssue(ctx, owner, repo, title, body)
	if err != nil {
		return fmt.Sprintf("❌ create issue: %v\n→ draft: %s (kept)", err, draftPath)
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

// prepareIssueDraftPath creates $HOME/.nightme/issue-drafts and
// returns a fresh per-invocation draft path in it. A fresh name
// guarantees that a file found after the turn was written by this
// run, not left over from an earlier one.
func prepareIssueDraftPath(now time.Time) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home: %w", err)
	}
	dir := pathutil.Join(home, nightmedir.DirName, issueDraftDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	p := pathutil.Join(dir, "issue-"+now.Format("20060102-150405.000000000")+".md")
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	return p, nil
}

// subscribePromptEnd registers for the PromptEnded event of
// userMsgID. Subscribing before the message is queued closes the
// window where a fast turn ends before anyone listens.
func subscribePromptEnd(cs *chatsession.ChatSession, userMsgID string) (<-chan chatsession.PromptEndedEvent, func(), error) {
	if cs == nil || cs.PromptEndBus == nil {
		return nil, nil, errors.New("PromptEndBus unavailable")
	}
	ch := make(chan chatsession.PromptEndedEvent, 1)
	unsub := cs.PromptEndBus.Subscribe(func(e chatsession.PromptEndedEvent) bool {
		if e.UserMsgID != userMsgID {
			return false
		}
		select {
		case ch <- e:
		default:
		}
		return true
	})
	return ch, unsub, nil
}

func awaitPromptEnd(ctx context.Context, ended <-chan chatsession.PromptEndedEvent, timeout time.Duration) error {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case ev := <-ended:
		if ev.Reason != agent.PromptEndClean {
			return fmt.Errorf("agent turn ended with reason %s", ev.Reason)
		}
		return nil
	case <-timer.C:
		return fmt.Errorf("agent did not finish within %s", timeout)
	}
}

func renderNewIssuePrompt(draftPath string, yes bool) string {
	rule := confirmRuleAsk
	if yes {
		rule = confirmRuleDefaults
	}
	out := strings.ReplaceAll(newIssuePrompt, placeholderConfirmRule, rule)
	return strings.ReplaceAll(out, placeholderDraftFileAbs, draftPath)
}

const confirmRuleAsk = `If step 2 produced questions whose answers change scope, behavior, interfaces, or acceptance criteria, ask them now: one chat message, numbered, each with your recommended default and a one-line reason. Then STOP — do not write the draft file in this turn. The user will answer and run /gtw new again; on that run, use the answers given in this conversation and do not ask the same questions again.
Questions that do not block implementation are not asked — they go under "Open Questions".
If there is nothing blocking, skip this step.`

const confirmRuleDefaults = `Do not ask the user anything. For every question from step 2, adopt your recommended default and record it under "Open Questions" as "<question> — assumed: <default>" so the user can review it in the issue.`

// newIssuePrompt is the archive task queued into the current agent
// session. It runs inside the user's own agent, which cannot see
// this repository's docs, so it is fully self-contained.
const newIssuePrompt = `/gtw new — archive the requirement discussion in THIS conversation into an issue.

The issue will be handed to a developer or a coding agent that has NOT seen this conversation and can only read the issue. It must be implementable from the issue alone.

Draft file (absolute path):
{{DRAFT_FILE_ABS}}

nightme creates the issue from that file after your turn ends. Do NOT run gh / glab or create the issue yourself. Do NOT modify any file other than the draft file.

## Source of truth
- Use only this conversation, plus code and files you have actually read in it. You may read more of the repository to resolve names, paths, and interfaces precisely.
- If the beginning of the discussion is no longer visible to you, say so in chat and stop without writing the draft.
- Never invent requirements, decisions, numbers, interfaces, or file paths. When completeness forces you to add something the discussion did not state, mark it inline with [inferred].

## Workflow
1. Extract (do not print). List every meaningful point of the discussion as an atomic, verifiable statement, typed as: GOAL, REQUIREMENT, CONSTRAINT, DECISION (with rationale), REJECTED (with reason), EDGE CASE, INTERFACE (API, data shape, config key, command, path), NON-GOAL, OPEN QUESTION, TERM. Note for each whether it is final, superseded (by which point), or uncertain.
2. Reconcile (do not print). For conflicting points, the later and more explicit statement wins. If a later remark was casual and contradicts an explicit earlier decision, do not decide — turn it into an open question. A superseded DECISION becomes REJECTED and keeps its reason. Also list implicit assumptions you had to make and the information a developer would ask for before starting.
3. Confirm.
{{CONFIRM_RULE}}
4. Write the draft file following the template below.
5. Review the draft against the conversation and fix it before finishing: requirements, constraints, or edge cases that were agreed but are missing; statements that differ from what was agreed; content with no basis in the discussion and no [inferred] mark; rejected options appearing anywhere except "Decisions"; items a developer could not act on (missing parameters, ambiguous interfaces, unverifiable acceptance criteria).

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
- <question> — impact: <what it affects> — interim: <how to proceed until answered>

## Output
After writing the draft file, reply in chat with a single line: "Issue draft written: <title>". Do not print the document.
If you stopped in step 3 to ask questions, end with: "Answer the questions above, then run /gtw new again."`
