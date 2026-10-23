package opschema

import (
	"strings"
	"testing"
)

// mustPanic runs fn and fails the test unless it panics with a message containing want.
func mustPanic(t *testing.T, want string, fn func()) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("expected a panic containing %q, got none", want)
		}
		if msg := strings.ToLower(strings.TrimSpace(toStr(r))); !strings.Contains(msg, strings.ToLower(want)) {
			t.Fatalf("panic message %q does not contain %q", msg, want)
		}
	}()
	fn()
}

func toStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if e, ok := v.(error); ok {
		return e.Error()
	}
	return ""
}

func okBuilder(map[string]string) ([]string, error) { return []string{"true"}, nil }

// The embedded schema and the compiled builders must be in exact lockstep — every registered op-class resolves
// AND builds an argv, and the set matches the compiled builders map (neither a schema-only nor a builder-only
// entry can exist, since mustBuildRegistry would have panicked at init).
func TestLoadedSchemaAndCompiledBuildersAreInLockstep(t *testing.T) {
	specs := Specs()
	if len(specs) == 0 {
		t.Fatal("registry loaded empty — embedded opschema.json did not parse")
	}
	// The lockstep is an ARGV-ENCODED property: an argv-encoded class (ssh-argv, proxmox-lifecycle) must have
	// EXACTLY ONE way to produce its argv — a compiled builder OR an operator-authored template. Neither is
	// unactuatable; both are two contradictory definitions of what the class does. A launch-encoded class
	// (awx-launch) legitimately has neither: the runner encodes its effect.
	compiled := 0
	for _, s := range specs {
		if !argvEncoded(s.Kind()) {
			if s.build != nil {
				t.Fatalf("op-class %q is %q (launch-encoded) but carries a compiled argv builder (contradiction)", s.OpClass, s.Kind())
			}
			if len(s.ArgvTemplate) != 0 {
				t.Fatalf("op-class %q is %q (launch-encoded) but carries an argv_template (contradiction)", s.OpClass, s.Kind())
			}
			continue
		}
		_, hasBuilder := builders[normalize(s.OpClass)]
		hasTemplate := len(s.ArgvTemplate) != 0
		if hasBuilder == hasTemplate {
			t.Fatalf("argv-encoded op-class %q has builder=%v template=%v — it must have EXACTLY ONE (neither "+
				"is unactuatable; both are two contradictory definitions of what it actuates)",
				s.OpClass, hasBuilder, hasTemplate)
		}
		if hasBuilder {
			compiled++
			if s.build == nil {
				t.Fatalf("argv-encoded op-class %q loaded without its compiled builder attached", s.OpClass)
			}
		} else if s.build != nil {
			t.Fatalf("templated op-class %q also had a compiled builder attached at load", s.OpClass)
		}
	}
	if compiled != len(builders) {
		t.Fatalf("compiled-class count %d != compiled builder count %d — lockstep drift", compiled, len(builders))
	}
	// every compiled builder is reachable via a loaded (argv-encoded) schema
	for key := range builders {
		s, ok := Lookup(key)
		if !ok {
			t.Fatalf("compiled builder %q has no loaded schema (unreachable)", key)
		}
		if !argvEncoded(s.Kind()) {
			t.Fatalf("compiled builder %q backs a %q op-class, not argv-encoded (contradiction)", key, s.Kind())
		}
	}
}

func TestMustBuildRegistryValid(t *testing.T) {
	j := []byte(`{"op_classes":[{"op_class":"x-op","family":"service-lifecycle","safety_tier":"low-reversible","op":"do","params":[{"name":"a","type":"string","required":true}]}]}`)
	m := mustBuildRegistry(j, map[string]ArgvBuilder{"x-op": okBuilder})
	if len(m) != 1 {
		t.Fatalf("want 1 op-class, got %d", len(m))
	}
	if _, ok := m["x-op"]; !ok {
		t.Fatal("x-op not registered")
	}
}

func TestMustBuildRegistryFailsClosed(t *testing.T) {
	good := map[string]ArgvBuilder{"x-op": okBuilder}

	t.Run("malformed JSON", func(t *testing.T) {
		mustPanic(t, "parse", func() { mustBuildRegistry([]byte(`{not json`), good) })
	})
	t.Run("schema with no compiled builder", func(t *testing.T) {
		j := []byte(`{"op_classes":[{"op_class":"orphan","family":"service-lifecycle","safety_tier":"low-reversible","op":"do","params":[]}]}`)
		mustPanic(t, "neither a compiled argv builder nor an argv_template", func() { mustBuildRegistry(j, good) })
	})
	t.Run("compiled builder with no schema", func(t *testing.T) {
		j := []byte(`{"op_classes":[]}`)
		mustPanic(t, "no schema", func() { mustBuildRegistry(j, good) })
	})
	t.Run("duplicate op-class", func(t *testing.T) {
		j := []byte(`{"op_classes":[{"op_class":"x-op","family":"service-lifecycle","safety_tier":"low-reversible"},{"op_class":"X-Op","family":"service-lifecycle","safety_tier":"low-reversible"}]}`)
		mustPanic(t, "duplicate", func() { mustBuildRegistry(j, good) })
	})
	t.Run("blank op_class", func(t *testing.T) {
		j := []byte(`{"op_classes":[{"op_class":"  "}]}`)
		mustPanic(t, "blank op_class", func() { mustBuildRegistry(j, map[string]ArgvBuilder{}) })
	})
}
