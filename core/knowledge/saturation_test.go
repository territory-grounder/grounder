package knowledge

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TIE SATURATION — the fraction of retrievals whose top-k CUT is decided by the alphabetical
// ExternalRef tiebreak rather than by relevance.
//
// WHAT THIS MEASURES AND WHY IT IS THE FIRST RETRIEVAL METRIC TG EVER HAD. Retrieve sorts by score
// descending and breaks ties on ExternalRef ascending (retriever.go). When the k-th and (k+1)-th
// candidates carry the SAME score, which one reaches the agent's precedent block is settled by
// alphabetical order — that is not ranking, it is a coin toss with a stable seed.
//
// It is measured on the SHIPPED SEED CORPUS, not on eval/corpus.json. The fixture cannot stand in for
// production: its sites are all "dc1" where the corpus uses "nl"/"gr" (intersection: EMPTY) and 14 of
// its 16 hosts do not exist in the corpus at all — so on the fixture weightSite is structurally dead and
// weightHost is dead for 88% of queries. Measuring 2 of 5 signals and calling it retrieval quality is
// how a baseline becomes fiction.
//
// The cause is rule saturation: weightRule=5.0 dominates every other channel, and the six commonest
// rules cover 73 of the 140 corpus rows. A query matching "Service-up/down" ties with 17 siblings at the
// same score before any other signal is consulted.
//
// THE REPO NUMBER UNDERSTATES PRODUCTION. Measured 2026-08-01 against the corpus the deployed worker
// actually loads (seed 140 + maintained 530 = 670 unique refs, pulled from the running container):
//
//	tie saturation 620/670 = 0.925   ·   top-6 rule share 592/670 = 0.88
//
// against 0.821 / 0.52 for the repo seed alone. The maintained corpus grows by lessons, and lessons
// concentrate in the rules that fire most, so the ratchet below is a LOWER BOUND on the real problem.
// CI can only measure what is checked in; the gap between the two is itself the finding.
//
// A CONSEQUENCE WORTH STATING, because it changes what to build next: the FusedRetriever blends this
// lexical channel with the semantic one by Reciprocal Rank Fusion at equal weight (rrfK=60, both
// channels 1.0). When 92.5% of lexical cuts are alphabetical, RRF is averaging a real signal against
// near-noise at parity — so the semantic channel being LIVE (verified: model=embed-nomic dim=768,
// 670/670 rows embedded) does not rescue the result the way the architecture diagram suggests.
//
// THE RECENCY CHANNEL AND WHAT IT ACTUALLY BOUGHT (2026-08-01) — a prediction that FAILED in magnitude,
// recorded because the reason is the useful part.
//
// Prediction: adding a continuous recency signal would make tie saturation "fall sharply".
// Measured, simulating the backfill against the deployed corpus and the 227 real production queries:
//
//	before  0.832 per shape · 0.918 volume-weighted
//	after   0.810 per shape · 0.887 volume-weighted      (3.1 pp, not a sharp fall)
//
// TWO reasons, both worth knowing before anyone tunes the weight:
//  1. NO CORPUS ROW CARRIES A TIMESTAMP TODAY — 0 of 670, because the old code dropped ResolvedAt at the
//     lessons boundary. Shipping the channel changes NOTHING until either a backfill runs or the corpus
//     regrows. The 530 maintained rows are backfillable (they join session_triage 530/530); the 140 seed
//     rows are not, and must stay unscored rather than be given an invented date.
//  2. EVEN BACKFILLED, THE CORPUS HAS ALMOST NO AGE SPREAD: min 1 day, p25 3, median 4, p75 4, max 8.
//     A 90-day linear decay maps that whole range into ~2 buckets after round2. Recency cannot separate
//     candidates that are all the same age. It will strengthen as the corpus ages and spreads — which is
//     an argument for shipping it now and re-measuring later, not for widening the weight to force a
//     number today.
//
// This number is a RATCHET, not a target. It may only move DOWN. Adding a discrete channel raises it;
// only a continuous, well-spread signal lowers it — which is the argument for embeddings and against
// bolting a reranker on top of a cut that was never ranked.
const (
	// saturationCeiling is the measured baseline, 2026-08-01: 115/140 leave-one-out over the shipped
	// seed. LOWER THIS when a change improves it; NEVER raise it to make a build pass — raising it is the
	// ratchet failing, and the reason to raise it is always the reason not to.
	saturationCeiling = 0.83
	// retrieveK mirrors the one production call site: temporal/runner/activities.go passes k=3.
	retrieveK = 3
)

