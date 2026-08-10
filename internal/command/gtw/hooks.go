package gtw

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/chatsession"
)

// Config is the user-level gtw configuration loaded from
// ~/.nightme/gtw.yml. See wip/gtw-hooks.md for the schema.
//
// Loading is best-effort and obeys the iron rule "hooks are
// additive, never block main flow":
//
//   - file does not exist → zero-value, no warnings (silent skip)
//   - read error other than not-exist → zero-value + warning note
//   - yaml malformed → zero-value + warning note
//
// Per-command fields (Fix/Push/Close/Sync) are independently
// optional — a partial yml is fine.
type Config struct {
	Fix   CmdConfig `yaml:"fix"`
	Push  CmdConfig `yaml:"push"`
	Close CmdConfig `yaml:"close"`
	Sync  CmdConfig `yaml:"sync"`
}

// CmdConfig is the per-command subsection of Config. Agent is the
// default agent name for flows that need one (only pushDirty in
// v1). Hooks is the before/after hook list.
type CmdConfig struct {
	Agent string `yaml:"agent"`
	Hooks Hooks  `yaml:"hooks"`
}

// Hooks holds the optional before/after hook lists. Both default
// to nil (no hooks). Order is preserved.
type Hooks struct {
	Before []Hook `yaml:"before"`
	After  []Hook `yaml:"after"`
}

// Hook is a single hook entry. v1 only supports shell hooks;
// the Type field is forward-compatible for future types (agent,
// notify, ...) — see wip/gtw-hooks.md "Future (v2+)".
//
// Plain-string sugar: yaml sequence elements can be either a
// bare string ("- codegraph init") or a mapping ("- type: shell\n
//  run: ..."). UnmarshalYAML below normalises the bare-string
// form into a Hook with Type="" and Run=<string>. The runner
// treats Type=="" identically to Type=="shell".
type Hook struct {
	Type string `yaml:"type,omitempty"`
	Run  string `yaml:"run,omitempty"`
}

// UnmarshalYAML accepts both bare-string and mapping forms so
// users can write either
//
//	before:
//	  - codegraph init
//
// or
//
//	before:
//	  - type: shell
//	    run: codegraph init
//
// without forcing one or the other. A bare string is interpreted
// as Run; a mapping is unmarshalled into the Hook fields
// directly. This is what makes the sugar form actually work —
// yaml.v3's default behaviour would otherwise reject the string
// form with "cannot unmarshal !!str into gtw.Hook".
func (h *Hook) UnmarshalYAML(node *yaml.Node) error {
	// Bare-string scalar: just set Run.
	if node.Kind == yaml.ScalarNode && node.Tag == "!!str" {
		h.Run = node.Value
		return nil
	}
	// Mapping form: defer to default behaviour via a typed
	// alias to avoid infinite recursion through this method.
	type hookAlias Hook
	return node.Decode((*hookAlias)(h))
}

// LoadNotes carries diagnostic info from Load: warnings to
// append to the command reply. Empty when load was clean or
// when the file simply did not exist.
type LoadNotes struct {
	Warnings []string
}

// HasWarnings reports whether any load-time warnings were
// collected. Used by callers to decide whether to surface the
// notes block in the reply.
func (l LoadNotes) HasWarnings() bool { return len(l.Warnings) > 0 }

// Load reads ~/.nightme/gtw.yml and returns the parsed Config
// plus any load-time warnings.
//
// See Config doc for the failure-mode matrix. The function
// itself never returns an error — every failure mode becomes
// a zero-value Config plus a LoadNotes warning, consistent with
// "hooks are additive" iron rule.
func Load() (Config, LoadNotes) {
	var cfg Config
	var notes LoadNotes

	home, err := os.UserHomeDir()
	if err != nil {
		// Cannot resolve $HOME: silent skip. There's no path to
		// surface and no useful warning we can write.
		return cfg, notes
	}
	path := filepath.Join(home, ".nightme", "gtw.yml")

	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		// Silent — a missing yml is the expected default state.
		return cfg, notes
	}
	if err != nil {
		notes.Warnings = append(notes.Warnings,
			fmt.Sprintf("⚠️ read %s: %v", path, err))
		return cfg, notes
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		notes.Warnings = append(notes.Warnings,
			fmt.Sprintf("⚠️ parse %s: %v", path, err))
		return cfg, notes
	}
	return cfg, notes
}

// --- Hook execution ---

// HookResult captures one hook's outcome. Err is non-nil on any
// execution failure (exit != 0, timeout, unsupported type, etc.).
// All failures are non-fatal by design — see wip/gtw-hooks.md.
type HookResult struct {
	Name   string // human-readable label: "sh -c <run>" or "<type>:<run-truncated>"
	Stdout string
	Stderr string
	Err    error
}

// hookTimeout caps a single hook's runtime. After this it is
// killed (SIGKILL via context) and reported as a failure. The
// 30s default is generous for shell hooks while still preventing
// a hung binary from stalling the chat.
const hookTimeout = 30 * time.Second

