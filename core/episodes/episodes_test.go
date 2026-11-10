package episodes

import (
	"bytes"
	"encoding/csv"
	"strings"
	"testing"
	"time"
)

// TG-672 / REQ-2534 — the attribution rules, each as a hand-built shape. Every expected value is read off
// the fixture, never off the implementation.
//
// Host "guest-a" is the 1670/1675 shape from the 2026-11-07 evidence pack: a container-hang (F1) and, 41 minutes
// later, a device-down (F2) on the same guest. F1's restore deadline is 45 minutes after its injection, so the
// OLD host+window join (injection → deadline + 10 min) put F2's alert and the 18:29 start-guest inside BOTH
// windows and credited both faults with one execution. Non-overlapping ownership ends F1 at F2's injection.

var t0 = time.Date(2026, 11, 6, 17, 47, 0, 0, time.UTC)

func m(min int) time.Time { return t0.Add(time.Duration(min) * time.Minute) }

func fixture() Input {
	since, until := m(-60), m(180)
	return Input{
		Since: since, Until: until,
		SiteOf: func(host, _ string) string {
			if strings.HasPrefix(host, "gr-") {
				return "dc2"
			}
			return "dc1"
		},
		Faults: []Fault{
			{ID: 1670, Host: "guest-a", FaultType: "container-hang", InjectedAt: m(0), RestoreDueAt: m(45), RestoredAt: m(42), Virt: "lxc"},
			{ID: 1675, Host: "guest-a", FaultType: "device-down", InjectedAt: m(41), RestoreDueAt: m(86), RestoredAt: m(86), Virt: "lxc"},
			{ID: 1680, Host: "guest-b", FaultType: "service-down", InjectedAt: m(10), RestoreDueAt: m(55), RestoredAt: m(55), Virt: "qemu"},
			{ID: 1690, Host: "gr-guest-c", FaultType: "service-down", InjectedAt: m(20), RestoreDueAt: m(65), RestoredAt: m(30)},
			// Injected BEFORE the window: it owns the first in-window alert on guest-d.
			{ID: 1600, Host: "guest-d", FaultType: "device-down", InjectedAt: m(-70), RestoreDueAt: m(-25)},
			// Injected AT until: outside the cohort.
			{ID: 1700, Host: "guest-e", FaultType: "device-down", InjectedAt: until, RestoreDueAt: until.Add(45 * time.Minute)},
		},
		Alerts: []Alert{
			{ExternalRef: "a-hang", Host: "guest-a", Rule: "Service up/down", ReceivedAt: m(6)},
			// F2's alert: inside F1's raw window (0..55) AND F2's ownership (41..) — owned by F2, flagged ambiguous.
			{ExternalRef: "a-down", Host: "guest-a", Rule: "Device Down (SNMP unreachable)", ReceivedAt: m(42)},
			// One-to-many on gr-guest-c: two sources report the same fault.
			{ExternalRef: "c-svc-1", Host: "gr-guest-c", Rule: "Service up/down", ReceivedAt: m(23)},
			{ExternalRef: "c-svc-2", Host: "gr-guest-c", Rule: "HTTP probe failed", ReceivedAt: m(24)},
			// Wrong class during F3's ownership on gr-guest-c: owned, dropped, never attributed.
			{ExternalRef: "c-wrong", Host: "gr-guest-c", Rule: "Devices up/down", ReceivedAt: m(25)},
			// guest-d: in-window alert owned by the pre-window fault 1600 (ownership runs to -25+10 = -15 … no:
			// 1600's own_until is m(-15), so an alert at m(-20) is owned; one at m(5) is NOT.
			{ExternalRef: "d-early", Host: "guest-d", Rule: "Device-Down", ReceivedAt: m(-20)},
			{ExternalRef: "d-organic", Host: "guest-d", Rule: "Device-Down", ReceivedAt: m(5)},
			// An alert on a host with no fault at all.
			{ExternalRef: "z-organic", Host: "guest-z", Rule: "Service up/down", ReceivedAt: m(30)},
		},
		Executions: []Execution{
			{ID: 1, ExternalRef: "a-down", OpClass: "start-guest", ExecutedAt: m(42).Add(5 * time.Second), Verdict: "match", Site: "dc1"},
			{ID: 2, ExternalRef: "c-svc-2", OpClass: "start-service", ExecutedAt: m(27), Verdict: "match", Site: "dc2"},
			{ID: 3, ExternalRef: "d-early", OpClass: "start-guest", ExecutedAt: m(-18), Verdict: "match", Site: "dc1"},
			{ID: 4, ExternalRef: "d-organic", OpClass: "start-guest", ExecutedAt: m(7), Verdict: "match", Site: "dc1"},
			{ID: 5, ExternalRef: "c-wrong", OpClass: "start-guest", ExecutedAt: m(26), Verdict: "match", Site: "dc2"},
			{ID: 6, ExternalRef: "no-such-alert", OpClass: "start-guest", ExecutedAt: m(50), Verdict: "match", Site: "dc1"},
		},
		Credits: []Credit{
			{ExternalRef: "a-down", OpClass: "start-guest", Outcome: "verified_clean", CreditedAt: m(43)},
			{ExternalRef: "c-svc-1", OpClass: "start-service", Outcome: "verified_clean", CreditedAt: m(28)},
		},
	}
}

func byID(t *testing.T, r Report, id int64) Episode {
	t.Helper()
	for _, e := range r.Episodes {
		if e.ID == id {
			return e
		}
	}
	t.Fatalf("fault %d not in the cohort episodes", id)
	return Episode{}
}

