package version

import (
	"runtime"
	"strings"
)

// uaProduct is the leading product token. Kept separate from the
// rest of the string so a grep for the wire identity lands here.
const uaProduct = "nightme"

// UserAgent builds the User-Agent nightme sends when it asks a
// server "what is the latest version?". The shape follows RFC 9110
// §10.1.5 — and, deliberately, the browser convention: a product
// token followed by a parenthesised platform comment.
//
//	nightme/0.3.7+926bbc4 (darwin; arm64; go1.24.0)   release build
//	nightme/0.1.0+local   (darwin; arm64; go1.24.0)   plain `go build`
//
// The platform comment carries GOOS and GOARCH because that is what
// decides which release asset a user can actually install — a
// version check from linux/arm64 means something different from one
// from windows/amd64. Both are compile-time constants from the
// toolchain, so they cost nothing and cannot disagree with the
// binary they are built into. The Go version rides along for triage
// of TLS / proxy behaviour that changes between toolchains.
//
// The commit suffix is always present. Builds that injected
// GitCommit via -ldflags carry the short SHA; builds that did not
// carry localCommit ("local"). Marking them rather than omitting
// the suffix matters because Version defaults to a hardcoded
// release string with no ldflags, so an unmarked development build
// would be indistinguishable on the wire from that actual release.
func UserAgent() string {
	v := uaToken(Normalize(Version))
	if v == "" {
		v = "unknown"
	}

	// GitCommit already defaults to localCommit, so this guard is
	// only for a build that injects an explicitly empty string —
	// without it the suffix would degenerate to a dangling "+".
	commit := uaToken(GitCommit)
	if commit == "" {
		commit = localCommit
	}

	var b strings.Builder
	b.WriteString(uaProduct)
	b.WriteByte('/')
	b.WriteString(v)
	b.WriteByte('+')
	b.WriteString(commit)

	// GOOS / GOARCH are compile-time constants from the toolchain,
	// so they need no sanitising; runtime.Version() does not either
	// in practice, but it costs nothing to run it through uaToken.
	b.WriteString(" (")
	b.WriteString(runtime.GOOS)
	b.WriteString("; ")
	b.WriteString(runtime.GOARCH)
	b.WriteString("; ")
	b.WriteString(uaToken(runtime.Version()))
	b.WriteByte(')')

	return b.String()
}

// uaToken strips everything that could break the header line or
// escape the parenthesised comment.
//
// Version and GitCommit arrive via -ldflags, which makes them
// effectively untrusted: a stray newline in either would make
// net/http reject the request outright ("invalid header field
// value") and take the whole version check down with it. Dropping
// the offending bytes keeps the check working with a slightly
// mangled identity, which is the right trade for a best-effort
// background call.
func uaToken(s string) string {
	return strings.Map(func(r rune) rune {
		// Anything outside printable ASCII, plus the two
		// characters that delimit a comment.
		if r <= ' ' || r > '~' || r == '(' || r == ')' {
			return -1
		}
		return r
	}, s)
}
