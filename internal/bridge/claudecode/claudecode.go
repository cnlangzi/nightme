
// Package claudecode implements a bridge to the Claude Code CLI
// using stream-json mode over stdin/stdout. See
// docs/feat/F-24-claudecode-bridge.md.
//
// Agent is BOTH the template (registered with agent.Builtins) and the
// live handle (returned by Start). The template half is set once by
// New and is immutable thereafter; Start clones the receiver and
// populates runtime fields on the clone. The two states share one
// type so the registry, the Spawner, and AgentSession.handle all
// deal with a single agent.Agent — no separate session struct.
package claudecode

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// ─── constants & exported errors ───

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

// eventsBufferSize is the capacity of the AgentEvent channel.
//
// Sized large enough to absorb a sustained backlog rather than
// dropping. The send sites in stream.go's translate() and elsewhere
// use bare `events <- ev` (no select, no default drop), so the
// producer is allowed to block until the consumer drains — the
// natural pipe back-pressure to the upstream claude PTY bounds
// memory in practice. Matches the producer-side contract across
// pi / acp / pty (no timeout, no default-drop).
//
// Allocated in startOnce; buffer_contract_test.go pins the value at
// the package level so a regression that lowers the cap or
// introduces a `default:` drop is caught in `go test`.
const eventsBufferSize = 40960

// pendingAsk is the in-flight AskUserQuestion state. We keep it here
// (rather than only on the EventAgentPermission) so Session.SendPermission
// can serialize the user's answer back to stdin even if the channel
// dropped its copy of the response channel.
type pendingAsk struct {
	ToolUseID  string
	Multi      bool
	ResponseCh chan string
}

// ─── driver struct (runtime + protocol) ───

// driver is the claudecode runtime handle. After the
// starter+driver split (P2 pilot, wip/agent.md), the template
// fields (name/command/args) live on starter; this struct holds
// only runtime state populated by starter.Start. Implements the
// agent.driver interface (5 methods, package-private) plus a few
// bridge-internal helpers (readPump, drainStderr, writeLine).
//
// driver is package-private. External code interacts through
// starter.Start returning *agent.Agent which delegates the
// public Send*/Events/Close methods back here.
//
// Two states share one type:
//
//   - Live state (after Start, before Close): the receiver is a
//     freshly-allocated clone with runtime fields populated (cmd,
//     stdin, events, stderrLines, pumpWG, pendingMu/Ask, ...).
//     Calls to Events / PID / Send* / New / Close are valid here.
//     Spec-half fields are still readable.
//
// The template (in Builtins) is never mutated; Start returns a
// separate *driver so concurrent Start calls from different chats do
// not interfere with each other.
type driver struct {
	// ─── runtime fields (zero before Start; populated by newDriver) ───
	cmd     *exec.Cmd
	stdin   *bufio.Writer
	stdinMu sync.Mutex // guards Write on the underlying pipe
	events  chan agent.AgentEvent
	pid     int

	// stderrLines captured line-by-line by drainStderr and pushed
	// to this buffered channel so the resume-fallback probe can
	// listen for --resume rejections and MCP server failures
	// without parsing the bridge's stderr-drain log lines.
	// Buffered so a slow probe can't wedge drainStderr. Closed by
	// drainStderr when EOF arrives.
	stderrLines chan string

	// pumpWG tracks the pumpStream goroutine lifecycle so Close
	// can wait for it to finish before allowing the events channel
	// to be closed.
	pumpWG sync.WaitGroup

	// agentName + workspace + branch are captured at session start
	// so the translate goroutine can stamp them onto the EventAgentReady
	// payload. All three are immutable for the session's lifetime.
	agentName string
	workspace string
	branch    string

	// pendingAsk is set when an AskUserQuestion EventAgentPermission
	// is emitted; SendPermission reads from it. nil means "no
	// pending question".
	pendingMu  sync.Mutex
	pendingAsk *pendingAsk

	closeOnce sync.Once
	closed    chan struct{}
}

// ─── lifecycle ───

