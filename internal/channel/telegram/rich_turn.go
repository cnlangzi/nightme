package telegram

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/statusbar"
)

// ---------------------------------------------------------------------------
// richTurn — L3 per-turn single rich message state (docs §20.6.3).
//
// One richTurn per (chatID, topicID, userMessageID). Holds one
// Telegram message that gets PATCHed with each Out* event instead of
// accumulating chunks like the v9 chain. Pure in-memory; never
// persisted to telegram_state.json. Reset on turn end
// (OnPromptEnded).
//
// Conceptually a slimmed-down placeholderChain:
//   - no chunks / no chunk rotation / no chunk body parsing
//   - single message ID + a flat blocks array
//   - debounced editMessageText(rich_message=...) flush
//
// Replaces v9 chain behaviour for chain-attached kinds (OutReply /
// OutThinking / OutTool* / OutError / OutTask*) when RichMode is
// enabled. Chain remains intact for the RichMode=off path; L3
// retirement happens incrementally as RichMode users migrate.
// ---------------------------------------------------------------------------

// richTurnEntry is a pending entry not yet flushed to the rich
// message. Stored as a tuple (kind, body) so the render layer can
// decorate each kind consistently with the chain's appendSegment
// (e.g., add 💭 prefix to thinking, ```fences``` to errors).
type richTurnEntry struct {
	kind string // "thinking" | "tool_start" | "tool_end" | "error" | "task" | "reply"
	body string // markdown body, walked through markdownToRichBlocks on flush
}

// richTurn is the per-turn state. Holds one Telegram rich message
// and an in-memory list of pending entries to flush via
// editMessageText(rich_message=...).
//
// blocks mirrors what Telegram will receive on the next flush:
// header (when set) + entries rendered as paragraph blocks + a
// task section (when non-empty) + footer. The render layer rebuilds
// it from scratch on every flush so the wire form is always a
// function of the current state — same discipline the v9 chain
// applies via chunkBody.Compose().
type richTurn struct {
	mu sync.Mutex

	chatID        string
	topicID       int
	userMessageID int

	// messageID is the Telegram message_id of the rich message we
	// PATCH. Zero means the turn hasn't emitted yet; ensure() sets
	// it on first append.
	messageID int64

	// headerLine is the most recent header (heartbeat text). nil
	// → no header in the rendered blocks. Mirrors chain.chunks[0].header
	// semantics without the chain's per-chunk rotation.
	headerLine string

	// hasContent flips to true the moment the first entry / task
	// list / footer-bearing event lands. Once true, the renderer
	// stops using the cold-create default heartbeat text as a
	// fallback heading — the placeholder card shows the user's
	// content without a stale "🤖 Working…" banner once real
	// activity has begun. Real heartbeat updates set headerLine to
	// a non-default value, so the heading reappears with the
	// live count / timestamp / terminal verdict.
	hasContent bool

	// entries are pending content lines not yet flushed. Cleared
	// after every successful flush so the next event rebuilds from
	// scratch.
	entries []richTurnEntry

	// taskList is the most recent task snapshot. Same render path
	// as chain.chunks[0].taskList.
	taskList []taskListItem

	// footer is the most recent StatusBar footer. Set by footer-
	// bearing events; nil until first such event.
	footer []string

	dirty bool

	// debounceTimer coalesces a burst of edits into one
	// editMessageText call. v9 chain uses 250ms; same window here.
	debounceTimer *time.Timer

	// resultMessageID mirrors v9 P2 chain.resultMessageID: Telegram
	// message_id of the standalone OutResult (if any). OnPromptEnded
	// prefers this anchor for its terminal 🎉 reaction; falls back
	// to the rich turn's messageID when no OutResult landed.
	//
	// Set by sendOutResultMessage when it ships a rich path. Zero
	// when no OutResult was sent this turn.
	resultMessageID int64
}

// taskListItem is the minimal task snapshot the rich turn renders.
// Mirrors the chain's agent.AgentTaskItem shape — kept local to
// avoid pulling the agent package into the rich file's API.
type taskListItem struct {
	Status     string
	ID         string
	Subject    string
	ActiveForm string
}

