package feishu

// F-14 inbound attachment handling.
//
// Feishu delivers image / file / audio / video messages as separate
// msg_type payloads whose content is a JSON envelope carrying
// image_key / file_key + (optional) file_name. To make these
// useful to the downstream agent, nightme must:
//
//  1. Extract the {Type, FileKey, FileName} triples at receive time
//     (extractAttachments, called from adapter.handleMessage).
//  2. Download the binary content into a per-session inbox directory
//     (DownloadAttachments). The download is synchronous with the
//     inbound event — see docs/feat/F-14-attachment-passthrough.md
//     for the rationale (one Feishu message = one agent turn).
//  3. Translate the downloaded attachments into agent.ContentBlock
//     values (BuildBlocks) and forward via SendBlocks — the
//     abstract agent protocol carries rich-text blocks; the Claude
//     Code bridge base64-encodes images, the PTY bridge uses
//     "@<path>" syntax.
//
// Stickers are silently skipped because Feishu blocks the resource
// download for sticker msg_type.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/channel"
)

// maxDownloadAttempts is the per-attachment retry budget. Total
// attempts = 3 (1 initial + 2 retries) with exponential backoff
// between them. The first retry waits 500ms, the second 1500ms.
const maxDownloadAttempts = 3

// initialBackoff / backoffMultiplier / maxBackoff bound the retry
// sleep schedule. Kept tight because downloads are on the inbound
// hot path — users see latency.
const (
	initialBackoff     = 500 * time.Millisecond
	backoffMultiplier  = 3
	maxBackoffDuration = 5 * time.Second
)

// InboxBaseDir is the global inbox root. Each session gets a
// sub-directory under this path: <InboxBaseDir>/<sessionID>/.
// 0700 permissions so other users on the box cannot list or
// read downloaded files.
var InboxBaseDir = defaultInboxBaseDir

// defaultInboxBaseDir returns ~/.nightme/inbox, creating it if
// necessary. Tests override InboxBaseDir to a temp dir.
func defaultInboxBaseDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("inbox: resolve home: %w", err)
	}
	base := filepath.Join(home, ".nightme", "inbox")
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", fmt.Errorf("inbox: mkdir %s: %w", base, err)
	}
	return base, nil
}

// resolveBaseDir returns InboxBaseDir, or falls back to the default
// if the override is unset / nil.
func resolveBaseDir() (string, error) {
	if InboxBaseDir != nil {
		return InboxBaseDir()
	}
	return defaultInboxBaseDir()
}

// inboxDirForSession returns the inbox directory for the given
// isolation key (typically chatID for the Feishu adapter), creating
// it on demand. The key forms the on-disk subdirectory under
// InboxBaseDir; concurrent chats get distinct subdirs so they
// never stomp on each other's downloads.
//
// Named "sessionID" for backwards compatibility with callers that
// previously passed the nightme session ID; the Feishu adapter
// passes chatID today, which gives per-chat isolation stable across
// AgentSession re-spawns.
func inboxDirForSession(sessionID string) (string, error) {
	base, err := resolveBaseDir()
	if err != nil {
		return "", err
	}
	if sessionID == "" {
		return "", fmt.Errorf("inbox: empty sessionID")
	}
	dir := filepath.Join(base, sessionID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("inbox: mkdir %s: %w", dir, err)
	}
	return dir, nil
}

