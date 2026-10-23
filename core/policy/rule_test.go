package policy

import (
	"errors"
	"testing"
)

func f64(v float64) *float64 { return &v }
func intp(v int) *int        { return &v }

// TestVerdictTrinaryParse proves the verdict is a CLOSED enum: the three valid verdicts parse; an unknown
// verdict is a load-time error (REQ-1506), never coerced.
func TestVerdictTrinaryParse(t *testing.T) {
	for _, v := range []string{"auto", "approve", "deny"} {
		doc := []byte(`{"rules":[{"id":"r","match":{"host":"h1"},"verdict":"` + v + `"}]}`)
		rs, err := ParseRuleSet(doc)
		if err != nil {
			t.Fatalf("verdict %q should parse: %v", v, err)
		}
		if rs.Rules[0].Verdict != Verdict(v) {
			t.Fatalf("verdict %q parsed to %q", v, rs.Rules[0].Verdict)
		}
	}
	bad := []byte(`{"rules":[{"id":"r","match":{"host":"h1"},"verdict":"maybe"}]}`)
	if _, err := ParseRuleSet(bad); !errors.Is(err, ErrMalformedRule) {
		t.Fatalf("unknown verdict must be ErrMalformedRule, got %v", err)
	}
}

// TestParamInheritance proves an unset rule param inherits from the global-default rule, and a param the
// rule sets itself is NOT overwritten (REQ-1507). A min_confidence unset everywhere falls back to 0.60.
func TestParamInheritance(t *testing.T) {
	rs := RuleSet{
		Default: Params{MinConfidence: f64(0.8), BandMode: BandRespect, RateLimit: intp(30)},
		Rules: []Rule{
			{ID: "inherits", Verdict: VerdictAuto},                                            // all params unset → inherit all
			{ID: "overrides", Verdict: VerdictAuto, Params: Params{MinConfidence: f64(0.95)}}, // own min_confidence
		},
	}

	eff := rs.EffectiveParams(rs.Rules[0])
	if eff.MinConfidence == nil || *eff.MinConfidence != 0.8 {
		t.Fatalf("unset min_confidence did not inherit 0.8: %v", eff.MinConfidence)
	}
	if eff.BandMode != BandRespect {
		t.Fatalf("unset band_mode did not inherit respect: %q", eff.BandMode)
	}
	if eff.RateLimit == nil || *eff.RateLimit != 30 {
		t.Fatalf("unset rate_limit did not inherit 30: %v", eff.RateLimit)
	}

	own := rs.EffectiveParams(rs.Rules[1])
	if own.MinConfidence == nil || *own.MinConfidence != 0.95 {
		t.Fatalf("rule's own min_confidence was overwritten: %v", own.MinConfidence)
	}

	// with NO global default, min_confidence falls back to the hard DefaultMinConfidence.
	bare := RuleSet{Rules: []Rule{{ID: "b", Verdict: VerdictAuto}}}
	if eff := bare.EffectiveParams(bare.Rules[0]); eff.MinConfidence == nil || *eff.MinConfidence != DefaultMinConfidence {
		t.Fatalf("bare min_confidence fallback = %v, want %v", eff.MinConfidence, DefaultMinConfidence)
	}
}

// TestMalformedFailClosed proves every malformation is refused with ErrMalformedRule and no partial RuleSet
// (INV-09): unknown key, empty id, unknown band_mode, out-of-range confidence, negative rate_limit, two
// estate selectors, a non-default rule with no match dimension, and a duplicate id.
func TestMalformedFailClosed(t *testing.T) {
	cases := map[string]string{
		"unknown key":          `{"rules":[{"id":"r","match":{"host":"h"},"verdict":"auto","bogus":1}]}`,
		"empty id":             `{"rules":[{"id":"","match":{"host":"h"},"verdict":"auto"}]}`,
		"unknown band_mode":    `{"rules":[{"id":"r","match":{"host":"h"},"verdict":"auto","params":{"band_mode":"paranoid"}}]}`,
		"confidence too high":  `{"rules":[{"id":"r","match":{"host":"h"},"verdict":"auto","params":{"min_confidence":1.5}}]}`,
		"negative rate_limit":  `{"rules":[{"id":"r","match":{"host":"h"},"verdict":"auto","params":{"rate_limit":-1}}]}`,
		"two estate selectors": `{"rules":[{"id":"r","match":{"host":"h","group":"g"},"verdict":"auto"}]}`,
		"no match dimension":   `{"rules":[{"id":"r","match":{},"verdict":"auto"}]}`,
		"duplicate id":         `{"rules":[{"id":"r","match":{"host":"h"},"verdict":"auto"},{"id":"r","match":{"host":"h2"},"verdict":"deny"}]}`,
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			rs, err := ParseRuleSet([]byte(doc))
			if !errors.Is(err, ErrMalformedRule) {
				t.Fatalf("expected ErrMalformedRule, got %v", err)
			}
			if len(rs.Rules) != 0 {
				t.Fatalf("malformed input returned a partial rule set: %+v", rs)
			}
		})
	}
}

// TestDefaultRuleExemptFromMatch proves the global-default rule may carry no match dimension (it contributes
// params, not a verdict match) while a non-default rule may not.
func TestDefaultRuleExemptFromMatch(t *testing.T) {
	doc := []byte(`{"rules":[{"id":"global","match":{},"verdict":"approve","is_default":true,"params":{"min_confidence":0.7}}]}`)
	rs, err := ParseRuleSet(doc)
	if err != nil {
		t.Fatalf("default rule with empty match should parse: %v", err)
	}
	if !rs.Rules[0].IsDefault {
		t.Fatalf("is_default not carried")
	}
}
