package catalog

import (
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/modules/desc"
)

// A SECRET VALUE must never become a config key.
//
// The config store is Postgres-backed and its writes are ledgered — a secret entering it would be
// durably recorded in two places that are read back by design. Secrets travel their own lane to the
// secret backend and are never returned by any read.
//
// KILLING MUTATION: remove the TypeSecretValue skip in ConfigKeys(). RED.
func TestSecretValuesNeverBecomeConfigKeys(t *testing.T) {
	all, err := All()
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	secretFields := map[string]bool{}
	for _, d := range all {
		for _, f := range d.Fields {
			if f.Type == desc.TypeSecretValue {
				secretFields[ConfigKeyName(d.Surface, d.SourceType, f.Name)] = true
			}
		}
	}
	if len(secretFields) == 0 {
		t.Fatal("vacuity floor: no descriptor declares a secret value, so this proves nothing")
	}
	for _, k := range ConfigKeys() {
		if secretFields[k.Name] {
			t.Errorf("secret field %q became a console config key — it would be written to Postgres and "+
				"recorded in a ledger row", k.Name)
		}
	}
}

// A read-only field (the secret POINTER) is display-only provenance and must not be writable.
func TestReadOnlyFieldsAreNotWritable(t *testing.T) {
	all, _ := All()
	ro := map[string]bool{}
	for _, d := range all {
		for _, f := range d.Fields {
			if f.Effect == desc.EffectReadOnly {
				ro[ConfigKeyName(d.Surface, d.SourceType, f.Name)] = true
			}
		}
	}
	if len(ro) == 0 {
		t.Fatal("vacuity floor: no read-only field declared")
	}
	for _, k := range ConfigKeys() {
		if ro[k.Name] {
			t.Errorf("read-only field %q is console-writable — the operator could rewrite the secret "+
				"pointer and move the token out from under the boot secret-policy gate", k.Name)
		}
	}
}

// Every generated key must live under the module namespace, or core/cpconfig will drop it and the field
// will silently have no config key at all.
func TestEveryGeneratedKeyIsNamespaced(t *testing.T) {
	keys := ConfigKeys()
	if len(keys) == 0 {
		t.Fatal("vacuity floor: the catalog generated no config keys")
	}
	for _, k := range keys {
		if !strings.HasPrefix(k.Name, "module.") {
			t.Errorf("generated key %q is outside the module namespace and will be dropped", k.Name)
		}
		if !k.ConsoleWritable {
			t.Errorf("generated key %q is not console-writable — the dialog could not save it", k.Name)
		}
	}
}

// The secret exclusion must hold on its OWN, not because the field happens to lack an env key.
//
// This is the oracle the first version of TestSecretValuesNeverBecomeConfigKeys was missing: removing the
// TypeSecretValue skip left it GREEN, because `EnvKey == ""` excluded the same field for an unrelated
// reason. Exclusion by coincidence is not exclusion.
//
// KILLING MUTATION: remove the TypeSecretValue skip in configKeysFrom. RED.
func TestSecretIsExcludedEvenWhenItCarriesAnEnvKey(t *testing.T) {
	got := configKeysFrom([]desc.Descriptor{{
		Surface: "notifier", SourceType: "probe", Title: "Probe",
		Fields: []desc.Field{
			{Name: "token", EnvKey: "TG_PROBE_TOKEN", Type: desc.TypeSecretValue,
				Security: desc.SecSecret, Effect: desc.EffectLive},
			{Name: "url", EnvKey: "TG_PROBE_URL", Type: desc.TypeURL,
				Security: desc.SecOrdinary, Effect: desc.EffectLive},
		},
	}})
	for _, k := range got {
		if strings.HasSuffix(k.Name, ".token") {
			t.Fatalf("a SECRET VALUE with an env key became config key %q — it would be written to "+
				"Postgres and recorded in a ledger row", k.Name)
		}
	}
	if len(got) != 1 || !strings.HasSuffix(got[0].Name, ".url") {
		t.Fatalf("the non-secret field was lost too: %+v", got)
	}
}
