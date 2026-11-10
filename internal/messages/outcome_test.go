package messages

import "testing"

// TestMerge_SeverityOrder pins the severity ladder:
//
//	failed > cancelled > timeout > incomplete > ok > noop > unknown
func TestMerge_SeverityOrder(t *testing.T) {
	order := []OutcomeStatus{
		OutcomeFailed,
		OutcomeCancelled,
		OutcomeTimeout,
		OutcomeIncomplete,
		OutcomeOK,
		OutcomeNoop,
		OutcomeUnknown,
	}
	for i := 0; i < len(order); i++ {
		for j := i + 1; j < len(order); j++ {
			hi, lo := order[i], order[j]
			got := Merge(
				Outcome{Status: hi, Summary: string(hi)},
				Outcome{Status: lo, Summary: string(lo)},
			)
			if got.Status != hi {
				t.Errorf("Merge(%s, %s).Status = %s, want %s", hi, lo, got.Status, hi)
			}
			got = Merge(
				Outcome{Status: lo, Summary: string(lo)},
				Outcome{Status: hi, Summary: string(hi)},
			)
			if got.Status != hi {
				t.Errorf("Merge(%s, %s).Status = %s, want %s", lo, hi, got.Status, hi)
			}
		}
	}
}

// TestMerge_UnknownOnlyWhenNothingReported covers the "unknown only
// when nothing reported" rule in both directions.
func TestMerge_UnknownOnlyWhenNothingReported(t *testing.T) {
	got := Merge(Outcome{Status: OutcomeUnknown}, Outcome{Status: OutcomeOK})
	if got.Status != OutcomeOK {
		t.Errorf("Merge(unknown, ok).Status = %s, want ok", got.Status)
	}
	got = Merge(Outcome{Status: OutcomeOK}, Outcome{Status: OutcomeUnknown})
	if got.Status != OutcomeOK {
		t.Errorf("Merge(ok, unknown).Status = %s, want ok", got.Status)
	}
	got = Merge(Outcome{Status: OutcomeUnknown}, Outcome{Status: OutcomeUnknown})
	if got.Status != OutcomeUnknown {
		t.Errorf("Merge(unknown, unknown).Status = %s, want unknown", got.Status)
	}
	// An empty status is the zero value and normalizes to unknown.
	got = Merge(Outcome{}, Outcome{})
	if got.Status != OutcomeUnknown {
		t.Errorf("Merge(empty, empty).Status = %s, want unknown", got.Status)
	}
}

// TestMerge_DataLaterWins checks that Data merges with later values
// winning and that keys only one side reported survive.
func TestMerge_DataLaterWins(t *testing.T) {
	got := Merge(
		Outcome{Status: OutcomeOK, Data: map[string]string{"branch": "a", "sha": "1"}},
		Outcome{Status: OutcomeOK, Data: map[string]string{"branch": "b"}},
	)
	if got.Data["branch"] != "b" {
		t.Errorf("Data[branch] = %q, want %q", got.Data["branch"], "b")
	}
	if got.Data["sha"] != "1" {
		t.Errorf("Data[sha] = %q, want %q", got.Data["sha"], "1")
	}
}

// TestMerge_SummaryFromMostSevere checks the summary rule: the most
// severe outcome's summary wins; on a tie the earlier one wins so a
// command's specific summary is not replaced by a later generic one.
func TestMerge_SummaryFromMostSevere(t *testing.T) {
	got := Merge(
		Outcome{Status: OutcomeOK, Summary: "committed"},
		Outcome{Status: OutcomeFailed, Summary: "push rejected"},
	)
	if got.Summary != "push rejected" {
		t.Errorf("Summary = %q, want %q", got.Summary, "push rejected")
	}

	got = Merge(
		Outcome{Status: OutcomeOK, Summary: "committed"},
		Outcome{Status: OutcomeOK, Summary: ""},
	)
	if got.Summary != "committed" {
		t.Errorf("Summary = %q, want %q", got.Summary, "committed")
	}

	got = Merge(
		Outcome{Status: OutcomeOK, Summary: ""},
		Outcome{Status: OutcomeOK, Summary: "later"},
	)
	if got.Summary != "later" {
		t.Errorf("Summary = %q, want %q", got.Summary, "later")
	}
}

// TestOutcome_IsReported covers the "did anything report" predicate.
func TestOutcome_IsReported(t *testing.T) {
	if (Outcome{}).IsReported() {
		t.Error("zero Outcome reported")
	}
	if (Outcome{Status: OutcomeUnknown}).IsReported() {
		t.Error("unknown Outcome reported")
	}
	if !(Outcome{Status: OutcomeNoop}).IsReported() {
		t.Error("noop Outcome not reported")
	}
}

// TestOutboundKind_Settled pins the wire name of the settled kind.
func TestOutboundKind_Settled(t *testing.T) {
	if got := OutMessageSettled.String(); got != "message_settled" {
		t.Fatalf("OutMessageSettled.String() = %q, want %q", got, "message_settled")
	}
}
