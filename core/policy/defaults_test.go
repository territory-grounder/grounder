package policy

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/actuate/opschema"
)

// fakeRuleset is an in-memory RulesetSeeder: absent until a document is Saved, then present.
type fakeRuleset struct {
	doc   []byte
	saves int
}

func (f *fakeRuleset) Load(context.Context) (RuleSet, []byte, error) {
	if f.doc == nil {
		return RuleSet{}, nil, ErrRulesetAbsent
	}
	rs, err := ParseRuleSet(f.doc)
	return rs, f.doc, err
}
func (f *fakeRuleset) Save(_ context.Context, doc []byte, _ string) (RuleSet, error) {
	rs, err := ParseRuleSet(doc)
	if err != nil {
		return RuleSet{}, err
	}
	f.doc = doc
	f.saves++
	return rs, nil
}

// fakeGrad is an in-memory GraduationSeeder: ErrClassAbsent until a class is Saved.
type fakeGrad struct {
	state map[string]ClassState
	saves int
}

func (f *fakeGrad) Load(_ context.Context, op string) (ClassState, error) {
	if st, ok := f.state[op]; ok {
		return st, nil
	}
	return ClassState{}, ErrClassAbsent
}
func (f *fakeGrad) Save(_ context.Context, st ClassState) error {
	if f.state == nil {
		f.state = map[string]ClassState{}
	}
	f.state[st.OpClass] = st
	f.saves++
	return nil
}

func quietLog(string, ...any) {}

// A fresh deploy seeds the curated ruleset AND graduates the curated classes to auto; a second run is a no-op
// (idempotent); an existing operator ruleset is never clobbered and its ladder is left untouched.
func TestSeedDefaults(t *testing.T) {
	ctx := context.Background()

	t.Run("fresh deploy seeds ruleset + graduation", func(t *testing.T) {
		rs, gr := &fakeRuleset{}, &fakeGrad{}
		got := SeedDefaults(ctx, rs, gr, quietLog)
		if rs.saves != 1 {
			t.Fatalf("ruleset must be seeded once, got %d saves", rs.saves)
		}
		if len(got.Rules) == 0 {
			t.Fatal("returned ruleset must carry the curated rules")
		}
		for _, op := range DefaultGraduatedClasses() {
			st, ok := gr.state[op]
			// TG-255: a SEEDED class lands on auto_notice — it may act without a vote, but never
			// unobserved. LevelAuto is silence, and silence is earned with two verified-clean streaks.
			if !ok || st.Level != LevelAutoNotice {
				t.Fatalf("curated class %q must be seeded to auto_notice (may act, never unobserved), got %+v (present=%v)", op, st, ok)
			}
			// TG-183: a SEEDED class must NOT masquerade as a VERIFIED run. Its provenance is `seeded`, never
			// the fabricated `verified_clean` (which asserts a verification that never happened). This is the
			// honesty guard — on the pre-fix code (LastOutcome: OutcomeVerifiedClean) this fails.
			if st.LastOutcome == OutcomeVerifiedClean {
				t.Fatalf("seeded class %q must NOT be stamped verified_clean (fabricated verification), got %v", op, st.LastOutcome)
			}
			if st.LastOutcome != OutcomeSeeded {
				t.Fatalf("seeded class %q must carry provenance=seeded, got %v", op, st.LastOutcome)
			}
		}

		// idempotent: a second boot re-seeds nothing
		got2 := SeedDefaults(ctx, rs, gr, quietLog)
		if rs.saves != 1 || gr.saves != len(DefaultGraduatedClasses()) {
			t.Fatalf("second boot must not re-seed: ruleset saves=%d grad saves=%d", rs.saves, gr.saves)
		}
		if len(got2.Rules) != len(got.Rules) {
			t.Fatal("second boot must return the same effective ruleset")
		}
	})

	t.Run("operator ruleset is never clobbered and its ladder is untouched", func(t *testing.T) {
		rs := &fakeRuleset{doc: []byte(`{"rules":[{"id":"op","verdict":"deny","match":{"argv_pattern":"rm -rf"}}]}`)}
		gr := &fakeGrad{} // operator left graduation empty on purpose
		got := SeedDefaults(ctx, rs, gr, quietLog)
		if rs.saves != 0 {
			t.Fatalf("an existing operator ruleset must NOT be overwritten, got %d saves", rs.saves)
		}
		if gr.saves != 0 {
			t.Fatalf("graduation must NOT be seeded under an operator ruleset (respect their trust setup), got %d saves", gr.saves)
		}
		if len(got.Rules) != 1 || got.Rules[0].ID != "op" {
			t.Fatalf("must return the operator's own ruleset, got %+v", got.Rules)
		}
	})

	t.Run("an earned class is never downgraded while absent siblings seed", func(t *testing.T) {
		// restart-service has earned autonomy (CleanRunCount 3); the other curated classes are absent.
		// Absent-only seeding must leave the earned class byte-for-byte untouched and seed ONLY the absent ones.
		rs := &fakeRuleset{}
		earned := ClassState{OpClass: "restart-service", Level: LevelAuto, CleanRunCount: 3}
		gr := &fakeGrad{state: map[string]ClassState{"restart-service": earned}}
		SeedDefaults(ctx, rs, gr, quietLog)
		if got := gr.state["restart-service"]; got != earned {
			t.Fatalf("earned class must not be re-seeded/downgraded: got %+v, want %+v", got, earned)
		}
		absent := len(DefaultGraduatedClasses()) - 1 // every curated class except the pre-earned one
		if gr.saves != absent {
			t.Fatalf("absent-only seed must save exactly the %d absent curated classes, got %d", absent, gr.saves)
		}
		for _, oc := range DefaultGraduatedClasses() {
			st, ok := gr.state[oc]
			if !ok {
				t.Fatalf("curated class %q is absent after seed", oc)
			}
			// The pre-EARNED class keeps the silent rung it won; the absent siblings are SEEDED and land on
			// the paging rung (TG-255). Asserting one level for both would erase exactly the distinction
			// this test exists to protect — earned silence versus curated placement.
			want := LevelAutoNotice
			if oc == "restart-service" {
				want = LevelAuto
			}
			if st.Level != want {
				t.Fatalf("curated class %q must be at %s after seed, got %+v", oc, want, st)
			}
		}
	})
}

