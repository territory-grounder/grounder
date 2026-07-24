package reconcile

import (
	"context"
	"testing"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/safety"
)

func TestReconcileClosesOnlyOnConfirmedClear(t *testing.T) {
	base := FinishedSession{ExternalRef: "TG-1", SessionID: "s1", Band: safety.BandAuto, HasTerminalResult: true}

	// not confirmed clear ⇒ do not close (no close on asserted success)
	if d := Reconcile(base); d.Close || d.Ticket != TicketToVerify {
		t.Fatalf("unconfirmed clear must not close: %+v", d)
	}
	// confirmed clear + auto band ⇒ close Done auto_resolved
	c := base
	c.ConfirmedClear = true
	if d := Reconcile(c); !d.Close || d.Ticket != TicketDone || d.Resolution != ResAutoResolved {
		t.Fatalf("confirmed clear under auto must auto-resolve to Done: %+v", d)
	}
}

func TestReconcileNoTerminalLeavesOpen(t *testing.T) {
	s := FinishedSession{ExternalRef: "TG-1", SessionID: "s1", Band: safety.BandAuto, ConfirmedClear: true, HasTerminalResult: false}
	if d := Reconcile(s); d.Close || d.Ticket != TicketToVerify {
		t.Fatalf("no terminal result must leave open (To Verify), got %+v", d)
	}
}

func TestReconcileDeviationNeverCloses(t *testing.T) {
	s := FinishedSession{ExternalRef: "TG-1", SessionID: "s1", Band: safety.BandAuto, ConfirmedClear: true, HasTerminalResult: true, HasVerdict: true, Verdict: safety.VerdictDeviation}
	if d := Reconcile(s); d.Close {
		t.Fatalf("a deviation must never auto-close: %+v", d)
	}
}

// A POLL_PAUSE band is human-owned: even a confirmed clear must NOT auto-close it to Done — that would be a
// silent "human_resolved" close with no human. It goes to To Verify, left open for the human.
func TestReconcilePollBandNotAutoClosed(t *testing.T) {
	s := FinishedSession{ExternalRef: "TG-1", SessionID: "s1", Band: safety.BandPollPause, ConfirmedClear: true, HasTerminalResult: true}
	if d := Reconcile(s); d.Close || d.Ticket != TicketToVerify {
		t.Fatalf("a poll-band incident must NOT be auto-closed by the reconciler: %+v", d)
	}
}

// An EXECUTED AUTO remediation (Phase 2) reaches Done ONLY on a clean match verdict: with the async verdict
// not yet landed, it HOLDS to To Verify rather than auto-close on the alert clearing alone. A match verdict
// closes it; a read-only (not executed) AUTO session still closes on the confirmed clear (Phase 0/1 unchanged).
func TestReconcileExecutedAutoHoldsForVerdict(t *testing.T) {
	base := FinishedSession{ExternalRef: "TG-1", SessionID: "s1", Band: safety.BandAuto, ConfirmedClear: true, HasTerminalResult: true, Executed: true}
	// executed, verdict pending (HasVerdict=false) → hold, do not close
	if d := Reconcile(base); d.Close || d.Ticket != TicketToVerify {
		t.Fatalf("an executed remediation without a landed verdict must hold, not close: %+v", d)
	}
	// executed, clean match verdict → close as auto_resolved
	m := base
	m.HasVerdict, m.Verdict = true, safety.VerdictMatch
	if d := Reconcile(m); !d.Close || d.Ticket != TicketDone || d.Resolution != ResAutoResolved {
		t.Fatalf("an executed remediation with a match verdict closes: %+v", d)
	}
	// read-only AUTO (not executed), verdict pending → closes on confirmed clear (Phase 0/1 behavior)
	ro := base
	ro.Executed = false
	if d := Reconcile(ro); !d.Close || d.Ticket != TicketDone {
		t.Fatalf("a read-only AUTO session still closes on confirmed clear: %+v", d)
	}
}

