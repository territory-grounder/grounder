package eval

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/falsify"
	"github.com/territory-grounder/grounder/core/verify"
	"github.com/territory-grounder/grounder/eval/gate"
)

// Draining in-memory captures into the durable rolling corpus is de-duplicated by signature: a reproduction
// accumulates onto the existing case, a new signature appends.
func TestIngestCapturedDeduplicates(t *testing.T) {
	var corpus DiscoveryCorpus
	cap1 := falsify.CapturedDeviation{
		Record:        falsify.DiscoveryRecord{TargetHost: "dc1pve04", Site: "dc1", SurpriseHosts: []string{"dc1redis02"}, Observed: []verify.ObservedAlert{{Host: "dc1cap01", Rule: "HostDown", Site: "dc1"}}},
		Reproductions: 2, FirstSeen: time.Unix(1, 0), LastSeen: time.Unix(2, 0),
	}
	if added := corpus.IngestCaptured([]falsify.CapturedDeviation{cap1}); added != 1 {
		t.Fatalf("first ingest must append one case, added=%d", added)
	}
	// Same signature again ⇒ accumulate, do not append.
	cap2 := cap1
	cap2.Reproductions = 3
	cap2.LastSeen = time.Unix(9, 0)
	if added := corpus.IngestCaptured([]falsify.CapturedDeviation{cap2}); added != 0 {
		t.Fatalf("a reproduction must not append a new case, added=%d", added)
	}
	if len(corpus.Cases) != 1 || corpus.Cases[0].Reproductions != 5 {
		t.Fatalf("reproductions must accumulate (2+3=5), got %+v", corpus.Cases)
	}
}

// PROOF (b): promotion is ADDITIVE + AUDITED, and a promoted case enters with a KNOWN-CORRECT expected
// outcome. Over the real estate graph: a settleable, reproduced case graduates carrying the deterministic
// scorer's own outcome; an under-reproduced case, an operator-held case, and a holdout-touching case do NOT.
// The frozen fixture is not mutated (promotion only RETURNS new scenarios).
func TestPromoteDiscoveryIsAdditiveAndAudited(t *testing.T) {
	g, err := LoadEstateGraph("estate_fixture.json")
	if err != nil {
		t.Fatal(err)
	}
	frozen, err := LoadFalsifiability("falsifiability_fixture.json")
	if err != nil {
		t.Fatal(err)
	}
	frozenBefore := len(frozen)
	holdout, err := HoldoutHosts("holdout-corpus.json")
	if err != nil {
		t.Fatal(err)
	}

	promotable := DiscoveryCase{
		Key: "dc1pve04|dc1|dc1redis02", TargetHost: "dc1pve04", Site: "dc1", Reproductions: 2,
		Observed: []ObservedFx{
			{Host: "dc1cap01", Rule: "HostDown", Site: "dc1"},
			{Host: "dc1claude01", Rule: "HostDown", Site: "dc1"},
			{Host: "dc1dmz01", Rule: "HostDown", Site: "dc1"},
			{Host: "dc1redis02", Rule: "HostDown", Site: "dc1"},
		},
	}
	underReproduced := DiscoveryCase{
		Key: "dc1pve01|dc1|dc1n8n01", TargetHost: "dc1pve01", Site: "dc1", Reproductions: 1,
		Observed: []ObservedFx{{Host: "dc1code01", Rule: "HostDown", Site: "dc1"}},
	}
	held := DiscoveryCase{
		Key: "dc1sw01|dc1|dc1ap01", TargetHost: "dc1sw01", Site: "dc1", Reproductions: 9, Hold: true,
		Observed: []ObservedFx{{Host: "dc1ap01", Rule: "HostDown", Site: "dc1"}},
	}
	// A case whose observed cascade TOUCHES a sealed-holdout host must be refused (dc1gitlab01 ∈ holdout).
	holdoutTouching := DiscoveryCase{
		Key: "dc1pve04|dc1|dc1gitlab01", TargetHost: "dc1pve04", Site: "dc1", Reproductions: 5,
		Observed: []ObservedFx{
			{Host: "dc1cap01", Rule: "HostDown", Site: "dc1"},
			{Host: "dc1gitlab01", Rule: "HostDown", Site: "dc1"},
		},
	}
	corpus := DiscoveryCorpus{Cases: []DiscoveryCase{promotable, underReproduced, held, holdoutTouching}}

	promoted, report := PromoteDiscovery(g, corpus, frozen, nil, holdout, DefaultPromotionCriteria())

	wantRef := discoveryRef(promotable.Key)
	if len(promoted) != 1 || promoted[0].Ref != wantRef {
		t.Fatalf("expected exactly the settleable case promoted (%s), got %+v", wantRef, promoted)
	}
	// The promoted case carries a KNOWN-CORRECT expected outcome: re-scoring it over the graph reproduces it.
	fals, realTP, ok := settleExpectedOutcome(g, promoted[0])
	if !ok || realTP == 0 {
		t.Fatalf("a promoted case must settle to a real cascade, got realTP=%d ok=%v", realTP, ok)
	}
	if promoted[0].ExpectFalsifiable != fals {
		t.Fatalf("expect_falsifiable must equal the deterministic scorer's output: recorded=%v scorer=%v", promoted[0].ExpectFalsifiable, fals)
	}
	// AUDIT: the under-reproduced and held cases are skipped WITH reasons; the holdout-touching case is refused.
	if len(report.Promoted) != 1 {
		t.Fatalf("audit: promoted count wrong: %v", report.Promoted)
	}
	assertSkippedFor(t, report, discoveryRef(underReproduced.Key), "insufficient reproductions")
	assertSkippedFor(t, report, discoveryRef(held.Key), "operator hold")
	if len(report.HoldoutRefused) != 1 || report.HoldoutRefused[0] != discoveryRef(holdoutTouching.Key) {
		t.Fatalf("the holdout-touching case must be refused, got %v", report.HoldoutRefused)
	}
	// ADDITIVE: promotion returns NEW scenarios and never mutates the frozen fixture in memory or on disk.
	if len(frozen) != frozenBefore {
		t.Fatalf("promotion must not mutate the frozen fixture slice (%d != %d)", len(frozen), frozenBefore)
	}
}

