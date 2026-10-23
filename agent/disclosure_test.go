package agent

// TG-215 — class-keyed progressive tool disclosure. The properties pinned here, beside the per-class
// goldens in prompt_golden_test.go:
//
//   1. Every class except FAST_AGENT renders the flat catalog BYTE-IDENTICAL to Catalog() — including
//      the empty class and garbage (the conservative fallback), and regardless of source labels.
//   2. The FAST_AGENT render still lists EVERY registered tool (an index line is a disclosure
//      reduction, never a capability removal), grouped by source namespace.
//   3. A directive naming an INDEX-ONLY-listed tool still EXECUTES — the loop's dispatch reads the
//      registered set, not the preamble — and its observation is citable like any other.
//   4. An UNKNOWN tool is still refused under FAST_AGENT (INV-08 unchanged).
//
// KILLING MUTATIONS: route CatalogFor's non-fast arm through the namespaced render — (1) reddens; drop
// an index entry from the fast render — (2) reddens; make the loop dispatch consult fastDisclosed —
// (3) reddens; accept an unregistered name under fast — (4) reddens.

import (
	"context"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/adapters/model"
	"github.com/territory-grounder/grounder/core/execclass"
)

// fxTool is the disclosure fixture's configurable ACI tool. `invoked` (optional) records every args map
// Invoke received, so the loop test can prove an index-listed tool actually EXECUTED.
type fxTool struct {
	name, desc string
	params     []ParamSpec
	result     ToolResult
	invoked    *[]map[string]string
}

func (t fxTool) Name() string        { return t.name }
func (t fxTool) ReadOnly() bool      { return true }
func (t fxTool) Description() string { return t.desc }
func (t fxTool) Params() []ParamSpec { return t.params }
func (t fxTool) Invoke(_ context.Context, args map[string]string) (ToolResult, error) {
	if t.invoked != nil {
		*t.invoked = append(*t.invoked, args)
	}
	return t.result, nil
}

// fxBareTool is a plain Tool (no ACI schema) — the backward-compatible bare-name path, registered with
// NO source so the fixture also exercises the "other" namespace fallback.
type fxBareTool struct{}

func (fxBareTool) Name() string   { return "probe-legacy" }
func (fxBareTool) ReadOnly() bool { return true }
func (fxBareTool) Invoke(context.Context, map[string]string) (ToolResult, error) {
	return ToolResult{ID: "pl-1", Tool: "probe-legacy", Output: "ok", Success: true}, nil
}

func fxHostP(what string) []ParamSpec {
	return []ParamSpec{{Name: "host", Type: "host", Required: true, Example: "web01", Description: what}}
}

// disclosureFixtureSet is the FIXED tool set the per-class goldens hash: live-shaped names across all
// four live source namespaces plus an unlabeled bare tool, fast-disclosed and index-only members of
// each kind. The descriptions deliberately exercise each oneLineSummary boundary (": ", " — ", ". ").
// The deep-class golden over this set was captured on main with plain Register — reproducing it via
// RegisterFrom is itself proof that source labels change nothing outside FAST_AGENT.
func disclosureFixtureSet() *ToolSet {
	ts := NewReadOnlyToolSet()
	for _, reg := range []struct {
		source string
		tool   Tool
	}{
		{"librenms", fxTool{name: "get-device-status", desc: "Read one device's CURRENT monitored state: up/down/disabled and when it was last polled. Use it first to establish reachability.", params: fxHostP("the device whose monitored status to read")}},
		{"librenms", fxTool{name: "get-device-eventlog", desc: "Read the most recent poller eventlog entries for a device. Puts a time and a sequence on a fault.", params: fxHostP("the device whose recent eventlog to read")}},
		{"librenms", fxTool{name: "get-active-alerts", desc: "List the alerts CURRENTLY firing on a device, with each rule's name and severity.", params: fxHostP("the device whose currently-firing alerts to list")}},
		{"estate", fxTool{name: "get-estate-context", desc: "Read a host's place in the estate's causal graph — upstream dependencies, blast radius, and common-cause siblings.", params: fxHostP("the host whose causal neighbourhood to read")}},
		{"host", fxTool{name: "check-host-services", desc: "SSH a host and read its SERVICE state: failed units, inactive units, and containers. Names a concrete restart target.", params: fxHostP("the host whose service state to inspect")}},
		{"host", fxTool{name: "search-host-logs", desc: "Search a device's syslog for a FIXED string and return the matching lines.", params: []ParamSpec{
			{Name: "host", Type: "host", Required: true, Example: "fw01", Description: "the device whose log to search"},
			{Name: "pattern", Type: "string", Required: true, Example: "%LINK-3-UPDOWN", Description: "the fixed string to search for"},
		}}},
		{"history", fxTool{name: "get-incident-history", desc: "Read the record of PRIOR incidents on a host: outcomes, op-classes, and conclusions.", params: fxHostP("the host whose prior incidents to read")}},
		{"", fxBareTool{}},
	} {
		if err := ts.RegisterFrom(reg.source, reg.tool); err != nil {
			panic(err)
		}
	}
	return ts
}

