package wiki

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
	"time"
)

// Job — Wiki.md §3.3.
//
// One Job per (repo root). Acquired on first /wiki invocation
// against that repo, released on Finalize. A concurrent /wiki
// for the same repo (different chat, same CWD) returns
// ErrJobRunning so the user gets the explicit "wiki job
// already running" message instead of corrupting wiki.yml.
//
// Job lifecycle:
//
//	Acquired ──► Running ──► Finalize (Done | Failed)
//
// The Job ctx is derived from ChatSession.Context() so the
// orchestrator outlives the slash-command Handle call (Wiki.md
// §3.3 second paragraph). When ChatSession shuts down, the
// derived ctx is cancelled and the goroutine unwinds via
// PromptEndBus / runtime events.
//
// Daemon termination clears the in-memory registry; persistent
// pending state in wiki.yml remains the recovery source for
// the next invocation.

const (
	jobStatusRunning = "running"
	jobStatusDone    = "done"
	jobStatusFailed  = "failed"
)

// ErrJobRunning is returned by Acquire when another Job for the
// same repo root is still in flight.
var ErrJobRunning = errors.New("wiki job already running")

// IsErrJobRunning reports whether err is ErrJobRunning. Slash
// command handlers use this to convert the sentinel into the
// canonical "wiki job already running" reply.
func IsErrJobRunning(err error) bool {
	return errors.Is(err, ErrJobRunning)
}

// Job is the per-repo-run orchestrator.
type Job struct {
	ID       string
	RepoRoot string
	ChatID   string

	// Ctx is the orchestrator's lifetime context, derived
	// from cs.Context() at Acquire time. Cancelled by
	// Release or by cs.Context() cancellation upstream.
	Ctx    context.Context
	cancel context.CancelFunc

	mu     sync.Mutex
	status string
	done   chan struct{}

	// Plan / apply state — written by the orchestrator, read
	// by Finalize.
	yml      *wikiYml
	headSHA  string
	batchIdx int
}

// Jobs is the package-level registry. Mutated only through
// Acquire / Release. Reads may be lock-free via the returned
// Job pointer (the Job itself is mutex-guarded for field
// updates).
var (
	jobsMu sync.Mutex
	jobs   = map[string]*Job{}
)

// Acquire reserves the Job for repoRoot. When one already
// exists in running state, ErrJobRunning is returned and the
// existing Job pointer is also returned so the caller can
// surface "queued behind existing job" semantics. A non-running
// (done / failed) entry is reaped first.
//
// chatID is recorded but not enforced — concurrent Jobs across
// chats ARE the case this registry exists to prevent. The
// returned Job tracks its origin chat for diagnostics.
func Acquire(repoRoot string, parent context.Context, chatID string) (*Job, error) {
	jobsMu.Lock()
	defer jobsMu.Unlock()

	if existing, ok := jobs[repoRoot]; ok {
		existing.mu.Lock()
		st := existing.status
		existing.mu.Unlock()
		if st == jobStatusRunning {
			return existing, ErrJobRunning
		}
		delete(jobs, repoRoot)
	}

	ctx, cancel := context.WithCancel(parent)
	j := &Job{
		ID:       newJobID(repoRoot, chatID),
		RepoRoot: repoRoot,
		ChatID:   chatID,
		Ctx:      ctx,
		cancel:   cancel,
		status:   jobStatusRunning,
		done:     make(chan struct{}),
	}
	jobs[repoRoot] = j
	return j, nil
}

// Release removes the Job from the registry and closes the
// Done channel. Idempotent; safe to call from Finalize and
// from the runtime shutdown path.
func (j *Job) Release() {
	j.mu.Lock()
	if j.status == jobStatusRunning {
		j.status = jobStatusDone
	}
	close(j.done)
	j.cancel()
	j.mu.Unlock()

	jobsMu.Lock()
	if cur, ok := jobs[j.RepoRoot]; ok && cur == j {
		delete(jobs, j.RepoRoot)
	}
	jobsMu.Unlock()
}

// MarkFailed transitions the Job to failed without releasing
// the registry entry. The next /wiki invocation against the
// same repo will reap the failed Job.
func (j *Job) MarkFailed() {
	j.mu.Lock()
	j.status = jobStatusFailed
	j.mu.Unlock()
}

// Done returns the channel closed at Release time. The
// orchestrator goroutine blocks on this when it has no further
// batches to submit.
func (j *Job) Done() <-chan struct{} { return j.done }

// Status returns the current lifecycle status.
func (j *Job) Status() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.status
}

// RepoJobs returns a snapshot of active Jobs — used by the
// runtime shutdown path to cancel everything at once.
func RepoJobs() []*Job {
	jobsMu.Lock()
	defer jobsMu.Unlock()
	out := make([]*Job, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, j)
	}
	return out
}

// Shutdown cancels every in-flight Job's context. Called by
// the daemon's shutdown path so orchestrator goroutines
// unblock on the (already-completed) PromptEndBus wait — the
// underlying Job ctx is derived from the ChatSession's ctx
// (which the runtime cancels separately), but this hook
// covers the case where the Job's parent ctx outlives any
// single ChatSession's lifecycle (multi-chat repos).
//
// Each Job is transitioned to done and removed from the
// registry. Idempotent — safe to call from graceful shutdown
// even if individual ChatSessions have already cancelled
// their derived ctxs.
func Shutdown() {
	jobsMu.Lock()
	pending := make([]*Job, 0, len(jobs))
	for _, j := range jobs {
		pending = append(pending, j)
	}
	jobs = map[string]*Job{}
	jobsMu.Unlock()

	for _, j := range pending {
		j.mu.Lock()
		if j.status == jobStatusRunning {
			j.status = jobStatusDone
		}
		close(j.done)
		j.cancel()
		j.mu.Unlock()
	}
}

func newJobID(repoRoot, chatID string) string {
	h := sha256.New()
	h.Write([]byte(repoRoot))
	h.Write([]byte{0})
	h.Write([]byte(chatID))
	h.Write([]byte{0})
	h.Write([]byte(time.Now().Format(time.RFC3339Nano)))
	sum := h.Sum(nil)
	return "wiki-" + hex.EncodeToString(sum[:6])
}