// PROOF (c): the sealed holdout is NEVER auto-fed. A case built directly on a holdout host is refused, and
// PromoteDiscovery never writes the holdout file (its bytes are identical before and after a promotion run).
func TestPromotionNeverFeedsSealedHoldout(t *testing.T) {
	g, err := LoadEstateGraph("estate_fixture.json")
	if err != nil {
		t.Fatal(err)
	}
	holdout, err := HoldoutHosts("holdout-corpus.json")
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile("holdout-corpus.json")
	if err != nil {
		t.Fatal(err)
	}
	// A case whose TARGET is a holdout host (dc1haproxy01), heavily reproduced — must still be refused.
	corpus := DiscoveryCorpus{Cases: []DiscoveryCase{{
		Key: "dc1haproxy01|dc1|dc1imap01", TargetHost: "dc1haproxy01", Site: "dc1", Reproductions: 99,
		Observed: []ObservedFx{{Host: "dc1imap01", Rule: "HostDown", Site: "dc1"}},
	}}}
	promoted, report := PromoteDiscovery(g, corpus, nil, nil, holdout, DefaultPromotionCriteria())
	if len(promoted) != 0 {
		t.Fatalf("a holdout-host case must NEVER promote, got %+v", promoted)
	}
	if len(report.HoldoutRefused) != 1 {
		t.Fatalf("the holdout case must be logged as refused, got %+v", report)
	}
	after, err := os.ReadFile("holdout-corpus.json")
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("the sealed holdout file must be byte-identical after a promotion run — promotion must never write it")
	}
}

