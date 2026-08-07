package claudecode

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// ErrResumeUnhealthy indicates the bridge refused to fall back from a
// failing --resume spawn. Surfaces a usable error message to the
// caller (chat-session runtime, which surfaces it as OutReply) so
// the user knows their --resume session ID could not be loaded.
//
// T-alive (2026-08-07): previously the bridge silently fell back to
// a fresh session on stderr-detected --resume rejection. That
// silently dropped the user's resume context — the runtime saw a
// "working" session but the assistant had no memory of the prior
// conversation. The user explicitly required resume preservation,
// so the bridge now fails loudly instead.
var ErrResumeUnhealthy = errors.New("claudecode: --resume session unhealthy")

// Agent is the agent.Agent descriptor for Claude Code. It returns
// agent.ModeJSONIO and spawns a stream-json session on Start.
//
// ModeJSONIO is a new value in the agent.Mode enum (added for v0.2 to
// distinguish Claude Code's bespoke JSON-IO from generic ACP / SDK /
// PTY modes). See docs/feat/F-24-claudecode-bridge.md §8.3.
type Agent struct {
	name    string
	command string
	args    []string
}

// New constructs a Claude Code agent descriptor. name is the registry key
// (typically "claude"); command is the CLI binary name on PATH (typically
// "claude"); args are extra flags appended after DefaultArgs.
func New(name, command string, args []string) *Agent {
	return &Agent{
		name:    name,
		command: command,
		args:    append([]string(nil), args...),
	}
}

// Name returns the registry key.
func (a *Agent) Name() string { return a.name }

// Mode reports the JSON-IO mode (introduced for v0.2).
func (a *Agent) Mode() agent.Mode { return agent.ModeJSONIO }

// Command returns the configured CLI binary (typically "claude").
// Surfaced by `nightme agents` so users can see what /run would spawn.
func (a *Agent) Command() string { return a.command }

// Args returns a defensive copy of the constructor args. Callers may
// not mutate the returned slice.
func (a *Agent) Args() []string {
	return append([]string(nil), a.args...)
}

// Detect verifies the `claude` binary resolves on PATH. Call before Start
// to surface a friendly "claude not installed" error rather than a
// confusing exec failure.
func (a *Agent) Detect() error {
	_, err := exec.LookPath(a.command)
	return err
}

// resumeFallbackTimeout is the safety net for the stderr-driven
// probe: if stderr emits no resume-rejection signal AND the
// stderr drainer closes within this window (meaning the child
// process died), we assume the spawn is wedged. stderr signals
// themselves short-circuit the fallback decision — this timer
// only catches "process died cleanly without stderr signal".
//
// T-alive (2026-08-07): renamed from a previous "60s no init"
// safety net. The earlier version raced with AgentSession's
// readpump over sess.Events() — both consume from the same
// buffered channel and only ONE goroutine sees each event. The
// probe stealing events meant AS readpump missed init/result
// events, which broke downstream routing (verified 2026-08-07:
// manual `claude --resume <id>` works in ~17s but nightme's
// bridge timed out at 60s because probe consumed init instead
// of letting the readpump see it). Probe is now STDERR-ONLY.
// Stderr is deterministic across machines, doesn't race with
// the AS readpump, and carries the same resume-rejection
// signal claude emits to stdout (we verified both empirically).
const resumeFallbackTimeout = 5 * time.Second

