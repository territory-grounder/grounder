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
	// The schema⟷builder lockstep is an ARGV-ENCODED property: only argv-encoded classes (ssh-argv,
	// proxmox-lifecycle) carry a compiled argv builder. A launch-encoded class (awx-launch) legitimately has NO
	// builder — the runner encodes its effect — so count/require builders over the argv-encoded classes ONLY.
	argvClasses := 0
	for _, s := range specs {
		if !argvEncoded(s.Kind()) {
			if s.build != nil {
				t.Fatalf("op-class %q is %q (launch-encoded) but carries a compiled argv builder (contradiction)", s.OpClass, s.Kind())
			}
			continue
		}
		argvClasses++
		if _, ok := builders[normalize(s.OpClass)]; !ok {
			t.Fatalf("argv-encoded op-class %q has a schema but no compiled builder", s.OpClass)
		}
		if s.build == nil {
			t.Fatalf("argv-encoded op-class %q loaded without its compiled builder attached", s.OpClass)
		}
	}
	if argvClasses != len(builders) {
		t.Fatalf("argv-encoded schema count %d != compiled builder count %d — lockstep drift", argvClasses, len(builders))
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
	j := []byte(`{"op_classes":[{"op_class":"x-op","op":"do","params":[{"name":"a","type":"string","required":true}]}]}`)
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
		j := []byte(`{"op_classes":[{"op_class":"orphan","op":"do","params":[]}]}`)
		mustPanic(t, "no compiled argv builder", func() { mustBuildRegistry(j, good) })
	})
	t.Run("compiled builder with no schema", func(t *testing.T) {
		j := []byte(`{"op_classes":[]}`)
		mustPanic(t, "no schema", func() { mustBuildRegistry(j, good) })
	})
	t.Run("duplicate op-class", func(t *testing.T) {
		j := []byte(`{"op_classes":[{"op_class":"x-op"},{"op_class":"X-Op"}]}`)
		mustPanic(t, "duplicate", func() { mustBuildRegistry(j, good) })
	})
	t.Run("blank op_class", func(t *testing.T) {
		j := []byte(`{"op_classes":[{"op_class":"  "}]}`)
		mustPanic(t, "blank op_class", func() { mustBuildRegistry(j, map[string]ArgvBuilder{}) })
	})
}
