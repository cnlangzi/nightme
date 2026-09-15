package stt

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ProcessManager owns the lifecycle of a single nightme-stt
// child process. It is the seam between "we need a transcription"
// (Telegram Voice handler) and "we have a working Transcriber".
//
// The Manager is designed around the constraints from issue
// #381 §3:
//
//   - STT starts lazily — first Voice message triggers a
//     spawn, not NightMe startup.
//   - One worker at a time — concurrent Voice messages
//     serialise through the manager's mu.
//   - Crash recovery — a worker crash closes the client,
//     resets state, and lets the next Voice request attempt
//     one clean EnsureReady.
type ProcessManager interface {
	// EnsureReady returns a Transcriber ready to call, or
	// an error. If the worker is not running it spawns it,
	// waits for readiness, and returns a fresh client.
	// Subsequent calls within the worker's lifetime reuse
	// the same client.
	EnsureReady(ctx context.Context) (Transcriber, error)
	// Stop shuts the worker down and releases resources.
	// Idempotent.
	Stop(ctx context.Context) error
	// Status reports the current state for `nightme doctor`.
	Status(ctx context.Context) Status
}

// Status is the snapshot returned by Status. Field names are
// stable for JSON / CLI consumers.
type Status struct {
	Running    bool
	PID        int
	Endpoint   string
	Version    int
	LastError  string
	BinaryPath string
	NotBuilt   bool
}

// Handle is what Spawner hands back to the Manager. Pid is
// reported in Status; Stop terminates the worker (gracefully
// if the platform supports it, forcefully after shutdownGrace).
type Handle interface {
	PID() int
	Stop(ctx context.Context) error
}

// Spawner is the function the Manager uses to start the
// worker process. The production spawner exec.Command's
// nightme-stt; the test spawner wires the loopback transport
// with an in-process server. Spawner returns once the worker
// is reachable on the local IPC endpoint; a long timeout in
// Spawner is the spawner's job.
type Spawner func(ctx context.Context, endpoint Endpoint) (Handle, error)

// SpawnResult is the type returned by the spawner. It mirrors
// `os.Process` for production spawners and an in-process
// placeholder for tests.
type SpawnResult struct {
	PID    int
	Handle Handle
}

// Manager is the production ProcessManager. Constructed once
// at startup; reused for every Voice message.
type Manager struct {
	transport     Transport
	spawner       Spawner
	endpoint      Endpoint
	readyTimeout  time.Duration
	shutdownGrace time.Duration

	mu      sync.Mutex
	client  *RPCClient
	running bool
	lastErr string
}

// NewManager builds a Manager. transport + spawner + endpoint
// are required; the timeout knobs default to sensible values
// that the tests can rely on.
func NewManager(transport Transport, spawner Spawner, endpoint Endpoint) (*Manager, error) {
	if endpoint == "" {
		return nil, errors.New("stt manager: empty endpoint")
	}
	if transport == nil {
		return nil, errors.New("stt manager: nil transport")
	}
	if spawner == nil {
		return nil, errors.New("stt manager: nil spawner")
	}
	return &Manager{
		transport:     transport,
		spawner:       spawner,
		endpoint:      endpoint,
		readyTimeout:  10 * time.Second,
		shutdownGrace: 3 * time.Second,
	}, nil
}

// SetReadyTimeout overrides the default 10s spawn-wait. Mostly
// useful for tests.
func (m *Manager) SetReadyTimeout(d time.Duration) {
	m.readyTimeout = d
}

// EnsureReady implements ProcessManager.
func (m *Manager) EnsureReady(ctx context.Context) (Transcriber, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.client != nil {
		// Fast path: existing client. Verify it's still
		// healthy; a previous failed call may have closed
		// the underlying conn without our noticing.
		if err := m.client.Health(ctx); err == nil {
			return m.client, nil
		}
		_ = m.client.Close()
		m.client = nil
		m.running = false
	}

	// Slow path: spawn + dial + wait for readiness.
	readyCtx, cancel := context.WithTimeout(ctx, m.readyTimeout)
	defer cancel()

	proc, err := m.spawner(readyCtx, m.endpoint)
	if err != nil {
		m.lastErr = err.Error()
		return nil, fmt.Errorf("stt: spawn worker: %w", err)
	}
	conn, err := m.transport.Dial(readyCtx, m.endpoint)
	if err != nil {
		_ = proc.Stop(context.Background())
		m.lastErr = err.Error()
		return nil, fmt.Errorf("stt: dial worker: %w", err)
	}
	client := NewRPCClient(conn)
	if err := client.Health(readyCtx); err != nil {
		_ = client.Close()
		_ = proc.Stop(context.Background())
		m.lastErr = err.Error()
		return nil, fmt.Errorf("stt: worker not ready: %w", err)
	}
	m.client = client
	m.running = true
	m.lastErr = ""
	return client, nil
}

// Stop implements ProcessManager.
func (m *Manager) Stop(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.client != nil {
		// Best-effort graceful shutdown. We don't care
		// about the error here — the worker may already
		// be gone.
		_ = m.callShutdown(ctx)
		_ = m.client.Close()
		m.client = nil
	}
	m.running = false
	return nil
}

func (m *Manager) callShutdown(ctx context.Context) error {
	if m.client == nil {
		return nil
	}
	_, err := m.client.call(ctx, &Request{Version: ProtocolVersion, Op: OpShutdown})
	return err
}

// Status implements ProcessManager.
func (m *Manager) Status(ctx context.Context) Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := Status{
		Running:  m.running,
		Endpoint: string(m.endpoint),
	}
	if m.lastErr != "" {
		s.LastError = m.lastErr
	}
	if m.client != nil {
		v, err := m.client.Version(ctx)
		if err == nil {
			s.Version = v
		}
	}
	return s
}