// loadSeedCorpus reads the corpus the worker actually ships with.
func loadSeedCorpus(t *testing.T) []Incident {
	t.Helper()
	path := filepath.Join("..", "..", "deploy", "knowledge", "corpus.seed.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read shipped seed corpus: %v", err)
	}
	var corpus []Incident
	if err := json.Unmarshal(raw, &corpus); err != nil {
		t.Fatalf("parse shipped seed corpus: %v", err)
	}
	return corpus
}

// tieSaturation runs leave-one-out over the corpus through the REAL retriever and returns the fraction
// of queries whose k-th cut is a tie. Driving the production scorer matters: a reimplementation here
// would drift from retriever.go and the number would quietly become fiction.
func tieSaturation(corpus []Incident, k int) (tied, evaluated int) {
	for i, row := range corpus {
		rest := make([]Incident, 0, len(corpus)-1)
		rest = append(rest, corpus[:i]...)
		rest = append(rest, corpus[i+1:]...)

		hits := NewLexicalRetriever(rest).Retrieve(Query{
			Host: row.Host, AlertRule: row.AlertRule, Site: row.Site,
			Summary: row.Summary, Tags: row.Tags,
		}, k+1)
		if len(hits) <= k {
			continue // fewer candidates than the cut — no boundary to decide
		}
		evaluated++
		if hits[k-1].Score == hits[k].Score {
			tied++
		}
	}
	return tied, evaluated
}

// TestTieSaturationRatchet is the ratchet. It fails when retrieval becomes MORE alphabetical.
//
// KILLING MUTATION (executed): neutralise a continuous channel in retriever.go — drop the summary-overlap
// term, or the tag-Jaccard term. Saturation rises 0.821 → 0.843 for either, and 0.907 with both gone,
// each of which clears the ceiling and turns this RED. The mutation is meaningful rather than cosmetic:
// those two terms are the ONLY continuous signals in the scorer, and they are what stands between the
// current state and a purely alphabetical top-k.
func TestTieSaturationRatchet(t *testing.T) {
	corpus := loadSeedCorpus(t)

	// VACUITY FLOOR: an empty or truncated corpus would make every assertion below trivially true. The
	// repo has paid for a vacuous guard before (axis_wiring_test.go), so the floor is explicit.
	if len(corpus) < 100 {
		t.Fatalf("vacuity floor: the shipped seed has only %d rows — a saturation number computed over "+
			"a truncated corpus certifies nothing", len(corpus))
	}

	tied, evaluated := tieSaturation(corpus, retrieveK)
	if evaluated == 0 {
		t.Fatal("vacuity floor: no query produced more than k candidates — the probe measured nothing")
	}
	got := float64(tied) / float64(evaluated)

	// Always printed, so every CI run and every later MR reprints the number rather than only reporting
	// pass/fail. A ratchet nobody reads is a constant.
	t.Logf("TIE SATURATION (leave-one-out, n=%d, k=%d): %d/%d = %.3f of top-k cuts are decided by the "+
		"alphabetical ExternalRef tiebreak, not by relevance", len(corpus), retrieveK, tied, evaluated, got)

	if got > saturationCeiling {
		t.Fatalf("tie saturation ROSE to %.3f (ceiling %.2f): retrieval became MORE alphabetical. "+
			"A discrete channel was probably added or a continuous one weakened. Do not raise the "+
			"ceiling to make this pass — the reason to raise it is always the reason not to.", got, saturationCeiling)
	}
}

