package telegram

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/internal/stt"
)

// fakeManager is a ProcessManager stub that returns canned
// results without spawning a worker. Lets the voice-handler
// tests exercise the integration without ffmpeg / sherpa.
type fakeManager struct {
	transcriber *fakeTranscriber
	err         error
}

func (f *fakeManager) EnsureReady(ctx context.Context) (stt.Transcriber, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.transcriber, nil
}

func (f *fakeManager) Stop(ctx context.Context) error        { return nil }
func (f *fakeManager) Status(ctx context.Context) stt.Status { return stt.Status{Running: true} }

type fakeTranscriber struct {
	text string
	err  error
}

func (t *fakeTranscriber) Health(ctx context.Context) error { return nil }
func (t *fakeTranscriber) Version(ctx context.Context) (int, error) {
	return stt.ProtocolVersion, nil
}
func (t *fakeTranscriber) WorkerStatus(ctx context.Context) (stt.WorkerStatus, error) {
	return stt.WorkerStatus{
		PID:       12345,
		StartedAt: time.Now(),
		Endpoint:  "test-sock",
		Version:   stt.ProtocolVersion,
		BuildVer:  "test",
	}, nil
}
func (t *fakeTranscriber) Transcribe(ctx context.Context, audio []byte, format string) (stt.TranscribeResult, error) {
	if t.err != nil {
		return stt.TranscribeResult{}, t.err
	}
	return stt.TranscribeResult{Text: t.text, Language: "en"}, nil
}
func (t *fakeTranscriber) Close() error { return nil }

// writeOggFile writes a 32-byte ogg-shaped buffer to a temp
// path. We don't need real opus — the handler's only job is to
// hand the bytes to the worker.
func writeOggFile(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "voice.ogg")
	if err := os.WriteFile(path, []byte("OggSOggSOggSOggSOggSOggSOggSOggS"), 0o600); err != nil {
		t.Fatalf("write voice file: %v", err)
	}
	return path
}

func TestVoiceHandler_TranscribeSucceeds(t *testing.T) {
	voiceFile := writeOggFile(t)
	manager := &fakeManager{transcriber: &fakeTranscriber{text: "hello world"}}
	h := NewVoiceHandler(manager, nil)

	msg := &Message{
		Voice: &Document{FileID: "abc", MimeType: "audio/ogg"},
	}
	outcome := h.HandleVoice(context.Background(), msg, voiceFile)
	if outcome.Err != nil {
		t.Fatalf("HandleVoice err: %v", outcome.Err)
	}
	if !outcome.Drop {
		t.Fatalf("HandleVoice: want Drop=true on success")
	}
	if outcome.Text != "hello world" {
		t.Fatalf("HandleVoice text=%q, want hello world", outcome.Text)
	}
}

func TestVoiceHandler_EmptyPath(t *testing.T) {
	manager := &fakeManager{transcriber: &fakeTranscriber{}}
	h := NewVoiceHandler(manager, nil)

	outcome := h.HandleVoice(context.Background(), &Message{}, "")
	if outcome.Err == nil {
		t.Fatalf("HandleVoice: want error on empty path")
	}
	if outcome.Drop {
		t.Fatalf("HandleVoice: Drop should be false on validation failure")
	}
}

func TestVoiceHandler_SpawnErrorSurfaces(t *testing.T) {
	voiceFile := writeOggFile(t)
	// errNotBuilt is unexported, so we can't construct the
	// install-needed signal from outside the stt package. The
	// InstallNeeded path is exercised end-to-end in the
	// production wiring (ProductionSpawner wraps it); here
	// we just verify that a non-nil spawn error propagates
	// and produces a non-empty user-facing message.
	manager := &fakeManager{
		err: stt.NewError("internal_error", "fake spawn failure"),
	}
	h := NewVoiceHandler(manager, nil)
	msg := &Message{Voice: &Document{FileID: "x", MimeType: "audio/ogg"}}

	outcome := h.HandleVoice(context.Background(), msg, voiceFile)
	if outcome.Err == nil {
		t.Fatalf("HandleVoice: want err on spawn failure")
	}
	if voiceFailureText(outcome) == "" {
		t.Fatalf("voiceFailureText: want non-empty for spawn error")
	}
}

func TestVoiceHandler_UnsupportedFormatError(t *testing.T) {
	voiceFile := writeOggFile(t)
	manager := &fakeManager{
		err: stt.NewError(stt.CodeUnsupportedFormat, "format \"flac\" not supported"),
	}
	h := NewVoiceHandler(manager, nil)
	msg := &Message{Voice: &Document{FileID: "x", MimeType: "audio/ogg"}}

	outcome := h.HandleVoice(context.Background(), msg, voiceFile)
	if outcome.Err == nil {
		t.Fatalf("HandleVoice: want err")
	}
	if stt.IsCode(outcome.Err, stt.CodeUnsupportedFormat) == false {
		// stt.IsCode checks the wire code; the manager
		// wraps via NewError which already uses the
		// code, so this should match.
		_ = errors.Is
		t.Fatalf("HandleVoice: want CodeUnsupportedFormat, got %v", outcome.Err)
	}
}

func TestVoiceHandler_NoManagerWired(t *testing.T) {
	h := NewVoiceHandler(nil, nil)
	outcome := h.HandleVoice(context.Background(), &Message{}, "/dev/null")
	if outcome.Err == nil {
		t.Fatalf("HandleVoice: want err on nil manager")
	}
}

func TestFindVoiceAttachment(t *testing.T) {
	atts := []messages.Attachment{
		{Name: "image.jpg", LocalPath: "/tmp/image.jpg"},
		{Name: "voice.ogg", LocalPath: "/tmp/voice.ogg"},
		{Name: "doc.pdf", LocalPath: "/tmp/doc.pdf"},
	}
	path := findVoiceAttachment(atts)
	if path != "/tmp/voice.ogg" {
		t.Fatalf("findVoiceAttachment path=%q, want /tmp/voice.ogg", path)
	}
}

func TestDropVoiceAttachment(t *testing.T) {
	atts := []messages.Attachment{
		{Name: "image.jpg", LocalPath: "/tmp/image.jpg"},
		{Name: "voice.ogg", LocalPath: "/tmp/voice.ogg"},
		{Name: "doc.pdf", LocalPath: "/tmp/doc.pdf"},
	}
	out := dropVoiceAttachment(atts)
	if len(out) != 2 {
		t.Fatalf("dropVoiceAttachment len=%d, want 2", len(out))
	}
	for _, a := range out {
		if a.Name == "voice.ogg" {
			t.Fatalf("dropVoiceAttachment: voice still in output")
		}
	}
}