// Append a text body to the rich turn. Returns an error if the
// rich message cold-create fails; otherwise returns nil after the
// entry is queued for the next debounced flush.
//
// footer, when non-nil, replaces the turn's footer — same
// "last-status-wins" semantics v9's chain.lastFooter applied.
// Pass nil to leave any previously-set footer in place (used when
// the caller has no status-bearing fields of its own).
//
// Callers (Send's chain-attached kind cases) propagate the error
// to runtime — the L3 migration's contract is "rich path failure
// surfaces, no silent truncation". A nil error means the entry
// was successfully queued; the actual flush happens 250ms later
// via scheduleRichTurnFlush.
func (a *Adapter) appendRichTurn(
	_ context.Context,
	chatID string,
	topicID int,
	userMessageID int,
	entry richTurnEntry,
	footer []string,
) error {
	turn := a.richTurns.getOrCreate(chatID, topicID, userMessageID)

	turn.mu.Lock()
	defer turn.mu.Unlock()

	// Lazy first-send: ensure the rich message exists before
	// scheduling an edit. On first call we don't have a messageID
	// yet, so we send (not edit). Subsequent calls edit.
	if turn.messageID == 0 {
		if err := a.sendRichTurnColdCreate(turn); err != nil {
			a.logger.Warn("telegram: rich turn cold-create failed",
				"chat_id", chatID,
				"err", err)
			return err
		}
	}

	turn.entries = append(turn.entries, entry)
	if footer != nil {
		turn.footer = footer
	}
	turn.hasContent = true
	turn.dirty = true
	a.scheduleRichTurnFlush(turn)
	return nil
}

// appendRichTurnAndFlush appends an entry and immediately flushes
// synchronously. Used by OutToolStart / OutToolEnd so the user sees
// `● Tool(args)` and `⎿ result` as adjacent blocks within the
// 250ms debounce window without waiting for the timer.
//
// Propagates cold-create errors so the caller can surface a "send
// failed" signal rather than silently dropping the tool line.
func (a *Adapter) appendRichTurnAndFlush(
	ctx context.Context,
	chatID string,
	topicID int,
	userMessageID int,
	entry richTurnEntry,
	footer []string,
) error {
	if err := a.appendRichTurn(ctx, chatID, topicID, userMessageID, entry, footer); err != nil {
		return err
	}
	turn, ok := a.richTurns.lookup(chatID, topicID, userMessageID)
	if !ok || turn == nil {
		return nil
	}
	// Stop any pending debounce; the synchronous flush supersedes it.
	turn.mu.Lock()
	if turn.debounceTimer != nil {
		turn.debounceTimer.Stop()
		turn.debounceTimer = nil
	}
	turn.mu.Unlock()
	return a.flushRichTurn(ctx, turn)
}

// scheduleRichTurnFlush arms a 250ms debounce timer that calls
// flushRichTurn. Caller MUST hold turn.mu.
func (a *Adapter) scheduleRichTurnFlush(turn *richTurn) {
	if turn.debounceTimer != nil {
		turn.debounceTimer.Stop()
	}
	turn.debounceTimer = time.AfterFunc(250*time.Millisecond, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := a.flushRichTurn(ctx, turn); err != nil && a.logger != nil {
			a.logger.Warn("telegram: rich turn flush failed",
				"chat_id", turn.chatID,
				"err", err)
		}
	})
}

// flushRichTurn sends the accumulated entries via editMessageText
// and clears the dirty bit. Called by the debounce timer; can also
// be called synchronously by OnPromptEnded before the 🎉 stamp.
func (a *Adapter) flushRichTurn(ctx context.Context, turn *richTurn) error {
	turn.mu.Lock()
	defer turn.mu.Unlock()

	if !turn.dirty || turn.messageID == 0 {
		return nil
	}
	blocksJSON, err := a.renderRichTurnBlocksLocked(turn)
	if err != nil {
		return err
	}
	if err := a.trySendRichBlocksEdit(ctx,
		turn.chatID, turn.messageID, blocksJSON); err != nil {
		return err
	}
	if a.logger != nil {
		a.logger.Info("telegram: rich turn flushed",
			"chat_id", turn.chatID,
			"message_id", turn.messageID,
			"has_content", turn.hasContent,
			"header_line", turn.headerLine,
			"blocks", blocksJSON,
		)
	}
	turn.dirty = false
	return nil
}

