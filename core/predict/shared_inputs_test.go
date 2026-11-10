package predict

// TG-681 — the predictor's SHARED inputs: the estate snapshot and the target NAME. These oracles pin what the
// predictor does when those inputs are stale or renamed, so the documented residual ("the predictor is not an
// independent source of truth about the graph or the identity") is a tested statement, not prose.
//
//   - A target whose name is not in the graph (renamed, decommissioned, never seeded) yields an EMPTY prediction
//     and an EMPTY control; upstream that is prediction-INELIGIBLE and the classifier polls, which the autonomous
//     fallback approver HOLDS (temporal/runner/shared_inputs_hold_test.go). Fail closed — already the case; pinned.
//   - A refresh outage keeps the prior snapshot (estate.Holder), and the predictor keeps predicting from it with
//     no signal of its own. The signal is the holder's age (core/estate/holder_age_test.go).
//   - The predictor resolves the target by NAME through the hybrid resolver; a separator variant resolves at the
//     FUZZY tier and is predicted over exactly as a confirmed identity would be. The cross-check that catches a
//     renamed guest before an effect is the TG-378 state precondition, which reads the guest-liveness projection
//     by the LITERAL proposal string — not by the resolved entity — so a name the projection does not carry is
//     unestablished and REFUSED. Pinned here as the identity contract the three components actually share.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/estate"
	"github.com/territory-grounder/grounder/core/proposal"
	"github.com/territory-grounder/grounder/core/safety"
)

func sharedInputsGraph() *estate.Graph {
	g := estate.NewGraph()
	g.Upsert(estate.Edge{From: estate.Entity{Type: estate.TypeLXC, Name: "web01"}, To: estate.Entity{Type: estate.TypePVENode, Name: "pve01"},
		Rel: estate.RelRunsOn, Confidence: 0.95, Source: estate.SourcePVE, ExpectedAlerts: []string{"HostDown"}})
	g.Upsert(estate.Edge{From: estate.Entity{Type: estate.TypeLXC, Name: "app01"}, To: estate.Entity{Type: estate.TypeLXC, Name: "web01"},
		Rel: estate.RelDependsOn, Confidence: 0.85, Source: estate.SourceDeclared, ExpectedAlerts: []string{"ServiceDown"}})
	return g
}

// eligibleBy mirrors the composition root's PredictionEligible closure (cmd/worker/main.go): eligible ⇔ the
// name resolves in the CURRENT snapshot. Mirrored rather than imported because main is not importable; the
// shape is one line and pinned here so a drift there is visible against this oracle.
func eligibleBy(h *estate.Holder) func(string) bool {
	return func(host string) bool { _, ok := h.Graph().Resolve(host); return ok }
}

func TestARenamedTargetPredictsNothingAndIsIneligible(t *testing.T) {
	h := estate.NewHolder(sharedInputsGraph())
	m := &InfragraphModel{EstateProvider: h.Graph, MaxDepth: 3}

	known := m.Predict("a", "p", "web01", "dc1", false)
	if _, ok := known.PredictedHosts["app01"]; !ok {
		t.Fatalf("fixture: web01's dependent must be predicted, got %v", known.PredictedHosts)
	}
	renamed := m.Predict("a", "p", "web01-renamed", "dc1", false)
	if len(renamed.PredictedHosts) != 0 || len(renamed.PredictedRules) != 0 {
		t.Fatalf("a target the graph cannot place must yield an EMPTY prediction, got %v", renamed.PredictedHosts)
	}
	if ctrl := m.controlHosts("p", "web01-renamed", 1, false); len(ctrl) != 0 {
		t.Fatalf("the control must mirror the empty prediction, got %v", ctrl)
	}
	if eligibleBy(h)("web01-renamed") {
		t.Fatal("a name the snapshot does not carry is prediction-INELIGIBLE — the classifier must poll")
	}
	if !eligibleBy(h)("web01") {
		t.Fatal("fixture: the known name is eligible")
	}
}

// The outage shape: every fallible source fails, the rebuild is empty, the holder keeps the prior snapshot. The
// predictor keeps answering from it — the SAME stale answer the agent seed and the verifier's graph reads get —
// and nothing in the prediction says so. The age does.
type failingSource struct{}

func (failingSource) Source() estate.Source { return estate.SourcePVE }
func (failingSource) Edges(context.Context) ([]estate.Edge, error) {
	return nil, errors.New("pve api down")
}

