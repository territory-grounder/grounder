package evidencepack

import (
	"testing"

	"github.com/territory-grounder/grounder/core/judge"
	"github.com/territory-grounder/grounder/core/trace"
)

// Assemble maps the stores' facts to the pack: the deliverable is the judge's own Facts, UNTOUCHED (the record the
// judge adjudicates and nothing the pack decides), the incident carries framing only, the harness passes through.
func TestAssembleUsesTheJudgesOwnFactsUntouched(t *testing.T) {
	row := judge.TriageRow{ExternalRef: "TG-a-1", Host: "web01", AlertRule: "R", Band: "ESCALATE", Outcome: "escalated", Proposed: false,
		Conclusion: "down", EvidenceIDs: []string{"lnms-dev-web01"}, SkillLoads: []string{"triage-protocol@1.1.0#290:store", "pack:proxmox"}}
	src := Source{Row: row, Steps: []trace.AgentStep{{ExternalRef: "TG-a-1", Cycle: 1, Tool: "get-device-status", Outcome: "success"}},
		Evidence: []trace.AgentStepEvidence{{ExternalRef: "TG-a-1", Cycle: 1, EvidenceID: "lnms-dev-web01", Tool: "get-device-status", Payload: "down"}},
		Harness:  map[string]string{"triage-protocol": "body"}}
	p := Assemble("live", src)
	if p.Incident.ExternalRef != "TG-a-1" || p.Incident.Host != "web01" || p.Incident.Severity != "" {
		t.Fatalf("incident framing: %+v", p.Incident)
	}
	if judge.Record(p.Deliverables[0].Record) != judge.Record(row.Facts()) || p.Deliverables[0].Record.Decisions != nil {
		t.Fatalf("the deliverable must be the judge's own Facts, untouched: %+v", p.Deliverables[0].Record)
	}
	if len(p.Rollouts) != 1 || len(p.Rollouts[0].Evidence) != 1 || p.Harness["triage-protocol"] != "body" || len(p.Pool) != 0 {
		t.Fatalf("rollout/harness must pass through, pool empty: %+v", p)
	}
}

// HarnessLoads agrees with judge.StoreVersionIDs — the ONE parser of the skill_loads grammar — on every input, and
// adds only the name: compiled-only, pack, precedent and shadow markers, malformed ids and `:storeX` tails are
// skipped by both; a trial-arm suffix keeps its id in both.
func TestHarnessLoadsAgreesWithTheStoreIDParser(t *testing.T) {
	inputs := []string{
		"triage-protocol@1.1.0#290:store", "base-prompt-guidance@21#300:store:trial7/arm1", "k8s-triage@1.1.1", "pack:proxmox",
		"precedent:librenms-dc1-1", "predicate-shadowed:k8s-triage", "bogus@1#notanumber:store", "x@1#0:store",
		"y@1#12abc:store", "z@1#7:storeX", "w@1#+5:store",
	}
	got := HarnessLoads(inputs)
	want := judge.StoreVersionIDs(inputs)
	// A nameless entry can never be a harness body, so HarnessLoads skips what StoreVersionIDs still counts.
	if nameless := HarnessLoads([]string{"#9:store", "@1#4:store"}); len(nameless) != 0 {
		t.Fatalf("nameless entries must be skipped, got %v", nameless)
	}
	ids := map[int64]bool{}
	for _, id := range want {
		ids[id] = true
	}
	if len(got) != len(ids) {
		t.Fatalf("HarnessLoads kept %d ids, StoreVersionIDs %d: %v vs %v", len(got), len(ids), got, want)
	}
	for name, id := range got {
		if !ids[id] {
			t.Fatalf("HarnessLoads kept %s=%d which StoreVersionIDs rejects", name, id)
		}
	}
	if got["triage-protocol"] != 290 || got["base-prompt-guidance"] != 300 {
		t.Fatalf("names must map to their ids: %v", got)
	}
}
