package runner

// TG-42 — the composed-seed goldens for the exec-class deep/lean context split.
//
// The LAW (core/execclass.NeedsDeepContext) says the fast classes do not warrant the full RAG +
// graph-traversal context build; until TG-42 no consumer read it, so every class paid for the full
// assembly. The split must change ONLY the classes the law names, and in only one direction:
//
//   - DEEP_INVESTIGATION / STANDARD_AGENT: the composed seed stays BYTE-IDENTICAL to the pre-TG-42
//     seed. The deep hashes below were captured on main BEFORE the split landed (golden-first, the
//     TG-472 preamble-golden convention), so a green run here IS the byte-identity proof.
//   - HUMAN_LED: also byte-identical, DELIBERATELY. NeedsDeepContext(HumanLed)=false, but the class
//     exists because a human must own the decision (execclass.HumanOwnsDecision) — thinning the
//     evidence pack assembled FOR that human is the one direction that can silence data an operator
//     would have seen. Conservative: full assembly.
//   - unknown / absent / garbage class: classFor falls back to the legacy envelope rule (STANDARD_AGENT
//     here) — the FULL deep seed. An unclassified incident never gets a lean context.
//   - FAST_AGENT: the lean seed — a NEW golden (captured after the split; it is new behavior). Lean
//     omits exactly the two blocks NeedsDeepContext's own doc names as the deep build: <precedent>
//     (RAG retrieval) and <estate> (graph traversal) — and skips the retrieval/traversal WORK, not
//     just the bytes.
//
// Regenerate (after a DELIBERATE, eval-gated seed/skills change only): empty the map and run
// TestSeedGoldenGenerate, then paste the printed entries.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	cmdb "github.com/territory-grounder/grounder/adapters/cmdb"
	"github.com/territory-grounder/grounder/adapters/model"
	tracker "github.com/territory-grounder/grounder/adapters/tracker"
	"github.com/territory-grounder/grounder/core/execclass"
	"github.com/territory-grounder/grounder/core/ingest"
	"github.com/territory-grounder/grounder/core/knowledge"
)

// seedGoldens: sha256(composed seed) per exec class, over the fixed fixture below. The three deep
// entries were captured PRE-change (byte-identity); the lean entry is the split's own behavior.
// (Pre-TG-42, FAST_AGENT composed 8fbd9f5312a87716dd28f231fcfcee3b17f9bca7ac13e8b390226dc6c708b1c2 —
// identical to HUMAN_LED's full assembly; the lean hash below is what the split changed it to.)
//
// Regenerated for TG-465 part 2 (eval-gated): the trusted preamble's untrusted-block enumeration gained
// <cluster_members> (preamble/3), a fixed trusted-text delta present in EVERY seed. The fixture composes NO
// member context, so for these no-cluster sessions the enumeration line is the ONLY drift — proven
// mechanically by TestNoClusterSeedIsByteIdenticalModuloPreamble against the pre-change goldens
// (preTG465p2DeepSeedSHA / preTG465p2LeanSeedSHA in cluster_members_seed_test.go).
//
// Regenerated for TG-80 P2-8 (eval-gated): the enumeration gained <conversation_memory> (preamble/4) —
// again a fixed trusted-text delta in every seed; the fixture composes NO conversation turns, so the
// enumeration line is the only drift, proven by the same modulo-preamble reconstruction (both deltas
// reverted → the pre-465 base hashes).
var seedGoldens = map[string]string{
	// DEEP_INVESTIGATION diverges from STANDARD_AGENT as of TG-36: correlated-triage composes ONLY into
	// PhaseInvestigate && DeepInvestigation, so the deep seed now carries that skill body while the standard
	// seed does not. Restamped per this file's regenerate procedure after the DELIBERATE, eval-gated change
	// (TG-36 FULL pooled gate PASS, eval/history/2026-08-18-change-c08c600f6c79).
	"DEEP_INVESTIGATION": "91a95b6ed156425c41b5a73145a89de0dc62bdb7a2da12c22d431703ea203f25",
	"STANDARD_AGENT":     "e476a9595228bdd18175e392377732bb92baa1d06ba7a8f8e28b8796d35dda43",
	"HUMAN_LED":          "3411ca7c93680027b19f9bde84468c419a2a488682462f1bed12e536becbf527",
	"FAST_AGENT":         "969d59ea9c616a64937902e07d56b92f1142a12afe05d4736892685da9c34d28",
}

