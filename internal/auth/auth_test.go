package auth

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// fakeProvider is a compile-time-checked Provider implementation
// used solely to exercise the interface contract. Anything more
// elaborate lives in the provider-specific sub-packages.
type fakeProvider struct {
	name string
	out  *Credentials
	err  error
}

func (f *fakeProvider) Name() string { return f.name }

func (f *fakeProvider) Login(_ context.Context) (*Credentials, error) {
	return f.out, f.err
}

// TestProvider_Interface is a compile-time check: any concrete
// Provider (real or fake) must satisfy the interface. If this
// stops compiling, the interface changed — update fakes too.
func TestProvider_Interface(t *testing.T) {
	var _ Provider = (*fakeProvider)(nil)
	var _ Provider = (*feishuAuthStub)(nil)
}

// feishuAuthStub exists so the interface check covers a second
// concrete type. The real FeishuAuth lives in internal/auth/feishu
// and cannot be imported here without an import cycle.
type feishuAuthStub struct{ name string }

func (f *feishuAuthStub) Name() string                                  { return f.name }
func (f *feishuAuthStub) Login(_ context.Context) (*Credentials, error) { return nil, nil }

// TestProvider_Name_And_Login verifies the fake behaves as the
// interface contract promises: name is sticky, errors propagate.
func TestProvider_Name_And_Login(t *testing.T) {
	want := &Credentials{AppID: "cli_x", AppSecret: "secret", AppName: "nightme", CreatedAt: time.Now()}
	sentinel := errors.New("boom")
	p := &fakeProvider{name: "feishu", out: want, err: sentinel}

	if got := p.Name(); got != "feishu" {
		t.Errorf("Name() = %q, want feishu", got)
	}
	got, err := p.Login(context.Background())
	if !errors.Is(err, sentinel) {
		t.Errorf("Login error = %v, want sentinel %v", err, sentinel)
	}
	if got != want {
		t.Errorf("Login credentials = %+v, want %+v", got, want)
	}
}

// TestCredentials_JSON exercises the on-disk JSON encoding. The
// shape is what `nightme auth status --json` will print, and what
// the CLI persists when writing channels.<provider>.accounts.main.
func TestCredentials_JSON(t *testing.T) {
	in := Credentials{
		AppID:     "cli_a1b2",
		AppSecret: "secret-value",
		AppName:   "nightme",
		CreatedAt: time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC),
	}

	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for _, want := range []string{
		`"app_id":"cli_a1b2"`,
		`"app_secret":"secret-value"`,
		`"app_name":"nightme"`,
		`"created_at":"2026-07-31T12:00:00Z"`,
	} {
		if !contains(data, want) {
			t.Errorf("Marshal missing %s\nactual: %s", want, data)
		}
	}

	var out Credentials
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out != in {
		t.Errorf("round-trip mismatch:\n got %+v\nwant %+v", out, in)
	}
}

// TestErrors_AreDistinct makes sure the sentinel errors stay
// distinct so errors.Is matching keeps working in callers.
func TestErrors_AreDistinct(t *testing.T) {
	sentinels := []error{ErrAuthTimeout, ErrAuthFailed}
	for i, a := range sentinels {
		for j, b := range sentinels {
			if i == j {
				continue
			}
			if errors.Is(a, b) {
				t.Errorf("errors.Is(%v, %v) = true; want false", a, b)
			}
		}
	}
}

// contains is a strings.Contains wrapper that works on []byte without
// an extra import.
func contains(haystack []byte, needle string) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if string(haystack[i:i+len(needle)]) == needle {
			return true
		}
	}
	return false
}
