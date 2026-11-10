package main

// TG-85 write slice 4: the OPERATOR CONFIG SURFACE + dormant arm for the Cisco write lane.
//
// Slices 1-3 built the write lane itself (a distinct gated WriteModule, the config-mode PTY transport, and the
// commit-confirmed reversibility primitive) and shipped it DARK — nothing constructed it, so no operator could
// state which devices it may touch or what it may change on them. This is that surface: a fail-closed JSON
// device policy (TG_CISCO_WRITE_DEVICES) plus the arm switch (TG_CISCO_WRITE_ARM), parsed at boot and reported
// in the boot log so the lane's posture is READABLE rather than inferred.
//
// It stays DARK by default and by construction:
//   - No devices declared (the default) ⇒ nothing is built at all.
//   - Devices declared but TG_CISCO_WRITE_ARM unset ⇒ the policy is parsed and REPORTED, nothing constructed.
//   - Armed ⇒ a WriteModule per device, each carrying the mode chokepoint and that device's closed config-line
//     prefix allowlist. Even then it can only refuse at Shadow (its ReadOnly()/gate re-check), and it is not
//     bound to any regime lane — the routing binding lands with the arm-live slice, once the lane decision is
//     made. Nothing reaches these modules today.
//
// A device declared WITHOUT config-line prefixes has no write path by construction (WriteModule.ReadOnly()
// stays true) — declaring a device is not the same as permitting a change on it.

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/territory-grounder/grounder/core/config"
)

// ciscoWriteDeviceSpec is one operator-declared Cisco write target: the connection profile plus the CLOSED set
// of config-line prefixes TG may emit on it. The credential is a SecretRef resolved in memory at use (INV-13);
// no key material is ever named here.
type ciscoWriteDeviceSpec struct {
	DeviceID              string   `json:"device_id"`
	Host                  string   `json:"host"`
	Port                  string   `json:"port"`
	Identity              string   `json:"identity"`
	KeyRef                string   `json:"key_ref"`
	KnownHosts            string   `json:"known_hosts"`
	LegacyCrypto          bool     `json:"legacy_crypto"`
	PagerOffCmd           string   `json:"pager_off_cmd"`
	Prompt                string   `json:"prompt"`
	AllowedConfigPrefixes []string `json:"allowed_config_prefixes"`
	// Platform names the device's CLI dialect ("asa" | "ios") — REQUIRED when reversible_ops are declared,
	// because the dialect is part of each op's identity (an IOS registry must never yield ASA's verb).
	Platform string `json:"platform"`
	// ReversibleOps is the operator-declared NAMED-change set for THIS device (TG-85 item 5): each op
	// carries its own undo, declared before the forward may ever run. The mechanism (four registration
	// rules, floor clamp, dialect identity) is modules/actuation/cisco's ReversibleRegistry; this field is
	// only the config-not-code surface feeding it. Empty = the op path stays refused (the default).
	ReversibleOps []ciscoReversibleOpSpec `json:"reversible_ops"`
}

// ciscoReversibleOpSpec is one operator-declared named change: forward lines + the declared inverse. The
// registry validates the four registration rules at construction; parse only shapes it.
type ciscoReversibleOpSpec struct {
	Name    string   `json:"name"`
	OpClass string   `json:"op_class"`
	Forward []string `json:"forward"`
	Inverse []string `json:"inverse"`
	Why     string   `json:"why"`
}

// ciscoWriteDevice is the parsed connection profile, held in NEUTRAL form (not yet a cisco.Device). The
// conversion — and the actuator construction — belong to the arm-live routing slice; keeping cmd/worker free of
// the cisco import until then keeps the still-unrouted package out of the dead-code analysis scope, so its dark
// surface is not grandfathered into the baseline for a config surface that constructs nothing.
type ciscoWriteDevice struct {
	Host         string
	Port         string
	Identity     string
	KeyRef       config.SecretRef // resolved in memory at use (INV-13)
	KnownHosts   string
	LegacyCrypto bool
	PagerOffCmd  string
	Prompt       *regexp.Regexp // nil ⇒ the transport default
}

