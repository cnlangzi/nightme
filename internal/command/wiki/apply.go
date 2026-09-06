package wiki

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/agentsession"
	"github.com/cnlangzi/nightme/internal/chatsession"
)

// Apply orchestration — Wiki.md §11.
//
// One /wiki invocation submits at most one Agent Prompt and
// one bounded batch. A batch contains at most 8 targets OR 64
// directly owned source files across those targets, whichever
// is reached first. A single module is never split across
// batches (§11.2).
//
// Processing order:
//
//  1. retryable in_progress / failed live modules
//  2. other live modules, deepest source path first
//  3. parent modules
//  4. removed modules, processed mechanically (no Agent)
//  5. architecture and quickstart after no live module entry
//     remains pending
//
// Delete actions never consume Agent batch capacity — they're
// applied mechanically and recorded as filesystem failures
// (§11.2 second paragraph).

const (
	maxBatchPages        = 8
	maxBatchSourceFiles  = 64
	defaultPromptTimeout = 30 * time.Minute
)

// Target is one page the Wiki Prompt is asking the Agent to
// produce (or asking /wiki itself to delete).
type Target struct {
	// Path is the source module path (e.g. "internal/bridge")
	// OR the aggregate name ("architecture" | "quickstart").
	Path string

	// RelFile is the wiki-relative file path (e.g.
	// "modules/internal/bridge/index.md" or "architecture.md").
	RelFile string

	// Action is "new" | "regenerate" | "delete".
	Action string

	// FilesChanged is populated for module regenerations so
	// the Agent sees which files changed since last_sha.
	FilesChanged []string

	// Purpose is the prior Purpose string — surfaced in the
	// prompt as "Purpose (prior)".
	Purpose string

	// Reason is the Plan-supplied reason for the action.
	Reason string

	// BeforeHash is the hash captured at SubmitBatch time;
	// Finalize uses it to decide whether a target's content
	// actually changed during the batch.
	BeforeHash *string
}

// KindLabel returns a human-readable label for prompts / logs.
func (t Target) KindLabel() string {
	if t.Path == ArchitectureKey || t.Path == QuickstartKey {
		return t.Path
	}
	return "module"
}

func (t Target) kindLabel() pageKindLabel {
	switch t.Path {
	case ArchitectureKey:
		return kindArchitecture
	case QuickstartKey:
		return kindQuickstart
	}
	return kindModule
}

// IsModule reports whether the target is a per-module page.
func (t Target) IsModule() bool {
	return t.kindLabel() == kindModule
}

// IsAggregate reports whether the target is an aggregate page.
func (t Target) IsAggregate() bool {
	return t.kindLabel() != kindModule
}

// Batch is a single prompt's payload. Targets is rendered in
// the order Apply chose; Finalize uses the same order to walk
// back through pending[] when stamping results.
type Batch struct {
	Targets []Target
}

// TotalSourceFiles sums FilesChanged across module targets.
// Aggregate targets contribute 0 — they don't directly own
// source files.
func (b Batch) TotalSourceFiles() int {
	n := 0
	for _, t := range b.Targets {
		n += len(t.FilesChanged)
	}
	return n
}

// Coordinator owns the per-repo Apply loop. One Coordinator
// lives per Job; it is created by Handle, runs in a goroutine
// after the slash command returns its ack, and finishes by
// releasing the Job.
type Coordinator struct {
	Job   *Job
	CS    *chatsession.ChatSession
	Input chatsession.Message
	Yml   *wikiYml
	Head  string
	Git   GitRunner

	mu      sync.Mutex
	textBuf strings.Builder
}

