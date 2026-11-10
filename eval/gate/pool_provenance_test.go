package gate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TG-678 — pooled scorecards lost Judged/Errors, and VerifyIntegrity's "Judged==0 && Overall>0 ⇒ legacy, skip"
// had no discriminator between a genuinely older record and a malformed fresh one.
//
// MUTATION CONTROLS (each executed before merge; the MR description records the red):
//   - drop the Judged/Errors aggregation in Pool (`out.Judged += c.Judged` / `out.Errors += c.Errors` removed)
//     → RED: TestPoolSumsTheIntegrityCounters, TestPoolKeepsTheFirstErroredRunsCause.
//   - drop the stamped-card check (`case c.CountersRecorded:` branch removed) → RED:
//     TestAFreshZeroJudgedCardIsAProblemNotLegacy.
//   - drop the per-run provenance check → RED: TestAPooledCardWithOneZeroJudgedRunIsRefused.

func provDims() map[string]float64 {
	return map[string]float64{"appropriate_band": 4, "correct_diagnosis": 4, "evidence_grounded": 4, "sensible_proposal": 4, "falsifiable_prediction": 4}
}

func freshCard(n, judged, errors int, firstErr string) Scorecard {
	return Scorecard{N: n, Judged: judged, Errors: errors, FirstErr: firstErr, CountersRecorded: true, Overall: 4.0,
		ProposalRate: 0.45, PredictionRate: 0.45,
		RubricVersion: "2026-10-23.1", CorpusSHA256: "abc123", HarnessFingerprints: []string{"fp-a"},
		DimMeans: provDims(), DimSamples: map[string]int{"appropriate_band": judged}, OverallFormula: OverallFormulaV2}
}

func legacyCard(n int) Scorecard {
	return Scorecard{N: n, Judged: 0, Errors: 0, Overall: 3.1, DimMeans: provDims(), OverallFormula: OverallFormulaV2}
}

// Good multi-run input: Judged and Errors are SUMS, N stays the per-run mean, Runs counts the cards, every run
// keeps its own provenance row with its identities, and the stamp survives because every input carried it.
func TestPoolSumsTheIntegrityCounters(t *testing.T) {
	p := Pool([]Scorecard{freshCard(20, 20, 0, ""), freshCard(20, 20, 0, ""), freshCard(20, 20, 0, "")})
	if p.Judged != 60 || p.Errors != 0 || p.N != 20 || p.Runs != 3 {
		t.Fatalf("pooled judged %d errors %d n %d runs %d, want 60 / 0 / 20 / 3", p.Judged, p.Errors, p.N, p.Runs)
	}
	if !p.CountersRecorded {
		t.Error("every input was stamped, so the pooled card must be stamped too")
	}
	if len(p.RunProvenance) != 3 {
		t.Fatalf("want 3 provenance rows, got %d", len(p.RunProvenance))
	}
	for i, r := range p.RunProvenance {
		if r.Run != i+1 || r.N != 20 || r.Judged != 20 || r.Errors != 0 || r.RubricVersion != "2026-10-23.1" || r.CorpusSHA256 != "abc123" || len(r.HarnessFingerprints) != 1 || !r.CountersRecorded {
			t.Errorf("row %d = %+v", i, r)
		}
	}
	if probs := VerifyIntegrity("pooled", []Scorecard{p}, 0); len(probs) != 0 {
		t.Errorf("a healthy pooled card must verify: %v", probs)
	}
}

// Errored multi-run input: the sums carry the damage and FirstErr is the FIRST errored run's cause, not the
// first run's (which was clean) and not the last.
func TestPoolKeepsTheFirstErroredRunsCause(t *testing.T) {
	p := Pool([]Scorecard{
		freshCard(20, 20, 0, ""),
		freshCard(20, 18, 2, "run2: connect: connection refused"),
		freshCard(20, 19, 1, "run3: 429 too many requests"),
	})
	if p.Judged != 57 || p.Errors != 3 {
		t.Fatalf("pooled judged %d errors %d, want 57 / 3", p.Judged, p.Errors)
	}
	if p.FirstErr != "run2: connect: connection refused" {
		t.Errorf("FirstErr = %q, want the first ERRORED run's cause", p.FirstErr)
	}
	// A pooled card is refused through its rows — each errored or short run named once, nothing reported twice.
	probs := VerifyIntegrity("pooled", []Scorecard{p}, 0)
	joined := strings.Join(probs, "\n")
	for _, want := range []string{"pooled run 2: 2/20 session(s) errored", "connection refused", "pooled run 2: only 18/20", "pooled run 3: 1/20 session(s) errored", "429 too many requests", "pooled run 3: only 19/20"} {
		if !strings.Contains(joined, want) {
			t.Errorf("integrity problems lack %q:\n%s", want, joined)
		}
	}
	if len(probs) != 4 {
		t.Errorf("want exactly 4 problems (two runs x errored+short), got %d:\n%s", len(probs), joined)
	}
}

