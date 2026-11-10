package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/manifest"
)

// TG-647: a unit to restart must be one the host has. Live case (2026-11-06): `uvicorn`, a process name from the
// memory read, proposed as a restart-service unit on a memory-pressure alert; it exited 5 at the host.

// unitHostTool is check-host-services with a scripted output for one host.
type unitHostTool struct{ out string }

func (unitHostTool) Name() string   { return "check-host-services" }
func (unitHostTool) ReadOnly() bool { return true }
func (u unitHostTool) Invoke(_ context.Context, args map[string]string) (ToolResult, error) {
	return ToolResult{ID: "hs-1", Tool: "check-host-services", Target: args["host"], Output: u.out, Success: true}, nil
}

const gpuServices = `failed systemd units:
(none)
enabled service unit files (the should-run baseline):
ollama.service enabled enabled
tei-reranker.service enabled enabled
process state (pid stat unit comm):
 812 Ssl ollama.service ollama
 913 Sl tei-reranker.service uvicorn`

func proposeUnit(unit, evidence string) string {
	return `{"action":"propose","confidence":0.85,"proposal":{"external_ref":"TG-1","target":"gpu01","op_class":"restart-service","op":"restart","params":{"unit":"` +
		unit + `"},"reversible":true,"confidence":0.85,"evidence_ids":["` + evidence + `"]}}`
}

const readGPU = `{"action":"tool","tool":"check-host-services","args":{"host":"gpu01"},"confidence":0.8}`

func unitGatedAgent(m Completer) *Agent {
	ts := NewReadOnlyToolSet()
	_ = ts.Register(readTool{})
	_ = ts.Register(unitHostTool{out: gpuServices})
	return &Agent{Model: m, Tools: ts, Limits: DefaultLimits(), ModelName: "primary", User: "agent",
		UnitTargetGate: &UnitTargetGate{Tool: "check-host-services"}}
}

func lastUser(msgs []string, want string) bool {
	for _, m := range msgs {
		if strings.Contains(m, want) {
			return true
		}
	}
	return false
}

func userContents(m *msgRecModel) []string {
	var out []string
	for _, msg := range m.last {
		if msg.Role == "user" {
			out = append(out, msg.Content)
		}
	}
	return out
}

