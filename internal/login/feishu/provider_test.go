package feishu

import (
	"bytes"
	"strings"
	"testing"

	"github.com/larksuite/oapi-sdk-go/v3/scene/registration"
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
		if got, want := a.preset.Desc, "Sleep tight, code all night."; got != want {
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
	if got, want := p.Desc, "Sleep tight, code all night."; got != want {
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

func containsString(haystack []string, needle string) bool {
	for _, v := range haystack {
		if v == needle {
			return true
		}
	}
	return false
}
