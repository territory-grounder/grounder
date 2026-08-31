package suppression

import (
	"testing"
	"time"
)

// TG-389 — an empty host is not a matchable dedup key. The (host, alert_rule) match treats host=="" as equal
// to every other host=="", so a normalizer that could not resolve a machine would let the dedup stage
// suppress genuinely different hostless workloads as duplicates. Measured: seven distinct CiliumAgentNotReady
// nodes collapsed onto one empty key. A hostless alert must always escalate, never silently suppress.
func TestEmptyHostIsNeverADedupCandidate(t *testing.T) {
	now := sunday3am
	inWin := now.Add(-time.Minute)

	// A hostless in-window prior with the SAME rule: without the guard the second alert suppresses as a
	// "duplicate", which is the exact node-collapse this fixes.
	s := &DedupStage{
		Recent: []TriageEntry{{Host: "", AlertRule: "CiliumAgentNotReady", LoggedAt: inWin}},
		Window: time.Hour,
	}
	a := Alert{ExternalRef: "am-CiliumAgentNotReady-nodeB", Host: "", AlertRule: "CiliumAgentNotReady"}
	d, err := s.Evaluate(ctx(), a, now)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if d.Outcome != OutcomeEscalate {
		t.Fatalf("a hostless alert was SUPPRESSED (%v) — an empty host matches every other empty host, so "+
			"distinct hostless nodes/workloads collapse onto one dedup key (TG-389)", d.Outcome)
	}

	// NEGATIVE CONTROL. A RESOLVED host with a real in-window prior must still suppress — the guard rejects
	// only the empty key, it does not break dedup wholesale.
	s2 := &DedupStage{
		Recent: []TriageEntry{{Host: "web01", AlertRule: "R", LoggedAt: inWin}},
		Window: time.Hour,
	}
	if d2, _ := s2.Evaluate(ctx(), Alert{ExternalRef: "x", Host: "web01", AlertRule: "R"}, now); d2.Outcome != OutcomeSuppressed {
		t.Fatalf("a resolved-host duplicate must still suppress, got %v — the empty-host guard broke normal dedup", d2.Outcome)
	}
}
