// Package review — the structured outcome every /review exit path
// reports.
//
// A program that drives nightme (the `me` personal agent) judges the
// result of a review from the Outcome that rides the settled signal
// — never from the text of the card the channel renders. Every place
// where /review decides what happened reports that decision here.
//
// The review's own turn runs on its own goroutine with its own
// context, so it reports through the Work it holds (cs.Hold); the
// paths that decide before the goroutine starts report through the
// ctx the command was dispatched with.
//
// The status vocabulary and the Data keys are documented in
// docs/me.md §1 — that table is the contract with the programs that
// read these outcomes.
package review

// groupCounts counts the groups a review ran and how many of them
// did not finish. The collector fills one entry per group, so the
// two numbers come straight from the fan-out — nothing is inferred
// from the review text.
func groupCounts(outcomes []reviewOutcome) (groups, unfinished int) {
	for _, o := range outcomes {
		groups++
		if o.err != nil {
			unfinished++
		}
	}
	return groups, unfinished
}
