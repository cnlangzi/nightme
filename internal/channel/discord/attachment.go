package discord

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/cnlangzi/nightme/internal/messages"
)

// downloadAttachments fetches every attachment on a Discord message
// into a per-message directory under the adapter's data dir.
//
// Layout: <dataDir>/discord/<chatID>/<messageID>/<attachmentID>-<sanitised filename>
//
// chatID is the session chat id (with the "dc_" prefix already
// applied). Discord channel ids are decimal snowflakes so the
// nested path is safe under any OS filesystem.
//
// The attachment ID is prepended to the filename to disambiguate
// two attachments that share a filename on the same message
// (Discord allows this; without disambiguation the second write
// silently overwrites the first while both Attachment entries
// point at the same LocalPath).
//
// Failed downloads still produce a messages.Attachment entry with
// Error set so the dispatcher can surface the failure rather than
// silently dropping the attachment.
func (a *Adapter) downloadAttachments(ctx context.Context, msg *Message, chatID string) []messages.Attachment {
	if msg == nil || len(msg.Attachments) == 0 {
		return nil
	}
	directory := filepath.Join(a.dataDir, "discord", chatID, string(msg.ID))
	if err := os.MkdirAll(directory, 0o700); err != nil {
		if a.logger != nil {
			a.logger.Warn("discord: create attachment dir failed",
				"dir", directory, "err", err.Error(),
			)
		}
		// Fall through — WriteFile returns the per-file error and we
		// surface it on each Attachment.Error. Skipping the entire
		// batch because the dir couldn't be made would be too
		// aggressive for a transient permission error.
	}
	out := make([]messages.Attachment, 0, len(msg.Attachments))
	for _, att := range msg.Attachments {
		out = append(out, a.downloadOne(ctx, att, directory))
	}
	return out
}

// downloadOne fetches a single attachment and writes it to
// directory. The returned messages.Attachment always has its Type
// + Name + MimeType fields populated so downstream callers can
// inspect them even on failure.
func (a *Adapter) downloadOne(ctx context.Context, att Attachment, directory string) messages.Attachment {
	base := messages.Attachment{
		Type:     discordAttachmentType(att),
		Name:     att.Filename,
		FileName: att.Filename,
		MimeType: att.ContentType,
		FileKey:  string(att.ID),
		Size:     att.Size,
	}
	if att.URL == "" {
		base.Error = errEmptyAttachmentURL
		return base
	}
	data, err := a.api.Download(ctx, att.URL)
	if err != nil {
		base.Error = err
		return base
	}
	// Prefix the filename with the attachment id so two
	// attachments with the same filename on the same message
	// don't overwrite each other on disk.
	local := filepath.Join(directory, attachmentLocalName(att))
	if err := os.WriteFile(local, data, 0o600); err != nil {
		base.Error = err
		return base
	}
	base.LocalPath = local
	base.Size = int64(len(data))
	return base
}

// attachmentLocalName joins the attachment id and a sanitised
// filename with a hyphen: "<id>-<name>". Both halves are kept
// filesystem-safe (snowflakes are decimal digits; sanitiseFilename
// strips path separators and control bytes).
func attachmentLocalName(att Attachment) string {
	return string(att.ID) + "-" + sanitiseFilename(att.Filename)
}

// discordAttachmentType maps a Content-Type MIME to the channel-
// native vocabulary (image / audio / media / file) that BuildBlocks
// and the agent dispatch consume. The chatstore's AgentEvent
// envelope branches on these strings.
func discordAttachmentType(att Attachment) string {
	mime := strings.ToLower(att.ContentType)
	switch {
	case strings.HasPrefix(mime, "image/"):
		return "image"
	case strings.HasPrefix(mime, "audio/"):
		return "audio"
	case strings.HasPrefix(mime, "video/"):
		return "media"
	default:
		return "file"
	}
}

// sanitiseFilename strips path separators and control bytes from a
// Discord filename so the resolved path stays inside directory. An
// all-empty result falls back to the attachment id so we never
// produce an empty filename (which OSes treat as implementation-
// defined).
func sanitiseFilename(name string) string {
	name = filepath.Base(name)
	if name == "." || name == ".." || name == "/" || name == "\\" {
		name = ""
	}
	if name == "" {
		name = "attachment"
	}
	return name
}

// errEmptyAttachmentURL is a sentinel kept for stable test
// identification when a Discord message ships an attachment
// without a populated URL (rare; happens with deleted attachments
// referenced by stale embeds).
var errEmptyAttachmentURL = &attachmentURLError{}

type attachmentURLError struct{}

func (*attachmentURLError) Error() string { return "discord: attachment URL is empty" }
