package estate

import (
	"context"
	"testing"
	"time"
)

// TG-206a: a decay-on-disproof pass must ATTACH THE CONTRADICTION TO THE EDGE — a durable, attributable disproof
// record (which edge, disproved by which misprediction, decayed to what, aged out?) — instead of lowering a
// number and discarding the DecayReport. Killing mutation: stop populating DecayReport.Disproofs → RED.
func TestDecayReportAttachesTheContradictionToTheEdge(t *testing.T) {
	g := NewGraph()
	g.Upsert(learned("pve01", "web7", 0.4))  // ON the mispredicted path (0.4*0.5=0.2 — decays, not aged)
	g.Upsert(learned("web7", "cache2", 0.8)) // touches a surprise host but NOT this path — no disproof record

	at := time.Now()
	_, rep := g.DecayOnDisproof(Disproof{
		Paths: []DisproofPath{{
			Target: "pve01", Surprised: []string{"web7"},
			DeviationKey: "pve01|nl|web7", ActionID: "act-abc",
		}},
		At: at,
	}, DecayOptions{Factor: 0.5})

	if rep.Decayed != 1 {
		t.Fatalf("decayed %d edge(s), want exactly 1 (only pve01->web7 on the path)", rep.Decayed)
	}
	if len(rep.Disproofs) != 1 {
		t.Fatalf("Disproofs has %d record(s), want exactly 1 — the decayed edge must leave an attributable "+
			"disproof, not just a lowered number (TG-206a)", len(rep.Disproofs))
	}
	d := rep.Disproofs[0]
	if d.DeviationKey != "pve01|nl|web7" || d.ActionID != "act-abc" {
		t.Errorf("disproof not attributed to the misprediction: deviationKey=%q actionID=%q, want pve01|nl|web7 / act-abc "+
			"— an unattributed disproof cannot be vindicated or refuted later", d.DeviationKey, d.ActionID)
	}
	if d.Target != "pve01" {
		t.Errorf("disproof target = %q, want pve01 (the host the prediction was made FROM)", d.Target)
	}
	if d.From != "pve01" || d.To != "web7" {
		t.Errorf("disproof edge = %q->%q, want pve01->web7", d.From, d.To)
	}
	if d.DecayedTo <= 0 || d.DecayedTo >= 0.4 {
		t.Errorf("decayedTo = %v, want 0 < x < 0.4 (0.4 decayed by factor 0.5)", d.DecayedTo)
	}
	if d.AgedOut {
		t.Errorf("edge decayed to %v (> floor 0), so it must NOT be recorded as aged out", d.DecayedTo)
	}
}

// An aged-out edge records aged_out=true, and the disproof round-trips through the store twin with its
// attribution and pass time intact.
func TestDisproofRecordsAgedOutAndRoundTripsThroughStore(t *testing.T) {
	g := NewGraph()
	g.Upsert(learned("pve01", "web7", 0.4))
	at := time.Now()
	// Floor 0.3 > decayed 0.2 → the edge ages out this pass.
	_, rep := g.DecayOnDisproof(Disproof{
		Paths: []DisproofPath{{Target: "pve01", Surprised: []string{"web7"}, DeviationKey: "k1", ActionID: "a1"}},
		At:    at,
	}, DecayOptions{Factor: 0.5, Floor: 0.3})
	if len(rep.Disproofs) != 1 || !rep.Disproofs[0].AgedOut {
		t.Fatalf("expected 1 aged-out disproof, got %+v", rep.Disproofs)
	}

	store := NewMemEdgeDisproofStore()
	n, err := store.Record(context.Background(), at, rep.Disproofs)
	if err != nil || n != 1 {
		t.Fatalf("record: n=%d err=%v", n, err)
	}
	got, err := store.List(context.Background())
	if err != nil || len(got) != 1 {
		t.Fatalf("list: %d rows err=%v", len(got), err)
	}
	if got[0].DeviationKey != "k1" || got[0].ActionID != "a1" || !got[0].AgedOut || !got[0].ObservedAt.Equal(at) {
		t.Errorf("round-trip lost attribution/aged/at: %+v", got[0])
	}
}

// The scoped-path outcome is unchanged for a path that carries attribution: an off-path edge is neither decayed
// nor recorded as disproved (attribution must not widen the blast radius).
func TestAttributionDoesNotWidenTheDecay(t *testing.T) {
	g := NewGraph()
	g.Upsert(learned("pve01", "web7", 0.8))
	g.Upsert(learned("web7", "cache2", 0.8)) // off the path

	_, rep := g.DecayOnDisproof(Disproof{
		Paths: []DisproofPath{{Target: "pve01", Surprised: []string{"web7"}, DeviationKey: "k", ActionID: "a"}},
		At:    time.Now(),
	}, DecayOptions{Factor: 0.5})

	if rep.Decayed != 1 || len(rep.Disproofs) != 1 {
		t.Fatalf("decayed=%d disproofs=%d, want 1/1 — attribution must not widen the mispredicted-path scope", rep.Decayed, len(rep.Disproofs))
	}
	if rep.Disproofs[0].To == "cache2" || rep.Disproofs[0].From == "cache2" {
		t.Errorf("off-path edge web7->cache2 was recorded as disproved: %+v", rep.Disproofs[0])
	}
}

// When TWO mispredictions implicate the SAME edge (same undirected pair), the edge decays exactly ONCE and its
// single disproof is attributed to the FIRST path deterministically (the pair map is first-seen-wins over the
// caller's fixed path order). A later verdict then knows which reading to vindicate/refute — an ambiguous or
// order-dependent attribution would make the record untrustworthy.
func TestSharedPairAttributesToTheFirstPathAndDecaysOnce(t *testing.T) {
	g := NewGraph()
	g.Upsert(learned("pve01", "web7", 0.8)) // one edge implicated by BOTH captures below

	// Two captures from the same target implicating the same surprise host, ordered so "aaa" sorts first.
	_, rep := g.DecayOnDisproof(Disproof{
		Paths: []DisproofPath{
			{Target: "pve01", Surprised: []string{"web7"}, DeviationKey: "aaa-first", ActionID: "act-A"},
			{Target: "pve01", Surprised: []string{"web7"}, DeviationKey: "zzz-second", ActionID: "act-Z"},
		},
		At: time.Now(),
	}, DecayOptions{Factor: 0.5})

	if rep.Decayed != 1 {
		t.Fatalf("decayed %d edge(s), want exactly 1 — an edge implicated by two captures must decay ONCE per pass, not twice", rep.Decayed)
	}
	if len(rep.Disproofs) != 1 {
		t.Fatalf("Disproofs has %d record(s), want 1 for the single decayed edge", len(rep.Disproofs))
	}
	if got := rep.Disproofs[0]; got.DeviationKey != "aaa-first" || got.ActionID != "act-A" {
		t.Errorf("shared-pair attribution = %q/%q, want the FIRST path (aaa-first/act-A) deterministically", got.DeviationKey, got.ActionID)
	}
	// The confidence must reflect a SINGLE decay (0.8*0.5=0.4), not a double (0.2).
	if got := rep.Disproofs[0].DecayedTo; got <= 0.3 {
		t.Errorf("decayedTo = %v, want ~0.4 (a single decay); a smaller value means the edge decayed twice", got)
	}
}
