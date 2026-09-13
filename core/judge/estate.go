package judge

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/territory-grounder/grounder/core/estate"
)

// ESTATE_GROUNDED — the judge dimension that checks a stated cause against the CAUSAL GRAPH (TG-202).
//
// ★ WHY THIS EXISTS. Until this file, `core/judge` had ZERO estate references. TG carries a multi-source
// causal infrastructure model (core/estate: 725 entities / 701 edges, runs_on + depends_on + routes_via,
// confidence-graded by source) and an evaluator plane, and the two never spoke. So the rubric asked an LLM
// "does the reasoning fit the alert + host" — an opinion formed with NO ACCESS to the topology that actually
// decides it. A diagnosis blaming a hypervisor the alerting guest does not run on, or a dependency that does
// not exist, read exactly like a correct one: fluent, specific, and topologically impossible.
//
// ★ WHY IT IS MACHINE-COMPUTED AND NOT ASKED OF THE MODEL. The weaker version of this idea hands the graph
// to the judge model and asks it to read the edges. That re-opens a checkable proposition — "the triple
// (cause, depends_on, symptom) is not in the graph" — as free text, in the same channel the untrusted session
// prose arrives on (INV-08/INV-10). Every fact in EstateFacts is computed here, by traversal, from a snapshot
// the model cannot author; the model is never shown the block and never scores this axis (it is declared in
// rubric.json's deterministic_dimensions beside diagnosis_grounded, and deliberately kept out of
// judge.Dimensions — the LLM reply schema and the eval Overall's fixed denominator).
//
// ★ THE CONSTRAINT THAT SHAPES EVERYTHING BELOW: ABSENCE OF A PATH IS NOT EVIDENCE OF A WRONG DIAGNOSIS.
// The estate graph is a MODEL of the estate, refreshed from sources that go down, seeded with edges that
// expire, and enriched by a co-occurrence learner whose confidence is capped at 0.75 precisely because it is
// guessing. If "no path found" meant "wrong", then a CMDB outage — the moment the graph thins out — would
// mark every diagnosis in the estate topologically impossible at once, which is the TG-61 global-floor
// failure wearing a different hat. So the axis distinguishes two things a naive check conflates:
//
//	the graph SAYS they are unrelated   — both endpoints resolved exactly, both carry live observed edges
//	                                      (estate.HasGroundTruth), and no path/sibling relation exists.
//	                                      That is a proposition, and it is scored.
//	the graph DOES NOT KNOW             — no graph wired, an empty snapshot, an unresolvable name, a name
//	                                      only the FUZZY tier could guess at, or an endpoint whose every
//	                                      edge is expired or below the ground-truth cutoff. N/A: NO ROW IS
//	                                      WRITTEN, never a floored one.
//
// TestTheGraphNotKnowingIsNeverAWrongDiagnosis pins that distinction; it is the difference between an
// evaluator that grounds a claim and one that punishes the agent for the estate's own gaps.

// DimEstateGrounded is the dimension name written to session_judgment.dimension, sourced from the ONE rubric
// (never re-declared here, so the stored name and the rubric's declaration cannot drift apart).
var DimEstateGrounded = mustDeterministicDim("estate_grounded")

// EstateRelation is the graph's deterministic reading of the relationship between the diagnosis's named cause
// and the symptom's target. It is a FACT about the snapshot that was consulted, not a judgement.
type EstateRelation string

const (
	// RelationUnknown is the honest default: the graph cannot speak to this claim. It is also the zero value,
	// so a caller that never computed the block writes no row rather than a silent floor.
	RelationUnknown EstateRelation = ""
	// RelationSelf — the named cause IS the alerting entity ("dc1pve01 is out of memory" on that host).
	RelationSelf EstateRelation = "self"
	// RelationAdjacent — a single dependency edge joins them, in either direction.
	RelationAdjacent EstateRelation = "adjacent"
	// RelationConnected — a multi-hop dependency path joins them (path-product confidence recorded).
	RelationConnected EstateRelation = "connected"
	// RelationSibling — no directed path, but they share an infrastructure parent: the common-cause shape
	// (the 2026-05-08 pattern, 4 VMs flapping on one silent PVE node) that a pure reachability walk misses.
	RelationSibling EstateRelation = "sibling"
	// RelationUnrelated — the graph knows both endpoints through live observed edges and joins them by
	// nothing. This is the graph-checkable proposition the dimension exists to price.
	RelationUnrelated EstateRelation = "unrelated"
)

