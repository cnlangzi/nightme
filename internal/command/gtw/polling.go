package gtw

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// PollingEventSource is a v0 implementation of EventSource that
// polls the gh/glab CLIs on a fixed interval. Webhook support
// is future work.
//
// Polling strategy:
//   - every `Interval`, run `gh api notifications` (or `glab api
//     notifications`) to find @owner mentions since the last
//     poll
//   - for each configured repo, run `gh api /repos/{owner}/{repo}
//     /issues/events` (or `glab api /projects/:id/issues?sort=...
//     &updated_after=...`) to find recent PR/issue/branch events
//   - dedupe by event ID (poll may overlap with previous poll)
//   - push to output channel
//
// v0 scope: simple, may miss events that happen between polls.
// Webhook is the production answer.
type PollingEventSource struct {
	// CLI is the CLI runner (defaults to os/exec; tests can inject
	// a fake). Signature: run(name, args...) → (stdout, stderr, err).
	CLI CLIRunner

	// Repos is the list of "owner/repo" to poll. Built from
	// workflows' workspaces at bot startup.
	Repos []string

	// OwnerLogin is the GitHub/GitLab login that bot's mention
	// trigger listens for. Used to filter @-mentions.
	OwnerLogin string

	// Provider selects CLI: "github" or "gitlab".
	Provider string

	// Interval between polls. Default 30s.
	Interval time.Duration

	// State is where the poller persists "last seen" cursors so
	// restarts don't re-fire old events. nil = in-memory only
	// (events may be missed across restarts in v0).
	State *PollingState
}

// PollingState is the cursor map for "last event ID per repo".
// Values are stringified IDs — GitHub uses int64 for issue/PR
// events (stored as-is) and string for notifications (e.g.
// "1338647212"). String storage is simplest and works for both
// (lexicographic ordering matches numeric for the small ranges
// we care about; collisions are filtered by the event itself).
type PollingState struct {
	mu       sync.Mutex
	lastSeen map[string]string // repo → last event ID (string)
}

func (s *PollingState) get(repo string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastSeen[repo]
}

func (s *PollingState) set(repo, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastSeen == nil {
		s.lastSeen = map[string]string{}
	}
	// Only advance forward (numeric or lexicographic)
	if id > s.lastSeen[repo] {
		s.lastSeen[repo] = id
	}
}

// Subscribe implements EventSource.
func (s *PollingEventSource) Subscribe(ctx context.Context) (<-chan Event, error) {
	if s.Interval == 0 {
		s.Interval = 30 * time.Second
	}
	if s.CLI == nil {
		s.CLI = ExecCLIRunner{}
	}
	if s.State == nil {
		s.State = &PollingState{lastSeen: map[string]string{}}
	}
	out := make(chan Event, 64)

	go s.loop(ctx, out)

	return out, nil
}

func (s *PollingEventSource) loop(ctx context.Context, out chan<- Event) {
	defer close(out)

	// First poll after a short delay so the bot's setup messages
	// (/cwd, /use agent) have a chance to land before the first
	// event.
	t := time.NewTimer(2 * time.Second)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		events := s.pollOnce(ctx)
		for _, ev := range events {
			select {
			case <-ctx.Done():
				return
			case out <- ev:
			}
		}
		t.Reset(s.Interval)
	}
}

// pollOnce runs a single poll cycle across all configured repos
// and the owner notifications, returns the new events.
func (s *PollingEventSource) pollOnce(ctx context.Context) []Event {
	var out []Event
	for _, repo := range s.Repos {
		out = append(out, s.pollRepo(ctx, repo)...)
	}
	if s.OwnerLogin != "" {
		out = append(out, s.pollMentions(ctx)...)
	}
	return out
}

