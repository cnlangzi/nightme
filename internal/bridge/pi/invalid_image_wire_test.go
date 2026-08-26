// Regression tests for issue #290 ("invalid image base64 content"
// session-broken-forever repro).
//
// Two bugs were conflated in the report:
//
//  1. stripDataURLPrefix (agent.go) silently never stripped the
//     data:<mime>;base64, prefix — HasSuffix was matched against
//     dataURL[:comma], which ends at the <mime> boundary, not at the
//     ;base64, token. Result: pi was being handed the full data URL
//     as the `data` field of a prompt image attachment, and the
//     upstream provider rejected it with "invalid image base64 content".
//
//  2. pi's assistant message_end.errorMessage field was not decoded by
//     the bridge at all (the assistantMessage struct had no field for
//     it). Even when pi faithfully surfaced the upstream error, the
//     user only saw an empty card with Subtype "error" — no way to
//     know what went wrong.
//
// These tests pin both bugs down at the unit level.
package pi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// ─── Bug A: stripDataURLPrefix ─────────────────────────────────────────

// TestStripDataURLPrefix_HappyPath covers the well-formed data URL
// encodeImage actually emits. Before the fix the function returned the
// input unchanged; this is the regression lock.
func TestStripDataURLPrefix_HappyPath(t *testing.T) {
	payload := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10}
	encoded := base64.StdEncoding.EncodeToString(payload)
	got := stripDataURLPrefix("data:image/jpeg;base64," + encoded)
	if got != encoded {
		t.Fatalf("stripDataURLPrefix = %q, want %q", got, encoded)
	}
	// Round-trip through base64 decoder to prove the prefix really
	// is gone (the bug shipped the full data URL which is not valid
	// base64 — that's what triggered the provider's "invalid base64"
	// validator).
	decoded, err := base64.StdEncoding.DecodeString(got)
	if err != nil {
		t.Fatalf("stripped result is not valid base64: %v\nvalue: %q", err, got)
	}
	if string(decoded) != string(payload) {
		t.Errorf("decoded bytes %x != original %x", decoded, payload)
	}
}

// TestStripDataURLPrefix_OtherMimes asserts the fix is not jpeg-
// specific — any `image/<sub>;base64,<payload>` shape must strip the
// full prefix. Mime boundary characters (slash, semicolon) must not
// confuse the splitter.
func TestStripDataURLPrefix_OtherMimes(t *testing.T) {
	cases := []struct {
		mime    string
		payload []byte
	}{
		{"image/png", []byte{0x89, 0x50, 0x4E, 0x47}},
		{"image/gif", []byte{0x47, 0x49, 0x46, 0x38}},
		{"image/webp", []byte{0x52, 0x49, 0x46, 0x46, 0x57, 0x45, 0x42, 0x50}},
	}
	for _, tc := range cases {
		encoded := base64.StdEncoding.EncodeToString(tc.payload)
		in := "data:" + tc.mime + ";base64," + encoded
		got := stripDataURLPrefix(in)
		if got != encoded {
			t.Errorf("stripDataURLPrefix(%q) = %q, want %q", in, got, encoded)
		}
	}
}

// TestStripDataURLPrefix_NoSepPassthrough guards the safe-fallback
// contract: a string without the ";base64," separator is returned
// unchanged so callers (especially SendBlocks's image loop, which
// runs stripDataURLPrefix unconditionally on whatever encodeImage
// returned) don't accidentally corrupt already-stripped payloads.
func TestStripDataURLPrefix_NoSepPassthrough(t *testing.T) {
	already := base64.StdEncoding.EncodeToString([]byte{1, 2, 3})
	if got := stripDataURLPrefix(already); got != already {
		t.Errorf("stripped already-stripped = %q, want passthrough %q", got, already)
	}
	empty := ""
	if got := stripDataURLPrefix(empty); got != empty {
		t.Errorf("stripped empty = %q, want passthrough", got)
	}
}

// TestStripDataURLPrefix_CommaInMime documents that a malformed mime
// containing a comma would have fooled the OLD HasSuffix-based
// implementation; the new strings.Cut-based one still strips
// correctly because it anchors on ";base64," as a unit.
func TestStripDataURLPrefix_CommaInMime(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString([]byte{0xAB, 0xCD})
	in := "data:weird/mime,with,commas;base64," + encoded
	got := stripDataURLPrefix(in)
	if got != encoded {
		t.Errorf("stripDataURLPrefix(mime-with-commas) = %q, want %q", got, encoded)
	}
}