// A mixed pool (one legacy input) is NOT stamped: the pooled card cannot claim counters it only partly has.
// Its legacy run is DISCLOSED through its row, not counted against the sum as a judge loss (that would
// double-penalise a run whose counters were simply never recorded), and the stamped run is verified in full.
func TestPoolOfAMixedSetIsNotStamped(t *testing.T) {
	p := Pool([]Scorecard{freshCard(20, 20, 0, ""), legacyCard(20)})
	if p.CountersRecorded {
		t.Fatal("one unstamped input must leave the pooled card unstamped")
	}
	if p.Judged != 20 || p.Runs != 2 || len(p.RunProvenance) != 2 || p.RunProvenance[1].CountersRecorded {
		t.Errorf("pooled %+v", p.RunProvenance)
	}
	probs, notes := VerifyIntegrity("pooled", []Scorecard{p}, 0), IntegrityDisclosures("pooled", []Scorecard{p})
	if len(probs) != 0 {
		t.Errorf("a stamped complete run beside a legacy run is not a judge loss: %v", probs)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "pooled run 2: LEGACY run") {
		t.Errorf("the legacy run must be disclosed once, by row: %v", notes)
	}
	// But a stamped SHORT run inside the same mixed pool is still refused, by row.
	short := Pool([]Scorecard{freshCard(20, 17, 0, ""), legacyCard(20)})
	if probs := VerifyIntegrity("pooled", []Scorecard{short}, 0); len(probs) != 1 || !strings.Contains(probs[0], "pooled run 1: only 17/20") {
		t.Errorf("the short stamped run must be named: %v", probs)
	}
	if len(Pool(nil).RunProvenance) != 0 || Pool(nil).Runs != 0 || Pool(nil).CountersRecorded {
		t.Error("an empty pool carries no provenance and no stamp")
	}
}

// THE DISCRIMINATOR. A FRESH card (stamped) that judged nothing beside Overall>0 is a problem — it is not
// legacy, whatever its judged count. Pre-fix this card passed integrity on the open-ended "older harness" rule.
func TestAFreshZeroJudgedCardIsAProblemNotLegacy(t *testing.T) {
	fresh := Scorecard{N: 20, Judged: 0, Errors: 0, CountersRecorded: true, Overall: 3.1, DimMeans: provDims()}
	probs, notes := VerifyIntegrity("candidate", []Scorecard{fresh}, 20), IntegrityDisclosures("candidate", []Scorecard{fresh})
	if len(probs) == 0 || !strings.Contains(probs[0], "only 0/20 session(s) judged") {
		t.Fatalf("a stamped zero-judged card was accepted as legacy: problems %v notes %v", probs, notes)
	}
	if len(notes) != 0 {
		t.Errorf("a fresh card must not be disclosed as legacy: %v", notes)
	}
	// And a stamped SHORT card fails exactly as before.
	if probs := VerifyIntegrity("candidate", []Scorecard{freshCard(20, 12, 0, "")}, 20); len(probs) == 0 {
		t.Error("a stamped card with Judged<N must fail")
	}
	// A stamped COMPLETE card passes, with no note.
	complete := []Scorecard{freshCard(20, 20, 0, "")}
	if probs, notes := VerifyIntegrity("candidate", complete, 20), IntegrityDisclosures("candidate", complete); len(probs) != 0 || len(notes) != 0 {
		t.Errorf("a complete stamped card must pass silently: %v %v", probs, notes)
	}
}

