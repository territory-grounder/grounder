package actuate

import (
	"context"
	"testing"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/estate"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/verify"
)

// The estate-derived coincidental-cross-site filter (spec/002 REQ-107) on the REAL execute path: the live
// defect it repairs is the interceptor's — governance_ledger seq 6555 records a 59-second sensor flap at the
// OTHER site scoring execute:deviation, demoting restart-container auto→approve and discarding ~80 hands-off
// clean runs. These oracles run the full interceptor chain (admission → gates → execute → verify) with a
// LIVE estate.Graph as the SiteAuthority — not a stub map — so the wiring from Request.HostSite through the
// scoped verdict author is what is under test.

// twoSiteGraph declares both hosts' site membership: the target is at nl, the flapping sensor host at gr.
func twoSiteGraph() *estate.Graph {
	g := estate.NewGraph()
	g.Upsert(estate.Edge{
		From: estate.Entity{Type: estate.TypeHost, Name: "web01"},
		To:   estate.Entity{Type: estate.TypeSite, Name: "nl-site"},
		Rel:  estate.RelMemberOf, Source: estate.SourceDeclared, Confidence: 0.85,
	})
	g.Upsert(estate.Edge{
		From: estate.Entity{Type: estate.TypeHost, Name: "dc2lte01"},
		To:   estate.Entity{Type: estate.TypeSite, Name: "gr-site"},
		Rel:  estate.RelMemberOf, Source: estate.SourceDeclared, Confidence: 0.85,
	})
	// web02 shares the target's site — a PROVEN same-site host, so the filter provably declines to exclude it
	// (rather than web02 merely being site-unknown).
	g.Upsert(estate.Edge{
		From: estate.Entity{Type: estate.TypeHost, Name: "web02"},
		To:   estate.Entity{Type: estate.TypeSite, Name: "nl-site"},
		Rel:  estate.RelMemberOf, Source: estate.SourceDeclared, Confidence: 0.85,
	})
	return g
}

// A NEW post-execution alert on a host the estate can PROVE is at the other site verifies as MATCH — the flap
// is coincidental background, not this action's cascade — so the op-class is not demoted and the breaker not
// armed by it.
func TestCrossSiteFlapDoesNotDeviateOnTheExecutePath(t *testing.T) {
	g := twoSiteGraph()
	r := goodRequest(t)
	r.Prediction = verify.Prediction{
		ActionID: r.Manifest.ActionID, TargetHost: "web01", Site: "nl",
		PredictedHosts: map[string]struct{}{"web01": {}},
	}
	r.HostSite = g.SiteOf
	// The flap APPEARS only after execution (so the TG-148 pair baseline does NOT remove it — the site filter
	// is the only thing standing between this observation and a deviation).
	call := 0
	r.Observe = func(context.Context) ([]verify.ObservedAlert, bool) {
		call++
		if call == 1 {
			return []verify.ObservedAlert{}, true // pre-execute baseline: quiet
		}
		return []verify.ObservedAlert{{Host: "dc2lte01", Rule: "Sensor under limit - Check Device Health Settings", Site: "gr"}}, true
	}
	out, err := actuatingInterceptor(safety.NewActuatingChokepoint(), &fakeActuator{}, audit.NewLedger()).Do(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Executed || out.Verdict != safety.VerdictMatch {
		t.Fatalf("a proven-other-site flap must verify MATCH on the execute path (the seq-6555 demotion shape), got %+v", out)
	}
}

// The fail-closed floor is untouched: the SAME observation with a site-UNKNOWN host (the estate makes no
// claim for it) still deviates, and so does a same-site surprise. Cross-site exclusion never widens past what
// the estate can prove.
func TestUnknownAndSameSiteSurprisesStillDeviateOnTheExecutePath(t *testing.T) {
	g := twoSiteGraph()
	for name, alert := range map[string]verify.ObservedAlert{
		"unknown-site host": {Host: "notrf01vps01", Rule: "HostDown", Site: "gr"},
		"same-site host":    {Host: "web02", Rule: "HostDown", Site: "nl"},
	} {
		r := goodRequest(t)
		r.Prediction = verify.Prediction{
			ActionID: r.Manifest.ActionID, TargetHost: "web01", Site: "nl",
			PredictedHosts: map[string]struct{}{"web01": {}},
		}
		r.HostSite = g.SiteOf
		call := 0
		a := alert
		r.Observe = func(context.Context) ([]verify.ObservedAlert, bool) {
			call++
			if call == 1 {
				return []verify.ObservedAlert{}, true
			}
			return []verify.ObservedAlert{a}, true
		}
		out, err := actuatingInterceptor(safety.NewActuatingChokepoint(), &fakeActuator{}, audit.NewLedger()).Do(context.Background(), r)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if out.Verdict != safety.VerdictDeviation {
			t.Fatalf("%s: a surprise the estate cannot prove elsewhere must still DEVIATE, got %+v", name, out)
		}
	}
}