// TestNonFastClassesRenderTheFullCatalogByteIdentical: the conservative arm is byte-equality with
// Catalog() itself, for every class that is not an affirmative FAST_AGENT — on ANY tool set, which is
// stronger than the fixture golden alone.
func TestNonFastClassesRenderTheFullCatalogByteIdentical(t *testing.T) {
	ts := disclosureFixtureSet()
	full := ts.Catalog()
	for name, class := range map[string]execclass.Class{
		"DEEP_INVESTIGATION": execclass.DeepInvestigation,
		"HUMAN_LED":          execclass.HumanLed,
		"STANDARD_AGENT":     execclass.StandardAgent,
		"DETERMINISTIC":      execclass.Deterministic,
		"unthreaded-empty":   "",
		"garbage":            execclass.Class("WAT"),
	} {
		if got := ts.CatalogFor(class); got != full {
			t.Errorf("CatalogFor(%s) must be byte-identical to Catalog(); diff begins at byte %d",
				name, firstDiff(got, full))
		}
	}
	if nilSet := (*ToolSet)(nil); nilSet.CatalogFor(execclass.FastAgent) != "" {
		t.Error("a nil set must render an empty catalog for every class (the sentinel path)")
	}
}

func firstDiff(a, b string) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

// TestFastCatalogListsEveryToolAndReducesDisclosure: the FAST_AGENT render (a) still names every
// registered tool — the disclosure floor an index line must never sink below; (b) groups by source
// namespace; (c) discloses full schemas ONLY for the fastDisclosed point reads; (d) carries the
// index-entries-are-callable note.
func TestFastCatalogListsEveryToolAndReducesDisclosure(t *testing.T) {
	ts := disclosureFixtureSet()
	fast := ts.CatalogFor(execclass.FastAgent)
	for _, name := range ts.Names() {
		if !strings.Contains(fast, "- "+name) {
			t.Errorf("FAST_AGENT catalog must still LIST %q — an index line is a disclosure reduction, "+
				"not a removal from the catalog", name)
		}
	}
	for _, heading := range []string{"estate:\n", "history:\n", "host:\n", "librenms:\n", "other:\n"} {
		if !strings.Contains(fast, heading) {
			t.Errorf("FAST_AGENT catalog must group by source namespace; missing heading %q", heading)
		}
	}
	// Disclosed: the full entry (param schema present, rendered by the same writer as the flat catalog).
	if !strings.Contains(fast, "the device whose monitored status to read") {
		t.Error("get-device-status is fast-disclosed — its full param schema must render")
	}
	// Index-only: name + one-line purpose, NO param schema.
	if !strings.Contains(fast, "- check-host-services — SSH a host and read its SERVICE state\n") {
		t.Error("check-host-services must render as a one-line index entry (name — head clause)")
	}
	for _, leaked := range []string{
		"the host whose service state to inspect", // check-host-services' param description
		"the fixed string to search for",          // search-host-logs' param description
		"the host whose prior incidents to read",  // get-incident-history's param description
	} {
		if strings.Contains(fast, leaked) {
			t.Errorf("an index-only tool's param schema leaked into the FAST_AGENT catalog: %q", leaked)
		}
	}
	if !strings.Contains(fast, indexNote) {
		t.Error("a FAST_AGENT catalog with index entries must carry the index-entries-are-callable note")
	}
}

