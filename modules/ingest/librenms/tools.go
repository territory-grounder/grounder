package librenms

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/territory-grounder/grounder/agent"
	"github.com/territory-grounder/grounder/core/config"
)

// getJSON is a read-only authenticated GET that decodes JSON into out. The response body is NEVER logged —
// a LibreNMS device body carries SNMP secrets, and the typed targets here declare only safe fields.
func getJSON(ctx context.Context, doer Doer, base, token, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Auth-Token", token)
	req.Header.Set("Accept", "application/json")
	resp, err := doer.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("GET %s: status %d", path, resp.StatusCode)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("malformed %s response: %w", path, err)
	}
	return nil
}

// The agent's READ-ONLY LibreNMS investigation tools. They ground triage in OBSERVED device state (status,
// event log, active alerts) instead of inference — the missing competence the eval surfaced (evidence_grounded
// floored at 1.0 because the agent had no tools). Every tool is GET-only (ReadOnly()=true; the ToolSet refuses
// a non-read-only tool), per-deployment, TLS via the injected client, token resolved from its ref at call time
// (INV-13). A device row carries SNMP secrets (community/authpass/cryptopass) — the toolDevice struct declares
// ONLY the safe fields, so json.Unmarshal drops the secrets and they can never reach the model. Response bodies
// are never logged. A lookup that fails returns ToolResult{Success:false} with a reason (the agent adapts) —
// never a Go error that aborts the session.

// toolDevice is the SAFE subset of a LibreNMS device row surfaced to the agent — never an SNMP credential.
type toolDevice struct {
	DeviceID   int    `json:"device_id"`
	Hostname   string `json:"hostname"`
	SysName    string `json:"sysName"`
	Status     int    `json:"status"` // 1 = up, 0 = down
	OS         string `json:"os"`
	Type       string `json:"type"`
	Hardware   string `json:"hardware"`
	Uptime     int64  `json:"uptime"`
	LastPolled string `json:"last_polled"`
	Disabled   int    `json:"disabled"`
}

// toolEvent is one /api/v0/logs/eventlog row (safe fields only).
type toolEvent struct {
	Datetime  string `json:"datetime"`
	Type      string `json:"type"`
	Message   string `json:"message"`
	Severity  int    `json:"severity"`
	Reference string `json:"reference"`
}

// toolBox is the shared LibreNMS read client the three tools hang off.
type toolBox struct {
	deployments []Deployment
	http        Doer
}

// NewTools returns the read-only LibreNMS investigation tools bound to the configured deployments + client.
// With no deployments it returns nil (the agent simply has no LibreNMS tools).
func NewTools(deployments []Deployment, doer Doer) []agent.Tool {
	if len(deployments) == 0 {
		return nil
	}
	if doer == nil {
		doer = http.DefaultClient
	}
	b := &toolBox{deployments: deployments, http: doer}
	return []agent.Tool{deviceStatusTool{b}, eventlogTool{b}, activeAlertsTool{b}, storageHealthTool{b}}
}

// getJSON is a read-only GET against a deployment; the response body is never logged (may carry secrets).
func (b *toolBox) getJSON(ctx context.Context, base, token, path string, out any) error {
	return getJSON(ctx, b.http, base, token, path, out)
}

// normHost normalizes a host for matching: lowercase, first whitespace/comment-free token, and — for
// name-like hosts only, never dotted IPs — the bare label with any DNS domain suffix stripped. So
// "dc1ap01.example.net" and "dc1ap01" both become "dc1ap01", while "192.168.181.1"
// is left intact (its first segment "192" has no letters, so the suffix strip is skipped).
func normHost(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	if i := strings.IndexAny(h, " \t#"); i >= 0 {
		h = strings.TrimSpace(h[:i])
	}
	if i := strings.Index(h, "."); i >= 0 && strings.ContainsAny(h[:i], "abcdefghijklmnopqrstuvwxyz") {
		h = h[:i]
	}
	return h
}