// The counterexample from the pack: one execution, two overlapping faults, exactly one owner. The pair overlaps
// (1675 was injected inside 1670's raw window), and the EVIDENCE separates it (TG-679): 1675's alert is a device
// rule a container-hang cannot raise, and its execution is start-guest — device-down's healing op, not
// container-hang's restart-container. So the truncation stands: 1670 is alerted-no-exec, 1675 credited.
func TestOneExecutionIsCreditedToExactlyOneFault(t *testing.T) {
	r := Attribute(fixture())
	f1, f2 := byID(t, r, 1670), byID(t, r, 1675)
	if len(f1.Executions) != 0 || len(f1.Credits) != 0 || f1.Disposition != DispositionAlertedNoExec {
		t.Fatalf("1670 (container-hang) must own only its own Service alert and NO execution; got execs=%d credits=%d disposition=%s",
			len(f1.Executions), len(f1.Credits), f1.Disposition)
	}
	if len(f1.Alerts) != 1 || f1.Alerts[0].ExternalRef != "a-hang" || !f1.OwnUntil.Equal(m(41)) {
		t.Fatalf("1670 must own exactly a-hang and end at 1675's injection (m+41), got %+v ownUntil=%s", f1.Alerts, f1.OwnUntil)
	}
	if len(f2.Executions) != 1 || f2.Executions[0].ID != 1 || len(f2.Credits) != 1 || f2.Disposition != DispositionCredited {
		t.Fatalf("1675 (device-down) must own the start-guest and its credit; got %+v", f2)
	}
	if f1.Ambiguous || f2.Ambiguous || f2.Alerts[0].RawOverlaps != 1 {
		t.Fatalf("the overlap is separated by evidence, not ambiguous (ambiguous 1670=%v 1675=%v; 1675's alert raw overlaps=%d, want 1)", f1.Ambiguous, f2.Ambiguous, f2.Alerts[0].RawOverlaps)
	}
	if len(r.Overlaps) != 1 || r.Overlaps[0].A != 1670 || r.Overlaps[0].B != 1675 || !r.Overlaps[0].Separated ||
		!strings.Contains(r.Overlaps[0].Rule, "op class separates") || !strings.Contains(r.Overlaps[0].Rule, "start-guest") {
		t.Fatalf("overlaps = %+v, want the one pair 1670→1675 separated by op class", r.Overlaps)
	}
	if !f2.ExecBeforeDue || !f2.ExecBeforeReconcile {
		t.Fatalf("1675 executed at +42m05s, before its +86m deadline and its +86m restore: before-due=%v before-reconcile=%v", f2.ExecBeforeDue, f2.ExecBeforeReconcile)
	}
}

// MUTATION CONTROL (the rule, not the instance): with the OLD rule — ownership to the restore deadline plus the
// grace, no cut at the next injection — 1670 would own a-down and the execution too. The fixture distinguishes
// the two rules by construction; this pins which one shipped.
func TestOwnershipEndsAtTheNextInjectionNotTheDeadline(t *testing.T) {
	in := fixture()
	sorted := append([]Fault(nil), in.Faults...)
	// ownUntil for 1670 must be 1675's injection (+41m), not its own deadline + grace (+55m).
	for i := range sorted {
		if sorted[i].ID == 1670 {
			// sort order inside Attribute is by host then time; emulate by passing the two guest-a faults in order.
			got := ownUntil([]Fault{sorted[i], in.Faults[1]}, 0)
			if !got.Equal(m(41)) {
				t.Fatalf("ownUntil(1670) = %s, want the next injection at %s (the deadline+grace %s is the OLD, double-attributing rule)",
					got, m(41), m(55))
			}
		}
	}
	// A fault with no successor owns to its deadline + grace; one with neither owns open-endedly.
	if got := ownUntil([]Fault{in.Faults[2]}, 0); !got.Equal(m(65)) {
		t.Fatalf("a last fault owns to deadline+grace (+65m), got %s", got)
	}
	if got := ownUntil([]Fault{{Host: "x", InjectedAt: m(0)}}, 0); got.Year() != 9999 {
		t.Fatalf("a fault with no deadline and no successor must own open-endedly, got %s", got)
	}
}

func TestOneToManyAlertsAndWrongClassDrops(t *testing.T) {
	r := Attribute(fixture())
	c := byID(t, r, 1690)
	if len(c.Alerts) != 2 || c.DroppedWrongClass != 1 {
		t.Fatalf("gr-guest-c: two service alerts owned, the Devices up/down one dropped as wrong class; got alerts=%d dropped=%d", len(c.Alerts), c.DroppedWrongClass)
	}
	if len(c.Executions) != 1 || c.Executions[0].ID != 2 || len(c.Credits) != 1 || c.Credits[0].ExternalRef != "c-svc-1" {
		t.Fatalf("the execution rides c-svc-2 and the credit rides c-svc-1 — both the fault's; got %+v", c)
	}
	if c.Site != "dc2" || c.Virt != "" || c.ExecBeforeReconcile != true {
		t.Fatalf("site/virt/reconcile: %q %q %v, want dc2, unknown virt, executed before the +30m restore", c.Site, c.Virt, c.ExecBeforeReconcile)
	}
	// Execution 5 answered the wrong-class alert: not the fault's, reported with the reason.
	var seen bool
	for _, u := range r.UnownedExecutions {
		if u.ID == 5 {
			seen = true
			if u.Reason != DispositionWrongClassOwner {
				t.Fatalf("execution 5 reason = %q, want %q", u.Reason, DispositionWrongClassOwner)
			}
		}
	}
	if !seen {
		t.Fatal("execution 5 (on a dropped wrong-class alert) vanished instead of being listed")
	}
}