// newDriver spawns the process and returns a fully-wired driver.
// This is what starter.Start calls; the resume-preservation probe
// (probeResume) sees the live driver before starter.Start wraps
// it in a *agent.Agent. Split out of the old Agent.Start so
// the probe can interact with the runtime before it's exposed.
func newDriver(ctx context.Context, s *Starter, cfg agent.StartConfig) (*driver, error) {
	args := buildArgs(s.args, cfg)

	env := append([]string(nil), cfg.Env...)
	env = append(env, s.command) // ensure command name is in env (defensive)

	// Spawn via the agent.NewCmd helper so the platform-specific
	// SysProcAttr (Setsid on unix, no-op on Windows) is in one
	// place — see internal/agent/exec_unix.go for the rationale.
	cmd := agent.NewCmd(ctx, s.command, args...)
	cmd.Dir = cfg.Workspace
	cmd.Env = append(os.Environ(), env...)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("claudecode: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("claudecode: stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, fmt.Errorf("claudecode: stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, fmt.Errorf("claudecode: start: %w", err)
	}

	branch := detectBranch(cfg.Workspace)

	live := &driver{
		cmd:         cmd,
		stdin:       bufio.NewWriter(stdin),
		events:      make(chan agent.AgentEvent, eventsBufferSize),
		stderrLines: make(chan string, 64),
		pid:         cmd.Process.Pid,
		agentName:   s.name,
		workspace:   cfg.Workspace,
		branch:      branch,
		closed:      make(chan struct{}),
	}

	// Register pendingAsk bridge for AskUserQuestion: the default
	// handler emits EventAgentPermission with our own ResponseCh so
	// SendPermission routes the user's answer back through the
	// same channel the bridge consumed from.
	handler := func(block contentBlock, events chan<- agent.AgentEvent, logger *slog.Logger) {
		var probe struct {
			Questions []struct {
				Header      string `json:"header"`
				MultiSelect bool   `json:"multiSelect"`
			} `json:"questions"`
		}
		_ = json.Unmarshal(block.Input, &probe)

		var multi bool
		if len(probe.Questions) > 0 {
			multi = probe.Questions[0].MultiSelect
		}
		respCh := make(chan string, 1)
		live.pendingMu.Lock()
		live.pendingAsk = &pendingAsk{
			ToolUseID:  block.ID,
			Multi:      multi,
			ResponseCh: respCh,
		}
		live.pendingMu.Unlock()

		// Translate the tool_use block. We need the resulting
		// EventAgentPermission to expose the SAME ResponseCh we just
		// stored in pendingAsk, otherwise the channel layer will
		// write to a stale channel and SendPermission will block
		// forever. To do that without re-parsing, we override
		// the ResponseCh on the most recent emitted event by
		// intercepting the channel output.
		interceptEvents := make(chan agent.AgentEvent, 4)
		done := make(chan struct{})
		go func() {
			defer close(done)
			for ev := range interceptEvents {
				if ev.Kind == agent.EventAgentPermission && ev.Permission != nil {
					ev.Permission.ResponseCh = respCh
				}
				events <- ev
			}
		}()
		defaultAskHandler(block, interceptEvents, logger)
		close(interceptEvents)
		<-done
	}

	logger := slog.Default()
	// pumpStream owns the events-channel close so Close() can wait
	// for it to drain (via pumpWG) before allowing close.
	live.pumpWG.Add(1)
	go func() {
		defer live.pumpWG.Done()
		defer close(live.events)
		pumpStream(stdout, live.events, handler, live.agentName, live.workspace, live.branch, logger)
	}()

	// stderr drainer — Claude Code logs to stderr; we both log
	// (debug) and forward each line to live.stderrLines so the
	// --resume fallback probe can react to deterministic stderr
	// signals.
	go func() {
		_ = drainStderr(stderr, logger, live.stderrLines)
	}()

	return live, nil
}

// ─── live-half methods ───

// Events returns the live event stream. Closed by Close() (via the
// pumpStream goroutine's defer).
func (d *driver) Events() <-chan agent.AgentEvent { return d.events }

// StderrLines exposes the per-line stderr stream captured by
// drainStderr. The --resume fallback probe reads from this channel
// to detect deterministic stderr signals without relying on
// timeout. Returns nil if drainStderr hasn't been wired (should not
// happen for sessions produced by Start).
func (d *driver) StderrLines() <-chan string { return d.stderrLines }

// PID returns the OS process id of the child.
func (d *driver) PID() int { return d.pid }

// SendBlocks writes a structured user turn to Claude Code's stdin
// in stream-json format. Each block is encoded into an Anthropic-API
// content-array element:
//
//	{type:"text",    text:"..."}              for ContentText
//	{type:"image",   source:{type:"base64",
//	                         media_type:"image/png",
//	                         data:"..."}}    for ContentImage
//	{type:"document",source:{type:"base64",
//	                         media_type:"application/pdf",
//	                         data:"..."}}    for ContentFile (PDF only)
//
// Non-image, non-PDF files fall back to a text-block annotation so
// the agent knows the file exists and can read it via its file
// tools.
//
// Empty blocks slice is a no-op. Image/file blocks whose Path does
// not exist are logged at warn level and dropped.
func (d *driver) SendBlocks(ctx context.Context, blocks []agent.ContentBlock) error {
	_ = ctx
	if len(blocks) == 0 {
		return nil
	}

	content := make([]map[string]any, 0, len(blocks))
	for _, b := range blocks {
		switch b.Type {
		case agent.ContentText:
			if b.Text == "" {
				continue
			}
			content = append(content, map[string]any{
				"type": "text",
				"text": b.Text,
			})

		case agent.ContentImage:
			if b.Path == "" {
				continue
			}
			info, statErr := os.Stat(b.Path)
			if statErr != nil {
				slog.Default().Warn("claudecode: skip image block (stat failed)",
					"path", b.Path, "err", statErr)
				continue
			}
			if info.Size() > 5*1024*1024 {
				slog.Default().Warn("claudecode: image exceeds 5MB, falling back to text annotation",
					"path", b.Path, "size", info.Size())
				content = append(content, map[string]any{
					"type": "text",
					"text": fmt.Sprintf("Image (too large to inline, %d bytes): %s", info.Size(), b.Path),
				})
				continue
			}
			encoded, err := readFileAsBase64(b.Path)
			if err != nil {
				slog.Default().Warn("claudecode: skip image block (read/encode failed)",
					"path", b.Path, "size", info.Size(), "err", err)
				continue
			}
			slog.Default().Debug("claudecode: inline image",
				"path", b.Path, "size", info.Size(), "media_type", b.MediaType)
			content = append(content, map[string]any{
				"type": "image",
				"source": map[string]any{
					"type":       "base64",
					"media_type": b.MediaType,
					"data":       encoded,
				},
			})

		case agent.ContentFile:
			if b.Path == "" {
				continue
			}
			// Anthropic API inline file support is PDF-only. For
			// PDFs we inline; for everything else we emit a text
			// annotation so the agent can `Read` the file.
			if b.MediaType == "application/pdf" {
				encoded, err := readFileAsBase64(b.Path)
				if err != nil {
					slog.Default().Warn("claudecode: skip file block",
						"path", b.Path, "err", err)
					continue
				}
				content = append(content, map[string]any{
					"type": "document",
					"source": map[string]any{
						"type":       "base64",
						"media_type": "application/pdf",
						"data":       encoded,
					},
				})
			} else {
				content = append(content, map[string]any{
					"type": "text",
					"text": fmt.Sprintf("File: %s", b.Path),
				})
			}

		default:
			slog.Default().Warn("claudecode: skip unknown block", "type", b.Type)
		}
	}
	if len(content) == 0 {
		return nil
	}

	msg := map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": content,
		},
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("claudecode: marshal user msg: %w", err)
	}
	return d.writeLine(data)
}

