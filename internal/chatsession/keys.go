package chatsession

// The Data keys the outcomes this package produces carry.
//
// A key names a fact, not a category: the status says which branch
// the work took. A key with no value does not appear at all.
const (
	// KeyUpstreamStatus is the HTTP status the upstream reported
	// when it failed the turn.
	KeyUpstreamStatus = "upstream_status"
	// KeyUpstreamCode is the upstream's own machine-readable code
	// for the failure.
	KeyUpstreamCode = "upstream_code"
)