// renderRichTurnBlocksLocked builds the JSON blocks array for
// the rich message. Section ordering: header (when present) →
// entries (paragraphs) → task section (when non-empty) → footer
// (when present). The placeholder card shows the live turn state
// to the user in one place; this is the merged UI the user
// asked for when reverting from the draft-preview split.
//
// Caller MUST hold turn.mu.
// defaultRichTurnHeader is the placeholder banner emitted on
// cold-create and on every PATCH until the first OutHeartbeat
// stamps cache.headerLine. Without this fallback, the
// cold-create "🤖 Working..." heading disappears on the first
// PATCH (render skips empty headerLine) and only reappears when
// patchChainHeader fires later — which the user sees as
// "heartbeat line only shows up at the end of the turn".
const defaultRichTurnHeader = "🤖 Working..."

func (a *Adapter) renderRichTurnBlocksLocked(turn *richTurn) (string, error) {
	var blocks []map[string]any

	// Header (heading) — three states:
	//
	//   - headerLine == defaultRichTurnHeader && !hasContent:
	//         cold-create banner. Render it so the user sees
	//         immediate feedback that the turn is alive. This is
	//         the only path that shows the cold-create default.
	//   - headerLine is a real heartbeat / terminal verdict:
	//         OutHeartbeat fired and stamped the cache with the
	//         live text. Always render.
	//   - headerLine == defaultRichTurnHeader && hasContent:
	//         content arrived but no real heartbeat. The default
	//         banner is suppressed — showing "🤖 Working…" once
	//         entries are already on the card reads as a stale
	//         "agent still thinking" cue. Render nothing.
	//
	// Strip <b>/</b> wrappers — rich block text fields don't
	// parse HTML.
	headerEmitted := false
	if turn.headerLine != "" {
		skipDefault := turn.headerLine == defaultRichTurnHeader && turn.hasContent
		if !skipDefault {
			// Heartbeat / status line — emitted as a paragraph
			// block so it sits at the same text scale as the
			// surrounding entries rather than dominating the
			// card as a heading would.
			headerText := strings.TrimSpace(turn.headerLine)
			if headerText != "" {
				blocks = append(blocks, map[string]any{
					"type": "paragraph",
					"text": headerText,
				})
				headerEmitted = true
			}
		}
	}

	// Heartbeat / body break — divider sits between the
	// heartbeat line and the first body entry so the card
	// reads as three distinct regions: [heartbeat] ─ [body] ─
	// [footer]. Mirrors the footer divider below. Skipped
	// when no body follows (the cold-create "🤖 Working…"
	// banner alone, before any entry has landed) — a divider
	// with nothing below it reads as a stray HR.
	if headerEmitted && (len(turn.entries) > 0 || len(turn.taskList) > 0) {
		blocks = append(blocks, map[string]any{"type": "divider"})
	}

	// Entries: one paragraph per pending event.
	for _, e := range turn.entries {
		// Re-use the existing markdown→rich block walker so inline
		// formatting (bold / italic / code / url) survives the
		// partial-fallback path. If the walker returns ok=false
		// (e.g., the body has a table), we fall through to a plain
		// paragraph block — better than dropping the entry.
		if jsonStr, ok := markdownToRichBlocks(e.body); ok {
			blocks = append(blocks, mustParseBlocksArray(jsonStr)...)
			continue
		}
		// Plain paragraph fallback.
		blocks = append(blocks, map[string]any{
			"type": "paragraph",
			"text": e.body,
		})
	}

	// Task list (when present): emit a heading + native list so
	// the rich renderer can use Telegram's first-class list blocks.
	// The heading is a small but critical visual anchor — without
	// it the user sees an unlabeled block of paragraphs mid-message
	// and can't tell "this is the plan" apart from regular entries.
	// Emoji prefix (📋) keeps parity with feishu's `**📋 Tasks**`
	// heading and matches the convention other sections use for
	// region markers (🤖 Working… / 💰 Usage).
	//
	// Empty / nil taskList → no heading, no list block — same as
	// before. Mirrors the v9 chain's "no orphan headline" rule.
	if taskBlocks, ok := renderRichTurnTaskListBlocks(turn.taskList); ok {
		blocks = append(blocks, taskBlocks...)
	}

	// Footer: rendered as a dedicated InputRichBlockFooter block
	// (Telegram Bot API 10.1) with a preceding divider so the
	// statusbar reads as a distinct footer region, not a code
	// fence. The previous `type:"pre"` shape rendered the
	// chevron-tail frame inside a code block with a "copy" affordance
	// — visually noisy and indistinguishable from an LLM-emitted
	// code sample. The footer block type is exactly what Telegram
	// designed for "session metadata at the bottom of the message"
	// and is rendered by the client as a muted caption region.
	//
	// A divider (InputRichBlockDivider, <hr/>) sits between the
	// entries and the footer to give the eye a clean break before
	// the metadata block. footerLinesToRichText turns the statusbar
	// lines into a RichText value: a plain `\n`-joined string when
	// no line carries inline entities, or a `[]any` that mixes
	// each line's inline-rich representation (PR anchors as url
	// entities, bold/code where applicable) with `\n` separators.
	// Same helper sendOutResultMessage uses, so the standalone
	// result message and the rich turn placeholder render their
	// footers identically.
	if len(turn.footer) > 0 {
		blocks = append(blocks, map[string]any{
			"type": "divider",
		})
		blocks = append(blocks, map[string]any{
			"type": "footer",
			"text": footerLinesToRichText(turn.footer),
		})
	}

	return encodeBlocksArray(blocks)
}

