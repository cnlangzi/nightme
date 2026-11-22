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
	taskList []agent.AgentTaskItem

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

// Append a text body to the rich turn. Returns the id of the
// message the turn's card lives on, or an error if the rich message
// cold-create failed.
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
) (string, error) {
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
			return "", err
		}
	}

	turn.entries = append(turn.entries, entry)
	if footer != nil {
		turn.footer = footer
	}
	turn.hasContent = true
	turn.dirty = true
	a.scheduleRichTurnFlush(turn)
	return strconv.FormatInt(turn.messageID, 10), nil
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
) (string, error) {
	msgID, err := a.appendRichTurn(ctx, chatID, topicID, userMessageID, entry, footer)
	if err != nil {
		return "", err
	}
	turn, ok := a.richTurns.lookup(chatID, topicID, userMessageID)
	if !ok || turn == nil {
		return msgID, nil
	}
	// Stop any pending debounce; the synchronous flush supersedes it.
	turn.mu.Lock()
	if turn.debounceTimer != nil {
		turn.debounceTimer.Stop()
		turn.debounceTimer = nil
	}
	turn.mu.Unlock()
	if err := a.flushRichTurn(ctx, turn); err != nil {
		return msgID, err
	}
	return msgID, nil
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
	items []agent.AgentTaskItem,
	footer []string,
) string {
	turn := a.richTurns.getOrCreate(chatID, topicID, userMessageID)
	turn.mu.Lock()
	if turn.messageID == 0 {
		// The checklist is content: a turn that has no card yet gets
		// one, the way a reply opens a card of its own. Without it
		// the checklist would have nowhere to go — the flush has no
		// message to patch.
		if err := a.sendRichTurnColdCreate(turn); err != nil {
			turn.mu.Unlock()
			a.logger.Warn("telegram: rich turn cold-create failed",
				"chat_id", chatID,
				"err", err)
			return ""
		}
	}
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
	msgID := ""
	if turn.messageID != 0 {
		msgID = strconv.FormatInt(turn.messageID, 10)
	}
	turn.mu.Unlock()
	a.scheduleRichTurnFlush(turn)
	return msgID
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
	a.richTurns.registerMessage(turn, mid)
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

	// byMessage indexes the turns by the id of the message they own:
	// a card is addressed by its own message id.
	byMessage map[richTurnMsgKey]richTurnKey
	// msgOf is the reverse of byMessage: the message a turn owns.
	// The eviction needs it to drop the byMessage entry of a turn
	// that goes away.
	msgOf map[richTurnKey]int64

	// orphanSeq numbers the turns that open without an anchor. Each
	// of them is a message of its own.
	orphanSeq int
}

type richTurnKey struct {
	chatID        string
	topicID       int
	userMessageID int
}

// richTurnMsgKey names a chat message that a turn owns.
type richTurnMsgKey struct {
	chatID    string
	messageID int64
}

const defaultRichTurnCap = 1000

func newRichTurnsIndex(cap int) *richTurnsIndex {
	if cap <= 0 {
		cap = defaultRichTurnCap
	}
	return &richTurnsIndex{
		cap:       cap,
		turns:     make(map[richTurnKey]*richTurn),
		byMessage: make(map[richTurnMsgKey]richTurnKey),
		msgOf:     make(map[richTurnKey]int64),
	}
}

