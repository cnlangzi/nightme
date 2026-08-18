package gtw

import (
	"context"
	"time"
)

// Event is a git-hosting event delivered to bot's trigger manager.
// Produced by the polling EventSource and consumed by the bot.
//
// The same struct covers all four git trigger kinds (pull_request,
// branch, issue, mention) — bot's trigger pipeline distinguishes
// by the Kind field.
type Event struct {
	// Kind is one of "pull_request", "branch", "issue", "mention".
	Kind string

	// Repo is "owner/repo" (e.g. "cnlangzi/nightme"). Set on all
	// events.
	Repo string

	// Action is the platform's action string ("opened",
	// "synchronize", "commented", "pushed", …). Set for
	// pull_request, branch, issue events; empty for mention (the
	// mention text carries the intent).
	Action string

	// PR is the pull request number (0 if not a PR event).
	PR int

	// Issue is the issue number (0 if not an issue event; PRs
	// have PR set, not Issue, since GitHub treats them as the
	// same resource but we keep them distinct in the model).
	Issue int

	// Branch is the branch name (only set for "branch" events).
	Branch string

	// Author is the user who triggered the event (PR author,
	// issue creator, branch pusher, comment author for mentions).
	Author string

	// CommentBody is the full comment text (only set for
	// "mention" events).
	CommentBody string

	// CommentID identifies the specific comment (only set for
	// "mention" events; can be used to deduplicate).
	CommentID int64

	// Command is the first word after @owner in the mention text
	// (only set for "mention" events; empty if the mention is
	// bare "@owner" with no command).
	Command string

	// URL is the human-readable link to the resource (PR, issue,
	// comment).
	URL string

	// Time is when the event happened on the platform (not when
	// the poller saw it).
	Time time.Time
}

// EventSource is the abstraction bot uses to receive git events.
// Implementations poll the git platform (since the existing
// GitProvider wraps the `gh` / `glab` CLI which has no native
// event subscription). v0: poll every 30s; future: webhook.
type EventSource interface {
	// Subscribe starts polling for events. Events are pushed to
	// the returned channel. The channel is closed when ctx is
	// cancelled or the source encounters a fatal error. Poll
	// errors are logged but do not stop the subscription (best-
	// effort delivery).
	Subscribe(ctx context.Context) (<-chan Event, error)
}
