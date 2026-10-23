package verify

import (
	"reflect"
	"testing"

	"github.com/territory-grounder/grounder/core/safety"
)

func pred() Prediction {
	return Prediction{
		ActionID: "a1", PlanHash: "p1", TargetHost: "web01", Site: "dc1",
		PredictedHosts: map[string]struct{}{"db01": {}, "cache01": {}},
		PredictedRules: map[string]struct{}{
			RuleKey("db01", "HighLatency"):    {},
			RuleKey("cache01", "MemPressure"): {},
		},
	}
}

func TestComputeVerdictMatch(t *testing.T) {
	// all observed alerts were predicted (host+rule) → match
	obs := []ObservedAlert{{Host: "db01", Rule: "HighLatency", Site: "dc1"}}
	if v := ComputeVerdict(pred(), obs); v != safety.VerdictMatch {
		t.Fatalf("want match, got %s", v)
	}
	// quiet case: no observed alerts → match
	if v := ComputeVerdict(pred(), nil); v != safety.VerdictMatch {
		t.Fatalf("quiet remediation want match, got %s", v)
	}
}

func TestComputeVerdictPartial(t *testing.T) {
	// predicted host, UNpredicted rule → partial
	obs := []ObservedAlert{{Host: "db01", Rule: "DiskFull", Site: "dc1"}}
	if v := ComputeVerdict(pred(), obs); v != safety.VerdictPartial {
		t.Fatalf("want partial, got %s", v)
	}
}

func TestComputeVerdictDeviation(t *testing.T) {
	// surprise host the prediction never named → deviation (dominates)
	obs := []ObservedAlert{
		{Host: "db01", Rule: "HighLatency", Site: "dc1"}, // predicted (match)
		{Host: "router09", Rule: "BGPDown", Site: "dc1"}, // SURPRISE
	}
	if v := ComputeVerdict(pred(), obs); v != safety.VerdictDeviation {
		t.Fatalf("a surprise host must be deviation, got %s", v)
	}
}

func TestComputeVerdictExclusions(t *testing.T) {
	// the action's own target-host alerting is the expected direct effect → excluded (stays match)
	obs := []ObservedAlert{{Host: "web01", Rule: "Down", Site: "dc1"}}
	if v := ComputeVerdict(pred(), obs); v != safety.VerdictMatch {
		t.Fatalf("target-host alert must be excluded (match), got %s", v)
	}
	// A surprise host is ALWAYS a deviation regardless of any self-reported site label — the verdict never
	// trusts an ingest-supplied site to downgrade a surprise cascade to a match (fail-closed). This holds for
	// a different site LABEL (a real cascade to a third-site host must not be swallowed) and for an
	// unknown/empty site (a VPS tunnel endpoint).
	for _, a := range []ObservedAlert{
		{Host: "gr-host99", Rule: "Down", Site: "dc2"},
		{Host: "notrf01vps01", Rule: "Down", Site: ""},
	} {
		if v := ComputeVerdict(pred(), []ObservedAlert{a}); v != safety.VerdictDeviation {
			t.Fatalf("surprise host %q must be a deviation regardless of site label, got %s", a.Host, v)
		}
	}
}

func TestAutoResolvable(t *testing.T) {
	if AutoResolvable(safety.VerdictDeviation) {
		t.Fatal("a deviation must never be auto-resolvable (REQ-104)")
	}
	if !AutoResolvable(safety.VerdictMatch) || !AutoResolvable(safety.VerdictPartial) {
		t.Fatal("match/partial should be auto-resolvable")
	}
}

// verdictCase is one prediction/observation pair with the enum the pre-detail ComputeVerdict produced. The
// battery is the NON-BREAKING oracle (REQ-103a): the typed detail's derived enum must equal both the named
// pre-existing verdict AND the bare ComputeVerdict for identical inputs — the verdict decision must not move.
type verdictCase struct {
	name string
	pred Prediction
	obs  []ObservedAlert
	want safety.Verdict
}

func verdictBattery() []verdictCase {
	p := pred() // target web01/dc1; predicts db01(HighLatency), cache01(MemPressure)
	return []verdictCase{
		{"quiet post-state (nil observed) → match", p, nil, safety.VerdictMatch},
		{"predicted host+rule → match", p, []ObservedAlert{{Host: "db01", Rule: "HighLatency", Site: "dc1"}}, safety.VerdictMatch},
		{"predicted host, unpredicted rule → partial", p, []ObservedAlert{{Host: "db01", Rule: "DiskFull", Site: "dc1"}}, safety.VerdictPartial},
		{"surprise host → deviation", p, []ObservedAlert{{Host: "router09", Rule: "BGPDown", Site: "dc1"}}, safety.VerdictDeviation},
		{"target-host alert excluded → match", p, []ObservedAlert{{Host: "web01", Rule: "Down", Site: "dc1"}}, safety.VerdictMatch},
		{"cross-site surprise fails closed → deviation", p, []ObservedAlert{{Host: "gr-host99", Rule: "Down", Site: "dc2"}}, safety.VerdictDeviation},
		{"empty-site surprise fails closed → deviation", p, []ObservedAlert{{Host: "notrf01vps01", Rule: "Down", Site: ""}}, safety.VerdictDeviation},
		{"match + mismatch, no surprise → partial", p, []ObservedAlert{
			{Host: "db01", Rule: "HighLatency", Site: "dc1"},
			{Host: "cache01", Rule: "DiskFull", Site: "dc1"},
		}, safety.VerdictPartial},
		{"surprise dominates a co-occurring mismatch → deviation", p, []ObservedAlert{
			{Host: "cache01", Rule: "DiskFull", Site: "dc1"}, // mismatch (partial trigger)
			{Host: "router09", Rule: "BGPDown", Site: "dc1"}, // surprise (deviation trigger)
		}, safety.VerdictDeviation},
		{"duplicate surprise alerts → deviation", p, []ObservedAlert{
			{Host: "router09", Rule: "BGPDown", Site: "dc1"},
			{Host: "router09", Rule: "IfDown", Site: "dc1"},
		}, safety.VerdictDeviation},
	}
}