// TestEncodeImage_RoundTrip locks encodeImage's contract (read file,
// validate mime + magic bytes, data-URL-wrap) so stripDataURLPrefix's
// partner function doesn't silently drift. The fixture starts with
// the JPEG SOI + APP0 marker so matchesMagicBytes accepts it.
func TestEncodeImage_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.jpg")
	jpeg := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10}
	if err := os.WriteFile(path, jpeg, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := encodeImage(path, "image/jpeg")
	if err != nil {
		t.Fatal(err)
	}
	want := "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(jpeg)
	if got != want {
		t.Errorf("encodeImage = %q, want %q", got, want)
	}
}

// ─── Bug A: end-to-end wire shape ─────────────────────────────────────

// captureWire is an io.WriteCloser that records every Write the
// rpcClient emits, so a unit test can assert the exact wire bytes
// SendBlocks wrote.
type captureWire struct {
	mu   sync.Mutex
	rows []string
}

func (c *captureWire) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// writeLine emits the payload and the trailing "\n" in two
	// separate Write calls; ignore the bare-newline flush so we
	// only see real JSONL frames in c.rows.
	s := strings.TrimRight(string(p), "\n")
	if s == "" {
		return len(p), nil
	}
	c.rows = append(c.rows, s)
	return len(p), nil
}
func (c *captureWire) Close() error { return nil }
func (c *captureWire) Lines() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.rows))
	copy(out, c.rows)
	return out
}

// newFakeDriver wires a minimal driver to captureWire so SendBlocks's
// rpc.request writes one JSONL frame and then waits. We then
// dispatch a synthetic response so the call returns without leaking
// a goroutine.
func newFakeDriver(t *testing.T, cw *captureWire) *driver {
	t.Helper()
	prev := piDebug
	piDebug = false
	t.Cleanup(func() { piDebug = prev })
	return &driver{
		rpc:    newRPCClient(cw),
		events: make(chan agent.AgentEvent, 64),
		closed: make(chan struct{}),
	}
}

func driveResponse(d *driver, promptID string, success bool, errStr string) {
	// The rpcClient stores pending slots under the JSON-encoded form
	// of the id (with surrounding quotes for string ids), and the
	// dispatchResponse path strips them back out. Replicate that
	// here so the test reaches the right waiter.
	env := responseEnvelope{
		ID:      json.RawMessage(`"` + promptID + `"`),
		Type:    "response",
		Command: "prompt",
		Success: success,
	}
	if !success {
		env.Error = errStr
	}
	d.rpc.dispatchResponse(env)
}

// TestSendBlocksImage_WireShape_PureBase64 is the end-to-end lock for
// Bug A. Before the fix the wire bytes contained the full data URL
// (`"data":"data:image/jpeg;base64,..."`) which the upstream provider
// rejected; the fix sends only the raw base64 payload.
func TestSendBlocksImage_WireShape_PureBase64(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tiny.jpg")
	header := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10}
	if err := os.WriteFile(path, header, 0o600); err != nil {
		t.Fatal(err)
	}

	cw := &captureWire{}
	d := newFakeDriver(t, cw)

	done := make(chan error, 1)
	go func() {
		done <- d.SendBlocks(context.Background(), []agent.ContentBlock{
			{Type: agent.ContentImage, Path: path, MediaType: "image/jpeg"},
			{Type: agent.ContentText, Text: "看看这张图"},
		})
	}()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(cw.Lines()) >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	lines := cw.Lines()
	if len(lines) < 1 {
		select {
		case err := <-done:
			t.Fatalf("no wire line in 2s; SendBlocks returned %v", err)
		case <-time.After(200 * time.Millisecond):
			t.Fatalf("no wire line in 2s; SendBlocks hung")
		}
	}
	raw := lines[len(lines)-1]

	var frame struct {
		ID      string `json:"id"`
		Type    string `json:"type"`
		Message string `json:"message"`
		Images  []struct {
			Type     string `json:"type"`
			Data     string `json:"data"`
			MimeType string `json:"mimeType"`
		} `json:"images"`
	}
	if err := json.Unmarshal([]byte(raw), &frame); err != nil {
		t.Fatalf("parse frame: %v\nline: %s", err, raw)
	}
	if frame.Type != "prompt" || frame.Message != "看看这张图" || len(frame.Images) != 1 {
		t.Fatalf("unexpected frame: %+v", frame)
	}
	img := frame.Images[0]
	if img.MimeType != "image/jpeg" {
		t.Errorf("images[0].mimeType = %q, want image/jpeg", img.MimeType)
	}
	if strings.HasPrefix(img.Data, "data:") {
		// Bug A's smoking gun: the data field must be pure base64,
		// not the data URL wrapping it.
		t.Fatalf("images[0].data starts with %q — full data URL leaked through; this is the bug #290 wire shape",
			img.Data[:min(20, len(img.Data))])
	}
	decoded, err := base64.StdEncoding.DecodeString(img.Data)
	if err != nil {
		t.Fatalf("images[0].data is not valid base64: %v\nvalue: %q", err, img.Data)
	}
	if string(decoded) != string(header) {
		t.Errorf("decoded %x != on-disk %x", decoded, header)
	}

	driveResponse(d, frame.ID, true, "")
	if err := <-done; err != nil {
		t.Errorf("SendBlocks err = %v, want nil", err)
	}
}

