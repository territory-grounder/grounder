package predict

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/territory-grounder/grounder/core/estate"
	"github.com/territory-grounder/grounder/core/proposal"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/verify"
)

func testGate(mode Mode) *PredictionGate {
	g := NewDependencyGraph(map[string][]string{
		"web01":   {"db01", "cache01"},
		"db01":    {"reports01"},
		"router0": {"web01"},
	})
	return &PredictionGate{
		Store: NewMemPredictionStore(),
		Model: &InfragraphModel{Graph: g, DefaultRules: []string{"HighLatency"}, MaxDepth: 3},
		Mode:  mode,
	}
}

func testProposal() proposal.Proposal {
	p, err := proposal.ParseProposal([]byte(`{"external_ref":"TG-1","target":"web01","op_class":"restart-service","op":"restart","confidence":0.8}`))
	if err != nil {
		panic(err)
	}
	return p
}

func TestCommitPersistsPredictionBeforePoll(t *testing.T) {
	g := testGate(ModeEnforce)
	ctx := context.Background()

	// before commit, no prediction exists
	if has, _ := g.Store.Has(ctx, "plan-1"); has {
		t.Fatal("no prediction should exist before commit")
	}
	gp, err := g.Commit(ctx, testProposal(), "plan-1", "dc1", safety.BandPollPause, true)
	if err != nil {
		t.Fatal(err)
	}
	// prediction is committed BEFORE any approval poll (REQ-101)
	if has, _ := g.Store.Has(ctx, "plan-1"); !has {
		t.Fatal("prediction must be committed by Commit")
	}
	if !gp.Gated() {
		t.Fatal("Commit must produce a gated proposal")
	}
	// blast radius named the dependents
	if _, ok := gp.Prediction().PredictedHosts["db01"]; !ok {
		t.Fatalf("prediction should include the blast radius: %+v", gp.Prediction().PredictedHosts)
	}
	// shuffled control present (falsifiability by construction)
	rec, _, _ := g.Store.Get(ctx, "plan-1")
	if rec.ControlHosts == nil {
		t.Fatal("every prediction row must carry a shuffled-graph control")
	}

	poll, err := BuildApprovalPoll(gp, g.Mode)
	if err != nil {
		t.Fatal(err)
	}
	if !poll.Blocking || poll.ActionID != gp.Manifest().ActionID {
		t.Fatalf("enforce-mode poll must block and bind the action_id: %+v", poll)
	}
}

func TestBuildApprovalPollDefaultDeniesUngated(t *testing.T) {
	// a zero-value / ungated proposal can never build a poll — default-deny (REQ-102).
	if _, err := BuildApprovalPoll(GatedProposal{}, ModeEnforce); !errors.Is(err, ErrNotGated) {
		t.Fatalf("an ungated proposal must be denied, got %v", err)
	}
}

func TestAnalysisOnlyRecordsButDoesNotBlock(t *testing.T) {
	g := testGate(ModeAnalysisOnly)
	ctx := context.Background()
	gp, err := g.Commit(ctx, testProposal(), "plan-1", "dc1", safety.BandPollPause, true)
	if err != nil {
		t.Fatal(err)
	}
	// the prediction is still recorded (REQ-105)
	if has, _ := g.Store.Has(ctx, "plan-1"); !has {
		t.Fatal("analysis-only must still commit the prediction")
	}
	poll, err := BuildApprovalPoll(gp, g.Mode)
	if err != nil {
		t.Fatal(err)
	}
	if poll.Blocking {
		t.Fatal("analysis-only poll must be non-blocking (fail-open advisory)")
	}
}

func TestActionIDThreadedAndReGateOnChange(t *testing.T) {
	g := testGate(ModeEnforce)
	ctx := context.Background()
	gp, err := g.Commit(ctx, testProposal(), "plan-1", "dc1", safety.BandPollPause, true)
	if err != nil {
		t.Fatal(err)
	}
	m := gp.Manifest()
	// each stage re-derives and asserts the same action_id
	if err := m.Assert(m.ActionID); err != nil {
		t.Fatalf("action_id must thread unchanged through the stages: %v", err)
	}
	// a mid-session action change mints a NEW id that fails the prior authorization
	changed := gp.Proposal().Action
	changed.Op = "reload"
	newID, err := changed.ID()
	if err != nil {
		t.Fatal(err)
	}
	if newID == m.ActionID {
		t.Fatal("a changed action must mint a new action_id (re-gate)")
	}
	if err := m.Assert(newID); err == nil {
		t.Fatal("asserting the manifest against the changed id must fail closed (forces re-gate)")
	}
}

