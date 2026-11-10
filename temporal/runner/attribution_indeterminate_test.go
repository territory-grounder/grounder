package runner

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/attribution"
	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/safety"
)

// INDETERMINATE ⇒ HOLD, FOR EVERY CLASS (TG-668, deep-review F8). TG-652 closed the gap for start-guest only:
// an unreadable PVE task log held the start-guest. Every other class kept REQ-2307's "unreadable == absent",
// so an admin's `systemctl stop` whose journal could not be read healed as a crash. The authoritative domain
// is now per class; a failed read of it is an explicit third state — indeterminate — that holds, while a
// positive reading of WHO (stand-down, security, self, the owner's authorized-test carve-out) still wins.

var errJournalDown = errors.New("journal: ssh dial timeout")

func attributeClass(t *testing.T, faultClass string, readers ...fakeActorReader) AttributeResult {
	t.Helper()
	return attributeWith(t, faultClass, readers...)
}

func TestUnreadableAuthoritativeReaderHoldsEveryClass(t *testing.T) {
	cases := []struct{ class, domain string }{
		{"start-service", "journal"}, {"restart-service", "journal"}, {"reload-service", "journal"},
		{"start-container", "journal"}, {"restart-container", "journal"},
		{"start-guest", "pve"}, {"stop-guest", "pve"},
		{"k8s-set-replicas", "k8s-audit"},
	}
	for _, c := range cases {
		res := attributeClass(t, c.class, fakeActorReader{domain: c.domain, err: errJournalDown})
		if !res.Escalate {
			t.Errorf("%s: a failed %s read must escalate (HOLD), got %+v", c.class, c.domain, res)
		}
		if !res.Finding.Indeterminate || len(res.Finding.IndeterminateDomains) != 1 || res.Finding.IndeterminateDomains[0] != c.domain {
			t.Errorf("%s: the finding must be marked indeterminate on %s, got %+v", c.class, c.domain, res.Finding)
		}
		if res.Finding.Taxonomy != attribution.Unattributable {
			t.Errorf("%s: indeterminacy is NOT a taxonomy — the taxonomy stays unattributable, got %s", c.class, res.Finding.Taxonomy)
		}
		if res.Security {
			t.Errorf("%s: a reader failure must never mint attributed-suspicious on its own (REQ-2307)", c.class)
		}
	}
}

// A NON-authoritative domain failing keeps REQ-2307's advisory posture, and a class with no authoritative
// witness keeps it on every failure.
func TestNonAuthoritativeFailuresStayAdvisory(t *testing.T) {
	if res := attributeClass(t, "start-service", fakeActorReader{domain: "pve", err: errPVE596}); res.Escalate || res.Finding.Indeterminate {
		t.Fatalf("pve is not the witness for a unit; a failed pve read must stay advisory, got %+v", res)
	}
	if res := attributeClass(t, "start-guest", fakeActorReader{domain: "journal", err: errJournalDown}); res.Escalate || res.Finding.Indeterminate {
		t.Fatalf("journal is not the witness for a guest; a failed journal read must stay advisory, got %+v", res)
	}
	if res := attributeClass(t, "disk-grow", fakeActorReader{domain: "pve", err: errPVE596}, fakeActorReader{domain: "journal", err: errJournalDown}); res.Escalate || res.Finding.Indeterminate {
		t.Fatalf("disk-grow has no authoritative witness; every failure stays advisory, got %+v", res)
	}
	if res := attributeClass(t, "", fakeActorReader{domain: "journal", err: errJournalDown}); res.Escalate {
		t.Fatalf("an empty fault class has no witness to miss, got %+v", res)
	}
}

// UNKNOWN is not INDETERMINATE: a reader that answered with nothing (no evidence, no coverage marker) is saying
// it saw nobody — the REQ-2303 pre-feature ladder, which heals. Only a reader that could not answer holds.
// ABSENT IS NOT CLEAR (owner prompt §2.F.6): an authoritative reader that answers with NO rows and NO coverage
// marker did not cover the subject (an unmonitored host, an unknown guest, an empty page). That is indeterminate,
// exactly like a failed read — never "nobody did this". A COVERED, actor-free answer (the marker) is the organic
// crash and heals; a NON-authoritative reader's empty answer is merely no evidence.
func TestAnEmptyAuthoritativeReadIsAbsentNotClear(t *testing.T) {
	res := attributeClass(t, "restart-container", fakeActorReader{domain: "journal"})
	if !res.Escalate || !res.Finding.Indeterminate || len(res.Finding.IndeterminateDomains) != 1 || res.Finding.IndeterminateDomains[0] != "journal:absent" {
		t.Fatalf("an authoritative reader answering with no rows and no marker is ABSENT ⇒ indeterminate ⇒ hold, got %+v", res)
	}
	// The organic crash: a readable, actor-free, COVERED journal heals.
	covered := fakeActorReader{domain: "journal", ev: []attribution.Evidence{attribution.CoverageMarker("journal", "web01", time.Now())}}
	if res := attributeClass(t, "restart-service", covered); res.Escalate || res.Finding.Indeterminate {
		t.Fatalf("a covered, actor-free journal is a crash and must heal, got %+v", res)
	}
	// A non-authoritative reader's empty answer is no evidence, not doubt.
	if res := attributeClass(t, "restart-service", covered, fakeActorReader{domain: "awx"}); res.Escalate || res.Finding.Indeterminate {
		t.Fatalf("an empty NON-authoritative read must not hold, got %+v", res)
	}
}