// Direction records which way the dependency runs, because both directions are legitimate causes and reading
// them as one would lose the fact: UPSTREAM is the classic cascade (the guest depends on the hypervisor that
// failed), DOWNSTREAM is the dependent that harms what it runs on (a container filling its node's disk).
const (
	DirectionUpstream   = "upstream"
	DirectionDownstream = "downstream"
)

// estateWalkDepth bounds the reachability walk — the same default depth estate.BlastRadius applies, named
// here so the judge's reading of "has a path" is a stated bound rather than an accident of the callee.
const estateWalkDepth = 3

// minClaimToken is the shortest token treated as a possible entity name. Below three characters a token is
// noise ("of", "a"), and the resolver's alias tier would happily match a two-letter entity name against an
// English word.
const minClaimToken = 3

// EstateFacts is the machine-computed estate block on a judged Session — read-only, derived by traversal, and
// never authored by a model. Every field is either a direct graph fact or the honest statement that the graph
// had nothing to say.
type EstateFacts struct {
	// Consulted is whether a graph was offered at all. False on a deployment with no estate wired into the
	// judge — the axis is then N/A everywhere, which is the correct backward-compatible default (an existing
	// deployment's scores do not move on the day this ships).
	Consulted bool
	// GraphEdges is the size of the snapshot consulted. A zero-edge graph is an OUTAGE or a cold boot, never a
	// world in which nothing depends on anything: it can corroborate nothing and refute nothing.
	GraphEdges int
	// Symptom is the resolved alerting entity and SymptomTier which resolution tier found it ("" = the alert
	// host is not in the graph at all).
	Symptom      string
	SymptomTier  string
	SymptomKnown bool // the graph holds live, observed (>= GroundTruthCutoff) edges touching it
	// Cause is the token from the typed diagnosis that resolved to an estate entity — the claim's named cause
	// as the GRAPH understands it — with the tier that resolved it and whether the graph really knows it.
	Cause      string
	CauseTier  string
	CauseKnown bool
	// Claimed is how many entity-shaped tokens the stated claim contained, and Resolved how many of them the
	// graph could place. Both are recorded so a run where the axis is silently N/A everywhere (a claim grammar
	// that stopped naming hosts, a resolver regression) is VISIBLE as zeros rather than as an absent dimension
	// nobody notices — the vacuity guard for the token scan.
	Claimed  int
	Resolved int
	// Relation is the verdict; Direction, Rel, Source, Confidence and Distance are the supporting facts (the
	// edge kind and provenance are only known for a single-hop relation — a multi-hop path has neither).
	Relation   EstateRelation
	Direction  string
	Rel        string
	Source     string
	Confidence float64
	Distance   int
}

// EstateApplicable reports whether estate_grounded is a meaningful axis for this session: only when the graph
// actually reached a verdict. Same rule and same reason as PredictionApplicable and DiagnosisApplicable — a
// dimension that is not meaningful for a session is OMITTED, never scored 1 (TG-61 seq C).
func EstateApplicable(s Session) bool {
	return s.Estate.Consulted && s.Estate.Relation != RelationUnknown
}

// NAReason names WHY estate_grounded was not applicable to a session, for the one purpose of making a
// permanently-N/A axis diagnosable.
//
// Measured 2026-08-05: across 3,233 judged sessions this axis had written ZERO rows and diagnosis_grounded
// had written ONE, while the four model-scored dimensions each had 3,233. The axis was wired at the
// composition root, the scorer ran on every session, and it was correctly declining to score — but nothing
// anywhere said which of its four gates was doing the declining, so "the axis has not accrued samples yet"
// (the precondition TG-307 and TG-314 both wait on) was indistinguishable from "the axis can never accrue
// samples in this deployment".
//
// N/A is the right BEHAVIOUR — a dimension that is not meaningful must be omitted, never floored (TG-61
// seq C). Being unable to say why is not.
const (
	NAWired      = "no-estate-wired"     // no graph handed to the judge at all
	NAEmptyGraph = "empty-graph"         // a snapshot with zero edges: an outage or a cold boot
	NAUnplaced   = "symptom-unplaced"    // the graph cannot place the alerting entity above TierFuzzy
	NAUnrelated  = "no-relation-derived" // symptom placed, but no claimed entity yielded a relation
	NAApplicable = ""                    // applicable: a row was written
)