// When an estate graph is wired, Predict draws its cascade from the real path-product blast radius + the
// target's common-cause siblings, using each edge's expected alerts — not a flat adjacency map.
func TestPredictUsesEstateBlastRadiusAndSiblings(t *testing.T) {
	g := estate.NewGraph()
	// three guests run on pve01; predicting an action ON pve01 → all three are in its blast radius.
	for _, guest := range []string{"n8n01", "litellm01", "grafana01"} {
		g.Upsert(estate.Edge{
			From: estate.Entity{Type: estate.TypeLXC, Name: guest}, To: estate.Entity{Type: estate.TypePVENode, Name: "dc1pve01"},
			Rel: estate.RelRunsOn, Confidence: 0.95, Source: estate.SourcePVE, ExpectedAlerts: []string{"HostDown"},
		})
	}
	m := &InfragraphModel{Estate: g, DefaultRules: []string{"HighLatency"}, MaxDepth: 3}
	pred := m.Predict("a1", "p1", "dc1pve01", "nl", true)
	for _, guest := range []string{"n8n01", "litellm01", "grafana01"} {
		if _, ok := pred.PredictedHosts[guest]; !ok {
			t.Errorf("guest %s must be in the blast radius of its pve node", guest)
		}
		// the per-edge expected alert (HostDown), not the DefaultRules, must be used
		if _, ok := pred.PredictedRules[verify.RuleKey(guest, "HostDown")]; !ok {
			t.Errorf("guest %s must carry its edge's expected alert HostDown", guest)
		}
	}
	// an unresolvable target yields an empty prediction (fail-closed eligibility).
	empty := m.Predict("a2", "p2", "unknown-host-xyz", "nl", true)
	if len(empty.PredictedHosts) != 0 {
		t.Fatalf("an unresolvable target must yield an empty prediction, got %d hosts", len(empty.PredictedHosts))
	}
}

// TG-61 blast-radius calibration: a target's common-cause SIBLINGS are predicted only for an availability/
// connectivity incident. Reproduces the exact box topology (three LXC guests co-hosted on one pve node) that
// over-predicted a ~130-host phantom cascade (real_fp≈130, tp=0) for a guest-LOCAL disk alert and floored the
// falsifiability signal. The gate must drop those siblings for a local fault while leaving the hypervisor's
// own genuine blast radius (the one box row with tp=1) untouched.
func TestPredictGatesSiblingsOnCommonCause(t *testing.T) {
	g := estate.NewGraph()
	guests := []string{"atlantis01", "librespeed01", "actualbudget01"}
	for _, guest := range guests {
		g.Upsert(estate.Edge{
			From: estate.Entity{Type: estate.TypeLXC, Name: guest}, To: estate.Entity{Type: estate.TypePVENode, Name: "pve01"},
			Rel: estate.RelRunsOn, Confidence: 0.95, Source: estate.SourcePVE, ExpectedAlerts: []string{"HostDown"},
		})
	}
	m := &InfragraphModel{Estate: g, DefaultRules: []string{"HighLatency"}, MaxDepth: 3}

	// LOCAL fault on a leaf guest (commonCause=false): it has no dependents and siblings are gated OFF, so the
	// prediction is empty — no phantom co-tenant cascade.
	local := m.Predict("a", "storage_perc", "atlantis01", "nl", false)
	if len(local.PredictedHosts) != 0 {
		t.Fatalf("a leaf guest's LOCAL fault must predict no siblings, got %v", local.PredictedHosts)
	}
	// AVAILABILITY fault on the same guest (commonCause=true): the shared parent could be the cause, so the
	// co-hosted siblings ARE predicted.
	avail := m.Predict("a", "DeviceDown", "atlantis01", "nl", true)
	for _, sib := range []string{"librespeed01", "actualbudget01"} {
		if _, ok := avail.PredictedHosts[sib]; !ok {
			t.Errorf("an availability fault must predict co-hosted sibling %s, got %v", sib, avail.PredictedHosts)
		}
	}
	// The HYPERVISOR's blast radius (its guests are DIRECT dependents, not siblings) survives the gate even for
	// a local-classed alert — the one genuine cascade signal is preserved.
	hyp := m.Predict("a", "storage_perc", "pve01", "nl", false)
	for _, guest := range guests {
		if _, ok := hyp.PredictedHosts[guest]; !ok {
			t.Errorf("hypervisor blast radius must include guest %s regardless of common-cause", guest)
		}
	}
	// The negative control mirrors the gate: a leaf guest's local fault yields an empty control too, so real
	// and control stay the same shape (no rigged asymmetry).
	if c := m.controlHosts("p", "atlantis01", 0, false); len(c) != 0 {
		t.Fatalf("control for a leaf-guest local fault must be empty (mirrors real), got %v", c)
	}
}