// STALE is not evidence about THIS change (REQ-2312): an admin's stop two hours ago, outside the 30-minute
// window, is discarded and the session is unattributable — never a stand-down, never a hold on its own. The
// intent that outlives the window is carried by the HOLD memory (TG-667), not by this reader.
func TestStaleEvidenceIsDiscardedNotTrusted(t *testing.T) {
	stale := fakeActorReader{domain: "pve", ev: []attribution.Evidence{
		{Domain: "pve", Actor: "root@pam", ActionKind: "vzstop", Target: "web01", ObservedAt: time.Now().Add(-2 * time.Hour), Ref: "UPID:old", Covered: true},
	}}
	res := attributeClass(t, "start-guest", stale)
	if res.StandDown || res.Escalate || res.Finding.Indeterminate || res.Finding.Taxonomy != attribution.Unattributable {
		t.Fatalf("evidence older than the window must be discarded (unattributable), got %+v", res)
	}
	fresh := fakeActorReader{domain: "pve", ev: []attribution.Evidence{
		{Domain: "pve", Actor: "root@pam", ActionKind: "vzstop", Target: "web01", ObservedAt: time.Now().Add(-2 * time.Minute), Ref: "UPID:new", Covered: true},
	}}
	if res := attributeClass(t, "start-guest", fresh); !res.StandDown {
		t.Fatalf("control: the same evidence inside the window is a stand-down, got %+v", res)
	}
}

// A POSITIVE reading of WHO outranks doubt: an admin's stop seen by the PVE reader keeps its stand-down even
// though the unit's authoritative journal read failed.
func TestAStrongerDispositionWinsOverIndeterminacy(t *testing.T) {
	admin := fakeActorReader{domain: "pve", ev: []attribution.Evidence{
		{Domain: "pve", Actor: "root@pam", ActionKind: "vzstop", Target: "web01", ObservedAt: time.Now().Add(-2 * time.Minute), Ref: "UPID:9", Covered: true},
	}}
	res := attributeClass(t, "start-service", fakeActorReader{domain: "journal", err: errJournalDown}, admin)
	if !res.StandDown || res.Escalate {
		t.Fatalf("an admin stop seen by another reader must stay a stand-down, got %+v", res)
	}
	if !res.Finding.Indeterminate {
		t.Fatalf("the indeterminacy is still RECORDED even when it does not decide: %+v", res.Finding)
	}
}

// THE CARVE-OUT DOES NOT CLEAR DOUBT (fresh-eyes review, 2026-11-07): a valid authorized-test match on one
// domain is recorded, but it is reached only when no reader showed a suspicious actor — and the failed
// authoritative reader showed nothing. So the unit's heal is HELD until its journal can be read, with the
// taxonomy kept honest in the audit row. The CONTROL shows the carve-out preserved where it is evidence: the
// same match with a readable, actor-free journal heals.
func TestAuthorizedTestCarveOutDoesNotClearIndeterminacy(t *testing.T) {
	now := time.Now()
	soak := fakeActorReader{domain: "pve", ev: []attribution.Evidence{
		{Domain: "pve", Actor: "root@pam!soak", ActionKind: "vzstop", Target: "web01", ObservedAt: now.Add(-2 * time.Minute), Ref: "UPID:soak", Covered: true},
	}}
	carve := attribution.CarveOut{
		ID: "soak-pool", Domain: "pve", Actors: []string{"root@pam!soak"}, Hosts: []string{"web01"},
		ValidFrom: now.Add(-time.Hour), ValidUntil: now.Add(time.Hour),
	}
	run := func(journal fakeActorReader) AttributeResult {
		deps := Deps{}
		attributeDeps(t, journal, soak)(&deps)
		deps.AttributionConfig.CarveOuts = append(deps.AttributionConfig.CarveOuts, carve)
		res, err := (&Activities{D: deps}).AttributeActivity(context.Background(), AttributeInput{Host: "web01", FaultClass: "start-service"})
		if err != nil {
			t.Fatal(err)
		}
		if res.Finding.Taxonomy != attribution.AuthorizedTest {
			t.Fatalf("control: the carve-out must match, got %s (%v)", res.Finding.Taxonomy, res.Finding.Warnings)
		}
		return res
	}
	held := run(fakeActorReader{domain: "journal", err: errJournalDown})
	if !held.Escalate || !held.Finding.Indeterminate {
		t.Fatalf("a carve-out match on another domain must not clear an unreadable authoritative domain, got %+v", held)
	}
	healed := run(fakeActorReader{domain: "journal", ev: []attribution.Evidence{attribution.CoverageMarker("journal", "web01", now)}})
	if healed.Escalate || healed.Finding.Indeterminate || healed.StandDown || healed.Security {
		t.Fatalf("control: the same carve-out with a readable journal heals, got %+v", healed)
	}
}

