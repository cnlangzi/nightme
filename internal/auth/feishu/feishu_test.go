package feishu

import (
	"bytes"
	"strings"
	"testing"

	"github.com/larksuite/oapi-sdk-go/v3/scene/registration"
)

// TestFeishuAuth_Name is the cheapest sanity check: the provider's
// name is the key the CLI subcommand tree uses to look it up.
func TestFeishuAuth_Name(t *testing.T) {
	a := NewFeishuAuth(FeishuAuthOptions{})
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
		"im:message:receive_v1",
		"im:message.reactions:write_only",
		"im:message:readonly",
		"im:message.group_at_msg:readonly",
		"im:message.p2p_msg:readonly",
		"im:resource",
		"im:chat:read",
		"im:chat.members:bot_access",
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

// TestNewFeishuAuth_FillsDefaults verifies the constructor fills in
// what callers most often leave blank: addons (DefaultAddons) and
// stdout. We can only observe the addons substitution indirectly —
// the Out is observable via a buffer.
func TestNewFeishuAuth_FillsDefaults(t *testing.T) {
	t.Run("addons substituted when caller passes nil", func(t *testing.T) {
		a := NewFeishuAuth(FeishuAuthOptions{})
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
		a := NewFeishuAuth(FeishuAuthOptions{Addons: custom})
		if got := a.addons.Scopes.Tenant; len(got) != 1 || got[0] != "only:this" {
			t.Errorf("caller addons not preserved: got %v", got)
		}
	})
}

// TestQrencode_Renders feeds an example URL through the renderer and
// checks the output is non-empty and carries QR-specific Unicode
// half-block characters. We deliberately do NOT check pixel-perfect
// output (skip2 owns that); we just confirm a QR appears.
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
	if !strings.ContainsAny(out, " ▀▄█") {
		t.Errorf("output contains no half-block QR characters:\n%s", out)
	}
	if !strings.Contains(out, "\n") {
		t.Errorf("output contains no newline (expected multi-line QR):\n%s", out)
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
// refactor of printQRCode cannot accidentally drop the URL line.
// The QR encoding itself is exercised by TestQrencode_Renders.
func TestPrintQRCode_WritesURLAndQR(t *testing.T) {
	a := NewFeishuAuth(FeishuAuthOptions{})
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
	if !strings.ContainsAny(out, " ▀▄█") {
		t.Errorf("printQRCode did not emit QR glyphs\n%s", out)
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
