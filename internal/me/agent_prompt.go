package me

import (
	"context"
	"strconv"
	"strings"

	"github.com/cnlangzi/nightme/internal/gitbase"
)

// systemPrompt renders the instructions the chat model works under.
//
// The prompt is the workflow: the goal, the steps, the notes and the
// limits. The model acts through the tools, and every tool handler
// enforces the policy, so a command the workflow does not allow comes
// back as an error the model can correct.
func (s *Service) systemPrompt(r *run) string {
	var b strings.Builder
	b.WriteString("You are the operator for the user. You run the workflow below on their behalf, one step at a time, and you report what happened.\n\n")
	b.WriteString(r.renderPolicy())
	b.WriteString("\nHow to read an outcome:\n")
	b.WriteString("- ok and noop mean the step did what it was asked to do.\n")
	b.WriteString("- incomplete means it partly worked; the rest is still open.\n")
	b.WriteString("- failed means it did not work.\n")
	b.WriteString("- cancelled means somebody stopped it; timeout means it ran out of time.\n")
	b.WriteString("- unknown means nobody reported a verdict — ask before you continue.\n")
	b.WriteString("- waiting means the step is still running and needs an answer, an instruction or a question.\n")
	b.WriteString("- error means the call itself was refused — the command is not in the workflow, or the workflow does not allow it. Read the message and call a tool the workflow allows.\n")
	b.WriteString("The status is the evidence. The transcript explains the status; it does not replace it.\n")
	b.WriteString("\nHow to work:\n")
	b.WriteString("- Use run_step for the workflow's commands. A command the workflow does not allow comes back as an error — do not run it again.\n")
	b.WriteString("- Use instruct for the work the commands do not cover, when the workflow allows it.\n")
	b.WriteString("- Use answer_choice when an outcome says a prompt is waiting.\n")
	b.WriteString("- Use ask_human when the next step needs the user's judgement or their credentials.\n")
	b.WriteString("- Use repo_status to read the branch and the working tree before you act on them.\n")
	b.WriteString("- Call finish when the workflow is done. A done verdict is only accepted after the last step succeeded.\n")
	b.WriteString("- Always call a tool. An answer without a tool call does not move the run forward.\n")
	return b.String()
}

// repoFacts holds the repository facts of the work chat.
type repoFacts struct {
	// Branch is the branch the work chat is on.
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
	out, _, err := s.cfg.Git.Run(ctx, dir, "status", "--porcelain", "--branch")
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
