// Package version holds the build-time identity of nightme.
//
// All three fields are intended to be injected via -ldflags at
// compile time, e.g.:
//
//	go build -ldflags "\
//	    -X github.com/cnlangzi/nightme/internal/version.Version=$(git describe --tags --always) \
//	    -X github.com/cnlangzi/nightme/internal/version.GitCommit=$(git rev-parse --short HEAD) \
//	    -X github.com/cnlangzi/nightme/internal/version.BuildDate=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
//	    ./cmd/nightme
//
// The defaults below are used when no -ldflags are supplied (e.g.
// `go run` during development) so `nightme --version` still prints
// a stable banner instead of empty fields.
package version

// Version is the human-friendly release tag. Typically injected
// from the git tag (e.g. "v0.2.0") at release-build time. The
// default is the most recent released version and is used when
// the binary is built without -ldflags.
var Version = "0.1.0"

// localCommit is GitCommit's value when nothing was injected. It
// says "built from a working tree" rather than "we have no idea",
// which is the truthful reading: a build without -ldflags is not a
// commit we failed to identify, it is a commit that does not exist
// yet. UserAgent falls back to the same string, so the wire
// identity and the --version banner never disagree.
const localCommit = "local"

// GitCommit is the short SHA at build time. localCommit means it
// was built without the -ldflags injection.
var GitCommit = localCommit

// BuildDate is the ISO-8601 UTC timestamp of the build. "unknown"
// means the binary was built without -ldflags.
var BuildDate = "unknown"

// String renders a single-line, human-friendly version banner
// matching what `nightme --version` prints. The exact wording is
// pinned by TestString for log scrapers.
func String() string {
	return "nightme version " + Version + " (commit: " + GitCommit + ", built: " + BuildDate + ")"
}