// ─── Bug B: errorMessage on EventAgentResult.Err ──────────────────────

// TestTranslateMessageEnd_ErrorMessage_SurfacesOnResult is the
// regression lock for Bug B. Before the fix, pi's
// assistant message_end.errorMessage was unread by the bridge — the
// user saw an empty card with Subtype "error" and no clue. After the
// fix, the upstream provider's failure text lands on
// EventAgentResult.Err.
func TestTranslateMessageEnd_ErrorMessage_SurfacesOnResult(t *testing.T) {
	tr := newTestTranslator()
	// Drive a complete turn: stream a tiny text_delta + final
	// message_end (with errorMessage) + agent_settled. By F-52
	// §2.5, EventAgentResult is emitted by agent_settled, not by
	// message_end itself.
	raw1 := []byte(`{"type":"message_update","assistantMessageEvent":{"type":"text_start","contentIndex":0}}`)
	if _, err := tr.translate(raw1, nil); err != nil {
		t.Fatalf("translate text_start: %v", err)
	}
	raw2 := []byte(`{"type":"message_update","assistantMessageEvent":{"type":"text_end","contentIndex":0}}`)
	if _, err := tr.translate(raw2, nil); err != nil {
		t.Fatalf("translate text_end: %v", err)
	}
	raw3 := []byte(`{"type":"message_end","message":{"role":"assistant",` +
		`"content":[],"stopReason":"error",` +
		`"errorMessage":"400: {\"message\":\"invalid image base64 content\",\"type\":\"invalid_request_error\",\"code\":\"3\"}"}}`)
	if _, err := tr.translate(raw3, nil); err != nil {
		t.Fatalf("translate message_end: %v", err)
	}
	settled, err := tr.translate([]byte(`{"type":"agent_settled"}`), nil)
	if err != nil {
		t.Fatalf("translate settled: %v", err)
	}

	var found *agent.AgentEvent
	for i := range settled {
		if settled[i].Kind == agent.EventAgentResult {
			ev := settled[i]
			found = &ev
			break
		}
	}
	if found == nil {
		t.Fatalf("no EventAgentResult emitted; events: %+v", settled)
	}
	if found.Result == nil {
		t.Fatal("EventAgentResult missing Result payload")
	}
	if found.Result.Subtype != "error" {
		t.Errorf("Result.Subtype = %q, want \"error\"", found.Result.Subtype)
	}
	if found.Err == nil {
		t.Fatal("EventAgentResult.Err is nil — Bug B regression: provider error text was dropped")
	}
	if !strings.Contains(found.Err.Error(), "invalid image base64 content") {
		t.Errorf("EventAgentResult.Err = %q, want it to contain \"invalid image base64 content\"",
			found.Err.Error())
	}
}

// TestTranslateMessageEnd_NoErrorMessage_HappyPath asserts the happy
// path is unaffected: a settled turn with stopReason != "error" and
// no errorMessage still emits a normal EventAgentResult without
// spurious Err.
func TestTranslateMessageEnd_NoErrorMessage_HappyPath(t *testing.T) {
	tr := newTestTranslator()
	var all []agent.AgentEvent
	for _, l := range []string{
		`{"type":"message_update","assistantMessageEvent":{"type":"text_start","contentIndex":0}}`,
		`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"hi"}}`,
		`{"type":"message_update","assistantMessageEvent":{"type":"text_end","contentIndex":0}}`,
		`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"hi"}],"stopReason":"end_turn"}}`,
		`{"type":"agent_settled"}`,
	} {
		evs, err := tr.translate([]byte(l), nil)
		if err != nil {
			t.Fatalf("translate %s: %v", l, err)
		}
		all = append(all, evs...)
	}
	var result *agent.AgentEvent
	for i := range all {
		if all[i].Kind == agent.EventAgentResult {
			ev := all[i]
			result = &ev
			break
		}
	}
	if result == nil {
		t.Fatalf("no EventAgentResult delivered; events: %+v", all)
	}
	if result.Err != nil {
		t.Errorf("happy-path Err = %v, want nil", result.Err)
	}
	if result.Result == nil || result.Result.Text != "hi" {
		t.Errorf("Result = %+v, want Text=\"hi\"", result.Result)
	}
	if result.Result.Subtype != "end_turn" {
		t.Errorf("Subtype = %q, want \"end_turn\"", result.Result.Subtype)
	}
}