// resolveDevice finds the deployment that knows host and returns the deployment, its token, and the SAFE
// device row. It tries a direct /devices/{host} first (works when host is the LibreNMS hostname or a
// device_id), then falls back to listing /devices and matching on sysName or hostname — LibreNMS does NOT
// resolve a sysName via /devices/{id}, and the estate's named servers are keyed by sysName. ok=false with a
// reason if no deployment knows the host.
func (b *toolBox) resolveDevice(ctx context.Context, host string) (Deployment, string, toolDevice, bool, string) {
	host = strings.TrimSpace(host)
	if host == "" {
		return Deployment{}, "", toolDevice{}, false, "no host provided (pass args.host)"
	}
	want := normHost(host)
	var lastErr string
	for _, d := range b.deployments {
		token, err := config.SecretRef(d.TokenRef).Resolve()
		if err != nil {
			lastErr = "token unresolved for deployment " + d.Site
			continue
		}
		// Fast path: direct lookup (hostname or device_id). A miss returns 200+empty or a non-2xx — either
		// way we fall through to the authoritative list match below.
		var one struct {
			Devices []toolDevice `json:"devices"`
		}
		if err := b.getJSON(ctx, d.BaseURL, token, "/api/v0/devices/"+url.PathEscape(host), &one); err == nil && len(one.Devices) > 0 {
			return d, token, one.Devices[0], true, ""
		}
		// Fallback: list + match on sysName/hostname.
		var all struct {
			Devices []toolDevice `json:"devices"`
		}
		if err := b.getJSON(ctx, d.BaseURL, token, "/api/v0/devices", &all); err != nil {
			lastErr = err.Error()
			continue
		}
		for _, dev := range all.Devices {
			if normHost(dev.SysName) == want || normHost(dev.Hostname) == want {
				return d, token, dev, true, ""
			}
		}
		lastErr = "not present in deployment " + d.Site
	}
	if lastErr == "" {
		lastErr = "not found in any configured LibreNMS deployment"
	}
	return Deployment{}, "", toolDevice{}, false, "device " + host + ": " + lastErr
}

func statusWord(s, disabled int) string {
	if disabled == 1 {
		return "DISABLED"
	}
	if s == 1 {
		return "UP"
	}
	return "DOWN"
}

func hostArg(args map[string]string) string {
	for _, k := range []string{"host", "target", "device", "hostname"} {
		if v := strings.TrimSpace(args[k]); v != "" {
			return v
		}
	}
	return ""
}

// hostParam is the ACI schema (agent.ParamSpec) of the ONE argument all three LibreNMS tools take.
//
// ADOPTED IN TG-197: these tools shipped implementing Name/ReadOnly/Invoke only, so the catalog rendered a
// bare "- get-device-status" and the model had to GUESS the argument name — inside a 5-cycle poll budget
// against a 5.4-step live mean, a guessed key costs a cycle the investigation does not have. Declared once
// here rather than three times so the three tools cannot drift into three different spellings of the same
// argument, which is the ACI failure this closes. Pure prompt DATA plus a validation gate; no field here
// becomes control flow (INV-08) — the value is still matched against the LibreNMS inventory, never executed.
func hostParam(what string) []agent.ParamSpec {
	return []agent.ParamSpec{{
		Name: "host", Type: "host", Required: true, Example: "app01",
		// ParamSpec carries no Aliases field (the constraint the actor-evidence tool documents), so the
		// tolerated alternatives are named here. `host` is the key the SCHEMA requires: a call naming only an
		// alias is refused by the loop's arg screen with an actionable message rather than executed.
		Description: what + " — pass it under the key `host` (target/device/hostname are read as fallbacks). " +
			"A short name or an FQDN both resolve; the device must exist in a configured LibreNMS deployment",
	}}
}

// ---- get-device-status ----
type deviceStatusTool struct{ b *toolBox }

func (deviceStatusTool) Name() string   { return "get-device-status" }
func (deviceStatusTool) ReadOnly() bool { return true }
func (deviceStatusTool) Description() string {
	return "Read one device's CURRENT monitored state from LibreNMS: up/down/disabled, OS and hardware, how " +
		"long it has been up, and when it was last polled. Use it first to establish whether the device itself " +
		"is reachable — an alert on a DOWN device is a different investigation from one on a device that is up. " +
		"The uptime doubles as a reboot check: a small uptime on a device alerting for a service means it " +
		"restarted recently."
}
func (deviceStatusTool) Params() []agent.ParamSpec {
	return hostParam("the device whose monitored status to read")
}
func (t deviceStatusTool) Invoke(ctx context.Context, args map[string]string) (agent.ToolResult, error) {
	host := hostArg(args)
	res := agent.ToolResult{ID: "lnms-dev-" + normHost(host), Tool: t.Name()}
	_, _, dev, ok, why := t.b.resolveDevice(ctx, host)
	if !ok {
		res.Output = why
		return res, nil
	}
	up := time.Duration(dev.Uptime) * time.Second
	res.Success = true
	res.Output = fmt.Sprintf("LibreNMS device %s: status=%s os=%s type=%s hardware=%q uptime=%s last_polled=%s sysName=%s",
		dev.Hostname, statusWord(dev.Status, dev.Disabled), dev.OS, dev.Type, dev.Hardware, up.String(), dev.LastPolled, dev.SysName)
	return res, nil
}