// TestRuleSaturationIsTheCause pins the DIAGNOSIS, not just the symptom, so a future reader does not
// have to re-derive why the number is what it is — and so that a change which fixes the symptom while
// leaving the cause intact is visible.
//
// KILLING MUTATION: rebalance the corpus so no rule dominates (or add rows until the top-6 share falls).
// That is a legitimate way to move the number and this test would then need updating deliberately —
// which is the point: the concentration is a FACT about the corpus, and it should not change silently.
func TestRuleSaturationIsTheCause(t *testing.T) {
	corpus := loadSeedCorpus(t)
	byRule := map[string]int{}
	for _, c := range corpus {
		byRule[c.AlertRule]++
	}
	// The six commonest rules.
	counts := make([]int, 0, len(byRule))
	for _, n := range byRule {
		counts = append(counts, n)
	}
	for i := 1; i < len(counts); i++ {
		for j := i; j > 0 && counts[j] > counts[j-1]; j-- {
			counts[j], counts[j-1] = counts[j-1], counts[j]
		}
	}
	top6 := 0
	for i := 0; i < 6 && i < len(counts); i++ {
		top6 += counts[i]
	}
	share := float64(top6) / float64(len(corpus))
	t.Logf("RULE CONCENTRATION: the 6 commonest alert rules cover %d of %d rows (%.0f%%) — every query "+
		"matching one of them ties with its siblings at weightRule=%.1f before any other signal is read",
		top6, len(corpus), share*100, weightRule)

	if share < 0.40 {
		t.Fatalf("rule concentration fell to %.2f: the documented CAUSE of tie saturation no longer "+
			"holds, so TestTieSaturationRatchet's comment is now misleading and must be re-derived", share)
	}
}

// TestRecencyScoreIsBoundedAndNeverInvented pins the recency channel's contract (MECH-105/107).
//
// KILLING MUTATION: return weightRecency for a zero timestamp instead of 0. Every corpus row lacking
// provenance is then promoted as if it were fresh — inventing evidence, and specifically promoting the
// 140 seed rows (which carry no timestamp at all) over real resolved incidents that do.
func TestRecencyScoreIsBoundedAndNeverInvented(t *testing.T) {
	now := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

	if got := recencyScore(time.Time{}, now); got != 0 {
		t.Fatalf("an UNKNOWN timestamp must earn nothing, got %v — recency is never invented", got)
	}
	if got := recencyScore(now.Add(-2*recencyWindow), now); got != 0 {
		t.Fatalf("beyond the window must earn nothing, got %v", got)
	}
	fresh := recencyScore(now.Add(-time.Hour), now)
	old := recencyScore(now.Add(-recencyWindow/2), now)
	if !(fresh > old && old > 0) {
		t.Fatalf("recency must decay monotonically: fresh=%v halfway=%v", fresh, old)
	}
	// The bound is the whole safety argument: recency must never outrank a real match. weightSite is the
	// smallest genuine relevance channel, and recency sits BELOW it, so a recent-but-unrelated incident
	// can never displace an exact same-site (let alone same-rule or same-host) precedent.
	if fresh > weightSite {
		t.Fatalf("recency %v exceeds weightSite %v — a recent unrelated row could outrank a real match", fresh, weightSite)
	}
	if fresh > weightRecency {
		t.Fatalf("recency %v exceeds its own weight %v", fresh, weightRecency)
	}
}

// TestRecencyOnlyAppliesToAlreadyMatchingRows: recency is a tiebreaker among relevant precedents, never
// a reason to surface an unrelated one.
//
// KILLING MUTATION: move the recency term above the `if score > 0` guard. A fresh row matching nothing
// then scores 0.25 and is retrieved as "precedent" on the strength of being recent, which is not
// relevance at all.
func TestRecencyOnlyAppliesToAlreadyMatchingRows(t *testing.T) {
	now := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	r := &LexicalRetriever{
		nowFn: func() time.Time { return now },
		corpus: []Incident{
			{ExternalRef: "unrelated-but-fresh", Host: "other", AlertRule: "Other-rule",
				Site: "gr", ResolvedAt: now.Add(-time.Hour)},
		},
	}
	if hits := r.Retrieve(Query{Host: "app01", AlertRule: "Service-up/down", Site: "nl"}, 3); len(hits) != 0 {
		t.Fatalf("a row matching NOTHING must not be surfaced by recency alone, got %+v", hits)
	}
}