// ciscoWritePolicy is one parsed device policy: the transport profile and its config-line allowlist.
type ciscoWritePolicy struct {
	Device  ciscoWriteDevice
	Allowed []string
	// PlatformName is the declared CLI dialect in NEUTRAL form ("asa"/"ios", validated non-empty exactly
	// when Ops are declared); the cisco-typed conversion lives in worker_cisco_lane.go with the rest of
	// the arm-live construction.
	PlatformName string
	Ops          []ciscoReversibleOpSpec
}

// parseCiscoWriteDevices parses TG_CISCO_WRITE_DEVICES (a JSON array of ciscoWriteDeviceSpec) into per-device
// policies. Empty ⇒ no devices (the lane cannot be armed). A malformed entry FAILS CLOSED: a device policy
// missing its host, login identity, credential reference, or host-key pinning could not be connected to
// safely, and a bad prompt pattern could mis-anchor the expect loop mid-config — neither may be guessed past.
func parseCiscoWriteDevices(spec string) (map[string]ciscoWritePolicy, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, nil
	}
	var raw []ciscoWriteDeviceSpec
	if err := json.Unmarshal([]byte(spec), &raw); err != nil {
		return nil, fmt.Errorf("TG_CISCO_WRITE_DEVICES is not a JSON array of {device_id,host,identity,key_ref,known_hosts,allowed_config_prefixes}: %w", err)
	}
	out := make(map[string]ciscoWritePolicy, len(raw))
	for i, d := range raw {
		id := strings.TrimSpace(d.DeviceID)
		if id == "" {
			return nil, fmt.Errorf("TG_CISCO_WRITE_DEVICES[%d]: device_id is required", i)
		}
		if _, dup := out[id]; dup {
			return nil, fmt.Errorf("TG_CISCO_WRITE_DEVICES[%d]: duplicate device_id %q", i, id)
		}
		host := strings.TrimSpace(d.Host)
		ident := strings.TrimSpace(d.Identity)
		keyRef := strings.TrimSpace(d.KeyRef)
		known := strings.TrimSpace(d.KnownHosts)
		if host == "" || ident == "" || keyRef == "" || known == "" {
			return nil, fmt.Errorf("TG_CISCO_WRITE_DEVICES[%d] (%s): host, identity, key_ref and known_hosts are all required (an unpinned or unauthenticated device connection is refused)", i, id)
		}
		dev := ciscoWriteDevice{
			Host:         host,
			Port:         strings.TrimSpace(d.Port),
			Identity:     ident,
			KeyRef:       config.SecretRef(keyRef),
			KnownHosts:   known,
			LegacyCrypto: d.LegacyCrypto,
			PagerOffCmd:  strings.TrimSpace(d.PagerOffCmd),
		}
		if p := strings.TrimSpace(d.Prompt); p != "" {
			re, err := regexp.Compile(p)
			if err != nil {
				return nil, fmt.Errorf("TG_CISCO_WRITE_DEVICES[%d] (%s): prompt %q is not a valid regexp: %w", i, id, p, err)
			}
			dev.Prompt = re
		}
		// Normalize the prefix allowlist here too (blank entries dropped): NewWriteModule freezes it the same
		// way, but a policy that LOOKS like it permits something must not report a write path it does not have.
		var allowed []string
		for _, p := range d.AllowedConfigPrefixes {
			if strings.TrimSpace(p) != "" {
				allowed = append(allowed, p)
			}
		}
		plat := strings.ToLower(strings.TrimSpace(d.Platform))
		if plat != "" && plat != "asa" && plat != "ios" {
			return nil, fmt.Errorf("TG_CISCO_WRITE_DEVICES[%d] (%s): unknown platform %q (want asa or ios; the dialect is part of a write's identity)", i, id, d.Platform)
		}
		if len(d.ReversibleOps) > 0 && plat == "" {
			return nil, fmt.Errorf("TG_CISCO_WRITE_DEVICES[%d] (%s): reversible_ops require an explicit platform — the dialect is part of each op's identity (an IOS registry must never yield ASA's verb)", i, id)
		}
		for j, op := range d.ReversibleOps {
			if strings.TrimSpace(op.Name) == "" || strings.TrimSpace(op.OpClass) == "" || len(op.Forward) == 0 || len(op.Inverse) == 0 {
				return nil, fmt.Errorf("TG_CISCO_WRITE_DEVICES[%d] (%s) reversible_ops[%d]: name, op_class, forward and inverse are all required — an op without its declared undo cannot register", i, id, j)
			}
		}
		out[id] = ciscoWritePolicy{Device: dev, Allowed: allowed, PlatformName: plat, Ops: append([]ciscoReversibleOpSpec(nil), d.ReversibleOps...)}
	}
	return out, nil
}

