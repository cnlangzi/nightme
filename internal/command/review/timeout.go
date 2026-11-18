// timeout.go — the per-group silence threshold of a review run, and
// what the chat is told when it fires.
//
// A review fan-out runs one subagent per group. A subagent that stops
// producing anything — a hung API call, a dead pipe the bridge never
// notices — would hold its group open forever. The threshold closes
// such a group on its own: the subagent is closed, the group is
// recorded as failed, and it stays resumable.
//
// The threshold is a silence window, not a deadline: every event on the
// group's session stream is a heartbeat that resets it.
//
// The policy lives here, in the command layer; the watchdog lives in
// the agent package's fan-out core (agent.WithReviewGroupTimeout).

package review

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/config"
)

// resolveReviewTimeout picks the silence threshold of a review run: the
// --timeout flag of this invocation, else review.timeout from the
// config file, else the built-in default.
//
// The second return value is the notice the user must see — empty when
// the threshold was resolved without a problem. A broken config value
// is a warning, not a failure: the review still runs, with the
// built-in default.
func resolveReviewTimeout(spec Spec) (time.Duration, string) {
	if spec.TimeoutSet {
		return spec.Timeout, ""
	}
	path := config.DefaultPath()
	raw, err := readReviewTimeout(path)
	if err != nil {
		return agent.DefaultReviewGroupTimeout, fmt.Sprintf(
			"⚠️ cannot read review.timeout from %s: %v — using %s",
			path, err, agent.DefaultReviewGroupTimeout)
	}
	if raw == "" {
		return agent.DefaultReviewGroupTimeout, ""
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return agent.DefaultReviewGroupTimeout, fmt.Sprintf(
			"⚠️ review.timeout %q is not a duration — using %s",
			raw, agent.DefaultReviewGroupTimeout)
	}
	if d < 0 {
		return agent.DefaultReviewGroupTimeout, fmt.Sprintf(
			"⚠️ review.timeout %q must not be negative — using %s",
			raw, agent.DefaultReviewGroupTimeout)
	}
	return d, ""
}

// readReviewTimeout reads the persisted review.timeout from the config
// file at path. An empty string means "not configured".
//
// config.Load (not config.LoadDefault) is deliberate: LoadDefault
// probes the built-in agents and may persist a detected primary as a
// side effect, which a slash command must not cause.
func readReviewTimeout(path string) (string, error) {
	cfg, err := config.Load(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(cfg.Review.Timeout), nil
}

// isGroupTimeout reports whether a group's failure was a silence
// timeout.
func isGroupTimeout(err error) bool {
	return errors.Is(err, agent.ErrGroupTimeout)
}

// timedOutGroupNames names the groups of a run that timed out. The
// count of the timed-out groups is the length of the slice.
func timedOutGroupNames(outcomes []reviewOutcome) []string {
	var names []string
	for _, o := range outcomes {
		if !isGroupTimeout(o.err) {
			continue
		}
		names = append(names, agent.ReviewGroupName(o.source, o.pattern))
	}
	return names
}

// reviewTimeoutNotice renders the notice a run sends when one of its
// groups timed out: how many of them, how long the silence lasted, and
// which groups they were.
//
// The notice is the actionable half of the report — it names the groups
// and points at the command that picks them up again.
func reviewTimeoutNotice(outcomes []reviewOutcome, timeout time.Duration) string {
	names := timedOutGroupNames(outcomes)
	if len(names) == 0 {
		return ""
	}
	return fmt.Sprintf("⏱️ %d of %d subagents timed out — no heartbeat for %s (%s) → /review -c",
		len(names), len(outcomes), timeout, strings.Join(names, ", "))
}

// timeoutOutcomeData adds the timed-out group count to a run's outcome
// data. A run without a timed-out group carries no key.
func timeoutOutcomeData(data map[string]string, outcomes []reviewOutcome) map[string]string {
	if n := len(timedOutGroupNames(outcomes)); n > 0 {
		data["timed_out_groups"] = fmt.Sprintf("%d", n)
	}
	return data
}
