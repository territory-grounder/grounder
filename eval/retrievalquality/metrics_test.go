package retrievalquality

import (
	"math"
	"testing"

	"github.com/territory-grounder/grounder/core/knowledge"
)

// --- metric math --------------------------------------------------------------------------------------

type fakeRetriever struct{ hits []knowledge.Hit }

func (f fakeRetriever) Retrieve(_ knowledge.Query, k int) []knowledge.Hit {
	if k >= 0 && k < len(f.hits) {
		return f.hits[:k]
	}
	return f.hits
}

func hitsOf(refs ...string) []knowledge.Hit {
	out := make([]knowledge.Hit, 0, len(refs))
	for _, r := range refs {
		out = append(out, knowledge.Hit{Incident: knowledge.Incident{ExternalRef: r}})
	}
	return out
}

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestEvaluateComputesPrecisionRecallMRR(t *testing.T) {
	// retrieved [TG-1,TG-2,TG-3]; relevant {TG-1,TG-3}: inter=2, first relevant at rank 1.
	got := Evaluate(fakeRetriever{hits: hitsOf("TG-1", "TG-2", "TG-3")},
		[]Labeled{{Name: "c", Relevant: []string{"TG-1", "TG-3"}}}, 3)
	if got.Cases != 1 {
		t.Fatalf("cases = %d, want 1", got.Cases)
	}
	if !approx(got.MeanPrecision, 2.0/3.0) {
		t.Errorf("precision = %v, want 2/3", got.MeanPrecision)
	}
	if !approx(got.MeanRecall, 1.0) {
		t.Errorf("recall = %v, want 1.0 (both relevant retrieved)", got.MeanRecall)
	}
	if !approx(got.MeanMRR, 1.0) {
		t.Errorf("MRR = %v, want 1.0 (first relevant at rank 1)", got.MeanMRR)
	}
}

func TestEvaluateMRRRankAndMisses(t *testing.T) {
	// first relevant at rank 2 → MRR 0.5; and a case whose one relevant is not retrieved → recall 0.
	got := Evaluate(fakeRetriever{hits: hitsOf("TG-9", "TG-1")},
		[]Labeled{
			{Name: "rank2", Relevant: []string{"TG-1"}}, // retrieved rank2 → RR 0.5, recall 1, precision 1/2
			{Name: "miss", Relevant: []string{"TG-404"}}, // never retrieved → RR 0, recall 0, precision 0
		}, 5)
	if got.Cases != 2 {
		t.Fatalf("cases = %d, want 2", got.Cases)
	}
	if !approx(got.MeanMRR, (0.5+0.0)/2) {
		t.Errorf("MRR = %v, want 0.25", got.MeanMRR)
	}
	if !approx(got.MeanRecall, (1.0+0.0)/2) {
		t.Errorf("recall = %v, want 0.5", got.MeanRecall)
	}
	if !approx(got.MeanPrecision, (0.5+0.0)/2) {
		t.Errorf("precision = %v, want 0.25", got.MeanPrecision)
	}
}

func TestEvaluateSkipsUnlabeledCases(t *testing.T) {
	got := Evaluate(fakeRetriever{hits: hitsOf("TG-1")},
		[]Labeled{{Name: "unlabeled", Relevant: nil}}, 3)
	if got.Cases != 0 {
		t.Fatalf("a case with no relevant refs must be skipped, got Cases=%d", got.Cases)
	}
}

// --- baseline floor over the real LexicalRetriever ----------------------------------------------------

