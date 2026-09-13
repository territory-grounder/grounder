// fixtures.go is the B4a FIXTURE ARM of the eval corpus: deterministic, captured tool service for the
// corpus's expected-propose incidents.
//
// Motivating failure (the 2026-07-30 trend record, eval/history/2026-07-30-trend-e22fc14b7ac5): every
// expected-propose incident in corpus.json had gone stale vs the LIVE estate — the down devices were
// re-enabled or healed after capture — so the freshness pass (correctly, honestly) excluded all of them,
// proposal capability was UNMEASURED, falsifiable_prediction floored at AbstentionFloor (1.00) every run,
// and the trend baseline could never refresh. That is not a one-off: a live-armed propose corpus DECAYS BY
// NATURE, because a healthy estate heals its faults. The fixture arm removes the decay channel: each
// expected-propose incident carries the CAPTURED outputs of the real investigation tools, served verbatim,
// so the incident is stale-proof by construction — the captured world IS the world — and the propose supply
// (and with it falsifiable_prediction) stays measurable forever. Stand-down/escalate incidents are NOT
// fixture-armed on purpose: their correctness IS live-groundedness (standing down because the live estate
// really is calm), so they keep the live tools.
//
// Shape faithfulness is the load-bearing property. A fixture must speak EXACTLY the dialect the real tools
// emit (modules/ingest/librenms/tools.go, modules/observability/hostdiag/hostdiag.go,
// modules/observability/syslogng/tools.go) — same headers, same section markers, same derived-summary
// phrasing, same "no data" refusals — or the eval measures an agent reading a language production never
// speaks. fixtures_test.go pins this by rendering the REAL hostdiag check-host-services output (fake SSH
// runner, real formatting path) and requiring the corpus fixture to match it byte-for-byte.
package eval

import (
	"context"
	"fmt"
	"strings"

	"github.com/territory-grounder/grounder/agent"
	"github.com/territory-grounder/grounder/core/estate"
	estatetools "github.com/territory-grounder/grounder/modules/estate"
)

// FixtureResult is one captured tool observation, served VERBATIM to the agent when it invokes the keyed
// (tool, host) pair on a fixture-armed incident. Success mirrors the real ToolResult.Success the capture
// had (an unreachable-host hostdiag read is a real, Success=false observation the agent must reason from).
type FixtureResult struct {
	Success bool   `json:"success"`
	Output  string `json:"output"`
}

// fixtureServedTools is the CLOSED set of network-backed production tool names the fixture arm serves —
// the same names evalTools registers live (the LibreNMS, hostdiag and syslog-ng read tools). The offline
// tools (get-device-context, get-estate-context) stay REAL in both arms: they are pure in-memory reads and
// never dial out. fixtures_test.go pins this list against the real constructors, so a production tool
// rename cannot silently drift past the fixture arm (the renamed tool would go live and dial out).
var fixtureServedTools = []string{
	// modules/ingest/librenms.NewTools
	"get-device-status", "get-device-eventlog", "get-active-alerts", "get-device-storage-health",
	// modules/observability/hostdiag.NewTools
	"check-host-disk", "check-host-memory", "check-host-services", "check-host-load",
	// modules/observability/syslogng.NewTools
	"get-host-logs", "search-host-logs",
}

// fixtureServable reports whether tool is in the served set (LoadCorpus fails closed on any other name —
// a typo'd fixture tool would otherwise silently never match and the arm would degrade to miss shapes).
func fixtureServable(tool string) bool {
	for _, t := range fixtureServedTools {
		if t == tool {
			return true
		}
	}
	return false
}

// normalizeFixtureHost normalizes a host for fixture matching exactly the way the real tools do (the
// librenms/syslogng normHost rule): lowercase, first whitespace/comment-free token, and — for name-like
// hosts only, never dotted IPs — the bare label with any DNS domain suffix stripped. So a model that calls
// with "dc1atlantis01.example.net" still hits the "dc1atlantis01" fixture.
func normalizeFixtureHost(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	if i := strings.IndexAny(h, " \t#"); i >= 0 {
		h = strings.TrimSpace(h[:i])
	}
	if i := strings.Index(h, "."); i >= 0 && strings.ContainsAny(h[:i], "abcdefghijklmnopqrstuvwxyz") {
		h = h[:i]
	}
	return h
}