// Start runs the Apply loop to completion. Synchronous from
// the caller's perspective (the orchestrator goroutine blocks
// on PromptEndBus); returns the final reply text the caller
// should send to the user.
func (c *Coordinator) Start() string {
	// Subscribe to AgentEventBus BEFORE SubmitBatch so the
	// first text event for the Prompt is observed.
	if c.CS.AgentEventBus != nil {
		unsub := c.CS.AgentEventBus.Subscribe(func(env chatsession.AgentEventEnvelope) bool {
			if env.UserMsgID != c.Input.ID {
				return false
			}
			if env.Event == nil {
				return false
			}
			c.appendAgentText(env.Event)
			return false
		})
		_ = unsub // subscriber lives until Job release; bus.Close handles it
	}

	// Initial batch — only when there's actually work.
	if work := c.pendingWork(); len(work) > 0 {
		if err := c.runBatch(work[0]); err != nil {
			c.Job.MarkFailed()
			c.Job.Release()
			return c.formatFinalReply(err)
		}
		for _, batch := range work[1:] {
			if err := c.runBatch(batch); err != nil {
				c.Job.MarkFailed()
				c.Job.Release()
				return c.formatFinalReply(err)
			}
		}
	}

	// After all live module batches, sweep remaining deletes
	// mechanically then run aggregate batches.
	c.applyDeletes()

	// Aggregate batches (architecture + quickstart, dirty only).
	for c.hasAggregateWork() {
		batch := c.nextAggregateBatch()
		if len(batch.Targets) == 0 {
			break
		}
		if err := c.runBatch(batch); err != nil {
			c.Job.MarkFailed()
			c.Job.Release()
			return c.formatFinalReply(err)
		}
	}

	c.finalize()
	c.Job.Release()
	return c.formatFinalReply(nil)
}

func (c *Coordinator) appendAgentText(ev *agent.AgentEvent) {
	if ev == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	switch ev.Kind {
	case agent.EventAgentText:
		c.textBuf.WriteString(ev.Text)
		c.textBuf.WriteString("\n")
	case agent.EventAgentResult:
		if ev.Result != nil && ev.Result.Text != "" {
			c.textBuf.WriteString(ev.Result.Text)
			c.textBuf.WriteString("\n")
		}
	}
}

func (c *Coordinator) textReply() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.textBuf.String()
}

// --- batching ---

// pendingWork returns the live-module batches (retryable
// first, then deepest-first) in target order. Excludes
// aggregate targets.
func (c *Coordinator) pendingWork() []Batch {
	var retry, deep, parent []pendingEntry

	for _, p := range c.Yml.Pending {
		if p.Path == ArchitectureKey || p.Path == QuickstartKey {
			continue
		}
		if p.Action == pendingActionDelete {
			continue
		}
		switch p.Status {
		case pendingStatusInProgress, pendingStatusFailed:
			retry = append(retry, p)
		default:
			if pathDepth(p.Path) >= 3 {
				deep = append(deep, p)
			} else {
				parent = append(parent, p)
			}
		}
	}

	sort.SliceStable(deep, func(i, j int) bool {
		di, dj := pathDepth(deep[i].Path), pathDepth(deep[j].Path)
		if di != dj {
			return di > dj
		}
		return deep[i].Path < deep[j].Path
	})
	sort.SliceStable(parent, func(i, j int) bool {
		return parent[i].Path < parent[j].Path
	})
	sort.SliceStable(retry, func(i, j int) bool {
		if retry[i].Status != retry[j].Status {
			return retry[i].Status == pendingStatusInProgress
		}
		return retry[i].Path < retry[j].Path
	})

	ordered := append(retry, deep...)
	ordered = append(ordered, parent...)
	return c.packBatches(ordered)
}

// packBatches groups entries into batches respecting both the
// page limit (maxBatchPages) and the source-file limit
// (maxBatchSourceFiles). One entry is never split across
// batches.
func (c *Coordinator) packBatches(entries []pendingEntry) []Batch {
	var batches []Batch
	var current Batch
	for _, e := range entries {
		t := c.entryToTarget(e)
		files := len(t.FilesChanged)

		if len(current.Targets) >= maxBatchPages || current.TotalSourceFiles()+files > maxBatchSourceFiles {
			if len(current.Targets) > 0 {
				batches = append(batches, current)
			}
			current = Batch{}
		}
		current.Targets = append(current.Targets, t)
	}
	if len(current.Targets) > 0 {
		batches = append(batches, current)
	}
	return batches
}

