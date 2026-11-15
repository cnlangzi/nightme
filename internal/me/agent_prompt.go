package me

import (
	"context"
	"strconv"
	"strings"

	"github.com/cnlangzi/nightme/internal/gitbase"
)

// systemPrompt renders the instructions the chat model works under.
//
// The prompt is the role, the tools and the workflow: what the model
// is, what it may do and the steps it works through. Every tool
// handler enforces the policy, so a command the workflow does not
// allow comes back as an error the model can correct.
func (s *Service) systemPrompt(r *run) string {
	var b strings.Builder
	b.WriteString("You are the owner's avatar in this chat. The owner started this workflow to get something done and left you to finish it. What you run executes exactly as if the owner had typed it. Work the way they would, and interrupt them only when a decision really is theirs.\n\n")

	b.WriteString("## When you are called\n")
	b.WriteString("The workflow runs its steps by itself as long as each one succeeds. You are called when a step has finished and the work cannot go on in the normal order. The situation report says why. Get the work back on course — once it is, the workflow continues without you.\n\n")

	b.WriteString("## The workflow\n")
	b.WriteString("The title is the objective. The steps are the owner's procedure for reaching it; each note says why a step is there and which fixes the owner uses for it. Follow their order. The nightme commands are yours to run as fixes, but only a step's own run moves the work on.\n")
	b.WriteString("A step that waits sits out a duration. A wait belongs at the start of a round: it is how a repeating job backs off when there is nothing to do. A step marked as the final step cleans up after the round: it runs when the round ends, however the round ends.\n\n")

	b.WriteString("## Your tools\n")
	b.WriteString("- run_step(step_id) runs a step as the workflow wrote it: the step that is ready, or one that already ran this round.\n")
	b.WriteString("- ntm_* run nightme commands, as the owner. Only you can run them.\n")
	if s.hasBrain(r) {
		b.WriteString("- agent(task) is the owner's thinking. Ask it in plain words; it investigates and comes back with the cause and a recommendation. It changes nothing. It remembers this round's questions only, so give it the context it needs.\n")
	}
	if r.workflow.Instruct {
		b.WriteString("- instruct(text) asks the agent doing the work to change something, in plain words: fix code, resolve a conflict, commit. Never put a nightme command in it.\n")
	}
	b.WriteString("- repo_status reads the branch and the working tree. answer_choice answers a prompt a running command is waiting for.\n")
	b.WriteString("- wait(duration) waits before you look again — for work that is still running in the background.\n")
	b.WriteString("- end_round ends the round of the job the run is on: the steps of the round that did not run are skipped, the job's final step runs, and the job starts its next round.\n")
	if !r.workflow.Instruct {
		b.WriteString("\nThis workflow does not allow instructions; fix things with nightme commands.\n")
	}
	b.WriteString("\n## How to work\n")
	b.WriteString("1. Find the cause. Read the situation report: the outcome, its reason and the transcript.")
	if s.hasBrain(r) {
		b.WriteString(" When they do not explain it, ask agent.")
	}
	b.WriteString("\n")
	b.WriteString("2. Remove the cause with the smallest action that does it: a nightme command, an instruct, or running the step again when the failure looks transient.\n")
	b.WriteString("3. Run the step again with run_step. The work moves on only when the step itself succeeds; a fix alone does not count.\n")
	b.WriteString("4. When a step's result is good enough even though it did not succeed, run the ready step after it — the workflow continues from there.\n")
	b.WriteString("Each retry must change something. When the same failure comes back twice, change the approach, move on, or ask.\n")

	b.WriteString("\n## When a step produced nothing\n")
	b.WriteString("A step that ends as failed because it found nothing to do means one of two things, and the reason in the report tells you which:\n")
	b.WriteString("- Nothing is left to do. The queue is empty, the tree is clean, the branch has nothing to push. Call end_round: the steps of the round that did not run are skipped, the job's final step runs, and the job starts its next round.\n")
	b.WriteString("- The work is still running in the background. The agent the step dispatched has not finished. Call wait, then look again — do not end the round before the work is done.\n")
	b.WriteString("The workflow may offer a step that waits — that is the owner's back-off. Run it with run_step when the round should wait before it goes on.\n")
	b.WriteString("Never read a step that produced nothing as a step that succeeded.\n")

	b.WriteString("\n## When to ask the owner\n")
	b.WriteString("Use ask_human only for credentials or access you do not have, an irreversible or public action the workflow does not call for, or a product decision the evidence does not settle — or when you have run out of reasonable fixes. Give the evidence and your recommendation in the question.\n")

	b.WriteString("\n## When to finish\n")
	b.WriteString("- finish(done) when the objective is met and the last step succeeded.\n")
	b.WriteString("- finish(failed) when the objective cannot be reached; say what blocked it and what you tried.\n")
	b.WriteString("- A job that repeats until the run is stopped has no end of its own: get it back on course and let it continue.\n")

	b.WriteString("\n## Outcomes\n")
	b.WriteString("ok and noop mean the step did its job. incomplete means part of it is still open. failed means it did not work; the reason says why. cancelled means somebody stopped it; timeout means it ran out of time. unknown means nobody reported a verdict — check the facts before you trust or retry it. waiting means the command is still running and needs an answer or an instruction. error means your call was refused; the message says why.\n")
	b.WriteString("The outcome is the evidence; the transcript explains it.\n")

	b.WriteString("\nAlways act through a tool.\n")

	b.WriteString("\n")
	b.WriteString(s.renderWorkflow(r))
	return b.String()
}