// ciscoWriteHasWritablePolicy reports whether ANY declared device carries a write path — config-line
// prefixes OR named reversible ops — the precondition for arming (arming a lane that may change nothing
// anywhere is refused config, not a silent no-op). Mirrors gitopsHasFieldRules. The Ops term is
// load-bearing (review 2026-08-25): this predicate predated reversible_ops and counted only prefixes, so
// an ops-ONLY device — the narrowest, safest declared shape, the one the reversible registry's own header
// recommends — could never arm: the arming gate and the construction gate (buildCiscoWriteLeaves, which
// happily builds for ops-only) quietly disagreed, and no fixture exercised the shape.
func ciscoWriteHasWritablePolicy(policies map[string]ciscoWritePolicy) bool {
	for _, p := range policies {
		if len(p.Allowed) > 0 || len(p.Ops) > 0 {
			return true
		}
	}
	return false
}

// ciscoWritePreflight reports the Cisco write lane's boot POSTURE from the parsed policy. It constructs no
// actuator: no regime lane routes to the Cisco write lane yet, so building live actuators nothing can reach
// would claim a capability the worker does not have — construction lands with the arm-live routing slice.
//
// What the preflight is FOR (a check that can report "nothing to check"): it names, on every boot, which of the
// four states the lane is in — unconfigured, configured-but-unarmed, armed-but-unwritable (declared devices
// that permit no config line), or armed-config — together with the mode. A posture that can only be inferred
// from env vars is a posture nobody verifies. Even ARMED-CONFIG is inert: the mode chokepoint governs every
// future actuator, so at Shadow the lane could only refuse.
func ciscoWritePreflight(policies map[string]ciscoWritePolicy, mayActuate bool, armEnabled bool) string {
	if len(policies) == 0 {
		return "DARK (no TG_CISCO_WRITE_DEVICES declared — the write lane is not configured)"
	}
	writable := ciscoWriteHasWritablePolicy(policies)
	if !armEnabled {
		return fmt.Sprintf("DARK (%d device(s) declared, config-line prefixes present=%v; TG_CISCO_WRITE_ARM unset)", len(policies), writable)
	}
	if !writable {
		return fmt.Sprintf("DARK (TG_CISCO_WRITE_ARM set but none of the %d declared device(s) carries allowed_config_prefixes or reversible_ops — refusing to arm a lane that may change nothing)", len(policies))
	}
	prefixes := 0
	for _, p := range policies {
		prefixes += len(p.Allowed)
	}
	ops := 0
	for _, p := range policies {
		ops += len(p.Ops)
	}
	return fmt.Sprintf("ARMED-CONFIG %d device(s) / %d allowed config-line prefix(es) / %d reversible op(s) (mode may_actuate=%v) — per-device actuators constructed on the cisco-interactive lane; reachable only through a regime rule selecting that lane, and every leaf still refuses at Shadow",
		len(policies), prefixes, ops, mayActuate)
}
