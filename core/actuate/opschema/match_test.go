package opschema

import "testing"

// MatchTemplate is the INVERSE of renderTemplate, and the two are used as a round-trip integrity check by the
// effect leaf: classify an argv, re-derive the canonical argv from the class that came back, refuse on
// mismatch. That makes a WRONG match worse than no match — the round-trip does not catch it, it HIDES it,
// because re-deriving from the wrong class produces the argv that class builds, which is the argv we started
// with. These tests pin the match to exact structure for that reason.

func matchSpec(template []string) OpClassSpec {
	return OpClassSpec{OpClass: "t", Family: FamilyServiceLifecycle, SafetyTier: TierLowReversible,
		ArgvTemplate: template,
		Params:       []ParamSpec{{Name: "unit", Type: "string", Required: true}}}
}

func TestMatchTemplateIsTheExactInverseOfRender(t *testing.T) {
	t.Parallel()
	spec := matchSpec([]string{"systemctl", "restart", "${unit}"})
	// Whatever renders must match back to the same value — including values that look like flags or contain
	// metacharacters, because the effect leaf's allowlist is applied to whatever comes back out.
	for _, v := range []string{"nginx", "a b c", "--privileged", "$(reboot)", "unit.with.dots", "x@y.service"} {
		argv, err := spec.Argv(map[string]string{"unit": v})
		if err != nil {
			t.Fatalf("render(%q): %v", v, err)
		}
		got, param, hit := MatchTemplate(spec, argv)
		if !hit {
			t.Errorf("rendered %v does not match its own template — render and match have drifted", argv)
			continue
		}
		if got != v {
			t.Errorf("round-trip changed the value: rendered %q, matched back %q", v, got)
		}
		if param != "unit" {
			t.Errorf("matched slot named %q, want %q", param, "unit")
		}
	}
}

func TestMatchTemplateRejectsAnythingButTheExactShape(t *testing.T) {
	t.Parallel()
	spec := matchSpec([]string{"systemctl", "restart", "${unit}"})
	for name, argv := range map[string][]string{
		"shorter":           {"systemctl", "restart"},
		"longer":            {"systemctl", "restart", "nginx", "--now"},
		"empty slot":        {"systemctl", "restart", ""},
		"blank slot":        {"systemctl", "restart", "   "},
		"literal differs":   {"systemctl", "reload", "nginx"},
		"case differs":      {"Systemctl", "restart", "nginx"},
		"absolute path":     {"/usr/bin/systemctl", "restart", "nginx"},
		"nil argv":          nil,
		"empty argv":        {},
		"slot text as arg0": {"${unit}", "restart", "nginx"},
	} {
		if v, _, hit := MatchTemplate(spec, argv); hit {
			t.Errorf("%s: argv %v matched with value %q — the match must be exact, or the leaf resolves an "+
				"action to a class that did not build it", name, argv, v)
		}
	}
}

// A class with NO template is a compiled builder — an opaque function that cannot be reversed. It must never
// match, and in particular an empty template must not behave as a wildcard against an empty argv.
func TestMatchTemplateNeverMatchesANonTemplatedClass(t *testing.T) {
	t.Parallel()
	compiled := OpClassSpec{OpClass: "c", Family: FamilyServiceLifecycle, SafetyTier: TierLowReversible}
	for _, argv := range [][]string{nil, {}, {"anything"}, {"systemctl", "restart", "nginx"}} {
		if _, _, hit := MatchTemplate(compiled, argv); hit {
			t.Errorf("a compiled (non-templated) class matched argv %v", argv)
		}
	}
}

// Zero slots yields no value for the allowlist to check; two slots cannot be reduced to the single
// (class, value) pair the leaf speaks in. Both must refuse rather than return a partial answer.
func TestMatchTemplateRefusesZeroAndMultiSlotTemplates(t *testing.T) {
	t.Parallel()
	noSlot := OpClassSpec{OpClass: "t", Family: FamilyServiceLifecycle, SafetyTier: TierLowReversible,
		ArgvTemplate: []string{"systemctl", "daemon-reload"}}
	if _, _, hit := MatchTemplate(noSlot, []string{"systemctl", "daemon-reload"}); hit {
		t.Error("an all-literal template matched — there is no value to hand the allowlist")
	}

	twoSlot := OpClassSpec{OpClass: "t", Family: FamilyServiceLifecycle, SafetyTier: TierLowReversible,
		ArgvTemplate: []string{"systemctl", "${verb}", "${unit}"},
		Params: []ParamSpec{{Name: "verb", Type: "string", Required: true},
			{Name: "unit", Type: "string", Required: true}}}
	if v, _, hit := MatchTemplate(twoSlot, []string{"systemctl", "restart", "nginx"}); hit {
		t.Errorf("a two-slot template matched and returned %q — one of two values is not a classification", v)
	}
}
