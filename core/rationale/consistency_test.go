package rationale

import "testing"

// The three acceptance cases from TG-317, stated as the ticket states them.
func TestTheThreeOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name, text, target string
		wantDisagree       bool
	}{
		{"different host escalates", "restart nginx on web01 because the unit is failed", "db01", true},
		{"same host does not escalate", "restart nginx on web01 because the unit is failed", "web01", false},
		{"no host at all abstains", "the unit is failed and the restart is idempotent", "web01", false},
	} {
		got := Check(tc.text, tc.target)
		if got.Disagrees != tc.wantDisagree {
			t.Errorf("%s: Disagrees = %v, want %v (named=%v target=%q)",
				tc.name, got.Disagrees, tc.wantDisagree, got.Named, got.Target)
		}
	}
}

// A rationale naming SEVERAL hosts, one of which is the target, agrees. Real rationales legitimately
// mention the neighbourhood ("web01 depends on db01, restarting db01"), and flagging those would make the
// check fire on the most careful prose in the corpus.
func TestNamingTheTargetAmongOthersAgrees(t *testing.T) {
	if got := Check("web01 depends on db01; restarting db01 to clear the fault", "db01"); got.Disagrees {
		t.Errorf("a rationale naming the target alongside others disagreed: %+v", got)
	}
}

// FQDN on either side must not manufacture a disagreement. A deployment that seals FQDNs would otherwise
// see this fire on every single action, which is how a heuristic gets switched off.
func TestFirstLabelComparisonSurvivesFQDNs(t *testing.T) {
	if got := Check("restart on web01", "web01.prod.example.net"); got.Disagrees {
		t.Errorf("short name vs FQDN target disagreed: %+v", got)
	}
	if got := Check("restart on web01.prod.example.net", "web01"); got.Disagrees {
		t.Errorf("FQDN in prose vs short target disagreed: %+v", got)
	}
}

// THE FALSE-POSITIVE FAMILIES. Each of these is a token that a naive "word with a digit" rule would call a
// hostname, and each would poll an honest action.
func TestNumbersAndUnitNamesAreNotHosts(t *testing.T) {
	for _, text := range []string{
		"free rises above 10% within 5min",
		"retried 3x before giving up",
		"restart nginx because the unit is failed",
		"postgresql is not accepting connections",
	} {
		if got := Check(text, "web01"); got.Disagrees {
			t.Errorf("%q was read as naming a host (%v) — this would poll an honest action", text, got.Named)
		}
	}
}

// An empty target ABSTAINS. Disagreeing here would poll the entire estate the moment a caller passed a
// blank — the heuristic must fail toward silence, because noise is what gets it disabled.
func TestEmptyTargetAbstainsRatherThanDisagreeing(t *testing.T) {
	if got := Check("restart nginx on web01", ""); got.Disagrees {
		t.Error("an empty sealed target produced a disagreement. Every rationale would 'disagree' against " +
			"a blank, so a single wiring slip would poll everything and the check would be turned off.")
	}
}

// The notice must name BOTH sides. A poll that says "the rationale disagrees" without saying how is a poll
// nobody can adjudicate — and adjudicating it is the entire point of escalating rather than refusing.
func TestReasonNamesBothSides(t *testing.T) {
	r := Check("restart nginx on web01", "db01").Reason()
	if r == "" {
		t.Fatal("a disagreement rendered an empty reason")
	}
	for _, want := range []string{"web01", "db01"} {
		if !contains(r, want) {
			t.Errorf("reason %q does not name %q — the reviewer cannot see what contradicts what", r, want)
		}
	}
	if Check("restart nginx on web01", "web01").Reason() != "" {
		t.Error("an agreeing check rendered a non-empty reason, which would put noise in every notice")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