// TG-652 PARITY: a start-guest whose PVE task log cannot be read is held even when a carve-out actor's record
// on another domain resolves authorized-test. The rule TG-652 shipped held it whatever the taxonomy; the
// per-class rule must not be laxer for the one class that already had it.
func TestStartGuestWithUnreadablePVEIsHeldDespiteACarveOut(t *testing.T) {
	now := time.Now()
	soakAWX := fakeActorReader{domain: "awx", ev: []attribution.Evidence{
		{Domain: "awx", Actor: "soak-runner", ActionKind: "job", Target: "web01", ObservedAt: now.Add(-2 * time.Minute), Ref: "job/7", Covered: true},
	}}
	deps := Deps{}
	attributeDeps(t, fakeActorReader{domain: "pve", err: errors.New("pve: 503 from every endpoint")}, soakAWX)(&deps)
	deps.AttributionConfig.CarveOuts = append(deps.AttributionConfig.CarveOuts, attribution.CarveOut{
		ID: "soak-awx", Domain: "awx", Actors: []string{"soak-runner"}, Hosts: []string{"web01"},
		ValidFrom: now.Add(-time.Hour), ValidUntil: now.Add(time.Hour),
	})
	res, err := (&Activities{D: deps}).AttributeActivity(context.Background(), AttributeInput{Host: "web01", FaultClass: "start-guest"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Finding.Taxonomy != attribution.AuthorizedTest {
		t.Fatalf("control: the awx carve-out must match, got %s (%v)", res.Finding.Taxonomy, res.Finding.Warnings)
	}
	if !res.Escalate || !res.Finding.Indeterminate || res.Finding.IndeterminateDomains[0] != "pve" {
		t.Fatalf("an unreadable PVE task log holds a start-guest whatever another domain says (TG-652), got %+v", res)
	}
}

// The audit row says WHY: the classify step records attribution_indeterminate beside the taxonomy, and the
// fallback approver holds the escalation for a class it would otherwise act on.
func TestIndeterminacyIsRecordedAndHeld(t *testing.T) {
	a := &Activities{D: Deps{Ledger: audit.NewLedger(), AutonomousResolver: true, PredictionEligible: func(string) bool { return true }}}
	in := ClassifyInput{ExternalRef: "INC-5", ActionID: "a5", PlanHash: "p5", RiskLevel: "low", OpClass: "restart-service", Op: "restart",
		Reversible: true, Host: "web01", IncidentHost: "web01", AlertRule: "Service-up/down",
		Attribution: "unattributable", AttributionEscalate: true, AttributionIndeterminate: "journal"}
	dec, err := a.ClassifyActivity(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if dec.Band != safety.BandPollPause || dec.Signals["poll_reason"] != "actor-attribution-escalate" {
		t.Fatalf("an indeterminate attribution must poll, got %s %v", dec.Band, dec.Signals)
	}
	if dec.Signals["attribution_indeterminate"] != "journal" || !strings.Contains(dec.Signals["actor_attribution"], "unattributable") {
		t.Fatalf("the audit row must carry both the taxonomy and the unreadable domain: %v", dec.Signals)
	}
	if r := autonomousResolutionOf(dec); !r.Resolved || r.Approve {
		t.Fatalf("the fallback approver must HOLD an indeterminate attribution, got %+v", r)
	}
	control := in
	control.AttributionEscalate, control.AttributionIndeterminate = false, ""
	cdec, err := a.ClassifyActivity(context.Background(), control)
	if err != nil {
		t.Fatal(err)
	}
	if cdec.Band != safety.BandAuto {
		t.Fatalf("control: the same proposal with an answered read is AUTO, got %s %v", cdec.Band, cdec.Signals)
	}
	if _, ok := cdec.Signals["attribution_indeterminate"]; ok {
		t.Fatal("no attribution_indeterminate signal when every read answered")
	}
}