// A LEGACY card (no stamp, Judged==0, Overall>0) is tolerated — legitimate history keeps verifying — and
// DISCLOSED: the note names the card and says the judged check could not be applied.
func TestALegacyCardIsToleratedAndDisclosed(t *testing.T) {
	probs, notes := VerifyIntegrity("base", []Scorecard{legacyCard(20)}, 20), IntegrityDisclosures("base", []Scorecard{legacyCard(20)})
	if len(probs) != 0 {
		t.Fatalf("a legacy card must still verify (legitimate history): %v", probs)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "base run 1: LEGACY card") || !strings.Contains(notes[0], "could NOT be applied") {
		t.Fatalf("the tolerance must be disclosed by name: %v", notes)
	}
	// An unstamped card that DID record a short judged count is a problem, as before.
	if probs := VerifyIntegrity("base", []Scorecard{{N: 20, Judged: 15, Overall: 3.0}}, 20); len(probs) == 0 {
		t.Error("unstamped Judged<N must fail")
	}
	// The pre-existing refusals are untouched: n=0, overall=0, truncated, errored.
	for _, c := range []Scorecard{{N: 0}, {N: 20, Overall: 0}, {N: 18, Overall: 3, CountersRecorded: true, Judged: 18}, {N: 20, Judged: 20, Errors: 1, Overall: 3, CountersRecorded: true}} {
		if probs := VerifyIntegrity("x", []Scorecard{c}, 20); len(probs) == 0 {
			t.Errorf("card %+v must fail integrity", c)
		}
	}
}

// One zero-judged run hidden inside a pooled sum is refused through its provenance row even though the sum
// (40 of 60 judged) would also fail — and a pooled card whose rows are all complete but whose sum was tampered
// is refused on the sum.
func TestAPooledCardWithOneZeroJudgedRunIsRefused(t *testing.T) {
	p := Pool([]Scorecard{freshCard(20, 20, 0, ""), Scorecard{N: 20, Judged: 0, CountersRecorded: true, Overall: 3.0, DimMeans: provDims()}, freshCard(20, 20, 0, "")})
	probs := VerifyIntegrity("pooled", []Scorecard{p}, 0)
	if !strings.Contains(strings.Join(probs, "\n"), "pooled run 2: only 0/20 session(s) judged") {
		t.Fatalf("the zero-judged run must be named through its provenance row: %v", probs)
	}
	healthy := Pool([]Scorecard{freshCard(20, 20, 0, ""), freshCard(20, 20, 0, "")})
	healthy.Judged = 20
	if probs := VerifyIntegrity("pooled", []Scorecard{healthy}, 0); len(probs) != 1 || !strings.Contains(probs[0], "pooled judged=20 errors=0 disagree with its 2 run(s) (judged 40") {
		t.Errorf("a pooled sum that disagrees with its rows must fail on the disagreement: %v", probs)
	}
	healthy.Judged = 40
	healthy.Errors = 1
	if probs := VerifyIntegrity("pooled", []Scorecard{healthy}, 0); len(probs) != 1 || !strings.Contains(probs[0], "errors=1 disagree") {
		t.Errorf("a pooled errors sum that disagrees with its rows must fail: %v", probs)
	}
	// The rows are self-reported, so the card's headline is tied to them too: a forged N, a forged stamp, a
	// row with no judged aggregate, and a row short of the expected corpus are each refused.
	forgedN := Pool([]Scorecard{freshCard(20, 20, 0, ""), freshCard(20, 20, 0, "")})
	forgedN.N = 60
	if probs := VerifyIntegrity("pooled", []Scorecard{forgedN}, 0); len(probs) != 1 || !strings.Contains(probs[0], "pooled n=60 disagrees with its 2 run(s) (mean n 20, total 40)") {
		t.Errorf("a pooled n that is neither the mean nor the total of its rows must fail: %v", probs)
	}
	// The committed trend baseline keeps Scorecard.N as the TOTAL (BuildRefreshedBaseline), so a refreshed
	// baseline's card — which the nightly archives as a trend comparator — must keep verifying.
	refreshed := BuildRefreshedBaseline([]Scorecard{freshCard(36, 36, 0, ""), freshCard(36, 36, 0, ""), freshCard(36, 36, 0, "")}, "deadbeef", "2026-11-07", OutcomePass)
	if refreshed.Scorecard.N != 108 || refreshed.Scorecard.Judged != 108 || len(refreshed.Scorecard.RunProvenance) != 3 {
		t.Fatalf("refreshed baseline card %+v", refreshed.Scorecard)
	}
	if probs, notes := VerifyIntegrity("baseline", []Scorecard{refreshed.Scorecard}, 0), IntegrityDisclosures("baseline", []Scorecard{refreshed.Scorecard}); len(probs) != 0 || len(notes) != 0 {
		t.Errorf("a refreshed trend baseline (n = total of its rows) must verify silently: %v %v", probs, notes)
	}
	if probs := VerifyIntegrity("baseline", []Scorecard{refreshed.Scorecard}, 36); len(probs) == 0 || !strings.Contains(probs[0], "baseline run 1: n=108 but expected the full corpus of 36") {
		t.Errorf("the card-level expect-n check still reads the card's own n, as before: %v", probs)
	}
	forgedStamp := Pool([]Scorecard{legacyCard(20), legacyCard(20)})
	forgedStamp.CountersRecorded = true
	if probs := VerifyIntegrity("pooled", []Scorecard{forgedStamp}, 0); len(probs) != 1 || !strings.Contains(probs[0], "counters_recorded=true disagrees with its rows (all stamped: false)") {
		t.Errorf("a stamp the rows do not support must fail: %v", probs)
	}
	zeroOverallRun := Pool([]Scorecard{freshCard(20, 20, 0, ""), {N: 20, Overall: 0, DimMeans: provDims()}})
	if probs := VerifyIntegrity("pooled", []Scorecard{zeroOverallRun}, 0); len(probs) != 1 || !strings.Contains(probs[0], "pooled run 2: overall=0") {
		t.Errorf("a legacy-looking row with no judged aggregate is a degraded run, not a tolerated legacy one: %v", probs)
	}
	short := Pool([]Scorecard{freshCard(20, 20, 0, ""), freshCard(12, 12, 0, "")})
	if probs := VerifyIntegrity("pooled", []Scorecard{short}, 20); len(probs) == 0 || !strings.Contains(strings.Join(probs, "\n"), "pooled run 2: n=12 but expected the full corpus of 20") {
		t.Errorf("a row short of the expected corpus must be named: %v", probs)
	}
}

