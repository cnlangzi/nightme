package telegram

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/internal/stt"
)

// VoiceOutcome is what the voice handler hands back to
// handleMessage. Text carries the transcript (empty if the
// recognizer returned silence or the call failed); Drop tells
// the caller whether the original voice attachment should be
// stripped from inbound.Attachments so the agent sees only the
// transcript text; Err is non-nil only when the failure is one
// the chat itself should know about (worker not installed,
// model missing, etc.).
//
// The three-field shape lets handleMessage distinguish
// "transcribed fine" (Drop=true, Err=nil) from "transcribed but
// empty" (Drop=true, Text="", Err=nil) from "user-facing failure"
// (Drop=false, Err non-nil) without overloading a single return.
type VoiceOutcome struct {
	Text string
	Drop bool
	Err  error
}

// VoiceHandler bridges the Telegram adapter to the local
// nightme-stt worker. The adapter holds one of these after
// SetVoiceHandler; handleMessage invokes HandleVoice whenever a
// message carries a Voice attachment. The handler is the seam
// between the chat-platform's "voice bytes" world and the
// runtime's "text prompt" world — see docs/channel/telegram.md
// §21.
type VoiceHandler struct {
	manager stt.ProcessManager
	logger  *slog.Logger
	// timeout caps a single Transcribe call so a misbehaving
	// worker cannot stall the inbound handler. Zero = no
	// extra timeout beyond ctx.
	timeout time.Duration
}

// NewVoiceHandler wires a VoiceHandler. manager is required;
// logger is optional and falls back to slog.Default().
func NewVoiceHandler(manager stt.ProcessManager, logger *slog.Logger) *VoiceHandler {
	if logger == nil {
		logger = slog.Default()
	}
	return &VoiceHandler{
		manager: manager,
		logger:  logger,
		timeout: 60 * time.Second,
	}
}

// SetTimeout overrides the default 60s transcription budget.
// Mostly useful for tests; production callers leave it alone.
func (h *VoiceHandler) SetTimeout(d time.Duration) {
	h.timeout = d
}

// HandleVoice reads voiceFile (already downloaded to disk by
// downloadAttachments), pipes it through the nightme-stt
// worker, and returns the transcript.
//
// The voice file is expected to be opus-in-ogg (the format
// Telegram delivers Voice messages in). The worker is
// responsible for decoding via ffmpeg; this layer does not
// need to know the codec.
//
// On success (regardless of whether the recognizer returned
// empty text — silent audio is a valid outcome) the outcome
// has Drop=true. The caller is expected to:
//   - replace inbound.Text with the transcript
//   - strip the matching voice attachment from
//     inbound.Attachments so the agent doesn't see raw bytes
//
// On failure the outcome has Drop=false (the original voice
// attachment is left in place — the user can retry or the
// agent can still try to handle the raw audio if it can) and
// Err set. handleMessage surfaces Err as a separate
// OutError-kind message so the user is told what went wrong.
func (h *VoiceHandler) HandleVoice(ctx context.Context, msg *Message, voiceFile string) VoiceOutcome {
	if h == nil || h.manager == nil {
		return VoiceOutcome{Err: errors.New("voice handler not configured")}
	}
	if voiceFile == "" {
		return VoiceOutcome{Err: errors.New("empty voice file path")}
	}
	data, err := os.ReadFile(voiceFile)
	if err != nil {
		return VoiceOutcome{Err: fmt.Errorf("read voice file: %w", err)}
	}
	if len(data) == 0 {
		return VoiceOutcome{Err: errors.New("voice file is empty")}
	}

	transcribeCtx := ctx
	if h.timeout > 0 {
		var cancel context.CancelFunc
		transcribeCtx, cancel = context.WithTimeout(ctx, h.timeout)
		defer cancel()
	}

	transcriber, err := h.manager.EnsureReady(transcribeCtx)
	if err != nil {
		if stt.IsNotBuilt(err) {
			return VoiceOutcome{Err: &VoiceInstallNeededError{Inner: err}}
		}
		return VoiceOutcome{Err: fmt.Errorf("start stt worker: %w", err)}
	}

	// Format hint: Telegram Voice is always opus-in-ogg. Some
	// clients send wav; the worker is told what it received so
	// ffmpeg can pick the right demuxer.
	format := detectAudioFormat(msg, data)
	res, err := transcriber.Transcribe(transcribeCtx, data, format)
	if err != nil {
		return VoiceOutcome{Err: fmt.Errorf("transcribe: %w", err)}
	}

	transcript := strings.TrimSpace(res.Text)
	if transcript == "" {
		// Empty transcript: still consume the voice attachment
		// so the agent gets a clean "you said nothing" prompt
		// rather than raw audio bytes it can't consume.
		return VoiceOutcome{Text: "", Drop: true}
	}
	return VoiceOutcome{Text: transcript, Drop: true}
}