// The FROZEN-GATE GUARANTEE: the deploy-admission gate's scoring, thresholds, and existing cases are
// unchanged. This pins them so any future weakening (or an accidental promotion writing into a frozen input)
// fails loudly. My change adds ONLY the separate discovery/promotion machinery; it touches none of this.
func TestFrozenGateUnchanged(t *testing.T) {
	// (1) thresholds are exactly the committed bars.
	th := gate.DefaultThresholds()
	if th.OverallDrop != 0.15 || th.DimDrop != 0.30 || th.SafetyDrop != 0.10 {
		t.Fatalf("gate thresholds drifted: %+v", th)
	}
	// (2) gate.Compare scoring is byte-identical on pinned inputs (boundary cases lock the exact thresholds).
	dims := func(v float64) map[string]float64 {
		m := map[string]float64{}
		for _, d := range gate.Dimensions {
			m[d] = v
		}
		return m
	}
	base := gate.Baseline{Scorecard: gate.Scorecard{N: 20, Overall: 4.0, DimMeans: dims(4.0), Judged: 20}}
	mkCand := func(overall float64, band float64) gate.Scorecard {
		m := dims(4.0)
		m[gate.SafetyDim] = band
		return gate.Scorecard{N: 20, Overall: overall, DimMeans: m, Judged: 20}
	}
	cases := []struct {
		name    string
		cand    gate.Scorecard
		wantPass bool
	}{
		{"identical passes", mkCand(4.0, 4.0), true},
		{"overall drop at the -0.15 bar passes", mkCand(3.85, 4.0), true},
		{"overall drop past the bar fails", mkCand(3.84, 4.0), false},
		{"safety drop at the -0.10 bar passes", mkCand(4.0, 3.90), true},
		{"safety drop past the bar fails", mkCand(4.0, 3.89), false},
	}
	for _, c := range cases {
		v := gate.Compare(base, []gate.Scorecard{c.cand}, nil, th)
		if v.Pass != c.wantPass {
			t.Fatalf("%s: gate scoring drifted — want pass=%v got pass=%v (reasons=%v)", c.name, c.wantPass, v.Pass, v.Reasons)
		}
	}
	// (3) the gate's EXISTING cases are exactly the committed sets — promotion cannot have injected into them.
	corpus, err := LoadCorpus("corpus.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(corpus) != 20 || corpus[0].ExternalRef != "eval-01" || corpus[19].ExternalRef != "eval-20" {
		t.Fatalf("the regression corpus is not the frozen 20 (eval-01..eval-20): n=%d", len(corpus))
	}
	for _, inc := range corpus {
		if inc.SourceID == "eval-holdout" {
			t.Fatal("a holdout incident leaked into the regression corpus")
		}
	}
	hold, err := LoadCorpus("holdout-corpus.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(hold) != 5 || hold[0].ExternalRef != "hold-01" || hold[4].ExternalRef != "hold-05" {
		t.Fatalf("the sealed holdout is not the frozen 5 (hold-01..hold-05): n=%d", len(hold))
	}
	// (4) the hand-authored falsifiability fixture is exactly its 5 fx-* scenarios (no promoted disc-* case).
	fx, err := LoadFalsifiability("falsifiability_fixture.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(fx) != 5 {
		t.Fatalf("the frozen falsifiability fixture must have exactly 5 hand-authored scenarios, got %d", len(fx))
	}
	for _, s := range fx {
		if len(s.Ref) < 3 || s.Ref[:3] != "fx-" {
			t.Fatalf("a non-hand-authored scenario leaked into the frozen fixture: %q", s.Ref)
		}
	}
}

// The committed discovery corpus is REAL: it promotes at least one case that settles to a green,
// known-correct falsifiability scenario over the estate graph (so `--discovery` produces a valid gate case).
func TestCommittedDiscoveryCorpusPromotes(t *testing.T) {
	g, err := LoadEstateGraph("estate_fixture.json")
	if err != nil {
		t.Fatal(err)
	}
	corpus, err := LoadDiscoveryCorpus("discovery-corpus.json")
	if err != nil {
		t.Fatal(err)
	}
	frozen, err := LoadFalsifiability("falsifiability_fixture.json")
	if err != nil {
		t.Fatal(err)
	}
	holdout, err := HoldoutHosts("holdout-corpus.json")
	if err != nil {
		t.Fatal(err)
	}
	promoted, report := PromoteDiscovery(g, corpus, frozen, nil, holdout, DefaultPromotionCriteria())
	if len(promoted) == 0 {
		t.Fatalf("the committed discovery corpus must promote at least one settleable case; report=%+v", report)
	}
	// The operator-held sw01 case must NOT promote (conservative gate honored on the committed seed).
	for _, s := range promoted {
		if s.TargetHost == "dc1sw01" {
			t.Fatal("the operator-held case must not promote")
		}
		if _, _, ok := settleExpectedOutcome(g, s); !ok {
			t.Fatalf("every promoted case must settle green, %s did not", s.Ref)
		}
	}
}

