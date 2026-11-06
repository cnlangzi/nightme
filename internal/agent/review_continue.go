// review_continue.go — the resume half of the review fan-out.
//
// A review is not one session: it is a fan-out of one session per
// review group (the ocr rule groups plus the simplify lens; the
// builtin path yields one builtin group plus simplify). A group can
// fail on its own — its session dies while the other groups deliver
// their findings — and the merged text marks the dead group inline.
//
// ContinueReviewJobs is how the user picks that run back up: it
// reopens the sessions of the groups that did NOT complete, sends
// them a short text, and merges their replies in the same group
// format a review produces.
//
// The sessions are resumed, not restarted: each group's session id
// goes to the bridge as StartConfig.SessionID, so the agent keeps the
// conversation it had — including the review prompt and the output
// format it was asked for. The resumed agent picks the work up where
// it stopped instead of re-reading the diff.

package agent

import (
	"context"
)

// ContinueReviewJobs resumes the given review sessions concurrently,
// sends text to each of them, and returns the merged reply.
//
// The sessions are resumed, not restarted: each group's session id is
// passed to the bridge as StartConfig.SessionID, so the agent keeps
// the conversation it had. The merged text uses the same group format
// as a review run — one "### Group: pattern <p> — files: …" section
// per group, with an inline "failed:" marker for a group that fails
// again.
//
// cfg.Workspace is the workspace the resumed sessions run in — the
// workspace of the review being continued, not the caller's current
// one. Each group's StartConfig.SessionID is that group's
// ReviewJob.SessionID.
//
// The events of all resumed sessions flow through the same event
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
		groups[i] = reviewGroup{Pattern: j.Pattern, Files: j.Files}
	}

	run := func(ctx context.Context, groupIdx int, cfg StartConfig, runOpts []RunOnceOption) (RunResult, error) {
		cfg.SessionID = jobs[groupIdx].SessionID
		return ResumeSession(ctx, s, cfg, []ContentBlock{{
			Type: ContentText,
			Text: text,
		}}, runOpts...)
	}
	return runReviewGroups(ctx, s, cfg, groups, opts, run)
}
