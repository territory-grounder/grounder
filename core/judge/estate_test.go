package judge

import (
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/estate"
	"github.com/territory-grounder/grounder/core/proposal"
)

// THE DEFECT THIS DIMENSION CLOSES (TG-202). `core/judge` had ZERO estate references: TG's causal
// infrastructure model and its evaluator plane never spoke, so the rubric asked an LLM "does the reasoning fit
// the alert + host" with no access to the runs_on/depends_on structure that actually decides it. A diagnosis
// blaming a hypervisor the alerting guest does not run on scored exactly like a correct one.
//
// The tests below pin BOTH halves of the ticket: the graph must be able to say NO, and it must be unable to
// say NO from ignorance.

// estateFixture is the two-node-two-guest estate every test here reasons over:
//
//	vm-a  --runs_on--> node-1        vm-b --runs_on--> node-2
//	svc-x --depends_on--> vm-a       (so svc-x reaches node-1 at two hops)
//
// vm-a and node-2 are the topologically impossible pair; vm-a and vm-b are siblings only if they share a
// parent, which they deliberately do NOT (they sit on different nodes) — the sibling case builds its own.
func estateFixture(t *testing.T) *estate.Graph {
	t.Helper()
	g := estate.NewGraph()
	g.Upsert(estate.Edge{From: estate.Entity{Type: estate.TypeVM, Name: "vm-a"}, To: estate.Entity{Type: estate.TypePVENode, Name: "node-1"},
		Rel: estate.RelRunsOn, Confidence: estate.SourceConfidence[estate.SourcePVE], Source: estate.SourcePVE})
	g.Upsert(estate.Edge{From: estate.Entity{Type: estate.TypeVM, Name: "vm-b"}, To: estate.Entity{Type: estate.TypePVENode, Name: "node-2"},
		Rel: estate.RelRunsOn, Confidence: estate.SourceConfidence[estate.SourcePVE], Source: estate.SourcePVE})
	g.Upsert(estate.Edge{From: estate.Entity{Type: estate.TypeService, Name: "svc-x"}, To: estate.Entity{Type: estate.TypeVM, Name: "vm-a"},
		Rel: estate.RelDependsOn, Confidence: estate.SourceConfidence[estate.SourceDeclared], Source: estate.SourceDeclared})
	if g.Len() != 3 {
		t.Fatalf("fixture graph has %d edges, want 3 — a vacuous fixture would make every assertion below pass for the wrong reason", g.Len())
	}
	return g
}

// claimed builds a session whose typed diagnosis states rootCause, on the given alerting host.
func claimed(host, rootCause string) Session {
	return Session{Ref: "TG-est", Host: host, Proposed: true, DiagnosisRecorded: true,
		Diagnosis: proposal.Diagnosis{RootCause: rootCause}}
}

func scoreAgainst(t *testing.T, g *estate.Graph, s Session) (int, string, bool, EstateFacts) {
	t.Helper()
	s.Estate = GroundInEstate(g, s)
	v, why, ok := ScoreEstateGrounded(s)
	return v, why, ok, s.Estate
}

