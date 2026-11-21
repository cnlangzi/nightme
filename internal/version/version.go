// Package version holds the build-time identity of nightme.
//
// All three fields are intended to be injected via -ldflags at
// compile time, e.g.:
//
//	go build -ldflags "\
//	    -X github.com/cnlangzi/nightme/internal/version.Version=0.3.7 \
//	    -X github.com/cnlangzi/nightme/internal/version.GitCommit=abc1234 \
//	    -X github.com/cnlangzi/nightme/internal/version.BuildDate=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
//	    ./cmd/nightme
//
// VERSION and GIT_COMMIT are typically set via environment variables
// by the CI pipeline (ci.yml: 0.0.0, release.yml: tag name).
//
// The defaults below are used when no -ldflags are supplied (e.g.
// `go run` during development) so `nightme --version` still prints
// a stable banner instead of empty fields.
package version

import (
	"strconv"
	"strings"
)

// Version is the human-friendly release tag. Typically injected
// from the git tag (e.g. "v0.2.0") at release-build time.
//
// The default below is NOT kept in sync with the latest release and
// should not be read as one — it exists only so `--version` prints
// something stable on a build with no -ldflags. It says "dev"
// because a build with no -ldflags is a build of the working tree:
// the version a workflow is held to is the version of a release, and
// a working tree is not one. GitCommit reads "local" on exactly those
// builds, so both the banner and the User-Agent already say "working
// tree" without this value having to be accurate.
var Version = "dev"

// GitCommit is the short SHA at build time. "local" means it was
// built without the -ldflags injection — which says "built from a
// working tree" rather than "we have no idea", the truthful reading:
// such a build is not a commit we failed to identify, it is a commit
// that does not exist yet. UserAgent emits the same string, so the
// wire identity and the --version banner never disagree.
var GitCommit = "local"

// BuildDate is the ISO-8601 UTC timestamp of the build. "unknown"
// means the binary was built without -ldflags.
var BuildDate = "unknown"

// String renders a single-line, human-friendly version banner
// matching what `nightme --version` prints. The exact wording is
// pinned by TestString for log scrapers.
func String() string {
	return "nightme version " + Version + " (commit: " + GitCommit + ", built: " + BuildDate + ")"
}

// IsRelease reports whether this build carries a release version.
//
// A release version is a clean major.minor.patch that is not 0.0.0:
// the CI pipeline injects 0.0.0, and a build that carries a suffix
// ("0.9.1-dev"), that is not a version at all ("dev", a commit sha)
// or that is empty is a development build.
//
// Only a release build holds a workflow to its version requirements:
// a development build is the newest code there is, and a version
// number that names no release must not stop it.
func IsRelease() bool {
	return isRelease(Version)
}

// isRelease reports whether the version string names a release.
func isRelease(v string) bool {
	major, minor, patch, ok := cleanVersion(v)
	if !ok {
		return false
	}
	return major != 0 || minor != 0 || patch != 0
}

// cleanVersion reads a clean version: three numbers and nothing else.
//
// ok is false when the string is not a clean version.
func cleanVersion(v string) (major, minor, patch int, ok bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, 0, 0, false
	}
	if v[0] == 'v' || v[0] == 'V' {
		v = v[1:]
	}
	fields := strings.Split(v, ".")
	if len(fields) != 3 {
		return 0, 0, 0, false
	}
	nums := make([]int, 3)
	for i, field := range fields {
		if !isDigits(field) {
			return 0, 0, 0, false
		}
		n, err := strconv.Atoi(field)
		if err != nil {
			return 0, 0, 0, false
		}
		nums[i] = n
	}
	return nums[0], nums[1], nums[2], true
}

// isDigits reports whether the string is nothing but digits.
func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
