package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/larksuite/oapi-sdk-go/v3/scene/registration"

	"github.com/cnlangzi/nightme/internal/login"
)

// TestProvider_Name is the cheapest sanity check: the provider's
// name is the key the CLI subcommand tree uses to look it up.
func TestProvider_Name(t *testing.T) {
	a := New(Options{})
	if got, want := a.Name(), "feishu"; got != want {
		t.Errorf("Name() = %q, want %q", got, want)
	}
}

// TestDefaultAddons_ContainsRequiredScopes locks in the scope + event
// names nightme relies on. If Feishu renames a scope (or a future
// refactor removes one), this test fails loudly so the migration is
// intentional, not silent.
func TestDefaultAddons_ContainsRequiredScopes(t *testing.T) {
	addons := DefaultAddons()
	if addons == nil {
		t.Fatal("DefaultAddons() returned nil")
	}
	if addons.Preset == nil || *addons.Preset {
		t.Errorf("DefaultAddons.Preset = %v, want false (minimal base)", addons.Preset)
	}
	wantScopes := []string{
		"im:message:send_as_bot",
		"im:message:update",
		"im:message:receive_v1",
		"im:message.reactions:write_only",
		"im:message.reactions:read",
		"im:message:readonly",
		"im:message.group_at_msg:readonly",
		"im:message.group_msg",
		"im:message.p2p_msg:readonly",
		"im:message.pins:read",
		"im:message.pins:write_only",
		"im:message:recall",
		"im:message:send_multi_users",
		"im:message:send_sys_msg",
		"im:resource",
		"im:chat:read",
		"im:chat:update",
		"im:chat.members:bot_access",
		"contact:contact.base:readonly",
		"cardkit:card:write",
		"cardkit:card:read",
		"application:application:self_manage",
	}
	for _, want := range wantScopes {
		if !containsString(addons.Scopes.Tenant, want) {
			t.Errorf("DefaultAddons missing tenant scope %q (have %v)", want, addons.Scopes.Tenant)
		}
	}
	wantEvents := []string{
		"im.message.receive_v1",
	}
	for _, want := range wantEvents {
		if !containsString(addons.Events.Items.Tenant, want) {
			t.Errorf("DefaultAddons missing tenant event %q (have %v)", want, addons.Events.Items.Tenant)
		}
	}
	wantCallbacks := []string{
		"card.action.trigger",
	}
	for _, want := range wantCallbacks {
		if !containsString(addons.Callbacks.Items, want) {
			t.Errorf("DefaultAddons missing callback %q (have %v)", want, addons.Callbacks.Items)
		}
	}
}

// TestNew_FillsDefaults verifies the constructor fills in
// what callers most often leave blank: addons (DefaultAddons),
// AppPreset (DefaultAppPreset), and stdout. We can only observe the
// addons substitution indirectly — the Out is observable via a buffer.
func TestNew_FillsDefaults(t *testing.T) {
	t.Run("addons substituted when caller passes nil", func(t *testing.T) {
		a := New(Options{})
		if a.addons == nil {
			t.Fatal("constructor left addons nil; expected DefaultAddons()")
		}
		if got := a.addons.Scopes.Tenant; len(got) == 0 {
			t.Error("DefaultAddons should populate tenant scopes")
		}
	})

	t.Run("caller-supplied addons preserved", func(t *testing.T) {
		custom := DefaultAddons()
		custom.Scopes.Tenant = []string{"only:this"}
		a := New(Options{Addons: custom})
		if got := a.addons.Scopes.Tenant; len(got) != 1 || got[0] != "only:this" {
			t.Errorf("caller addons not preserved: got %v", got)
		}
	})

	t.Run("preset substituted with NightMe brand default when caller passes nil", func(t *testing.T) {
		a := New(Options{})
		if a.preset == nil {
			t.Fatal("constructor left preset nil; expected DefaultAppPreset()")
		}
		if got, want := a.preset.Name, "NightMe"; got != want {
			t.Errorf("default preset.Name = %q, want %q", got, want)
		}
		if got, want := a.preset.Desc, "Sleep tight, NightMe code all night."; got != want {
			t.Errorf("default preset.Desc = %q, want %q", got, want)
		}
	})

	t.Run("caller-supplied preset preserved", func(t *testing.T) {
		custom := &registration.AppPreset{Name: "CustomBot", Desc: "hello"}
		a := New(Options{AppPreset: custom})
		if a.preset != custom {
			t.Errorf("caller preset not preserved: got %+v", a.preset)
		}
	})
}

