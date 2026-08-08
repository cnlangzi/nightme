package claudecode

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// session is the runtime handle for one Claude Code invocation. It owns
// the child process, the JSON event pump goroutine, and the
// AskUserQuestion permission flow.
//
// Implements agent.AgentSession. Safe for concurrent calls to SendText
// (writes are serialized via a mutex on stdin).
type session struct {
	cmd     *exec.Cmd
	stdin   *bufio.Writer
	stdinMu sync.Mutex // guards Write on the underlying pipe
	events  chan agent.AgentEvent
	pid     int

	// T-alive (2026-08-07): stderr lines captured line-by-line
	// by drainStderr and pushed to this buffered channel so the
	// resume-fallback probe can listen for --resume rejections
	// and MCP server failures without parsing the bridge's
	// stderr-drain log lines. Buffered so a slow probe can't
	// wedge drainStderr. Closed by drainStderr when EOF arrives.
	stderrLines chan string

	// pumpWG tracks the pumpStream goroutine lifecycle so Close
	// can wait for it to finish before allowing the events channel
	// to be closed. Before this existed, Close() called
	// close(s.events) directly while pumpStream was still in its
	// post-Scan teardown (scanner.Err check + final EventAgentError
	// emit), racing a send on a closed channel — see the
	// TestClaudeCodeBridge_RealSubprocess race that surfaced in
	// PR #53's CI. pumpStream owns the events-channel close via
	// `defer close(s.events)`; Close only waits on pumpWG.
	pumpWG sync.WaitGroup

	// agentName + workspace + branch are captured at session start
	// so the translate goroutine can stamp them onto the EventAgentReady
	// payload (consumed by channel-layer receipt rendering for
	// the "Agent | repo | branch | tokens" foot note). All
	// three are immutable for the session's lifetime so no
	// mutex is needed.
	agentName string
	workspace string
	branch    string

	// pendingAsk is set when an AskUserQuestion EventAgentPermission is
	// emitted; SendPermission reads from it (matching ResponseCh).
	// nil means "no pending question".
	pendingMu  sync.Mutex
	pendingAsk *pendingAsk

	closeOnce sync.Once
	closed    chan struct{}
}

// pendingAsk is the in-flight AskUserQuestion state. We keep it here
// (rather than only on the EventAgentPermission) so Session.SendPermission
// can serialize the user's answer back to stdin even if the channel
// dropped its copy of the response channel.
type pendingAsk struct {
	ToolUseID  string
	Multi      bool
	ResponseCh chan string
}