// readFileAsBase64 reads the file at path and returns its contents
// base64-encoded. Errors include the path for log readability.
func readFileAsBase64(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	return base64.StdEncoding.EncodeToString(data), nil
}

// SendPermission writes the user's answer to the most recent
// AskUserQuestion prompt as a tool_result user message. resp is the
// option label the user selected.
//
// Blocks until the pending question is resolved.
func (d *driver) SendPermission(resp string) error {
	d.pendingMu.Lock()
	ask := d.pendingAsk
	d.pendingAsk = nil
	d.pendingMu.Unlock()

	if ask == nil {
		return fmt.Errorf("claudecode: no pending AskUserQuestion")
	}

	var selected []string
	if ask.Multi {
		for _, part := range splitAndTrim(resp, ",") {
			if part != "" {
				selected = append(selected, part)
			}
		}
	} else {
		selected = []string{resp}
	}

	data, err := encodeUserAnswer(ask.ToolUseID, selected, ask.Multi)
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return nil
	}
	return d.writeLine(data)
}

// New resets the conversation context on the running session without
// terminating the underlying process. F-34 §3.2.1 + Phase 3 final
// verification 2026-08-04 (live claude binary, stream-json mode):
//
// Claude Code honors the `/clear` slash command when sent as a
// `user`-typed message in stream-json mode. Empirically observed:
//
//   - Round 1 (memory plant): {"type":"user","message":{...,"content":"Remember 77777"}}
//   - Round 2 (recall): assistant returned "77777"
//   - Round 3 (/clear): assistant returned "NONE" (memory cleared)
//
// Therefore: write a properly-structured user message whose content
// is literally "/clear" via writeLine. In-process reset: process
// stays alive, transport stays open, Events() stays open, PID
// stays the same.
// Reset is the agent.driver interface name for New. Implements
// the agent.driver Reset method (F-34).
func (d *driver) Reset(ctx context.Context) error { return d.New(ctx) }

