package opschema

import (
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// A templated op-class is the DATA alternative to a compiled ArgvBuilder. The safety claim is that it does
// not weaken INV-02 — the rendered result is a fixed argv vector built from an OPERATOR-authored template
// and typed, validated params, with no model token contributing and no shell anywhere. These tests are that
// claim, executable.

func tmplSpec(t *testing.T, template []string, params []ParamSpec) OpClassSpec {
	t.Helper()
	return OpClassSpec{OpClass: "t", Family: FamilyServiceLifecycle, SafetyTier: TierLowReversible,
		ArgvTemplate: template, Params: params}
}

// TestMigratedVerbsStillRenderTheirPreMigrationArgv is the migration-safety oracle, and the GOLDENS below are
// the point of it. Before 2026-07-28 these four verbs were compiled Go builders; they are now argv templates
// carried as registry data. The vectors here are exactly what those builders emitted, transcribed from the
// code that was deleted.
//
// This test was ORIGINALLY written to compare the template against the live compiled builder. That version
// silently became a tautology the moment the verbs migrated — Lookup returns the template, so it compared the
// template to itself and would have passed no matter what the migration changed. An oracle whose subject
// becomes its own reference has stopped being an oracle. Hardcoded goldens cannot degrade that way.
func TestMigratedVerbsStillRenderTheirPreMigrationArgv(t *testing.T) {
	t.Parallel()
	cases := []struct {
		opClass string
		params  map[string]string
		want    []string
	}{
		{"restart-service", map[string]string{"unit": "nginx"}, []string{"systemctl", "restart", "nginx"}},
		{"reload-service", map[string]string{"unit": "nginx"}, []string{"systemctl", "reload", "nginx"}},
		{"start-service", map[string]string{"unit": "nginx"}, []string{"systemctl", "start", "nginx"}},
		{"restart-container", map[string]string{"container": "mealie"}, []string{"docker", "restart", "mealie"}},
	}
	for _, c := range cases {
		shipped, ok := Lookup(c.opClass)
		if !ok {
			t.Fatalf("%s is not registered — this oracle would pass vacuously", c.opClass)
		}
		if len(shipped.ArgvTemplate) == 0 {
			t.Fatalf("%s is no longer templated — this oracle exists to guard the MIGRATED verbs; if the verb "+
				"went back to a compiled builder, say so deliberately rather than leaving a test that reads as "+
				"if it still checks the migration", c.opClass)
		}
		got, err := shipped.Argv(c.params)
		if err != nil {
			t.Fatalf("%s: %v", c.opClass, err)
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s renders %v, but its compiled builder produced %v — the migration CHANGED what this "+
				"verb actuates on the estate", c.opClass, got, c.want)
		}
	}
}

// TestTemplateSubstitutionIsWholeElementOnly is the injection oracle. A param value becomes EXACTLY ONE argv
// element whatever it contains — it cannot add an element, a flag, a separator or a redirection. There is no
// shell on this path, so shell metacharacters are inert filename bytes.
func TestTemplateSubstitutionIsWholeElementOnly(t *testing.T) {
	t.Parallel()
	spec := tmplSpec(t, []string{"systemctl", "restart", "${unit}"},
		[]ParamSpec{{Name: "unit", Type: "string", Required: true}})

	for _, hostile := range []string{
		"nginx; rm -rf /",
		"nginx && reboot",
		"nginx\nreboot",
		"--privileged",
		"$(reboot)",
		"`reboot`",
		"a b c",
	} {
		got, err := spec.Argv(map[string]string{"unit": hostile})
		if err != nil {
			t.Fatalf("render(%q): %v", hostile, err)
		}
		if len(got) != 3 {
			t.Errorf("param %q produced %d argv elements, want exactly 3 — a value must never change the "+
				"SHAPE of the command", hostile, len(got))
		}
		if got[0] != "systemctl" || got[1] != "restart" {
			t.Errorf("param %q altered the literal prefix: %v", hostile, got)
		}
		if got[2] != hostile {
			t.Errorf("param %q was transformed to %q — the renderer must substitute verbatim, never "+
				"interpret", hostile, got[2])
		}
	}
}

// TestTemplateRejectsAnUndeclaredSlotAtInit — a slot naming a param that does not exist could never be
// filled, so the class is unactuatable and must fail closed at boot rather than at 3am.
func TestTemplateRejectsAnUndeclaredSlotAtInit(t *testing.T) {
	t.Parallel()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("an undeclared template slot must panic at registry build")
		}
		if !strings.Contains(r.(string), "undeclared param") {
			t.Fatalf("panic did not name the problem: %v", r)
		}
	}()
	mustBuildRegistry([]byte(`{"op_classes":[{"op_class":"t","op":"do","family":"service-lifecycle",
		"safety_tier":"low-reversible","argv_template":["systemctl","restart","${nope}"],
		"params":[{"name":"unit","type":"string","required":true}]}]}`), map[string]ArgvBuilder{})
}

// TestTemplateRejectsAnOptionalSlotAtInit — an optional slot would render BLANK when absent, silently
// producing a different command than the template reads as. That is the quiet-wrong-command failure.
func TestTemplateRejectsAnOptionalSlotAtInit(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("an OPTIONAL template slot must panic at registry build")
		}
	}()
	mustBuildRegistry([]byte(`{"op_classes":[{"op_class":"t","op":"do","family":"service-lifecycle",
		"safety_tier":"low-reversible","argv_template":["systemctl","restart","${unit}"],
		"params":[{"name":"unit","type":"string","required":false}]}]}`), map[string]ArgvBuilder{})
}