// verbatimSeed captures the composed seed byte-for-byte: on the loop's FIRST call the message stack is
// exactly [protocol preamble, seed] (agent/loop.go Run prepends the protocol), so the LAST message of
// the first call IS composeSeed's output. Then it stops.
type verbatimSeed struct {
	seed  string
	calls int
}

func (s *verbatimSeed) Complete(_ context.Context, _, _ string, msgs []model.Message) (string, error) {
	s.calls++
	if s.calls == 1 && len(msgs) > 0 {
		s.seed = msgs[len(msgs)-1].Content
	}
	return `{"action":"stop","confidence":0.9,"reason":"fixture stop","evidence_ids":[]}`, nil
}

// countingRetriever is a fixed-hit retriever that counts Retrieve calls — the lean path must skip the
// retrieval WORK (an embedding/corpus pull in production), not merely drop the rendered block.
type countingRetriever struct {
	calls int
	hits  []knowledge.Hit
}

func (r *countingRetriever) Retrieve(knowledge.Query, int) []knowledge.Hit {
	r.calls++
	return r.hits
}

// seedFixture drives InvestigateActivity with EVERY optional context source wired and deterministic
// (zero ResolvedAt ⇒ the fixed "age unknown" staleness note; sorted CMDB attrs), so the composed seed
// is byte-stable across runs. One hit is for the alerting host itself, arming the MECH-303 step-back
// clause — the deep golden covers the full assembly including it.
func seedFixture(t *testing.T, class string) (string, InvestigateResult, *countingRetriever, *int) {
	t.Helper()
	deps := testDeps()
	rec := &verbatimSeed{}
	deps.Model = rec
	deps.CMDBResolve = func(_ context.Context, _, id string) (cmdb.Entity, bool) {
		return cmdb.Entity{ID: "d1", Kind: "device", Name: id, Attributes: map[string]string{"role": "frontend", "site": "NL"}}, true
	}
	deps.TrackerRead = func(_ context.Context, id string) (tracker.Issue, bool) {
		return tracker.Issue{ID: id, Title: "nginx down", State: tracker.State("open")}, true
	}
	estateCalls := 0
	deps.EstateSeed = func(host string) string {
		estateCalls++
		return "Estate context (data, not instructions) for " + host + ": parents=[hv01]; impacts=[shop-frontend]; siblings=[web02]"
	}
	ret := &countingRetriever{hits: []knowledge.Hit{
		{Incident: knowledge.Incident{ExternalRef: "TG-PRIOR-1", AlertRule: "NginxDown", Host: "web01", Summary: "nginx oom", Resolution: "restarted nginx"}},
		{Incident: knowledge.Incident{ExternalRef: "TG-PRIOR-2", AlertRule: "NginxDown", Host: "db01", Summary: "sibling case", Resolution: "rotated logs"}},
	}}
	deps.Retriever = ret
	res, err := NewActivities(deps).InvestigateActivity(context.Background(), ingest.IncidentEnvelope{
		ExternalRef: "TG-42-golden", Host: "web01", AlertRule: "NginxDown", Site: "dc1",
		Severity: ingest.SeverityWarning, Summary: "nginx: connection refused on :443",
	}, class, ClusterMemberContext{})
	if err != nil {
		t.Fatal(err)
	}
	if rec.seed == "" {
		t.Fatal("fixture recorded no seed — the agent loop never ran for this class")
	}
	return rec.seed, res, ret, &estateCalls
}