func (d *driver) New(ctx context.Context) error {
	_ = ctx
	if d.closed == nil {
		return fmt.Errorf("claudecode: session not initialized")
	}
	select {
	case <-d.closed:
		return fmt.Errorf("claudecode: session closed")
	default:
	}
	payload := []byte(`{"type":"user","message":{"role":"user","content":"/clear"}}`)
	return d.writeLine(payload)
}

// Stop sends SIGINT to the claude code child. The stream-json
// protocol does not expose a structured cancel, so the closest
// portable action is the same Ctrl-C signal a user would press.
// In --output-format stream-json pipe mode the child's SIGINT
// behavior is best-effort: it may either emit a final `result`
// event with is_error=true (which the bridge translates to
// EventAgentDone, IsReady flips) or it may simply exit on the
// signal (the events channel closes, the bridge sets the AS to
// StatusExited, the next Submit triggers a respawn via
// LookupSelectedAgentSession). Either way the chat layer's
// TryFlush loop picks up the next queued prompt — Stop itself
// does not need to coordinate the turn-end → next-submit
// transition.
//
// Stop is fire-and-forget: it does NOT block waiting for a clean
// EventAgentDone. The caller does not need to know which of the
// two post-states the child landed in.
//
// Returns ErrNotSupported if the bridge is not started.
func (d *driver) Stop(ctx context.Context) error {
	_ = ctx
	if d.cmd == nil || d.cmd.Process == nil {
		return agent.ErrNotSupported
	}
	return agent.SignalProcessGroup(d.cmd.Process, os.Interrupt)
}

// SetModel is not supported on the claudecode bridge. The
// stream-json protocol does not expose a model swap mechanism.
// Operators who want a different model must restart the bridge
// with the desired `--model` flag.
func (d *driver) SetModel(ctx context.Context, providerID, modelID string) error {
	_ = ctx
	_ = providerID
	_ = modelID
	return agent.ErrNotSupported
}

