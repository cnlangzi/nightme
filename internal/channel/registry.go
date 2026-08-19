// Package channel — channel registry for OCP-friendly multi-channel
// auto-start.
//
// Each channel adapter (feishu, telegram, future slack, …) calls
// `Register(name, NewAdapter)` from its `init()`. The runtime calls
// `BuildAll(cfg)` to construct every registered channel that has
// valid credentials in cfg; channels missing required credentials
// are skipped (their Builder returns an error).
//
// echo is intentionally NOT in the registry — it's a smoke-test
// channel wired through Deps.NewChannels for tests; production
// runtime should never start an echo channel.
package channel

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/cnlangzi/nightme/internal/config"
)

// Builder constructs a Channel from cfg. It should return an error
// when cfg is missing the credentials the channel requires (e.g.
// feishu returns error if cfg.Feishu.AppID/AppSecret is empty).
// BuildAll treats such errors as "skip", not as fatal.
type Builder func(*config.Config) (Channel, error)

var (
	mu  sync.RWMutex
	reg = map[string]Builder{}
)

// Register adds a channel Builder under name. Called from each
// adapter's init(). Panics on duplicate registration (a programming
// error, not a runtime condition).
func Register(name string, b Builder) {
	mu.Lock()
	defer mu.Unlock()
	if _, exists := reg[name]; exists {
		panic(fmt.Sprintf("channel: duplicate registration for %q", name))
	}
	reg[name] = b
}

// Available returns the registered channel names in deterministic
// (sorted) order. Used by BuildAll and by tests.
func Available() []string {
	mu.RLock()
	defer mu.RUnlock()
	names := make([]string, 0, len(reg))
	for name := range reg {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// GetBuilder returns the builder registered under name, or nil.
// Reserved for tests; production code uses BuildAll.
func GetBuilder(name string) Builder {
	mu.RLock()
	defer mu.RUnlock()
	return reg[name]
}

// BuildAll iterates every registered channel and attempts to
// construct it from cfg. A Builder returning an error is treated as
// "channel has no usable credentials for this cfg" and skipped
// silently (a single missing channel must not prevent the daemon
// from starting the others).
//
// If every registered channel fails to construct, BuildAll returns
// an error aggregating the per-channel messages, so the runtime
// can surface a clear "no channels configured" diagnostic and tell
// the user to run `nightme login <channel>` first.
func BuildAll(cfg *config.Config) ([]Channel, error) {
	mu.RLock()
	names := make([]string, 0, len(reg))
	for name := range reg {
		names = append(names, name)
	}
	mu.RUnlock()
	sort.Strings(names)

	var (
		out      []Channel
		failures []string
	)
	for _, name := range names {
		b := GetBuilder(name)
		if b == nil {
			continue
		}
		ch, err := b(cfg)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		out = append(out, ch)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf(
			"no channels configured (%s); run `nightme login <channel>` first",
			strings.Join(failures, "; "),
		)
	}
	return out, nil
}

// Reset clears the registry. Test-only: production code MUST NOT
// call this. Each adapter package's init() re-registers on next
// process start; if a test wants fresh state, the test must
// re-import the adapter packages after Reset.
func Reset() {
	mu.Lock()
	defer mu.Unlock()
	reg = map[string]Builder{}
}
