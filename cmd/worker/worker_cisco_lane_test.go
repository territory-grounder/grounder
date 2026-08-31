package main

// Drills for the arm-live cisco lane construction (TG-85 items 4+5): the leaves exist exactly when the
// operator declared-and-armed them, every fail-closed direction refuses at BOOT (parse or construction),
// and the per-target resolution admits only declared devices.

import (
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/regime"
)

func writablePolicySpec(ops string) string {
	base := `[{"device_id":"fw01","host":"192.0.2.10","identity":"tg","key_ref":"bao:secret/tg/cisco#key",` +
		`"known_hosts":"/etc/tg/kh","platform":"asa","allowed_config_prefixes":["interface "]` + ops + `}]`
	return base
}

func TestParseCiscoWriteDevicesReversibleOpsFailClosed(t *testing.T) {
	cases := []struct{ name, raw, want string }{
		{"ops without platform",
			`[{"device_id":"fw01","host":"h","identity":"tg","key_ref":"bao:x#k","known_hosts":"/kh",` +
				`"reversible_ops":[{"name":"x","op_class":"c","forward":["a"],"inverse":["b"]}]}]`,
			"require an explicit platform"},
		{"unknown platform", `[{"device_id":"fw01","host":"h","identity":"tg","key_ref":"bao:x#k","known_hosts":"/kh","platform":"nexus"}]`,
			"unknown platform"},
		{"op missing inverse",
			`[{"device_id":"fw01","host":"h","identity":"tg","key_ref":"bao:x#k","known_hosts":"/kh","platform":"asa",` +
				`"reversible_ops":[{"name":"x","op_class":"c","forward":["a"]}]}]`,
			"forward and inverse are all required"},
	}
	for _, c := range cases {
		if _, err := parseCiscoWriteDevices(c.raw); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: want refusal containing %q, got %v", c.name, c.want, err)
		}
	}
}

