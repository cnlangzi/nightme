// Package version holds the build-time identity of nightme.
//
// The Version constant is updated by hand as part of the release
// commit; GitCommit and BuildDate are intended to be injected via
// -ldflags at compile time, e.g.:
//
//	go build -ldflags "-X github.com/cnlangzi/nightme/internal/version.GitCommit=$(git rev-parse --short HEAD) -X github.com/cnlangzi/nightme/internal/version.BuildDate=$(date -u +%Y-%m-%dT%H:%M:%SZ)" ./cmd/nightme
//
// Release tooling in v0.2 will automate this. The plain `go build`
// output is good enough for v0.1.
package version

// Version is the human-friendly release tag (without the leading
// "v"). Update this string in the release commit.
const Version = "0.1.0"

// GitCommit is the short SHA at build time. "unknown" means it
// was built without the -ldflags injection.
var GitCommit = "unknown"

// BuildDate is the ISO-8601 UTC timestamp of the build. "unknown"
// means the binary was built without -ldflags.
var BuildDate = "unknown"

// String renders a single-line, human-friendly version banner
// matching what `nightme --version` prints. The exact wording is
// pinned by TestString for log scrapers.
func String() string {
	return "nightme version " + Version + " (commit: " + GitCommit + ", built: " + BuildDate + ")"
}
