package opschema

import (
	"strings"
	"testing"
)

// restart-service is the v1 registry entry: its argv is [systemctl restart <unit>], built ONLY here.
func TestRestartServiceArgv(t *testing.T) {
	argv, err := Argv("restart-service", map[string]string{ParamUnit: "nginx"})
	if err != nil {
		t.Fatalf("restart-service with a unit must build an argv: %v", err)
	}
	if len(argv) != 3 || argv[0] != "systemctl" || argv[1] != "restart" || argv[2] != "nginx" {
		t.Fatalf("restart-service argv must be exactly [systemctl restart nginx], got %v", argv)
	}
}

// A missing unit is a fail-closed error at BOTH the builder and the validator — never a silent empty argv.
func TestRestartServiceMissingUnitFailsClosed(t *testing.T) {
	spec, ok := Lookup("restart-service")
	if !ok {
		t.Fatal("restart-service must be registered")
	}
	if _, err := spec.Argv(map[string]string{}); err == nil {
		t.Fatal("restart-service with no unit must NOT build an argv")
	}
	verr := ValidateArgs(spec, map[string]string{})
	if verr == nil {
		t.Fatal("restart-service with no unit must be rejected by ValidateArgs")
	}
	if !strings.Contains(verr.Error(), ParamUnit) {
		t.Fatalf("the rejection must actionably name the missing param %q, got %q", ParamUnit, verr.Error())
	}
}

// start-service: the down-service remediation — argv [systemctl start <unit>], built ONLY here; a missing
// unit fails closed at both the builder and the validator (mirrors restart-service).
func TestStartServiceArgv(t *testing.T) {
	argv, err := Argv("start-service", map[string]string{ParamUnit: "nginx"})
	if err != nil {
		t.Fatalf("start-service with a unit must build an argv: %v", err)
	}
	if len(argv) != 3 || argv[0] != "systemctl" || argv[1] != "start" || argv[2] != "nginx" {
		t.Fatalf("start-service argv must be exactly [systemctl start nginx], got %v", argv)
	}
}

func TestStartServiceMissingUnitFailsClosed(t *testing.T) {
	spec, ok := Lookup("start-service")
	if !ok {
		t.Fatal("start-service must be registered")
	}
	if _, err := spec.Argv(map[string]string{}); err == nil {
		t.Fatal("start-service with no unit must NOT build an argv")
	}
	if verr := ValidateArgs(spec, map[string]string{}); verr == nil {
		t.Fatal("start-service with no unit must be rejected by ValidateArgs")
	}
}

// Lookup is an EXACT registered-slug match — an unregistered op_class fails closed (no argv builder).
func TestLookupUnregisteredFailsClosed(t *testing.T) {
	if _, ok := Lookup("kubectl-get"); ok {
		t.Fatal("an unregistered op_class must not resolve")
	}
	if _, err := Argv("kubectl-get", map[string]string{"x": "y"}); err == nil {
		t.Fatal("an unregistered op_class must not build an argv")
	}
	// case/whitespace variants normalize to the same registered slug (fail-closed direction).
	if _, ok := Lookup("  Restart-Service "); !ok {
		t.Fatal("a case/whitespace variant of a registered slug must still resolve")
	}
}

// (d) THE poka-yoke invariant: the validator must be EXACTLY as tolerant as the argv builder it guards —
// no stricter (rejecting a unit the builder accepts → the 0/60 proposing regression), no looser (passing a
// params set that yields an empty/invalid argv). For every input, ValidateArgs-passes IFF Argv-succeeds.
func TestValidatorToleranceEqualsBuilderTolerance(t *testing.T) {
	// Cover EVERY registered op-class (the loaded schema + its compiled builder), so schema drift on any class
	// — not just restart-service — is caught at test time rather than only fail-closed at runtime. Each case
	// set is derived from the op-class's OWN required params, so it adapts as the loadable schema changes.
	for _, spec := range Specs() {
		spec := spec
		// The validator-tolerance == builder-tolerance property is an ARGV-ENCODED property (REQ-1204): it pins the
		// param validator to the compiled argv BUILDER (ssh-argv, proxmox-lifecycle). A launch-encoded class
		// (awx-launch) has no builder (Argv always errors — its effect is encoded elsewhere), so the property does
		// not apply; the runner's LaunchSpec encoding has its own typed-extra_vars validation. Skip launch-encoded.
		if !argvEncoded(spec.Kind()) {
			continue
		}
		t.Run(spec.OpClass, func(t *testing.T) {
			cases := []map[string]string{
				nil, // no params at all
				{},  // empty params
				{"x": "z"}, // only an unrelated extra param
			}
			// For each declared required param, exercise blank / whitespace / valid / padded / extra-param.
			for _, p := range spec.Params {
				if !p.Required {
					continue
				}
				val := p.Example
				if val == "" {
					val = "v"
				}
				cases = append(cases,
					map[string]string{p.Name: ""},                  // present but blank
					map[string]string{p.Name: "   "},               // whitespace-only
					map[string]string{p.Name: val},                 // a valid value
					map[string]string{p.Name: "  " + val + "  "},    // surrounding whitespace (builder trims; both accept)
					map[string]string{p.Name: val, "extra": "z"},    // an extra param the schema ignores
				)
			}
			for _, params := range cases {
				validatorOK := ValidateArgs(spec, params) == nil
				_, buildErr := spec.Argv(params)
				builderOK := buildErr == nil
				if validatorOK != builderOK {
					t.Fatalf("validator/builder tolerance MISMATCH for %s params=%v: validatorOK=%v builderOK=%v — a poka-yoke validator must be exactly as tolerant as the reader it guards", spec.OpClass, params, validatorOK, builderOK)
				}
			}
		})
	}
}