// ---- get-device-eventlog ----
type eventlogTool struct{ b *toolBox }

func (eventlogTool) Name() string   { return "get-device-eventlog" }
func (eventlogTool) ReadOnly() bool { return true }
func (eventlogTool) Description() string {
	return "Read the most recent LibreNMS eventlog entries for a device (up to 20): what the poller OBSERVED " +
		"changing — interfaces going down, sensors crossing thresholds, the device rebooting. Use it to put a " +
		"time and a sequence on a fault the status call only shows the end state of. Observations from the " +
		"monitoring system, not a diagnosis: the log says what changed, never why."
}
func (eventlogTool) Params() []agent.ParamSpec {
	return hostParam("the device whose recent eventlog to read")
}
func (t eventlogTool) Invoke(ctx context.Context, args map[string]string) (agent.ToolResult, error) {
	host := hostArg(args)
	res := agent.ToolResult{ID: "lnms-events-" + normHost(host), Tool: t.Name()}
	d, token, dev, ok, why := t.b.resolveDevice(ctx, host)
	if !ok {
		res.Output = why
		return res, nil
	}
	// Query by the resolved device_id — LibreNMS /logs/eventlog does not resolve a sysName.
	var wrap struct {
		Logs []toolEvent `json:"logs"`
	}
	if err := t.b.getJSON(ctx, d.BaseURL, token, fmt.Sprintf("/api/v0/logs/eventlog/%d?limit=20", dev.DeviceID), &wrap); err != nil {
		res.Output = "eventlog fetch failed: " + err.Error()
		return res, nil
	}
	if len(wrap.Logs) == 0 {
		res.Success = true
		res.Output = "no recent eventlog entries for " + host
		return res, nil
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "LibreNMS eventlog for %s (most recent %d):", host, len(wrap.Logs))
	for _, e := range wrap.Logs {
		msg := strings.TrimSpace(e.Message)
		if msg == "" {
			msg = e.Type
		}
		fmt.Fprintf(&sb, "\n  [%s] %s: %s", e.Datetime, e.Type, msg)
	}
	res.Success = true
	res.Output = sb.String()
	return res, nil
}

// ---- get-active-alerts ----
type activeAlertsTool struct{ b *toolBox }

func (activeAlertsTool) Name() string   { return "get-active-alerts" }
func (activeAlertsTool) ReadOnly() bool { return true }
func (activeAlertsTool) Description() string {
	return "List the alerts CURRENTLY firing on a device in LibreNMS, with each rule's name and severity. Use " +
		"it to see whether the alert being triaged is alone or one of several on the same device, and — with " +
		"the neighbours get-estate-context names — to test a shared-cause theory by probing those hosts. An " +
		"empty list means nothing else is firing there right now; it is not proof the device is healthy."
}
func (activeAlertsTool) Params() []agent.ParamSpec {
	return hostParam("the device whose currently-firing alerts to list")
}
func (t activeAlertsTool) Invoke(ctx context.Context, args map[string]string) (agent.ToolResult, error) {
	host := hostArg(args)
	res := agent.ToolResult{ID: "lnms-alerts-" + normHost(host), Tool: t.Name()}
	d, token, dev, ok, why := t.b.resolveDevice(ctx, host)
	if !ok {
		res.Output = why
		return res, nil
	}
	// rule_id -> name+severity
	var rwrap struct {
		Rules []apiRule `json:"rules"`
	}
	rules := map[int]apiRule{}
	if err := t.b.getJSON(ctx, d.BaseURL, token, "/api/v0/rules", &rwrap); err == nil {
		for _, r := range rwrap.Rules {
			rules[r.ID] = r
		}
	}
	var awrap struct {
		Alerts []apiAlert `json:"alerts"`
	}
	if err := t.b.getJSON(ctx, d.BaseURL, token, "/api/v0/alerts?state=1", &awrap); err != nil {
		res.Output = "alerts fetch failed: " + err.Error()
		return res, nil
	}
	var firing []string
	for _, a := range awrap.Alerts {
		if a.DeviceID != dev.DeviceID {
			continue
		}
		name := rules[a.RuleID].Name
		if name == "" {
			name = fmt.Sprintf("rule-%d", a.RuleID)
		}
		sev := rules[a.RuleID].Severity
		firing = append(firing, fmt.Sprintf("%s (severity=%s, since=%s)", name, sev, a.Timestamp))
	}
	sort.Strings(firing)
	res.Success = true
	if len(firing) == 0 {
		res.Output = "no active LibreNMS alerts firing on " + host + " (device status=" + statusWord(dev.Status, dev.Disabled) + ")"
		return res, nil
	}
	res.Output = fmt.Sprintf("active LibreNMS alerts on %s: %s", host, strings.Join(firing, "; "))
	return res, nil
}

