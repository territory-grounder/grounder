package opclasscluster

// THE CRON MUST REPORT WHAT IT IS HOLDING, NOT JUST THAT IT PROMOTED NOTHING (TG-348).
//
// The pass that promotes nothing is exactly the pass whose reasoning an operator needs, and it is the one
// the existing summary line stays silent for — it logs only when ToCandidate/ToRatifyReady/Expired are
// non-zero. Measured 2026-08-06: all 8 candidates on the deployed estate sat at `observing` for days, and
// the cron said nothing about why on any pass.
//
// Removing the per-candidate report leaves every other test in this package green, which is why this one
// exists: the reporting is the deliverable, and a deliverable with no oracle is the shape this repo keeps
// paying for.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/opclasscat"
)

// A held candidate must arrive in the Result with the legs it fails — the caller cannot report what the
// pass did not record.
func TestAHeldCandidateCarriesItsGapsInTheResult(t *testing.T) {
	var res Result
	gaps := opclasscat.RatifyReadyGaps(opclasscat.Evidence{DistinctRefs: 1}, opclasscat.ReadyInput{})
	if len(gaps) == 0 {
		t.Fatal("the fixture produced no gaps, so this test asserts nothing about reporting them")
	}
	res.Held = append(res.Held, HeldCandidate{Key: "k-1", OpClass: "restart-snmp-agent", Gaps: gaps})

	if len(res.Held) != 1 {
		t.Fatalf("Result carries %d held candidate(s), want 1", len(res.Held))
	}
	h := res.Held[0]
	if h.OpClass == "" || h.Key == "" {
		t.Error("a held candidate with no op_class or key cannot be acted on — the operator cannot find it")
	}
	if len(h.Gaps) == 0 {
		t.Error("a held candidate carrying NO gaps is the state this exists to end: it says the candidate " +
			"is held and not why, which is what `observing` already said")
	}
	// The gaps must be human-actionable text, not a code.
	joined := strings.Join(h.Gaps, " ")
	if !strings.Contains(joined, "distinct_refs") && !strings.Contains(joined, "blast_radius") {
		t.Errorf("the gaps name no recognisable leg: %q", joined)
	}
}

// The Result type must HAVE the field. A pass that computes the gaps and drops them on the floor reports
// nothing, and the caller has nowhere to read them from.
func TestResultDeclaresAHeldField(t *testing.T) {
	res := Result{Held: []HeldCandidate{{Key: "k", OpClass: "c", Gaps: []string{"g"}}}}
	if res.Held[0].Gaps[0] != "g" {
		t.Fatal("Result.Held does not round-trip its gaps")
	}
	// Vacuity floor: an empty pass must produce an empty Held, not a nil-vs-empty distinction anyone
	// depends on.
	var empty Result
	if len(empty.Held) != 0 {
		t.Fatalf("a zero Result reports %d held candidate(s)", len(empty.Held))
	}
}

// THE REAL PASS MUST RECORD THE REASON, not merely compute it. Driving Job.Run with a candidate the gate
// holds: without this, computing the gaps and dropping them on the floor leaves every other test in this
// package green — which is exactly the mutation that survived when this file only exercised the type.
func TestTheRealPassRecordsWhyItHeldACandidate(t *testing.T) {
	now := fixedNow()
	var occs []opclasscat.Occurrence
	for i, h := range []string{"a", "b", "c", "d", "e"} {
		occs = append(occs, occ("r"+h, "host"+h, 0.9, now.Add(-time.Duration(i+1)*time.Hour)))
	}
	st, lg := seed(occs...)
	st.live[0].Status = opclasscat.StatusCandidate

	// Ready nil ⇒ no blast-radius walk ⇒ coverage 0 ⇒ the gate holds it. This is the DEPLOYED state:
	// measured 2026-08-06, all 8 candidates sat at `observing` with no provider wired and nothing said so.
	j := Job{Store: st, Ledger: lg, Liveness: healthyLiveness, Now: fixedNow}
	res, err := j.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.ToRatifyReady != 0 {
		t.Fatal("the fixture advanced a candidate, so it is not exercising the HELD path at all")
	}
	if len(res.Held) == 0 {
		t.Fatal("the pass held a candidate and recorded NO reason. An operator reading '0 ratify-ready' " +
			"then cannot tell a candidate two incidents short from one whose blast-radius provider was " +
			"never wired — and only the second will never resolve on its own.")
	}
	// The nil-resolver branch names the RESOLVER, not the generic coverage leg — deliberately, because
	// that branch can never resolve on its own and the operator needs the cause, not the symptom.
	joined := strings.ToLower(strings.Join(res.Held[0].Gaps, " "))
	if !strings.Contains(joined, "blast-radius") && !strings.Contains(joined, "blast_radius") {
		t.Errorf("the recorded reason does not name the failing leg (%q); with no resolver the cause is "+
			"the absent blast-radius walk and the report should say so", joined)
	}
	if !strings.Contains(joined, "resolver") {
		t.Errorf("the nil-resolver branch reports a generic coverage shortfall (%q) rather than naming "+
			"the missing resolver. Those need different responses: a shortfall resolves with more "+
			"incidents, an absent resolver never does.", joined)
	}
	if res.Held[0].OpClass == "" {
		t.Error("the held candidate carries no op_class — the operator cannot find which one it is")
	}
}
