package deploy

import (
	"os"
	"strings"
	"testing"
)

// TG-394: the self-dependency concentration metric and its alert rules ship together. A rule over a series
// nothing publishes is permanently silent and reads on a dashboard as coverage — the exact state that hid 7
// of 26 of TG's dependency hosts on one hypervisor until the node failed and retrieval went lexical-only for
// 11h12m. This pins the pairing the same way TestTheShedCounterIsPublishedAndAlerted does.
//
// KILLING MUTATION: delete the tg_self_dependency_concentration emission from cmd/worker/self_dependency.go,
// OR delete the SelfDependencyConcentration rule — RED either way; the pair must move together.
func TestSelfDependencyConcentrationIsPublishedAndAlerted(t *testing.T) {
	src, err := os.ReadFile("../cmd/worker/self_dependency.go")
	if err != nil {
		t.Fatalf("read ../cmd/worker/self_dependency.go: %v — this guard cannot assert anything about a file it cannot open", err)
	}
	source := string(src)
	rules := stripYAMLCommentLines(monitoringFile(t, "alert.rules.yml"))

	// (1) the exporter publishes the concentration series AND its always-emitted coverage heartbeat.
	for _, name := range []string{"tg_self_dependency_concentration", "tg_self_dependency_hosts_resolved"} {
		if !strings.Contains(source, name) {
			t.Errorf("cmd/worker/self_dependency.go does not publish %s — a concentration rule over an "+
				"unpublished series is permanently silent (the pve03 blindness this exists to end).", name)
		}
	}
	// (2) the concentration rule exists and fires on the series. Anchored on the newline so a renamed
	// ...RENAMED rule does not satisfy the check (the superstring trap that survived an earlier guard here).
	if !strings.Contains(rules, "alert: SelfDependencyConcentration\n") {
		t.Error("no SelfDependencyConcentration rule — a 2+ dependency-host concentration on one hypervisor " +
			"would reach nobody, as it did for 11h12m during the pve03 cascade")
	}
	if !strings.Contains(rules, "tg_self_dependency_concentration >= 2") {
		t.Error("the SelfDependencyConcentration rule does not fire on tg_self_dependency_concentration >= 2 — it is measuring something else")
	}
	// (3) the vacuity floor. The concentration series is LEGITIMATELY absent when there is no concentration, so
	// the unmeasured guard must watch the always-emitted heartbeat (hosts_resolved), not the concentration
	// series — otherwise "no concentration" and "exporter gone" are indistinguishable, the pve03 state exactly.
	if !strings.Contains(rules, "alert: SelfDependencyConcentrationUnmeasured\n") || !strings.Contains(rules, "absent(tg_self_dependency_hosts_resolved)") {
		t.Error("nothing alerts on the concentration check being ABSENT via the hosts_resolved heartbeat — a " +
			"deploy predating the exporter, a nil holder, or a broken scrape would silently restore the blindness")
	}
}