func TestBuildCiscoWriteLeavesConstructsHostBoundLeaves(t *testing.T) {
	ops := `,"reversible_ops":[{"name":"tunnel-reup","op_class":"tunnel-reup","forward":["tunnel-group 198.51.100.1 ipsec-attributes"],"inverse":["no tunnel-group 198.51.100.1 ipsec-attributes"],"why":"drill"}]`
	pol, err := parseCiscoWriteDevices(writablePolicySpec(ops))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	leaves, err := buildCiscoWriteLeaves(pol, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	for _, key := range []string{"fw01", "192.0.2.10"} {
		leaf, ok := leaves[key]
		if !ok {
			t.Fatalf("leaf missing under %q (keys must cover device_id AND host)", key)
		}
		hb, ok := leaf.(interface{ ActuationHost() string })
		if !ok || hb.ActuationHost() != "192.0.2.10" {
			t.Fatalf("leaf under %q must be host-bound to the device host (got %T)", key, leaf)
		}
		if !leaf.ReadOnly() {
			t.Fatal("a leaf with a NIL chokepoint must report ReadOnly (mutation off, fail closed)")
		}
	}
}

func TestBuildCiscoWriteLeavesRefusesFloorListedOp(t *testing.T) {
	// interface-shutdown sits on core/safety's non-configurable never-auto floor; the registry's clamp
	// must refuse it AT CONSTRUCTION — the operator learns from the boot, not from a rollback.
	ops := `,"reversible_ops":[{"name":"if-down","op_class":"interface-shutdown","forward":["shutdown"],"inverse":["no shutdown"],"why":"nope"}]`
	pol, err := parseCiscoWriteDevices(writablePolicySpec(ops))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, err := buildCiscoWriteLeaves(pol, nil); err == nil {
		t.Fatal("a floor-listed op_class must refuse leaf construction (fail closed at boot)")
	}
}

func TestBuildCiscoWriteLeavesSkipsUnwritableDevice(t *testing.T) {
	pol, err := parseCiscoWriteDevices(`[{"device_id":"fw02","host":"192.0.2.11","identity":"tg",` +
		`"key_ref":"bao:x#k","known_hosts":"/kh"}]`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	leaves, err := buildCiscoWriteLeaves(pol, nil)
	if err != nil || len(leaves) != 0 {
		t.Fatalf("a device with no prefixes and no ops has no write path — nothing may construct (got %d, %v)", len(leaves), err)
	}
}

// The dormancy pair: the SAME invalid-op policy set is inert unarmed and fatal armed — arming is what makes
// operator config load-bearing, and nothing constructs before it.
func TestCiscoInteractiveLaneForDormancyPair(t *testing.T) {
	bad := `,"reversible_ops":[{"name":"if-down","op_class":"interface-shutdown","forward":["shutdown"],"inverse":["no shutdown"],"why":"nope"}]`
	pol, err := parseCiscoWriteDevices(writablePolicySpec(bad))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	lane, err := ciscoInteractiveLaneFor(pol, nil, false)
	if err != nil || lane == nil || lane.Regime() != regime.RegimeCiscoInteractive {
		t.Fatalf("unarmed: the lane must exist as the fail-closed pending channel, constructing NOTHING (err=%v)", err)
	}
	if _, err := ciscoInteractiveLaneFor(pol, nil, true); err == nil {
		t.Fatal("armed: the same forbidden op must refuse lane construction (the boot is where the operator learns)")
	}
}

func TestCiscoResolveLeafAdmitsOnlyDeclaredTargets(t *testing.T) {
	pol, err := parseCiscoWriteDevices(writablePolicySpec(""))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	leaves, err := buildCiscoWriteLeaves(pol, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	// This drives the SHIPPED resolution function — the same one the lane's closure delegates to.
	if leaf, err := ciscoResolveLeaf(leaves, []string{"fw01"}, "FW01"); err != nil || leaf == nil {
		t.Fatalf("declared device_id must resolve case-insensitively: %v", err)
	}
	if leaf, err := ciscoResolveLeaf(leaves, []string{"fw01"}, "192.0.2.10"); err != nil || leaf == nil {
		t.Fatalf("declared host must resolve: %v", err)
	}
	if _, err := ciscoResolveLeaf(leaves, []string{"fw01"}, "dc1fw99"); err == nil || !strings.Contains(err.Error(), "declared: fw01") {
		t.Fatalf("an undeclared target must refuse NAMING the declared set, got %v", err)
	}
	if _, err := ciscoResolveLeaf(leaves, []string{"fw01"}, "  "); err == nil {
		t.Fatal("an empty target must refuse")
	}
}

// The review catch (2026-08-25): an ops-ONLY device — no config-line prefixes, only named reversible ops,
// the narrowest declared shape — must ARM. The old predicate counted only prefixes, so this shape parsed,
// reported writable=false, and the lane silently stayed pending while buildCiscoWriteLeaves would have
// constructed for it: the arming gate and the construction gate disagreed and no fixture exercised it.
func TestOpsOnlyDeviceArmsTheLane(t *testing.T) {
	opsOnly := `[{"device_id":"fw03","host":"192.0.2.12","identity":"tg","key_ref":"bao:x#k","known_hosts":"/kh",` +
		`"platform":"asa","reversible_ops":[{"name":"tunnel-reup","op_class":"tunnel-reup",` +
		`"forward":["tunnel-group 198.51.100.1 ipsec-attributes"],"inverse":["no tunnel-group 198.51.100.1 ipsec-attributes"],"why":"drill"}]}]`
	pol, err := parseCiscoWriteDevices(opsOnly)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !ciscoWriteHasWritablePolicy(pol) {
		t.Fatal("an ops-only device IS a write path — the arming predicate must count reversible_ops (the 2026-08-25 review catch)")
	}
	leaves, err := buildCiscoWriteLeaves(pol, nil)
	if err != nil || len(leaves) == 0 {
		t.Fatalf("an ops-only device must construct its leaf: %d, %v", len(leaves), err)
	}
	// And the posture line must not lie about it.
	if s := ciscoWritePreflight(pol, false, true); !strings.HasPrefix(s, "ARMED-CONFIG") || !strings.Contains(s, "1 reversible op(s)") {
		t.Fatalf("armed ops-only posture must report ARMED-CONFIG with the op count: %q", s)
	}
	// The free-form path on that leaf stays REFUSING (empty prefix allowlist): ReadOnly with a nil gate,
	// and the write path exists only as the named-op route once the ExecOp integration lands.
	if !leaves["fw03"].ReadOnly() {
		t.Fatal("ops-only leaf with a nil chokepoint must still report ReadOnly (fail closed)")
	}
}