// FixtureKey is the ToolFixtures map key for one captured (tool, host) observation: "tool|normalized-host".
// LoadCorpus requires every stored key to already be in this normalized form, so an un-normalized authored
// key cannot silently never match.
func FixtureKey(tool, host string) string {
	return tool + "|" + normalizeFixtureHost(host)
}

// FixtureArmed reports whether this incident is served from captured fixtures (the deterministic B4a arm).
func (inc Incident) FixtureArmed() bool { return len(inc.ToolFixtures) > 0 }

// NeedsFreshnessCheck reports whether the live corpus-freshness pass must verify this incident against the
// estate. Only LIVE-armed expected-propose incidents qualify: a fixture-armed incident is stale-proof by
// construction (its evidence is the captured world, which cannot drift), and non-propose incidents were
// never freshness-checked (a stand-down label has nothing to go stale against).
func NeedsFreshnessCheck(inc Incident) bool {
	return inc.Expected == "propose" && !inc.FixtureArmed()
}

// fixtureHostArg mirrors the shared argument convention of every real investigation tool (librenms /
// estate / syslogng / hostdiag): the host may arrive under any of these keys.
func fixtureHostArg(args map[string]string) string {
	for _, k := range []string{"host", "target", "device", "hostname"} {
		if v := strings.TrimSpace(args[k]); v != "" {
			return v
		}
	}
	return ""
}

