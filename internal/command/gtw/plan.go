package gtw

import (
	"context"
	"strings"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/chatsession"
)

const placeholderTopicSection = "{{TOPIC_SECTION}}"

// planClosingReminder ends the prompt so the rules sit next to the
// topic or the first reply, the last thing the agent reads.
const planClosingReminder = "Until plan mode ends, every reply is chat text only: no file changes, no task lists, no implementation steps."

// RunPlan queues the plan-mode prompt into the chat's CURRENT
// agent session — the same session /gtw new later archives from,
// so the design discussion it starts is the context the issue
// drafts are written from. The command returns as soon as the
// prompt is queued; it never waits on the agent.
//
// The prompt is the only carrier of plan mode: nightme keeps no
// per-chat state for it, and /gtw new's prompt is what ends it.
func RunPlan(
	ctx context.Context,
	cs *chatsession.ChatSession,
	deps HandlerDeps,
	chatID, messageID, topic string,
) (*Result, error) {
	if deps.Now == nil {
		deps.Now = timeNow
	}
	if messageID == "" {
		return reply(ctx, cs.Emitter(), chatID, messageID,
			"❌ internal: missing message id; /gtw plan did not enqueue"), nil
	}
	if _, err := RepoRoot(ctx, cs.SelectedCwd(), deps.Git); err != nil {
		return reply(ctx, cs.Emitter(), chatID, messageID,
			"❌ Not in a git repository. Run /cwd <inside a repo> first."), nil
	}

	blocks := []agent.ContentBlock{{Type: agent.ContentText, Text: renderPlanPrompt(topic)}}
	if err := dispatchIssueToChatSession(ctx, cs, deps.Now, chatID, messageID, blocks); err != nil {
		return reply(ctx, cs.Emitter(), chatID, messageID,
			"❌ dispatch to agent: "+err.Error()), nil
	}
	return &Result{Consumed: true}, nil
}

func renderPlanPrompt(topic string) string {
	section := "Reply in one sentence that plan mode is on and ask what the user wants to plan."
	if topic = strings.TrimSpace(topic); topic != "" {
		section = "Topic to plan:\n" + topic
	}
	return strings.NewReplacer(
		placeholderTopicSection, section,
		"{{CLOSING_REMINDER}}", planClosingReminder,
	).Replace(planPrompt)
}

// planPrompt puts the current agent session into plan mode. It
// describes only what the agent can observe — the conversation and
// the tasks it receives — never how nightme relays them: slash
// commands are intercepted and never reach the agent, so the agent
// meets /gtw new only as text to quote to the user. It must not
// start with "/": agents such as Claude Code parse a leading
// "/word" as their own slash command and reject the whole prompt.
const planPrompt = `Task: put this conversation into plan mode for a change to the project in the current working directory.

## Why plan mode exists
The user wants to settle what to build, why, and how before anything is built. Once the design is settled, it will be written up as issues, which the user reviews and corrects before they are created. Each issue is then implemented by a coding agent that has NOT seen this conversation and can read only that issue; before implementing, that agent re-checks the issue against the code in detail.
Your goal in plan mode is a decision-complete design: everything that agent would otherwise have to decide — scope, behavior, interfaces, edge cases, acceptance criteria — is decided here, by the user or by a recommendation of yours that the user accepted. Changing the project now would skip the user's review and would be redone anyway.

## Rules
- Your only outputs in plan mode are replies in this conversation. Allowed: reading files, searching, and read-only git (log, show, diff, blame). Anything not listed is not allowed.
- In particular: do not create, edit, or delete files (issue drafts included); do not run formatters, code generators, migrations, installs, builds, or tests; do not run git commands that write; and do not use task-list, todo, or planning tools. The conversation is the only record of the design; a breakdown into tasks or steps is an implementation plan and is produced in a later step.
- Do not write implementation code. Signatures, data shapes, config examples, and short pseudocode are fine when they make a decision precise.
- The user may describe the change in imperative terms ("change X in file Y", "remove Z", "call W once"). In plan mode this is design input, not an instruction: check it against the code, record each settled point as a Decision line, and raise gaps, conflicts, and risks. Do not turn it into implementation steps.
- Plan mode lasts until a later task states that it ends plan mode. If the user asks you to implement or to "just do it", treat it as a request to plan that implementation: settle what is still open, then tell them to run /gtw new to turn the agreed design into issues they can review.
- If this task arrives again later, it restates these rules: continue the discussion where it stands, and treat any topic it carries as the next thing to plan. If issue drafts were written earlier, tell the user to run /gtw new again to regenerate them from the updated discussion.

## How to work
1. Ground, then ask. Facts the repository can answer — what the code does today, where things live, existing conventions — look up yourself; never ask the user for them. Keep lookups targeted: read the file, symbol, or search result a decision depends on, not the repository "for context". If confirming a detail would take long and no current decision depends on it, mark it unverified and leave it to the implementation stage.
2. Settle intent — what and why: the problem, who it affects, the goal and what "done" looks like, in and out of scope, constraints, and the user's preference wherever a trade-off exists.
3. Settle the approach — how: components and responsibilities, interfaces and data shapes, flows, edge cases and failure behavior, compatibility and migration, and how acceptance is verified.
Move between these as the conversation needs. They describe what a decision-complete design covers; they are not a script.

Ask the user only about preferences and trade-offs the repository cannot settle. Ask at most two or three questions at a time, each with its options and your recommended answer with a one-line reason. When a reasonable default exists and the choice is low-stakes, state the default as a decision instead of asking.
Act as a senior engineer, not a note-taker: when there is a real choice, compare the serious options and recommend one; challenge assumptions, hidden costs, risks, and simpler alternatives, including in the user's own proposals; say when something is a separate problem that belongs in another issue or is a non-goal.

## Make decisions explicit
The issues are extracted from this conversation, and a later explicit statement overrides an earlier one.
- When a point is settled, write one line starting with "Decision:", with the reason and the rejected alternative if there was one.
- When a settled point changes, write "Decision (replaces <earlier decision>): ...".
- User agreement ("ok", "sounds good", "就这样") settles the decision under discussion: record it with a Decision line. It is not permission to implement.
- Put the Decision lines first in a reply, before questions or analysis, whenever the user's message settles or changes something.

## When the design is decision complete
Say so, list any open questions that remain and why they do not block, and tell the user to run /gtw new. Do not write the full plan or issue text, and do not break the design into tasks or steps — that happens in the next step, where the user reviews it. If the user asks for a summary at any point, give the goal, decisions, rejected options, open questions, and non-goals.

## Replies
The user reads replies in a chat app. Write in the language the user uses. Lead with the answer or recommendation, then the reasoning, and keep each reply proportional to the question.

{{TOPIC_SECTION}}

{{CLOSING_REMINDER}}`