// detectAudioFormat picks the protocol-level format hint to
// send to the worker. Telegram's Voice messages are
// documented as opus-in-ogg, so the default is "ogg" unless
// the MIME type explicitly says otherwise. MimeType is
// optional on Voice — Telegram clients may omit it.
func detectAudioFormat(msg *Message, data []byte) string {
	if msg != nil && msg.Voice != nil {
		mime := strings.ToLower(msg.Voice.MimeType)
		switch {
		case strings.HasPrefix(mime, "audio/wav"), strings.HasPrefix(mime, "audio/x-wav"):
			return "wav"
		case strings.HasPrefix(mime, "audio/ogg"), strings.HasPrefix(mime, "audio/opus"):
			return "ogg"
		}
	}
	// Sniff the OGG magic bytes ("OggS") as a fallback when
	// the MIME hint is missing or wrong. WAV starts with
	// "RIFF", but most Telegram voice notes are opus-in-ogg
	// so we test OGG first.
	if len(data) >= 4 && string(data[:4]) == "OggS" {
		return "ogg"
	}
	if len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WAVE" {
		return "wav"
	}
	return "ogg"
}

// VoiceInstallNeededError is the typed error returned when the
// user hasn't run `nightme stt install` yet. The chat
// adapter surfaces this as a one-line actionable message
// rather than a stack trace — see handleMessage.
type VoiceInstallNeededError struct {
	Inner error
}

func (e *VoiceInstallNeededError) Error() string {
	return "voice transcription requires installing nightme-stt: " + e.Inner.Error()
}

func (e *VoiceInstallNeededError) Unwrap() error { return e.Inner }

// IsVoiceInstallNeeded reports whether err is the typed
// "binary not installed" signal. Exported so handleMessage
// can branch without importing stt's internal sentinel.
func IsVoiceInstallNeeded(err error) bool {
	var v *VoiceInstallNeededError
	return errors.As(err, &v)
}

// voiceFailureText returns the user-facing message for a
// Voice handler failure. Kept here so the wording is owned by
// this package; callers pass the result straight to
// messages.OutboundMessage.Text.
func voiceFailureText(outcome VoiceOutcome) string {
	if outcome.Err == nil {
		return ""
	}
	if IsVoiceInstallNeeded(outcome.Err) {
		return "🎙 Voice messages need `nightme stt install` first."
	}
	if stt.IsCode(outcome.Err, stt.CodeTimeout) {
		return "🎙 Voice transcription timed out — please try a shorter clip."
	}
	if stt.IsCode(outcome.Err, stt.CodeUnsupportedFormat) {
		return "🎙 Voice audio format not supported — please send an ogg or wav note."
	}
	return fmt.Sprintf("🎙 Voice transcription failed: %v", outcome.Err)
}

// EnsureOutcomeMessageKind is a tiny helper the adapter uses
// to ensure the chat-level message we send is OutError — the
// one kind that always surfaces even in silent-drop chains
// (per docs/channel/telegram.md §11.11). Kept here so the
// voice-handler package owns its messaging policy.
func EnsureOutcomeMessageKind(kind messages.OutboundKind, err error) messages.OutboundKind {
	if err == nil {
		return kind
	}
	return messages.OutError
}

// findVoiceAttachment returns the LocalPath of the first
// attachment in atts whose name matches the voice file
// collector's convention ("voice.ogg"). The collector names
// Voice attachments "voice.ogg" — see collectAttachmentSources.
// Returns "" when no match is found.
func findVoiceAttachment(atts []messages.Attachment) string {
	for _, att := range atts {
		if att.Name == "voice.ogg" || strings.HasSuffix(att.LocalPath, "voice.ogg") {
			return att.LocalPath
		}
	}
	return ""
}

// dropVoiceAttachment returns atts minus any entry that
// matches the voice-attachment convention. Called from
// handleMessage after the voice handler runs (or fails) so the
// agent never receives raw opus bytes.
func dropVoiceAttachment(atts []messages.Attachment) []messages.Attachment {
	out := atts[:0:0]
	for _, att := range atts {
		if att.Name == "voice.ogg" || strings.HasSuffix(att.LocalPath, "voice.ogg") {
			continue
		}
		out = append(out, att)
	}
	return out
}