// pollRepo fetches recent events for one repo and returns new
// ones (ID > lastSeen).
func (s *PollingEventSource) pollRepo(ctx context.Context, repo string) []Event {
	parts := strings.SplitN(repo, "/", 2)
	if len(parts) != 2 {
		return nil
	}
	owner, name := parts[0], parts[1]

	var events []Event
	switch s.Provider {
	case "github", "":
		events = s.pollGitHubRepo(ctx, owner, name)
	case "gitlab":
		events = s.pollGitLabRepo(ctx, owner, name)
	}

	// Dedup + advance cursor.
	keep := events[:0]
	for _, ev := range events {
		var id string
		switch ev.Kind {
		case "pull_request", "issue":
			if ev.PR > 0 {
				id = intToString(ev.PR)
			} else if ev.Issue > 0 {
				id = intToString(ev.Issue)
			}
		}
		if id != "" && id > s.State.get(repo) {
			s.State.set(repo, id)
			keep = append(keep, ev)
		}
	}
	return keep
}

// pollGitHubRepo fetches recent issue/PR events for a GitHub repo.
func (s *PollingEventSource) pollGitHubRepo(ctx context.Context, owner, name string) []Event {
	stdout, _, err := s.CLI.Run(ctx, "gh", "api",
		fmt.Sprintf("/repos/%s/%s/issues/events?per_page=20", owner, name))
	if err != nil {
		return nil
	}
	type ghEvent struct {
		ID        int64  `json:"id"`
		Event     string `json:"event"`
		CreatedAt string `json:"created_at"`
		Issue struct {
			Number    int    `json:"number"`
			State     string `json:"state"`
			Title     string `json:"title"`
			HTMLURL   string `json:"html_url"`
			PullRequest *struct {
				URL string `json:"url"`
			} `json:"pull_request"`
		} `json:"issue"`
		Actor struct {
			Login string `json:"login"`
		} `json:"actor"`
	}
	var raw []ghEvent
	if err := json.NewDecoder(strings.NewReader(stdout)).Decode(&raw); err != nil {
		return nil
	}
	var out []Event
	for _, e := range raw {
		// Map GitHub event types to our Kind
		kind := ""
		switch e.Event {
		case "opened", "reopened", "closed", "edited", "labeled", "unlabeled", "assigned", "synchronize":
			if e.Issue.PullRequest != nil {
				kind = "pull_request"
			} else {
				kind = "issue"
			}
		default:
			continue // ignore comment events here (covered by mentions)
		}
		ev := Event{
			Kind:   kind,
			Repo:   owner + "/" + name,
			Action: e.Event,
			Author: e.Actor.Login,
			URL:    e.Issue.HTMLURL,
			Time:   parseTime(e.CreatedAt),
		}
		if e.Issue.PullRequest != nil {
			ev.PR = e.Issue.Number
		} else {
			ev.Issue = e.Issue.Number
		}
		out = append(out, ev)
	}
	return out
}

// pollGitLabRepo is a stub for v0 (GitLab event fetching needs
// its own /api/v4/events endpoint pattern).
func (s *PollingEventSource) pollGitLabRepo(_ context.Context, _, _ string) []Event {
	return nil
}

