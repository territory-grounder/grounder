package httpapi

import (
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/ingest"
	"github.com/territory-grounder/grounder/modules/ingest/pveliveness"
)

// ONE RECORD CONSTRUCTOR, TWO CALLERS — and the reason it had to be exported.
//
// A1 (detection recall) asks: of the faults deliberately injected, how many did TG NOTICE? It answers by
// correlating each injected_fault against an ingest_alert row for the same host, inside a detection window,
// whose alert_rule matches the fault class.
//
// The pve-liveness poller detects a stopped guest by polling Proxmox in ~39 SECONDS — versus the ~6-11
// minute LibreNMS push it exists to beat — and mints its triage DIRECTLY through Temporal. It never passed
// through the HTTP front door, so it never produced an ingest_alert row, so A1 could not see it. TG's
// fastest detector scored ZERO on the metric it was built to raise; a guest-down it caught first was
// counted as a MISS unless the slow path later pushed the same alert and took the credit.
//
// These oracles pin the two things that make the fix real rather than cosmetic: the record carries the
// fields the correlation needs, and the rule name it carries is one the A1 mapping actually admits.

func livenessEnvelope() ingest.IncidentEnvelope {
	return ingest.IncidentEnvelope{
		ExternalRef: "tg-liveness-dc1mealie01-1785253986",
		SourceID:    "pve-liveness",
		AlertRule:   pveliveness.DeviceDownRule, // the PRODUCTION value, not a copy of it
		Host:        "dc1mealie01",
		Site:        "dc1",
		Summary:     "guest dc1mealie01 (vmid 101 on pve01) observed STOPPED by TG PVE liveness poller",
		ObservedAt:  time.Now().Add(-30 * time.Second).UTC(),
		ReceivedAt:  time.Now().UTC(),
	}
}

// TestRecordCarriesWhatDetectionRecallCorrelatesOn — host, rule and receipt time are the three fields the
// A1 query joins on. A record missing any of them is a row that cannot be credited.
func TestRecordCarriesWhatDetectionRecallCorrelatesOn(t *testing.T) {
	env := livenessEnvelope()
	rec := RecordFromEnvelope("pve-liveness", env, "tg/"+env.ExternalRef)

	if rec.Host != "dc1mealie01" {
		t.Errorf("host = %q — A1 correlates injected_fault.host against ingest_alert.host; without it the "+
			"detection can never be matched to the fault it caught", rec.Host)
	}
	if rec.AlertRule != pveliveness.DeviceDownRule {
		t.Errorf("alert_rule = %q — A1 is RULE-CLASS MATCHED, so a detection carrying the wrong rule is "+
			"counted as a miss even though the row exists", rec.AlertRule)
	}
	if rec.ReceivedAt.IsZero() {
		t.Error("received_at is zero — the detection window is measured from it, so a zero time puts the " +
			"detection outside every window")
	}
	if rec.ExternalRef != env.ExternalRef {
		t.Errorf("external_ref = %q, want the envelope's own ref", rec.ExternalRef)
	}
	if rec.WorkflowID == "" {
		t.Error("workflow_id is empty — the record must bind the detection to the triage it opened, or the " +
			"alert log cannot show that this detection actually started an investigation")
	}
}

// TestTheLivenessRuleIsOneTheA1MappingAdmits is the load-bearing one, and it is deliberately a DUPLICATE of
// the predicate in core/db/axis_read.go rather than a call into it.
//
// Recording a row is worthless if the rule it carries is not one the recall query accepts: the detection
// would still score as a miss, and the wire would look done while changing nothing. The mapping fails CLOSED
// by design (no match = miss), which is correct and must NOT be loosened to make a number move — so the
// check runs the other way: the rule the poller emits must already satisfy the mapping as it stands.
func TestTheLivenessRuleIsOneTheA1MappingAdmits(t *testing.T) {
	rec := RecordFromEnvelope("pve-liveness", livenessEnvelope(), "wf")

	// The device-down arm of detectRuleMatch: alert_rule ILIKE '%Device%Down%' OR '%ICMP%' OR '%SNMP%'.
	r := rec.AlertRule
	matches := containsFold(r, "device") && containsFold(r, "down")
	if !matches {
		t.Errorf("the liveness detector emits alert_rule %q, which the A1 device-down mapping "+
			"(ILIKE '%%Device%%Down%%' OR '%%ICMP%%' OR '%%SNMP%%') does NOT admit. Writing the row would "+
			"then change nothing: the detection still scores as a miss. Fix the emitted rule — do NOT widen "+
			"the mapping, which fails closed on purpose so a new class cannot silently inflate recall", r)
	}
}

func containsFold(s, sub string) bool {
	ls, lsub := []rune(s), []rune(sub)
	lower := func(rs []rune) string {
		out := make([]rune, len(rs))
		for i, r := range rs {
			if r >= 'A' && r <= 'Z' {
				r += 'a' - 'A'
			}
			out[i] = r
		}
		return string(out)
	}
	a, b := lower(ls), lower(lsub)
	for i := 0; i+len(b) <= len(a); i++ {
		if a[i:i+len(b)] == b {
			return true
		}
	}
	return false
}