// KILLING MUTATION (the ticket's): make GroundInEstate ignore the graph — e.g. return the zero EstateFacts,
// or drop the RelationUnrelated branch so an unreachable cause reads as "connected". RED here with the message
// below: the judge scores a diagnosis that blames a hypervisor the alerting guest does not run on exactly as
// it scores the correct one, which is the whole defect (`core/judge` had zero estate references).
func TestATopologicallyImpossibleCauseIsPenalised(t *testing.T) {
	g := estateFixture(t)

	bad, badWhy, ok, badFacts := scoreAgainst(t, g, claimed("vm-a", "node-2 exhausted its memory and the guest was killed"))
	if !ok {
		t.Fatal("a claim naming a host the graph knows was scored N/A — the estate axis cannot grade anything, " +
			"so the causal graph is wired to the judge and produces nothing (TG-202's defect, restored)")
	}
	if badFacts.Relation != RelationUnrelated {
		t.Fatalf("relation=%q, want %q: node-2 has no path to the alerting vm-a in this estate", badFacts.Relation, RelationUnrelated)
	}
	good, _, ok, goodFacts := scoreAgainst(t, g, claimed("vm-a", "node-1 exhausted its memory and the guest was killed"))
	if !ok {
		t.Fatal("the CORRECT diagnosis (the guest's own hypervisor) was scored N/A")
	}
	if goodFacts.Relation != RelationAdjacent || goodFacts.Direction != DirectionUpstream {
		t.Fatalf("relation=%q direction=%q, want adjacent/upstream: vm-a runs_on node-1", goodFacts.Relation, goodFacts.Direction)
	}
	if bad >= good {
		t.Fatalf("blaming an unrelated hypervisor scored %d and blaming the guest's OWN hypervisor scored %d — "+
			"the judge is grading a root cause with no access to the estate graph, so a topologically impossible "+
			"claim is indistinguishable from a correct one and the agent pays nothing for proposing a restart on "+
			"the wrong machine (%q)", bad, good, badWhy)
	}
	if bad != 2 || good != 5 {
		t.Errorf("scores %d (impossible) / %d (adjacent), want 2 / 5", bad, good)
	}
	if !strings.Contains(badWhy, "node-2") || !strings.Contains(badWhy, "vm-a") {
		t.Errorf("the written reason must name both entities — it is what an operator reads on the row; got %q", badWhy)
	}
	// The supporting facts are the graph's, not a model's: the provenance and confidence of the winning edge.
	if goodFacts.Source != string(estate.SourcePVE) || goodFacts.Rel != string(estate.RelRunsOn) {
		t.Errorf("adjacency facts lost their provenance: rel=%q source=%q, want runs_on/pve", goodFacts.Rel, goodFacts.Source)
	}
}

// THE OTHER HALF OF THE TICKET, AND THE MORE IMPORTANT ONE: absence of a path must NOT automatically mean a
// wrong diagnosis. The estate graph is a MODEL — sources go down, edges expire, hosts are missing. If "no path
// found" scored as "impossible", a CMDB outage would mark every diagnosis in the estate wrong at once (TG-61's
// global floor, wearing a different hat).
//
// KILLING MUTATION: drop the guards in GroundInEstate — return RelationUnrelated for an unresolvable name, or
// remove the `!(SymptomKnown && CauseKnown)` demotion. RED on every case below.
func TestTheGraphNotKnowingIsNeverAWrongDiagnosis(t *testing.T) {
	g := estateFixture(t)
	cases := []struct {
		name    string
		graph   *estate.Graph
		session Session
		why     string
	}{
		{"no estate wired at all", nil, claimed("vm-a", "node-2 ran out of memory"),
			"a deployment with no estate graph must score exactly as it did before this shipped"},
		{"empty snapshot", estate.NewGraph(), claimed("vm-a", "node-2 ran out of memory"),
			"a zero-edge graph is an outage or a cold boot, not a world in which nothing depends on anything"},
		{"the alerting host is not in the graph", g, claimed("unknown-host", "node-2 ran out of memory"),
			"the graph cannot place the thing that alerted, so it cannot speak to what caused it"},
		{"the claimed cause is not in the graph", g, claimed("vm-a", "the upstream billing API timed out"),
			"a cause the graph has never heard of is not a claim about this estate"},
		{"the claim names no machine at all", g, claimed("vm-a", "journald grew unbounded because vacuuming is disabled"),
			"a mechanism-only claim names no entity to place — there is nothing topological to check"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, _, ok, f := scoreAgainst(t, tc.graph, tc.session)
			if ok {
				t.Fatalf("scored %d/5 where the graph DOES NOT KNOW (relation=%q) — %s. An evaluator that reads "+
					"its own blind spots as the agent being wrong punishes the agent for the estate's gaps and "+
					"floors the dimension across the whole population the moment a source goes down", v, f.Relation, tc.why)
			}
			if f.Relation != RelationUnknown {
				t.Fatalf("relation=%q, want the honest unknown", f.Relation)
			}
		})
	}
}

