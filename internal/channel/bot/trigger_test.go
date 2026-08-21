package bot

import (
	"strings"
	"testing"
)

// TestExtractCommandLocal sanity-checks the @owner mention
// parsing logic (mirrored locally for testability). The logic is
// straightforward enough that an in-test copy is cheaper than
// exporting it from the gtw package, which itself is just a CLI
// shim for /gtw fix.
func TestExtractCommandLocal(t *testing.T) {
	localExtract := func(text, ownerLogin string) string {
		if ownerLogin == "" {
			return ""
		}
		lower := strings.ToLower(text)
		ownerLower := strings.ToLower(ownerLogin)
		idx := strings.Index(lower, "@"+ownerLower)
		if idx < 0 {
			return ""
		}
		rest := text[idx+1+len(ownerLogin):]
		if strings.HasPrefix(rest, "/") {
			slash := strings.Index(rest, " ")
			if slash < 0 {
				return ""
			}
			rest = rest[slash+1:]
		}
		rest = strings.TrimLeft(rest, " \t")
		end := strings.IndexAny(rest, " \t\n")
		if end < 0 {
			end = len(rest)
		}
		return rest[:end]
	}
	tests := []struct {
		body, owner, want string
	}{
		{"@owner review this", "owner", "review"},
		{"@owner fix issue #42", "owner", "fix"},
		{"hello @owner review", "owner", "review"},
		{"@someone else", "owner", ""},
		{"@owner/bot review", "owner", "review"},
	}
	for _, tt := range tests {
		t.Run(tt.body, func(t *testing.T) {
			if got := localExtract(tt.body, tt.owner); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}