// newSession spawns `claude` with args + env, then starts the JSON
// event pump goroutine. The returned AgentSession is ready for
// SendText / Events immediately on success.
//
// agentName + workspace + branch are stamped onto every EventAgentReady
// emitted by the pump so channel-layer receipts can render the
// "Agent | repo | branch | tokens" foot note. The values are
// stable for the session's lifetime (set once at start).
//
// branch is captured by running `git -C workspace symbolic-ref
// --short HEAD` (or the equivalent rev-parse). Failure is
// non-fatal — the branch is just left empty and the receipt's
// foot note omits that segment. We run this BEFORE spawning the
// child so a slow `git` invocation doesn't block receipt init.
func newSession(ctx context.Context, agentName, command string, args, env []string, workspace string) (agent.AgentSession, error) {
	if workspace == "" {
		return nil, fmt.Errorf("claudecode: workspace is required")
	}

	branch := detectBranch(workspace)

	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = workspace
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

	s := &session{
		cmd:         cmd,
		stdin:       bufio.NewWriter(stdin),
		events:      make(chan agent.AgentEvent, 64),
		stderrLines: make(chan string, 64),
		pid:         cmd.Process.Pid,
		agentName:   agentName,
		workspace:   workspace,
		branch:      branch,
		closed:      make(chan struct{}),
	}

	// Register pendingAsk bridge for AskUserQuestion: the default
	// handler emits EventAgentPermission with our own ResponseCh so
	// SendPermission routes the user's answer back through the
	// same channel the bridge consumed from.
	handler := func(block contentBlock, events chan<- agent.AgentEvent, logger *slog.Logger) {
		// Pre-decode just enough to capture the ResponseCh + multi
		// flag. The full Options parsing happens inside the
		// default handler below; we only need the channel binding
		// to be stable across handler invocation and SendPermission
		// delivery.
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
		s.pendingMu.Lock()
		s.pendingAsk = &pendingAsk{
			ToolUseID:  block.ID,
			Multi:      multi,
			ResponseCh: respCh,
		}
		s.pendingMu.Unlock()

		// Translate the tool_use block. We need the resulting
		// EventAgentPermission to expose the SAME ResponseCh we just
		// stored in pendingAsk, otherwise the channel layer will
		// write to a stale channel and SendPermission will block
		// forever. To do that without re-parsing, we override
		// the ResponseCh on the most recent emitted event by
		// intercepting the channel output.
		//
		// Strategy: run the default handler with a wrapper
		// channel that intercepts the EventAgentPermission and
		// rewrites its ResponseCh to our pending channel.
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
	// for it to drain (via pumpWG) before allowing close. Pre-fix
	// Close closed the channel directly, racing pumpStream's
	// post-Scan EventAgentError emit. See PR #53 CI race trace.
	s.pumpWG.Add(1)
	go func() {
		defer s.pumpWG.Done()
		defer close(s.events)
		pumpStream(stdout, s.events, handler, s.agentName, s.workspace, s.branch, logger)
	}()

	// stderr drainer — Claude Code logs to stderr; we both log
	// (debug) and forward each line to s.stderrLines so the
	// --resume fallback probe can react to deterministic stderr
	// signals ("No conversation found..." / MCP failures) without
	// machine-dependent timeouts.
	go func() {
		_ = drainStderr(stderr, logger, s.stderrLines)
	}()

	// NOTE: an earlier revision of this code spawned a watchdog
	// goroutine that called cmd.Wait() and was supposed to close
	// the events channel on process exit. The body was a no-op
	// (the comment said "events channel will be closed by Close()")
	// so the goroutine only existed to call cmd.Wait() — which is
	// also called from Close(), creating a documented data race
	// against the Wait field of exec.Cmd. pumpStream is the real
	// watchdog: it sends EventAgentDone on EOF, and Close() is the
	// single owner of cmd.Wait(). Remove the redundant goroutine.

	return s, nil
}

// Events returns the live event stream. Closed by Close().
func (s *session) Events() <-chan agent.AgentEvent { return s.events }

// StderrLines (T-alive 2026-08-07) exposes the per-line stderr
// stream captured by drainStderr. The --resume fallback probe
// reads from this channel to detect deterministic stderr signals
// (--resume rejection / MCP server failure) without relying on
// timeout. Returns nil if drainStderr hasn't been wired (should
// not happen for sessions produced by newSession).
func (s *session) StderrLines() <-chan string { return s.stderrLines }

// PID returns the OS process id of the child.
func (s *session) PID() int { return s.pid }

// SendText writes a user message to Claude Code's stdin in
// stream-json format. Each call emits one user message; Claude Code
// batches multiple user messages per turn until it sees a "result".
//
// SendText is a convenience wrapper around SendBlocks for the
// text-only path. Implementations that need image / file
// attachments must use SendBlocks directly.
func (s *session) SendText(text string) error {
	if text == "" {
		return nil
	}
	return s.SendBlocks(context.Background(), []agent.ContentBlock{
		{Type: agent.ContentText, Text: text},
	})
}

// SendBlocks writes a structured user turn to Claude Code's stdin in
// stream-json format. Each block is encoded into an Anthropic-API
// content-array element:
//
//	{type:"text",    text:"..."}              for ContentText
//	{type:"image",   source:{type:"base64",
//	                         media_type:"image/png",
//	                         data:"..."}}    for ContentImage (file base64-inlined)
//	{type:"document",source:{type:"base64",
//	                         media_type:"application/pdf",
//	                         data:"..."}}    for ContentFile (PDF only)
//
// Non-image, non-PDF files fall back to a text-block annotation
// "File: <path>" so the agent knows the file exists and can read it
// via its file tools. (Anthropic API only supports images and PDFs
// inline; other MIME types are accepted as text references.)
//
// Empty blocks slice is a no-op. Image/file blocks whose Path does
// not exist are logged at warn level and dropped — sending a
// half-broken content array is worse than silently degrading.
func (s *session) SendBlocks(ctx context.Context, blocks []agent.ContentBlock) error {
	if len(blocks) == 0 {
		return nil
	}
	// Derive a per-call ctx so the bridge layer owns its own
	// cancellation surface. Claudecode's write path is
	// fire-and-forget (no ack wait), so callCtx mostly exists for
	// symmetry with pi / acp — future bridge-level semantics
	// (per-call timeout, retry budget, trace values) attach here
	// without leaking into the ChatSession layer. cancelCall is
	// still wired in case callCtx is ever passed to a downstream
	// call that does honor it (currently none — writeLine is sync).
	callCtx, cancelCall := context.WithCancel(ctx)
	defer cancelCall()
	_ = callCtx

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
				// Anthropic API rejects base64 images > ~5 MB.
				// Surface as a text annotation so the agent
				// knows the file exists and can try to read it
				// via its file tools (which may use the Files
				// API for larger assets).
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
			// PDFs we inline; for everything else we emit a
			// text annotation so the agent can `Read` the file.
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
			// Unknown block type — drop with a warn rather than
			// send malformed content.
			slog.Default().Warn("claudecode: skip unknown block", "type", b.Type)
		}
	}
	if len(content) == 0 {
		// All blocks were dropped (empty / unreadable). Nothing
		// to send — matches SendText's empty-input no-op.
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
	return s.writeLine(data)
}