// Start spawns Claude Code in stream-json mode and returns an
// AgentSession that streams parsed events on its Events channel.
//
// Workspace is the child process's cwd. cfg.Args are appended after the
// agent's defaults (DefaultArgs + a.args). cfg.Env is appended to
// os.Environ() for the child.
//
// cfg.PermissionMode overrides the --permission-mode flag baked into
// DefaultArgs. Empty string falls back to PermissionBypass (preserves
// v0.1 behaviour). Unknown values are forwarded as-is — Claude Code
// itself validates the set of legal modes.
//
// cfg.ResumeID, when non-empty, is appended as `--resume <id>` after
// cfg.Args so the child resumes the previous Claude Code session.
// Empty means "no --resume; start a fresh session".
//
// Resume-preservation (T-alive, 2026-08-07): when cfg.ResumeID is set,
// the spawn is probed for stderr-detected rejection signals (see
// classifyStderrLineForResume). On detection, the bridge now RETURNS
// ErrResumeUnhealthy instead of silently falling back to a fresh
// session. The previous fallback behavior dropped the user's resume
// context — the runtime saw a "working" session but the assistant had
// no memory of the prior conversation, which the user explicitly
// required us to preserve.
//
// On Start success, the returned session has an active process; the
// caller must Close() it when done.
func (a *Agent) Start(ctx context.Context, cfg agent.StartConfig) (agent.AgentSession, error) {
	sess, err := a.startOnce(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if cfg.ResumeID == "" {
		return sess, nil
	}
	if reason, unhealthy := probeResume(ctx, sess); unhealthy {
		slog.Warn("claudecode: --resume spawn unhealthy; refusing fallback to preserve resume context",
			"resume_id", cfg.ResumeID, "reason", reason)
		_ = sess.Close()
		return nil, fmt.Errorf("%w: %s (session_id=%s); check workspace path and resume id",
			ErrResumeUnhealthy, reason, cfg.ResumeID)
	}
	return sess, nil
}

// startOnce is the bare spawn. Single call site: Agent.Start.
// Kept private so the resume-preservation decision lives in Start.
func (a *Agent) startOnce(ctx context.Context, cfg agent.StartConfig) (agent.AgentSession, error) {
	args := buildArgs(a.args, cfg)

	env := append([]string(nil), cfg.Env...)
	env = append(env, a.command) // ensure command name is in env (defensive)

	return newSession(ctx, a.name, a.command, args, env, cfg.Workspace)
}

// probeResume monitors the session's stderr stream for resume-rejection
// signals within resumeFallbackTimeout. STDERR-ONLY by design (see
// resumeFallbackTimeout doc) — earlier versions also read
// sess.Events() but that channel is shared with the AS readpump
// and the two consumers race for events.
//
// Returns (reason, true) if a rejection signal is observed within
// the probe window. Returns ("", false) if:
//   - stderr emits no rejection signal AND the window expires
//     (process alive but quiet → assume healthy)
//   - stderr drainer closes within the window without a rejection
//     (process died cleanly — not a resume-specific issue)
//
// Note: this probe does NOT validate that the spawn is
// "responding" — that's the AS readpump's job (it sees init,
// text, result, etc.). The probe only checks for KNOWN-BAD
// stderr signals and for process death during the probe window.
func probeResume(ctx context.Context, sess agent.AgentSession) (string, bool) {
	stderrCh := stderrLinesOf(sess)
	if stderrCh == nil {
		// Defensive fallback — every claudecode.session MUST
		// expose stderr per the newSession contract. If this
		// ever fires, the session is missing a feature;
		// surface it loudly rather than silently proceeding.
		slog.Error("claudecode: session has no stderr channel; resume probe disabled")
		return "", false
	}

	type probeResult struct {
		healthy bool
		reason  string
	}
	done := make(chan probeResult, 1)
	go func() {
		defer close(done)

		safetyCtx, safetyCancel := context.WithTimeout(ctx, resumeFallbackTimeout)
		defer safetyCancel()

		for {
			select {
			case <-safetyCtx.Done():
				// stderr drainer closed within the window
				// (process died) OR window expired (process
				// alive but no signal — we let it through).
				// stderr-only probe: no signal = healthy.
				done <- probeResult{healthy: true, reason: "no_stderr_signal_within_window"}
				return
			case line, ok := <-stderrCh:
				if !ok {
					// stderr drainer exited; spawn is done.
					// No rejection observed → healthy.
					done <- probeResult{healthy: true, reason: "stderr_drained_no_signal"}
					return
				}
				if reason, isBad := classifyStderrLineForResume(line); isBad {
					done <- probeResult{healthy: false, reason: reason}
					return
				}
			}
		}
	}()
	res := <-done
	if !res.healthy {
		slog.Warn("claudecode: resume spawn unhealthy",
			"reason", res.reason)
	}
	return res.reason, !res.healthy
}

// stderrLinesOf returns a channel of stderr lines emitted by the
// session, or nil if the bridge doesn't expose one. The
// claudecode.session implementation exposes a buffered channel
// that the drainStderr goroutine pushes lines into as it reads
// them; the channel is closed when drainStderr exits.
//
// Returning nil lets the probe fall back to event-only
// detection — defensive in case a future bridge doesn't
// implement the surface.
func stderrLinesOf(sess agent.AgentSession) <-chan string {
	type stderrExposer interface {
		StderrLines() <-chan string
	}
	if e, ok := sess.(stderrExposer); ok {
		return e.StderrLines()
	}
	return nil
}

// classifyStderrLineForResume examines one stderr line and
// returns (reason, true) if it indicates --resume rejection that
// the bridge should surface to the user.
//
// T-alive (2026-08-07): MCP server failure signals were REMOVED
// from this classifier. Previous versions treated stderr lines
// like "Failed to connect MCP server ..." as a probe trigger, which
// combined with the silent fallback to silently drop the user's
// resume context whenever an MCP server was misconfigured. MCP
// server failures do NOT prevent claude from emitting init or
// processing the user message (verified 2026-08-07: a session with
// most MCP servers in failed/pending state still emits init at
// ~1s with the correct session_id). Such failures are
// informational and should be logged, not used to invalidate the
// resume.
//
// Verified resume-rejection shapes (claude 2.1.220, 2026-08-07):
//
//	"No conversation found with session ID: <uuid>"
//	"--resume requires a valid session ID or session title..."
func classifyStderrLineForResume(line string) (string, bool) {
	l := strings.ToLower(line)
	if l == "" {
		return "", false
	}
	// --resume rejection signals only.
	if strings.Contains(l, "session") && (strings.Contains(l, "not found") ||
		strings.Contains(l, "no conversation found") ||
		strings.Contains(l, "resume requires")) {
		return "stderr_resume_rejection", true
	}
	return "", false
}

// isResumeErrorMessage returns true when the stream-json result
// event's Text (which carries the joined errors[] payload) looks
// like claude's "invalid --resume" diagnostic.
//
// Per claude 2.1.220 (verified 2026-08-07), the two known shapes
// are:
//
//	"No conversation found with session ID: <uuid>"
//	"--resume requires a valid session ID or session title..."
//
// We match case-insensitive on the substrings "session" + ("not
// found" or "no conversation found" or "resume requires"). Loose
// enough to cover future rewordings, tight enough not to
// false-positive on unrelated errors.
func isResumeErrorMessage(text string) bool {
	t := strings.ToLower(text)
	if !strings.Contains(t, "session") {
		return false
	}
	return strings.Contains(t, "not found") ||
		strings.Contains(t, "no conversation found") ||
		strings.Contains(t, "resume requires")
}

// buildArgs concatenates DefaultArgs + extraArgs + cfg.Args, rewriting
// the --permission-mode placeholder baked into DefaultArgs with the
// effective mode from cfg.PermissionMode (PermissionBypass when empty).
// When cfg.ResumeID is non-empty, `--resume <id>` is appended last so
// user-supplied cfg.Args are visible to the user before the resume flag.
//
// Extracted as a package-private helper so tests can assert on the
// produced argv without spawning a process. Mirrors the contract of
// Agent.Start exactly.
func buildArgs(extraArgs []string, cfg agent.StartConfig) []string {
	mode := cfg.PermissionMode
	if mode == "" {
		mode = PermissionBypass
	}

	// Walk DefaultArgs; when we see "--permission-mode" the next
	// element is the placeholder — replace it with the effective
	// mode instead of copying the placeholder verbatim.
	out := make([]string, 0, len(DefaultArgs)+len(extraArgs)+len(cfg.Args)+2)
	for i := 0; i < len(DefaultArgs); i++ {
		if DefaultArgs[i] == "--permission-mode" && i+1 < len(DefaultArgs) {
			out = append(out, "--permission-mode", mode)
			i++ // skip the placeholder value
			continue
		}
		out = append(out, DefaultArgs[i])
	}
	out = append(out, extraArgs...)
	out = append(out, cfg.Args...)
	if cfg.ResumeID != "" {
		out = append(out, "--resume", cfg.ResumeID)
	}
	return out
}