// NAReason returns the first gate that made this axis N/A, or NAApplicable when it was scored. The order
// matches the gates in GroundInEstate, so the reason names the EARLIEST cause rather than a later symptom
// of it — an empty graph also cannot place a symptom, and reporting "symptom-unplaced" there would send
// someone to look at hostname resolution when the real answer is that the estate never seeded.
func (f EstateFacts) NAReason() string {
	switch {
	case !f.Consulted:
		return NAWired
	case f.GraphEdges == 0:
		return NAEmptyGraph
	case f.Symptom == "":
		return NAUnplaced
	case f.Relation == RelationUnknown:
		return NAUnrelated
	default:
		return NAApplicable
	}
}

// ScoreEstateGrounded grades the topological consistency of the session's stated cause 1..5 and returns the
// one-line reason written onto the row. ok=false means N/A — the caller must write NO row.
//
// THE SCALE:
//
//	5  the named cause is the alerting entity itself, or one dependency edge away from it (either
//	   direction — a failing hypervisor and a runaway guest are both real causes).
//	4  the named cause reaches the symptom along a multi-hop dependency path.
//	3  no path, but they are common-cause SIBLINGS under a shared infrastructure parent. Topologically
//	   plausible and not corroborated: the shared parent, not the sibling, is usually the real cause.
//	2  the graph knows both entities through live observed edges and joins them by NOTHING. The claim is
//	   topologically impossible as stated — the diagnosis names a machine that cannot reach the symptom.
//
// ★ 1 IS DELIBERATELY UNUSED. The floor of the diagnosis_grounded scale (1) is reserved for a session that
// held the refutation in its own captured evidence and asserted anyway — a fact about the RECORD. This axis
// speaks from a MODEL of the estate, and a model is never entitled to the same certainty: an edge may simply
// not have been discovered yet. Scoring 2 says "your own estate graph does not support this", which is the
// strongest honest thing a snapshot can say.
func ScoreEstateGrounded(s Session) (int, string, bool) {
	if !EstateApplicable(s) {
		return 0, "", false
	}
	e := s.Estate
	switch e.Relation {
	case RelationSelf:
		return 5, fmt.Sprintf("the named cause %q IS the alerting entity", e.Cause), true
	case RelationAdjacent:
		return 5, fmt.Sprintf("the estate graph joins %q to the alerting %q directly (%s %s, %s, confidence %.2f)",
			e.Cause, e.Symptom, e.Direction, e.Rel, e.Source, e.Confidence), true
	case RelationConnected:
		return 4, fmt.Sprintf("the estate graph reaches the alerting %q from %q %s over %d hops (path confidence %.2f)",
			e.Symptom, e.Cause, e.Direction, e.Distance, e.Confidence), true
	case RelationSibling:
		return 3, fmt.Sprintf("%q does not reach %q, but they are common-cause siblings under a shared infrastructure parent (confidence %.2f)",
			e.Cause, e.Symptom, e.Confidence), true
	default:
		return 2, fmt.Sprintf("the estate graph knows both %q and the alerting %q and joins them by NOTHING — no dependency path either way and no shared parent, so the stated cause cannot produce this symptom",
			e.Cause, e.Symptom), true
	}
}

// EstateRule returns the rubric's written calibration for this dimension — the one source an operator reads
// to learn how their agent is graded, for every scored axis and not just the five a model grades.
func EstateRule() string { return rubric.EstateRule }