func (c *Coordinator) entryToTarget(e pendingEntry) Target {
	rel := moduleFileForPath(c.Yml, e.Path)
	t := Target{
		Path:         e.Path,
		RelFile:      rel,
		Action:       e.Action,
		FilesChanged: e.FilesChanged,
		Reason:       e.Reason,
	}
	if h := hashFile(filepath.Join(c.Job.RepoRoot, "wiki", rel)); h != "" {
		t.BeforeHash = &h
	}
	for _, m := range c.Yml.Modules {
		if m.Path == e.Path {
			t.Purpose = m.Purpose
			break
		}
	}
	return t
}

// moduleFileForPath returns the wiki-root-relative path of the
// page for the given source module path (or aggregate name).
// Looks up the moduleYml / aggregateYml record; falls back to
// "modules/<path>/index.md" when no record exists (this can
// happen for newly-discovered modules before mergeModules runs).
func moduleFileForPath(yml *wikiYml, path string) string {
	for _, m := range yml.Modules {
		if m.Path == path {
			return m.File
		}
	}
	if a, ok := yml.Aggregates[path]; ok && a != nil && a.File != "" {
		return a.File
	}
	return "modules/" + path + "/index.md"
}

// pathDepth counts the number of slash-separated segments in p.
func pathDepth(p string) int {
	if p == "" {
		return 0
	}
	return strings.Count(filepath.ToSlash(p), "/") + 1
}

// --- batch submission ---

// runBatch marks entries in_progress, captures before_hash,
// builds the prompt, submits to ChatSession.QueueUserMessage,
// waits on PromptEndBus, then applies Finalize logic.
func (c *Coordinator) runBatch(batch Batch) error {
	// Mark in_progress and capture before_hash (one per target).
	for i := range batch.Targets {
		c.markInProgress(batch.Targets[i])
	}

	// Snapshot git status BEFORE the prompt so we can
	// detect out-of-allowlist writes (§11.5).
	beforeStatus, _ := c.Git.Status(c.Job.RepoRoot)

	// Build the prompt and the Message.
	prompt := BuildBatchPrompt(c.Job.RepoRoot, c.Yml.PlanSha, c.Job.ID, batch)
	msg := chatsession.Message{
		ID:     c.Input.ID,
		ChatID: c.Input.ChatID,
		Blocks: []agent.ContentBlock{{Type: agent.ContentText, Text: prompt}},
		Kind:   chatsession.MessageKindQueue,
	}
	if err := c.CS.QueueUserMessage(msg); err != nil {
		// §11.3: queue failure → entries back to pending,
		// Job key released.
		for _, t := range batch.Targets {
			c.rollbackToPending(t)
		}
		c.persist()
		return fmt.Errorf("queue wiki prompt: %w", err)
	}

	// Wait for PromptEnd.
	if err := c.waitForPromptEnd(); err != nil {
		for _, t := range batch.Targets {
			c.rollbackToPending(t)
		}
		c.persist()
		return err
	}

	// Parse the framed result.
	reply := c.textReply()
	result, ok := validateFraming(reply, c.Job.ID)
	if !ok {
		// §11.4 fallback: accept files whose content
		// changed during the batch AND pass validation.
		c.applyContentFallback(&batch)
	} else {
		c.applyFramedResult(&batch, result)
	}

	// Output-boundary check.
	afterStatus, _ := c.Git.Status(c.Job.RepoRoot)
	allowlist := batchAllowlist(&batch)
	rep := CheckOutputBoundary(beforeStatus, afterStatus, allowlist)
	if len(rep.Violations) > 0 {
		// §11.5: refuse to finalize; leave pending for retry.
		for _, t := range batch.Targets {
			c.markFailed(t, fmt.Sprintf("output boundary violated: %v", rep.Violations))
		}
		c.persist()
		return fmt.Errorf("output boundary violated: %v", rep.Violations)
	}

	c.persist()
	return nil
}

// markInProgress writes Status=in_progress and the before_hash
// to the matching pending entry. Idempotent across retries.
func (c *Coordinator) markInProgress(t Target) {
	for i := range c.Yml.Pending {
		if c.Yml.Pending[i].Path != t.Path {
			continue
		}
		c.Yml.Pending[i].Status = pendingStatusInProgress
		c.Yml.Pending[i].BeforeHash = t.BeforeHash
		return
	}
}

