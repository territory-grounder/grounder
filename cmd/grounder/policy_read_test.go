package main

import (
	"testing"

	"github.com/territory-grounder/grounder/core/credential"
	"github.com/territory-grounder/grounder/core/policy"
)

func f64p(f float64) *float64 { return &f }
func intp(i int) *int         { return &i }
func bp(b bool) *bool         { return &b }

// TG-104 slice-2: projectPolicyRule is the READ projection the console rule editor round-trips through, so
// its wire shape is load-bearing. Two facts the editor's read→write mapping depends on and MUST NOT drift:
//   1. the estate selector renders as a single "kind:pattern" string whose kind is the credential.SelectorKind
//      spelling — HYPHENATED for host-glob / device-class (the editor maps these back to the write schema's
//      UNDERSCORE match keys host_glob / device_class);
//   2. every write-relevant per-rule field (verdict, op_class, argv_pattern, territory, reversible,
//      inverse_only, min_confidence, band_mode, rate_limit, approve_by, is_default) is present on the read item, so a
//      faithful round-trip drops nothing (a dropped match dimension or default could turn a deny into an
//      implicit allow — the safety property this whole slice exists to protect).
func TestProjectPolicyRule_HyphenatedSelectorAndEveryField(t *testing.T) {
	// A host-glob rule with every policy-specific dimension + every param set.
	full := policy.Rule{
		ID: "deny-rm",
		Match: policy.Match{
			Selector:    &credential.Selector{Kind: credential.KindHostGlob, Pattern: "nl*"},
			OpClass:     "exec-shell",
			ArgvPattern: "rm -rf",
			Territory:   "prod",
			Reversible:  bp(false),
			InverseOnly: bp(true),
		},
		Verdict:   policy.VerdictDeny,
		Params:    policy.Params{MinConfidence: f64p(0.9), BandMode: policy.BandForce, RateLimit: intp(3)},
		ApproveBy: []string{"group:sre", "user:root"},
	}
	got := projectPolicyRule(full)
	if got.ID != "deny-rm" || got.Verdict != "deny" {
		t.Fatalf("id/verdict projected wrong: %+v", got)
	}
	// THE critical fact: host-glob renders hyphenated, not underscored — the editor's kind map keys off this.
	if got.Match.Selector != "host-glob:nl*" {
		t.Fatalf("host-glob selector must render HYPHENATED as %q, got %q", "host-glob:nl*", got.Match.Selector)
	}
	if got.Match.OpClass != "exec-shell" || got.Match.ArgvPattern != "rm -rf" || got.Match.Territory != "prod" {
		t.Fatalf("match dimensions dropped: %+v", got.Match)
	}
	if got.Match.Reversible == nil || *got.Match.Reversible != false {
		t.Fatalf("reversible must round-trip false (not nil): %+v", got.Match.Reversible)
	}
	if got.Match.InverseOnly == nil || *got.Match.InverseOnly != true {
		t.Fatalf("inverse_only dropped by the read projection (the editor would silently strip it on save): %+v", got.Match.InverseOnly)
	}
	if got.MinConfidence == nil || *got.MinConfidence != 0.9 {
		t.Fatalf("min_confidence dropped: %+v", got.MinConfidence)
	}
	if got.BandMode != "force" {
		t.Fatalf("band_mode dropped: %q", got.BandMode)
	}
	if got.RateLimit == nil || *got.RateLimit != 3 {
		t.Fatalf("rate_limit dropped: %+v", got.RateLimit)
	}
	if len(got.ApproveBy) != 2 || got.ApproveBy[0] != "group:sre" {
		t.Fatalf("approve_by dropped: %+v", got.ApproveBy)
	}

	// device-class is the other hyphenated kind; a default rule carries is_default and no match.
	dc := projectPolicyRule(policy.Rule{
		ID:      "class-net",
		Match:   policy.Match{Selector: &credential.Selector{Kind: credential.KindDeviceClass, Pattern: "cisco-asa"}},
		Verdict: policy.VerdictApprove,
	})
	if dc.Match.Selector != "device-class:cisco-asa" {
		t.Fatalf("device-class must render HYPHENATED, got %q", dc.Match.Selector)
	}
	def := projectPolicyRule(policy.Rule{ID: "global", Verdict: policy.VerdictApprove, IsDefault: true})
	if !def.IsDefault || def.Match.Selector != "" {
		t.Fatalf("is_default rule must project is_default=true and no selector: %+v", def)
	}
	// A rule with min_confidence explicitly ZERO must keep it (0 is a set value, not "unset") — the shipped
	// default_ruleset.json relies on this; a naive falsy check here would silently drop the floor.
	z := projectPolicyRule(policy.Rule{ID: "z", Verdict: policy.VerdictAuto, Params: policy.Params{MinConfidence: f64p(0)}})
	if z.MinConfidence == nil || *z.MinConfidence != 0 {
		t.Fatalf("min_confidence:0 must round-trip as a set value, got %+v", z.MinConfidence)
	}
}

// TG-104 slice-2: rulesetHasTopLevelDefault backs PolicyRulesPage.HasDefault — the editor's fail-closed guard.
// It must read true iff the ruleset's top-level default sets ANY tunable, and false for the all-unset default
// the parser yields when a document has no `default` block (the shipped ruleset's shape).
func TestRulesetHasTopLevelDefault(t *testing.T) {
	if rulesetHasTopLevelDefault(policy.RuleSet{}) {
		t.Fatal("an absent/all-unset default must read false (shipped ruleset shape)")
	}
	cases := []policy.Params{
		{MinConfidence: f64p(0.6)},
		{BandMode: policy.BandRespect},
		{RateLimit: intp(10)},
	}
	for i, d := range cases {
		if !rulesetHasTopLevelDefault(policy.RuleSet{Default: d}) {
			t.Fatalf("case %d: a default setting a tunable must read true: %+v", i, d)
		}
	}
	// Parsing the shipped default_ruleset.json must yield NO top-level default (live this guard is inert).
	rs, err := policy.ParseRuleSet([]byte(`{"rules":[{"id":"r","verdict":"auto","match":{"op_class":"restart-service"}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if rulesetHasTopLevelDefault(rs) {
		t.Fatal("a ruleset with no default block must read has_default=false")
	}
}