// No host read: the proposal is held once with the exact call; after the read, the corrected unit is accepted.
func TestUnitTargetGate_HoldsAnUnreadUnitForTheHostRead(t *testing.T) {
	m := &msgRecModel{scriptedModel: scriptedModel{responses: []string{toolCall, proposeUnit("uvicorn", "tr-1"), readGPU, proposeUnit("tei-reranker", "hs-1")}}}
	res, err := unitGatedAgent(m).Run(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomeProposed || res.Proposal.Action.Params["unit"] != "tei-reranker" || res.Cycles != 4 {
		t.Fatalf("want the corrected unit proposed on cycle 4, got %s unit=%q cycles=%d (%s)",
			res.Outcome, res.Proposal.Action.Params["unit"], res.Cycles, res.Reason)
	}
	if !lastUser(userContents(m), `you have not read the host's unit list`) || !lastUser(userContents(m), `"host":"gpu01"`) {
		t.Fatal("the held proposal must be answered with the exact host read for the proposal's target")
	}
}

// Host read, unit absent AS A UNIT (only as a process name in the comm column): held once with the reason.
func TestUnitTargetGate_ProcessNameIsNotAUnit(t *testing.T) {
	m := &msgRecModel{scriptedModel: scriptedModel{responses: []string{readGPU, proposeUnit("uvicorn", "hs-1"), proposeUnit("tei-reranker", "hs-1")}}}
	res, _ := unitGatedAgent(m).Run(context.Background(), nil)
	if res.Outcome != OutcomeProposed || res.Proposal.Action.Params["unit"] != "tei-reranker" || res.Cycles != 3 {
		t.Fatalf("want the listed unit proposed after one hold, got %s unit=%q cycles=%d", res.Outcome, res.Proposal.Action.Params["unit"], res.Cycles)
	}
	if !lastUser(userContents(m), `do not contain it`) {
		t.Fatal("the hold must say the unit is not in the unit list that was read")
	}
}

// A listed unit passes at once, bare or with its suffix; a repeat of an unlisted unit is accepted (never grind).
func TestUnitTargetGate_ListedUnitPassesAndRepeatIsAccepted(t *testing.T) {
	for _, u := range []string{"tei-reranker", "tei-reranker.service", "OLLAMA"} {
		m := &scriptedModel{responses: []string{readGPU, proposeUnit(u, "hs-1")}}
		if res, _ := unitGatedAgent(m).Run(context.Background(), nil); res.Outcome != OutcomeProposed || res.Cycles != 2 {
			t.Fatalf("listed unit %q must pass at once, got %s cycles=%d", u, res.Outcome, res.Cycles)
		}
	}
	m := &scriptedModel{responses: []string{readGPU, proposeUnit("uvicorn", "hs-1"), proposeUnit("uvicorn", "hs-1")}}
	if res, _ := unitGatedAgent(m).Run(context.Background(), nil); res.Outcome != OutcomeProposed || res.Cycles != 3 {
		t.Fatalf("a repeat proposal must be accepted after the one hold, got %s cycles=%d", res.Outcome, res.Cycles)
	}
}

// Inert: no gate, a non-unit action, or a read of a DIFFERENT host does not count as the target's read.
func TestUnitTargetGate_Scope(t *testing.T) {
	g := &UnitTargetGate{Tool: "check-host-services"}
	ts := NewReadOnlyToolSet()
	_ = ts.Register(unitHostTool{})
	other := []ToolResult{{ID: "hs-9", Tool: "check-host-services", Target: "web01", Output: "uvicorn.service", Success: true}}
	if (*UnitTargetGate)(nil).hold(ts, manifest.Action{Target: "gpu01", Params: map[string]string{"unit": "x"}}, nil) != "" {
		t.Fatal("a nil gate must be inert")
	}
	if g.hold(ts, manifest.Action{Target: "gpu01", OpClass: "restart-container", Params: map[string]string{"container": "x"}}, nil) != "" {
		t.Fatal("an action without a unit param must pass")
	}
	if g.hold(ts, manifest.Action{Target: "gpu01", OpClass: "restart-service", Params: map[string]string{"unit": "uvicorn"}}, other) == "" {
		t.Fatal("a read of another host must not ground the target's unit")
	}
	if g.hold(NewReadOnlyToolSet(), manifest.Action{Target: "gpu01", Params: map[string]string{"unit": "uvicorn"}}, nil) != "" {
		t.Fatal("a session without the host read tool must be unchanged")
	}
}

// Review follow-ups: template instances match their template file, host names compare on the short form, and a
// proposal at or past the poll limit is never held (a hold there would become a hand-off escalation).
func TestUnitTargetGate_TemplateFQDNAndLateProposal(t *testing.T) {
	if !listsUnit("getty@.service enabled enabled", "getty@tty1.service") || !listsUnit("getty@.service enabled", "getty@tty1") {
		t.Fatal("a template instance must match its template unit file")
	}
	if listsUnit("getty@.service enabled", "getty") {
		t.Fatal("a bare name must not match a template file")
	}
	g := &UnitTargetGate{Tool: "check-host-services"}
	ts := NewReadOnlyToolSet()
	_ = ts.Register(unitHostTool{})
	fqdn := []ToolResult{{ID: "hs-1", Tool: "check-host-services", Target: "gpu01.example.net", Output: gpuServices, Success: true}}
	if g.hold(ts, manifest.Action{Target: "gpu01", OpClass: "restart-service", Params: map[string]string{"unit": "ollama"}}, fqdn) != "" {
		t.Fatal("a read of the host's FQDN must ground a proposal on its short name")
	}

	lim := DefaultLimits()
	late := append(monitoringReads(lim.HandoffPoll-1), readGPU, proposeUnit("uvicorn", "hs-1"))
	m := &scriptedModel{responses: late}
	res, _ := unitGatedAgent(m).Run(context.Background(), nil)
	if res.Outcome != OutcomeProposed || res.Cycles != lim.HandoffPoll+1 {
		t.Fatalf("a proposal past the poll limit must be accepted, not held into an escalation, got %s cycles=%d (%s)",
			res.Outcome, res.Cycles, res.Reason)
	}
}

// With HandoffHalt only one past HandoffPoll there is no room for the host read AND the decision: the decide-now
// nudge goes first, as before the hand-off host-read nudge existed.
func TestHostReadGate_HandoffNudgeNeedsTwoCyclesLeft(t *testing.T) {
	lim := Limits{HandoffPoll: 5, HandoffHalt: 6}
	m := &msgRecModel{scriptedModel: scriptedModel{responses: monitoringReads(lim.HandoffHalt)}}
	a := gatedAgent(m, true)
	a.Limits = lim
	a.Run(context.Background(), nil)
	if host, decide := handoffNudged(m.last); host != 0 || decide != 1 {
		t.Fatalf("with one cycle left only the decide-now nudge may fire, got host=%d decide=%d", host, decide)
	}
}