// fixtureSanitizeID keeps a ToolResult id printable and stable (the syslogng sanitize rule; identical to
// hostdiag's for the estate's plain alphanumeric hostnames).
func fixtureSanitizeID(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	var b strings.Builder
	for _, r := range h {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	if b.Len() == 0 {
		return "unnamed"
	}
	return b.String()
}

// fixtureResultID mirrors each real tool's ToolResult id scheme, so a fixture-armed session's evidence ids
// read exactly like a live session's (the INV-11 citation gate and the judge see one dialect, not two).
func fixtureResultID(tool, host string) string {
	switch tool {
	case "get-device-status":
		return "lnms-dev-" + normalizeFixtureHost(host)
	case "get-device-eventlog":
		return "lnms-events-" + normalizeFixtureHost(host)
	case "get-active-alerts":
		return "lnms-alerts-" + normalizeFixtureHost(host)
	case "get-host-logs":
		return "syslogng-logs-" + fixtureSanitizeID(host)
	case "search-host-logs":
		return "syslogng-search-" + fixtureSanitizeID(host)
	default: // the check-host-* family: hostdiag ids are "<check>-<sanitized-host>"
		return tool + "-" + fixtureSanitizeID(host)
	}
}

// fixtureMiss is the faithful "no data" observation for a (tool, host) the capture does not cover — each
// family's HONEST production shape for "I cannot serve this read", copied verbatim from the real tools:
//   - librenms: the resolveDevice miss ("device X: not present in deployment nl", Success=false);
//   - hostdiag: the fail-closed credential refusal (hostdiag.go Invoke's rerr branch);
//   - syslogng: the missing-logfile shape (tools.go's non-zero tail/grep exit branch).
//
// Returning a real-dialect miss (never an invented "fixture not found") keeps the fixture arm
// indistinguishable in TEXTURE from a live session: the agent adapts to a refusal the same way production
// would present it.
func fixtureMiss(tool, rawHost string) agent.ToolResult {
	res := agent.ToolResult{ID: fixtureResultID(tool, rawHost), Tool: tool}
	trimmed := strings.TrimSpace(rawHost)
	norm := normalizeFixtureHost(rawHost)
	switch tool {
	case "get-device-status", "get-device-eventlog", "get-active-alerts":
		if trimmed == "" {
			res.Output = "no host provided (pass args.host)"
			return res
		}
		res.Output = "device " + trimmed + ": not present in deployment nl"
	case "get-host-logs":
		if norm == "" {
			res.Output = fmt.Sprintf("refused: no host provided (pass args.host) (host=%q)", rawHost)
			return res
		}
		res.Output = fmt.Sprintf("no syslog-ng log for %s via dc1syslogng01 (date today (today.log)): the device may not log there, or that day has no file", norm)
	case "search-host-logs":
		if norm == "" {
			res.Output = fmt.Sprintf("refused: no host provided (pass args.host) (host=%q)", rawHost)
			return res
		}
		res.Output = fmt.Sprintf("no syslog-ng log to search for %s via dc1syslogng01 (date today (today.log)): the device may not log there, or that day has no file", norm)
	default: // check-host-*
		if trimmed == "" {
			res.Output = "refused: no host given"
			return res
		}
		res.Output = fmt.Sprintf("no resolvable SSH credential for %s — it is not covered by any credential rule/source (or the match is ambiguous), so I cannot investigate it directly", trimmed)
	}
	return res
}

// fixtureTool serves ONE real tool name from an incident's captured fixtures. It is pure text lookup —
// no HTTP client, no SSH runner, no credential resolution: a fixture-armed session structurally cannot
// dial out (fixtures_test.go guards this with a tripwire transport AND a type allowlist).
type fixtureTool struct {
	name     string
	fixtures map[string]FixtureResult
}

func (t fixtureTool) Name() string { return t.name }
func (fixtureTool) ReadOnly() bool { return true }

func (t fixtureTool) Invoke(_ context.Context, args map[string]string) (agent.ToolResult, error) {
	raw := fixtureHostArg(args)
	if f, ok := t.fixtures[FixtureKey(t.name, raw)]; ok {
		return agent.ToolResult{ID: fixtureResultID(t.name, raw), Tool: t.name, Output: f.Output, Success: f.Success}, nil
	}
	return t.miss(raw), nil
}

// miss is split out so the never-dials guard can exercise the miss path for every tool name directly.
func (t fixtureTool) miss(rawHost string) agent.ToolResult { return fixtureMiss(t.name, rawHost) }

// incidentContextTool hands the agent the incident's alert framing — the deterministic get-device-context
// both arms register (it is the offline seed observation; INV-11 needs concrete evidence to cite even when
// no estate tool answers).
type incidentContextTool struct{ ctx string }

func (incidentContextTool) Name() string   { return "get-device-context" }
func (incidentContextTool) ReadOnly() bool { return true }
func (t incidentContextTool) Invoke(_ context.Context, _ map[string]string) (agent.ToolResult, error) {
	return agent.ToolResult{ID: "dev-ctx-1", Tool: "get-device-context", Output: t.ctx, Success: true}, nil
}

// IncidentContextTool builds the alert-framing context tool for one incident (shared by both arms).
func IncidentContextTool(inc Incident) agent.Tool {
	return incidentContextTool{ctx: fmt.Sprintf("LibreNMS reports %s on %s (severity %s): %s", inc.AlertRule, inc.Host, inc.Severity, inc.Summary)}
}

// NewFixtureToolSet builds the agent toolset for a fixture-armed incident: the SAME tool names a
// production-parity live session has, with the network-backed ones served from the incident's captured
// fixtures and the offline ones (incident context, estate graph) kept REAL. Every registered real tool
// name resolves — the agent's calling surface is identical to the live arm's — but nothing here can reach
// the network, and none of it is env-gated: the fixture arm measures the same toolset on every box,
// including CI's.
func NewFixtureToolSet(inc Incident, g *estate.Graph) *agent.ToolSet {
	tools := agent.NewReadOnlyToolSet()
	_ = tools.Register(IncidentContextTool(inc))
	// The REAL estate-context tool over the same fixture graph the prediction gate reasons with — pure
	// in-memory topology, deterministic, no I/O; fixturing it would only fork the dialect.
	for _, tl := range estatetools.New(func() *estate.Graph { return g }) {
		_ = tools.Register(tl)
	}
	for _, name := range fixtureServedTools {
		_ = tools.Register(fixtureTool{name: name, fixtures: inc.ToolFixtures})
	}
	return tools
}
