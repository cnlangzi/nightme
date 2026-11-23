package outcome

import (
	"net/http"
	"testing"
)

// TestOutcome_SuccessAgreesWithTheStatusCategory asserts the rule the
// contract names: a 2xx is a success, a 4xx or a 5xx is not, and a
// status of zero leaves the verdict to whoever produced it.
func TestOutcome_SuccessAgreesWithTheStatusCategory(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		success bool
	}{
		{"ok", http.StatusOK, true},
		{"no content", http.StatusNoContent, true},
		{"bad request", http.StatusBadRequest, false},
		{"conflict", http.StatusConflict, false},
		{"too many requests", http.StatusTooManyRequests, false},
		{"internal server error", http.StatusInternalServerError, false},
		{"service unavailable", http.StatusServiceUnavailable, false},
		{"no status, success", 0, true},
		{"no status, failure", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := Outcome{Success: tc.success, Status: tc.status}
			if got := o.Succeeded(); got != tc.success {
				t.Fatalf("Succeeded() = %v, want %v", got, tc.success)
			}
			if got := o.Exception(); got == tc.success {
				t.Fatalf("Exception() = %v, want %v", got, !tc.success)
			}
		})
	}
}

// TestOutcome_Reported — an outcome that carries nothing is not a
// verdict; one that carries a status, words or facts is.
func TestOutcome_Reported(t *testing.T) {
	if (Outcome{}).Reported() {
		t.Fatal("the zero outcome reports a verdict")
	}
	if !(Outcome{Status: http.StatusOK}).Reported() {
		t.Fatal("an outcome with a status reports a verdict")
	}
	if !(Outcome{StatusText: "the work was done"}).Reported() {
		t.Fatal("an outcome with words reports a verdict")
	}
	if !(Outcome{Data: map[string]string{"issue": "42"}}).Reported() {
		t.Fatal("an outcome with facts reports a verdict")
	}
}

// TestMerge_TheFailingSideWins — a command that found nothing to do
// does not hide a turn that died on an upstream 429.
func TestMerge_TheFailingSideWins(t *testing.T) {
	cmd := Outcome{Success: true, Status: http.StatusOK, StatusText: "there was nothing to do"}
	turn := Outcome{Success: false, Status: http.StatusTooManyRequests, StatusText: "the agent is rate limited"}

	merged := Merge(cmd, turn)
	if merged.Success {
		t.Fatal("the merged outcome is a success")
	}
	if merged.Status != http.StatusTooManyRequests {
		t.Fatalf("merged status = %d, want %d", merged.Status, http.StatusTooManyRequests)
	}
	if merged.StatusText != turn.StatusText {
		t.Fatalf("merged text = %q, want %q", merged.StatusText, turn.StatusText)
	}

	// The order does not matter: the failing side wins either way.
	merged = Merge(turn, cmd)
	if merged.Status != http.StatusTooManyRequests {
		t.Fatalf("merged status = %d, want %d", merged.Status, http.StatusTooManyRequests)
	}
}

// TestMerge_TwoFailuresKeepTheEarlierOne — the command's own
// conclusion survives a later, more generic one.
func TestMerge_TwoFailuresKeepTheEarlierOne(t *testing.T) {
	cmd := Outcome{Success: false, Status: http.StatusConflict, StatusText: "the branch exists"}
	turn := Outcome{Success: false, Status: http.StatusInternalServerError, StatusText: "the turn died"}

	merged := Merge(cmd, turn)
	if merged.Status != http.StatusConflict {
		t.Fatalf("merged status = %d, want %d", merged.Status, http.StatusConflict)
	}
	if merged.StatusText != cmd.StatusText {
		t.Fatalf("merged text = %q, want %q", merged.StatusText, cmd.StatusText)
	}
}

// TestMerge_ASideWithoutAStatusNeverWins — a side that carries no
// verdict of its own says what it knows and nothing about how the
// work went, so it never decides.
func TestMerge_ASideWithoutAStatusNeverWins(t *testing.T) {
	facts := Outcome{
		Success: false,
		Status:  0,
		Data:    map[string]string{"branch": "feat/x"},
	}
	verdict := Outcome{
		Success:    false,
		Status:     http.StatusTooManyRequests,
		StatusText: "the upstream is rate limiting",
	}

	for _, merged := range []Outcome{Merge(facts, verdict), Merge(verdict, facts)} {
		if merged.Status != http.StatusTooManyRequests {
			t.Errorf("merged status = %d, want %d", merged.Status, http.StatusTooManyRequests)
		}
		if merged.Data["branch"] != "feat/x" {
			t.Errorf("the facts did not ride along: %v", merged.Data)
		}
	}
}

// TestMerge_AnOutcomeThatSaysNothingLoses — saying nothing is not a
// failure.
func TestMerge_AnOutcomeThatSaysNothingLoses(t *testing.T) {
	said := Outcome{Success: true, Status: http.StatusOK, StatusText: "the work was done"}

	merged := Merge(Outcome{}, said)
	if merged.Status != http.StatusOK {
		t.Fatalf("merged status = %d, want %d", merged.Status, http.StatusOK)
	}
	merged = Merge(said, Outcome{})
	if merged.Status != http.StatusOK {
		t.Fatalf("merged status = %d, want %d", merged.Status, http.StatusOK)
	}
}

// TestMerge_TheFactsAreMergedKeyByKey — the later value wins, and a
// key only one side carries survives.
func TestMerge_TheFactsAreMergedKeyByKey(t *testing.T) {
	a := Outcome{Success: true, Status: http.StatusOK, Data: map[string]string{"issue": "42", "branch": "main"}}
	b := Outcome{Success: false, Status: http.StatusConflict, Data: map[string]string{"branch": "fix/42"}}

	merged := Merge(a, b)
	if merged.Data["issue"] != "42" {
		t.Fatalf("issue = %q, want %q", merged.Data["issue"], "42")
	}
	if merged.Data["branch"] != "fix/42" {
		t.Fatalf("branch = %q, want %q", merged.Data["branch"], "fix/42")
	}
}

// TestMerge_TheFactsAreNotShared — the merged outcome never hands out
// the map of the outcome it came from.
func TestMerge_TheFactsAreNotShared(t *testing.T) {
	a := Outcome{Success: true, Status: http.StatusOK, Data: map[string]string{"issue": "42"}}
	merged := Merge(a, Outcome{})
	merged.Data["issue"] = "7"
	if a.Data["issue"] != "42" {
		t.Fatalf("the source outcome was changed: %q", a.Data["issue"])
	}
}