// LiveIncidentState reports the live-estate facts a corpus-freshness check needs for one host: whether the
// device resolves anywhere, whether LibreNMS has it administratively disabled, and how many alerts are
// actively firing on it right now. Added for the eval harness (Phase B4, 2026-07-30): a corpus captured
// against a past estate drifts — devices get disabled, faults heal — and an expected-propose incident whose
// live evidence contradicts it must be EXCLUDED from proposal-recall rather than scored as an agent miss
// (standing down on a stale incident is correct behavior; the 07-25 "collapse to 0% proposals" was mostly
// this). Read-only; errors surface to the caller so freshness can fail loud rather than guess.
func LiveIncidentState(ctx context.Context, deployments []Deployment, doer Doer, host string) (found, disabled bool, activeAlerts int, err error) {
	if doer == nil {
		doer = http.DefaultClient
	}
	b := &toolBox{deployments: deployments, http: doer}
	d, token, dev, ok, why := b.resolveDevice(ctx, host)
	if !ok {
		if strings.Contains(why, "fetch") || strings.Contains(why, "token unresolved") {
			return false, false, 0, fmt.Errorf("librenms live state for %s: %s", host, why)
		}
		return false, false, 0, nil
	}
	var awrap struct {
		Alerts []apiAlert `json:"alerts"`
	}
	if aerr := b.getJSON(ctx, d.BaseURL, token, "/api/v0/alerts?state=1", &awrap); aerr != nil {
		return true, dev.Disabled != 0, 0, fmt.Errorf("librenms alerts for %s: %w", host, aerr)
	}
	for _, a := range awrap.Alerts {
		if a.DeviceID == dev.DeviceID {
			activeAlerts++
		}
	}
	return true, dev.Disabled != 0, activeAlerts, nil
}

// ---- get-device-storage-health ----
//
// The storage pack's read connector (TG-78 storage slice): the per-volume capacity table and the
// RAID/volume/disk state sensors LibreNMS already polls over SNMP — the vendor layer the
// storage-alert-triage doctrine needs (trajectory-not-threshold, name-the-failed-member) and which the
// storage-appliance lane was tool-gated on. Same deployment, same token, same resolveDevice/getJSON
// discipline as the three shipped tools: no new secret, no new host key, no new boot knob. Two bounded
// request fans (the class listings return ids only, values live per-sensor): every storage sensor up to
// storageHealthMaxVolumes, and ONLY the storage-relevant state sensors (Disk/Volume/Storage Pool/RAID by
// descr) up to storageHealthMaxSensors — a NAS-sized cap, never the whole sensor estate.
type storageHealthTool struct{ b *toolBox }

const (
	storageHealthMaxVolumes = 12
	storageHealthMaxSensors = 16
)

func (storageHealthTool) Name() string   { return "get-device-storage-health" }
func (storageHealthTool) ReadOnly() bool { return true }
func (storageHealthTool) Description() string {
	return "Read a device's storage health from LibreNMS: every polled volume's capacity (size/used/percent " +
		"against its own warn threshold) and the RAID/volume/disk STATE sensors (on Synology MIBs a state " +
		"sensor reads 1 when normal — anything else is the fault detail to quote). Use it on a storage " +
		"appliance or any SNMP device whose alert is about disks, volumes, pools, or space: it shows WHICH " +
		"member is unhealthy and how full each volume actually is, where the alert only says that something is."
}
func (storageHealthTool) Params() []agent.ParamSpec {
	return hostParam("the device whose storage health to read")
}