// SiblingsEligible must fire ONLY for whole-host availability/connectivity faults, and must NOT be fooled by a
// service- or link-scoped rule that merely contains "down".
func TestSiblingsEligibleClassifier(t *testing.T) {
	for _, r := range []string{"Device Down", "device-down", "host_unreachable", "ICMP unreachable", "InstanceDown", "node down", "ProbeFailure", "not responding", "server offline", "no response"} {
		if !SiblingsEligible(r) {
			t.Errorf("availability rule %q must be common-cause eligible", r)
		}
	}
	for _, r := range []string{"", "storage_perc", "disk / 92%", "High Memory", "processor load", "service nginx down", "BGP session down", "interface down", "HighLatency", "certificate expiry"} {
		if SiblingsEligible(r) {
			t.Errorf("local/service rule %q must NOT be common-cause eligible", r)
		}
	}
}

// The negative-control scorer makes INV-22 real: a prediction that beats its degree-preserving control is
// falsifiable-valid; one whose control does as well is not. P1-13.
func TestScoreControlFalsifiability(t *testing.T) {
	// real prediction names {db01, cache01}; the control names {web09, web10} — same count, wrong hosts.
	rec := PredictionRecord{
		Prediction: verify.Prediction{
			TargetHost: "app01", Site: "nl",
			PredictedHosts: map[string]struct{}{"db01": {}, "cache01": {}},
		},
		ControlHosts: map[string]struct{}{"web09": {}, "web10": {}},
	}
	observed := []verify.ObservedAlert{
		{Host: "app01", Rule: "Reboot", Site: "nl"},      // target host — excluded (expected direct effect)
		{Host: "elsewhere", Rule: "X", Site: "gr"},       // cross-site — excluded as background noise
		{Host: "db01", Rule: "HighLatency", Site: "nl"},  // a true cascade the real prediction foresaw
		{Host: "cache01", Rule: "HighLatency", Site: "nl"},
	}
	cs := ScoreControl(rec, observed)
	if cs.RealTP != 2 || cs.ControlTP != 0 || cs.ControlFP != 2 {
		t.Fatalf("real must catch both, control none (FP=2): %+v", cs)
	}
	if !cs.Falsifiable() || cs.Ratio() != 0 {
		t.Fatalf("a real prediction that beats its control is falsifiable-valid, ratio=%.2f", cs.Ratio())
	}
	// a VACUOUS prediction whose control catches as much as it does → ratio ≥ ceiling → NOT falsifiable.
	vac := PredictionRecord{
		Prediction:   verify.Prediction{TargetHost: "app01", Site: "nl", PredictedHosts: map[string]struct{}{"db01": {}}},
		ControlHosts: map[string]struct{}{"db01": {}, "cache01": {}},
	}
	cs2 := ScoreControl(vac, observed)
	if cs2.RealTP != 1 || cs2.ControlTP != 2 {
		t.Fatalf("vacuous: real catches db01 (1), control catches db01+cache01 (2): %+v", cs2)
	}
	if cs2.Falsifiable() {
		t.Fatalf("a control matching the real hits must fail falsifiability, ratio=%.2f", cs2.Ratio())
	}
}

// A committed prediction over a wired estate draws its control from estate.ShuffledControl (degree-
// preserving), not the flat graph — the control host count matches the real prediction's blast shape.
func TestCommitControlUsesEstateDegreePreserving(t *testing.T) {
	g := estate.NewGraph()
	for _, guest := range []string{"n8n01", "litellm01", "grafana01"} {
		g.Upsert(estate.Edge{
			From: estate.Entity{Type: estate.TypeLXC, Name: guest}, To: estate.Entity{Type: estate.TypePVENode, Name: "dc1pve01"},
			Rel: estate.RelRunsOn, Confidence: 0.95, Source: estate.SourcePVE, ExpectedAlerts: []string{"HostDown"},
		})
	}
	m := &InfragraphModel{Estate: g, DefaultRules: []string{"HighLatency"}, MaxDepth: 3}
	ctrl := m.controlHosts("plan-x", "dc1pve01", 3, true)
	if len(ctrl) != 3 {
		t.Fatalf("the degree-preserving control must name the same NUMBER of hosts as the blast radius, got %d", len(ctrl))
	}
	// an unresolvable target ⇒ empty control (mirrors the empty prediction).
	if len(m.controlHosts("plan-y", "unknown-xyz", 3, true)) != 0 {
		t.Fatal("an unresolvable target must yield an empty control")
	}
}

