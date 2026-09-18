package main

import (
	"strings"
	"testing"
)

const ciscoOneDevice = `[{"device_id":"fw01","host":"fw01.example","identity":"netops",
 "key_ref":"env:CISCO_KEY","known_hosts":"/etc/tg/cisco_known_hosts",
 "allowed_config_prefixes":["interface ","description "]}]`

// The happy path: a full device policy parses into a transport profile + its closed prefix allowlist, and the
// credential stays a REFERENCE (INV-13) — no key material in config.
func TestParseCiscoWriteDevices(t *testing.T) {
	got, err := parseCiscoWriteDevices(ciscoOneDevice)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	p, ok := got["fw01"]
	if !ok {
		t.Fatalf("device fw01 missing: %+v", got)
	}
	if p.Device.Host != "fw01.example" || p.Device.Identity != "netops" || p.Device.KnownHosts == "" {
		t.Errorf("transport profile not parsed: %+v", p.Device)
	}
	if string(p.Device.KeyRef) != "env:CISCO_KEY" {
		t.Errorf("credential must stay a SecretRef, got %q", p.Device.KeyRef)
	}
	if len(p.Allowed) != 2 || p.Allowed[0] != "interface " {
		t.Errorf("config-line prefixes not parsed: %+v", p.Allowed)
	}
}

// An empty policy is not an error — it is the DEFAULT posture (the lane is simply not configured).
func TestParseCiscoWriteDevicesEmptyIsNotAnError(t *testing.T) {
	got, err := parseCiscoWriteDevices("   ")
	if err != nil || got != nil {
		t.Fatalf("empty policy must parse to no devices, got %v / %v", got, err)
	}
}

// Fail-closed matrix: a device that cannot be connected to SAFELY (no host / no login / no credential / no
// host-key pinning), a duplicate id, a malformed prompt, or non-JSON must all be refused — never defaulted past.
func TestParseCiscoWriteDevicesFailsClosed(t *testing.T) {
	cases := map[string]string{
		"not json":       `{"device_id":"fw01"}`,
		"no device_id":   `[{"host":"h","identity":"i","key_ref":"env:K","known_hosts":"/kh"}]`,
		"no host":        `[{"device_id":"fw01","identity":"i","key_ref":"env:K","known_hosts":"/kh"}]`,
		"no identity":    `[{"device_id":"fw01","host":"h","key_ref":"env:K","known_hosts":"/kh"}]`,
		"no key_ref":     `[{"device_id":"fw01","host":"h","identity":"i","known_hosts":"/kh"}]`,
		"no known_hosts": `[{"device_id":"fw01","host":"h","identity":"i","key_ref":"env:K"}]`,
		"duplicate id": `[{"device_id":"fw01","host":"h","identity":"i","key_ref":"env:K","known_hosts":"/kh"},
		                  {"device_id":"fw01","host":"h2","identity":"i","key_ref":"env:K","known_hosts":"/kh"}]`,
		"bad prompt regexp": `[{"device_id":"fw01","host":"h","identity":"i","key_ref":"env:K","known_hosts":"/kh","prompt":"([unclosed"}]`,
	}
	for name, spec := range cases {
		if _, err := parseCiscoWriteDevices(spec); err == nil {
			t.Errorf("%s: must fail closed", name)
		}
	}
}

// A blank config-line prefix must NOT survive parsing: strings.HasPrefix(x, "") matches everything, so a stray
// empty entry would report a device as writable when its allowlist is meaningless.
func TestParseCiscoWriteDevicesDropsBlankPrefixes(t *testing.T) {
	got, err := parseCiscoWriteDevices(`[{"device_id":"fw01","host":"h","identity":"i","key_ref":"env:K",
	 "known_hosts":"/kh","allowed_config_prefixes":["","   ","interface "]}]`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if p := got["fw01"]; len(p.Allowed) != 1 || p.Allowed[0] != "interface " {
		t.Fatalf("blank prefixes must be dropped, got %+v", got["fw01"].Allowed)
	}
}

// ciscoWriteHasWritablePolicy is the arm precondition: a device with NO prefixes grants no write path.
func TestCiscoWriteHasWritablePolicy(t *testing.T) {
	none, _ := parseCiscoWriteDevices(`[{"device_id":"fw01","host":"h","identity":"i","key_ref":"env:K","known_hosts":"/kh"}]`)
	if ciscoWriteHasWritablePolicy(none) {
		t.Error("a device with no allowed_config_prefixes must not count as writable")
	}
	some, _ := parseCiscoWriteDevices(ciscoOneDevice)
	if !ciscoWriteHasWritablePolicy(some) {
		t.Error("a device WITH prefixes must count as writable")
	}
}

// The preflight must distinguish all four postures, and must NEVER report an armed posture as reachable —
// nothing routes to the Cisco write lane yet, and the boot log has to say so rather than imply capability.
func TestCiscoWritePreflightPostures(t *testing.T) {
	writable, _ := parseCiscoWriteDevices(ciscoOneDevice)
	noPrefix, _ := parseCiscoWriteDevices(`[{"device_id":"fw01","host":"h","identity":"i","key_ref":"env:K","known_hosts":"/kh"}]`)

	// 1. Unconfigured (the default posture).
	if s := ciscoWritePreflight(nil, false, true); !strings.HasPrefix(s, "DARK") || !strings.Contains(s, "no TG_CISCO_WRITE_DEVICES") {
		t.Errorf("unconfigured posture: %q", s)
	}
	// 2. Configured but not armed — declaring devices does not arm anything.
	if s := ciscoWritePreflight(writable, false, false); !strings.HasPrefix(s, "DARK") || !strings.Contains(s, "TG_CISCO_WRITE_ARM unset") {
		t.Errorf("configured-but-unarmed posture: %q", s)
	}
	// 3. Armed but no device permits a config line — refused config, not a silent no-op.
	if s := ciscoWritePreflight(noPrefix, false, true); !strings.HasPrefix(s, "DARK") || !strings.Contains(s, "may change nothing") {
		t.Errorf("armed-but-unwritable posture: %q", s)
	}
	// 4. Armed config: names the scope (devices + how many prefixes) and the mode, and states plainly that
	//    nothing routes here. An operator must be able to read the blast radius off this line.
	s := ciscoWritePreflight(writable, false, true)
	if !strings.Contains(s, "ARMED-CONFIG 1 device(s)") || !strings.Contains(s, "2 allowed config-line prefix(es)") {
		t.Fatalf("armed posture must name the scope: %q", s)
	}
	if !strings.Contains(s, "may_actuate=false") {
		t.Errorf("the posture must state the mode: %q", s)
	}
	// The arm-live slice (TG-85 item 4) made construction REAL, so the honest statement changed with it:
	// actuators exist on the cisco-interactive lane, reachability still needs a regime rule, and Shadow
	// refuses regardless. The posture must say all three — an operator reads dormancy's layers off this line.
	if !strings.Contains(s, "constructed on the cisco-interactive lane") || !strings.Contains(s, "regime rule selecting that lane") || !strings.Contains(s, "refuses at Shadow") {
		t.Errorf("the posture must name the constructed-but-unrouted-and-Shadow-gated state: %q", s)
	}
	if !strings.Contains(s, "0 reversible op(s)") {
		t.Errorf("the posture must count the reversible-op set (TG-85 item 5): %q", s)
	}
	// The mode is REPORTED, not assumed: with the mode escalated the same policy reads may_actuate=true.
	if s := ciscoWritePreflight(writable, true, true); !strings.Contains(s, "may_actuate=true") {
		t.Errorf("armed+actuating posture must report the live mode: %q", s)
	}
}