// extractAttachments parses a Feishu message envelope into its
// text, attachment components, and (for rich-text messages) an
// ordered block slice that preserves paragraph-internal node order.
//
// msgType is the Feishu msg_type discriminator ("text", "image",
// "file", "audio", "media", "post", "sticker", "interactive", …).
// content is the raw JSON string from event.message.content.
//
// Returns:
//
//   - text: the textual portion of the message, joined across all
//     paragraphs. Empty for msg_types that carry no text (image,
//     file, audio, media, sticker) AND for post rich-text messages
//     where text nodes are folded into Blocks (the post path uses
//     Blocks as source-of-truth). The dispatcher uses msg.Text only
//     on the legacy single-resource path (msg.Blocks == nil).
//   - atts: one channel.Attachment per non-text resource. Empty
//     for text-only and sticker messages. Used as the download
//     candidate list (Feishu SDK file_key / image_key goes in
//     FileKey; LocalPath is empty here, filled by DownloadAttachments).
//   - blocks: ordered []agent.ContentBlock for post rich-text
//     messages ONLY. Each Feishu paragraph node becomes one block
//     in source order — `tag:"text"` and `tag:"a"` map to
//     ContentText, `tag:"img"` maps to ContentImage with FileKey
//     in Path (placeholder until resolveBlocks back-fills LocalPath).
//     Non-post msg_types return nil here.
//
// The function never errors — unrecognized msg_types fall back to
// messageText(content) for the Text field, matching the legacy
// behaviour (raw JSON passed through unchanged).
func extractAttachments(msgType, content string) (text string, attachments []channel.Attachment, blocks []agent.ContentBlock) {
	if msgType == "" {
		// Backwards compat: legacy callers that do not populate
		// message_type fall through to the old text-only path.
		return messageText(content), nil, nil
	}

	switch msgType {
	case larkim.MsgTypeText:
		return messageText(content), nil, nil

	case larkim.MsgTypeImage:
		var p struct {
			ImageKey string `json:"image_key"`
		}
		if err := json.Unmarshal([]byte(content), &p); err != nil || p.ImageKey == "" {
			return "", nil, nil
		}
		return "", []channel.Attachment{{
			Type:    "image",
			FileKey: p.ImageKey,
			// No file_name for bare image messages — synthesize
			// a stable fallback at download time.
		}}, nil

	case larkim.MsgTypeFile:
		var p struct {
			FileKey  string `json:"file_key"`
			FileName string `json:"file_name"`
		}
		if err := json.Unmarshal([]byte(content), &p); err != nil || p.FileKey == "" {
			return "", nil, nil
		}
		return "", []channel.Attachment{{
			Type:     "file",
			FileKey:  p.FileKey,
			FileName: p.FileName,
		}}, nil

	case larkim.MsgTypeAudio:
		var p struct {
			FileKey  string `json:"file_key"`
			FileName string `json:"file_name"`
		}
		if err := json.Unmarshal([]byte(content), &p); err != nil || p.FileKey == "" {
			return "", nil, nil
		}
		name := p.FileName
		if name == "" {
			name = p.FileKey + ".m4a"
		}
		return "", []channel.Attachment{{
			Type:     "audio",
			FileKey:  p.FileKey,
			FileName: name,
		}}, nil

	case larkim.MsgTypeMedia:
		// Video: file_key for the video + image_key for the cover.
		// We forward ONLY the video — the cover (image_key) is a
		// thumbnail Feishu picks; sending it as a separate "image"
		// attachment misrepresents the user's payload to the agent
		// ("look at this image" when they actually sent a video).
		var p struct {
			FileKey  string `json:"file_key"`
			ImageKey string `json:"image_key"`
			FileName string `json:"file_name"`
		}
		if err := json.Unmarshal([]byte(content), &p); err != nil {
			return "", nil, nil
		}
		var atts []channel.Attachment
		if p.FileKey != "" {
			atts = append(atts, channel.Attachment{
				Type:     "media",
				FileKey:  p.FileKey,
				FileName: p.FileName,
			})
		}
		return "", atts, nil

	case larkim.MsgTypePost:
		// Rich text: {"title":..., "content":[[{tag:..., ...}, ...]]}.
		// v1.4b: paragraph-internal ordering is preserved end-to-end.
		// We walk paragraphs in source order; within each paragraph,
		// we walk nodes in source order, emitting one ContentBlock
		// per node (text → ContentText, img → ContentImage with
		// FileKey placeholder). attachements[] collects image/file
		// resources for DownloadAttachments to resolve later.
		// Paragraphs are separated by ContentText("\n") so a multi-
		// paragraph message renders as a multi-line text with image
		// blocks interleaved at their original positions.
		var p struct {
			Title   string             `json:"title"`
			Content [][]map[string]any `json:"content"`
		}
		if err := json.Unmarshal([]byte(content), &p); err != nil {
			return messageText(content), nil, nil
		}
		var atts []channel.Attachment
		blocks = make([]agent.ContentBlock, 0)
		emitText := func(s string) {
			if s == "" {
				return
			}
			blocks = append(blocks, agent.ContentBlock{
				Type: agent.ContentText,
				Text: s,
			})
		}
		for paraIdx, para := range p.Content {
			if paraIdx > 0 {
				// Paragraph separator — single newline. The user
				// pressed Enter between paragraphs in Feishu; that
				// visual break should survive into the agent turn.
				emitText("\n")
			}
			for _, node := range para {
				tag, _ := node["tag"].(string)
				switch tag {
				case "text", "a":
					// tag:"a" carries an inline hyperlink whose
					// visible text is the user-typed label. We
					// include it so the agent sees what the user
					// actually wrote ("see [link]"), not just
					// "see ".
					if t, _ := node["text"].(string); t != "" {
						emitText(t)
					}
				case "img":
					k, _ := node["image_key"].(string)
					if k == "" {
						continue
					}
					atts = append(atts, channel.Attachment{Type: "image", FileKey: k})
					blocks = append(blocks, agent.ContentBlock{
						Type: agent.ContentImage,
						Path: k, // placeholder; resolveBlocks back-fills LocalPath
					})
				case "media":
					// tag:"media" (inline video) — out of scope for
					// v1.4 (F-14). The video key is intentionally
					// dropped to avoid misrepresenting the user's
					// payload. A future PR will extend resolveBlocks
					// and ContentFile handling.
				case "at", "emotion":
					// Mentions and reactions don't carry payload
					// the agent needs to act on. Silent skip.
				}
			}
		}
		// For post messages, Text is intentionally "": the source of
		// truth is Blocks, which the messageDispatcher picks up via
		// msg.Blocks != nil.
		return "", atts, blocks

	case larkim.MsgTypeSticker:
		// Feishu blocks the resource download for sticker msg_type.
		// Silent skip — no attachment, no error, no forwarding.
		return "", nil, nil

	default:
		// Unknown msg_type (interactive, share_chat, share_user, …)
		// carries no extractable file content in v0.2. Preserve
		// the raw payload in Text for forward compatibility.
		return messageText(content), nil, nil
	}
}