// disclosureCapture is a scripted Completer that records the FIRST call's system preamble, so the loop
// test can prove the session it drives really ran on the reduced FAST_AGENT disclosure.
type disclosureCapture struct {
	responses []string
	i         int
	preamble  string
}

func (m *disclosureCapture) Complete(_ context.Context, _, _ string, msgs []model.Message) (string, error) {
	if m.preamble == "" && len(msgs) > 0 {
		m.preamble = msgs[0].Content
	}
	if m.i >= len(m.responses) {
		return `{"action":"stop","confidence":0.9}`, nil
	}
	r := m.responses[m.i]
	m.i++
	return r, nil
}

// TestIndexOnlyListedToolStillExecutes — THE capability-preservation proof (TG-215). A FAST_AGENT
// session whose preamble index-listed check-host-services (name + purpose, schema withheld — asserted
// on the very preamble the model received) invokes it by name and the call EXECUTES, its observation is
// captured, and the stop citing it passes the citation gate. The preamble is disclosure; the dispatch
// is the registered set.
func TestIndexOnlyListedToolStillExecutes(t *testing.T) {
	var invoked []map[string]string
	ts := NewReadOnlyToolSet()
	if err := ts.RegisterFrom("librenms", fxTool{name: "get-device-status", desc: "Read one device's CURRENT monitored state: up/down.", params: fxHostP("the device whose monitored status to read"), result: ToolResult{ID: "dev-1", Tool: "get-device-status", Output: "up", Success: true}}); err != nil {
		t.Fatal(err)
	}
	if err := ts.RegisterFrom("host", fxTool{name: "check-host-services", desc: "SSH a host and read its SERVICE state: failed units.", params: fxHostP("the host whose service state to inspect"), result: ToolResult{ID: "hd-1", Tool: "check-host-services", Output: "nginx inactive", Success: true}, invoked: &invoked}); err != nil {
		t.Fatal(err)
	}
	m := &disclosureCapture{responses: []string{
		`{"action":"tool","tool":"check-host-services","args":{"host":"web01"},"confidence":0.8}`,
		`{"action":"stop","confidence":0.9,"reason":"nginx was stopped cleanly; no safe action","evidence_ids":["hd-1"]}`,
	}}
	ag := &Agent{Model: m, Tools: ts, Limits: DefaultLimits(), ModelName: "fast", User: "t", Class: execclass.FastAgent}
	res, err := ag.Run(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	// The session really ran on the reduced disclosure: index line present, schema absent.
	if !strings.Contains(m.preamble, "- check-host-services — SSH a host and read its SERVICE state") {
		t.Fatal("the preamble the model received did not index-list check-host-services — this test is not proving what it claims")
	}
	if strings.Contains(m.preamble, "the host whose service state to inspect") {
		t.Fatal("the preamble disclosed check-host-services' schema — this test is not exercising an index-only tool")
	}
	// …and the index-only tool still executed, was observed, and its observation grounded the stop.
	if len(invoked) != 1 || invoked[0]["host"] != "web01" {
		t.Fatalf("the index-listed tool must EXECUTE exactly as a fully-disclosed one; invocations: %v", invoked)
	}
	if len(res.ToolResults) != 1 || res.ToolResults[0].ID != "hd-1" {
		t.Fatalf("the index-listed tool's observation must be captured; got %+v", res.ToolResults)
	}
	if res.Outcome != OutcomeStop || res.Reason != "agent requested stop" {
		t.Fatalf("want a clean grounded stop, got %s (%s)", res.Outcome, res.Reason)
	}
	if len(res.ConclusionEvidence) != 1 || res.ConclusionEvidence[0] != "hd-1" {
		t.Fatalf("citing an index-listed tool's observation must pass the citation gate; got %v", res.ConclusionEvidence)
	}
}

// TestUnknownToolStillRefusedUnderFastClass — INV-08 unchanged by disclosure: a name outside the
// registered set is never dispatched under FAST_AGENT; it becomes the same recoverable TOOL_ERROR as
// under every other class.
func TestUnknownToolStillRefusedUnderFastClass(t *testing.T) {
	ts := disclosureFixtureSet()
	m := &disclosureCapture{responses: []string{
		`{"action":"tool","tool":"get-made-up-tool","args":{"host":"web01"},"confidence":0.8}`,
		`{"action":"stop","confidence":0.9}`,
	}}
	ag := &Agent{Model: m, Tools: ts, Limits: DefaultLimits(), ModelName: "fast", User: "t", Class: execclass.FastAgent}
	res, err := ag.Run(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.ToolResults) != 0 {
		t.Fatalf("an unknown tool must never be dispatched; got observations %+v", res.ToolResults)
	}
	if len(res.Steps) == 0 || res.Steps[0].Outcome != "tool-error" || res.Steps[0].Observation != "TOOL_ERROR (unknown tool)" {
		t.Fatalf("the unknown name must become the recoverable unknown-tool TOOL_ERROR; steps: %+v", res.Steps)
	}
}

// TestUnknownActionRecoversInsteadOfStandingDown — the ACTION-level mirror of the unknown-TOOL recovery
// (TG-552): a TOOL name in the action field ("check-host-services" instead of {"action":"tool",...}) must
// become a recoverable ACTION_ERROR the model retries from, NOT the empty no-proposal:stop it formerly
// aborted with. INV-08 unchanged: the unknown action is never dispatched.
func TestUnknownActionRecoversInsteadOfStandingDown(t *testing.T) {
	ts := disclosureFixtureSet()
	m := &disclosureCapture{responses: []string{
		`{"action":"check-host-services","args":{"host":"web01"},"confidence":0.8}`, // TOOL name in the ACTION field — the TG-552 slip
		`{"action":"stop","confidence":0.9,"reason":"recovered"}`,                   // a valid terminal
	}}
	ag := &Agent{Model: m, Tools: ts, Limits: DefaultLimits(), ModelName: "fast", User: "t", Class: execclass.FastAgent}
	res, err := ag.Run(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	// KILLING MUTATION: restore the default to `OutcomeStop, "unknown action "+d.Action; return res, nil` and this
	// fails — the session would abort on cycle 1 with reason "unknown action check-host-services", never the stop.
	if strings.HasPrefix(res.Reason, "unknown action") {
		t.Fatalf("an unknown action must RECOVER, not stand down; got outcome=%s reason=%q", res.Outcome, res.Reason)
	}
	if len(res.Steps) == 0 || res.Steps[0].Outcome != "action-error" || res.Steps[0].Observation != "ACTION_ERROR (unknown action)" {
		t.Fatalf("the unknown action must become the recoverable ACTION_ERROR step; steps: %+v", res.Steps)
	}
	if res.Outcome != OutcomeStop {
		t.Fatalf("after recovery the session must reach its own terminal; got %s / %q", res.Outcome, res.Reason)
	}
}

// TestOneLineSummary pins the head-clause derivation the index lines rest on.
func TestOneLineSummary(t *testing.T) {
	for in, want := range map[string]string{
		"Read one device's CURRENT monitored state: up/down and uptime. Use it first.":  "Read one device's CURRENT monitored state",
		"Read a host's place in the causal graph — upstream and blast radius.":          "Read a host's place in the causal graph",
		"List the alerts CURRENTLY firing on a device, with severity. Empty ≠ healthy.": "List the alerts CURRENTLY firing on a device, with severity",
		"Report filesystem usage for a host.":                                           "Report filesystem usage for a host",
		"":                                                                              "",
	} {
		if got := oneLineSummary(in); got != want {
			t.Errorf("oneLineSummary(%q) = %q, want %q", in, got, want)
		}
	}
	long := strings.Repeat("x", 200)
	if got := oneLineSummary(long); len([]rune(got)) != 141 || !strings.HasSuffix(got, "…") {
		t.Errorf("an unbroken 200-rune description must cap at 140 runes + ellipsis; got %d runes", len([]rune(got)))
	}
}