// TestPrecedentDisclosesItsAge is MECH-107's second half: SCORING age and TELLING the model about it are
// different mechanisms. The recency term (MECH-105) changes WHICH precedents are chosen; this changes how
// much the model should trust the one it was handed.
//
// KILLING MUTATION: return "" for the unknown-age case. Every row in the deployed corpus currently has an
// unknown date (0 of 670 carry one), so silence there is not an edge case — it is the common case, and to
// a model with no other cue silence reads as "recent". RED.
func TestPrecedentDisclosesItsAge(t *testing.T) {
	now := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	row := func(ref string, ts time.Time) Hit {
		return Hit{Incident: Incident{ExternalRef: ref, AlertRule: "Service-up/down", Host: "app01",
			Resolution: "restarted the unit", ResolvedAt: ts}}
	}
	out := contextAt([]Hit{
		row("fresh", now.Add(-2*24*time.Hour)),
		row("weekish", now.Add(-14*24*time.Hour)),
		row("ancient", now.Add(-200*24*time.Hour)),
		row("undated", time.Time{}),
	}, now)

	// A genuinely recent precedent needs no STALENESS caveat — a warning on everything is a warning on
	// nothing. Asserted against the staleness markers themselves rather than against "the row ends after
	// the resolution": TG-172 added an always-printed provenance note beside this one, deliberately, and a
	// positional assertion would read that as a staleness regression. What must stay true is that a fresh
	// row is not aged, not that nothing may ever follow the resolution text.
	var freshRow string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "[fresh]") {
			freshRow = line
		}
	}
	if freshRow == "" {
		t.Fatalf("the fresh row was not rendered at all, so this assertion checked nothing:\n%s", out)
	}
	for _, note := range []string{"[Note: resolved", "[Warning: resolved", "[age unknown"} {
		if strings.Contains(freshRow, note) {
			t.Fatalf("a 2-day-old precedent carried the staleness note %q:\n%s", note, freshRow)
		}
	}
	for _, want := range []string{
		"[Note: resolved 14d ago",
		"[Warning: resolved 200d ago",
		"[age unknown",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing disclosure %q in:\n%s", want, out)
		}
	}
	// The note must sit INSIDE the row it qualifies, so no later truncation of the block can separate a
	// claim from its caveat.
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "[Warning: resolved 200d") && !strings.Contains(line, "ancient") {
			t.Fatalf("the staleness note escaped its own row: %q", line)
		}
	}
}

// TestIndexedButUnreachableRefsAreDropped pins the behaviour that made a production log line lie.
//
// FusedRetriever resolves every semantic match through Base.ByRef and drops what it cannot find — the
// join that stops a stale index row resurrecting removed precedent. It also means indexing a ref that is
// NOT in the retriever's corpus makes it findable by nothing. The AWX runbook lane did exactly that and
// logged "runbooks now RAG-retrievable" on every tick.
//
// This test states the rule so the next lane that indexes into a separate corpus discovers it here rather
// than in production: EMBEDDING A REF IS NOT ENOUGH; IT MUST ALSO BE IN THE RETRIEVER'S CORPUS.
//
// KILLING MUTATION: drop the ByRef guard in FusedRetriever.Retrieve. Unreachable refs then surface as
// precedent — including rows deliberately pruned from the corpus, which is the defect the guard exists
// to prevent. RED.
func TestIndexedButUnreachableRefsAreDropped(t *testing.T) {
	inCorpus := Incident{ExternalRef: "in-corpus", Host: "app01", AlertRule: "Service-up/down", Resolution: "restarted"}
	base := NewLexicalRetriever([]Incident{inCorpus})

	// ByRef is the exact seam FusedRetriever filters on.
	if _, ok := base.ByRef("in-corpus"); !ok {
		t.Fatal("a corpus member must resolve")
	}
	if _, ok := base.ByRef("indexed-only"); ok {
		t.Fatal("a ref absent from the corpus must NOT resolve — this is what makes an index-only lane " +
			"unretrievable, and why claiming otherwise in a log line was false")
	}
}