// ─── Round 2: encodeImage validation + auto-recovery hook ─────────────

// TestEncodeImage_RejectsUnsupportedMime locks the mime whitelist
// at the bridge boundary. Telegram hard-codes "image/jpeg" for
// every Photo (telegram/adapter.go:677) and would otherwise ship a
// declared "image/jpeg" / actual PNG payload to pi — which the
// provider then rejects with an ambiguous error. The bridge rejects
// it pre-send with a precise error.
func TestEncodeImage_RejectsUnsupportedMime(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.tiff")
	if err := os.WriteFile(path, []byte{0x49, 0x49, 0x2A, 0x00}, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := encodeImage(path, "image/tiff")
	if !errors.Is(err, ErrInvalidImageMime) {
		t.Errorf("err = %v, want ErrInvalidImageMime", err)
	}
}

// TestEncodeImage_RejectsEmptyMime documents that an empty
// ContentImage.MediaType (which the channel layer might emit for an
// attachment with no detected mime) is refused with a clear error
// instead of the previous silent "data:;base64,<payload>" data
// URL — which pi accepted on the wire but the upstream provider
// then rejected.
func TestEncodeImage_RejectsEmptyMime(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.bin")
	if err := os.WriteFile(path, []byte{0x00, 0x01, 0x02, 0x03}, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := encodeImage(path, "")
	if !errors.Is(err, ErrInvalidImageMime) {
		t.Errorf("err = %v, want ErrInvalidImageMime", err)
	}
}

// TestEncodeImage_RejectsMismatchedMagicBytes covers the case where
// the channel layer hands the bridge a ContentBlock claiming
// "image/jpeg" but the actual file on disk is something else
// (e.g. Telegram says image/jpeg for every Photo regardless of
// content). The bridge must reject pre-send rather than let the
// upstream provider reject with an ambiguous error.
func TestEncodeImage_RejectsMismatchedMagicBytes(t *testing.T) {
	dir := t.TempDir()
	// PNG magic bytes declared as image/jpeg.
	path := filepath.Join(dir, "lie.jpg")
	pngHeader := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}
	if err := os.WriteFile(path, pngHeader, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := encodeImage(path, "image/jpeg")
	if !errors.Is(err, ErrInvalidImageMime) {
		t.Errorf("err = %v, want ErrInvalidImageMime (mime declared as jpeg but file is PNG)", err)
	}
	if !strings.Contains(err.Error(), "image/jpeg") {
		t.Errorf("err = %q, want it to mention the rejected mime", err.Error())
	}
}

// TestEncodeImage_ErrImageTooLarge_HasContext locks the size-context
// requirement on ErrImageTooLarge — codex already does this; pi
// shipped just "pi: image too large" which gave the user no idea
// how much over the cap they were. Now it reads e.g.
// "pi: image too large: 12582912 > 10485760 bytes".
func TestEncodeImage_ErrImageTooLarge_HasContext(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.jpg")
	jpeg := make([]byte, maxImageBytes+1)
	jpeg[0], jpeg[1], jpeg[2] = 0xFF, 0xD8, 0xFF // valid JPEG magic
	if err := os.WriteFile(path, jpeg, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := encodeImage(path, "image/jpeg")
	if !errors.Is(err, ErrImageTooLarge) {
		t.Fatalf("err = %v, want ErrImageTooLarge", err)
	}
	if !strings.Contains(err.Error(), "10485760") {
		t.Errorf("err = %q, want it to include the cap (10485760) so the user sees how much over", err.Error())
	}
}

// TestEncodeImage_AcceptsAllSupportedMimes is the happy-path smoke
// for the whitelist + magic-byte validation: each supported mime
// round-trips cleanly with a matching header.
func TestEncodeImage_AcceptsAllSupportedMimes(t *testing.T) {
	cases := []struct {
		mime    string
		header  []byte
	}{
		{"image/jpeg", []byte{0xFF, 0xD8, 0xFF, 0xE0}},
		{"image/png", []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}},
		{"image/gif", []byte{0x47, 0x49, 0x46, 0x38, 0x39, 0x61}}, // GIF89a
		{"image/webp", append([]byte{0x52, 0x49, 0x46, 0x46, 0x00, 0x00, 0x00, 0x00}, []byte{0x57, 0x45, 0x42, 0x50}...)},
	}
	for _, tc := range cases {
		dir := t.TempDir()
		ext := map[string]string{"image/jpeg": ".jpg", "image/png": ".png", "image/gif": ".gif", "image/webp": ".webp"}[tc.mime]
		path := filepath.Join(dir, "img"+ext)
		if err := os.WriteFile(path, tc.header, 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := encodeImage(path, tc.mime)
		if err != nil {
			t.Errorf("encodeImage(%s) err = %v, want nil", tc.mime, err)
			continue
		}
		if !strings.HasPrefix(out, "data:"+tc.mime+";base64,") {
			t.Errorf("encodeImage(%s) = %q, want %q prefix", tc.mime, out, "data:"+tc.mime+";base64,")
		}
	}
}

// TestMatchesMagicBytes is the direct unit test for the magic-byte
// table — each accepted and rejected pair is asserted explicitly so
// future contributors don't subtly weaken the validation.
func TestMatchesMagicBytes(t *testing.T) {
	cases := []struct {
		name string
		mime string
		data []byte
		want bool
	}{
		{"jpeg-ok", "image/jpeg", []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00}, true},
		{"jpeg-bad", "image/jpeg", []byte{0x89, 0x50, 0x4E, 0x47}, false},
		{"jpeg-truncated", "image/jpeg", []byte{0xFF, 0xD8}, false},
		{"png-ok", "image/png", []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}, true},
		{"png-truncated", "image/png", []byte{0x89, 0x50, 0x4E, 0x47, 0x0D}, false},
		{"gif87-ok", "image/gif", []byte{0x47, 0x49, 0x46, 0x38, 0x37, 0x61}, true},
		{"gif89-ok", "image/gif", []byte{0x47, 0x49, 0x46, 0x38, 0x39, 0x61}, true},
		{"gif-bad", "image/gif", []byte{0x47, 0x49, 0x46, 0x38, 0x36, 0x61}, false}, // GIF86a, not supported
		{"webp-ok", "image/webp", append([]byte{0x52, 0x49, 0x46, 0x46, 0x00, 0x00, 0x00, 0x00}, []byte{0x57, 0x45, 0x42, 0x50}...), true},
		{"webp-truncated", "image/webp", []byte{0x52, 0x49, 0x46, 0x46, 0x00, 0x00, 0x00}, false},
		{"unknown-mime", "image/heic", []byte{0x00, 0x00, 0x00, 0x18}, false},
	}
	for _, tc := range cases {
		got := matchesMagicBytes(tc.data, tc.mime)
		if got != tc.want {
			t.Errorf("%s: matchesMagicBytes(%x, %q) = %v, want %v", tc.name, tc.data, tc.mime, got, tc.want)
		}
	}
}

// TestSendBlocksImage_WireShape_HasTypeTag locks the
// docs/bridge/pi.md §6 contract that each prompt.images[] entry
// carries {"type":"image", ...}. pi 0.84.x accepts without it
// (inferred from images[] membership) but the explicit tag is the
// canonical shape and protects against future schema tightening.
func TestSendBlocksImage_WireShape_HasTypeTag(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tiny.jpg")
	jpeg := []byte{0xFF, 0xD8, 0xFF, 0xE0}
	if err := os.WriteFile(path, jpeg, 0o600); err != nil {
		t.Fatal(err)
	}
	cw := &captureWire{}
	d := newFakeDriver(t, cw)

	done := make(chan error, 1)
	go func() {
		done <- d.SendBlocks(context.Background(), []agent.ContentBlock{
			{Type: agent.ContentImage, Path: path, MediaType: "image/jpeg"},
		})
	}()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(cw.Lines()) >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	lines := cw.Lines()
	if len(lines) < 1 {
		t.Fatal("no wire line written")
	}
	raw := lines[len(lines)-1]
	var frame struct {
		ID     string `json:"id"`
		Images []struct {
			Type     string `json:"type"`
			Data     string `json:"data"`
			MimeType string `json:"mimeType"`
		} `json:"images"`
	}
	if err := json.Unmarshal([]byte(raw), &frame); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(frame.Images) != 1 {
		t.Fatalf("images len = %d, want 1", len(frame.Images))
	}
	if frame.Images[0].Type != "image" {
		t.Errorf("images[0].type = %q, want \"image\"", frame.Images[0].Type)
	}

	driveResponse(d, frame.ID, true, "")
	<-done
}

// TestTranslate_PoisonTriggersOnPoisonedCallback is the auto-recovery
// hook regression: when finishTurnLocked sees stopReason="error" +
// errorMessage, it MUST invoke the onPoisoned callback exactly once.
// The driver wires this to d.New() so the session is rotated; without
// the callback, the "session broken forever" symptom of #290 stays.
func TestTranslate_PoisonTriggersOnPoisonedCallback(t *testing.T) {
	tr := newTestTranslator()
	var fired int
	tr.SetOnSessionPoisoned(func() { fired++ })

	mustTranslate(t, tr, `{"type":"message_update","assistantMessageEvent":{"type":"text_start","contentIndex":0}}`)
	mustTranslate(t, tr, `{"type":"message_update","assistantMessageEvent":{"type":"text_end","contentIndex":0}}`)
	mustTranslate(t, tr, `{"type":"message_end","message":{"role":"assistant",`+
		`"content":[],"stopReason":"error",`+
		`"errorMessage":"400: invalid image base64 content"}}`)
	mustTranslate(t, tr, `{"type":"agent_settled"}`)

	if fired != 1 {
		t.Errorf("onPoisoned fired %d times, want exactly 1", fired)
	}
}

// TestTranslate_PoisonNotFiredOnHappyPath asserts the symmetric
// guard: a settled turn with no errorMessage does NOT invoke the
// auto-recovery hook. Without this, every successful turn would
// trigger a new_session handshake, which would be catastrophic.
func TestTranslate_PoisonNotFiredOnHappyPath(t *testing.T) {
	tr := newTestTranslator()
	var fired int
	tr.SetOnSessionPoisoned(func() { fired++ })

	for _, l := range []string{
		`{"type":"message_update","assistantMessageEvent":{"type":"text_start","contentIndex":0}}`,
		`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"hi"}}`,
		`{"type":"message_update","assistantMessageEvent":{"type":"text_end","contentIndex":0}}`,
		`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"hi"}],"stopReason":"end_turn"}}`,
		`{"type":"agent_settled"}`,
	} {
		mustTranslate(t, tr, l)
	}
	if fired != 0 {
		t.Errorf("onPoisoned fired %d times on happy path, want 0", fired)
	}
}

// TestTranslate_PoisonErrIsErrSessionPoisoned locks the sentinel
// contract: errors.Is on the surfaced EventAgentResult.Err matches
// the exported ErrSessionPoisoned, so callers (chatsession, channel
// adapters, /gtw) can branch on it without string-matching.
func TestTranslate_PoisonErrIsErrSessionPoisoned(t *testing.T) {
	tr := newTestTranslator()
	tr.SetOnSessionPoisoned(func() {})

	mustTranslate(t, tr, `{"type":"message_update","assistantMessageEvent":{"type":"text_start","contentIndex":0}}`)
	mustTranslate(t, tr, `{"type":"message_update","assistantMessageEvent":{"type":"text_end","contentIndex":0}}`)
	mustTranslate(t, tr, `{"type":"message_end","message":{"role":"assistant",`+
		`"content":[],"stopReason":"error",`+
		`"errorMessage":"400: {\"message\":\"invalid image base64 content\"}"}}`)
	events := mustTranslate(t, tr, `{"type":"agent_settled"}`)

	var result *agent.AgentEvent
	for i := range events {
		if events[i].Kind == agent.EventAgentResult {
			ev := events[i]
			result = &ev
			break
		}
	}
	if result == nil || result.Err == nil {
		t.Fatal("expected EventAgentResult with non-nil Err")
	}
	if !errors.Is(result.Err, ErrSessionPoisoned) {
		t.Errorf("Err = %v, want errors.Is(_, ErrSessionPoisoned) == true", result.Err)
	}
	// And the original provider text must still be reachable so
	// the user can read the rejection reason.
	if !strings.Contains(result.Err.Error(), "invalid image base64 content") {
		t.Errorf("Err = %q, want it to contain the provider's rejection text", result.Err.Error())
	}
}