// storageHealthSensorRelevant keeps the state-sensor fan bounded to the storage plane by descr — the same
// vocabulary the Synology MIBs expose (Disk N, Volume N, Storage Pool N, plus explicit RAID sensors).
func storageHealthSensorRelevant(descr string) bool {
	d := strings.ToLower(strings.TrimSpace(descr))
	return strings.HasPrefix(d, "disk") || strings.HasPrefix(d, "volume") ||
		strings.HasPrefix(d, "storage pool") || strings.Contains(d, "raid")
}

func (t storageHealthTool) Invoke(ctx context.Context, args map[string]string) (agent.ToolResult, error) {
	host := hostArg(args)
	res := agent.ToolResult{ID: "lnms-storage-" + normHost(host), Tool: t.Name()}
	d, token, dev, ok, why := t.b.resolveDevice(ctx, host)
	if !ok {
		res.Output = why
		return res, nil
	}
	type sensorRef struct {
		SensorID int    `json:"sensor_id"`
		Desc     string `json:"desc"`
	}
	var list struct {
		Graphs []sensorRef `json:"graphs"`
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "LibreNMS storage health for %s:", host)

	// Volumes: list then per-sensor detail (the listing carries ids only).
	if err := t.b.getJSON(ctx, d.BaseURL, token, fmt.Sprintf("/api/v0/devices/%d/health/storage", dev.DeviceID), &list); err != nil {
		res.Output = "storage listing fetch failed: " + err.Error()
		return res, nil
	}
	vols := list.Graphs
	if len(vols) > storageHealthMaxVolumes {
		fmt.Fprintf(&sb, "\n  (showing the first %d of %d volumes)", storageHealthMaxVolumes, len(vols))
		vols = vols[:storageHealthMaxVolumes]
	}
	volumes := 0
	for _, v := range vols {
		var det struct {
			Graphs []struct {
				Descr    string `json:"storage_descr"`
				Size     int64  `json:"storage_size"`
				Used     int64  `json:"storage_used"`
				Perc     int    `json:"storage_perc"`
				PercWarn int    `json:"storage_perc_warn"`
			} `json:"graphs"`
		}
		if err := t.b.getJSON(ctx, d.BaseURL, token, fmt.Sprintf("/api/v0/devices/%d/health/storage/%d", dev.DeviceID, v.SensorID), &det); err != nil || len(det.Graphs) == 0 {
			continue // one unreadable volume is a gap in the table, not a failed read of the device
		}
		g := det.Graphs[0]
		fmt.Fprintf(&sb, "\n  volume %s: %d%% used (%.1f/%.1f GB, warn at %d%%)",
			g.Descr, g.Perc, float64(g.Used)/1e9, float64(g.Size)/1e9, g.PercWarn)
		volumes++
	}
	if volumes == 0 {
		fmt.Fprintf(&sb, "\n  no polled volumes") // the check reports its empty case, never silence
	}

	// State sensors: list, filter to the storage plane by descr, then per-sensor detail.
	list.Graphs = nil
	if err := t.b.getJSON(ctx, d.BaseURL, token, fmt.Sprintf("/api/v0/devices/%d/health/state", dev.DeviceID), &list); err != nil {
		fmt.Fprintf(&sb, "\n  state sensors unreadable: %s", err.Error())
		res.Output = sb.String()
		return res, nil
	}
	sensors := 0
	for _, s := range list.Graphs {
		if !storageHealthSensorRelevant(s.Desc) {
			continue
		}
		if sensors >= storageHealthMaxSensors {
			fmt.Fprintf(&sb, "\n  (more storage state sensors not shown)")
			break
		}
		var det struct {
			Graphs []struct {
				Descr   string  `json:"sensor_descr"`
				Type    string  `json:"sensor_type"`
				Current float64 `json:"sensor_current"`
			} `json:"graphs"`
		}
		if err := t.b.getJSON(ctx, d.BaseURL, token, fmt.Sprintf("/api/v0/devices/%d/health/state/%d", dev.DeviceID, s.SensorID), &det); err != nil || len(det.Graphs) == 0 {
			continue
		}
		g := det.Graphs[0]
		fmt.Fprintf(&sb, "\n  state %s (%s): %g", g.Descr, g.Type, g.Current)
		sensors++
	}
	if sensors == 0 {
		fmt.Fprintf(&sb, "\n  no storage state sensors polled")
	}
	res.Success = true
	res.Output = sb.String()
	return res, nil
}