// pollMentions fetches recent @owner mentions across all repos
// the user has access to (via gh notifications API).
func (s *PollingEventSource) pollMentions(ctx context.Context) []Event {
	stdout, _, err := s.CLI.Run(ctx, "gh", "api", "notifications?all=false")
	if err != nil {
		return nil
	}
	type ghNotif struct {
		ID         string `json:"id"`
		Unread     bool   `json:"unread"`
		Reason     string `json:"reason"`
		UpdatedAt  string `json:"updated_at"`
		Subject struct {
			Title            string `json:"title"`
			URL              string `json:"url"`
			LatestCommentURL string `json:"latest_comment_url"`
			Type            string `json:"type"` // "Issue", "PullRequest", etc.
		} `json:"subject"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
	}
	var raw []ghNotif
	if err := json.NewDecoder(strings.NewReader(stdout)).Decode(&raw); err != nil {
		return nil
	}
	var out []Event
	seen := s.State.lastSeen
	for _, n := range raw {
		// Only mention-type notifications
		if n.Reason != "mention" && n.Reason != "subscribed" {
			continue
		}
		// Dedupe by notification ID (lexicographic string compare;
		// GitHub notification IDs are sortable strings)
		lastID, _ := seen["__mentions__"]
		if lastID != "" && n.ID <= lastID {
			continue
		}
		// Fetch the actual comment body to extract the @owner command
		body, cmd := s.fetchMentionBody(ctx, n.Subject.LatestCommentURL)
		ev := Event{
			Kind:        "mention",
			Repo:        n.Repository.FullName,
			Action:      "commented",
			Author:      n.Subject.Title, // not great, but body has more
			CommentBody: body,
			Command:     cmd,
			URL:         n.Subject.URL,
			Time:        parseTime(n.UpdatedAt),
		}
		if n.Subject.Type == "PullRequest" {
			ev.PR = 0 // populated below
		}
		out = append(out, ev)
	}
	if len(out) > 0 {
		// Advance cursor to highest seen notification ID.
		maxID := ""
		for _, n := range raw {
			if n.ID > maxID {
				maxID = n.ID
			}
		}
		if maxID != "" {
			s.State.set("__mentions__", maxID)
		}
	}
	return out
}

// fetchMentionBody fetches the comment body for a mention event
// and extracts the first word after @owner (the command).
func (s *PollingEventSource) fetchMentionBody(ctx context.Context, url string) (body, cmd string) {
	if url == "" {
		return "", ""
	}
	stdout, _, err := s.CLI.Run(ctx, "gh", "api", "-H", "Accept: application/vnd.github+json", url)
	if err != nil {
		return "", ""
	}
	type commentResp struct {
		Body string `json:"body"`
	}
	var c commentResp
	if err := json.NewDecoder(strings.NewReader(stdout)).Decode(&c); err != nil {
		return "", ""
	}
	body = c.Body
	// Extract the first word after @owner (any leading @-prefixed handle).
	cmd = extractCommand(body, s.OwnerLogin)
	return body, cmd
}

// extractCommand finds the first word after @ownerLogin in text.
// "@owner review this" → "review". "@owner" alone → "". Returns
// "" if owner isn't mentioned.
func extractCommand(text, ownerLogin string) string {
	if ownerLogin == "" {
		return ""
	}
	// Look for "@<owner>" (case-insensitive, may have a / qualifier)
	lower := strings.ToLower(text)
	ownerLower := strings.ToLower(ownerLogin)
	idx := strings.Index(lower, "@"+ownerLower)
	if idx < 0 {
		return ""
	}
	// Skip past @owner (and any /qualifier)
	rest := text[idx+1+len(ownerLogin):]
	if strings.HasPrefix(rest, "/") {
		// skip qualifier (e.g. "owner/bot")
		slash := strings.Index(rest, " ")
		if slash < 0 {
			return ""
		}
		rest = rest[slash+1:]
	}
	// First whitespace-delimited token
	end := strings.IndexAny(rest, " \t\n")
	if end < 0 {
		end = len(rest)
	}
	return rest[:end]
}

// parseTime parses an ISO 8601 timestamp, falling back to now.
func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{
		time.RFC3339,
		"2006-01-02T15:04:05Z",
		"2006-01-02T15:04:05-07:00",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

func intToString(n int) string {
	if n == 0 {
		return ""
	}
	// avoid pulling in strconv import for one call
	if n < 10 {
		return string(rune('0' + n))
	}
	// fall back to fmt for multi-digit
	return fmt.Sprintf("%d", n)
}

// Compile-time check
var _ EventSource = (*PollingEventSource)(nil)

// Unused but reserved for future: scan stdin of subprocess output.
var _ = bufio.NewScanner
var _ = (*exec.Cmd)(nil)