// STALE KNOWLEDGE IS NOT KNOWLEDGE. Both endpoints are in the index — but every edge that mentioned the named
// cause has EXPIRED, so the graph's silence about a path is the silence of an out-of-date model. The row must
// not be written. (This is the case a naive `Resolve() == ok` check gets wrong: the name is there, the
// topology is gone.)
func TestExpiredTopologyCannotAssertUnrelatedness(t *testing.T) {
	past := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	now := func() time.Time { return time.Date(2026, 8, 4, 0, 0, 0, 0, time.UTC) }
	g := estate.NewGraph(estate.WithClock(now))
	g.Upsert(estate.Edge{From: estate.Entity{Type: estate.TypeVM, Name: "vm-a"}, To: estate.Entity{Type: estate.TypePVENode, Name: "node-1"},
		Rel: estate.RelRunsOn, Confidence: 0.95, Source: estate.SourcePVE})
	// node-9's only edge lapsed three days ago: the graph remembers the NAME and nothing current about it.
	g.Upsert(estate.Edge{From: estate.Entity{Type: estate.TypeVM, Name: "vm-z"}, To: estate.Entity{Type: estate.TypePVENode, Name: "node-9"},
		Rel: estate.RelRunsOn, Confidence: 0.95, Source: estate.SourcePVE, ValidUntil: past})

	if _, ok := g.Resolve("node-9"); !ok {
		t.Fatal("fixture broken: node-9 must still RESOLVE, or this test proves nothing about stale edges")
	}
	v, _, ok, f := scoreAgainst(t, g, claimed("vm-a", "node-9 lost its storage"))
	if ok {
		t.Fatalf("scored %d/5 (relation=%q) off an entity whose every edge has EXPIRED — a name in the index is "+
			"not live topology, and reading it as such turns an out-of-date estate into a verdict that the "+
			"agent's diagnosis is impossible", v, f.Relation)
	}
	if f.SymptomKnown != true || f.CauseKnown != false || f.Cause != "node-9" {
		t.Fatalf("known-flags wrong: symptom=%v cause=%q known=%v — the block must NAME the endpoint the graph "+
			"is blind to, or an operator reading an unscored session cannot tell a stale estate from an agent "+
			"error", f.SymptomKnown, f.Cause, f.CauseKnown)
	}
}

// A HEURISTIC EDGE IS NOT GROUND TRUTH EITHER. The co-occurrence learner's confidence is hard-capped at 0.75,
// deliberately below the 0.80 cutoff, precisely because it is guessing. An entity the graph knows ONLY through
// a learned edge cannot underwrite "these two are unrelated".
func TestLearnedOnlyKnowledgeCannotAssertUnrelatedness(t *testing.T) {
	g := estateFixture(t)
	g.Upsert(estate.Edge{From: estate.Entity{Type: estate.TypeHost, Name: "guessed-host"}, To: estate.Entity{Type: estate.TypeHost, Name: "other-guess"},
		Rel: estate.RelDependsOn, Confidence: estate.LearnedConfidence(100), Source: estate.SourceIncident})
	if estate.LearnedConfidence(100) >= estate.GroundTruthCutoff {
		t.Fatal("fixture broken: the learned cap must sit below the ground-truth cutoff")
	}
	v, _, ok, f := scoreAgainst(t, g, claimed("vm-a", "guessed-host stopped answering"))
	if ok {
		t.Fatalf("scored %d/5 (relation=%q) against an entity known only from capped incident co-occurrence — "+
			"a heuristic that is capped BELOW the suppression cutoff because it may be wrong cannot be the "+
			"evidence that an agent's diagnosis is impossible", v, f.Relation)
	}
}

