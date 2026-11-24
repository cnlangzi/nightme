package review

// The Data keys this package's outcomes carry.
//
// A key names a fact, not a category: the status says which branch
// the work took. A key with no value does not appear at all.
//
// A count is a decimal string and a boolean is "true" or "false".
const (
	// BoolKeyAddressed says whether the findings were acted on: the
	// workspace carries a new commit or a new change since the
	// findings went out. It is absent when the review found no
	// findings, when the agent's turn did not end cleanly, and when
	// the workspace could not be read.
	BoolKeyAddressed = "addressed"
	// KeyAgent is the name of the agent that ran the review.
	KeyAgent = "agent"
	// KeyContinuedGroups is how many groups a continuation finished.
	KeyContinuedGroups = "continued_groups"
	// KeyDelivered says whether the findings were handed to the agent.
	BoolKeyDelivered = "delivered"
	// KeyElapsed is how long the cancelled review had run.
	KeyElapsed = "elapsed"
	// KeyFindings is how many findings the review collected.
	KeyFindings = "findings"
	// KeyGroups is how many groups the review ran.
	KeyGroups = "groups"
	// KeyKind is which run was cancelled: review or continue.
	KeyKind = "kind"
	// KeyScope is the scope the runner was written in: global, repo or
	// default.
	KeyScope = "scope"
	// KeyTimedOutGroups is how many groups ran out of time.
	KeyTimedOutGroups = "timed_out_groups"
	// KeyUnfinished is the number of review groups that did not
	// finish. It is the switch a consumer reads to decide how to pick
	// the work up — the chain of a step hands the unfinished part
	// over to the next agent, and the count is what tells it that
	// there is a part to hand over.
	//
	// The count is a number, not a list: the groups themselves stay
	// with the run that produced them.
	KeyUnfinished = "unfinished"
)
