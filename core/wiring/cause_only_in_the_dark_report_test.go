package wiring

// ONE STRING COULD NOT ANSWER TWO QUESTIONS (TG-354 / TG-250).
//
// A seam's prose is printed in two places: the DARK report (report.go) and the yield register's STARVED and
// UNOBSERVED lines (yield.go). Every Consequence used to open with its dark cause, and yield.go worked
// around that by wrapping the text — "[the cost is the same as if it had never been wired — …]" — a true
// frame around a false sentence.
//
// Audited 2026-08-06 against the running worker: FIVE of six such texts named a state the deployment was
// not in. "no entry tracker is bound" while the boot log read "tracker: entry ticket read/transitioned via
// youtrack"; "no tier-1 suppression chain is configured" while it read "tier-1 gate active — 0 freeze …";
// "deps.Notify is nil" while it read "notifier: governance notices/polls delivered via matrix".
//
// SeamSpec.Cause now carries the dark-state reason. The dark report prints it; the yield register cannot,
// because nothing hands it one.

import (
	"strings"
	"testing"
	"time"
)

// KILLING MUTATION: render f.Cause in yield.go's starved/unobserved line. RED — that is the whole defect,
// reintroduced through a different door.
func TestTheYieldRegisterNeverPrintsACause(t *testing.T) {
	f := YieldFinding{
		Seam:        SeamTrackerEntry,
		State:       YieldStarved,
		Offered:     306,
		Produced:    0,
		Unit:        Unit{Offered: "entry-ticket lookups", Produced: "tickets read or transitioned"},
		Consequence: "the incident's own ticket is not read: the dedup stage cannot tell whether a parent is open",
	}
	line := f.Reason()

	// Every cause string in the register must be absent from a starved line.
	for _, sp := range All() {
		if sp.Cause == "" {
			continue
		}
		if strings.Contains(line, sp.Cause) {
			t.Errorf("the starved line carries the dark CAUSE %q. For a starved seam the thing it names is "+
				"bound and running — that is the state the register just reported — so the line asserts a "+
				"reason it did not establish.\nGot: %s", sp.Cause, line)
		}
	}
	// The cost must still be there: a line that names neither cause nor cost says only "something is wrong".
	if !strings.Contains(line, "dedup stage") {
		t.Errorf("the starved line dropped the COST as well — it now reports a state with no statement of "+
			"what it loses.\nGot: %s", line)
	}
}

// The DARK report is the one place a cause is true, and it must actually print it — otherwise the split
// deleted information rather than relocating it.
//
// KILLING MUTATION: drop the Cause arm from Reason(), or stop copying sp.Cause into the Finding. RED.
func TestTheDarkReportPrintsCauseThenCost(t *testing.T) {
	f := Finding{
		Seam:        SeamGovNotify,
		State:       DeclaredDark,
		Cause:       "deps.Notify is nil",
		Consequence: "governance notices reach no operator",
	}
	line := f.Reason()
	if !strings.Contains(line, "deps.Notify is nil") {
		t.Errorf("the dark report does not name the cause. Removing it from the Consequence strings was a "+
			"RELOCATION, not a deletion — for a genuinely dark seam, naming the nil field is the most "+
			"useful sentence available.\nGot: %s", line)
	}
	if !strings.Contains(line, "governance notices reach no operator") {
		t.Errorf("the dark report dropped the cost.\nGot: %s", line)
	}
	if strings.Index(line, "deps.Notify is nil") > strings.Index(line, "governance notices reach no operator") {
		t.Errorf("cost printed before cause — the reader wants why, then what it costs.\nGot: %s", line)
	}
}

// A seam with no distinctive cause must still render its cost. Cause is optional and an empty one must not
// produce a dangling separator.
func TestASeamWithNoCauseStillRendersItsCost(t *testing.T) {
	f := Finding{Seam: SeamSyslogRead, State: DarkUnbound, Consequence: "the agent has no device-log window"}
	line := f.Reason()
	if !strings.Contains(line, "the agent has no device-log window") {
		t.Fatalf("cost missing: %s", line)
	}
	if strings.Contains(line, ": :") || strings.HasSuffix(strings.TrimSpace(line), "—") {
		t.Errorf("an empty cause left a dangling separator: %q", line)
	}
}

// VACUITY FLOOR: the register must actually declare causes, or the first test iterates over nothing and
// passes on a yield renderer that prints all of them.
func TestTheRegisterDeclaresCauses(t *testing.T) {
	n := 0
	for _, sp := range All() {
		if strings.TrimSpace(sp.Cause) != "" {
			n++
		}
	}
	if n < 5 {
		t.Fatalf("only %d seam(s) declare a Cause — the audit found six texts carrying one inline, so a "+
			"near-empty set means the relocation dropped them instead of moving them", n)
	}
}

// THE WIRING, NOT THE RENDERER. TestTheDarkReportPrintsCauseThenCost builds a Finding literal — it proves
// Reason() formats a cause it is handed, and nothing about whether Manifest.Report ever hands it one.
// Removing `Cause: sp.Cause` from the finding constructor SURVIVED that test completely: the eleventh time
// this session a value was correct at the resolver and absent at the call site.
//
// KILLING MUTATION: drop `Cause: sp.Cause` from Manifest.Report's Finding literal. RED.
func TestReportCarriesTheSpecsCauseIntoTheFinding(t *testing.T) {
	// A manifest with a seam recorded ABSENT is the dark path Report renders.
	m := New()
	Absent[struct{}](m, SeamGovNotify, validBecause())

	findings, _ := m.Report(time.Now().UTC())

	var got Finding
	var seen bool
	for _, f := range findings {
		if f.Seam == SeamGovNotify {
			got, seen = f, true
		}
	}
	if !seen {
		t.Fatal("gov.notify produced no finding from a manifest that recorded it Absent — this guard is " +
			"examining nothing")
	}

	var want string
	for _, sp := range All() {
		if sp.ID == SeamGovNotify {
			want = sp.Cause
		}
	}
	if want == "" {
		t.Fatal("gov.notify declares no Cause in the register, so this test cannot detect the wiring being " +
			"cut — fix the register or this guard")
	}
	if got.Cause != want {
		t.Errorf("Report built a Finding with Cause=%q, want %q from the SeamSpec. The renderer formats a "+
			"cause correctly and never receives one — a dark seam then reports its cost with no reason, "+
			"which is the information the split was supposed to RELOCATE rather than delete", got.Cause, want)
	}
	if !strings.Contains(got.Reason(), want) {
		t.Errorf("the end-to-end line does not carry the cause.\nGot: %s", got.Reason())
	}
}
