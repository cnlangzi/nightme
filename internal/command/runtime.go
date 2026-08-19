package command

import (
	"log/slog"
	"sync"
	"time"
)

// RuntimeServices aggregates the dependencies a slash command
// receives at Handle() time. The runtime (cmd/nightme/run.go)
// builds this once at startup; the Commander passes it to every
// dispatched Handle() call.
//
// Commands that need per-chat state hold *chatsession.Manager
// directly in their Factory. The remaining fields are shared
// interfaces with multiple implementations or cross-cutting concerns.
type RuntimeServices struct {
	// Config provides cross-command read-only configuration.
	// Currently exposes only Primary (default agent name).
	Config Config

	// Logger is the structured logger used for diagnostic output.
	// May be nil; commands should fall back to slog.Default() in
	// that case.
	Logger *slog.Logger

	// Clock returns the current time. May be nil; commands
	// fall back to time.Now in that case. Test code overrides
	// for deterministic timestamps.
	Clock func() time.Time
}

// Config is the read-only configuration slice RuntimeServices
// exposes to commands. Currently just Primary; grows as commands
// need more shared config.
type Config struct {
	// Primary is the default agent name (cmdline
	// `nightme --primary` or cfg.Primary). Previously each
	// Factory received this directly as `defaultPrimary`; now
	// it lives in rt.Config.Primary and Factories no longer
	// carry the field.
	Primary string
}

// Deps carries the runtime-constructed state each command
// factory needs at registration time. Distinct from
// RuntimeServices (which carries per-dispatch deps): Deps is
// the wiring context the orchestrator provides once at
// startup; RuntimeServices is what every Handle() invocation
// receives.
//
// Each command package's init() calls RegisterBuilder with a
// closure that knows how to build its SlashCommandFactory
// from Deps. The orchestrator calls SetDeps once at startup
// to finalize every registered builder.
type Deps struct {
	// Primary is the primary agent name.
	Primary string
	// GTWExt is the gtw.HandlerDeps the gtw command needs
	// (git runner, HTTP prober, PR invalidator). Typed as
	// `any` to avoid command � gtw import cycle (gtw already
	// imports command for SlashCommandFactory). The gtw
	// package's builder closure type-asserts.
	GTWExt any
}

var (
	depsMu          sync.Mutex
	currentDeps     Deps
	builders        []func(rt Deps) SlashCommandFactory
	defaultRegistry = NewRegistry()
)

// RegisterBuilder adds a factory builder. Called from each
// command package's init(). Re-calling with a new builder
// appends to the list — the next SetDeps rebuilds every
// factory. Tests can call Reset() to wipe both the builders
// list and the default registry.
//
// RegisterBuilder does NOT immediately instantiate the
// factory. SetDeps is the single point where every builder
// runs against currentDeps. This avoids the double-build
// bug (Phase 2.5: late RegisterBuilder after SetDeps was
// building the factory twice — once at register time, once
// on the next SetDeps — which was wasted work for every
// builder and observable for gtw, whose builder creates a
// *Manager + sets up routes).
func RegisterBuilder(b func(rt Deps) SlashCommandFactory) {
	if b == nil {
		return
	}
	depsMu.Lock()
	builders = append(builders, b)
	depsMu.Unlock()
}

// SetDeps initializes the global Deps and finalizes every
// registered builder into the default registry. Idempotent:
// a second call re-builds every factory with the new Deps.
//
// Callers MUST call SetDeps before Default() returns a useful
// registry. The orchestrator (internal/runtime) is the only
// production caller; tests can drive SetDeps directly.
func SetDeps(d Deps) {
	depsMu.Lock()
	defer depsMu.Unlock()
	currentDeps = d
	defaultRegistry = NewRegistry()
	for _, b := range builders {
		defaultRegistry.Register(b(d))
	}
}

// Default returns the package-level default registry. Empty
// until SetDeps has been called at least once.
func Default() *Registry {
	return defaultRegistry
}

// Reset clears the registry + builders list + Deps.
// Tests-only — production code MUST NOT call this. After
// Reset, RegisterBuilder + SetDeps work as if starting from
// a fresh process.
func Reset() {
	depsMu.Lock()
	defer depsMu.Unlock()
	currentDeps = Deps{}
	builders = nil
	defaultRegistry = NewRegistry()
}