// rollbackToPending resets Status / Error / BeforeHash on the
// matching entry after a queue failure.
func (c *Coordinator) rollbackToPending(t Target) {
	for i := range c.Yml.Pending {
		if c.Yml.Pending[i].Path != t.Path {
			continue
		}
		c.Yml.Pending[i].Status = pendingStatusPending
		c.Yml.Pending[i].Error = ""
		c.Yml.Pending[i].BeforeHash = nil
		return
	}
}

// markFailed records a permanent failure for the target.
func (c *Coordinator) markFailed(t Target, reason string) {
	for i := range c.Yml.Pending {
		if c.Yml.Pending[i].Path != t.Path {
			continue
		}
		c.Yml.Pending[i].Status = pendingStatusFailed
		c.Yml.Pending[i].Error = reason
		return
	}
}

// waitForPromptEnd blocks until PromptEndBus delivers an
// event for c.Input.ID or ctx is cancelled.
func (c *Coordinator) waitForPromptEnd() error {
	if c.CS.PromptEndBus == nil {
		return errors.New("PromptEndBus unavailable")
	}
	evCh := make(chan chatsession.PromptEndedEvent, 1)
	unsub := c.CS.PromptEndBus.Subscribe(func(e chatsession.PromptEndedEvent) bool {
		if e.UserMsgID != c.Input.ID {
			return false
		}
		select {
		case evCh <- e:
		default:
		}
		return true
	})
	defer unsub()

	ctx := c.Job.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case ev := <-evCh:
		if ev.Reason != agentsession.PromptEndClean {
			return fmt.Errorf("prompt ended with reason %v", ev.Reason)
		}
		return nil
	case <-time.After(defaultPromptTimeout):
		return errors.New("wiki prompt timeout")
	}
}

// applyFramedResult walks the framed JSON and stamps the
// matching entries. completed paths are validated; failed
// paths stay failed with the reported reason.
//
// Per Wiki.md §11.3 the Agent reports paths with the "wiki/"
// prefix (matching the repo-relative target allowlist). The
// lookup key is built by joining "wiki/" with the wiki-relative
// target RelFile so the framing-set comparison is consistent
// with the boundary check (which uses the same prefix).
func (c *Coordinator) applyFramedResult(batch *Batch, r *FramedResult) {
	completed := map[string]bool{}
	for _, p := range r.Completed {
		completed[normalizeFramedPath(p)] = true
	}
	failed := map[string]string{}
	for _, f := range r.Failed {
		failed[normalizeFramedPath(f.Path)] = f.Error
	}

	for _, t := range batch.Targets {
		key := "wiki/" + filepath.ToSlash(t.RelFile)
		if msg, ok := failed[key]; ok {
			c.markFailed(t, msg)
			continue
		}
		if completed[key] {
			if err := c.validateAndStamp(t); err != nil {
				c.markFailed(t, fmt.Sprintf("validation: %v", err))
				continue
			}
			c.markDone(t)
			continue
		}
		// Not reported by the Agent — treat as failed with a
		// reason that explains the silent omission.
		c.markFailed(t, "agent did not report completion")
	}
}

// normalizeFramedPath accepts both "wiki/..." and "<path>/..."
// forms in the Agent's framed JSON output. The contract is
// "wiki/..." (per §11.3 example) but we tolerate the bare form
// for test convenience and for agents that drop the prefix.
//
// Returns the canonical form with "wiki/" prefix.
func normalizeFramedPath(p string) string {
	p = filepath.ToSlash(p)
	if rest, ok := strings.CutPrefix(p, "wiki/"); ok {
		_ = rest
		return p
	}
	return "wiki/" + p
}

// applyContentFallback handles the §11.4 case where the
// Agent's reply is missing / malformed but at least one file
// was changed during the batch AND passes validation.
func (c *Coordinator) applyContentFallback(batch *Batch) {
	for _, t := range batch.Targets {
		if t.BeforeHash == nil {
			c.markFailed(t, "framed result missing and no before_hash to compare")
			continue
		}
		abs := filepath.Join(c.Job.RepoRoot, "wiki", t.RelFile)
		now := hashFile(abs)
		if now == "" || now == *t.BeforeHash {
			c.markFailed(t, "framed result missing and file unchanged")
			continue
		}
		content, err := os.ReadFile(abs)
		if err != nil {
			c.markFailed(t, fmt.Sprintf("read changed file: %v", err))
			continue
		}
		if err := validatePage(t.pageKind(), t.RelFile, content); err != nil {
			c.markFailed(t, fmt.Sprintf("validation: %v", err))
			continue
		}
		c.markDone(t)
	}
}

