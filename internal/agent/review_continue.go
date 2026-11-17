// review_continue.go — the resume half of the review fan-out.
//
// A review is not one session: it is a fan-out of one session per
// review group (the ocr rule groups plus the simplify lens; the
// builtin path yields one builtin group plus simplify). A group can
// fail on its own — its session dies while the other groups deliver
// their findings — and the merged text marks the dead group inline.
//
// ContinueReviewJobs is how the user picks that run back up: it
// finishes the groups that did NOT complete and merges their replies
// in the same group format a review produces.
//
// There are two ways to finish a group, and which one applies depends
// on how far the group got:
//
//   - a group that reached the agent is RESUMED: its session id goes
//     to the bridge as StartConfig.SessionID, so the agent keeps the
//     conversation it had — including the review prompt and the output
//     format it was asked for — and picks the work up where it
//     stopped;
//   - a group that never reached the agent has no conversation to
//     continue, so it is RE-RUN: a fresh session carries the group's
//     stored prompt, replayed verbatim.
package agent

import (
	"context"
)

// ContinueReviewJobs finishes the given review groups concurrently and
// returns the merged reply.
//
// A group with a session id is resumed: the session is reopened and
// sent the given text. A group without one is re-run: a fresh session
// carries the group's stored prompt.
//
// The merged text uses the same group format as a review run — one
// "### Group: pattern <p> — files: …" section per group, with an
// inline "failed:" marker for a group that fails again.
//
// cfg.Workspace is the workspace the sessions run in — the workspace
// of the review being continued, not the caller's current one. Each
// group's StartConfig.SessionID is that group's ReviewJob.SessionID.
//
// The events of all the sessions flow through the same event
// aggregator a review's groups use, so the chat sees a single outer
// lifecycle (one Ready, one Result) for the whole continuation.
//
// A group that fails does not sink the run: the surviving groups'
// replies still flow into the merged text, and the failed group is
// marked inline. Only when every group fails does the call return an
// error.
func ContinueReviewJobs(
	ctx context.Context,
	s Starter,
	cfg StartConfig,
	jobs []ReviewJob,
	text string,
	opts ...RunOnceOption,
) (RunResult, error) {
	groups := make([]reviewGroup, len(jobs))
	for i, j := range jobs {
		mode := ReviewModeRerun
		if j.SessionID != "" {
			mode = ReviewModeResume
		}
		groups[i] = reviewGroup{
			Pattern: j.Pattern,
			Source:  j.Source,
			Files:   j.Files,
			Mode:    mode,
		}
	}

	run := func(ctx context.Context, groupIdx int, cfg StartConfig, runOpts []RunOnceOption) (RunResult, error) {
		job := jobs[groupIdx]
		if job.SessionID != "" {
			cfg.SessionID = job.SessionID
			return ResumeSession(ctx, s, cfg, []ContentBlock{{
				Type: ContentText,
				Text: text,
			}}, runOpts...)
		}
		// The group never reached the agent: there is no conversation
		// to continue, so a fresh session carries the stored prompt.
		// The prompt is replayed verbatim, not re-assembled against
		// the current workspace: one continuation covers one snapshot,
		// and the resumed groups talk about the snapshot the review
		// took.
		prompt := job.Prompt
		if prompt == "" {
			prompt = BuiltinPrompt
		}
		groups[groupIdx].Prompt = prompt
		return RunSession(ctx, s, cfg, []ContentBlock{{
			Type: ContentText,
			Text: prompt,
		}}, runOpts...)
	}
	return runReviewGroups(ctx, s, cfg, groups, opts, run)
}