// Round trip through the archive reader: a pooled card written as JSON and read back with LoadScorecard keeps
// its sums, stamp and provenance; a legacy JSON record without any of the new keys reads back unstamped.
func TestPooledCardRoundTripsThroughTheReader(t *testing.T) {
	dir := t.TempDir()
	p := Pool([]Scorecard{freshCard(20, 20, 0, ""), freshCard(20, 19, 1, "run2 boom")})
	path := filepath.Join(dir, "scorecard.json")
	if err := os.WriteFile(path, mustJSON(t, p), 0o644); err != nil {
		t.Fatal(err)
	}
	back, err := LoadScorecard(path)
	if err != nil {
		t.Fatal(err)
	}
	if back.Judged != 39 || back.Errors != 1 || back.Runs != 2 || !back.CountersRecorded || len(back.RunProvenance) != 2 || back.FirstErr != "run2 boom" {
		t.Fatalf("round trip lost the counters: %+v", back)
	}
	if back.RunProvenance[1].Judged != 19 || back.RunProvenance[1].FirstErr != "run2 boom" || back.RunProvenance[1].RubricVersion != "2026-10-23.1" {
		t.Errorf("round trip lost a provenance row: %+v", back.RunProvenance[1])
	}
	raw := string(mustJSON(t, p))
	for _, key := range []string{`"counters_recorded": true`, `"runs": 2`, `"run_provenance"`} {
		if !strings.Contains(raw, key) {
			t.Errorf("JSON lacks %s:\n%s", key, raw)
		}
	}
	legacy := filepath.Join(dir, "legacy.json")
	if err := os.WriteFile(legacy, []byte(`{"n":20,"judged":0,"errors":0,"overall":3.1,"dim_means":{"appropriate_band":3.1}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	lb, err := LoadScorecard(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if lb.CountersRecorded || lb.Runs != 0 || lb.RunProvenance != nil {
		t.Errorf("a legacy record must read back unstamped: %+v", lb)
	}
	if raw := string(mustJSON(t, lb)); strings.Contains(raw, "counters_recorded") || strings.Contains(raw, "run_provenance") {
		t.Errorf("re-serialising a legacy card must not invent the new keys:\n%s", raw)
	}
}

// Compare still reads a pooled candidate the same way: the counters ride along, the verdict is unchanged.
func TestPooledCountersDoNotChangeTheVerdict(t *testing.T) {
	base := Baseline{Runs: 1, Scorecard: freshCard(20, 20, 0, "")}
	v := Compare(base, []Scorecard{freshCard(20, 20, 0, ""), freshCard(20, 20, 0, "")}, []ControlRun{{N: 1, Results: []ControlResult{{Ref: "ctl-01"}}}}, DefaultThresholds())
	if v.Outcome != OutcomePass {
		t.Fatalf("outcome %q: %v", v.Outcome, v.Reasons)
	}
	if pooled := Pool([]Scorecard{freshCard(20, 20, 0, ""), freshCard(20, 20, 0, "")}); pooled.Judged != 40 {
		t.Errorf("pooled judged %d", pooled.Judged)
	}
}
