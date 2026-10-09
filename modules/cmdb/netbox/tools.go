package netbox

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/territory-grounder/grounder/agent"
)

// The agent's READ-ONLY NetBox investigation tool (TG-56). It grounds triage in the authoritative CMDB
// record for a host — its lifecycle status, site, role, model, and primary IP — instead of inference: the
// source-of-truth inventory says what a host IS, which frames whether an outage is load-bearing and whether
// a 'decommissioning'/'offline' lifecycle explains an alert that needs no remediation. This is TG-56's
// "consume a read-only vendor server to broaden investigation reach" re-authored for TG's architecture: the
// ReAct investigation surface is agent.Tool (the MCP actuation chokepoint is a separate, mutation-only lane),
// so the read-only vendor consumer is an agent.Tool exactly like the LibreNMS investigation tools.
//
// It reuses the CMDB Module's authenticated GET (do): the token is resolved from its secret reference at
// call time (INV-13), never a literal; the response is decoded into a SAFE typed struct (deviceRecord), so
// an opaque or credential custom-field a NetBox record may carry is dropped by json.Unmarshal and can never
// reach the model. GET-only (ReadOnly()=true; the ToolSet refuses a non-read-only tool, INV-08). A lookup
// that fails returns ToolResult{Success:false} with a reason the agent adapts to — never a Go error that
// aborts the session. DORMANT until an operator arms it (TG_NETBOX_INVESTIGATION) — a deliberate,
// transparency-gated act, exactly like the NetBox actor-evidence reader.

// deviceRecord is the SAFE investigation view of a NetBox device — declared narrow so json.Unmarshal drops
// every field a record carries that is not named here (a custom field, an API-token echo).
type deviceRecord struct {
	Name   string `json:"name"`
	Status struct {
		Value string `json:"value"`
		Label string `json:"label"`
	} `json:"status"`
	Site       struct{ Name string }    `json:"site"`
	Role       struct{ Name string }    `json:"role"`
	DeviceType struct{ Model string }   `json:"device_type"`
	PrimaryIP  struct{ Address string } `json:"primary_ip"`
}

// LookupDevice returns NetBox's authoritative record for a device by name (read-only). found=false when the
// inventory has no such device; err only on a transport/auth/decoding failure. It reuses the CMDB Module's
// do() so auth and per-request token resolution stay in one place.
func (m *Module) LookupDevice(ctx context.Context, name string) (deviceRecord, bool, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return deviceRecord{}, false, fmt.Errorf("netbox: empty device name")
	}
	body, err := m.do(ctx, "/api/dcim/devices/?name="+url.QueryEscape(name))
	if err != nil {
		return deviceRecord{}, false, err
	}
	var page struct {
		Results []deviceRecord `json:"results"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		return deviceRecord{}, false, fmt.Errorf("netbox: malformed device response: %w", err)
	}
	if len(page.Results) == 0 {
		return deviceRecord{}, false, nil
	}
	return page.Results[0], true, nil
}

// NewTools returns the read-only NetBox investigation tools bound to the CMDB module. A nil module yields no
// tools (the agent simply has no NetBox tool) — the config gate lives at the wiring site (cmd/worker).
func NewTools(m *Module) []agent.Tool {
	if m == nil {
		return nil
	}
	return []agent.Tool{inventoryDeviceTool{m}}
}

// ---- get-inventory-device ----

type inventoryDeviceTool struct{ m *Module }

func (inventoryDeviceTool) Name() string   { return "get-inventory-device" }
func (inventoryDeviceTool) ReadOnly() bool { return true }
func (inventoryDeviceTool) Description() string {
	return "Read one host's authoritative record from the NetBox inventory (the CMDB source-of-truth): its " +
		"lifecycle status, site, role, model, and primary IP. Use it to ground an alert in what the inventory " +
		"says the host IS — its role and site frame whether an outage is load-bearing, and a lifecycle status " +
		"of 'decommissioning' or 'offline' can explain an alert that needs no remediation. The record is what " +
		"the inventory DECLARES, not live monitored state (use the LibreNMS tools for current up/down)."
}
func (inventoryDeviceTool) Params() []agent.ParamSpec {
	return []agent.ParamSpec{{
		Name: "host", Type: "host", Required: true, Example: "app01",
		Description: "the host to look up in the NetBox inventory — pass it under the key `host` " +
			"(target/device/hostname are read as fallbacks); the value matches the NetBox device name",
	}}
}
func (t inventoryDeviceTool) Invoke(ctx context.Context, args map[string]string) (agent.ToolResult, error) {
	host := toolHostArg(args)
	res := agent.ToolResult{ID: "netbox-dev-" + strings.ToLower(host), Tool: t.Name()}
	if host == "" {
		res.Output = "no host provided (pass args.host)"
		return res, nil
	}
	d, found, err := t.m.LookupDevice(ctx, host)
	if err != nil {
		res.Output = "netbox inventory lookup failed: " + err.Error()
		return res, nil
	}
	if !found {
		res.Output = fmt.Sprintf("no NetBox inventory record for %q", host)
		return res, nil
	}
	res.Success = true
	res.Output = fmt.Sprintf("NetBox inventory %s: status=%s site=%s role=%s model=%s primary_ip=%s",
		d.Name, deviceStatus(d), d.Site.Name, d.Role.Name, d.DeviceType.Model, d.PrimaryIP.Address)
	return res, nil
}

// deviceStatus prefers NetBox's human label ("Active") over the raw value ("active").
func deviceStatus(d deviceRecord) string {
	if d.Status.Label != "" {
		return d.Status.Label
	}
	if d.Status.Value != "" {
		return d.Status.Value
	}
	return "unknown"
}

// toolHostArg reads the host under the schema key `host`, tolerating the common aliases the model may emit.
func toolHostArg(args map[string]string) string {
	for _, k := range []string{"host", "target", "device", "hostname"} {
		if v := strings.TrimSpace(args[k]); v != "" {
			return v
		}
	}
	return ""
}