// RunHooks executes the given hooks sequentially in declaration
// order. Failures never abort the loop — each hook gets a chance
// to run, and per-hook failures are surfaced via the returned
// HookResult.Err.
//
// Returns nil when hooks is empty or nil. Otherwise the returned
// slice has the same length as hooks (1:1 index correspondence).
func RunHooks(ctx context.Context, hooks []Hook, cwd string) []HookResult {
	if len(hooks) == 0 {
		return nil
	}
	results := make([]HookResult, 0, len(hooks))
	for _, h := range hooks {
		results = append(results, runOneHook(ctx, h, cwd))
	}
	return results
}

// runOneHook executes a single hook. See Hook for type semantics;
// v1 only honours shell hooks (Type == "" or "shell"). Anything
// else is a warn-skip per the iron rule.
func runOneHook(ctx context.Context, h Hook, cwd string) HookResult {
	t := strings.ToLower(strings.TrimSpace(h.Type))
	if t != "" && t != "shell" {
		return HookResult{
			Name: fmt.Sprintf("%s:%s", h.Type, truncate(h.Run, 60)),
			Err:  fmt.Errorf("unsupported hook type %q (v1 supports: shell)", h.Type),
		}
	}
	run := strings.TrimSpace(h.Run)
	if run == "" {
		return HookResult{
			Name: "<empty>",
			Err:  fmt.Errorf("hook has no run field"),
		}
	}
	if cwd == "" {
		return HookResult{
			Name: run,
			Err:  fmt.Errorf("no active workspace; /cwd <path> first"),
		}
	}

	hctx, cancel := context.WithTimeout(ctx, hookTimeout)
	defer cancel()

	cmd := exec.CommandContext(hctx, "sh", "-c", run)
	cmd.Dir = cwd
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	return HookResult{
		Name:   "sh -c " + run,
		Stdout: stdout.String(),
		Stderr: stderr.String(),
		Err:    err,
	}
}

// FormatResults renders hook results as a single markdown block
// suitable for appending to a command reply. The block is
// formatted with a section header so users can see what
// actually ran (per the "always echo" decision in
// wip/gtw-hooks.md).
//
// Returns "" when results is empty so callers can concatenate
// without nil checks.
func FormatResults(label string, results []HookResult) string {
	if len(results) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n━━ hooks: ")
	b.WriteString(label)
	b.WriteString(" ━━\n")
	for i, r := range results {
		fmt.Fprintf(&b, "[%d] $ %s\n", i+1, r.Name)
		if out := strings.TrimRight(r.Stdout, "\n"); out != "" {
			b.WriteString(indentLines(out, "  "))
			b.WriteString("\n")
		}
		if errStr := strings.TrimRight(r.Stderr, "\n"); errStr != "" {
			b.WriteString(indentLines(errStr, "  "))
			b.WriteString("\n")
		}
		if r.Err != nil {
			fmt.Fprintf(&b, "  ⚠️ %v\n", r.Err)
		}
	}
	return b.String()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// --- Agent priority resolver ---

// ResolveAgent applies the 3-tier priority chain described in
// wip/gtw-hooks.md "Agent 选择 — 3 档优先级":
//
//  1. cliAgent      — `-a` / `--agent` flag (highest)
//  2. ymlAgent      — ~/.nightme/gtw.yml <cmd>.agent
//  3. cs.SelectedAgent() — chat's currently /use'd agent
//
// Returns the chosen agent name plus any diagnostic notes. A
// ymlAgent that doesn't resolve via agent.Builtins.Get falls
// through to the session default with a warning (Q6 decision:
// never silently swap a user-configured agent; never brick
// /gtw on a missing name).
//
// The function never returns an error itself — the empty-string
// return + nil notes signals "no agent available anywhere",
// which the caller translates into the existing
// "❌ no agent selected" reply path. When ymlAgent is set but
// neither the session fallback exists, the diagnostic note
// deliberately omits "; using session default" so the user
// isn't told a fallback exists when one doesn't — that would
// hide the underlying "you configured an agent that isn't
// registered" cause behind a misleading "no agent selected"
// error.
func ResolveAgent(cliAgent, ymlAgent string, cs *chatsession.ChatSession) (name string, notes []string) {
	if cliAgent != "" {
		return cliAgent, nil
	}
	if ymlAgent != "" {
		if _, err := agent.Builtins.Get(ymlAgent); err == nil {
			return ymlAgent, nil
		}
		// yml references unknown agent: warn + fall through.
		// The session default is consulted next; if it's also
		// empty, the caller hits the existing ❌ reply path.
		fallback := ""
		if cs != nil {
			fallback = cs.SelectedAgent()
		}
		note := fmt.Sprintf("⚠️ gtw.yml agent %q not found", ymlAgent)
		if fallback != "" {
			note += "; falling back to session default"
		}
		return fallback, []string{note}
	}
	if cs != nil {
		return cs.SelectedAgent(), nil
	}
	return "", nil
}