type fakeTickets struct{ last map[string]TicketState }

func (f *fakeTickets) Transition(_ context.Context, ref string, to TicketState) error {
	if f.last == nil {
		f.last = map[string]TicketState{}
	}
	f.last[ref] = to
	return nil
}

func TestCloseOutRecordsResolutionAndChains(t *testing.T) {
	l := audit.NewLedger()
	tk := &fakeTickets{}
	s := FinishedSession{ExternalRef: "TG-1", SessionID: "s1", ActionID: "a1", Band: safety.BandAuto, ConfirmedClear: true, HasTerminalResult: true}
	d := Reconcile(s)
	rec, err := CloseOut(context.Background(), l, tk, s, d)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Resolution != ResAutoResolved || !rec.Closed || rec.SchemaVersion <= 0 {
		t.Fatalf("close-out record wrong: %+v", rec)
	}
	if tk.last["TG-1"] != TicketDone {
		t.Fatalf("ticket must transition to Done, got %q", tk.last["TG-1"])
	}
	if l.Len() != 1 || l.Verify() != nil {
		t.Fatalf("close-out must chain exactly one ledger entry and verify")
	}
}

func TestBestOutcomePerIncidentNotPerEvent(t *testing.T) {
	r := NewBestOutcomeRollup()
	// an alert storm: one incident TG-1 fires many events with varying outcomes
	r.Record("TG-1", ResDeferred)
	r.Record("TG-1", ResAutoResolved) // the best
	r.Record("TG-1", ResEscalated)
	r.Record("TG-2", ResHumanResolved)
	if r.IncidentCount() != 2 {
		t.Fatalf("denominator must count incidents not events, got %d", r.IncidentCount())
	}
	if r.AutoResolvedCount() != 1 {
		t.Fatalf("TG-1's best outcome is auto_resolved: got %d", r.AutoResolvedCount())
	}
	if best, _ := r.Best("TG-1"); best != ResAutoResolved {
		t.Fatalf("best-outcome must keep the best, got %q", best)
	}
}

// A partial verdict — a predicted host firing an unpredicted rule — must never silently auto-close (P0-8);
// it routes to To Verify even under an auto band with a confirmed-clear condition.
func TestReconcilePartialNeverAutoCloses(t *testing.T) {
	s := FinishedSession{ExternalRef: "TG-1", SessionID: "s1", Band: safety.BandAuto, ConfirmedClear: true, HasTerminalResult: true, HasVerdict: true, Verdict: safety.VerdictPartial}
	if d := Reconcile(s); d.Close || d.Ticket != TicketToVerify {
		t.Fatalf("a partial verdict must not auto-close: %+v", d)
	}
	// an invalid verdict likewise never auto-closes
	s.Verdict = safety.Verdict("bogus")
	if d := Reconcile(s); d.Close {
		t.Fatalf("an invalid verdict must not auto-close: %+v", d)
	}
	// a clean MATCH under an auto band still auto-closes
	s.Verdict = safety.VerdictMatch
	if d := Reconcile(s); !d.Close || d.Ticket != TicketDone {
		t.Fatalf("a clean match under auto must auto-close: %+v", d)
	}
}

// An orphaned poll (unanswered POLL_PAUSE) never silently drops: it schedules a re-check (REQ-206, P1-14).
func TestReconcileOrphanedPollSchedulesReCheck(t *testing.T) {
	s := FinishedSession{ExternalRef: "TG-1536", SessionID: "s1", Band: safety.BandPollPause, PollUnanswered: true, ReCheckAttempts: 1}
	d := Reconcile(s)
	if !d.ScheduleReCheck || d.ReCheckAttempts != 1 {
		t.Fatalf("an orphaned poll must schedule a re-check carrying the attempt count: %+v", d)
	}
	if d.Resolution != ResPollUnanswered || d.Close {
		t.Fatalf("an orphaned poll archives as poll_unanswered and stays open: %+v", d)
	}
}