// Close terminates the session: closes stdin (so the child sees EOF
// and exits cleanly), waits briefly for graceful shutdown, then
// SIGKILLs if necessary. Idempotent.
func (d *driver) Close() error {
	var firstErr error
	d.closeOnce.Do(func() {
		close(d.closed)

		// Close stdin so claude sees EOF and exits.
		d.stdinMu.Lock()
		_ = d.stdin.Flush()
		d.stdinMu.Unlock()

		if d.cmd != nil && d.cmd.Process != nil {
			// Broadcast to the whole process group so a Bash
			// tool subprocess doesn't keep the kernel pipe alive
			// past the cli's own exit.
			_ = agent.SignalProcessGroup(d.cmd.Process, os.Interrupt)
		}

		// Wait up to 2s for graceful exit; SIGKILL after.
		done := make(chan struct{})
		go func() {
			if d.cmd != nil {
				_ = d.cmd.Wait()
			}
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			if d.cmd != nil && d.cmd.Process != nil {
				_ = d.cmd.Process.Kill()
			}
			<-done
		}

		// Wait for pumpStream to drain + close the events channel
		// itself (via defer in startOnce).
		d.pumpWG.Wait()
	})
	return firstErr
}

// writeLine writes a single JSON line to claude's stdin followed by
// \n. The mutex serializes writes (multiple SendBlocks calls in
// flight).
func (d *driver) writeLine(data []byte) error {
	d.stdinMu.Lock()
	defer d.stdinMu.Unlock()

	if _, err := d.stdin.Write(data); err != nil {
		return fmt.Errorf("claudecode: write stdin: %w", err)
	}
	if _, err := d.stdin.WriteString("\n"); err != nil {
		return fmt.Errorf("claudecode: write newline: %w", err)
	}
	if err := d.stdin.Flush(); err != nil {
		return fmt.Errorf("claudecode: flush stdin: %w", err)
	}
	return nil
}

// ─── resume probe + helpers (package-level) ───