// The curated default must parse, and its `auto` rules must be in exact lockstep with the graduated-class
// seed: every graduated class is named by a reversible `auto` rule, and every `auto` rule's class is
// graduated — otherwise the fresh-deploy seed leaves an inert `auto` rule (downgraded to approve) or grants
// autonomy to a class the ruleset never names.
func TestDefaultRuleSetDocumentParsesAndIsInLockstep(t *testing.T) {
	rs, err := ParseRuleSet(DefaultRuleSetDocument())
	if err != nil {
		t.Fatalf("curated default ruleset must parse: %v", err)
	}
	if len(rs.Rules) == 0 {
		t.Fatal("curated default must define at least one rule")
	}
	graduated := map[string]bool{}
	for _, g := range DefaultGraduatedClasses() {
		graduated[strings.ToLower(g)] = true
	}
	// forward: every graduated class has a reversible `auto` rule
	for g := range graduated {
		found := false
		for _, r := range rs.Rules {
			if r.Verdict == VerdictAuto && strings.EqualFold(r.Match.OpClass, g) {
				found = true
				if r.Match.Reversible == nil || !*r.Match.Reversible {
					t.Errorf("curated auto rule for %q must be reversible-gated (a curated auto class must be reversible)", g)
				}
			}
		}
		if !found {
			t.Errorf("DefaultGraduatedClasses names %q but no curated `auto` rule grants it — the seed would be inert", g)
		}
	}
	// REVERSE DIRECTION — CORRECTED (roadmap P4-1). This previously required every `auto` rule to have a
	// matching graduation SEED, on the grounds that an unseeded rule "would be inert". That conflates two
	// different things and blocks the earn path the ladder exists to provide.
	//
	// An `auto` rule for an UNGRADUATED class is not inert — it is the PRECONDITION for earning. Verified
	// against graduation.go: graduatedVerdict(LevelApprove, VerdictAuto) returns VerdictApprove, so the rule
	// grants nothing until the class reaches LevelAuto by accumulating verified-clean runs. WITHOUT the rule,
	// a fully-graduated class still resolves to `approve` forever — a silent dead end, which is exactly what
	// held start-service at clean_run_count=1 and disk-grow at 0 mutations across 19 proposals.
	//
	// So the requirement is the ONE-WAY implication that actually matters for safety: a SEEDED class must
	// have a rule (else the seed is inert, checked above). A rule WITHOUT a seed is the earn path, and
	// deliberately does NOT seed autonomy — the opposite mistake, which P0-1 had to undo by demoting two
	// classes that were seeded to auto with zero verified runs.
	for _, r := range rs.Rules {
		if r.Verdict == VerdictAuto && !graduated[strings.ToLower(r.Match.OpClass)] {
			t.Logf("curated `auto` rule %q (op_class %q) has no graduation seed — EARN PATH: inert until the "+
				"class accumulates verified-clean runs, which is the intended ladder behaviour", r.ID, r.Match.OpClass)
		}
		// The confidence gate MUST be explicitly off (min_confidence:0), NOT merely omitted — an omitted value
		// inherits the 0.60 EffectiveParams fallback and would clamp the curated auto to approve (inert).
		if r.Verdict == VerdictAuto {
			if r.Params.MinConfidence == nil {
				t.Errorf("curated `auto` rule %q must set min_confidence explicitly (0) — omitting it inherits the 0.60 floor and goes inert", r.ID)
			} else if *r.Params.MinConfidence != 0 {
				t.Errorf("curated `auto` rule %q sets min_confidence=%v — the confidence gate must be off (0) until calibrated", r.ID, *r.Params.MinConfidence)
			}
		}
	}
}