func TestUnmatchedFaultsAndUnownedExecutionsStayVisible(t *testing.T) {
	r := Attribute(fixture())
	if b := byID(t, r, 1680); b.Disposition != DispositionNoAlert || len(b.Alerts) != 0 {
		t.Fatalf("guest-b never alerted: disposition %q, want no-alert (it stays in the denominators)", b.Disposition)
	}
	if len(r.Episodes) != 4 {
		t.Fatalf("cohort = %d faults, want 4 (1670, 1675, 1680, 1690; 1600 is before the window, 1700 AT until)", len(r.Episodes))
	}
	// 1600 owns d-early (-20m) and execution 3: a pre-window owner of in-window evidence, listed, not credited to the cohort.
	if len(r.OwnersOutside) != 1 || r.OwnersOutside[0].ID != 1600 || len(r.OwnersOutside[0].Executions) != 1 {
		t.Fatalf("owners outside the window = %+v, want 1600 with execution 3", r.OwnersOutside)
	}
	reasons := map[int64]string{}
	for _, u := range r.UnownedExecutions {
		reasons[u.ID] = u.Reason
	}
	if reasons[3] != DispositionOwnerOutside || reasons[4] != DispositionUnattributed || reasons[6] != DispositionUnattributed || len(reasons) != 4 {
		t.Fatalf("unowned executions = %v, want 3=owner-outside-window, 4 and 6=unattributed, 5=owner-wrong-class", reasons)
	}
	s := r.Summary
	if s.Total.Faults != 4 || s.Total.Alerted != 3 || s.Total.NoAlert != 1 || s.Total.Executed != 2 || s.Total.Credited != 2 ||
		s.Total.ExecutedNoCredit != 0 || s.Total.Ambiguous != 0 || s.Total.Executions != 2 {
		t.Fatalf("summary total = %+v", s.Total)
	}
	if s.ExecutionsAttributed != 2 || s.ExecutionsOwnerOutside != 1 || s.ExecutionsOwnerWrongClass != 1 || s.ExecutionsUnattributed != 2 ||
		s.AlertsOwnedWrongClass != 1 || s.AlertsUnowned != 2 || s.CreditsAttributed != 2 || s.CreditsAmbiguous != 0 || s.CreditsOtherOutcome != 0 || s.CreditsUnowned != 0 ||
		s.OverlapPairs != 1 || s.OverlapsSeparated != 1 || s.OverlapsAmbiguous != 0 {
		t.Fatalf("summary executions/credits/alerts/overlaps = %+v", s)
	}
	if s.BySite["dc1"].Faults != 3 || s.BySite["dc2"].Faults != 1 || s.ByVirt["lxc"].Faults != 2 || s.ByVirt["qemu"].Faults != 1 || s.ByVirt["unknown"].Faults != 1 {
		t.Fatalf("by site/virt = %v / %v", s.BySite, s.ByVirt)
	}
	if c := s.BySiteFaultTypeVirt["dc1/device-down/lxc"]; c.Credited != 1 || c.Ambiguous != 0 || s.MaxGuestShareOfExecutions["dc1"] != "1/1 (guest-a)" {
		t.Fatalf("coverage matrix / guest share = %v / %v", s.BySiteFaultTypeVirt, s.MaxGuestShareOfExecutions)
	}
}