// A MIXED CLAIM IS NOT A REFUTED CLAIM. The diagnosis names one machine the graph knows and cannot connect,
// and one whose topology the graph has lost. The unreachable one may be a bystander the claim merely mentions
// and the blind one may be the real cause — so the honest verdict is silence, not a penalty assembled out of
// the half the estate happens to remember.
func TestAClaimNamingOneUnknowableEntityIsNotRefuted(t *testing.T) {
	past := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	now := func() time.Time { return time.Date(2026, 8, 4, 0, 0, 0, 0, time.UTC) }
	g := estate.NewGraph(estate.WithClock(now))
	g.Upsert(estate.Edge{From: estate.Entity{Type: estate.TypeVM, Name: "vm-a"}, To: estate.Entity{Type: estate.TypePVENode, Name: "node-1"},
		Rel: estate.RelRunsOn, Confidence: 0.95, Source: estate.SourcePVE})
	g.Upsert(estate.Edge{From: estate.Entity{Type: estate.TypeVM, Name: "vm-b"}, To: estate.Entity{Type: estate.TypePVENode, Name: "node-2"},
		Rel: estate.RelRunsOn, Confidence: 0.95, Source: estate.SourcePVE}) // known, and unreachable from vm-a
	g.Upsert(estate.Edge{From: estate.Entity{Type: estate.TypeVM, Name: "vm-z"}, To: estate.Entity{Type: estate.TypePVENode, Name: "node-9"},
		Rel: estate.RelRunsOn, Confidence: 0.95, Source: estate.SourcePVE, ValidUntil: past}) // knowledge lapsed

	// Sanity: node-2 ALONE would score the impossibility floor. The mixed claim must not.
	if v, _, ok, _ := scoreAgainst(t, g, claimed("vm-a", "node-2 exhausted its memory")); !ok || v != 2 {
		t.Fatalf("fixture broken: the node-2-only claim scored %d (applicable=%v), want the floor 2", v, ok)
	}
	v, _, ok, f := scoreAgainst(t, g, claimed("vm-a", "node-9 lost its storage, which also disturbed node-2"))
	if ok {
		t.Fatalf("a claim naming node-9 (topology lapsed) alongside node-2 scored %d/5 (relation=%q) — the "+
			"entity the graph is blind to may be the real cause and the reachable-looking one a bystander; "+
			"a verdict assembled from the half of the estate that happens to be fresh is the estate's gap "+
			"deciding the session", v, f.Relation)
	}
}

// A FUZZY-TIER MATCH IS A GUESS ABOUT IDENTITY, and the axis refuses to score off it in EITHER direction —
// it will not penalise a diagnosis for a host it only guessed at, and it will not reward one either. The
// asymmetric version (reward on a guess, never penalise) is the tempting one and it is exploitable: a
// near-miss hostname would buy full marks.
func TestFuzzyResolutionNeitherRewardsNorPenalises(t *testing.T) {
	g := estate.NewGraph()
	g.Upsert(estate.Edge{From: estate.Entity{Type: estate.TypeVM, Name: "vm-a"}, To: estate.Entity{Type: estate.TypePVENode, Name: "dc1-pve01"},
		Rel: estate.RelRunsOn, Confidence: 0.95, Source: estate.SourcePVE})
	// The separator-folded form resolves — through the FUZZY tier, which is a well-founded guess, not an
	// identity confirmation.
	if _, tier, ok := g.ResolveTiered("dc1pve01"); !ok || tier != estate.TierFuzzy {
		t.Fatalf("fixture broken: expected the fuzzy tier to answer, got tier=%q ok=%v", tier, ok)
	}
	v, _, ok, f := scoreAgainst(t, g, claimed("vm-a", "dc1pve01 exhausted its memory"))
	if ok {
		t.Fatalf("scored %d/5 (relation=%q) off a FUZZY identity match — the separator-folded tier resolves "+
			"dc1pve01 to dc1-pve01 as a guess; scoring on it lets a near-miss hostname buy marks and "+
			"lets a genuine near-miss be condemned", v, f.Relation)
	}
	if f.CauseTier != "" {
		t.Errorf("a fuzzy match must not be recorded as a resolved cause, got tier=%q", f.CauseTier)
	}
}