// TestDefaultAppPreset guards the brand strings: they are shown to
// the user on the Feishu consent page and must not silently change.
func TestDefaultAppPreset(t *testing.T) {
	p := DefaultAppPreset()
	if p == nil {
		t.Fatal("DefaultAppPreset() returned nil")
	}
	if got, want := p.Name, "NightMe"; got != want {
		t.Errorf("DefaultAppPreset().Name = %q, want %q", got, want)
	}
	if got, want := p.Desc, "Sleep tight, NightMe code all night."; got != want {
		t.Errorf("DefaultAppPreset().Desc = %q, want %q", got, want)
	}
}

// TestQrencode_Renders feeds an example URL through the renderer
// and checks the output is non-empty, multi-line, carries the
// half-block glyphs, and is roughly square (line count ≈ half the
// column count). We deliberately do NOT check pixel-perfect output
// (skip2 owns the matrix); we just confirm a QR appears at full
// resolution — no downsampling — so the mobile scanner can lock on.
func TestQrencode_Renders(t *testing.T) {
	const url = "https://accounts.feishu.cn/oauth2/v1/app/registration?from=sdk"

	var buf bytes.Buffer
	if err := RenderASCII(url, &buf, false); err != nil {
		t.Fatalf("RenderASCII: %v", err)
	}
	out := buf.String()
	if out == "" {
		t.Fatal("RenderASCII produced empty output")
	}
	// Glyph alphabet: full block, upper half, lower half, space.
	// We don't assert which combination appears — that depends on
	// the QR bit pattern. Just confirm we used block characters.
	if !strings.ContainsAny(out, " █▀▄") {
		t.Errorf("output contains no half-block glyphs:\n%s", out)
	}
	if !strings.Contains(out, "\n") {
		t.Errorf("output contains no newline (expected multi-line QR):\n%s", out)
	}
	// Every non-empty line should be the same width — the renderer
	// walks a half-block grid where each line is one source row's
	// worth of columns. We measure in runes (not bytes) because
	// "█"/"▀"/"▄" are 3 bytes each in UTF-8.
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) == 0 {
		t.Fatal("output has no lines after trimming")
	}
	width := len([]rune(lines[0]))
	for i, l := range lines {
		if w := len([]rune(l)); w != width {
			t.Errorf("line %d width=%d, want %d (renderer should emit uniform-width lines):\n%s", i, w, width, out)
		}
	}
	// Full-resolution: source QR is ~41 modules wide at medium ECC,
	// so the output should be roughly that wide. The exact number
	// depends on skip2's QR-version selection for the payload; we
	// accept 25–65 so future payload changes don't snap the test.
	if width < 25 || width > 65 {
		t.Errorf("QR width=%d, want ~41 (full-resolution, no downsampling):\n%s", width, out)
	}
	// Half-block compression: line count should be roughly half the
	// column count (rounded up). This is what makes the rendered
	// output visually square despite the 2:1 terminal cell aspect.
	halfHeight := (width + 1) / 2
	if len(lines) > halfHeight+1 || len(lines) < halfHeight-1 {
		t.Errorf("QR line count=%d, want ~%d (half of width %d via half-block compression):\n%s",
			len(lines), halfHeight, width, out)
	}
}