// setRichTurnTaskList replaces the task snapshot wholesale. Same
// pattern as the v9 chain's setTaskList.
func (a *Adapter) setRichTurnTaskList(
	chatID string,
	topicID int,
	userMessageID int,
	items []taskListItem,
	footer []string,
) {
	turn := a.richTurns.getOrCreate(chatID, topicID, userMessageID)
	turn.mu.Lock()
	if len(items) == 0 {
		turn.taskList = nil
	} else {
		turn.taskList = items
	}
	if footer != nil {
		turn.footer = footer
	}
	turn.hasContent = true
	turn.dirty = true
	turn.mu.Unlock()
	a.scheduleRichTurnFlush(turn)
}

// updateRichTurnHeader replaces the header line (heartbeat text).
// Coalesces with the next debounce flush so a burst of heartbeat
// ticks results in one PATCH to Telegram, not one per snapshot.
// Empty header text is a no-op so the cold-create placeholder
// banner ("🤖 Working...") sticks until a real OutHeartbeat
// arrives.
func (a *Adapter) updateRichTurnHeader(
	chatID string,
	topicID int,
	userMessageID int,
	header string,
) {
	if header == "" {
		return
	}
	turn := a.richTurns.getOrCreate(chatID, topicID, userMessageID)
	turn.mu.Lock()
	if turn.headerLine == header {
		turn.mu.Unlock()
		return
	}
	turn.headerLine = header
	turn.dirty = true
	turn.mu.Unlock()
	a.scheduleRichTurnFlush(turn)
}

// OnPromptEndedRichTurn flushes the rich turn (if any) so the 🎉
// reaction stamps the fully-rendered final state. Mirrors the
// chain's flush-before-stamp discipline in v9.
func (a *Adapter) OnPromptEndedRichTurn(chatID string, topicID int, userMessageID int) {
	turn, ok := a.richTurns.lookup(chatID, topicID, userMessageID)
	if !ok || turn == nil {
		return
	}
	// Stop any pending debounce; we're flushing synchronously now.
	turn.mu.Lock()
	if turn.debounceTimer != nil {
		turn.debounceTimer.Stop()
		turn.debounceTimer = nil
	}
	turn.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = a.flushRichTurn(ctx, turn)
}

// OnPromptEndedRichMessageID returns the rich turn's message ID
// (the 🎉 anchor). Falls back to chain.resultMessageID semantics
// when the rich path wasn't used (rich messageID == 0).
func (a *Adapter) OnPromptEndedRichMessageID(chatID string, topicID int, userMessageID int) int64 {
	if turn, ok := a.richTurns.lookup(chatID, topicID, userMessageID); ok && turn != nil {
		turn.mu.Lock()
		mid := turn.messageID
		turn.mu.Unlock()
		if mid != 0 {
			return mid
		}
	}
	return 0
}

// sendRichTurnColdCreate sends the rich message that becomes the
// per-turn target. After this call, turn.messageID is set and
// subsequent events go via editMessageText(rich_message=...).
//
// Cold-create MUST include at least one block — Telegram rejects
// `{"blocks":[]}` with RICH_MESSAGE_EMPTY (issue #368). The
// placeholder banner is a paragraph block (not a heading) so
// the immediate-feedback text matches the scale of subsequent
// PATCH content. Subsequent edits replace this paragraph via
// renderRichTurnBlocksLocked.
//
// Caller MUST hold turn.mu.
func (a *Adapter) sendRichTurnColdCreate(turn *richTurn) error {
	body := `{"blocks":[{"type":"paragraph","text":"🤖 Working..."}]}`
	mid, err := a.trySendRichBlocksEditRaw(turn.chatID, 0, body, turn.topicID, turn.userMessageID)
	if err != nil {
		return err
	}
	turn.messageID = mid
	return nil
}

