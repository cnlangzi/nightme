package pathutil

import (
	"path/filepath"
	"testing"
)

// TestShortWorkspacePath — the short form of a working directory.
// The card's workspace line (internal/statusbar) and the shutdown
// notice (internal/chatsession) share this rule, so it is pinned
// here rather than through one of them.
func TestShortWorkspacePath(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{path: "", want: ""},
		{path: ".", want: ""},
		{path: "/", want: ""},
		{path: "/home/devin", want: filepath.FromSlash("home/devin")},
		{path: "/tmp/foo", want: filepath.FromSlash("tmp/foo")},
		{path: "/home/devin/code/nightme", want: filepath.FromSlash("code/nightme")},
		{path: "/home/devin/code/nightme/internal", want: filepath.FromSlash("nightme/internal")},
		{path: "/tmp/a/b/c", want: filepath.FromSlash("b/c")},
	}
	for _, tt := range tests {
		if got := ShortWorkspacePath(tt.path); got != tt.want {
			t.Errorf("ShortWorkspacePath(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}