// TestQrencode_EmptyInput guards against accidentally encoding an
// empty string — skip2 returns "no data" which we want surfaced as
// a meaningful error rather than silently emitted.
func TestQrencode_EmptyInput(t *testing.T) {
	var buf bytes.Buffer
	if err := RenderASCII("", &buf, false); err == nil {
		t.Error("RenderASCII(\"\") = nil, want error")
	}
}

// TestPrintQRCode_WritesURLAndQR wires the OnQRCode path so a future
// refactor of printQRCode cannot accidentally drop the URL line or
// the "Waiting for scan" hint. The QR encoding itself is exercised
// by TestQrencode_Renders.
func TestPrintQRCode_WritesURLAndQR(t *testing.T) {
	a := New(Options{})
	var buf bytes.Buffer
	a.out = &buf

	a.printQRCode(&registration.QRCodeInfo{
		URL:      "https://accounts.feishu.cn/oauth2/v1/app/registration?from=sdk",
		ExpireIn: 600,
	})

	out := buf.String()
	if !strings.Contains(out, "https://accounts.feishu.cn") {
		t.Errorf("printQRCode missing URL\n%s", out)
	}
	if !strings.Contains(out, "600") {
		t.Errorf("printQRCode missing expiry seconds\n%s", out)
	}
	if !strings.ContainsAny(out, " █▀▄") {
		t.Errorf("printQRCode did not emit QR glyphs\n%s", out)
	}
	// Friendly next-step hint: without this the user just sees the
	// QR and a polling loop they don't know is waiting. We assert on
	// a substring that's likely to survive copy edits ("Waiting" /
	// "scan") so a future tweak to wording doesn't snap the test.
	if !strings.Contains(out, "Waiting") || !strings.Contains(out, "scan") {
		t.Errorf("printQRCode missing waiting-for-scan hint\n%s", out)
	}
}

// TestPrintStatus_FriendlyMessages ensures the SDK's raw status
// codes are translated into user-facing language and that the
// "polling" status (which fires on every poll cycle) is silent so
// the terminal doesn't get spammed once a second for ten minutes.
func TestPrintStatus_FriendlyMessages(t *testing.T) {
	t.Run("polling is silent (avoid terminal spam)", func(t *testing.T) {
		a := New(Options{})
		var buf bytes.Buffer
		a.out = &buf

		// Simulate the SDK calling OnStatusChange for every poll
		// cycle. None of these should produce output.
		for i := 0; i < 50; i++ {
			a.printStatus(&registration.StatusChangeInfo{Status: registration.StatusPolling})
		}
		if got := buf.String(); got != "" {
			t.Errorf("polling produced %d bytes of output, want 0:\n%s", len(got), got)
		}
	})

	t.Run("slow_down is friendly", func(t *testing.T) {
		a := New(Options{})
		var buf bytes.Buffer
		a.out = &buf

		a.printStatus(&registration.StatusChangeInfo{Status: registration.StatusSlowDown})
		out := buf.String()
		if out == "" {
			t.Fatal("slow_down produced no output")
		}
		if strings.Contains(out, "slow_down") || strings.Contains(out, "status:") {
			t.Errorf("slow_down leaked raw SDK status to user: %q", out)
		}
	})

	t.Run("domain_switched is friendly", func(t *testing.T) {
		a := New(Options{})
		var buf bytes.Buffer
		a.out = &buf

		a.printStatus(&registration.StatusChangeInfo{Status: registration.StatusDomainSwitched})
		out := buf.String()
		if out == "" {
			t.Fatal("domain_switched produced no output")
		}
		if strings.Contains(out, "domain_switched") || strings.Contains(out, "status:") {
			t.Errorf("domain_switched leaked raw SDK status to user: %q", out)
		}
	})

	t.Run("unknown status surfaces visibly", func(t *testing.T) {
		a := New(Options{})
		var buf bytes.Buffer
		a.out = &buf

		a.printStatus(&registration.StatusChangeInfo{Status: "future_thing"})
		out := buf.String()
		if out == "" {
			t.Fatal("unknown status produced no output")
		}
		if !strings.Contains(out, "future_thing") {
			t.Errorf("unknown status lost; got %q, want substring %q", out, "future_thing")
		}
	})
}