// The prompt catalog renders the schema the agent must satisfy: the op_class, the required param, and an
// example — this is what makes the agent emit params.unit for restart-service.
func TestCatalogRendersRestartServiceSchema(t *testing.T) {
	cat := Catalog()
	for _, want := range []string{"restart-service", "unit", "required", "nginx"} {
		if !strings.Contains(cat, want) {
			t.Fatalf("op-class catalog must render %q; got:\n%s", want, cat)
		}
	}
}

// --- effect-kind: the non-SSH (awx-launch) channel (TG-139/TG-151) --------------------------------------

func TestEffectKindDefaultsToSSHArgv(t *testing.T) {
	// A shipped class with no effect_kind field is ssh-argv (behavior-preserving; the loadable schema need not
	// stamp the default on every existing class).
	s, ok := Lookup("restart-service")
	if !ok {
		t.Fatal("restart-service must be registered")
	}
	if s.Kind() != EffectSSHArgv {
		t.Fatalf("absent effect_kind must default to %q, got %q", EffectSSHArgv, s.Kind())
	}
}

func TestAWXLaunchClassRegistersWithoutABuilder(t *testing.T) {
	js := []byte(`{"op_classes":[{"op_class":"disk-grow","op":"grow","effect_kind":"awx-launch","params":[` +
		`{"name":"filesystem","type":"string","required":true,"description":"the mount to grow"},` +
		`{"name":"grow_by","type":"string","required":true,"description":"the amount to grow by, e.g. 10G"}]}]}`)
	reg := mustBuildRegistry(js, map[string]ArgvBuilder{}) // NO builder — required for an awx-launch class
	s, ok := reg["disk-grow"]
	if !ok {
		t.Fatal("an awx-launch op-class must register")
	}
	if s.Kind() != EffectAWXLaunch {
		t.Fatalf("Kind()=%q want %q", s.Kind(), EffectAWXLaunch)
	}
	// It has NO SSH argv — Argv fails closed (the runner encodes its LaunchSpec instead of an argv).
	if _, err := s.Argv(map[string]string{"filesystem": "/var", "grow_by": "10G"}); err == nil {
		t.Fatal("an awx-launch class must have no SSH argv (Argv must error)")
	}
	// ValidateArgs still screens its params (the extra_vars-to-be), effect-kind-agnostic.
	if ValidateArgs(s, map[string]string{"filesystem": "/var"}) == nil {
		t.Fatal("ValidateArgs must reject a missing required awx-launch param")
	}
	if err := ValidateArgs(s, map[string]string{"filesystem": "/var", "grow_by": "10G"}); err != nil {
		t.Fatalf("ValidateArgs must accept complete awx-launch params: %v", err)
	}
}

func TestEffectKindRegistryFailsClosed(t *testing.T) {
	assertOpschemaPanics(t, "awx-launch WITH a builder (contradiction)", func() {
		mustBuildRegistry(
			[]byte(`{"op_classes":[{"op_class":"disk-grow","effect_kind":"awx-launch","params":[]}]}`),
			map[string]ArgvBuilder{"disk-grow": func(map[string]string) ([]string, error) { return []string{"x"}, nil }},
		)
	})
	assertOpschemaPanics(t, "ssh-argv (default) with NO builder (unactuatable)", func() {
		mustBuildRegistry([]byte(`{"op_classes":[{"op_class":"restart-foo","params":[]}]}`), map[string]ArgvBuilder{})
	})
	assertOpschemaPanics(t, "unknown effect_kind", func() {
		mustBuildRegistry([]byte(`{"op_classes":[{"op_class":"weird","effect_kind":"telepathy","params":[]}]}`), map[string]ArgvBuilder{})
	})
	assertOpschemaPanics(t, "compiled builder with no schema (unreachable)", func() {
		mustBuildRegistry([]byte(`{"op_classes":[]}`), map[string]ArgvBuilder{"ghost": func(map[string]string) ([]string, error) { return nil, nil }})
	})
}

func assertOpschemaPanics(t *testing.T, what string, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatalf("%s: expected a fail-closed panic, got none", what)
		}
	}()
	fn()
}

// --- start-guest (proxmox-lifecycle: argv-encoded, kind-routed) — TG-138 ---------------------------------

func TestStartGuestArgv(t *testing.T) {
	s, ok := Lookup("start-guest")
	if !ok {
		t.Fatal("start-guest must be registered")
	}
	if s.Kind() != EffectProxmoxLifecycle {
		t.Fatalf("start-guest Kind()=%q, want %q", s.Kind(), EffectProxmoxLifecycle)
	}
	if !argvEncoded(s.Kind()) {
		t.Fatal("proxmox-lifecycle must be argv-encoded (it has a compiled builder)")
	}
	argv, err := s.Argv(map[string]string{ParamGuest: "librespeed01"})
	if err != nil {
		t.Fatalf("start-guest Argv: %v", err)
	}
	if want := []string{"start", "librespeed01"}; len(argv) != 2 || argv[0] != want[0] || argv[1] != want[1] {
		t.Fatalf("start-guest argv = %v, want %v", argv, want)
	}
	// Missing guest fails closed at BOTH the builder and the validator (tolerance).
	if _, err := s.Argv(map[string]string{}); err == nil {
		t.Fatal("start-guest with no guest must fail closed (no argv)")
	}
	if ValidateArgs(s, map[string]string{}) == nil {
		t.Fatal("start-guest with no guest must be rejected by ValidateArgs")
	}
}