// trySendRichBlocksEditRaw is the lowest-level edit/send helper.
// When messageID == 0, sends a new message; otherwise edits. Used
// by the per-turn rich flow.
func (a *Adapter) trySendRichBlocksEditRaw(
	chatID string,
	messageID int64,
	richMessageJSON string,
	topicID int,
	replyToMessageID int,
) (int64, error) {
	params := map[string]any{
		"chat_id":      chatID,
		"rich_message": json.RawMessage(richMessageJSON),
	}
	if topicID > 0 {
		params["message_thread_id"] = topicID
	}
	if messageID == 0 {
		if replyToMessageID > 0 {
			params["reply_to_message_id"] = replyToMessageID
		}
		var result SendMessageResult
		if err := a.apiCall(context.Background(), "sendRichMessage", params, &result); err != nil {
			return 0, err
		}
		if result.MessageID == 0 {
			return 0, &apiError{Message: "sendRichMessage returned empty message_id"}
		}
		return int64(result.MessageID), nil
	}
	// editMessageText path.
	params["message_id"] = strconvFormatInt(messageID)
	if err := a.apiCall(context.Background(), "editMessageText", params, nil); err != nil {
		return 0, err
	}
	return messageID, nil
}

// trySendRichBlocksEdit is the edit-only path (messageID > 0).
func (a *Adapter) trySendRichBlocksEdit(
	ctx context.Context,
	chatID string,
	messageID int64,
	blocksJSON string,
) error {
	if messageID == 0 {
		return &apiError{Message: "trySendRichBlocksEdit: zero messageID"}
	}
	// Wrap blocksJSON in {"blocks": ...} envelope. encoding/json
	// inlines json.RawMessage verbatim so the wire form is
	// `rich_message={"blocks":[...]}`, exactly what Telegram expects.
	params := map[string]any{
		"chat_id":      chatID,
		"message_id":   messageID,
		"rich_message": json.RawMessage(`{"blocks":` + blocksJSON + `}`),
	}
	return a.apiCall(ctx, "editMessageText", params, nil)
}

// mustParseBlocksArray parses a JSON array string and asserts that
// every element is a JSON object (map[string]any). Used to convert
// the walker's JSON output into the []map[string]any slice the
// renderRichTurnBlocksLocked builder expects.
//
// Returns nil on parse failure; callers fall through to the
// plain-paragraph fallback. The walker output is produced by
// json.Marshal over a controlled shape, so a failure here means
// the walker contract drifted — flag it on the logger so the
// regression surfaces.
func mustParseBlocksArray(jsonStr string) []map[string]any {
	var arr []map[string]any
	if err := json.Unmarshal([]byte(jsonStr), &arr); err != nil {
		return nil
	}
	return arr
}

// encodeBlocksArray JSON-encodes a []map[string]any slice. Returns
// "" + error on failure (caller should fall back to plain text).
func encodeBlocksArray(blocks []map[string]any) (string, error) {
	b, err := json.Marshal(blocks)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// strconvFormatInt is a tiny helper that wraps strconv.FormatInt with
// base 10. Pulled out so the apiCall call site stays compact.
func strconvFormatInt(n int64) string {
	return strconv.FormatInt(n, 10)
}

// richTurns is the Adapter-scoped index of per-turn richTurn state.
// Capacity-bounded with simple FIFO eviction (no LRU — per-turn
// state is short-lived and OnPromptEnded purges aggressively).
type richTurnsIndex struct {
	mu    sync.Mutex
	cap   int
	turns map[richTurnKey]*richTurn
	order []richTurnKey // insertion order, head = oldest
}

type richTurnKey struct {
	chatID        string
	topicID       int
	userMessageID int
}

const defaultRichTurnCap = 1000

func newRichTurnsIndex(cap int) *richTurnsIndex {
	if cap <= 0 {
		cap = defaultRichTurnCap
	}
	return &richTurnsIndex{
		cap:   cap,
		turns: make(map[richTurnKey]*richTurn),
	}
}

func (r *richTurnsIndex) getOrCreate(chatID string, topicID int, userMessageID int) *richTurn {
	key := richTurnKey{chatID: chatID, topicID: topicID, userMessageID: userMessageID}
	r.mu.Lock()
	defer r.mu.Unlock()

	if t, ok := r.turns[key]; ok {
		return t
	}
	if len(r.turns) >= r.cap {
		oldest := r.order[0]
		r.order = r.order[1:]
		delete(r.turns, oldest)
	}
	t := &richTurn{
		chatID:        chatID,
		topicID:       topicID,
		userMessageID: userMessageID,
	}
	r.turns[key] = t
	r.order = append(r.order, key)
	return t
}

func (r *richTurnsIndex) lookup(chatID string, topicID int, userMessageID int) (*richTurn, bool) {
	key := richTurnKey{chatID: chatID, topicID: topicID, userMessageID: userMessageID}
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.turns[key]
	return t, ok
}

func (r *richTurnsIndex) purge(chatID string, topicID int, userMessageID int) {
	key := richTurnKey{chatID: chatID, topicID: topicID, userMessageID: userMessageID}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.turns, key)
	for i, k := range r.order {
		if k == key {
			r.order = append(r.order[:i], r.order[i+1:]...)
			return
		}
	}
}

