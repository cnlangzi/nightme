package telegram

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

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
	body string // markdown body, rendered by the same renderMarkdownSafe the chain uses
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

// Append a text body to the rich turn. Returns true on success
// (message created or PATCH scheduled). Caller (Send cases)
// doesn't need the return value — failure here is best-effort and
// the chain fallback already kicked in upstream.
func (a *Adapter) appendRichTurn(
	ctx context.Context,
	chatID string,
	topicID int,
	userMessageID int,
	entry richTurnEntry,
) {
	if !a.richModeAllowsSend() {
		return
	}
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
			return
		}
	}

	turn.entries = append(turn.entries, entry)
	turn.dirty = true
	a.scheduleRichTurnFlush(turn)
}

// appendRichTurnAndFlush appends an entry and immediately flushes
// synchronously. Used by OutToolStart / OutToolEnd so the user sees
// `● Tool(args)` and `⎿ result` as adjacent blocks within the
// 250ms debounce window without waiting for the timer.
func (a *Adapter) appendRichTurnAndFlush(
	ctx context.Context,
	chatID string,
	topicID int,
	userMessageID int,
	entry richTurnEntry,
) error {
	if !a.richModeAllowsSend() {
		return nil
	}
	a.appendRichTurn(ctx, chatID, topicID, userMessageID, entry)
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
	turn.dirty = false
	return nil
}

// renderRichTurnBlocksLocked builds the JSON blocks array for the
// rich message. Mirrors chunkBody.Compose()'s section ordering:
// header (when present) → entries (paragraphs) → task section
// (when non-empty) → footer.
//
// Caller MUST hold turn.mu.
func (a *Adapter) renderRichTurnBlocksLocked(turn *richTurn) (string, error) {
	var blocks []map[string]any

	// Header: optional, single heading-style block. Use heading size 1
	// for max visibility (matches the cold "🤖 Working..." banner).
	if turn.headerLine != "" {
		blocks = append(blocks, map[string]any{
			"type": "heading",
			"text": turn.headerLine,
			"size": 1,
		})
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

	// Task list (when present): emit a heading + bullet list so
	// the rich renderer can use native list blocks. v9 chain
	// uses a custom "📋 Tasks" markdown todo list; here we use
	// the explicit list/heading blocks which render more crisply.
	if len(turn.taskList) > 0 {
		var items []map[string]any
		for _, t := range turn.taskList {
			text := t.Subject
			if text == "" {
				text = t.ID
			}
			if t.Status == "completed" {
				text = "✓ " + text
			}
			items = append(items, map[string]any{
				"blocks": []map[string]any{
					{"type": "paragraph", "text": text},
				},
			})
		}
		blocks = append(blocks, map[string]any{
			"type":  "list",
			"items": items,
		})
	}

	// Footer: rendered as a fenced code block so box-drawing chars
	// (┌──› / └──›) preserve. Falls back to a single paragraph if
	// we want richer footer rendering in the future.
	if len(turn.footer) > 0 {
		blocks = append(blocks, map[string]any{
			"type": "pre",
			"text": strings.Join(turn.footer, "\n"),
		})
	}

	return encodeBlocksArray(blocks)
}

// ensureRichTurn is the lazy first-event hook. Called from Send
// before the first Out* event of a turn when RichMode is on. We
// don't actually send anything here — the rich message is created
// on the first appendRichTurn call. This function is reserved for
// future "send a heading-only placeholder immediately" semantics
// that match the v9 chain's "🤖 Working..." cold-create.
//
// Currently a no-op placeholder to keep Send's call site stable
// across future enhancements (e.g., showing a banner while waiting
// for the agent to think).
func (a *Adapter) ensureRichTurn(chatID string, topicID int, userMessageID int) {
	if !a.richModeAllowsSend() {
		return
	}
	_ = a.richTurns.getOrCreate(chatID, topicID, userMessageID)
}

// updateRichTurnHeader replaces the header line (heartbeat text).
// Coalesces with the next debounce flush.
func (a *Adapter) updateRichTurnHeader(
	chatID string,
	topicID int,
	userMessageID int,
	header string,
) {
	if !a.richModeAllowsSend() {
		return
	}
	turn := a.richTurns.getOrCreate(chatID, topicID, userMessageID)
	turn.mu.Lock()
	turn.headerLine = header
	turn.dirty = true
	turn.mu.Unlock()
	a.scheduleRichTurnFlush(turn)
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
	if !a.richModeAllowsSend() {
		return
	}
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
// Currently uses an empty blocks array; the first appendRichTurn
// triggers a flush that adds the first entry. Future work could
// pre-populate with a "🤖 Working..." heading.
//
// Caller MUST hold turn.mu.
func (a *Adapter) sendRichTurnColdCreate(turn *richTurn) error {
	body := `{"blocks":[]}`
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
	return jsonNumberString(n)
}

// jsonNumberString formats an int64 as JSON-compatible number bytes.
// Defined separately to avoid importing strconv just for one callsite.
func jsonNumberString(n int64) string {
	if n == 0 {
		return "0"
	}
	negative := n < 0
	if negative {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if negative {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
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