func (t Target) pageKind() pageKind {
	if t.Path == ArchitectureKey {
		return pageArchitecture
	}
	if t.Path == QuickstartKey {
		return pageQuickstart
	}
	return pageModule
}

// validateAndStamp reads the file written by the Agent and
// runs validatePage; on success it stamps module/aggregate
// last_sha + purpose / LastSHA.
func (c *Coordinator) validateAndStamp(t Target) error {
	abs := filepath.Join(c.Job.RepoRoot, "wiki", t.RelFile)
	content, err := os.ReadFile(abs)
	if err != nil {
		return err
	}
	if err := validatePage(t.pageKind(), t.RelFile, content); err != nil {
		return err
	}
	if t.IsAggregate() {
		c.stampAggregate(t, content)
	} else {
		c.stampModule(t, content)
	}
	return nil
}

// stampModule updates moduleYml.LastSHA / PromptVer / Purpose
// for the matching path.
func (c *Coordinator) stampModule(t Target, content []byte) {
	head := c.Head
	purpose := extractPurpose(content)
	for i := range c.Yml.Modules {
		if c.Yml.Modules[i].Path == t.Path {
			c.Yml.Modules[i].LastSHA = &head
			c.Yml.Modules[i].PromptVer = ModulePromptVersion
			if purpose != "" {
				c.Yml.Modules[i].Purpose = purpose
			}
			return
		}
	}
	c.Yml.Modules = append(c.Yml.Modules, moduleYml{
		Path:      t.Path,
		File:      t.RelFile,
		LastSHA:   &head,
		PromptVer: ModulePromptVersion,
		Purpose:   purpose,
	})
}

// stampAggregate updates the matching aggregate entry.
func (c *Coordinator) stampAggregate(t Target, _ []byte) {
	head := c.Head
	a := ensureAggregate(c.Yml, t.Path)
	a.LastSHA = &head
	a.PromptVer = currentPromptVerFor(t.Path)
	a.Dirty = false
	a.Status = pendingStatusDone
	a.Error = ""
	a.BeforeHash = nil
}

// markDone clears Error, sets Status=done. before_hash is
// cleared too — once stamped, the "during-batch" record has
// done its job.
func (c *Coordinator) markDone(t Target) {
	for i := range c.Yml.Pending {
		if c.Yml.Pending[i].Path != t.Path {
			continue
		}
		c.Yml.Pending[i].Status = pendingStatusDone
		c.Yml.Pending[i].Error = ""
		c.Yml.Pending[i].BeforeHash = nil
		return
	}
}

// extractPurpose pulls the first non-empty paragraph after the
// `> ` blockquote at the top of a module page. Returns "" when
// the page is malformed.
func extractPurpose(content []byte) string {
	for line := range strings.SplitSeq(string(content), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "# ") {
			continue
		}
		if rest, ok := strings.CutPrefix(trimmed, "> "); ok {
			return rest
		}
		if trimmed == "" {
			continue
		}
		return trimmed
	}
	return ""
}

// --- delete handling (§11.2 "mechanical") ---

func (c *Coordinator) applyDeletes() {
	for i := range c.Yml.Pending {
		if c.Yml.Pending[i].Action != pendingActionDelete {
			continue
		}
		t := c.entryToTarget(c.Yml.Pending[i])
		abs := filepath.Join(c.Job.RepoRoot, "wiki", t.RelFile)
		if err := os.Remove(abs); err != nil && !errors.Is(err, os.ErrNotExist) {
			c.markFailed(t, fmt.Sprintf("delete: %v", err))
			continue
		}
		c.markDone(t)
	}
	c.persist()
}

// --- aggregate batches ---

