// Package channel — channel registry for OCP-friendly multi-channel
// auto-start.
//
// Each channel adapter (feishu, telegram, future slack, …) calls
// `Register(name, prefix, NewAdapter)` from its `init()`. The
// runtime calls `BuildAll(cfg)` to construct every registered
// channel that has valid credentials in cfg; channels missing
// required credentials are skipped (their Builder returns an
// error).
//
// prefix is the chat-id namespace tag every ChatID the adapter
// produces carries (e.g. "tg_", "oc_", "sl_", "bt_"). It is read
// by chatstore.New at file-load time to recognise chat_sessions.json
// keys WITHOUT having to construct the adapter — chatstore loads
// BEFORE BuildAll, and an adapter's Builder may legitimately fail
// (missing credentials) at startup even when valid entries
// already exist on disk. The prefix is the same string the
// Channel.ChatIDPrefix() method returns at runtime; the registry
// mirrors it at init() time so the consumer (chatstore) does not
// depend on adapter construction.
//
// Pass prefix="" for channels whose chat IDs legitimately have no
// namespace tag. ChatIDPrefixes skips empty prefixes so they do
// not falsely accept arbitrary keys.
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

// entry is one registry slot. The prefix is stored alongside the
// builder so consumers (chatstore validation, future routing)
// can introspect the channel's namespace tag without having to
// call the Builder and inspect a constructed Channel.
type entry struct {
	prefix  string
	builder Builder
}

var (
	mu  sync.RWMutex
	reg = map[string]entry{}
)

// Register adds a channel Builder under name, declaring the
// chat-id namespace prefix the channel attaches to every chat id
// (e.g. "tg_", "oc_", "sl_"). See package doc for the rationale.
//
// Panics on duplicate registration (a programming error, not a
// runtime condition). prefix must not contain ':' or '/' —
// those characters would break the on-disk key format used by
// chat_sessions.json (telegram encodes thread ids with ':' as a
// suffix separator).
func Register(name, prefix string, b Builder) {
	if strings.ContainsAny(prefix, ":/") {
		panic(fmt.Sprintf("channel: prefix %q for %q contains reserved character (':' or '/')", prefix, name))
	}
	mu.Lock()
	defer mu.Unlock()
	if _, exists := reg[name]; exists {
		panic(fmt.Sprintf("channel: duplicate registration for %q", name))
	}
	reg[name] = entry{prefix: prefix, builder: b}
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
	return reg[name].builder
}

// ChatIDPrefix returns the prefix declared by the channel
// registered under name, or "" if the channel is unknown or
// declared no prefix (e.g. the bot workflows engine). Used by
// tests; production code reads the full prefix set via
// ChatIDPrefixes().
func ChatIDPrefix(name string) string {
	mu.RLock()
	defer mu.RUnlock()
	return reg[name].prefix
}

// ChatIDPrefixes returns the chat-id namespace prefixes declared
// by every registered channel that has one (empty prefixes are
// skipped so they do not accept every key). Used by chatstore to
// validate chat_sessions.json keys without depending on which
// channels happen to be constructable at load time.
//
// Returned in deterministic (sorted) order so error messages and
// iteration are stable.
func ChatIDPrefixes() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(reg))
	for _, e := range reg {
		if e.prefix != "" {
			out = append(out, e.prefix)
		}
	}
	sort.Strings(out)
	return out
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
	reg = map[string]entry{}
}