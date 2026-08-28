package version

import (
	"fmt"
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

	// GOOS / GOARCH are compile-time constants from the toolchain,
	// so they need no sanitising; runtime.Version() goes through
	// uaToken only for consistency with the injected fields.
	return fmt.Sprintf("%s/%s+%s (%s; %s; %s)",
		uaProduct, v, commit,
		runtime.GOOS, runtime.GOARCH, uaToken(runtime.Version()))
}

// uaToken strips everything that is not an RFC 9110 §5.6.2 tchar,
// which is what a product token and a product-version are allowed
// to contain.
//
// Version and GitCommit arrive via -ldflags, which makes them
// effectively untrusted. Two things must not happen. A stray
// newline would make net/http reject the request outright
// ("invalid header field value") and take the whole version check
// down with it — note that http.Request.Write does NOT catch this,
// it silently rewrites CR/LF to spaces, so the failure only
// surfaces inside the transport. And a "(", ")" or ";" would let
// an injected value forge or escape the platform comment, whose
// own separator is ";". Restricting to tchar covers both classes
// plus the rest of the grammar, rather than blocklisting the
// characters we happened to think of.
//
// Dropping the offending bytes keeps the check working with a
// slightly mangled identity, which is the right trade for a
// best-effort background call.
func uaToken(s string) string {
	return strings.Map(func(r rune) rune {
		if isTchar(r) {
			return r
		}
		return -1
	}, s)
}

// isTchar reports whether r is an RFC 9110 §5.6.2 tchar. Every
// value we actually emit — a semver tag, a `git describe` string,
// a hex SHA, a Go toolchain version — is already tchar-clean, so
// this only ever fires on injected garbage.
func isTchar(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	}
	switch r {
	case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
		return true
	}
	return false
}