// DownloadResult aggregates the outcome of attempting to download a
// single inbound message's attachments. The caller uses this to
// decide whether to forward the message to the agent, drop it
// entirely (all-fail), or forward with a partial-failure notice.
type DownloadResult struct {
	// Atts has the same length as the input slice. Each entry's
	// LocalPath is populated on success; Error is populated on
	// failure. Both cannot be set simultaneously.
	Atts []channel.Attachment

	// HasAttachments is true if the caller passed a non-empty
	// atts slice (i.e. this message actually carried files).
	// Distinguishes "text-only message — no download needed" from
	// "attachments all failed — drop the message".
	HasAttachments bool

	// AllFailed is true iff HasAttachments AND every entry in Atts
	// has Error != "". The caller should NOT forward these
	// messages to the agent — instead, notify the user.
	AllFailed bool

	// FailureKeys lists the file_keys of attachments that failed.
	// Empty when no failures. Used by the user-facing notification
	// message.
	FailureKeys []string
}

// DownloadAttachments downloads every attachment in atts into the
// per-session inbox directory, with retry + exponential backoff on
// each download. Best-effort: per-attachment failures populate
// Error on the returned Attachment; the function itself never
// returns an error (it always succeeds in producing a DownloadResult).
//
// If atts is empty / nil, the result has HasAttachments=false and
// zero entries — the caller can treat this as a text-only message
// and skip the failure-handling branch entirely.
//
// sessionID is the inbox isolation key (Feishu adapter passes
// chatID; see inboxDirForSession for rationale). It is used purely
// to construct the on-disk subdirectory and is opaque to this
// function — empty values will be rejected by inboxDirForSession.
func DownloadAttachments(ctx context.Context, c *lark.Client,
	messageID string, atts []channel.Attachment, sessionID string,
) DownloadResult {
	if len(atts) == 0 {
		return DownloadResult{}
	}

	dir, err := inboxDirForSession(sessionID)
	if err != nil {
		// Directory creation failed — every attachment gets a
		// shared Error. This is rare (perm issue on ~/.nightme).
		results := make([]channel.Attachment, len(atts))
		keys := make([]string, 0, len(atts))
		for i, a := range atts {
			results[i] = a
			results[i].Error = err
			if a.FileKey != "" {
				keys = append(keys, a.FileKey)
			}
		}
		return DownloadResult{
			Atts:           results,
			HasAttachments: true,
			AllFailed:      true,
			FailureKeys:    keys,
		}
	}

	results := make([]channel.Attachment, len(atts))
	failedKeys := make([]string, 0)
	for i, a := range atts {
		results[i] = downloadOneWithRetry(ctx, c, messageID, a, dir)
		if results[i].Error != nil && results[i].FileKey != "" {
			failedKeys = append(failedKeys, results[i].FileKey)
		}
	}

	return DownloadResult{
		Atts:           results,
		HasAttachments: true,
		AllFailed:      len(failedKeys) == len(atts),
		FailureKeys:    failedKeys,
	}
}