// The curated `auto` rule's RESOLVED min_confidence must be 0 (gate off) — proving the seed is not inert (the
// exact failure the review flagged: an unset value inherits the 0.60 EffectiveParams fallback, which clamps
// the curated auto to approve whenever the bound confidence is unset/low).
func TestCuratedAutoConfidenceGateOff(t *testing.T) {
	rs, err := ParseRuleSet(DefaultRuleSetDocument())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, r := range rs.Rules {
		if r.Verdict != VerdictAuto {
			continue
		}
		eff := rs.EffectiveParams(r)
		if eff.MinConfidence == nil || *eff.MinConfidence != 0 {
			t.Fatalf("curated auto rule %q resolves min_confidence=%v — must be 0 (gate off), else auto clamps to approve at low confidence (inert)", r.ID, eff.MinConfidence)
		}
	}
}

// DefaultRuleSetDocument must hand back a fresh copy so a caller can never corrupt the embedded default.
func TestDefaultRuleSetDocumentReturnsCopy(t *testing.T) {
	a := DefaultRuleSetDocument()
	if len(a) == 0 {
		t.Fatal("empty default document")
	}
	a[0] = 'X'
	if bytes.Equal(a, DefaultRuleSetDocument()) {
		t.Fatal("DefaultRuleSetDocument must return a fresh copy (a caller mutation leaked into the embed)")
	}
}

// EVERY DECLARED OP-CLASS MUST HAVE A POLICY RULE (roadmap P4-1).
//
// A class with no matching rule falls through to the engine's default `approve` REGARDLESS of its position
// on the graduation ladder — so it can never auto-execute, however many clean runs it earns. That is a
// silent dead end, not a refusal: nothing logs "this class has no rule", it simply never graduates.
//
// Measured live before this test existed: 6 op-classes were declared and only 4 had rules. start-service sat
// at clean_run_count=1 and disk-grow was proposed 19 times and mutated 0 — both because the ladder they were
// climbing led nowhere. A5 breadth cannot move while a class is in that state.
//
// disk-grow is the ONE legitimate exception and is named explicitly rather than skipped by a wildcard: its
// effect kind is awx-launch and the AWX lane is deliberately fail-closed (pendingActuator) until its
// credentials are wired, so granting it a policy rule would authorize a class whose effect leaf refuses
// anyway. When that lane lands, this list shrinks — and the test forces that to be a deliberate edit.
func TestEveryDeclaredOpClassHasAPolicyRule(t *testing.T) {
	// Classes intentionally without a rule, each with the reason it is exempt.
	exempt := map[string]string{
		"disk-grow": "effect kind awx-launch — the AWX lane is fail-closed until its credentials are wired",
		"k8s-set-replicas": "effect kind k8s-declarative — the gitops-mr lane is DARK (no actuator) until its owner-present slice-4 arm; the change it proposes is a merge request a human merges, so `approve` is the correct default and it needs no graduation rule to auto-execute yet (TG-122 slice 3)",
	}

	var ruleset struct {
		Rules []struct {
			ID    string `json:"id"`
			Match struct {
				OpClass string `json:"op_class"`
			} `json:"match"`
		} `json:"rules"`
	}
	if err := json.Unmarshal(DefaultRuleSetDocument(), &ruleset); err != nil {
		t.Fatalf("parse default ruleset: %v", err)
	}
	ruled := map[string]bool{}
	for _, r := range ruleset.Rules {
		ruled[r.Match.OpClass] = true
	}

	for _, spec := range opschema.Specs() {
		oc := spec.OpClass
		if ruled[oc] {
			continue
		}
		if why, ok := exempt[oc]; ok {
			t.Logf("op_class %q has no policy rule — exempt: %s", oc, why)
			continue
		}
		t.Errorf("op_class %q is DECLARED but has NO policy rule: it falls through to the default `approve` "+
			"regardless of its graduation level, so it can never auto-execute however many clean runs it "+
			"earns — a silent dead end. Add a rule to default_ruleset.json, or add it to the exempt list "+
			"here with the reason.", oc)
	}
}

