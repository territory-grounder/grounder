package falsify

import (
	"context"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/verify"
)

// PROOF (a): a scored DEVIATION is CAPTURED into the discovery corpus. The prediction over pve01 named
// {n8n01, litellm01}; reality instead alerted web09 (a surprise host the prediction never named) — a
// deviation. The scorer captures it: one record, keyed by the misprediction signature, carrying the surprise
// host, the observed cascade, and the confusion matrix the deterministic scorer produced. The capture is
// additive — the score, verdict, and cascade window are exactly what they were before (unchanged by capture).
func TestScoreDueCapturesDeviationIntoDiscoveryCorpus(t *testing.T) {
	store := NewMemStore()
	store.Seed(samplePrediction("plan-d1", "act-d1"), fixedNow.Add(-time.Hour))
	disc := NewMemDiscoveryCorpus(0)
	sc := newScorer(store, []verify.ObservedAlert{{Host: "web09", Rule: "HostDown", Site: "nl"}})
	sc.Discovery = disc

	res, err := sc.ScoreDue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Deviations != 1 || res.DiscoveryCaptured != 1 || res.DiscoveryErrs != 0 {
		t.Fatalf("expected 1 deviation captured with no errors, got %+v", res)
	}
	snap := disc.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("expected exactly one captured deviation, got %d", len(snap))
	}
	rec := snap[0].Record
	if rec.TargetHost != "pve01" || rec.Site != "nl" || rec.Verdict != safety.VerdictDeviation {
		t.Fatalf("captured record identity wrong: %+v", rec)
	}
	if len(rec.SurpriseHosts) != 1 || rec.SurpriseHosts[0] != "web09" {
		t.Fatalf("captured record must carry the surprise host web09, got %v", rec.SurpriseHosts)
	}
	if rec.DeviationKey() != "pve01|nl|web09" {
		t.Fatalf("deviation key wrong: %q", rec.DeviationKey())
	}
	// The captured confusion matrix is exactly what the deterministic scorer wrote back onto the row.
	if rec.Score != (Score{TP: 0, FP: 2, FN: 1, ControlTP: 1, ControlFP: 0}) {
		t.Fatalf("captured score must equal the written score, got %+v", rec.Score)
	}
	if len(rec.Observed) != 1 || rec.Observed[0].Host != "web09" {
		t.Fatalf("captured record must carry the observed cascade, got %v", rec.Observed)
	}
	// Capture is side-effect-free on the gate outputs: the writeback score, the verdict, and the cascade window
	// are precisely what they were WITHOUT a discovery writer wired.
	if s, _ := store.ScoreOf("plan-d1"); s != (Score{TP: 0, FP: 2, FN: 1, ControlTP: 1, ControlFP: 0}) {
		t.Fatalf("capture must not alter the writeback score: %+v", s)
	}
	if v, _ := store.VerdictOf("act-d1"); v != safety.VerdictDeviation {
		t.Fatalf("capture must not alter the verdict: %q", v)
	}
}

// A MATCH is NOT a deviation and must NOT be captured — the discovery corpus holds only falsified predictions.
func TestScoreDueDoesNotCaptureMatches(t *testing.T) {
	store := NewMemStore()
	store.Seed(samplePrediction("plan-d2", "act-d2"), fixedNow.Add(-time.Hour))
	disc := NewMemDiscoveryCorpus(0)
	// The prediction named n8n01/litellm01; reality alerts exactly n8n01 ⇒ a match (no surprise host).
	sc := newScorer(store, []verify.ObservedAlert{{Host: "n8n01", Rule: "HostDown", Site: "nl"}})
	sc.Discovery = disc
	res, err := sc.ScoreDue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Deviations != 0 || res.DiscoveryCaptured != 0 {
		t.Fatalf("a match must not be captured, got %+v", res)
	}
	if disc.Len() != 0 {
		t.Fatalf("the discovery corpus must be empty after a match, got %d", disc.Len())
	}
}