// repoFacts holds the repository facts of the chat.
type repoFacts struct {
	// Branch is the branch the chat is on.
	Branch string
	// Dirty is true when the working tree has changes.
	Dirty bool
	// Upstream is the branch's tracking ref, when it has one.
	Upstream string
	// AheadOfUpstream and BehindUpstream count the commits the
	// branch is ahead of and behind its upstream.
	AheadOfUpstream int
	BehindUpstream  int
	// Base is the branch the work started from.
	Base string
	// AheadOfBase and BehindBase count the commits the branch is
	// ahead of and behind its base.
	AheadOfBase int
	BehindBase  int
}

// repoFacts reads the repository facts of a workspace.
//
// The facts come from git itself: the porcelain status carries the
// branch, the tracking ref and the ahead/behind counts, and the base
// branch is the remote's default branch.
func (s *Service) repoFacts(ctx context.Context, dir string) (repoFacts, error) {
	// `--no-optional-locks` skips git's optional index refresh: the
	// status only reads the working tree, and a refresh would take
	// the index lock.
	out, _, err := s.cfg.Git.Run(ctx, dir,
		"--no-optional-locks", "status", "--porcelain", "--branch")
	if err != nil {
		return repoFacts{}, err
	}
	facts := parsePorcelainStatus(out)

	base := s.baseBranch(ctx, dir)
	if base != "" {
		facts.Base = base
		if ahead, behind, ok := s.countDivergence(ctx, dir, base); ok {
			facts.AheadOfBase = ahead
			facts.BehindBase = behind
		}
	}
	return facts, nil
}

// parsePorcelainStatus reads the branch header of a porcelain status
// and counts the changes in the working tree.
//
// The header looks like "## branch...origin/branch [ahead 1, behind
// 2]"; every line below it is a change.
func parsePorcelainStatus(out string) repoFacts {
	var facts repoFacts
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	for i, line := range lines {
		if i == 0 {
			facts = parseBranchHeader(line)
			continue
		}
		if strings.TrimSpace(line) != "" {
			facts.Dirty = true
		}
	}
	return facts
}

// parseBranchHeader reads the branch line of a porcelain status.
func parseBranchHeader(line string) repoFacts {
	var facts repoFacts
	header := strings.TrimSpace(strings.TrimPrefix(line, "## "))
	if header == "" {
		return facts
	}
	if idx := strings.Index(header, "..."); idx >= 0 {
		facts.Branch = strings.TrimSpace(header[:idx])
		rest := header[idx+3:]
		if bracket := strings.Index(rest, "["); bracket >= 0 {
			facts.Upstream = strings.TrimSpace(rest[:bracket])
			counts := rest[bracket+1:]
			if end := strings.Index(counts, "]"); end >= 0 {
				counts = counts[:end]
			}
			for _, part := range strings.Split(counts, ",") {
				fields := strings.Fields(strings.TrimSpace(part))
				if len(fields) != 2 {
					continue
				}
				n, err := strconv.Atoi(fields[1])
				if err != nil {
					continue
				}
				switch fields[0] {
				case "ahead":
					facts.AheadOfUpstream = n
				case "behind":
					facts.BehindUpstream = n
				}
			}
		}
		return facts
	}
	facts.Branch = header
	return facts
}

// baseBranch returns the remote's default branch, or an empty string
// when the remote has none.
//
// The name carries its remote: the counts are against the branch as
// the remote has it, not against a local branch of the same name.
//
// The resolver is read-only here — /me reports facts about a
// repository, it must not modify one.
func (s *Service) baseBranch(ctx context.Context, dir string) string {
	res := gitbase.Resolve(ctx, dir, gitbase.Options{Repair: false})
	if res.Branch == "" {
		return ""
	}
	return "origin/" + res.Branch
}

// countDivergence counts the commits the branch is ahead of and
// behind another ref.
func (s *Service) countDivergence(ctx context.Context, dir, base string) (ahead, behind int, ok bool) {
	out, _, err := s.cfg.Git.Run(ctx, dir, "rev-list", "--left-right", "--count", base+"...HEAD")
	if err != nil {
		return 0, 0, false
	}
	fields := strings.Fields(strings.TrimSpace(out))
	if len(fields) != 2 {
		return 0, 0, false
	}
	left, err := strconv.Atoi(fields[0])
	if err != nil {
		return 0, 0, false
	}
	right, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, 0, false
	}
	// The left count is what the other ref has that HEAD does not.
	return right, left, true
}