// downloadOneWithRetry attempts up to maxDownloadAttempts downloads
// of a single attachment, sleeping between failures with
// exponential backoff (initialBackoff * backoffMultiplier^attempt).
func downloadOneWithRetry(ctx context.Context, c *lark.Client,
	messageID string, att channel.Attachment, dir string,
) channel.Attachment {
	backoff := initialBackoff
	var lastErr error
	for attempt := 1; attempt <= maxDownloadAttempts; attempt++ {
		att, lastErr = downloadOne(ctx, c, messageID, att, dir)
		if lastErr == nil {
			return att
		}
		if attempt == maxDownloadAttempts {
			break
		}
		// Sleep with ctx-aware cancellation so a daemon shutdown
		// doesn't leave us waiting on retries.
		select {
		case <-ctx.Done():
			att.Error = ctx.Err()
			return att
		case <-time.After(backoff):
		}
		backoff = nextBackoff(backoff)
	}
	att.Error = lastErr
	return att
}

// nextBackoff doubles-and-bounds the retry delay. Capped at
// maxBackoffDuration so a pathological backoff schedule cannot
// stall the inbound pipeline.
func nextBackoff(prev time.Duration) time.Duration {
	next := prev * backoffMultiplier
	if next > maxBackoffDuration {
		return maxBackoffDuration
	}
	return next
}

// downloadOne performs a single download attempt. The returned
// Attachment has LocalPath + Size set on success, or Error set on
// failure (LocalPath empty).
func downloadOne(ctx context.Context, c *lark.Client,
	messageID string, att channel.Attachment, dir string,
) (channel.Attachment, error) {
	// Feishu resource API: type=image for images, type=file for
	// everything else (file/audio/video). Sticker is rejected by
	// the API but we never reach here for sticker — extractAttachments
	// drops them upstream.
	dlType := "file"
	if att.Type == "image" {
		dlType = "image"
	}

	req := larkim.NewGetMessageResourceReqBuilder().
		MessageId(messageID).
		FileKey(att.FileKey).
		Type(dlType).
		Build()

	resp, err := c.Im.V1.MessageResource.Get(ctx, req)
	if err != nil {
		return att, fmt.Errorf("download: %w", err)
	}
	if !resp.Success() {
		return att, fmt.Errorf("download: api code=%d msg=%s", resp.Code, resp.Msg)
	}

	// Pick a target filename: prefer the channel-supplied
	// FileName, fall back to a synthesized stable name. If a file
	// with this name already exists (collision across retries —
	// shouldn't happen, but defensive), append a suffix.
	name := att.FileName
	if name == "" {
		name = fmt.Sprintf("%s_%s", att.Type, att.FileKey)
	}
	target, err := uniquePath(dir, name)
	if err != nil {
		return att, fmt.Errorf("download: pick path: %w", err)
	}

	if err := resp.WriteFile(target); err != nil {
		return att, fmt.Errorf("download: write: %w", err)
	}

	// SDK uses 0666 — harden to 0600. The inbox directory itself
	// is 0700, so this is belt-and-suspenders.
	_ = os.Chmod(target, 0o600)

	info, statErr := os.Stat(target)
	if statErr == nil {
		att.Size = info.Size()
	}
	// Reject 0-byte responses: a network blip or truncated fetch
	// can leave a successful-looking but empty file. Treat as a
	// download failure so the dispatcher surfaces it (matches the
	// user's mental model: "I sent a 200 KB file, why is it empty?").
	if statErr == nil && att.Size == 0 {
		_ = os.Remove(target)
		return att, fmt.Errorf("download: empty response body")
	}
	att.LocalPath = target
	return att, nil
}

