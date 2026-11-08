package pi

import (
	"reflect"
	"testing"

	"github.com/cnlangzi/nightme/internal/bridge/telemetry"
)

func TestStarterInfo_ExposesHeadlessDefaults(t *testing.T) {
	telemetry.Allow(false)

	s := NewStarter("pi", "pi", nil)
	wantArgs := append([]string(nil), DefaultArgs...)
	wantEnv := append([]string(nil), telemetryEnv...)

	if got := s.Info().Args; !reflect.DeepEqual(got, wantArgs) {
		t.Fatalf("Info().Args = %v, want %v", got, wantArgs)
	}
	if got := s.Info().Env; !reflect.DeepEqual(got, wantEnv) {
		t.Fatalf("Info().Env = %v, want %v", got, wantEnv)
	}
}