// TestComputeVerdictDetailEnumIsByteIdentical is the non-breaking guarantee (REQ-103a): for every case in the
// battery the typed detail's DERIVED enum equals (a) the pre-existing verdict this input has always produced
// and (b) the bare ComputeVerdict for the same input. The detail enriches; it never moves a verdict.
func TestComputeVerdictDetailEnumIsByteIdentical(t *testing.T) {
	for _, tc := range verdictBattery() {
		t.Run(tc.name, func(t *testing.T) {
			detail := ComputeVerdictDetail(tc.pred, tc.obs)
			if detail.Verdict != tc.want {
				t.Fatalf("detail enum = %q, want %q", detail.Verdict, tc.want)
			}
			if bare := ComputeVerdict(tc.pred, tc.obs); bare != detail.Verdict {
				t.Fatalf("bare ComputeVerdict %q != detail.Verdict %q (must be one authority)", bare, detail.Verdict)
			}
			if detail.AutoResolvable() != AutoResolvable(detail.Verdict) {
				t.Fatalf("detail.AutoResolvable disagreed with the free function for %q", detail.Verdict)
			}
		})
	}
}

// TestComputeVerdictDetailPopulatesBreakdown proves surprise hosts and rule mismatches are correctly
// collected, deduplicated, and sorted in ONE pass — even when a deviation dominates the derived verdict, the
// full partial-triggering mismatch breakdown is still surfaced (it is not short-circuited away).
func TestComputeVerdictDetailPopulatesBreakdown(t *testing.T) {
	p := pred()
	obs := []ObservedAlert{
		{Host: "db01", Rule: "HighLatency", Site: "dc1"}, // predicted host+rule → nothing
		{Host: "cache01", Rule: "DiskFull", Site: "dc1"}, // predicted host, unpredicted rule → mismatch
		{Host: "cache01", Rule: "DiskFull", Site: "dc1"}, // duplicate mismatch → deduped
		{Host: "db01", Rule: "DiskFull", Site: "dc1"},    // another mismatch on a predicted host
		{Host: "zeta01", Rule: "Down", Site: "dc1"},      // surprise host
		{Host: "alpha01", Rule: "Down", Site: "dc1"},     // surprise host (out of order → must sort)
		{Host: "alpha01", Rule: "IfDown", Site: "dc1"},   // same surprise host again → deduped
		{Host: "web01", Rule: "Down", Site: "dc1"},       // target host → excluded entirely
	}
	d := ComputeVerdictDetail(p, obs)
	if d.Verdict != safety.VerdictDeviation {
		t.Fatalf("a surprise host must derive a deviation, got %q", d.Verdict)
	}
	wantHosts := []string{"alpha01", "zeta01"} // sorted + deduped
	if !reflect.DeepEqual(d.SurpriseHosts, wantHosts) {
		t.Fatalf("surprise hosts = %v, want %v", d.SurpriseHosts, wantHosts)
	}
	wantMismatches := []RuleMismatch{{Host: "cache01", Rule: "DiskFull"}, {Host: "db01", Rule: "DiskFull"}} // sorted by (host,rule), deduped
	if !reflect.DeepEqual(d.Mismatches, wantMismatches) {
		t.Fatalf("mismatches = %v, want %v", d.Mismatches, wantMismatches)
	}
}

// TestComputeVerdictDetailQuietIsEmpty proves a clean post-state carries a match with NO surprise/mismatch
// noise (both breakdown slices nil), so a caller can trust that non-empty slices mean a real finding.
func TestComputeVerdictDetailQuietIsEmpty(t *testing.T) {
	d := ComputeVerdictDetail(pred(), []ObservedAlert{{Host: "db01", Rule: "HighLatency", Site: "dc1"}})
	if d.Verdict != safety.VerdictMatch || len(d.SurpriseHosts) != 0 || len(d.Mismatches) != 0 {
		t.Fatalf("a matched post-state must be a clean match with empty breakdown, got %+v", d)
	}
}

// TestComputeVerdictDetailDeviationForcesNeverAuto proves the safety floor is preserved through the typed
// path: a deviation detail is never auto-resolvable, while match/partial details remain auto-resolvable
// (REQ-104, INV-10).
func TestComputeVerdictDetailDeviationForcesNeverAuto(t *testing.T) {
	dev := ComputeVerdictDetail(pred(), []ObservedAlert{{Host: "router09", Rule: "BGPDown", Site: "dc1"}})
	if dev.Verdict != safety.VerdictDeviation {
		t.Fatalf("setup: expected deviation, got %q", dev.Verdict)
	}
	if dev.AutoResolvable() {
		t.Fatal("a deviation detail must never be auto-resolvable (REQ-104)")
	}
	match := ComputeVerdictDetail(pred(), nil)
	part := ComputeVerdictDetail(pred(), []ObservedAlert{{Host: "db01", Rule: "DiskFull", Site: "dc1"}})
	if !match.AutoResolvable() || !part.AutoResolvable() {
		t.Fatalf("match/partial details should be auto-resolvable, got match=%v partial=%v", match.AutoResolvable(), part.AutoResolvable())
	}
}