// TestTemplateRejectsASlotEmbeddedInALargerElement is the fail-open oracle. It exists because the first cut
// of this renderer HAD the defect: templateSlotRE is whole-element anchored, so "--unit=${unit}" matched
// nothing, fell through as a literal, and rendered the eight characters "${unit}" onto the wire. Nothing
// errored — a silent, permissive wrong command, which is the exact shape that made three shipped safety
// regexes inert. An element is either a literal or a whole slot; anything in between fails closed at boot.
func TestTemplateRejectsASlotEmbeddedInALargerElement(t *testing.T) {
	for _, el := range []string{`--unit=${unit}`, `${unit}.service`, `pre${unit}post`, `${unit}${unit}`} {
		func() {
			defer func() {
				r := recover()
				if r == nil {
					t.Errorf("template element %q embeds a slot in a larger string and was ACCEPTED — it would "+
						"render the literal text ${...} onto the estate", el)
					return
				}
				if !strings.Contains(r.(string), "WHOLE-element") {
					t.Errorf("element %q: panic did not name the problem: %v", el, r)
				}
			}()
			mustBuildRegistry([]byte(`{"op_classes":[{"op_class":"t","op":"do","family":"service-lifecycle",
				"safety_tier":"low-reversible","argv_template":["systemctl","restart",`+
				strconv.Quote(el)+`],"params":[{"name":"unit","type":"string","required":true}]}]}`),
				map[string]ArgvBuilder{})
		}()
	}
}

// TestBuilderAndTemplateAreMutuallyExclusive — two definitions of what a class actuates is worse than none,
// because a reader cannot tell which one runs.
func TestBuilderAndTemplateAreMutuallyExclusive(t *testing.T) {
	t.Parallel()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("declaring BOTH a builder and a template must panic")
		}
		if !strings.Contains(r.(string), "BOTH") {
			t.Fatalf("panic did not name the contradiction: %v", r)
		}
	}()
	mustBuildRegistry([]byte(`{"op_classes":[{"op_class":"t","op":"do","family":"service-lifecycle",
		"safety_tier":"low-reversible","argv_template":["true"],"params":[]}]}`),
		map[string]ArgvBuilder{"t": func(map[string]string) ([]string, error) { return []string{"true"}, nil }})
}

// TestValidatorToleranceEqualsRendererTolerance — the aliases hazard. A params set ValidateArgs ACCEPTS must
// always render, and one it REJECTS must never. A gap either way means the validator and the thing that
// touches the estate disagree about what is legal.
func TestValidatorToleranceEqualsRendererTolerance(t *testing.T) {
	t.Parallel()
	spec := tmplSpec(t, []string{"systemctl", "restart", "${unit}"},
		[]ParamSpec{{Name: "unit", Type: "string", Required: true}})

	for _, params := range []map[string]string{
		{"unit": "nginx"}, {}, {"unit": ""}, {"unit": "   "}, {"unit": "nginx", "extra": "ignored"},
	} {
		validErr := ValidateArgs(spec, params)
		_, renderErr := spec.Argv(params)
		if (validErr == nil) != (renderErr == nil) {
			t.Errorf("params %v: ValidateArgs err=%v but render err=%v — validator and renderer disagree "+
				"about what is legal", params, validErr, renderErr)
		}
	}
}

// THE PROGRAM IS NEVER A SLOT (INV-02).
//
// argv[0] is the executable. A compiled ArgvBuilder structurally cannot let a param supply it — it is a
// literal in Go source. The migration from compiled builders to JSON `argv_template` was argued as "a
// template with typed, validated slots is exactly as safe as the hand-written function", and that holds for
// every element EXCEPT the first: ["${unit}", "--now"] rendered with unit="/bin/sh" produces argv[0]="/bin/sh"
// — a model-supplied program, the thing INV-02 exists to forbid.
//
// Found by adversarial audit 2026-07-28. LATENT, never live: all seven shipped templates start with
// `systemctl` or `docker`. That is precisely why it needs a boot-time gate rather than review — INV-02 must
// rest on the structure making it unrepresentable, not on nobody having written the bad template yet.
func TestATemplateMayNotBeginWithASlot(t *testing.T) {
	t.Run("argv_template", func(t *testing.T) {
		j := []byte(`{"op_classes":[{"op_class":"probe-argv0","family":"service-lifecycle","safety_tier":"low-reversible","op":"do","params":[{"name":"unit","type":"string","required":true}],"argv_template":["${unit}","--now"]}]}`)
		mustPanic(t, "argv[0]", func() { mustBuildRegistry(j, map[string]ArgvBuilder{}) })
	})
	t.Run("rollback_template", func(t *testing.T) {
		j := []byte(`{"op_classes":[{"op_class":"probe-rb0","family":"service-lifecycle","safety_tier":"low-reversible","op":"do","params":[{"name":"unit","type":"string","required":true}],"argv_template":["systemctl","start","${unit}"],"rollback_template":["${unit}"]}]}`)
		mustPanic(t, "argv[0]", func() { mustBuildRegistry(j, map[string]ArgvBuilder{}) })
	})
}

// The rule must not over-reach: a slot in any LATER position is the entire point of templates.
func TestASlotAfterTheProgramIsStillAllowed(t *testing.T) {
	j := []byte(`{"op_classes":[{"op_class":"probe-ok","family":"service-lifecycle","safety_tier":"low-reversible","op":"do","params":[{"name":"unit","type":"string","required":true}],"argv_template":["systemctl","restart","${unit}"]}]}`)
	m := mustBuildRegistry(j, map[string]ArgvBuilder{})
	if len(m) != 1 {
		t.Fatalf("a template with a literal program and a trailing slot was rejected — that is the ordinary form")
	}
}