// GroundInEstate computes the estate block for one session against a graph snapshot. It is READ-ONLY over the
// graph, deterministic, and total: a nil graph, an empty graph, an unknown host and a claim that names no
// machine all return the honest "the graph does not know" block rather than an error or a guess.
//
// The claim it reads is the TYPED diagnosis (TG-201) — root_cause and mechanism — and deliberately NOT the
// free-text Conclusion: a conclusion is prose about what the agent DID, and a grounded stand-down that names
// no cause must not be dragged onto a causal axis it never made a claim on.
func GroundInEstate(g *estate.Graph, s Session) EstateFacts {
	f := EstateFacts{Relation: RelationUnknown}
	if g == nil {
		return f // no estate wired — the axis stays N/A, exactly as it was before this shipped
	}
	f.Consulted = true
	f.GraphEdges = g.Len()
	if f.GraphEdges == 0 {
		return f // an empty snapshot is an outage or a cold boot, not a world without dependencies
	}
	sym, symTier, ok := g.ResolveTiered(s.Host)
	if ok {
		f.SymptomTier = string(symTier)
	}
	if !ok || symTier == estate.TierFuzzy {
		return f // the graph cannot place the thing that alerted; nothing downstream of this is knowable
	}
	f.Symptom = sym.Name
	f.SymptomKnown = g.HasGroundTruth(sym)

	tokens := claimTokens(s.Diagnosis.RootCause, s.Diagnosis.Mechanism)
	f.Claimed = len(tokens)
	best := 0
	thin := false // the claim named an entity the graph holds no live observed topology for
	for _, tok := range tokens {
		cause, tier, ok := g.ResolveTiered(tok)
		if !ok || tier == estate.TierFuzzy {
			// A name the graph cannot place, or one only a separator-fold guessed at, is not a claim about
			// this estate — it is not scored in either direction (see ResolveTiered).
			continue
		}
		f.Resolved++
		cand := relate(g, sym, cause)
		cand.Cause, cand.CauseTier = cause.Name, string(tier)
		cand.CauseKnown = g.HasGroundTruth(cause)
		if cand.Relation == RelationUnrelated && !(f.SymptomKnown && cand.CauseKnown) {
			// The endpoints are in the index but the graph holds no LIVE OBSERVED topology for one of them —
			// its edges expired, or all it has is a capped co-occurrence guess. Silence from thin knowledge is
			// not a claim of unrelatedness (TG-202's central constraint).
			cand.Relation = RelationUnknown
			thin = true
		}
		// Record the best-connected candidate — and, when nothing connects at all, the FIRST one the graph
		// could place. An N/A block that named no entity would tell an operator only "not scored"; naming the
		// machine the graph is blind to is what distinguishes a stale estate from an agent error.
		if r := relationRank(cand.Relation); r > best || f.Cause == "" {
			best = r
			f.Relation, f.Direction, f.Rel, f.Source = cand.Relation, cand.Direction, cand.Rel, cand.Source
			f.Confidence, f.Distance = cand.Confidence, cand.Distance
			f.Cause, f.CauseTier, f.CauseKnown = cand.Cause, cand.CauseTier, cand.CauseKnown
		}
	}
	// A MIXED CLAIM IS NOT A REFUTED ONE. If the claim named some entity the graph is blind to AND the best it
	// could otherwise say is "unrelated", the honest verdict is silence: the machine it cannot speak about may
	// be the real cause, and the one it can speak about may be a bystander the diagnosis merely mentioned.
	// Penalising here would let the estate's own gaps decide a session, which is the failure this whole
	// dimension is built to avoid — and it is over-suppression, not over-scoring, that Outcome.EstateScored
	// makes visible if this ever proves too cautious.
	if f.Relation == RelationUnrelated && thin {
		f.Relation, f.Direction, f.Rel, f.Source, f.Confidence, f.Distance = RelationUnknown, "", "", "", 0, 0
	}
	return f
}

// relationRank orders the verdicts so that a claim naming SEVERAL entities is judged by its best-connected
// one. A diagnosis that names the right hypervisor and also mentions an unrelated jump host has not made a
// topologically impossible claim, and reading the worst token would manufacture one.
func relationRank(r EstateRelation) int {
	switch r {
	case RelationSelf:
		return 5
	case RelationAdjacent:
		return 4
	case RelationConnected:
		return 3
	case RelationSibling:
		return 2
	case RelationUnrelated:
		return 1
	default:
		return 0
	}
}

