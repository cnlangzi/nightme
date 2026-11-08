package telemetry

import "testing"

func TestAllowed_DefaultsToSuppressed(t *testing.T) {
	Allow(false)
	if Allowed() {
		t.Fatal("Allowed() = true with the policy off, want false")
	}
}

func TestAllow_OptsOutOfThePolicy(t *testing.T) {
	t.Cleanup(func() { Allow(false) })

	Allow(true)
	if !Allowed() {
		t.Fatal("Allowed() = false after Allow(true), want true")
	}
}
