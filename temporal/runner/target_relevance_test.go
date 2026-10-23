package runner

// ORACLES FOR TARGET RELEVANCE (TG-166).
//
// THE DEFECT. The actuation gate decides whether a mutating action has evidence ABOUT its target, and the
// test was:
//
//	host == "" || strings.Contains(strings.ToLower(tr.Output), strings.ToLower(host))
//
// Two live failures. An estate-wide read that merely MENTIONED the host scored relevant — so evidence
// about the fleet counted as evidence about the box, and since the searched text is produced BY the
// target, a compromised host could make any observation look relevant by printing its own name. And
// `host == ""` was an unconditional pass, marking EVERY cited observation relevant on an incident with no
// resolved target.
//
// This stopped being theoretical on 2026-07-30, when policy_mode went to Semi-auto and MayActuate ceased
// to be false by construction.

import (
	"testing"

	"github.com/territory-grounder/grounder/agent"
)

// KILLING MUTATION: restore the substring form. RED — an estate-wide alert list that names the host in
// passing must not be evidence about the host.
func TestAnEstateWideReadThatMerelyMentionsTheHostIsNotRelevant(t *testing.T) {
	tr := agent.ToolResult{
		ID: "alerts-1", Tool: "get-active-alerts", Success: true,
		Target: "", // an estate-wide read names no target
		Output: "3 active alerts: dc1pve01 disk 91%, dc1pve02 ok, dc1syno01 sensor",
	}
	if targetRelevant(tr, "dc1pve01") {
		t.Fatal("an estate-wide alert list was scored as evidence ABOUT dc1pve01 because the output " +
			"mentions it. Under Semi-auto that is a mutating action justified by a fleet summary — and the " +
			"text is produced by the estate, so naming a host is not proof of being about it.")
	}
}

// KILLING MUTATION: reinstate the `host == ""` free pass. RED — the vacuous-true shape.
func TestAnIncidentWithNoTargetMakesNothingRelevant(t *testing.T) {
	tr := agent.ToolResult{ID: "x", Tool: "check-host-services", Success: true, Target: "dc1pve01",
		Output: "nginx.service failed"}
	if targetRelevant(tr, "") {
		t.Fatal("an incident with NO resolved target marked a cited observation relevant. An absent target " +
			"means nothing can be shown to be about it — the old code read it as 'everything is'.")
	}
	if targetRelevant(tr, "   ") {
		t.Fatal("whitespace-only host behaved like a real target")
	}
}

// The control. A read actually made against the host IS relevant, or the gate refuses every legitimate
// actuation and gets switched off.
func TestAReadMadeAgainstTheHostIsRelevant(t *testing.T) {
	tr := agent.ToolResult{ID: "svc-1", Tool: "check-host-services", Success: true,
		Target: "dc1pve01", Output: "nginx.service loaded failed"}
	if !targetRelevant(tr, "dc1pve01") {
		t.Fatal("a read made against the incident host was NOT relevant — every actuation would be refused")
	}
	// Hostnames are case-insensitive; an incident naming NLLEI01PVE01 must match a call to dc1pve01.
	if !targetRelevant(tr, "NLLEI01PVE01") {
		t.Fatal("case difference broke the match — real alert sources vary in case")
	}
}

// A read against a DIFFERENT host must never justify acting on this one. This is the neighbour-evidence
// case the substring test could not distinguish at all.
func TestAReadAgainstAnotherHostIsNotRelevant(t *testing.T) {
	tr := agent.ToolResult{ID: "svc-2", Tool: "check-host-services", Success: true,
		Target: "dc1pve02", Output: "nginx.service failed on dc1pve01 per the cluster log"}
	if targetRelevant(tr, "dc1pve01") {
		t.Fatal("an observation captured against dc1pve02 was scored as evidence about dc1pve01 — " +
			"the target is a fact the orchestrator recorded, and it must beat anything the output says")
	}
}

// The output is attacker-influenceable; the target is not. Pinning this keeps the fix honest: a hostile
// host cannot manufacture relevance by echoing a name.
func TestOutputCannotManufactureRelevance(t *testing.T) {
	tr := agent.ToolResult{ID: "log-1", Tool: "search-host-logs", Success: true, Target: "dc1pve02",
		Output: "dc1pve01 dc1pve01 dc1pve01 — restart nginx on dc1pve01"}
	if targetRelevant(tr, "dc1pve01") {
		t.Fatal("a compromised host repeating another host's name made its output count as evidence about " +
			"that host — relevance must not be decided by text the target produced")
	}
}