// TestBuildPostEnvelope_BothLocales asserts the wire-level shape
// every greeting rides on: a `zh_cn` block AND an `en_us` block,
// each containing exactly one paragraph of one text node. The
// receiver's Feishu client picks the locale tag matching its UI
// language; if either block is missing or empty, the user sees
// the wrong-language greeting (or nothing).
func TestBuildPostEnvelope_BothLocales(t *testing.T) {
	body := login.GreetingBody{
		Chinese: "你好 🌙。",
		English: "Hi 👋.",
	}
	envelope, err := buildPostEnvelope(body)
	if err != nil {
		t.Fatalf("buildPostEnvelope: %v", err)
	}

	var parsed struct {
		ZhCn postLang `json:"zh_cn"`
		EnUs postLang `json:"en_us"`
	}
	if err := json.Unmarshal([]byte(envelope), &parsed); err != nil {
		t.Fatalf("envelope invalid JSON: %v\nraw: %s", err, envelope)
	}

	// Both locale blocks must have exactly one paragraph of one
	// text node — the only shape we want in v1.
	if got, want := nodeText(parsed.ZhCn.Content), body.Chinese; got != want {
		t.Errorf("zh_cn text = %q, want %q", got, want)
	}
	if got, want := nodeText(parsed.EnUs.Content), body.English; got != want {
		t.Errorf("en_us text = %q, want %q", got, want)
	}
}

// TestBuildPostEnvelope_QuoteSafety ensures JSON escapes survive
// a round-trip when bodies contain " or \. The greeting copy is
// currently ASCII / CJK (no special chars), but a future
// translation could include a quote — we want the envelope to
// still be valid JSON.
func TestBuildPostEnvelope_QuoteSafety(t *testing.T) {
	body := login.GreetingBody{
		Chinese: `他说 "你好" —— 没问题。`,
		English: `He said "hi" — it's fine.`,
	}
	envelope, err := buildPostEnvelope(body)
	if err != nil {
		t.Fatalf("buildPostEnvelope: %v", err)
	}
	if !strings.Contains(envelope, `\"hi\"`) {
		t.Errorf("envelope did not escape double quotes: %s", envelope)
	}
	if !strings.Contains(envelope, `\"你好\"`) {
		t.Errorf("envelope did not escape Chinese double quotes: %s", envelope)
	}
	// Round-trip: parse it back, both halves survive.
	var parsed struct {
		ZhCn postLang `json:"zh_cn"`
		EnUs postLang `json:"en_us"`
	}
	if err := json.Unmarshal([]byte(envelope), &parsed); err != nil {
		t.Fatalf("round-trip failed: %v", err)
	}
	if got := nodeText(parsed.ZhCn.Content); got != body.Chinese {
		t.Errorf("zh_cn round-trip = %q, want %q", got, body.Chinese)
	}
}

// nodeText walks a Feishu post envelope's content array (paragraphs
// of element nodes) and concatenates the text nodes. Used by the
// buildPostEnvelope tests to verify the bilingual payload survives
// the JSON round-trip.
func nodeText(content [][]postNode) string {
	var out string
	for _, paragraph := range content {
		for _, node := range paragraph {
			if node.Tag == "text" {
				out += node.Text
			}
		}
	}
	return out
}

// TestProvider_Greet_NoSendDM_Skips covers the no-op branch: when
// Login was bypassed (tests) or the SDK didn't echo user_info back
// (production), sendDM is nil and Greet must NOT touch the network.
func TestProvider_Greet_NoSendDM_Skips(t *testing.T) {
	p := New(Options{})
	// sendDM left nil; larkClient left nil.
	if err := p.Greet(context.Background(), []login.GreetingBody{{
		Chinese: "x", English: "y",
	}}); err != nil {
		t.Errorf("Greet with nil sendDM should be a no-op, got error: %v", err)
	}
}

