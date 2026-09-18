package main

// TG-85 read-tool slice: the OPERATOR CONFIG SURFACE for the cisco-show agent tool. Mirrors the write
// lane's shape (worker_cisco_write.go) with the read lane's much smaller trust footprint: a fail-closed
// JSON device list (TG_CISCO_READ_DEVICES) parsed at boot; each declared device becomes an enum member of
// ONE read-only tool (`cisco-show`, modules/actuation/cisco/showtool.go) over the CLOSED show-command
// catalog. No arm switch exists on purpose — the tool is read-only by construction (RunShow only; the
// credential-bearing show family refused BY NAME even if catalogued), so declaration IS the decision, the
// same trust shape as hostdiag's deployments.
//
// DARK by default: TG_CISCO_READ_DEVICES unset ⇒ nothing parses, nothing registers, the model's preamble
// is byte-identical to before this slice (registering a tool is the eval-visible act — the eval arms set
// no cisco env, so the change gate's arms are unchanged by construction; the first CONFIGURED use on the
// box is the arming act and is boot-log-readable below).

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/territory-grounder/grounder/agent"
	"github.com/territory-grounder/grounder/core/config"
	cisco "github.com/territory-grounder/grounder/modules/actuation/cisco"
)

// ciscoReadDeviceSpec is one operator-declared read-only diagnostic target. The credential is a
// SecretRef resolved in memory at use (INV-13); no key material is ever named here.
type ciscoReadDeviceSpec struct {
	DeviceID     string `json:"device_id"`
	Host         string `json:"host"`
	Port         string `json:"port"`
	Platform     string `json:"platform"` // "asa" | "ios" | "any"
	Identity     string `json:"identity"`
	KeyRef       string `json:"key_ref"`
	KnownHosts   string `json:"known_hosts"`
	LegacyCrypto bool   `json:"legacy_crypto"`
	PagerOffCmd  string `json:"pager_off_cmd"`
}

// parseCiscoReadDevices parses TG_CISCO_READ_DEVICES (a JSON array of ciscoReadDeviceSpec). Fail-closed:
// any malformed entry refuses the WHOLE list with the index named — a half-parsed device set would make
// the tool's enum silently narrower than the operator declared (the covers-less-than-it-claims shape).
func parseCiscoReadDevices(raw string) ([]cisco.ReadDevice, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var specs []ciscoReadDeviceSpec
	if err := json.Unmarshal([]byte(raw), &specs); err != nil {
		return nil, fmt.Errorf("TG_CISCO_READ_DEVICES is not a JSON array of device specs: %w", err)
	}
	out := make([]cisco.ReadDevice, 0, len(specs))
	for i, s := range specs {
		if s.DeviceID == "" || s.Host == "" || s.Identity == "" || s.KeyRef == "" || s.KnownHosts == "" {
			return nil, fmt.Errorf("TG_CISCO_READ_DEVICES[%d]: device_id, host, identity, key_ref and known_hosts are all required (host-key pinning is not optional)", i)
		}
		var plat cisco.Platform
		switch strings.ToLower(strings.TrimSpace(s.Platform)) {
		case "asa":
			plat = cisco.PlatformASA
		case "ios":
			plat = cisco.PlatformIOS
		case "", "any":
			plat = cisco.PlatformAny
		default:
			return nil, fmt.Errorf("TG_CISCO_READ_DEVICES[%d]: unknown platform %q (asa|ios|any)", i, s.Platform)
		}
		out = append(out, cisco.ReadDevice{
			ID: s.DeviceID,
			Dev: cisco.Device{
				Host: s.Host, Port: s.Port, Identity: s.Identity,
				KeyRef:       config.SecretRef(s.KeyRef),
				KnownHosts:   s.KnownHosts,
				LegacyCrypto: s.LegacyCrypto,
				PagerOffCmd:  s.PagerOffCmd,
			},
			Platform: plat,
		})
	}
	return out, nil
}

// wireCiscoReadTool parses the declaration and, when non-empty, builds the cisco-show tool. The caller
// registers it and logs; a parse or build error is FATAL at boot (a declared-but-unusable device set must
// stop the worker, not silently ship a narrower tool — the same posture as the hostdiag credential
// engine). Returns (nil, "") when nothing is declared — the dark default.
func wireCiscoReadTool(getenv func(string, string) string) (agent.Tool, int, error) {
	raw := getenv("TG_CISCO_READ_DEVICES", "")
	devs, err := parseCiscoReadDevices(raw)
	if err != nil {
		return nil, 0, err
	}
	if len(devs) == 0 {
		return nil, 0, nil
	}
	tl, err := cisco.NewShowTool(devs, 45*time.Second, nil)
	if err != nil {
		return nil, 0, err
	}
	return tl, len(devs), nil
}