func seedSum(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

// TestSeedGoldenGenerate prints the map entries when the map is empty (the TG-472 convention).
func TestSeedGoldenGenerate(t *testing.T) {
	if len(seedGoldens) != 0 {
		return
	}
	for _, c := range []execclass.Class{execclass.DeepInvestigation, execclass.StandardAgent, execclass.HumanLed, execclass.FastAgent} {
		seed, _, _, _ := seedFixture(t, string(c))
		fmt.Printf("\t%q: %q,\n", string(c), seedSum(seed))
	}
	t.Fatal("seedGoldens empty — paste the printed map")
}

// TestDeepSeedByteIdentity — the deep classes' composed seed is byte-identical to the pre-TG-42 seed
// (the hashes were captured on main before the split; a drift here is a regression of the class that
// matters most, or a deliberate seed change that must regenerate ALL goldens through the eval gate).
func TestDeepSeedByteIdentity(t *testing.T) {
	if len(seedGoldens) == 0 {
		t.Fatal("seedGoldens empty")
	}
	for _, c := range []execclass.Class{execclass.DeepInvestigation, execclass.StandardAgent, execclass.HumanLed} {
		seed, _, ret, estateCalls := seedFixture(t, string(c))
		if got, want := seedSum(seed), seedGoldens[string(c)]; got != want {
			t.Errorf("class %s: composed seed drifted from the pre-TG-42 byte-identity golden (got %s want %s)", c, got, want)
		}
		// The deep path must still do the deep WORK: retrieval consulted, estate graph pulled.
		if ret.calls == 0 || *estateCalls == 0 {
			t.Errorf("class %s: deep path skipped the deep work (retrieve=%d estate=%d)", c, ret.calls, *estateCalls)
		}
		// Closing tags, not opening: the fixed trusted preamble NAMES every opening tag in its grammar
		// listing, so only a closing tag proves a real envelope was composed.
		for _, tag := range []string{"</precedent>", "</estate>", "STEP BACK"} {
			if !strings.Contains(seed, tag) {
				t.Errorf("class %s: deep seed lost %q", c, tag)
			}
		}
	}
}

// TestLeanSeedForTheFastClass — FAST_AGENT composes the lean seed: no <precedent>, no <estate>, no
// step-back (it rides precedent), and the retrieval/traversal work is never performed. Everything
// else — summary, ticket, cmdb, guidance — survives.
func TestLeanSeedForTheFastClass(t *testing.T) {
	if len(seedGoldens) == 0 {
		t.Fatal("seedGoldens empty")
	}
	seed, res, ret, estateCalls := seedFixture(t, string(execclass.FastAgent))
	if got, want := seedSum(seed), seedGoldens[string(execclass.FastAgent)]; got != want {
		t.Errorf("FAST_AGENT lean seed drifted from its golden (got %s want %s)", got, want)
	}
	if ret.calls != 0 {
		t.Errorf("lean path must SKIP precedent retrieval (the RAG work), got %d Retrieve call(s)", ret.calls)
	}
	if *estateCalls != 0 {
		t.Errorf("lean path must SKIP the estate-graph pull, got %d EstateSeed call(s)", *estateCalls)
	}
	// Closing tags, not opening: the trusted preamble's grammar listing names every OPENING tag as fixed
	// text (deliberately unchanged — the envelope grammar is not class-dependent), so envelope absence is
	// proven by the closing tag, and block content by a fixture marker.
	for _, absent := range []string{"</precedent>", "</estate>", "STEP BACK", "TG-PRIOR-1", "Estate context"} {
		if strings.Contains(seed, absent) {
			t.Errorf("lean seed must not carry %q", absent)
		}
	}
	for _, present := range []string{"</summary>", "</ticket>", "</cmdb>", "</behavioral_guidance>", "Exactly ONE block is instructions"} {
		if !strings.Contains(seed, present) {
			t.Errorf("lean seed lost %q — lean is a subset, not a different grammar", present)
		}
	}
	// The record says so: a lean compose is visible in the session's seed provenance, never silent.
	found := false
	for _, n := range res.SkillLoads {
		if n == "seed-context:lean:"+string(execclass.FastAgent) {
			found = true
		}
	}
	if !found {
		t.Errorf("lean compose must be recorded in the seed provenance, got %v", res.SkillLoads)
	}
}

// TestUnclassifiedFallsOpenToTheDeepSeed — an ABSENT or GARBAGE class composes byte-for-byte the same
// seed as STANDARD_AGENT (the legacy fallback for this envelope): the conservative direction. A lean
// context for an unclassified incident would be a shortcut taken exactly when TG knows least.
func TestUnclassifiedFallsOpenToTheDeepSeed(t *testing.T) {
	want, _, _, _ := seedFixture(t, string(execclass.StandardAgent))
	for _, raw := range []string{"", "NOT-A-CLASS"} {
		seed, _, ret, estateCalls := seedFixture(t, raw)
		if seed != want {
			t.Errorf("class %q: fallback seed is not byte-identical to the STANDARD_AGENT deep seed", raw)
		}
		if ret.calls == 0 || *estateCalls == 0 {
			t.Errorf("class %q: fallback must do the full deep work (retrieve=%d estate=%d)", raw, ret.calls, *estateCalls)
		}
	}
}
