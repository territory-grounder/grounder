package cpconfig

import (
	"context"
	"testing"
)

// fakeConsole is an in-test ConsoleStore.
type fakeConsole struct {
	overrides map[string]string
	err       error
}

func (f fakeConsole) Overrides(context.Context) (map[string]string, error) {
	return f.overrides, f.err
}

func find(vals []Value, name string) (Value, bool) {
	for _, v := range vals {
		if v.Name == name {
			return v, true
		}
	}
	return Value{}, false
}

func TestResolveLawIsPinned(t *testing.T) {
	// Even if env AND a console override try to set a LAW key, it resolves to the compiled Law value.
	r := Resolver{
		Law: map[string]string{
			"safety.never_auto_floor":    "enforced",
			"safety.mutation_enabled":    "off",
			"safety.predict_then_verify": "required",
		},
		Env:     map[string]string{"safety.mutation_enabled": "on", "safety.never_auto_floor": "disabled"},
		Console: fakeConsole{overrides: map[string]string{"safety.mutation_enabled": "on"}},
	}
	vals, err := r.Resolve(context.Background())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	mut, ok := find(vals, "safety.mutation_enabled")
	if !ok || mut.Source != SourceLaw || mut.Value != "off" {
		t.Fatalf("LAW mutation must stay law-pinned 'off', got %+v", mut)
	}
	floor, _ := find(vals, "safety.never_auto_floor")
	if floor.Source != SourceLaw || floor.Value != "enforced" {
		t.Fatalf("LAW floor must stay law-pinned, got %+v", floor)
	}
}

func TestResolveEnvAndDefault(t *testing.T) {
	r := Resolver{
		Law: map[string]string{"safety.never_auto_floor": "enforced", "safety.mutation_enabled": "off", "safety.predict_then_verify": "required"},
		Env: map[string]string{"gateway.litellm_url": "http://litellm:4000"},
		// no console store (Phase A)
	}
	vals, err := r.Resolve(context.Background())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	// env-supplied key shows env
	if v, _ := find(vals, "gateway.litellm_url"); v.Source != SourceEnv || v.Value != "http://litellm:4000" {
		t.Fatalf("litellm_url should be env-sourced, got %+v", v)
	}
	// a non-law key with no env value falls to default (empty)
	if v, _ := find(vals, "net.public_addr"); v.Source != SourceDefault || v.Value != "" {
		t.Fatalf("public_addr should be default, got %+v", v)
	}
	// nil Console store ⇒ no console-sourced values anywhere
	for _, v := range vals {
		if v.Source == SourceConsole {
			t.Fatalf("Phase A (nil Console) must yield no console sources, got %+v", v)
		}
	}
}

func TestResolveConsoleOnlyForWritableNonLaw(t *testing.T) {
	r := Resolver{
		Law: map[string]string{"safety.never_auto_floor": "enforced", "safety.mutation_enabled": "off", "safety.predict_then_verify": "required"},
		Env: map[string]string{"gateway.litellm_url": "http://env:4000", "operator.name": "kyriakos"},
		Console: fakeConsole{overrides: map[string]string{
			"gateway.litellm_url": "http://console:4000", // console-writable → honored
			"operator.name":       "attacker",            // NOT console-writable → ignored
		}},
	}
	vals, _ := r.Resolve(context.Background())
	if v, _ := find(vals, "gateway.litellm_url"); v.Source != SourceConsole || v.Value != "http://console:4000" {
		t.Fatalf("console override should win for a writable key, got %+v", v)
	}
	// operator.name is not console-writable: the console entry must be ignored, env wins
	if v, _ := find(vals, "operator.name"); v.Source != SourceEnv || v.Value != "kyriakos" {
		t.Fatalf("non-writable key must ignore console override, got %+v", v)
	}
}