// probeResume monitors the session's stderr stream for resume-rejection
// signals within resumeFallbackTimeout. STDERR-ONLY by design.
func probeResume(ctx context.Context, sess *driver) (string, bool) {
	stderrCh := sess.StderrLines()
	if stderrCh == nil {
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
				for {
					select {
					case line, ok := <-stderrCh:
						if !ok {
							done <- probeResult{healthy: true, reason: "stderr_drained_no_signal"}
							return
						}
						if reason, isBad := classifyStderrLineForResume(line); isBad {
							done <- probeResult{healthy: false, reason: reason}
							return
						}
					default:
						done <- probeResult{healthy: true, reason: "no_stderr_signal_within_window"}
						return
					}
				}
			case line, ok := <-stderrCh:
				if !ok {
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

// classifyStderrLineForResume examines one stderr line and returns
// (reason, true) if it indicates --resume rejection.
func classifyStderrLineForResume(line string) (string, bool) {
	l := strings.ToLower(line)
	if l == "" {
		return "", false
	}
	if strings.Contains(l, "session") && (strings.Contains(l, "not found") ||
		strings.Contains(l, "no conversation found") ||
		strings.Contains(l, "resume requires")) {
		return "stderr_resume_rejection", true
	}
	return "", false
}

// isResumeErrorMessage returns true when the stream-json result
// event's Text looks like claude's "invalid --resume" diagnostic.
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
// effective mode from cfg.PermissionMode (PermissionBypass when
// empty). When cfg.SessionID is non-empty, `--resume <id>` is
// appended last.
func buildArgs(extraArgs []string, cfg agent.StartConfig) []string {
	mode := cfg.PermissionMode
	if mode == "" {
		mode = PermissionBypass
	}

	out := make([]string, 0, len(DefaultArgs)+len(extraArgs)+len(cfg.Args)+2)
	for i := 0; i < len(DefaultArgs); i++ {
		if DefaultArgs[i] == "--permission-mode" && i+1 < len(DefaultArgs) {
			out = append(out, "--permission-mode", mode)
			i++
			continue
		}
		out = append(out, DefaultArgs[i])
	}
	out = append(out, extraArgs...)
	out = append(out, cfg.Args...)
	if cfg.SessionID != "" {
		out = append(out, "--resume", cfg.SessionID)
	}
	return out
}

// ─── misc helpers (package-level) ───

// drainStderr reads stderr line-by-line until EOF. Each non-empty
// line is forwarded to lines (best-effort: a slow consumer is
// dropped, never blocked) AND logged. Lines matching the
// --resume / MCP failure patterns get elevated to Warn so they
// show up in default-level daemon logs; the rest stay at Debug
// to keep the log readable. The lines channel is closed when EOF
// arrives.
func drainStderr(r interface {
	Read(p []byte) (int, error)
}, logger *slog.Logger, lines chan<- string) error {
	defer func() {
		if lines != nil {
			close(lines)
		}
	}()
	if r == nil {
		return nil
	}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 4096), 64*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		if logger != nil {
			level := slog.LevelDebug
			if isLoudStderr(line) {
				level = slog.LevelWarn
			}
			logger.Log(context.Background(), level, "claudecode stderr", "line", line)
		}
		if lines != nil {
			select {
			case lines <- line:
			default:
				// Probe consumer is slow; drop. We log anyway
				// and the dropped line still surfaces via the
				// elevated logger level.
			}
		}
	}
	return nil
}

// isLoudStderr returns true when a stderr line carries a
// resume-rejection or MCP-failure signal that operators need to
// see at default log level.
func isLoudStderr(line string) bool {
	l := strings.ToLower(line)
	if l == "" {
		return false
	}
	if strings.Contains(l, "session") && (strings.Contains(l, "not found") ||
		strings.Contains(l, "no conversation found") ||
		strings.Contains(l, "resume requires")) {
		return true
	}
	if (strings.Contains(l, "mcp") || strings.Contains(l, "tool")) &&
		(strings.Contains(l, "failed") ||
			strings.Contains(l, "timeout") ||
			strings.Contains(l, "refused") ||
			strings.Contains(l, "unreachable") ||
			strings.Contains(l, "error")) {
		return true
	}
	return false
}

func trimTrailingNewline(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

// splitAndTrim splits s by sep and trims whitespace from each piece.
// Empty pieces are removed.
func splitAndTrim(s, sep string) []string {
	parts := splitString(s, sep)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = trimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// splitString + trimSpace are tiny helpers to avoid importing strings
// from this file just for two calls.
func splitString(s, sep string) []string {
	if s == "" {
		return nil
	}
	var out []string
	start := 0
	for i := 0; i+len(sep) <= len(s); i++ {
		if s[i:i+len(sep)] == sep {
			out = append(out, s[start:i])
			start = i + len(sep)
			i += len(sep) - 1
		}
	}
	out = append(out, s[start:])
	return out
}

func trimSpace(s string) string {
	start := 0
	end := len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t' || s[start] == '\n' || s[start] == '\r') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t' || s[end-1] == '\n' || s[end-1] == '\r') {
		end--
	}
	return s[start:end]
}

// detectBranch returns the current git branch for workspace, or ""
// if the workspace is not a git repo / git is unavailable / the
// command errors out. Uses `git -C ws symbolic-ref --short HEAD`.
func detectBranch(workspace string) string {
	if workspace == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git",
		"-C", workspace,
		"symbolic-ref", "--short", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	branch := trimSpace(string(out))
	if branch == "" || branch == "HEAD" {
		return ""
	}
	return branch
}

// Compile-time guarantee that *driver satisfies the package-private
// agent.driver interface (SendBlocks/SendPermission/Reset/Close).
// External callers reach driver via *agent.Agent, which forwards
// the public methods. The package-private starter half is type-checked
// in starter.go via the same agentDriver interface declaration.
var _ agentDriver = (*driver)(nil)

// agentDriver is the local alias for the agent.driver interface so
// this file can do a compile-time check that driver satisfies it
// without importing the unexported name from the agent package.
type agentDriver interface {
	SendBlocks(ctx context.Context, blocks []agent.ContentBlock) error
	SendPermission(resp string) error
	Reset(ctx context.Context) error
	Close() error
}