// The safety property the corrected invariant above RELIES ON, asserted directly rather than assumed: an
// `auto` rule for a class that has not earned graduation must resolve to `approve`. If this ever regressed,
// adding a rule would grant UNEARNED autonomy — precisely the defect P0-1 had to undo by demoting two
// classes that sat at `auto` with zero verified runs.
func TestAutoRuleIsInertUntilTheClassEarnsGraduation(t *testing.T) {
	if got := graduatedVerdict(LevelApprove, VerdictAuto); got != VerdictApprove {
		t.Fatalf("auto rule + UNGRADUATED class = %q, want %q — a rule would grant unearned autonomy rather "+
			"than merely enabling the earn path", got, VerdictApprove)
	}
	if got := graduatedVerdict(LevelAuto, VerdictAuto); got != VerdictAuto {
		t.Fatalf("auto rule + GRADUATED class = %q, want %q — else the earn path leads nowhere and a class "+
			"can never auto-execute however many clean runs it accumulates", got, VerdictAuto)
	}
	// A deny is never lifted by graduation, at any level.
	for _, lvl := range []Level{LevelApprove, LevelAuto} {
		if got := graduatedVerdict(lvl, VerdictDeny); got != VerdictDeny {
			t.Fatalf("graduation lifted a DENY at level %v (got %q) — no amount of earned trust may override a deny", lvl, got)
		}
	}
}

// A SEEDED CLASS MUST NEVER LAND ABOVE THE PAGING RUNG (TG-255).
//
// The two rungs are semantically distinct and the code says so: LevelAutoNotice is "may act, but never
// unobserved"; LevelAuto is "SILENT autonomy … honored with no page". Silence is what the EARNED path
// awards after two verified-clean streaks, so a hand-curated out-of-box allowlist must not start there.
//
// The original placement was not an oversight — the owner ruled "keep AUTO" on 2026-07-26 and the
// auto_notice rung did not exist until 2026-07-31. The decision simply predated the option by five days.
//
// This is a floor, not an equality: a class that EARNS its way to Auto must still be able to sit there.
// So the assertion is "no seeded class is above auto_notice", which stays true as the ladder grows.
func TestNoSeededClassLandsAboveThePagingRung(t *testing.T) {
	ctx := context.Background()
	rs := &fakeRuleset{}
	gr := &fakeGrad{state: map[string]ClassState{}}
	SeedDefaults(ctx, rs, gr, quietLog)

	if len(gr.state) == 0 {
		t.Fatal("vacuity floor: the seed placed no class at all, so this asserts nothing about where it places them")
	}
	for oc, st := range gr.state {
		if st.LastOutcome != OutcomeSeeded {
			continue // only curated placements are in scope; an earned class may legitimately be higher
		}
		if st.Level == LevelAuto {
			t.Errorf("seeded class %q landed on LevelAuto — the SILENT rung. NoticeFloor fires at exactly "+
				"RungAutoNotice, so this class acts with no vote AND raises no page, holding the property "+
				"the graduation ladder reserves for two verified-clean streaks. Every future seeded class "+
				"inherits the same skip.", oc)
		}
	}
}