func TestAStaleSnapshotStillDrivesThePredictionUnmarked(t *testing.T) {
	clk := time.Date(2026, 11, 7, 20, 0, 0, 0, time.UTC)
	h := estate.NewHolderWithClock(sharedInputsGraph(), func() time.Time { return clk })
	m := &InfragraphModel{EstateProvider: h.Graph, MaxDepth: 3}

	clk = clk.Add(6 * time.Hour)
	kept, _ := h.Refresh(context.Background(), []estate.EdgeSource{failingSource{}})
	if !kept {
		t.Fatal("fixture: a failed, empty rebuild keeps the prior snapshot")
	}
	pred := m.Predict("a", "p", "web01", "dc1", false)
	if _, ok := pred.PredictedHosts["app01"]; !ok {
		t.Fatalf("the predictor answers from the kept snapshot — the shared stale answer — got %v", pred.PredictedHosts)
	}
	if eligibleBy(h)("web01") != true {
		t.Fatal("eligibility reads the same kept snapshot")
	}
	age, ok := h.Age()
	if !ok || age != 6*time.Hour {
		t.Fatalf("the only staleness signal is the holder's age, which must read 6h, got (%v, %v)", age, ok)
	}
}

// A separator variant of a known name resolves at the FUZZY tier and is predicted over as if confirmed. This is
// current behaviour and it is pinned as such: the predictor does NOT distinguish a guessed identity from a
// confirmed one. The reason that is tolerable for an effect is the next oracle.
func TestAFuzzyNameIsPredictedOverAsIfConfirmed(t *testing.T) {
	g := sharedInputsGraph()
	if _, tier, ok := g.ResolveTiered("web-01"); !ok || tier != estate.TierFuzzy {
		t.Fatalf("fixture: web-01 must resolve at the fuzzy tier, got tier=%q ok=%v", tier, ok)
	}
	m := &InfragraphModel{Estate: g, MaxDepth: 3}
	exact := m.Predict("a", "p", "web01", "dc1", false)
	fuzzy := m.Predict("a", "p", "web-01", "dc1", false)
	if len(fuzzy.PredictedHosts) != len(exact.PredictedHosts) {
		t.Fatalf("a fuzzy-resolved target is predicted over like the exact one (current contract), exact=%v fuzzy=%v", exact.PredictedHosts, fuzzy.PredictedHosts)
	}
	if fuzzy.TargetHost != "web-01" {
		t.Fatalf("the prediction carries the LITERAL target string, not the resolved entity, got %q", fuzzy.TargetHost)
	}
}

// THE IDENTITY CROSS-CHECK THAT EXISTS. For a state-preconditioned class the gate asks the guest-liveness reader
// about the LITERAL proposal string; the production reader is keyed by the exact guest name the PVE sweep wrote
// (cmd/worker/guest_liveness.go). A renamed or separator-variant guest therefore has no fresh row, the reader
// answers ok=false, and the seal is REFUSED with no committed prediction left behind. That — not the graph — is
// what stands between a name mismatch and an effect on the wrong guest.
func TestTheStatePreconditionAsksByTheLiteralNameAndRefusesAnUnknownOne(t *testing.T) {
	var asked []string
	projection := map[string]bool{"web01": false} // the sweep wrote web01 as stopped; nothing else exists
	reader := func(_ context.Context, guest string) (bool, string, bool) {
		asked = append(asked, guest)
		running, ok := projection[guest]
		return running, "guest_liveness test projection", ok
	}
	g := &PredictionGate{Store: NewMemPredictionStore(), Model: &InfragraphModel{Estate: sharedInputsGraph(), MaxDepth: 3}, Mode: ModeEnforce, GuestRunning: reader}
	ctx := context.Background()

	p, err := proposal.ParseProposal([]byte(`{"external_ref":"TG-681","target":"web-01","op_class":"start-guest","op":"start","params":{"guest":"web-01"},"confidence":0.9}`))
	if err != nil {
		t.Fatal(err)
	}
	_, err = g.Commit(ctx, p, "plan-681-fuzzy", "dc1", safety.BandPollPause, false)
	if !errors.Is(err, ErrPreconditionUnestablished) {
		t.Fatalf("a guest name the liveness projection does not carry must REFUSE to seal (unknown is not not-running), got %v", err)
	}
	if len(asked) != 1 || asked[0] != "web-01" {
		t.Fatalf("the gate must ask the projection by the LITERAL proposal string, asked %v", asked)
	}
	if has, _ := g.Store.Has(ctx, "plan-681-fuzzy"); has {
		t.Fatal("a refused seal must leave no committed prediction")
	}

	ok, err := proposal.ParseProposal([]byte(`{"external_ref":"TG-681","target":"web01","op_class":"start-guest","op":"start","params":{"guest":"web01"},"confidence":0.9}`))
	if err != nil {
		t.Fatal(err)
	}
	gp, err := g.Commit(ctx, ok, "plan-681-exact", "dc1", safety.BandPollPause, false)
	if err != nil {
		t.Fatalf("the exact name the projection carries, observed stopped, must seal: %v", err)
	}
	if _, named := gp.Prediction().PredictedHosts["app01"]; !named {
		t.Fatalf("the sealed prediction carries the graph's dependents, got %v", gp.Prediction().PredictedHosts)
	}
}