// COMMON-CAUSE SIBLINGS ARE NOT "UNRELATED". Two guests on one silent PVE node co-fail (the 2026-05-08
// pattern); a reachability-only check would call that claim impossible. It is not corroborated either — the
// shared parent is usually the real cause — so it lands between the two.
func TestSiblingsScoreBetweenCorroborationAndImpossibility(t *testing.T) {
	g := estate.NewGraph()
	for _, guest := range []string{"vm-a", "vm-b"} {
		g.Upsert(estate.Edge{From: estate.Entity{Type: estate.TypeVM, Name: guest}, To: estate.Entity{Type: estate.TypePVENode, Name: "node-1"},
			Rel: estate.RelRunsOn, Confidence: 0.95, Source: estate.SourcePVE})
	}
	sib, _, ok, f := scoreAgainst(t, g, claimed("vm-a", "vm-b is saturating the shared host"))
	if !ok {
		t.Fatal("a sibling claim was scored N/A — both entities are known and the graph has plenty to say about them")
	}
	if f.Relation != RelationSibling {
		t.Fatalf("relation=%q, want sibling: vm-a and vm-b share node-1", f.Relation)
	}
	if sib != 3 {
		t.Fatalf("sibling claim scored %d, want 3 — co-failure under a shared parent is neither corroboration "+
			"nor impossibility, and scoring it as either loses the most common cascade shape TG sees", sib)
	}
}

// A MULTI-HOP PATH IS STILL A PATH, and it must score below a direct edge: the further the cause is from the
// symptom, the weaker the claim (the path product says so numerically, and the score must move with it).
func TestAMultiHopPathScoresBelowADirectEdge(t *testing.T) {
	g := estateFixture(t)
	// svc-x --depends_on--> vm-a --runs_on--> node-1: the alert is on node-1, the blamed cause two hops down.
	far, _, ok, f := scoreAgainst(t, g, claimed("node-1", "svc-x went into a crash loop"))
	if !ok {
		t.Fatal("a two-hop causal claim was scored N/A")
	}
	if f.Relation != RelationConnected || f.Distance != 2 {
		t.Fatalf("relation=%q distance=%d, want connected at 2 hops", f.Relation, f.Distance)
	}
	if f.Direction != DirectionDownstream {
		t.Fatalf("direction=%q — svc-x depends on the alerting node, so the cause is DOWNSTREAM; recording it as "+
			"upstream would misstate which way the dependency runs", f.Direction)
	}
	near, _, _, _ := scoreAgainst(t, g, claimed("node-1", "vm-a went into a crash loop"))
	if far >= near {
		t.Fatalf("a two-hop cause scored %d and a one-hop cause %d — distance must cost, or 'has a path' collapses "+
			"into a single bit and the path-product confidence the graph computes is thrown away", far, near)
	}
}

// THE DEPENDENT THAT HARMS ITS HOST IS A REAL CAUSE. A container filling its node's disk explains the node's
// alert; only checking "does the symptom depend on the cause" would score that correct diagnosis as impossible.
func TestADownstreamCauseIsNotImpossible(t *testing.T) {
	g := estateFixture(t)
	v, _, ok, f := scoreAgainst(t, g, claimed("node-1", "vm-a filled the shared storage"))
	if !ok {
		t.Fatal("scored N/A")
	}
	if f.Relation != RelationAdjacent || f.Direction != DirectionDownstream || v != 5 {
		t.Fatalf("relation=%q direction=%q score=%d — a guest filling its hypervisor's disk is a textbook cause; "+
			"a one-directional reachability check would call it topologically impossible", f.Relation, f.Direction, v)
	}
}