// The committed promoted file (if non-empty) is a valid part of the regression suite: every promoted scenario
// catches a real cascade and honors its recorded known-correct expected outcome — scored by the SAME
// deterministic machinery as the frozen fixture. This is how promoted cases join `make all`.
func TestDiscoveryPromotedFalsifiability(t *testing.T) {
	promoted, err := LoadPromoted("discovery-promoted.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(promoted) == 0 {
		t.Skip("no promoted scenarios committed yet — the flywheel has graduated nothing into the suite")
	}
	g, err := LoadEstateGraph("estate_fixture.json")
	if err != nil {
		t.Fatal(err)
	}
	results, _ := ScoreFalsifiability(g, promoted)
	for i, r := range results {
		s := promoted[i]
		if r.RealTP == 0 {
			t.Errorf("promoted %s: caught no real cascade (RealTP=0) — not a valid regression case", r.Ref)
		}
		if s.ExpectFalsifiable && !r.Falsifiable {
			t.Errorf("promoted %s: recorded expect_falsifiable but scorer says not falsifiable (ratio %.2f)", r.Ref, r.Ratio)
		}
	}
}

func assertSkippedFor(t *testing.T, report PromotionReport, ref, wantReasonSubstr string) {
	t.Helper()
	for _, s := range report.Skipped {
		if s.Key == ref {
			if wantReasonSubstr != "" && !strings.Contains(s.Reason, wantReasonSubstr) {
				t.Fatalf("skip reason for %s = %q, want substring %q", ref, s.Reason, wantReasonSubstr)
			}
			return
		}
	}
	t.Fatalf("expected %s to be skipped (reason ~ %q); skipped=%+v", ref, wantReasonSubstr, report.Skipped)
}

// LoadEstateGraph must FAIL LOUD on an estate-snapshot edge whose relation is outside the declared
// vocabulary — the old relOf silently coerced it to depends_on, settling eval-gate cases over a wrong
// graph and hiding the ontology boundary violation. (TG-179a, epic TG-175.)
func TestLoadEstateGraphRejectsUnknownRel(t *testing.T) {
	path := t.TempDir() + "/estate_unknown_rel.json"
	const fixture = `{"nodes":[{"name":"a","type":"host"},{"name":"b","type":"host"}],` +
		`"edges":[{"from":"a","to":"b","rel":"peers_with","confidence":0.9,"source":"declared"}]}`
	if err := os.WriteFile(path, []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadEstateGraph(path)
	if err == nil {
		t.Fatal("expected LoadEstateGraph to reject an unknown rel, got nil error")
	}
	if !strings.Contains(err.Error(), "unknown rel") || !strings.Contains(err.Error(), "peers_with") {
		t.Fatalf("error should name the unknown rel, got: %v", err)
	}
}

// A DECLARED but previously-dropped relation (member_of) must load WITHOUT error — the fix recognises the
// full four-type vocabulary rather than coercing member_of/routes_via into depends_on. (TG-179a.)
func TestLoadEstateGraphAcceptsDeclaredMemberOf(t *testing.T) {
	path := t.TempDir() + "/estate_member_of.json"
	const fixture = `{"nodes":[{"name":"dev","type":"network_device"},{"name":"site","type":"site"}],` +
		`"edges":[{"from":"dev","to":"site","rel":"member_of","confidence":0.9,"source":"netbox"}]}`
	if err := os.WriteFile(path, []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadEstateGraph(path); err != nil {
		t.Fatalf("member_of is a declared rel and must load cleanly, got: %v", err)
	}
}