func fixtureCorpus() []knowledge.Incident {
	return []knowledge.Incident{
		{ExternalRef: "TG-1", Host: "web01", AlertRule: "NginxDown", Site: "nl", Tags: []string{"web"}, Resolution: "restart nginx"},
		{ExternalRef: "TG-2", Host: "web01", AlertRule: "NginxDown", Site: "nl", Tags: []string{"web"}, Resolution: "clear tmp, restart"},
		{ExternalRef: "TG-3", Host: "web02", AlertRule: "NginxDown", Site: "nl", Tags: []string{"web"}, Resolution: "restart nginx"},
		{ExternalRef: "TG-4", Host: "db01", AlertRule: "DiskFull", Site: "nl", Tags: []string{"db"}, Resolution: "extend the LV"},
		{ExternalRef: "TG-5", Host: "db02", AlertRule: "DiskFull", Site: "gr", Tags: []string{"db"}, Resolution: "prune WAL"},
		{ExternalRef: "TG-6", Host: "fw01", AlertRule: "HighCPU", Site: "nl", Tags: []string{"net"}, Resolution: "kill runaway proc"},
		{ExternalRef: "TG-7", Host: "web01", AlertRule: "HighMemory", Site: "nl", Tags: []string{"web"}, Resolution: "bounce the app"},
		{ExternalRef: "TG-8", Host: "db01", AlertRule: "ReplicationLag", Site: "nl", Tags: []string{"db"}, Resolution: "resync replica"},
		{ExternalRef: "TG-9", Host: "sw01", AlertRule: "InterfaceDown", Site: "gr", Tags: []string{"net"}, Resolution: "bounce the port"},
		{ExternalRef: "TG-10", Host: "web03", AlertRule: "NginxDown", Site: "gr", Tags: []string{"web"}, Resolution: "restart nginx"},
	}
}

func fixtureCases() []Labeled {
	return []Labeled{
		// Same host + same rule are the unambiguous precedents; a same-rule other-host is also relevant.
		{Name: "nginx-web01", Query: knowledge.Query{Host: "web01", AlertRule: "NginxDown", Site: "nl", Tags: []string{"web"}},
			Relevant: []string{"TG-1", "TG-2"}},
		{Name: "diskfull-db01", Query: knowledge.Query{Host: "db01", AlertRule: "DiskFull", Site: "nl", Tags: []string{"db"}},
			Relevant: []string{"TG-4", "TG-5"}},
		{Name: "highcpu-fw01", Query: knowledge.Query{Host: "fw01", AlertRule: "HighCPU", Site: "nl", Tags: []string{"net"}},
			Relevant: []string{"TG-6"}},
	}
}

// The current LexicalRetriever must clear a retrieval-quality FLOOR on the labeled fixture: it surfaces the
// relevant precedent (recall) and ranks it first (MRR). This is the regression gate the ticket asks for — a
// scoring change that stops surfacing the relevant precedent drops recall/MRR below the floor and reddens.
// Precision is LOGGED not floored: lexical retrieval admits same-rule noise on purpose, and shrinking that
// noise is exactly what the follow-on threshold/RRF stages improve — measured against this same baseline.
func TestLexicalRetrieverMeetsRetrievalQualityFloor(t *testing.T) {
	r := knowledge.NewLexicalRetriever(fixtureCorpus())
	const k = 3
	res := Evaluate(r, fixtureCases(), k)

	t.Logf("retrieval quality @%d over %d cases: recall=%.3f MRR=%.3f precision=%.3f",
		res.K, res.Cases, res.MeanRecall, res.MeanMRR, res.MeanPrecision)

	if res.Cases != len(fixtureCases()) {
		t.Fatalf("scored %d cases, want %d — a labeled case was silently skipped", res.Cases, len(fixtureCases()))
	}
	const recallFloor, mrrFloor = 0.80, 0.80
	if res.MeanRecall < recallFloor {
		t.Errorf("mean recall@%d = %.3f < floor %.2f — the retriever is no longer surfacing the relevant precedent", k, res.MeanRecall, recallFloor)
	}
	if res.MeanMRR < mrrFloor {
		t.Errorf("mean MRR@%d = %.3f < floor %.2f — the relevant precedent is no longer ranked first", k, res.MeanMRR, mrrFloor)
	}
}
