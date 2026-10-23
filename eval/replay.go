package eval

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/territory-grounder/grounder/agent"
	"github.com/territory-grounder/grounder/core/estate"
	"github.com/territory-grounder/grounder/core/trace"
	estatetools "github.com/territory-grounder/grounder/modules/estate"
)

// REPLAY TOOL-SET (TG-580, Harness² learnings H2-6a).
//
// Harness² probes a candidate harness by rolling it out on the SAME task the base ran. TG's analogue — offline
// admission — was single-shot and tool-less, and spec/025 documented the 200-300-incident replay corpus as not
// existing. The raw material has been persisted all along: agent_step (0031) holds each cycle's tool + outcome,
// agent_step_evidence (0053) the SCREENED, SCRUBBED observation behind it. A RecordedSession is that record
// read back; NewReplayToolSet serves it to a fresh agent run with the SAME tool names a live session has, so a
// candidate skill body or guidance half can be re-run against exactly the world the original session saw.
//
// Two honesty rules, both in the agent's own calling surface:
//   - A call the recording never answered gets a DISTINCT refusal (replayMiss): Success=false and a body that
//     names the state — "not recorded in this session" — never an empty result and never the fixture arm's
//     "device not present" shape. "Found nothing" and "was never asked" are different facts (AGENTS.md).
//   - A targeted call (host/target/device arg) is served ONLY from an observation recorded for THAT target,
//     matched through the tool result's own id (every investigation tool stamps its normalized target into
//     the id: lnms-dev-<host>, syslogng-logs-<host>, check-host-disk-<host>, …). Serving another host's
//     observation would be a quiet lie on an evidence surface. Target-less calls are served in cycle order.
//   - A tool that is REAL in both deterministic arms (the estate graph is in-memory) is still served from the
//     recording when the session recorded what it said: the graph a replay runs against — empty on a CI box, a
//     later snapshot anywhere else — is not the graph the session saw, and a probe's whole point is the world the
//     original saw (first live dump 2026-10-23: the real estate reads were the only observations that could not
//     round-trip). A recording without such an observation keeps the real tool, exactly the fixture arm's shape.
//
// Nothing here dials out: the replay tools are pure text lookups (replay_test.go pins the type allowlist).

// RecordedCall is one observation a past session recorded: the cycle it happened in, the tool, the result id
// the tool itself stamped, the persisted (bounded) payload, and whether that cycle's outcome was a success.
type RecordedCall struct {
	Cycle      int
	Tool       string
	EvidenceID string
	Output     string
	Truncated  bool
	Success    bool
}

// RecordedSession is a past session's observations in cycle order plus its identity.
type RecordedSession struct {
	ExternalRef string
	Calls       []RecordedCall
}

// LoadRecordedSession joins a session's reasoning steps (per-cycle outcome) with its recorded observations.
// A session with steps but no evidence (recorded before migration 0053) yields an empty Calls — a real answer the
// caller must read as "nothing to replay", not as a tool that returned nothing.
func LoadRecordedSession(ctx context.Context, steps interface {
	Steps(ctx context.Context, externalRef string) ([]trace.AgentStep, error)
}, evidence trace.AgentStepEvidenceLister, externalRef string) (RecordedSession, error) {
	rec := RecordedSession{ExternalRef: externalRef}
	st, err := steps.Steps(ctx, externalRef)
	if err != nil {
		return rec, fmt.Errorf("replay: steps for %s: %w", externalRef, err)
	}
	// One reasoning step per cycle (the loop records exactly one tool call per cycle), so the cycle's outcome is
	// the observation's: a failed cycle replays its observation with Success=false.
	outcome := map[int]string{}
	for _, s := range st {
		outcome[s.Cycle] = s.Outcome
	}
	ev, err := evidence.EvidenceFor(ctx, externalRef)
	if err != nil {
		return rec, fmt.Errorf("replay: evidence for %s: %w", externalRef, err)
	}
	for _, e := range ev {
		rec.Calls = append(rec.Calls, RecordedCall{
			Cycle: e.Cycle, Tool: e.Tool, EvidenceID: e.EvidenceID, Output: e.Payload, Truncated: e.Truncated,
			// The evidence row carries no success flag; the step row of the same cycle does. An unknown
			// outcome ('' — pre-outcome rows) reads as success: the payload was recorded as an observation.
			Success: outcome[e.Cycle] != "failure",
		})
	}
	sort.SliceStable(rec.Calls, func(i, j int) bool { return rec.Calls[i].Cycle < rec.Calls[j].Cycle })
	return rec, nil
}

