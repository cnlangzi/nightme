//go:build !windows

package chatsession

// Platform-native cwd fixtures after pathutil.Clean / SetSelectedCwd.
// Keep in sync with path_fixture_windows_test.go.
const (
	testCwdBailing = "/code/bailing"
	testCwdTmp     = "/tmp"
)
