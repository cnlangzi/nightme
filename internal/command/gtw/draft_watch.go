package gtw

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/internal/pathutil"
)

// draftWatcher renders the chat's pending issue drafts into the
// chat when an agent turn ends cleanly. /gtw new arms one per chat;
// it lives until the next /gtw new replaces it, so drafts written
// in later turns — after the agent's questions are answered, or on
// a change request — are rendered too.
//
// The draft files on disk are the authoritative content: the agent
// writes them and nightme shows them, so what the user reviews is
// exactly what /gtw new -y will submit, with no dependence on the
// agent reprinting the drafts itself.
type draftWatcher struct {
	unsub    func()
	chatID   string
	draftDir string
	// ctx keeps the command's values but drops its cancellation:
	// rendering happens long after the command returned, and the
	// send must not fail because the command's context is done.
	ctx context.Context
	em  messages.Emitter

	mu sync.Mutex // serialises a render and guards lastDigest
	// lastDigest is the fingerprint of the pending set the last
	// successfully sent render carried. A failed send leaves it
	// untouched, so the next clean turn retries.
	lastDigest string
}

// armDrafts replaces chatID's draft watcher with a fresh one
// subscribed to cs.PromptEndBus. /gtw new calls it after the draft
// directory has been reset and before the archive prompt is
// dispatched: the subscription must be in place before the turn
// that writes the drafts can end, and a run that fails before that
// point must not leave a watcher rendering the previous run's
// leftovers.
//
// The replacement is atomic under watcherMu: unsubscribe-old and
// store-new are one critical section, so two concurrent /gtw new
// runs can never leave two active watchers on the same chat.
func (m *Manager) armDrafts(ctx context.Context, cs *chatsession.ChatSession, chatID, draftDir string) {
	m.watcherMu.Lock()
	defer m.watcherMu.Unlock()
	if old, ok := m.watchers.LoadAndDelete(chatID); ok {
		old.(*draftWatcher).unsubscribe()
	}
	m.watchers.Store(chatID, newDraftWatcher(ctx, cs, chatID, draftDir))
}

// newDraftWatcher subscribes to the chat's prompt-end bus. The
// handler only forks the render onto its own goroutine — the bus
// publishes on the chat's event pump, which a directory read or a
// slow channel must not block — and always returns false: returning
// true would consume the event and starve later subscribers, so the
// message's own hold-until-prompt-end would never release.
//
// Subscribe is nil-receiver-safe: a chat without a bus gets a no-op
// unsubscribe and the watcher simply never fires.
func newDraftWatcher(ctx context.Context, cs *chatsession.ChatSession, chatID, draftDir string) *draftWatcher {
	w := &draftWatcher{
		chatID:   chatID,
		draftDir: draftDir,
		ctx:      context.WithoutCancel(ctx),
		em:       cs.Emitter(),
	}
	w.unsub = cs.PromptEndBus.Subscribe(func(e chatsession.PromptEndedEvent) bool {
		if e.Reason != agent.PromptEndClean {
			return false
		}
		go w.render(e.UserMsgID)
		return false
	})
	return w
}

// unsubscribe drops the bus subscription; the next /gtw new calls
// it when it replaces this watcher.
func (w *draftWatcher) unsubscribe() {
	w.unsub()
}

// render sends the pending drafts as one reply anchored at the
// ended turn's message. A render goes out only when the pending set
// is non-empty and its fingerprint changed since the last
// successful send; a failed send leaves the fingerprint untouched,
// so the next cleanly ended turn retries.
func (w *draftWatcher) render(anchorMsgID string) {
	if w.em == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	drafts, err := loadPendingDrafts(w.draftDir)
	if err != nil {
		slog.Default().Warn("gtw: could not read the issue drafts",
			"dir", w.draftDir, "err", err)
		return
	}
	if len(drafts) == 0 {
		return
	}
	digest := draftDigest(drafts)
	if digest == w.lastDigest {
		return
	}
	out := messages.OutboundMessage{
		ChatID:  w.chatID,
		Kind:    messages.OutReply,
		ReplyTo: anchorMsgID,
		Text:    renderPendingDrafts(drafts),
	}
	if _, err := w.em.Send(w.ctx, out); err != nil {
		slog.Default().Warn("gtw: could not send the rendered issue drafts",
			"dir", w.draftDir, "err", err)
		return
	}
	w.lastDigest = digest
}

// loadPendingDrafts reads the draft directory for rendering:
// regular files named NN.md, in file order. Anything else —
// NN.created records, stray files, subdirectories — is ignored:
// rendering is a view, so an unexpected entry must not silence it
// (the /gtw new -y creation path stays strict and still rejects
// it). A missing directory is an empty set.
func loadPendingDrafts(dir string) ([]issueDraft, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var drafts []issueDraft
	for _, e := range entries {
		m := issueDraftFileRe.FindStringSubmatch(e.Name())
		if m == nil || m[2] != issueDraftExt || !e.Type().IsRegular() {
			continue
		}
		path := pathutil.Join(dir, e.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		drafts = append(drafts, issueDraft{path: path, key: m[1], raw: string(raw)})
	}
	return drafts, nil
}

// draftDigest fingerprints the pending set: name and content of
// every draft, so any write, rename, or deletion changes it.
// NN.created records are not part of it — they carry no content the
// render would show.
func draftDigest(drafts []issueDraft) string {
	h := sha256.New()
	for _, d := range drafts {
		h.Write([]byte(d.name()))
		h.Write([]byte{0})
		h.Write([]byte(d.raw))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// draftRenderRule introduces a block of the render: a horizontal
// rule on a line of its own, separated from the text of the block
// above by a blank line — a rule directly under a text line is a
// setext heading in markdown, not a rule.
const draftRenderRule = "---"

// renderPendingDrafts builds the render body: a sequence of blocks,
// each introduced by a horizontal rule. A draft block is the file
// name and then the file content verbatim; a draft /gtw new -y
// would refuse opens with a ⚠️ line carrying the parse error —
// flagging one draft must not hide the others. The last block is the
// closing hint, so the operator reads what to do next after the
// drafts.
//
// The files on disk are untouched: the render is a view of what the
// agent wrote and of what /gtw new -y will submit.
func renderPendingDrafts(drafts []issueDraft) string {
	var sb strings.Builder
	for _, d := range drafts {
		fmt.Fprintln(&sb, draftRenderRule)
		if _, _, err := parseIssueDraft(d.raw); err != nil {
			fmt.Fprintf(&sb, "⚠️ %s: %v\n", d.name(), err)
		}
		fmt.Fprintln(&sb, d.name())
		sb.WriteString(d.raw)
		if !strings.HasSuffix(d.raw, "\n") {
			sb.WriteByte('\n')
		}
		sb.WriteByte('\n')
	}
	fmt.Fprintln(&sb, draftRenderRule)
	fmt.Fprintln(&sb, newIssueHint)
	return sb.String()
}
