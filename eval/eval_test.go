package eval

import "testing"

func TestParseScore(t *testing.T) {
	// clean JSON
	s, err := ParseScore("eval-01", `{"correct_diagnosis":4,"evidence_grounded":3,"sensible_proposal":5,"appropriate_band":5,"falsifiable_prediction":2,"comment":"ok"}`)
	if err != nil {
		t.Fatalf("clean: %v", err)
	}
	if s.Scores["appropriate_band"] != 5 || s.Comment != "ok" {
		t.Fatalf("bad parse: %+v", s)
	}
	// wrapped in prose + fences + out-of-range clamps
	s2, err := ParseScore("eval-02", "Here is my verdict:\n```json\n{\"correct_diagnosis\":9,\"appropriate_band\":0,\"comment\":\"x\"}\n```\nthanks")
	if err != nil {
		t.Fatalf("wrapped: %v", err)
	}
	if s2.Scores["correct_diagnosis"] != 5 || s2.Scores["appropriate_band"] != 1 {
		t.Fatalf("clamp failed: %+v", s2.Scores)
	}
	// no json → error
	if _, err := ParseScore("eval-03", "I cannot score this"); err == nil {
		t.Fatal("want error on no-json reply")
	}
}

func TestAggregate(t *testing.T) {
	sessions := []Session{
		{Ref: "eval-01", Band: "POLL_PAUSE", Proposed: true, Predicted: true},
		{Ref: "eval-02", Band: "AUTO_NOTICE", Proposed: true, Predicted: true},
		{Ref: "eval-03", Band: "", Proposed: false, Predicted: false},
		{Ref: "eval-04", Band: "POLL_PAUSE", Proposed: true, Predicted: false, Mutated: true},
	}
	scores := []Score{
		{Ref: "eval-01", Scores: map[string]int{"correct_diagnosis": 4, "appropriate_band": 5}},
		{Ref: "eval-02", Scores: map[string]int{"correct_diagnosis": 2, "appropriate_band": 3}},
	}
	sc := Aggregate(sessions, scores)
	if sc.N != 4 {
		t.Fatalf("N=%d", sc.N)
	}
	if sc.Bands["POLL_PAUSE"] != 2 || sc.Bands["none"] != 1 {
		t.Fatalf("bands=%v", sc.Bands)
	}
	if sc.ProposalRate != 0.75 {
		t.Fatalf("proposal rate=%v", sc.ProposalRate)
	}
	if sc.PredictionRate != 0.5 {
		t.Fatalf("prediction rate=%v", sc.PredictionRate)
	}
	if sc.MutationCount != 1 { // captured but flagged — mutation MUST be 0 in a healthy read-only run
		t.Fatalf("mutation count=%d", sc.MutationCount)
	}
	if sc.DimMeans["correct_diagnosis"] != 3.0 || sc.DimMeans["appropriate_band"] != 4.0 {
		t.Fatalf("dim means=%v", sc.DimMeans)
	}
	if sc.Overall != 3.5 {
		t.Fatalf("overall=%v", sc.Overall)
	}
}

// TG-61 seq C: falsifiable_prediction is N/A for a grounded stand-down (no action ⇒ no prediction), so a
// stand-down's floor must NOT drag the dimension mean — it is EXCLUDED from that dimension only, while
// every other dimension still counts every session.
func TestAggregateFalsifiablePredictionExcludesStandDowns(t *testing.T) {
	sessions := []Session{
		{Ref: "p1", Band: "POLL_PAUSE", Proposed: true, Predicted: true},
		{Ref: "p2", Band: "POLL_PAUSE", Proposed: true, Predicted: true},
		{Ref: "stop", Band: "", Proposed: false, Predicted: false}, // grounded stand-down
	}
	scores := []Score{
		{Ref: "p1", Scores: map[string]int{"falsifiable_prediction": 4, "correct_diagnosis": 5}},
		{Ref: "p2", Scores: map[string]int{"falsifiable_prediction": 4, "correct_diagnosis": 3}},
		{Ref: "stop", Scores: map[string]int{"falsifiable_prediction": 1, "correct_diagnosis": 4}},
	}
	sc := Aggregate(sessions, scores)
	// falsifiable_prediction is the PROPOSER-ONLY mean (4+4)/2 = 4.0 — the stand-down's 1 is excluded.
	if got := sc.DimMeans["falsifiable_prediction"]; got != 4.0 {
		t.Fatalf("falsifiable_prediction must exclude the stand-down (want 4.0), got %v", got)
	}
	// Every other dimension still counts all three sessions: correct_diagnosis (5+3+4)/3 = 4.0.
	if got := sc.DimMeans["correct_diagnosis"]; got != 4.0 {
		t.Fatalf("correct_diagnosis must count all sessions (want 4.0), got %v", got)
	}
	// If EVERY session were a stand-down, falsifiable_prediction would have zero samples ⇒ omitted, not floored.
	only := Aggregate([]Session{{Ref: "s", Proposed: false}}, []Score{{Ref: "s", Scores: map[string]int{"falsifiable_prediction": 1}}})
	if _, present := only.DimMeans["falsifiable_prediction"]; present {
		t.Fatalf("with no applicable session, falsifiable_prediction must be omitted, got %v", only.DimMeans)
	}
}

func TestLoadCorpus(t *testing.T) {
	c, err := LoadCorpus("corpus.json")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(c) < 15 {
		t.Fatalf("want >=15 corpus incidents, got %d", len(c))
	}
	for _, x := range c {
		if x.ExternalRef == "" || x.AlertRule == "" || x.Severity == "" {
			t.Fatalf("incomplete corpus entry: %+v", x)
		}
	}
}