// EstateProvider makes the model read the CURRENT graph, so a runtime refresh (swapping the holder's graph)
// changes predictions without rebuilding the gate.
func TestInfragraphModelEstateProviderRefreshes(t *testing.T) {
	g1 := estate.NewGraph()
	g1.Upsert(estate.Edge{From: estate.Entity{Type: estate.TypeLXC, Name: "guestA"}, To: estate.Entity{Type: estate.TypePVENode, Name: "pve01"}, Rel: estate.RelRunsOn, Confidence: 0.95, Source: estate.SourcePVE})
	holder := estate.NewHolder(g1)
	m := &InfragraphModel{EstateProvider: holder.Graph, MaxDepth: 3}

	if _, ok := m.Predict("a", "p", "pve01", "nl", true).PredictedHosts["guestA"]; !ok {
		t.Fatal("prediction must see guestA from the initial graph")
	}
	// refresh: a new graph where pve01 hosts guestB instead.
	g2 := estate.NewGraph()
	g2.Upsert(estate.Edge{From: estate.Entity{Type: estate.TypeLXC, Name: "guestB"}, To: estate.Entity{Type: estate.TypePVENode, Name: "pve01"}, Rel: estate.RelRunsOn, Confidence: 0.95, Source: estate.SourcePVE})
	holder.Set(g2)
	pred := m.Predict("a", "p", "pve01", "nl", true)
	if _, ok := pred.PredictedHosts["guestB"]; !ok {
		t.Fatal("after refresh, prediction must see guestB (the model read the CURRENT graph)")
	}
	if _, ok := pred.PredictedHosts["guestA"]; ok {
		t.Fatal("the stale guestA must be gone after the refresh")
	}
}

// ADVERSARIAL (INV-22, concurrent inputs): the prediction store is shared across the worker's concurrent gate
// activities. Concurrent Commit/Has/Get must be race-free and keep first-wins append-only semantics.
func TestMemPredictionStoreConcurrent(t *testing.T) {
	s := NewMemPredictionStore()
	const n = 200
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(k int) {
			defer wg.Done()
			ph := "plan-" + string(rune('a'+k%26))
			_ = s.Commit(context.Background(), PredictionRecord{Prediction: verify.Prediction{PlanHash: ph}})
			_, _ = s.Has(context.Background(), ph)
			_, _, _ = s.Get(context.Background(), ph)
		}(i)
	}
	wg.Wait()
	// exactly the 26 distinct plan hashes are committed (first-wins dedup held under concurrency).
	got := 0
	for c := 'a'; c <= 'z'; c++ {
		if ok, _ := s.Has(context.Background(), "plan-"+string(c)); ok {
			got++
		}
	}
	if got != 26 {
		t.Fatalf("first-wins dedup must hold under concurrency: %d distinct, want 26", got)
	}
}

// ADVERSARIAL (INV-22, partial inputs): a misconfigured model with NEITHER an estate graph nor a flat graph
// must FAIL CLOSED to an empty prediction — never a nil-graph panic.
func TestPredictFailsClosedOnNilGraphs(t *testing.T) {
	m := &InfragraphModel{} // no Estate, no EstateProvider, no Graph
	p := m.Predict("a", "ph", "web01", "nl", true)
	if len(p.PredictedHosts) != 0 || len(p.PredictedRules) != 0 {
		t.Fatalf("a graph-less model must yield an empty prediction, got %+v", p)
	}
	// the control + scorer are likewise nil-safe.
	if got := m.controlHosts("ph", "web01", 0, true); len(got) != 0 {
		t.Fatalf("controlHosts on a nil graph must be empty, got %+v", got)
	}
	if cs := ScoreControl(PredictionRecord{}, nil); cs != (ControlScore{}) {
		t.Fatalf("ScoreControl on an empty record must be zero, got %+v", cs)
	}
}