// getOrCreate returns the turn the key names, creating it when it
// does not exist yet.
//
// A message id that names a card is the card's own address: the
// turn that owns the message is the turn the event belongs to. The
// card the chat wrote is patched in place instead of a second card
// being opened.
func (r *richTurnsIndex) getOrCreate(chatID string, topicID int, userMessageID int) *richTurn {
	r.mu.Lock()
	defer r.mu.Unlock()

	if turn, ok := r.turnByMessageLocked(chatID, userMessageID); ok {
		return turn
	}
	key := richTurnKey{chatID: chatID, topicID: topicID, userMessageID: userMessageID}
	if t, ok := r.turns[key]; ok {
		return t
	}
	if len(r.turns) >= r.cap {
		oldest := r.order[0]
		r.order = r.order[1:]
		delete(r.turns, oldest)
		if mid, ok := r.msgOf[oldest]; ok {
			delete(r.byMessage, richTurnMsgKey{chatID: oldest.chatID, messageID: mid})
			delete(r.msgOf, oldest)
		}
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

// turnByMessageLocked returns the turn that owns the message. The
// caller holds r.mu.
func (r *richTurnsIndex) turnByMessageLocked(chatID string, messageID int) (*richTurn, bool) {
	if messageID <= 0 {
		return nil, false
	}
	key, ok := r.byMessage[richTurnMsgKey{chatID: chatID, messageID: int64(messageID)}]
	if !ok {
		return nil, false
	}
	t, ok := r.turns[key]
	return t, ok
}

// registerMessage records that the turn owns the message: the card
// is addressed by its own message id from now on.
func (r *richTurnsIndex) registerMessage(turn *richTurn, messageID int64) {
	if turn == nil || messageID == 0 {
		return
	}
	key := richTurnKey{chatID: turn.chatID, topicID: turn.topicID, userMessageID: turn.userMessageID}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byMessage[richTurnMsgKey{chatID: turn.chatID, messageID: messageID}] = key
	r.msgOf[key] = messageID
}

// orphanAnchor mints the key of a turn that opens without an anchor.
//
// Every orphan turn is a message of its own: the events that follow
// name the message it opened.
func (r *richTurnsIndex) orphanAnchor() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.orphanSeq++
	return -r.orphanSeq
}

func (r *richTurnsIndex) lookup(chatID string, topicID int, userMessageID int) (*richTurn, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if turn, ok := r.turnByMessageLocked(chatID, userMessageID); ok {
		return turn, true
	}
	key := richTurnKey{chatID: chatID, topicID: topicID, userMessageID: userMessageID}
	t, ok := r.turns[key]
	return t, ok
}

func (r *richTurnsIndex) purge(chatID string, topicID int, userMessageID int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := richTurnKey{chatID: chatID, topicID: topicID, userMessageID: userMessageID}
	delete(r.turns, key)
	if mid, ok := r.msgOf[key]; ok {
		delete(r.byMessage, richTurnMsgKey{chatID: chatID, messageID: mid})
		delete(r.msgOf, key)
	}
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

// Task section emitted by renderRichTurnTaskListBlocks. 📋 Tasks
// heading matches feishu's `**📋 Tasks**` for cross-channel parity;
// the rest of the consts are the feishu checklistBudgetRunes /
// checklistMore / checklistOverflowPlaceholder triple.
const (
	richTurnTaskListHeadline            = "📋 Tasks"
	richTurnTaskListBudgetRunes         = 3000
	richTurnTaskListMore                = "…"
	richTurnTaskListOverflowPlaceholder = "1. [ ] …"
)

// renderRichTurnTaskListBlock builds the heading + native list
// blocks emitted for a non-empty taskList. Returns ok=false when
// items is empty so callers can short-circuit without an explicit
// length check. Two-pass: collect row texts under the rune budget,
// then assemble block maps (so the truncation suffix is appended
// once at the source rather than re-mutating a block map).
//
// The budget accounts for the headline + every row + the trailing
// " " + … suffix on the last row (when truncated). Every state a task
// can be in is rendered: a task that failed or was cancelled is a row
// the operator has to see. A deleted task is not: the bridges drop
// it before they emit the snapshot.
//
// The text of a row is the shared row of agent.VisibleTaskRows,
// number included: Telegram's `list` block has no ordered flag, so
// the number rides in the text of the item.
func renderRichTurnTaskListBlocks(items []agent.AgentTaskItem) ([]map[string]any, bool) {
	if len(items) == 0 {
		return nil, false
	}
	visible := agent.VisibleTaskRows(items)
	if len(visible) == 0 {
		return nil, false
	}

	headlineRunes := utf8.RuneCountInString(richTurnTaskListHeadline)
	// +2 for the trailing " " + … suffix when the list is truncated.
	const truncationSuffixRunes = 2
	rowTexts := make([]string, 0, len(visible))
	total := headlineRunes
	for _, rowText := range visible {
		cost := utf8.RuneCountInString(rowText) + 1 // +1 for inter-row separator
		if total+cost+truncationSuffixRunes > richTurnTaskListBudgetRunes {
			break
		}
		rowTexts = append(rowTexts, rowText)
		total += cost
	}
	// When even the first row overflowed the budget, fall back to
	// a single placeholder row so the list block stays well-formed
	// and the user still sees the section header.
	if len(rowTexts) == 0 {
		rowTexts = []string{richTurnTaskListOverflowPlaceholder}
	} else if len(rowTexts) < len(visible) {
		// Truncation marker: appended to the last visible row's
		// text before block construction so we never have to
		// re-mutate the block map after building it.
		rowTexts[len(rowTexts)-1] += " " + richTurnTaskListMore
	}

	items2 := make([]map[string]any, 0, len(rowTexts))
	for _, t := range rowTexts {
		items2 = append(items2, map[string]any{
			"blocks": []map[string]any{
				{"type": "paragraph", "text": t},
			},
		})
	}
	return []map[string]any{
		{"type": "heading", "text": richTurnTaskListHeadline, "size": 1},
		{"type": "list", "items": items2},
	}, true
}