// size returns the current number of turns in the index. Used by
// HealthSnapshot to expose pending rich turn count to operators.
func (r *richTurnsIndex) size() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.turns)
}

// unused — kept for compile-time reference of statusbar import
// path (the rich turn could pull in custom StatusBar rendering in
// the future without re-adding the import).
var _ = statusbar.RenderPanel

// taskStatusToString maps agent.AgentTaskStatus to a stable string
// the rich block renderer's list section consumes. Mirrors the
// v9 chain's renderTaskLine switch (chain_body.go). Strings are
// stable on the wire (no enum-name leakage) so future agent
// status additions don't ripple into the rich-block schema.
func taskStatusToString(s agent.AgentTaskStatus) string {
	switch s {
	case agent.TaskPending:
		return "pending"
	case agent.TaskInProgress:
		return "in_progress"
	case agent.TaskCompleted:
		return "completed"
	case agent.TaskDeleted:
		return "deleted"
	case agent.TaskCancelled:
		return "cancelled"
	}
	return "pending"
}

// richTurnTaskListHeadline is the pre-baked text prepended to the
// task section. Kept as a package-level const so the §11.12 docs and
// the tests pin the same string — a future change has one source of
// truth. The 📋 emoji matches feishu's `**📋 Tasks**` heading for
// cross-channel visual parity.
const richTurnTaskListHeadline = "📋 Tasks"

// richTurnTaskListBudgetRunes caps the rendered task section
// (headline + list rows summed). Telegram's rich message limit is
// 32K chars total, but the card has other regions (header /
// entries / footer / dividers) competing for that budget, so the
// task section gets a generous-but-bounded slice. Long task lists
// truncate at this budget with a "…" tail — same shape as feishu's
// checklistBudgetRunes / checklistMore pair, so a 100-task snapshot
// renders the first ~30 rows + truncation marker.
const richTurnTaskListBudgetRunes = 3000

// richTurnTaskListMore is the inline tail appended to the last
// visible row when the rune budget truncated the list. Pure
// ellipsis — matches feishu's checklistMore constant.
const richTurnTaskListMore = "…"

// richTurnTaskListOverflowPlaceholder is the single-row fallback
// when the renderer drops every row to fit the budget. Mirrors
// feishu's checklistOverflowPlaceholder so a degenerate giant
// subject still surfaces a single task row instead of an empty
// list block.
const richTurnTaskListOverflowPlaceholder = "• …"

