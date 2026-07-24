package policy

import (
	"bytes"
	"context"
	"strings"
	"testing"
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
			if st, ok := gr.state[op]; !ok || st.Level != LevelAuto {
				t.Fatalf("curated class %q must be graduated to auto, got %+v (present=%v)", op, st, ok)
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
			if st, ok := gr.state[oc]; !ok || st.Level != LevelAuto {
				t.Fatalf("curated class %q must be at auto after seed, got %+v (present=%v)", oc, st, ok)
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
	// reverse: every `auto` rule's class is graduated (else the auto rule downgrades to approve)
	for _, r := range rs.Rules {
		if r.Verdict == VerdictAuto && !graduated[strings.ToLower(r.Match.OpClass)] {
			t.Errorf("curated `auto` rule %q (op_class %q) has no matching graduation seed — it would be inert", r.ID, r.Match.OpClass)
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
