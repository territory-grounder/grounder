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
	return []agent.Tool{deviceStatusTool{b}, eventlogTool{b}, activeAlertsTool{b}}
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

// ---- get-device-status ----
type deviceStatusTool struct{ b *toolBox }

func (deviceStatusTool) Name() string   { return "get-device-status" }
func (deviceStatusTool) ReadOnly() bool { return true }
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