// The same misprediction signature seen across DIFFERENT predictions is one discovery case whose reproduction
// count grows — the signal promotion gates on. It is NOT re-inserted (dedup first-wins per signature).
func TestDiscoveryCorpusDeduplicatesAndCountsReproductions(t *testing.T) {
	disc := NewMemDiscoveryCorpus(0)
	base := DiscoveryRecord{TargetHost: "pve01", Site: "nl", SurpriseHosts: []string{"web09"}, Verdict: safety.VerdictDeviation}
	first := base
	first.ActionID, first.ObservedAt = "act-a", fixedNow
	second := base
	second.ActionID, second.ObservedAt = "act-b", fixedNow.Add(time.Hour) // same signature, later incident
	if newly, _ := disc.Capture(context.Background(), first); !newly {
		t.Fatal("first sighting of a signature must be newly captured")
	}
	if newly, _ := disc.Capture(context.Background(), second); newly {
		t.Fatal("a reproduction of the same signature must NOT be a new capture")
	}
	snap := disc.Snapshot()
	if len(snap) != 1 || snap[0].Reproductions != 2 {
		t.Fatalf("expected one case with 2 reproductions, got %+v", snap)
	}
	if !snap[0].LastSeen.Equal(second.ObservedAt) {
		t.Fatalf("last-seen must advance to the reproduction time, got %v", snap[0].LastSeen)
	}
}

// The rolling cap is HONEST: a new signature at capacity evicts the oldest and RECORDS the drop — no silent cap.
func TestDiscoveryCorpusRollingCapDropsAreLogged(t *testing.T) {
	disc := NewMemDiscoveryCorpus(2)
	mk := func(host string) DiscoveryRecord {
		return DiscoveryRecord{TargetHost: "t", Site: "nl", SurpriseHosts: []string{host}, ObservedAt: fixedNow}
	}
	disc.Capture(context.Background(), mk("h1"))
	disc.Capture(context.Background(), mk("h2"))
	disc.Capture(context.Background(), mk("h3")) // at capacity ⇒ evicts h1's signature
	if disc.Len() != 2 {
		t.Fatalf("rolling cap must hold exactly 2, got %d", disc.Len())
	}
	dropped := disc.Dropped()
	if len(dropped) != 1 || dropped[0] != "t|nl|h1" {
		t.Fatalf("the evicted signature must be recorded (no silent cap), got %v", dropped)
	}
}

// A nil discovery writer keeps the scorer inert on capture — deviations are still scored, nothing is captured,
// no panic (honest zeros).
func TestScoreDueDiscoveryInertWhenUnwired(t *testing.T) {
	store := NewMemStore()
	store.Seed(samplePrediction("plan-d3", "act-d3"), fixedNow.Add(-time.Hour))
	sc := newScorer(store, []verify.ObservedAlert{{Host: "web09", Rule: "HostDown", Site: "nl"}})
	sc.Discovery = nil // explicitly unwired
	res, err := sc.ScoreDue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Deviations != 1 || res.DiscoveryCaptured != 0 || res.DiscoveryErrs != 0 {
		t.Fatalf("unwired discovery ⇒ scored deviation, nothing captured, no error, got %+v", res)
	}
}

// A capture blip is BEST-EFFORT: it is counted, never fatal — the score+verdict already landed.
func TestScoreDueDiscoveryErrorIsBestEffort(t *testing.T) {
	store := NewMemStore()
	store.Seed(samplePrediction("plan-d4", "act-d4"), fixedNow.Add(-time.Hour))
	sc := newScorer(store, []verify.ObservedAlert{{Host: "web09", Rule: "HostDown", Site: "nl"}})
	sc.Discovery = failingDiscovery{}
	res, err := sc.ScoreDue(context.Background())
	if err != nil {
		t.Fatalf("a capture error must never fail the pass, got %v", err)
	}
	if res.Deviations != 1 || res.DiscoveryCaptured != 0 || res.DiscoveryErrs != 1 {
		t.Fatalf("a capture blip must be counted not fatal, got %+v", res)
	}
	if _, ok := store.ScoreOf("plan-d4"); !ok {
		t.Fatal("the score must still be durable after a capture blip")
	}
}

type failingDiscovery struct{}

func (failingDiscovery) Capture(context.Context, DiscoveryRecord) (bool, error) {
	return false, context.DeadlineExceeded
}
