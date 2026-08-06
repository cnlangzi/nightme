// F-think §3.1.2: markdown rendering for OutThinking.
//
// OutThinking events are rendered as Feishu Card 2.0 interactive
// cards whose body is a single (or multi-div) lark_md element, so
// code blocks / lists / emphasis survive the round-trip. Plain text
// would lose all formatting — the user would see raw markdown
// source instead of rendered prose.
//
// This file owns two helpers:
//
//   - buildThinkingCard(body) builds the Card 2.0 JSON envelope.
//     It uses F-37's splitMarkdownForDivs to chunk long bodies
//     into per-div slices that respect Feishu's hard limits while
//     keeping code blocks atomic.
//
//   - postThreadMarkdownReply is the parallel sibling of
//     postThreadReply: same shape (rate limit → SendCard → thread
//     reply), different transport (interactive card instead of
//     plain text).
//
// The split is intentionally simple — there is no per-tool-name
// awareness here (that's summarize_tool.go's job for OutToolEnd);
// OutThinking is the raw agent reasoning and gets rendered as-is.
package feishu

import (
	"context"
	"errors"
	"fmt"
)

// buildThinkingCard wraps a thinking body in a Card 2.0 JSON
// payload. Content longer than divTextCharLimit runes (the Feishu
// hard limit on a single div.text.content — see receipt.go) is
// split into multiple div elements via F-37 splitMarkdownForDivs,
// so Feishu renders each div without truncation. Code blocks and
// list blocks stay atomic across splits — see F-37's atomic-split
// guarantees.
//
// Body is expected to include any leading emoji / marker (e.g. the
// "💭 " prefix that Adapter.Send prepends). The prefix lives
// inside the lark_md content so Feishu renders it inline; this
// keeps the card visually anchored as "thinking" content even
// after markdown wrapping.
//
// Returns an error when body is empty — there's nothing to render.
// Adapter.Send's OutThinking case already guards against empty
// text via gateway.Translate, but the helper is total for direct
// callers (tests, future internal callers).
func buildThinkingCard(body string) (string, error) {
	if body == "" {
		return "", errors.New("feishu: thinking body is empty")
	}

	// Sanitize markdown so we don't ship non-HTTP links (230001 invalid
	// href) or un-newlined code fences (lark_md parses as inline code).
	// Mirrors the OutResult surface; see card_sanitize.go doc.
	body = SanitizeCardMarkdown(body)
	chunks := splitMarkdownForDivs(body, divTextCharLimit)

	elements := make([]map[string]any, 0, len(chunks))
	for _, c := range chunks {
		elements = append(elements, map[string]any{
			"tag":  "div",
			"text": map[string]any{"tag": "lark_md", "content": c},
		})
	}

	card := map[string]any{
		"config":   map[string]any{"wide_screen_mode": true},
		"elements": elements,
	}
	// encodeCardJSON (not json.Marshal) — see adapter.go for the
	// rationale: SetEscapeHTML(false) keeps <, >, & as their
	// literal HTML entities instead of < / > / &.
	// lark_md accepts inline HTML (<font color='red'>, etc.) and
	// the literal form matches Feishu's docs. Agent reasoning
	// routinely surfaces Go generics (Foo[Bar]), comparison code,
	// and HTML snippets where default escaping would corrupt the
	// visible content and produce wire-format divergence vs every
	// other card this adapter sends.
	out, err := encodeCardJSON(card)
	if err != nil {
		return "", fmt.Errorf("feishu: marshal thinking card: %w", err)
	}
	return string(out), nil
}

// postThreadMarkdownReply is the markdown-rendering sibling of
// postThreadReply. Same shape (rate limit → thread reply via
// rootID), different transport:
//
//   - postThreadReply    → larkim.MsgTypeText     (plain text)
//   - postThreadMarkdownReply → larkim.MsgTypeInteractive (lark_md card)
//
// Used exclusively by the OutThinking case in Adapter.Send.
// replyOnly=true (the default for thinking content per F-37) keeps
// the card out of the main chat — it lives only in the thread, so
// the receipt card stays the pinned final-answer view in the main
// chat.
//
// Fail-open: when rootID is empty (orphan event, e.g. startup init),
// falls back to plain text via sendRawOutText — exactly like
// postThreadReply's rootID=="" path. This keeps the two sibling
// helpers' orphan-event behaviour aligned: OutToolStart /
// OutToolEnd / OutCompaction / OutThinking all degrade to the
// same plain-text bubble when there's no user message to thread
// to. A divergent fallback (markdown card vs plain text) would
// surface as an inconsistent UX on the rare orphan path.
//
// In practice this branch is unreachable for any OutboundKind
// during normal flow (Translate + the runtime's EventCallback
// always stamp ReplyTo for in-turn events). The fallback exists
// so the helper is total and never silently drops content.
//
// Limiter ordering: Wait() FIRST, then any work. Matches
// postThreadReply exactly. Rationale: when a chat is saturated
// (high-QPS hot agent), every rejected OutThinking would
// otherwise pay full JSON-encode cost (splitMarkdownForDivs scan
// + ~5KB envelope) before being told "rate-limited". The build
// is the expensive path; the limiter check is cheap. Putting the
// expensive path first wastes the work; putting the cheap path
// first rejects cheaply. (F-35 / F-34 review P1-4 established
// this ordering for the sibling helper.)
func (a *Adapter) postThreadMarkdownReply(ctx context.Context, chatID, rootID, body string, replyOnly bool) error {
	// F-35 / F-34 review P1-4: thread-reply rate limit (5 QPS
	// per chat on Feishu). Same limiter as postThreadReply so
	// thinking + tool + compaction events share one bucket.
	if a.threadReplyLimiter != nil {
		if err := a.threadReplyLimiter.Wait(ctx, chatID); err != nil {
			a.logger.Warn("feishu: thread markdown reply rate-limited",
				"chat_id", chatID, "err", err)
			return err
		}
	}

	if rootID == "" {
		// Aligns with postThreadReply's orphan fallback: plain
		// text in main chat. Both siblings share the same
		// degrade-mode contract for rootID=="" events.
		return a.sendRawOutText(ctx, chatID, body)
	}

	cardJSON, err := buildThinkingCard(body)
	if err != nil {
		return err
	}

	_, err = a.sendCardContent(ctx, chatID, cardJSON, rootID, replyOnly)
	return err
}