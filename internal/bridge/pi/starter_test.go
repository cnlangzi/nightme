package pi

import (
	"reflect"
	"testing"
)

func TestStarterInfo_ExposesHeadlessDefaults(t *testing.T) {
	s := NewStarter("pi", "pi", nil)
	wantArgs := append([]string(nil), DefaultArgs...)
	wantEnv := append([]string(nil), headlessEnv...)

	if got := s.Info().Args; !reflect.DeepEqual(got, wantArgs) {
		t.Fatalf("Info().Args = %v, want %v", got, wantArgs)
	}
	if got := s.Info().Env; !reflect.DeepEqual(got, wantEnv) {
		t.Fatalf("Info().Env = %v, want %v", got, wantEnv)
	}
}