// RecordedFromFixtures builds the RecordedSession a fixture-armed incident WOULD have recorded: one call per
// captured (tool, host), with the id the fixture arm stamps. It is the bridge between the two deterministic arms
// and the oracle's instrument: a fixture incident replayed through NewReplayToolSet must be byte-identical.
// Its cycle numbers are SYNTHETIC (sorted fixture keys), not the trajectory order of any real session — fixture
// recordings are never mixed with live ones, and nothing but same-target ordering depends on the numbers.
func RecordedFromFixtures(inc Incident) RecordedSession {
	rec := RecordedSession{ExternalRef: inc.ExternalRef}
	keys := make([]string, 0, len(inc.ToolFixtures))
	for k := range inc.ToolFixtures {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for i, k := range keys {
		tool, host, _ := strings.Cut(k, "|")
		f := inc.ToolFixtures[k]
		rec.Calls = append(rec.Calls, RecordedCall{Cycle: i + 1, Tool: tool, EvidenceID: fixtureResultID(tool, host), Output: f.Output, Success: f.Success})
	}
	return rec
}

// replayState is the per-run cursor over a recording, shared by every replay tool of one tool-set.
type replayState struct {
	mu     sync.Mutex
	rec    RecordedSession
	served map[int]bool // index into rec.Calls → already served once (ordinal fallback advances past these)
}

// replayTool serves ONE real tool name from the recording. Pure text lookup; no client, no runner.
type replayTool struct {
	name string
	st   *replayState
	aci  agent.ACITool // the displaced real tool's catalog entry (the catalog twin, or the tool itself) — never invoked
}

// Description and Params are the real tool's, so the replayed agent's prompt and arg screen are byte-for-byte
// the fixture arm's — and, through the catalog twin (TG-592), the live arm's.
func (t replayTool) Description() string {
	d, _ := describedBy(t.aci)
	return d
}

func (t replayTool) Params() []agent.ParamSpec {
	_, p := describedBy(t.aci)
	return p
}

func (t replayTool) Name() string { return t.name }
func (replayTool) ReadOnly() bool { return true }

func (t replayTool) Invoke(_ context.Context, args map[string]string) (agent.ToolResult, error) {
	raw := fixtureHostArg(args)
	t.st.mu.Lock()
	defer t.st.mu.Unlock()
	// ONE RULE for targeted and target-less calls alike: an observation answers a call only when the target its
	// recorded id names EQUALS the call's target (a target-less call, norm "", matches only a target-less
	// recording — a call that omits the host is never served another host's observation). Prefer an unserved
	// match so a session that queried the same target twice (two cycles, two payloads) replays both in order.
	last := -1
	for i, c := range t.st.rec.Calls {
		recorded, known := RecordedTarget(c.Tool, c.EvidenceID)
		if c.Tool != t.name || !known || !targetMatches(recorded, raw) {
			continue
		}
		last = i
		if !t.st.served[i] {
			t.st.served[i] = true
			return t.result(c, false), nil
		}
	}
	if last >= 0 {
		// Exhausted: the recording has no further observation for this call. Repeating the last one keeps the
		// replay deterministic, but SAYS so — a live tool would have re-read current state, and a replayed
		// agent must never take an unchanged repeat for a confirmation.
		return t.result(t.st.rec.Calls[last], true), nil
	}
	return replayMiss(t.name, raw, t.st.rec.ExternalRef), nil
}

// ReplayExhaustedNote is appended to a repeated observation once a tool's recordings for that target are used up.
const ReplayExhaustedNote = "\n[replay: recording exhausted for this call — this repeats the last recorded observation; a live tool would have re-read current state]"

// ReplayTruncatedNote is appended to an observation whose capture was bounded at write time: the replay must not
// present the stored prefix as the whole tool output.
const ReplayTruncatedNote = "\n[replay: the recorded observation was truncated at capture — this is the stored prefix, not the full tool output]"

func (t replayTool) result(c RecordedCall, exhausted bool) agent.ToolResult {
	out := c.Output
	if c.Truncated {
		out += ReplayTruncatedNote
	}
	if exhausted {
		out += ReplayExhaustedNote
	}
	return agent.ToolResult{ID: c.EvidenceID, Tool: c.Tool, Output: out, Success: c.Success}
}

// recordedIDPrefixes are the `<family>-` prefixes the investigation tools stamp before their normalized target
// (modules/ingest/librenms, modules/observability/syslogng, modules/cmdb/netbox, modules/actorevidence); the
// hostdiag family stamps `<tool name>-`. Order matters only in that a longer, more specific prefix is tried
// before a shorter one.
var recordedIDPrefixes = []string{"lnms-dev-", "lnms-events-", "lnms-alerts-", "lnms-storage-", "syslogng-logs-", "syslogng-search-", "netbox-dev-", "actor-ev-",
	// modules/estate stamps estate-ctx-<sanitizeID(host)>; its sanitizer lowercases and KEEPS dots, dashes and
	// underscores (the syslogng spelling), so the id carries the lowercase raw host.
	"estate-ctx-"}

// replayPreferredTools are the tools REAL in both deterministic arms whose recorded observation nevertheless WINS
// when the recording holds one (see the package comment). Exported through ReplayPreferred for the dump/verify
// tool, which reports such observations as served from the recording rather than by the live graph.
var replayPreferredTools = map[string]bool{"get-estate-context": true}

// ReplayPreferred reports whether a tool is real in the fixture arm but served from the recording when recorded.
func ReplayPreferred(tool string) bool { return replayPreferredTools[tool] }

// recordingHas reports whether the recording holds at least one observation of the named tool.
func recordingHas(rec RecordedSession, tool string) bool {
	for _, c := range rec.Calls {
		if c.Tool == tool {
			return true
		}
	}
	return false
}

// RecordedTarget returns the target a recorded result id names — the remainder after the family prefix the
// tool stamps ("" for a target-less read) — and whether the id belongs to a KNOWN family at all. An id in no
// known family is not "target-less": it is unreadable, and ok=false makes the matcher refuse it rather than let
// a target-less call be answered by an observation of unknown shape. Exported so the dump/verify tool and the
// matcher derive the target the same way.
func RecordedTarget(tool, evidenceID string) (target string, ok bool) {
	id := strings.ToLower(strings.TrimSpace(evidenceID))
	for _, pfx := range append([]string{strings.ToLower(tool) + "-"}, recordedIDPrefixes...) {
		if strings.HasPrefix(id, pfx) {
			rest := strings.TrimPrefix(id, pfx)
			if pfx == "estate-ctx-" && rest == "unnamed" {
				return "", true // the estate tool's spelling of "no host given" — a target-less read
			}
			return rest, true
		}
	}
	return "", false
}

// targetMatches reports whether a recorded target EQUALS the call's target under the spellings the real tools
// use: the librenms rule (normalizeFixtureHost — lowercase, first token, DNS domain stripped for name-like hosts),
// the hostdiag rule (sanitizeID: every non-alphanumeric run → "-", trimmed, capped at 48 — so a dotted FQDN or
// IP is dashed) and the syslogng rule (its sanitizeID KEEPS dots, so the id carries the lowercase raw host). A
// target-less call ("" after normalization) matches only a target-less recording. Equality, never a substring:
// `web1` is not `web11`, `pve` is not `tg78-pve-guest-02`, and the tool name is not a host. Deliberately strict
// the other way too: a recording made under the FQDN spelling (`check-host-disk-web01-example-net`) is not served
// to a bare `web01` call — a replayed agent must ask the way the original asked, or hear that it did not.
func targetMatches(recorded, rawCall string) bool {
	norm := normalizeFixtureHost(rawCall)
	if norm == "" || recorded == "" {
		return norm == "" && recorded == ""
	}
	rawLower := strings.ToLower(strings.TrimSpace(rawCall))
	for _, cand := range []string{norm, sanitizeLikeID(norm), sanitizeLikeID(rawLower), rawLower, sanitizeLikeEstate(rawLower)} {
		if cand == recorded || (len(recorded) == 48 && len(cand) > 48 && cand[:48] == recorded) {
			return true
		}
	}
	return false
}

var nonAlnumRun = regexp.MustCompile(`[^a-z0-9]+`)

// sanitizeLikeEstate mirrors modules/estate's result-id sanitizer EXACTLY: lowercase; every rune outside [a-z0-9._-]
// becomes "-" one-for-one (no run collapsing, no trimming, no cap). It differs from sanitizeLikeID on a trailing or
// doubled special ("web01!" → "web01-", "fe80::1" → "fe80--1") and on hosts over 48 bytes.
func sanitizeLikeEstate(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return b.String()
}

// sanitizeLikeID mirrors the hostdiag/syslogng result-id sanitizer: non-alphanumeric runs → "-", trimmed, ≤ 48.
func sanitizeLikeID(s string) string {
	s = nonAlnumRun.ReplaceAllString(strings.ToLower(strings.TrimSpace(s)), "-")
	if len(s) > 48 {
		s = s[:48]
	}
	return strings.Trim(s, "-")
}

// ReplayMissMarker is the fixed prefix of a replay refusal's body — the test for "the replay could not answer"
// that a caller or an oracle keys on, distinct from every fixture-arm miss shape.
const ReplayMissMarker = "REPLAY: not recorded"

// replayMiss is the DISTINCT state for a call the recording never answered. Success=false, its own id, and a
// body that says what it is: not an empty result, not "device not present", not an error.
func replayMiss(tool, rawHost, externalRef string) agent.ToolResult {
	target := strings.TrimSpace(rawHost)
	if target == "" {
		target = "(no target)"
	}
	return agent.ToolResult{
		ID:   "replay-miss-" + tool + "-" + normalizeFixtureHost(rawHost),
		Tool: tool,
		Output: fmt.Sprintf("%s — session %s never observed %s on %s; this call is NOVEL relative to the recording and cannot be answered offline (a replay serves only what the original session saw).",
			ReplayMissMarker, externalRef, tool, target),
		Success: false,
	}
}

// NewReplayToolSet builds the agent toolset for a replayed session: the SAME tool names a production-parity live
// session has, with every network-backed one served from the recording and the offline ones (incident framing,
// estate graph) kept REAL — exactly the fixture arm's shape (NewFixtureToolSet), so a run differs from the live
// arm only in WHERE the observations come from — except that an estate read the session RECORDED is served from
// the recording (replayPreferredTools): the graph handed in here is not the graph the session saw. Nothing here
// can reach the network.
func NewReplayToolSet(rec RecordedSession, inc Incident, g *estate.Graph) *agent.ToolSet {
	st := &replayState{rec: rec, served: map[int]bool{}}
	tools := agent.NewReadOnlyToolSet()
	_ = tools.Register(IncidentContextTool(inc))
	for _, tl := range estatetools.New(func() *estate.Graph { return g }) {
		if replayPreferredTools[tl.Name()] && recordingHas(rec, tl.Name()) {
			rt := replayTool{name: tl.Name(), st: st}
			rt.aci, _ = tl.(agent.ACITool) // the displaced tool's own entry
			_ = tools.Register(rt)
			continue
		}
		_ = tools.Register(tl)
	}
	for _, name := range fixtureServedTools {
		_ = tools.Register(replayTool{name: name, st: st, aci: catalogTwin()[name]})
	}
	return tools
}