// hasAggregateWork reports whether any aggregate still has a
// pending entry or is marked dirty.
func (c *Coordinator) hasAggregateWork() bool {
	for _, p := range c.Yml.Pending {
		if p.Path == ArchitectureKey || p.Path == QuickstartKey {
			if p.Status != pendingStatusDone {
				return true
			}
		}
	}
	if a, ok := c.Yml.Aggregates[ArchitectureKey]; ok && a != nil && a.Dirty {
		return true
	}
	if a, ok := c.Yml.Aggregates[QuickstartKey]; ok && a != nil && a.Dirty {
		return true
	}
	return false
}

// nextAggregateBatch produces a Batch covering any dirty
// aggregate whose Pending entry isn't done. At most one
// batch — aggregates don't compete for Agent capacity against
// module pages because they only run after every live module
// is processed (§11.2 step 5).
func (c *Coordinator) nextAggregateBatch() Batch {
	var targets []Target
	for _, name := range []string{ArchitectureKey, QuickstartKey} {
		a, ok := c.Yml.Aggregates[name]
		if !ok || a == nil {
			continue
		}
		// Skip if a pending entry exists and is already done.
		if p, ok := findPending(c.Yml.Pending, name); ok && p.Status == pendingStatusDone {
			continue
		}
		if !a.Dirty {
			continue
		}
		t := Target{
			Path:    name,
			RelFile: a.File,
			Action:  pendingActionRegenerate,
			Reason:  "aggregate is dirty",
		}
		if h := hashFile(filepath.Join(c.Job.RepoRoot, "wiki", a.File)); h != "" {
			t.BeforeHash = &h
		}
		targets = append(targets, t)
	}
	return Batch{Targets: targets}
}

func findPending(pending []pendingEntry, path string) (pendingEntry, bool) {
	for _, p := range pending {
		if p.Path == path {
			return p, true
		}
	}
	return pendingEntry{}, false
}

// --- finalize ---

// finalize rebuilds indexes, drops done pending entries, and
// stamps last_commit on the whole yml.
func (c *Coordinator) finalize() {
	if _, err := buildHierarchicalIndexes(c.Job.RepoRoot, c.Yml); err != nil {
		// Index rebuild is best-effort — log via Job but don't
		// fail the Job for a render glitch.
		_ = err
	}

	// Drop done pending entries.
	var remaining []pendingEntry
	for _, p := range c.Yml.Pending {
		if p.Status != pendingStatusDone {
			remaining = append(remaining, p)
		}
	}
	c.Yml.Pending = remaining
	if len(c.Yml.Pending) == 0 {
		c.Yml.Pending = nil
		c.Yml.PlanSha = ""
		if c.Head != "" {
			h := c.Head
			c.Yml.LastCommit = &h
		}
	}
	c.persist()
}

// persist writes yml atomically.
func (c *Coordinator) persist() {
	_ = writeWikiYml(c.Job.RepoRoot, c.Yml)
}

func batchAllowlist(b *Batch) []string {
	out := make([]string, 0, len(b.Targets))
	for _, t := range b.Targets {
		out = append(out, "wiki/"+filepath.ToSlash(t.RelFile))
	}
	return out
}

// --- final reply rendering (§14) ---

func (c *Coordinator) formatFinalReply(runErr error) string {
	if runErr != nil {
		return "❌ /wiki failed: " + runErr.Error()
	}

	completed, failed := 0, 0
	for _, p := range c.Yml.Pending {
		switch p.Status {
		case pendingStatusFailed:
			failed++
		case pendingStatusDone, pendingStatusInProgress:
			completed++
		}
	}
	remaining := len(c.Yml.Pending)
	if remaining == 0 && failed == 0 {
		return "✅ /wiki\n\nAll wiki pages are up to date."
	}
	var b strings.Builder
	b.WriteString("✅ /wiki\n\n")
	fmt.Fprintf(&b, "Completed %d module page(s) this run.\n", completed)
	if failed > 0 {
		fmt.Fprintf(&b, "Failed %d module page(s); they remain pending.\n", failed)
	}
	if remaining > 0 {
		fmt.Fprintf(&b, "%d module page(s) remain; run /wiki again to continue.\n", remaining)
	}
	return b.String()
}