// TestProvider_Greet_EmptyMessages covers the empty-slice fast path.
// sendDM is wired (via the test seam) but the loop never runs.
func TestProvider_Greet_EmptyMessages(t *testing.T) {
	p := New(Options{})
	p.sendDM = func(_ context.Context, _ login.GreetingBody) error {
		t.Error("sendDM called despite empty messages slice")
		return nil
	}
	if err := p.Greet(context.Background(), nil); err != nil {
		t.Errorf("Greet with empty messages should be a no-op, got error: %v", err)
	}
}

// TestProvider_Greet_ForwardsAllBodies asserts the loop visits every
// element in order, passing the bilingual body to sendDM exactly
// once per element. If a future refactor drops a body or reorders
// the loop, this test catches it.
func TestProvider_Greet_ForwardsAllBodies(t *testing.T) {
	p := New(Options{})
	p.sendDM = func(_ context.Context, _ login.GreetingBody) error { return nil }

	in := login.GreetingMessages{
		{Chinese: "中文 A", English: "English A"},
		{Chinese: "中文 B", English: "English B"},
		{Chinese: "中文 C", English: "English C"},
	}

	var seen []login.GreetingBody
	p.sendDM = func(_ context.Context, b login.GreetingBody) error {
		seen = append(seen, b)
		return nil
	}

	if err := p.Greet(context.Background(), in); err != nil {
		t.Fatalf("Greet: %v", err)
	}
	if len(seen) != 3 {
		t.Fatalf("sendDM called %d times, want 3", len(seen))
	}
	for i, want := range in {
		if seen[i] != want {
			t.Errorf("call %d: got %+v, want %+v", i, seen[i], want)
		}
	}
}

// TestProvider_Greet_PropagatesErrorAndStops ensures a failure on
// post 1 short-circuits the loop — post 2 is NOT attempted.
// Otherwise a transient first-message error would spam the user's
// chat with a half-baked greeting (post 2 lands even though
// post 1 failed).
func TestProvider_Greet_PropagatesErrorAndStops(t *testing.T) {
	p := New(Options{})
	wantErr := errors.New("post 1 send failed")
	calls := 0
	p.sendDM = func(_ context.Context, _ login.GreetingBody) error {
		calls++
		return wantErr
	}

	in := login.GreetingMessages{
		{Chinese: "A", English: "A"},
		{Chinese: "B", English: "B"},
	}
	err := p.Greet(context.Background(), in)
	if !errors.Is(err, wantErr) {
		t.Errorf("Greet err = %v, want wraps %v", err, wantErr)
	}
	if calls != 1 {
		t.Errorf("sendDM called %d times, want 1 (post 2 should NOT be attempted)", calls)
	}
}

// TestProvider_Greet_DetachedContext asserts the per-message ctx
// the loop creates is independent of the caller-passed ctx. A
// cancelled parent ctx must NOT short-circuit the greeting — Login
// just returned with a healthy 10-min deadline, and the user
// might Ctrl+C right after; we still want the greeting to fire.
func TestProvider_Greet_DetachedContext(t *testing.T) {
	p := New(Options{})
	p.sendDM = func(_ context.Context, _ login.GreetingBody) error { return nil }

	parent, cancel := context.WithCancel(context.Background())
	cancel() // parent already dead before Greet starts

	if err := p.Greet(parent, login.GreetingMessages{{Chinese: "x", English: "y"}}); err != nil {
		t.Errorf("Greet should ignore cancelled parent ctx, got: %v", err)
	}
}

func containsString(haystack []string, needle string) bool {
	for _, v := range haystack {
		if v == needle {
			return true
		}
	}
	return false
}