// readFileAsBase64 reads the file at path and returns its contents
// base64-encoded. The Anthropic API accepts base64-inlined images
// up to ~5 MB and PDFs up to ~32 MB; larger files would require a
// different transport (file_id via Files API). Errors include the
// path for log readability.
func readFileAsBase64(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	return base64.StdEncoding.EncodeToString(data), nil
}

// SendPermission writes the user's answer to the most recent
// AskUserQuestion prompt as a tool_result user message. resp is the
// option label the user selected (single-select) or a comma-
// separated list (multi-select legacy); an "Other" choice is passed
// through as the typed text.
//
// Blocks until the pending question is resolved. If no question is
// pending, returns an error (channel should not call this in that
// case).
func (s *session) SendPermission(resp string) error {
	s.pendingMu.Lock()
	ask := s.pendingAsk
	s.pendingAsk = nil
	s.pendingMu.Unlock()

	if ask == nil {
		return fmt.Errorf("claudecode: no pending AskUserQuestion")
	}

	var selected []string
	if ask.Multi {
		// Split by comma; trim whitespace. Empty entries skipped.
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
	return s.writeLine(data)
}

// New resets the conversation context on the running session without
// terminating the underlying process. F-34 §3.2.1 + Phase 3 final
// verification 2026-08-04 (live claude binary, stream-json mode):
//
// Claude Code DOES honor the `/clear` slash command when sent as a
// `user`-typed message in stream-json mode. Empirically observed:
//
//   - Round 1 (memory plant): {"type":"user","message":{"role":"user",
//     "content":"Remember 77777"}} → assistant returned "REMEMBERED"
//   - Round 2 (recall):        → assistant returned "77777"
//   - Round 3 (/clear):         {"type":"user","message":{"role":"user",
//     "content":"/clear"}} → claude fired SessionStart:clear hook +
//     generated a NEW session_id + emitted a fresh init event
//   - Round 4 (recall):         → assistant returned "NONE"  ← memory cleared
//
// Other candidate paths were verified to NOT work:
//   - {"type":"control","control":{"type":"clear"|"reset"|"rewind"|"compact"
//     |"new_session"|"exit"}} are silently dropped (no new init,
//     no new session_id, no memory change).
//   - raw bytes "/clear\n" to stdin is malformed JSON and either
//     errors or is dropped by the parser.
//
// Therefore: write a properly-structured user message whose content
// is literally "/clear" via writeLine. In-process reset: process
// stays alive, transport stays open, Events() stays open, PID stays
// the same. The bridge emits a new `system/init` event with the
// new session_id which the runtime picks up via SetResumeID
// (cmd/nightme/run.go newEventHandler).
func (s *session) New(ctx context.Context) error {
	if s.closed == nil {
		return fmt.Errorf("claudecode: session not initialized")
	}
	select {
	case <-s.closed:
		return fmt.Errorf("claudecode: session closed")
	default:
	}
	// {"type":"user","message":{"role":"user","content":"/clear"}}
	payload := []byte(`{"type":"user","message":{"role":"user","content":"/clear"}}`)
	if err := s.writeLine(payload); err != nil {
		return err
	}
	// F-34 review C3 (known limitation): writeLine returns before
	// claude actually processes /clear and emits the fresh system/init.
	// The /new handler therefore replies "Reset 1/1" to the user
	// while the actual reset is still in flight (typically <100ms).
	// In practice the user's next message latency is much larger
	// than this window, so the impact is minimal. A proper fix would
	// require piping an ack channel through pumpStream so the
	// readPump signals "fresh init observed" — out of scope for
	// this PR; documented as a follow-up.
	return nil
}

// Close terminates the session: closes stdin (so the child sees EOF
// and exits cleanly), waits briefly for graceful shutdown, then
// SIGKILLs if necessary. Idempotent.
func (s *session) Close() error {
	var firstErr error
	s.closeOnce.Do(func() {
		close(s.closed)

		// Close stdin so claude sees EOF and exits.
		s.stdinMu.Lock()
		_ = s.stdin.Flush()
		s.stdinMu.Unlock()

		if s.cmd.Process != nil {
			_ = s.cmd.Process.Signal(os.Interrupt)
		}

		// Wait up to 2s for graceful exit; SIGKILL after.
		done := make(chan struct{})
		go func() {
			_ = s.cmd.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			if s.cmd.Process != nil {
				_ = s.cmd.Process.Kill()
			}
			<-done
		}

		// Wait for pumpStream to drain + close the events channel
		// itself (via defer in newSession). Pre-fix, Close closed
		// s.events directly, racing pumpStream's
		// scanner.Err → events <- EventAgentError emit. See the
		// TestClaudeCodeBridge_RealSubprocess race in PR #53's CI.
		s.pumpWG.Wait()
	})
	return firstErr
}

// writeLine writes a single JSON line to claude's stdin followed by
// \n. The mutex serializes writes (multiple SendText calls in flight).
func (s *session) writeLine(data []byte) error {
	s.stdinMu.Lock()
	defer s.stdinMu.Unlock()

	if _, err := s.stdin.Write(data); err != nil {
		return fmt.Errorf("claudecode: write stdin: %w", err)
	}
	if _, err := s.stdin.WriteString("\n"); err != nil {
		return fmt.Errorf("claudecode: write newline: %w", err)
	}
	if err := s.stdin.Flush(); err != nil {
		return fmt.Errorf("claudecode: flush stdin: %w", err)
	}
	return nil
}

// drainStderr reads stderr line-by-line until EOF. Each non-empty
// line is forwarded to lines (best-effort: a slow consumer is
// dropped, never blocked) AND logged. Lines matching the
// --resume / MCP failure patterns get elevated to Warn so they
// show up in default-level daemon logs (helps T-alive debugging
// of "claude hung after init"); the rest stay at Debug to keep
// the log readable. The lines channel is closed when EOF arrives.
// Used by the bridge to keep stderr from blocking and to surface
// stderr content to the resume fallback probe.
//
// T-alive: claude hang-on-init post-mortem (2026-08-07). Root cause
// was a nil `eventQueue` channel left uninitialized by the daemon
// restart code path — `AgentSession` got it from `NewAgentSession`
// but `FromAgentSessionEntry` missed it, so the readpump blocked on
// `send to nil channel` while the child `claude` process stayed
// alive. Restoring `--print` and wiring `KindLifecycle` into
// `SetExited` were necessary but not sufficient.
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
				// Probe consumer is slow; drop. We log
				// anyway and the dropped line still
				// surfaces via the elevated logger level.
			}
		}
	}
	return nil
}