// uniquePath returns a path inside dir for name. If dir/name
// already exists, append "_2", "_3", … until a free slot is found.
// This handles the (rare) case where two attachments in the same
// message share a filename.
func uniquePath(dir, name string) (string, error) {
	candidate := filepath.Join(dir, name)
	if _, err := os.Stat(candidate); os.IsNotExist(err) {
		return candidate, nil
	} else if err != nil {
		return "", err
	}
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	for i := 2; i < 1000; i++ {
		candidate = filepath.Join(dir, fmt.Sprintf("%s_%d%s", base, i, ext))
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return candidate, nil
		} else if err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("could not find free filename for %s in %s", name, dir)
}

// stripAny extracts a string from an any-typed map value (the
// result of json.Unmarshal into map[string]any). Returns "" if
// the value is missing or not a string. Used to extract text from
// post-message nodes without panicking on malformed input.
func stripAny(v any) string {
	if v == nil {
		return ""
	}
	s, _ := v.(string)
	return s
}

// imageMediaType returns the MIME type for an image based on the
// Feishu content hint + the file extension. Feishu doesn't tell us
// the format directly — image messages just carry an image_key.
// We infer from the first 4 bytes of the file (PNG / JPEG / GIF /
// WEBP magic numbers); anything else falls back to "image/png".
//
// For richer MIME inference we could sniff the file with
// http.DetectContentType; for v0.2 the magic-number check covers
// the formats Claude Code's Anthropic API actually accepts.
func imageMediaType(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return "image/png"
	}
	defer f.Close()
	hdr := make([]byte, 12)
	if _, err := f.Read(hdr); err != nil {
		return "image/png"
	}
	switch {
	case len(hdr) >= 3 && hdr[0] == 0xFF && hdr[1] == 0xD8 && hdr[2] == 0xFF:
		return "image/jpeg"
	case len(hdr) >= 8 && string(hdr[:8]) == "\x89PNG\r\n\x1a\n":
		return "image/png"
	case len(hdr) >= 6 && (string(hdr[:6]) == "GIF87a" || string(hdr[:6]) == "GIF89a"):
		return "image/gif"
	case len(hdr) >= 12 && string(hdr[:4]) == "RIFF" && string(hdr[8:12]) == "WEBP":
		return "image/webp"
	default:
		return "image/png"
	}
}

// BuildBlocks converts a channel.Message into the structured
// agent.ContentBlock slice that the agent.AgentSession.SendBlocks
// contract expects. Used by cmd/nightme/run.go to translate the
// downloaded attachments + caption text into one ordered turn.
//
// Behaviour:
//
//   - The text (if any) becomes a single ContentText block. It is
//     emitted FIRST so the agent reads the caption before the
//     attachments.
//   - Each attachment with a populated LocalPath becomes a
//     ContentImage (Type=="image") or ContentFile (everything
//     else) block. The MediaType is inferred for images via the
//     file magic; non-image attachments leave MediaType empty.
//   - Attachments with an empty LocalPath (download failed) are
//     silently skipped — the dispatcher already sent a user-facing
//     notification via the responder before reaching here.
//   - Sticker attachments never reach this function (extractAttachments
//     drops them upstream).
//
// Returns nil iff text is empty AND no attachments succeeded —
// matching BuildForwardedText's previous contract for callers that
// probe for "nothing to send".
func BuildBlocks(text string, atts []channel.Attachment) []agent.ContentBlock {
	var blocks []agent.ContentBlock
	skippedEmptyPath := 0
	if text != "" {
		blocks = append(blocks, agent.ContentBlock{
			Type: agent.ContentText,
			Text: text,
		})
	}
	for _, a := range atts {
		if a.LocalPath == "" {
			// F-14 silent-skip diagnostic: surface upstream-vs-bridge
			// split. If skippedEmptyPath > 0 then either DownloadAttachments
			// was never called (handleMessage bug) or it failed for all
			// entries (AllFailed branch). Caller is expected to have
			// produced a user-facing notification in the failure case.
			skippedEmptyPath++
			slog.Default().Debug("feishu: attachment skipped (empty LocalPath)",
				"type", a.Type, "file_key", a.FileKey, "error", a.Error)
			continue
		}
		switch a.Type {
		case "image":
			blocks = append(blocks, agent.ContentBlock{
				Type:      agent.ContentImage,
				Path:      a.LocalPath,
				MediaType: imageMediaType(a.LocalPath),
			})
		default:
			// file / audio / media — emit as ContentFile. The
			// Claude Code bridge decides whether to inline
			// (PDF) or fall back to a text annotation (other
			// MIME types not yet supported inline by Anthropic).
			blocks = append(blocks, agent.ContentBlock{
				Type: agent.ContentFile,
				Path: a.LocalPath,
			})
		}
	}
	slog.Default().Debug("feishu: BuildBlocks",
		"text_len", len(text),
		"attachments_in", len(atts),
		"blocks_out", len(blocks),
		"skipped_empty_path", skippedEmptyPath,
	)
	return blocks
}