// A CLAIM NAMING SEVERAL MACHINES IS JUDGED BY ITS BEST-CONNECTED ONE. Mentioning an unrelated jump host
// alongside the correct hypervisor is not a topologically impossible claim, and reading the worst token would
// manufacture one.
func TestAClaimIsJudgedByItsBestConnectedEntity(t *testing.T) {
	g := estateFixture(t)
	v, _, ok, f := scoreAgainst(t, g, claimed("vm-a", "node-1 exhausted memory; noticed while node-2 was being patched"))
	if !ok {
		t.Fatal("scored N/A")
	}
	if v != 5 || f.Cause != "node-1" {
		t.Fatalf("score=%d cause=%q — the claim names the guest's OWN hypervisor and one bystander; judging it by "+
			"the bystander invents an impossibility the agent never claimed", v, f.Cause)
	}
	if f.Claimed < 2 || f.Resolved < 2 {
		t.Fatalf("claimed=%d resolved=%d — the token scan must SEE both machines; a scan that silently matches "+
			"nothing would make this dimension N/A everywhere and nobody would notice", f.Claimed, f.Resolved)
	}
}

// VACUITY FLOOR for the token scan itself: it must find the entity a realistically-worded claim names, in every
// reference form the estate actually uses. A scan that matched nothing would leave the whole dimension silently
// N/A forever — wired, and producing nothing.
func TestTheClaimScanFindsTheEntityItNames(t *testing.T) {
	g := estateFixture(t)
	forms := []string{
		"node-1 exhausted its memory",                   // bare
		"the hypervisor node-1.mgmt.lan lost its NVMe",  // domain-qualified
		"NODE-1 rebooted unexpectedly",                  // case variant (alias tier)
		"memory pressure on node-1, oom-killer engaged", // mid-sentence, punctuation-adjacent
	}
	for _, form := range forms {
		v, _, ok, f := scoreAgainst(t, g, claimed("vm-a", form))
		if !ok || f.Cause == "" {
			t.Fatalf("the claim %q named node-1 and the scan resolved nothing (claimed=%d resolved=%d) — a "+
				"dimension whose scan never matches is N/A on every session forever, which reads on the "+
				"scorecard exactly like a feature that was never wired", form, f.Claimed, f.Resolved)
		}
		if v != 5 {
			t.Errorf("claim %q scored %d, want 5 (node-1 is the alerting guest's hypervisor)", form, v)
		}
	}
}

// The axis's NAME comes from the one rubric source, and it must not have been smuggled into the LLM reply
// schema: showing the model the graph and asking it to read the edges is the WEAKER form this dimension
// replaces (it re-opens a checkable proposition as free text — INV-10).
func TestTheEstateAxisIsDeterministicAndNotAnLLMDimension(t *testing.T) {
	if DimEstateGrounded != "estate_grounded" {
		t.Fatalf("dimension name %q — the durable session_judgment rows are keyed on it", DimEstateGrounded)
	}
	var declared bool
	for _, d := range LoadedRubric().DeterministicDimensions {
		if d == DimEstateGrounded {
			declared = true
		}
	}
	if !declared {
		t.Fatal("DimEstateGrounded is not declared in rubric.json — a second declaration is a second thing to drift")
	}
	for _, d := range Dimensions {
		if d == DimEstateGrounded {
			t.Fatal("estate_grounded entered judge.Dimensions — that asks the judge MODEL to re-author a fact " +
				"computed by traversal, and widens the eval Overall denominator so every historical scorecard " +
				"moves for a reason unrelated to agent quality")
		}
	}
	if strings.Contains(Prompt(goldenSession), DimEstateGrounded) || strings.Contains(Prompt(goldenSession), "estate graph") {
		t.Fatal("the estate axis leaked into the judge prompt — the model must not be asked to read the graph")
	}
	if EstateRule() == "" {
		t.Fatal("the dimension ships with no written calibration — an axis nobody can read the rule for is one nobody can audit")
	}
	if !strings.Contains(EstateRule(), "N/A") {
		t.Fatal("the rule must state the N/A case in the ONE place an operator reads it: a graph that does not " +
			"know scores nothing at all")
	}
}