func TestClassFolding(t *testing.T) {
	for _, c := range []struct {
		in   string
		want Class
	}{{"device-down", ClassGuest}, {"service-down", ClassService}, {"container-down", ClassService}, {"container-hang", ClassService},
		{"service-hang", ClassService}, {"disk-fill", ClassDisk}, {"log-fill", ClassDisk}, {"mem-pressure", ClassMemory}, {"frobnicate", ClassOther}} {
		if got := FaultClass(c.in); got != c.want {
			t.Errorf("FaultClass(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	for _, c := range []struct {
		in   string
		want Class
	}{{"Device Down (SNMP unreachable)", ClassGuest}, {"Devices up/down", ClassGuest}, {"Device-Down-Due-to-no-ICMP-response.", ClassGuest},
		{"Service up/down", ClassService}, {"HTTP probe failed", ClassService}, {"nginx down", ClassService}, {"Disk space low", ClassDisk},
		{"Memory utilisation high", ClassMemory}, {"Port status", ClassOther}} {
		if got := RuleClass(c.in); got != c.want {
			t.Errorf("RuleClass(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// A missing mapping fails CLOSED: two unknowns never match each other (core/db.detectRuleMatch's rule).
	if classesMatch(ClassOther, ClassOther) || !classesMatch(ClassDisk, ClassDisk) || classesMatch(ClassDisk, ClassGuest) {
		t.Fatal("classesMatch must refuse ClassOther on either side and require equality otherwise")
	}
}

// An unknown fault class cannot claim an unknown rule, and a known class cannot claim an unknown rule: both are
// dropped as wrong-class, never attributed.
func TestUnknownClassesFailClosed(t *testing.T) {
	in := fixture()
	in.Faults = append(in.Faults,
		Fault{ID: 1800, Host: "guest-f", FaultType: "frobnicate", InjectedAt: m(10), RestoreDueAt: m(55)},
		Fault{ID: 1801, Host: "guest-g", FaultType: "disk-fill", InjectedAt: m(10), RestoreDueAt: m(55)})
	in.Alerts = append(in.Alerts,
		Alert{ExternalRef: "f-port", Host: "guest-f", Rule: "Port status", ReceivedAt: m(12)},
		Alert{ExternalRef: "g-port", Host: "guest-g", Rule: "Port status", ReceivedAt: m(12)},
		Alert{ExternalRef: "g-disk", Host: "guest-g", Rule: "Disk space low", ReceivedAt: m(13)})
	r := Attribute(in)
	f, g := byID(t, r, 1800), byID(t, r, 1801)
	if len(f.Alerts) != 0 || f.DroppedWrongClass != 1 || f.Disposition != DispositionNoAlert {
		t.Fatalf("an unknown fault class claimed an unknown rule: %+v", f)
	}
	if len(g.Alerts) != 1 || g.Alerts[0].ExternalRef != "g-disk" || g.DroppedWrongClass != 1 {
		t.Fatalf("disk-fill must own the disk alert and drop the port alert: %+v", g)
	}
}

// A fault injected AFTER the window still ends the last in-window fault's ownership — otherwise the right edge
// of the window re-creates the double attribution at the left edge the lookback already prevents.
func TestASuccessorAfterTheWindowEndsTheLastOwnership(t *testing.T) {
	in := fixture()
	until := in.Until
	in.Faults = append(in.Faults,
		Fault{ID: 1900, Host: "guest-h", FaultType: "device-down", InjectedAt: until.Add(-5 * time.Minute), RestoreDueAt: until.Add(40 * time.Minute)},
		Fault{ID: 1901, Host: "guest-h", FaultType: "device-down", InjectedAt: until.Add(5 * time.Minute), RestoreDueAt: until.Add(50 * time.Minute)})
	in.Alerts = append(in.Alerts, Alert{ExternalRef: "h-down", Host: "guest-h", Rule: "Device-Down", ReceivedAt: until.Add(6 * time.Minute)})
	in.Executions = append(in.Executions, Execution{ID: 9, ExternalRef: "h-down", OpClass: "start-guest", ExecutedAt: until.Add(7 * time.Minute), Verdict: "match"})
	r := Attribute(in)
	if last := byID(t, r, 1900); len(last.Alerts) != 0 || len(last.Executions) != 0 || !last.OwnUntil.Equal(until.Add(5*time.Minute)) {
		t.Fatalf("the last in-window fault must end at its successor's injection and own nothing after it: %+v", last)
	}
	// Execution 9 happened AFTER the window: it is attached to its owner 1901 (listed beside the cohort as an owner
	// outside the window) and is NOT listed as the window's unowned evidence — nothing inside the window lost it.
	var owner *Episode
	for i := range r.OwnersOutside {
		if r.OwnersOutside[i].ID == 1901 {
			owner = &r.OwnersOutside[i]
		}
	}
	if owner == nil || len(owner.Executions) != 1 || owner.Executions[0].ID != 9 {
		t.Fatalf("execution 9 must ride on its post-window owner 1901, got %+v", owner)
	}
	for _, u := range r.UnownedExecutions {
		if u.ID == 9 {
			t.Fatalf("execution 9 (after the window) was listed as the window's unowned evidence: %+v", u)
		}
	}
}

// The note prefix selects what is REPORTED; it never narrows ownership. A same-host fault from another campaign
// ends the cohort fault's interval and keeps its own evidence.
func TestTheCampaignPrefixSelectsTheCohortNotTheOwnership(t *testing.T) {
	in := fixture()
	in.NotePrefix = "soak"
	for i := range in.Faults {
		in.Faults[i].Note = "soak 2026-11"
	}
	// A drill on guest-b, 20 minutes after the soak's fault: it ends 1680's ownership and owns the alert at +32m.
	in.Faults = append(in.Faults, Fault{ID: 1950, Host: "guest-b", FaultType: "service-down", Note: "drill", InjectedAt: m(30), RestoreDueAt: m(75)})
	in.Alerts = append(in.Alerts, Alert{ExternalRef: "b-svc", Host: "guest-b", Rule: "Service up/down", ReceivedAt: m(32)})
	in.Executions = append(in.Executions, Execution{ID: 10, ExternalRef: "b-svc", OpClass: "start-service", ExecutedAt: m(33), Verdict: "match"})
	r := Attribute(in)
	if len(r.Episodes) != 4 {
		t.Fatalf("cohort = %d, want the 4 soak faults only", len(r.Episodes))
	}
	if b := byID(t, r, 1680); len(b.Alerts) != 0 || !b.OwnUntil.Equal(m(30)) {
		t.Fatalf("1680 must end at the drill's injection (+30m) and own nothing after it: %+v", b)
	}
	var drill *Episode
	for i := range r.OwnersOutside {
		if r.OwnersOutside[i].ID == 1950 {
			drill = &r.OwnersOutside[i]
		}
	}
	if drill == nil || len(drill.Executions) != 1 {
		t.Fatalf("the drill must be listed beside the cohort with its execution: %+v", drill)
	}
	for _, u := range r.UnownedExecutions {
		if u.ID == 10 && u.Reason != DispositionOwnerOtherCamp {
			t.Fatalf("execution 10 reason = %q, want owner-outside-campaign", u.Reason)
		}
	}
}

// Only a verified_clean credit is a credit; any other outcome is carried, visible and uncounted.
func TestOnlyVerifiedCleanCountsAsACredit(t *testing.T) {
	in := fixture()
	for i := range in.Credits {
		if in.Credits[i].ExternalRef == "c-svc-1" {
			in.Credits[i].Outcome = "collateral"
		}
	}
	r := Attribute(in)
	c := byID(t, r, 1690)
	if len(c.Credits) != 0 || len(c.OtherCredits) != 1 || c.Disposition != DispositionExecutedNoCred {
		t.Fatalf("a non-verified credit must not credit the fault: credits=%d other=%d disposition=%s", len(c.Credits), len(c.OtherCredits), c.Disposition)
	}
	if r.Summary.Total.Credited != 1 || r.Summary.CreditsOtherOutcome != 1 || r.Summary.Total.ExecutedNoCredit != 1 {
		t.Fatalf("summary = %+v", r.Summary.Total)
	}
	// A credit keyed to an alert nobody owns is listed, not dropped.
	in.Credits = append(in.Credits, Credit{ExternalRef: "z-organic", OpClass: "start-service", Outcome: "verified_clean", CreditedAt: m(40)})
	if r := Attribute(in); r.Summary.CreditsUnowned != 1 || len(r.UnownedCredits) != 1 || r.UnownedCredits[0].Reason != DispositionUnattributed {
		t.Fatalf("an unowned credit vanished: %+v", r.UnownedCredits)
	}
}

func TestCSVCarriesEveryFaultAndTheFlaggedOwnersOutside(t *testing.T) {
	r := Attribute(fixture())
	var buf bytes.Buffer
	if err := WriteCSV(&buf, r); err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(&buf).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1+4+1 || strings.Join(rows[0], ",") != strings.Join(CSVHeader, ",") {
		t.Fatalf("csv rows = %d, want header + 4 cohort + 1 owner-outside; header ok=%v", len(rows), strings.Join(rows[0], ",") == strings.Join(CSVHeader, ","))
	}
	col := map[string]int{}
	for i, h := range rows[0] {
		col[h] = i
	}
	var saw1670, saw1600 bool
	for _, row := range rows[1:] {
		switch row[col["fault_id"]] {
		case "1670":
			saw1670 = true
			if row[col["disposition"]] != DispositionAlertedNoExec || row[col["ambiguous_with"]] != "" || row[col["n_exec"]] != "0" || row[col["in_cohort"]] != "true" {
				t.Errorf("1670 row = %v", row)
			}
		case "1600":
			saw1600 = true
			if row[col["in_cohort"]] != "false" || row[col["n_exec"]] != "1" {
				t.Errorf("1600 (owner outside the window) row = %v", row)
			}
		}
	}
	if !saw1670 || !saw1600 {
		t.Fatal("csv lost a row")
	}
	if txt := r.Text(); !strings.Contains(txt, "owner-outside-window-or-campaign=1") || !strings.Contains(txt, "execution 3 ") {
		t.Fatalf("text summary lacks the unowned executions:\n%s", txt)
	}
}

func TestDefaultSiteOfRefusesToGuess(t *testing.T) {
	if DefaultSiteOf("dc2x", "dc1pve") != "" || DefaultSiteOf("x", "dc2pve") != "dc2" || DefaultSiteOf("dc1x", "") != "dc1" || DefaultSiteOf("x", "") != "" {
		t.Fatal("DefaultSiteOf must mirror core/db.faultSite: node first, host second, conflict or neither = unattributed")
	}
}

// Evidence the loader read OUTSIDE the window (its lookback) is attached to its owner for attribution but never
// LISTED as the window's unowned evidence: on 2026-11-07 the live report listed five pre-window heals and five
// pre-window credits beside the two in-window ones it was asked about.
func TestOnlyInWindowEvidenceIsListedAsUnowned(t *testing.T) {
	in := fixture()
	// Execution 3 (m-18) and its credit are inside the window [m-60, m+180) and owned by pre-window fault 1600: listed.
	// Add a heal and a credit BEFORE the window, keyed to the same pre-window alert: attached to 1600, not listed.
	in.Executions = append(in.Executions, Execution{ID: 99, ExternalRef: "d-early", OpClass: "start-guest", ExecutedAt: m(-65), Verdict: "match", Site: "dc1"})
	in.Credits = append(in.Credits, Credit{ExternalRef: "d-early", OpClass: "start-guest", Outcome: "verified_clean", CreditedAt: m(-64)},
		Credit{ExternalRef: "d-early", OpClass: "start-guest", Outcome: "verified_clean", CreditedAt: m(-17)})
	r := Attribute(in)
	listed := map[int64]bool{}
	for _, u := range r.UnownedExecutions {
		listed[u.ID] = true
	}
	if !listed[3] || listed[99] {
		t.Fatalf("unowned executions listed = %v, want execution 3 (in window) and NOT 99 (before the window)", listed)
	}
	if len(r.UnownedCredits) != 1 || !r.UnownedCredits[0].CreditedAt.Equal(m(-17)) {
		t.Fatalf("unowned credits = %+v, want only the in-window one at m-17", r.UnownedCredits)
	}
	var owner Episode
	for _, o := range r.OwnersOutside {
		if o.ID == 1600 {
			owner = o
		}
	}
	if len(owner.Executions) != 2 || len(owner.Credits) != 2 {
		t.Fatalf("the pre-window owner must still carry everything keyed to its alert (execs=%d credits=%d)", len(owner.Executions), len(owner.Credits))
	}
	if r.Summary.ExecutionsOwnerOutside != 1 || r.Summary.CreditsUnowned != 1 {
		t.Fatalf("summary counts the LISTED unowned evidence only: execs-outside=%d credits-unowned=%d", r.Summary.ExecutionsOwnerOutside, r.Summary.CreditsUnowned)
	}
}

// TG-679 — OVERLAP SEPARATION (pre-registration v1 §5). A pair overlaps when B is injected inside A's raw window;
// the ownership cut stands only where evidence separates the two faults, else both are ambiguous (in every
// denominator, never credited, the credit disclosed). Every case is one shape on one guest, A injected at m0 with
// its restore due at m45 (raw window [m0, m55)), B injected at m48 unless stated; every expectation is read off
// the shape.
func TestOverlapSeparation(t *testing.T) {
	type shape struct {
		name               string
		aType, bType       string
		aRestored          int
		bInjected          int
		aDueZero           bool
		alerts             []Alert
		execs              []Execution
		wantOverlap        bool
		wantSeparated      bool
		wantA, wantB       string
		wantRule           string
		wantCredited       int
		wantAmbiguousCreds int
		noCreditFor        string
		noCredits          bool
		creditAt           map[string]int
	}
	al := func(ref, rule string, observed, received int) Alert {
		a := Alert{ExternalRef: ref, Host: "g", Rule: rule, ReceivedAt: m(received)}
		if observed >= 0 {
			a.ObservedAt = m(observed)
		}
		return a
	}
	ex := func(id int64, ref, op string, at int) Execution {
		return Execution{ID: id, ExternalRef: ref, OpClass: op, ExecutedAt: m(at), Verdict: "match"}
	}
	cases := []shape{
		{name: "op class separates a same-family pair (container-down then container-hang, restart-container)",
			aType: "container-down", bType: "container-hang", aRestored: 45, bInjected: 48,
			alerts: []Alert{al("a", "Service up/down", 2, 9), al("b", "Service up/down", 52, 58)}, execs: []Execution{ex(7, "b", "restart-container", 59)},
			wantOverlap: true, wantSeparated: true, wantA: DispositionAlertedNoExec, wantB: DispositionCredited, wantRule: "op class separates", wantCredited: 1},
		{name: "A acted on first separates even a same-type pair",
			aType: "service-down", bType: "service-down", aRestored: 45, bInjected: 48,
			alerts: []Alert{al("a", "Service up/down", 2, 9), al("b", "Service up/down", 52, 58)}, execs: []Execution{ex(6, "a", "start-service", 10), ex(7, "b", "start-service", 59)},
			wantOverlap: true, wantSeparated: true, wantA: DispositionCredited, wantB: DispositionCredited, wantRule: "follows A's execution 6 (start-service), verified clean before B", wantCredited: 2},
		{name: "A's healing op ran first but was never verified: not a resolution — ambiguous",
			aType: "service-down", bType: "service-down", aRestored: 45, bInjected: 48,
			alerts: []Alert{al("a", "Service up/down", 2, 9), al("b", "Service up/down", 52, 58)}, execs: []Execution{ex(6, "a", "start-service", 10), ex(7, "b", "start-service", 59)},
			noCreditFor: "a",
			wantOverlap: true, wantSeparated: false, wantA: DispositionAmbiguous, wantB: DispositionAmbiguous, wantRule: "not verified healed", wantAmbiguousCreds: 1},
		{name: "A's credit verified AFTER B was injected, op class separates — stands",
			aType: "service-down", bType: "service-hang", aRestored: 45, bInjected: 48,
			alerts: []Alert{al("a", "Service up/down", 2, 9), al("b", "Service up/down", 52, 58)}, execs: []Execution{ex(6, "a", "start-service", 30), ex(7, "b", "restart-service", 59)},
			creditAt:    map[string]int{"a": 49},
			wantOverlap: true, wantSeparated: true, wantA: DispositionCredited, wantB: DispositionCredited, wantRule: "verified after B was injected and is A's healing op", wantCredited: 2},
		{name: "A's credit verified AFTER B was injected, same type — ambiguous",
			aType: "service-down", bType: "service-down", aRestored: 45, bInjected: 48,
			alerts: []Alert{al("a", "Service up/down", 2, 9)}, execs: []Execution{ex(6, "a", "start-service", 30)},
			creditAt:    map[string]int{"a": 49},
			wantOverlap: true, wantSeparated: false, wantA: DispositionAmbiguous, wantB: DispositionAmbiguous, wantRule: "after B was injected, and its op class does not separate", wantAmbiguousCreds: 1},
		{name: "A's own execution after B was injected, A's healing op — separated",
			aType: "service-down", bType: "service-hang", aRestored: 45, bInjected: 48,
			alerts: []Alert{al("a", "Service up/down", 2, 9)}, execs: []Execution{ex(6, "a", "start-service", 50)},
			wantOverlap: true, wantSeparated: true, wantA: DispositionCredited, wantB: DispositionNoAlert, wantRule: "A's healing op, not B's", wantCredited: 1},
		{name: "A's own execution after B was injected, B's healing op — ambiguous",
			aType: "service-down", bType: "service-hang", aRestored: 45, bInjected: 48,
			alerts: []Alert{al("a", "Service up/down", 2, 9)}, execs: []Execution{ex(6, "a", "restart-service", 50)},
			wantOverlap: true, wantSeparated: false, wantA: DispositionAmbiguous, wantB: DispositionAmbiguous, wantRule: "ran after B was injected and its op class does not separate", wantAmbiguousCreds: 1},
		{name: "A of a type with no healing op (disk-fill): B's start-service cannot be told apart — ambiguous",
			aType: "disk-fill", bType: "service-down", aRestored: 45, bInjected: 48,
			alerts: []Alert{al("b", "Service up/down", 52, 58)}, execs: []Execution{ex(7, "b", "start-service", 59)},
			wantOverlap: true, wantSeparated: false, wantA: DispositionAmbiguous, wantB: DispositionAmbiguous, wantRule: "is not B's healing op", wantAmbiguousCreds: 1},
		{name: "A of an unknown type could raise any alert — fails closed",
			aType: "novel-fault", bType: "device-down", aRestored: 45, bInjected: 48,
			alerts:      []Alert{al("b", "Device Down (SNMP unreachable)", -1, 50)},
			wantOverlap: true, wantSeparated: false, wantA: DispositionAmbiguous, wantB: DispositionAmbiguous, wantRule: "observed before B was injected (or its observation time is unknown)"},
		{name: "A acted on first with ANOTHER op (not its healing op, no credit): not a resolution — ambiguous",
			aType: "service-hang", bType: "service-hang", aRestored: 45, bInjected: 48,
			alerts: []Alert{al("a", "Service up/down", 2, 9), al("b", "Service up/down", 52, 58)}, execs: []Execution{ex(6, "a", "start-service", 10), ex(7, "b", "restart-service", 59)},
			noCreditFor: "a",
			wantOverlap: true, wantSeparated: false, wantA: DispositionAmbiguous, wantB: DispositionAmbiguous, wantRule: "A was not verified healed before B", wantAmbiguousCreds: 1},
		{name: "same type, A never acted on: the op class cannot separate — ambiguous",
			aType: "service-down", bType: "service-down", aRestored: 45, bInjected: 48,
			alerts: []Alert{al("a", "Service up/down", 2, 9), al("b", "Service up/down", 52, 58)}, execs: []Execution{ex(7, "b", "start-service", 59)},
			wantOverlap: true, wantSeparated: false, wantA: DispositionAmbiguous, wantB: DispositionAmbiguous, wantRule: "AMBIGUOUS", wantAmbiguousCreds: 1},
		{name: "B's execution is A's healing op and A never acted on — ambiguous",
			aType: "container-hang", bType: "container-down", aRestored: 45, bInjected: 48,
			alerts: []Alert{al("a", "Service up/down", 2, 9), al("b", "Service up/down", 52, 58)}, execs: []Execution{ex(7, "b", "restart-container", 59)},
			wantOverlap: true, wantSeparated: false, wantA: DispositionAmbiguous, wantB: DispositionAmbiguous, wantRule: "is not B's healing op", wantAmbiguousCreds: 1},
		{name: "B's alert was OBSERVED before B was injected — it is A's state, ambiguous",
			aType: "container-down", bType: "container-hang", aRestored: 45, bInjected: 48,
			alerts: []Alert{al("a", "Service up/down", 2, 9), al("b", "Service up/down", 44, 50)}, execs: []Execution{ex(7, "b", "restart-container", 51)},
			wantOverlap: true, wantSeparated: false, wantA: DispositionAmbiguous, wantB: DispositionAmbiguous, wantRule: "observed before B was injected", wantAmbiguousCreds: 1},
		{name: "B's alert has no observation time — fails closed",
			aType: "container-down", bType: "container-hang", aRestored: 45, bInjected: 48,
			alerts: []Alert{al("a", "Service up/down", 2, 9), al("b", "Service up/down", -1, 50)}, execs: []Execution{ex(7, "b", "restart-container", 51)},
			wantOverlap: true, wantSeparated: false, wantA: DispositionAmbiguous, wantB: DispositionAmbiguous, wantRule: "unknown", wantAmbiguousCreds: 1},
		{name: "A not restored before B was injected and B's alert is of A's class — ambiguous",
			aType: "container-down", bType: "container-hang", aRestored: 50, bInjected: 48,
			alerts: []Alert{al("a", "Service up/down", 2, 9), al("b", "Service up/down", 52, 58)}, execs: []Execution{ex(7, "b", "restart-container", 59)},
			wantOverlap: true, wantSeparated: false, wantA: DispositionAmbiguous, wantB: DispositionAmbiguous, wantRule: "not restored before B", wantAmbiguousCreds: 1},
		{name: "B's alert is of a class A cannot raise and no execution — nothing to separate",
			aType: "service-hang", bType: "device-down", aRestored: 45, bInjected: 48,
			alerts:      []Alert{al("a", "Service up/down", 2, 9), al("b", "Device Down (SNMP unreachable)", 49, 50)},
			wantOverlap: true, wantSeparated: true, wantA: DispositionAlertedNoExec, wantB: DispositionAlertedNoExec, wantRule: "is a rule class service-hang cannot raise"},
		{name: "no alert and no execution on B — nothing to separate",
			aType: "container-down", bType: "container-hang", aRestored: 45, bInjected: 48,
			alerts:      []Alert{al("a", "Service up/down", 2, 9)},
			wantOverlap: true, wantSeparated: true, wantA: DispositionAlertedNoExec, wantB: DispositionNoAlert, wantRule: "nothing to separate"},
		{name: "no execution on either side, B's alert observed after B — separated",
			aType: "container-down", bType: "container-hang", aRestored: 45, bInjected: 48,
			alerts:      []Alert{al("a", "Service up/down", 2, 9), al("b", "Service up/down", 55, 62)},
			wantOverlap: true, wantSeparated: true, wantA: DispositionAlertedNoExec, wantB: DispositionAlertedNoExec, wantRule: "observed"},
		{name: "B injected exactly when A's raw window closes — no overlap at all",
			aType: "container-down", bType: "container-hang", aRestored: 45, bInjected: 55,
			alerts: []Alert{al("a", "Service up/down", 2, 9), al("b", "Service up/down", 56, 60)}, execs: []Execution{ex(7, "b", "start-container", 61)},
			noCredits:   true,
			wantOverlap: false, wantA: DispositionAlertedNoExec, wantB: DispositionExecutedNoCred},
		{name: "A with no deadline and no restore: its raw window is open, so B overlaps it, and A was never restored — fails closed",
			aType: "container-down", bType: "container-hang", aRestored: 45, bInjected: 120, aDueZero: true,
			alerts: []Alert{al("a", "Service up/down", 2, 9), al("b", "Service up/down", 125, 130)}, execs: []Execution{ex(7, "b", "restart-container", 131)},
			wantOverlap: true, wantSeparated: false, wantA: DispositionAmbiguous, wantB: DispositionAmbiguous, wantRule: "not restored before B", wantAmbiguousCreds: 1},
	}
	for _, c := range cases {
		a := Fault{ID: 1, Host: "g", FaultType: c.aType, InjectedAt: m(0), RestoreDueAt: m(45), RestoredAt: m(c.aRestored)}
		if c.aDueZero {
			a.RestoreDueAt, a.RestoredAt = time.Time{}, time.Time{}
		}
		b := Fault{ID: 2, Host: "g", FaultType: c.bType, InjectedAt: m(c.bInjected), RestoreDueAt: m(c.bInjected + 45), RestoredAt: m(c.bInjected + 45)}
		in := Input{Since: m(-10), Until: m(300), SiteOf: func(string, string) string { return "dc1" }, Faults: []Fault{a, b}, Alerts: c.alerts, Executions: c.execs}
		for _, x := range c.execs {
			if !c.noCredits && x.ExternalRef != c.noCreditFor {
				at := x.ExecutedAt.Add(time.Minute)
				if mm, ok := c.creditAt[x.ExternalRef]; ok {
					at = m(mm)
				}
				in.Credits = append(in.Credits, Credit{ExternalRef: x.ExternalRef, OpClass: x.OpClass, Outcome: "verified_clean", CreditedAt: at})
			}
		}
		r := Attribute(in)
		if got := len(r.Overlaps) == 1; got != c.wantOverlap {
			t.Errorf("%s: overlap recorded=%v (%d), want %v", c.name, got, len(r.Overlaps), c.wantOverlap)
			continue
		}
		if c.wantOverlap {
			o := r.Overlaps[0]
			if o.Separated != c.wantSeparated || !strings.Contains(o.Rule, c.wantRule) || o.ADisposition != c.wantA || o.BDisposition != c.wantB {
				t.Errorf("%s: separated=%v rule=%q dispositions %s/%s, want %v %q %s/%s", c.name, o.Separated, o.Rule, o.ADisposition, o.BDisposition, c.wantSeparated, c.wantRule, c.wantA, c.wantB)
			}
		}
		if got := byID(t, r, 1).Disposition; got != c.wantA {
			t.Errorf("%s: A = %s, want %s", c.name, got, c.wantA)
		}
		if got := byID(t, r, 2).Disposition; got != c.wantB {
			t.Errorf("%s: B = %s, want %s", c.name, got, c.wantB)
		}
		if tot := r.Summary.Total; tot.Credited != c.wantCredited || tot.AmbiguousCredited != c.wantAmbiguousCreds || tot.Faults != 2 {
			t.Errorf("%s: credited=%d ambiguous-credited=%d faults=%d, want %d/%d/2 (ambiguous faults stay in the denominator)", c.name, tot.Credited, tot.AmbiguousCredited, tot.Faults, c.wantCredited, c.wantAmbiguousCreds)
		}
	}
}

// The overlap table renders every pair with its rule, as Markdown and as CSV, one row per pair.
func TestOverlapTableRenders(t *testing.T) {
	r := Attribute(fixture())
	var md, cs bytes.Buffer
	if err := WriteOverlapsMarkdown(&md, r); err != nil {
		t.Fatal(err)
	}
	if err := WriteOverlapsCSV(&cs, r); err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(md.String(), "\n"); lines != 3 || !strings.Contains(md.String(), "| 1670 | container-hang |") || !strings.Contains(md.String(), "op class separates") {
		t.Fatalf("markdown (%d lines):\n%s", lines, md.String())
	}
	rows, err := csv.NewReader(&cs).ReadAll()
	if err != nil || len(rows) != 2 || len(rows[1]) != len(OverlapsHeader) || rows[1][0] != "1670" || rows[1][3] != "1675" || rows[1][17] != "true" {
		t.Fatalf("csv rows = %v %v", rows, err)
	}
}

// Consecutive pairs only: with a fault between A and C on the guest, A's ownership ended at the one between, so the
// pairs recorded are (A, B) and (B, C) — never (A, C) — even when C lands inside A's raw window too. Executions on
// the guest that neither fault owns are disclosed on the pair, never credited to either.
func TestOverlapsAreConsecutiveAndDiscloseUnownedHostExecutions(t *testing.T) {
	in := Input{Since: m(-10), Until: m(300), SiteOf: func(string, string) string { return "dc1" },
		Faults: []Fault{
			{ID: 1, Host: "g", FaultType: "container-down", InjectedAt: m(0), RestoreDueAt: m(45), RestoredAt: m(45)},
			{ID: 2, Host: "g", FaultType: "device-down", InjectedAt: m(46), RestoreDueAt: m(91), RestoredAt: m(50)},
			{ID: 3, Host: "g", FaultType: "service-hang", InjectedAt: m(52), RestoreDueAt: m(97), RestoredAt: m(97)},
		},
		Alerts:     []Alert{{ExternalRef: "organic", Host: "g", Rule: "Something else entirely", ReceivedAt: m(53), ObservedAt: m(53)}},
		Executions: []Execution{{ID: 9, ExternalRef: "organic", OpClass: "restart-service", ExecutedAt: m(54), Verdict: "match", TargetHost: "g"}},
	}
	r := Attribute(in)
	got := map[[2]int64]bool{}
	for _, o := range r.Overlaps {
		got[[2]int64{o.A, o.B}] = true
	}
	if len(r.Overlaps) != 2 || !got[[2]int64{1, 2}] || !got[[2]int64{2, 3}] || got[[2]int64{1, 3}] {
		t.Fatalf("overlaps = %v, want exactly (1,2) and (2,3)", got)
	}
	for _, o := range r.Overlaps {
		if o.A == 2 && (len(o.HostExecsUnattributed) != 1 || o.HostExecsUnattributed[0].ID != 9) {
			t.Fatalf("pair (2,3) must disclose the unowned execution 9 on the guest, got %+v", o.HostExecsUnattributed)
		}
	}
	if r.Summary.Total.Credited != 0 {
		t.Fatalf("an unowned execution credited a fault: %+v", r.Summary.Total)
	}
}