// relate is the traversal: how (if at all) does the graph join the named cause to the alerting entity. Checked
// nearest-first so the strongest true statement wins, and in BOTH directions because a dependent can cause its
// dependency's symptom (the container that fills its node's disk) just as a dependency can cascade into it.
func relate(g *estate.Graph, symptom, cause estate.Entity) EstateFacts {
	if sameNode(symptom, cause) {
		return EstateFacts{Relation: RelationSelf, Confidence: 1}
	}
	for _, p := range g.Parents(symptom) { // symptom depends-on cause: the classic cascade
		if sameNode(p.Entity, cause) {
			return EstateFacts{Relation: RelationAdjacent, Direction: DirectionUpstream, Rel: string(p.Rel),
				Source: string(p.Source), Confidence: p.Confidence, Distance: 1}
		}
	}
	for _, p := range g.Parents(cause) { // cause depends-on symptom: the dependent that harms its host
		if sameNode(p.Entity, symptom) {
			return EstateFacts{Relation: RelationAdjacent, Direction: DirectionDownstream, Rel: string(p.Rel),
				Source: string(p.Source), Confidence: p.Confidence, Distance: 1}
		}
	}
	// Multi-hop: is the alerting entity in the blast radius of the named cause (or the reverse)? The
	// path-product confidence decays multiplicatively, so a long chain of weak edges reports as the weak
	// claim it is.
	for _, imp := range g.BlastRadius(cause, estateWalkDepth) {
		if sameNode(imp.Entity, symptom) {
			return EstateFacts{Relation: RelationConnected, Direction: DirectionUpstream,
				Confidence: imp.Confidence, Distance: imp.Distance}
		}
	}
	for _, imp := range g.BlastRadius(symptom, estateWalkDepth) {
		if sameNode(imp.Entity, cause) {
			return EstateFacts{Relation: RelationConnected, Direction: DirectionDownstream,
				Confidence: imp.Confidence, Distance: imp.Distance}
		}
	}
	for _, sib := range g.Siblings(symptom) {
		if sameNode(sib.Entity, cause) {
			return EstateFacts{Relation: RelationSibling, Confidence: sib.Confidence, Distance: sib.Distance}
		}
	}
	return EstateFacts{Relation: RelationUnrelated}
}

// sameNode compares two resolved entities by the graph's own name identity — domain-stripped and
// case-insensitive. Comparing the Entity structs would read a machine seen by two sources under two typed
// names (a NetBox physical_host and a LibreNMS host twin) as two different machines, which is the
// "disconnected twin" bug the resolver exists to prevent, re-introduced one layer up.
func sameNode(a, b estate.Entity) bool {
	return strings.EqualFold(bareName(a.Name), bareName(b.Name))
}

func bareName(name string) string {
	return strings.SplitN(strings.TrimSpace(name), ".", 2)[0]
}

// claimTokens extracts the entity-SHAPED tokens from the stated claim, in order, de-duplicated. It is
// deliberately dumb: it splits on everything that cannot appear in a hostname and hands each survivor to the
// graph's own resolver. The resolver — not a heuristic here — decides what is an estate entity, so a token
// like "journald" or "memory" simply fails to resolve and costs nothing, while `dc1pve01`,
// `dc1pve01.mgmt.lan` and `web-01` all survive intact to be looked up.
func claimTokens(fields ...string) []string {
	var out []string
	seen := map[string]bool{}
	var cur strings.Builder
	flush := func() {
		t := strings.Trim(cur.String(), "-_.")
		cur.Reset()
		if len(t) < minClaimToken || seen[strings.ToLower(t)] {
			return
		}
		seen[strings.ToLower(t)] = true
		out = append(out, t)
	}
	for _, f := range fields {
		for _, r := range f {
			if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' || r == '.' {
				cur.WriteRune(r)
				continue
			}
			flush()
		}
		flush()
	}
	return out
}