// isLoudStderr returns true when a stderr line carries a
// resume-rejection or MCP-failure signal that operators need to
// see at default log level. Mirrors classifyStderrLineForResume
// in claudecode.go — kept duplicated rather than exported
// because the two files live in the same package's bridge
// surface (drainStderr is internal to claudecode).
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
// from this file just for two calls. Keeps the diff with the rest
// of the package small.
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

// detectBranch returns the current git branch for workspace, or
// "" if the workspace is not a git repo / git is unavailable / the
// command errors out. Uses `git -C ws symbolic-ref --short HEAD`
// which is the most direct way to read the current branch without
// invoking rev-parse heuristics. stderr is discarded — non-git
// directories simply produce "" without surfacing an error to the
// user (the foot note just omits the branch segment).
func detectBranch(workspace string) string {
	if workspace == "" {
		return ""
	}
	// Bound the command to a short window so a slow / hung
	// git invocation never blocks receipt init. 1 second is
	// generous for a local file lookup; if git can't answer
	// in that window we'd rather show no branch than block
	// receipt rendering.
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
	// `git symbolic-ref --short HEAD` on a detached HEAD prints
	// nothing to stdout (and exits non-zero); the err path
	// above catches that. As a defensive fallback, also handle
	// the literal "HEAD" output that rev-parse emits on a
	// detached checkout — we don't want it showing up in the
	// foot note.
	if branch == "" || branch == "HEAD" {
		return ""
	}
	return branch
}