// BuildForwardedText is the legacy string-format helper retained
// for callers that haven't migrated to the blocks API yet. It
// produces the same "attachment (<type>): <abs path>" lines that
// the v0.2 channel used before ContentBlock was added.
//
// New code should call BuildBlocks and feed the result into
// session.SendBlocks / session.QueueUserMessage. This wrapper is
// kept for tests + the v1.1 legacy PTY bridge path that still
// wants a pre-formatted string. The v1.2 daemon uses
// BuildBlocks + QueueUserMessage instead.
func BuildForwardedText(text string, atts []channel.Attachment) string {
	var b strings.Builder
	if text != "" {
		b.WriteString(text)
		b.WriteString("\n")
	}
	for _, a := range atts {
		if a.LocalPath == "" {
			continue
		}
		fmt.Fprintf(&b, "attachment (%s): %s\n", a.Type, a.LocalPath)
	}
	return strings.TrimRight(b.String(), "\n")
}

// BuildForwardedTextFromBlocks is the inverse of BuildBlocks: takes
// a slice of ContentBlocks and produces the same flat string
// representation BuildForwardedText emits for raw attachments.
//
// Used by callers (notably the F-25 Renderer OnUserMessage hook in
// session.Manager) that still operate on the string shape. The
// conversion is lossy — image blocks become "@<path>" annotations,
// text blocks become their Text verbatim, file blocks become
// "@<path>" the same as images.
func BuildForwardedTextFromBlocks(blocks []agent.ContentBlock) string {
	var b strings.Builder
	for _, blk := range blocks {
		switch blk.Type {
		case agent.ContentText:
			if blk.Text == "" {
				continue
			}
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(blk.Text)
		case agent.ContentImage, agent.ContentFile:
			if blk.Path == "" {
				continue
			}
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			fmt.Fprintf(&b, "@%s", blk.Path)
		default:
			continue
		}
	}
	return b.String()
}

// resolveBlocks back-fills LocalPath into image / file blocks whose
// Path currently holds a Feishu file_key placeholder (set by
// extractAttachments for post rich-text messages). The input blocks
// were produced in source order; resolveBlocks preserves that order
// and only mutates Path on blocks whose placeholder file_key matches
// a successful download.
//
// Behaviour:
//
//   - Image blocks whose FileKey placeholder matches a successful
//     download get LocalPath filled in (Path = localPath). On
//     download failure, the block is omitted (the surrounding text
//     context still flows through — see F-14 §5).
//   - Text blocks are preserved verbatim.
//   - Returns a fresh slice; does not mutate the input.
//
// ats must be the same slice that extractAttachments produced (so
// file_keys align). Caller is responsible for producing the
// user-facing notification when ats has any failures; this function
// silently drops failed-image blocks without notice.
func resolveBlocks(blocks []agent.ContentBlock, ats []channel.Attachment) []agent.ContentBlock {
	// Build a file_key → LocalPath index from ats. Failed entries
	// have LocalPath == "" and are intentionally absent from the
	// index so the corresponding image block is dropped below.
	keyToPath := make(map[string]string, len(ats))
	for _, a := range ats {
		if a.FileKey == "" || a.LocalPath == "" {
			continue
		}
		keyToPath[a.FileKey] = a.LocalPath
	}
	out := make([]agent.ContentBlock, 0, len(blocks))
	for _, blk := range blocks {
		switch blk.Type {
		case agent.ContentText:
			out = append(out, blk)
		case agent.ContentImage, agent.ContentFile:
			if path, ok := keyToPath[blk.Path]; ok {
				blk.Path = path
				if blk.Type == agent.ContentImage {
					blk.MediaType = imageMediaType(path)
				}
				out = append(out, blk)
			}
			// else: failed download; drop. Text context preserved.
		default:
			out = append(out, blk)
		}
	}
	return out
}