// renderRichTurnTaskListBlock builds the two-block section emitted
// for a non-empty taskList: a `heading` block (📋 Tasks) followed
// by a `list` block of native Telegram list items. Returns ok=false
// when items is empty (no blocks emitted) so callers can short-
// circuit without an explicit length check.
//
// Pipeline:
//   1. Filter rows whose status is terminal-but-not-rendered
//      (cancelled / deleted). Other statuses (pending / in_progress
//      / completed) always render — better than silently dropping a
//      row the bridge meant to surface. Feishu does the same filter
//      at checklistBudgetRunes (buildTaskChecklistChunks).
//   2. Truncate trailing rows when the rendered rune count would
//      exceed richTurnTaskListBudgetRunes. Whole-row drop — never
//      mid-row — so the list shape stays well-formed. The last
//      visible row gets a "…" suffix so the user sees the truncation.
//   3. Compose each visible row into a Telegram list item: one
//      paragraph per item, with status-specific prefix:
//        pending     → "• <Subject>"
//        in_progress → "• <Subject> (<ActiveForm>)"
//        completed   → "✓ <Subject>"
//   4. Prepend the heading block. Heading is unconditional when
//      any task row is visible — the user needs the visual anchor
//      to know "this is the plan" rather than "these are random
//      entries mid-message".
func renderRichTurnTaskListBlocks(items []taskListItem) ([]map[string]any, bool) {
	if len(items) == 0 {
		return nil, false
	}

	// Filter out terminal-but-not-rendered statuses first — these
	// must not consume the rune budget (they're invisible downstream).
	visible := make([]taskListItem, 0, len(items))
	for _, it := range items {
		switch it.Status {
		case "cancelled", "deleted":
			// Terminal statuses that don't make sense on a live
			// checklist — drop them. Feishu buildTaskChecklistChunks
			// filters the same way (TaskInProgress / TaskPending /
			// TaskCompleted only).
			continue
		}
		visible = append(visible, it)
	}
	if len(visible) == 0 {
		// Every item filtered out → no heading, no list block. Same
		// shape as the empty-input guard at the top.
		return nil, false
	}

	// Rune-budgeted row selection. The budget is consumed by BOTH
	// rows AND the trailing "…" suffix (so the marker doesn't push
	// a real row over the limit).
	const headlineRunes = 9 // "📋 Tasks" is 9 runes (1 emoji + " Tasks")
	rows := make([]map[string]any, 0, len(visible))
	total := headlineRunes
	rendered := 0
	for _, it := range visible {
		rowText := renderTaskRowText(it)
		cost := utf8.RuneCountInString(rowText) + 1 // +1 for separator
		// Account for the trailing "…" suffix on the last visible
		// row when this is the row we're about to render — keeps
		// the truncated row's total cost inside the budget.
		if total+cost+1 > richTurnTaskListBudgetRunes {
			break
		}
		rows = append(rows, map[string]any{
			"blocks": []map[string]any{
				{"type": "paragraph", "text": rowText},
			},
		})
		total += cost
		rendered++
	}
	if rendered == 0 {
		// First row alone overflowed the budget — emit a single
		// placeholder row so the list block stays well-formed and
		// the user still sees the section header above it.
		return []map[string]any{
			{"type": "heading", "text": richTurnTaskListHeadline, "size": 1},
			{
				"type": "list",
				"items": []map[string]any{
					{"blocks": []map[string]any{
						{"type": "paragraph", "text": richTurnTaskListOverflowPlaceholder},
					}},
				},
			},
		}, true
	}
	if rendered < len(visible) {
		// Append "…" suffix to the last visible row so the user
		// sees the truncation marker. Matches feishu's
		// `chunks[len-1] += " " + checklistMore` pattern.
		last := rows[rendered-1]
		// last["blocks"][0]["text"] — guard with type assertion so a
		// future block-shape refactor doesn't NPE here.
		if bs, ok := last["blocks"].([]map[string]any); ok && len(bs) > 0 {
			if p, ok := bs[0]["text"].(string); ok {
				bs[0]["text"] = p + " " + richTurnTaskListMore
			}
		}
	}

	return []map[string]any{
		{"type": "heading", "text": richTurnTaskListHeadline, "size": 1},
		{
			"type":  "list",
			"items": rows,
		},
	}, true
}

// renderTaskRowText formats one task row for the rich block list.
// Status enum decides only the prefix shape (• / ✓); the row shape
// is identical for every status so the output reads as a single
// coherent list.
//
//   - pending     → • <Subject>
//   - in_progress → • <Subject> (<ActiveForm>)    (open dot + soft suffix)
//   - completed   → ✓ <Subject>                    (check mark)
//
// Empty Subject falls back to the task ID so a malformed snapshot
// still produces a non-empty row. In_progress rows only show the
// ActiveForm suffix when it's non-empty (no trailing parens for
// empty forms).
func renderTaskRowText(it taskListItem) string {
	subject := strings.TrimSpace(it.Subject)
	if subject == "" {
		subject = it.ID
	}
	switch it.Status {
	case "completed":
		return "✓ " + subject
	case "in_progress":
		if it.ActiveForm != "" {
			return "• " + subject + " (" + it.ActiveForm + ")"
		}
		return "• " + subject
	}
	return "• " + subject
}
