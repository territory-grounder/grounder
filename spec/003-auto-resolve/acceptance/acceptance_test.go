package acceptance

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/cucumber/godog"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/escalation"
	"github.com/territory-grounder/grounder/core/persist"
	"github.com/territory-grounder/grounder/core/reconcile"
	"github.com/territory-grounder/grounder/core/safety"
)

var accNow = time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)

type fakeTickets struct {
	last map[string]reconcile.TicketState
}

func (f *fakeTickets) Transition(_ context.Context, ref string, to reconcile.TicketState) error {
	if f.last == nil {
		f.last = map[string]reconcile.TicketState{}
	}
	f.last[ref] = to
	return nil
}

type fakeCondition struct{ active map[string]bool }

func (c fakeCondition) StillActive(_ context.Context, ref string) (bool, error) {
	return c.active[ref], nil
}

type fakePager struct{ paged []string }

func (p *fakePager) Page(_ context.Context, ref, tier string) error {
	p.paged = append(p.paged, ref+"@"+tier)
	return nil
}

// TestAutoResolveAcceptance runs the spec/003 acceptance feature against core/reconcile + core/escalation.
func TestAutoResolveAcceptance(t *testing.T) {
	suite := godog.TestSuite{
		Name:                "spec/003 auto-resolve",
		ScenarioInitializer: initializeScenario,
		Options:             &godog.Options{Format: "pretty", Paths: []string{"."}, Tags: "~@pending", Strict: true, TestingT: t},
	}
	if suite.Run() != 0 {
		t.Fatal("spec/003 acceptance scenarios failed")
	}
}

type world struct {
	session  reconcile.FinishedSession
	decision reconcile.ReconcileDecision
	tickets  *fakeTickets
	ledger   *audit.Ledger
	closeRec reconcile.CloseoutRecord
	closeErr error
	rollup   *reconcile.BestOutcomeRollup

	ctrl    *escalation.Controller
	queue   *persist.EscalationQueue // the concrete store behind ctrl, held so a step can assert the append-only history
	pager   *fakePager
	fire    func() error
	fireOut escalation.Outcome
	fireErr error
}

func initializeScenario(sc *godog.ScenarioContext) {
	w := &world{tickets: &fakeTickets{}, ledger: audit.NewLedger()}
	ctx := context.Background()

	// --- reconcile (REQ-201..205) ---
	sc.Step(`^a finished band-AUTO session whose host recovery has no orchestrator-captured ToolResult$`, func() error {
		w.session = reconcile.FinishedSession{ExternalRef: "TG-1", SessionID: "s1", ActionID: "a1", Band: safety.BandAuto, HasTerminalResult: true, ConfirmedClear: false}
		return nil
	})
	sc.Step(`^a finished band-AUTO session with a match verdict and confirmed-clear evidence$`, func() error {
		w.session = reconcile.FinishedSession{ExternalRef: "TG-1", SessionID: "s1", ActionID: "a1", Band: safety.BandAuto, HasTerminalResult: true, HasVerdict: true, Verdict: safety.VerdictMatch, ConfirmedClear: true}
		return nil
	})
	sc.Step(`^a session reaching close-out$`, func() error {
		w.session = reconcile.FinishedSession{ExternalRef: "TG-1", SessionID: "s1", ActionID: "a1", Band: safety.BandAuto, HasTerminalResult: true, ConfirmedClear: true}
		return nil
	})
	sc.Step(`^a session that ended with no terminal result$`, func() error {
		w.session = reconcile.FinishedSession{ExternalRef: "TG-1", SessionID: "s1", Band: safety.BandAuto, HasTerminalResult: false, ConfirmedClear: true}
		return nil
	})
	evaluate := func() error {
		w.decision = reconcile.Reconcile(w.session)
		w.closeRec, w.closeErr = reconcile.CloseOut(ctx, w.ledger, w.tickets, w.session, w.decision)
		return w.closeErr
	}
	sc.Step(`^the reconciler evaluates the session for close-out$`, evaluate)
	sc.Step(`^the reconciler writes the close-out record$`, evaluate)

	sc.Step(`^the incident is not closed and remains open for confirmation$`, func() error {
		if w.decision.Close || w.decision.Ticket != reconcile.TicketToVerify {
			return fmt.Errorf("must not close on unconfirmed clear: %+v", w.decision)
		}
		return nil
	})
	sc.Step(`^the recovered host is reconciled and the ticket transitions to "Done"$`, func() error {
		if !w.decision.Close || w.tickets.last["TG-1"] != reconcile.TicketDone {
			return fmt.Errorf("confirmed-clear auto session must transition to Done: %+v ticket=%q", w.decision, w.tickets.last["TG-1"])
		}
		return nil
	})
	sc.Step(`^the record carries a resolution_type drawn from auto_resolved, human_resolved, escalated, or deferred$`, func() error {
		switch w.closeRec.Resolution {
		case reconcile.ResAutoResolved, reconcile.ResHumanResolved, reconcile.ResEscalated, reconcile.ResDeferred:
			return nil
		default:
			return fmt.Errorf("invalid resolution_type %q", w.closeRec.Resolution)
		}
	})
	sc.Step(`^the incident is left open and transitions to "To Verify"$`, func() error {
		if w.decision.Close || w.tickets.last["TG-1"] != reconcile.TicketToVerify {
			return fmt.Errorf("no terminal result must leave open (To Verify): %+v ticket=%q", w.decision, w.tickets.last["TG-1"])
		}
		return nil
	})

	sc.Step(`^an alert storm that produced many events for a single incident$`, func() error {
		w.rollup = reconcile.NewBestOutcomeRollup()
		return nil
	})
	sc.Step(`^the reconciler records the outcomes$`, func() error {
		// one incident TG-1 with many events, plus a distinct incident TG-2
		w.rollup.Record("TG-1", reconcile.ResDeferred)
		w.rollup.Record("TG-1", reconcile.ResAutoResolved)
		w.rollup.Record("TG-1", reconcile.ResEscalated)
		w.rollup.Record("TG-2", reconcile.ResHumanResolved)
		return nil
	})
	sc.Step(`^exactly one per-incident best-outcome row is recorded and the auto-resolve denominator counts the incident once$`, func() error {
		if best, _ := w.rollup.Best("TG-1"); best != reconcile.ResAutoResolved {
			return fmt.Errorf("TG-1 best outcome must be auto_resolved, got %q", best)
		}
		if w.rollup.IncidentCount() != 2 {
			return fmt.Errorf("denominator must count incidents (2), not events, got %d", w.rollup.IncidentCount())
		}
		if w.rollup.AutoResolvedCount() != 1 {
			return fmt.Errorf("TG-1 counts once as auto_resolved, got %d", w.rollup.AutoResolvedCount())
		}
		return nil
	})

	// --- escalation (REQ-206..208) ---
	newCtrl := func(active map[string]bool, cap int) {
		w.pager = &fakePager{}
		w.queue = persist.NewEscalationQueue()
		w.ctrl = escalation.NewController(w.queue, fakeCondition{active: active}, w.pager, cap)
	}

	sc.Step(`^an approval poll that went unanswered until its session archived$`, func() error {
		newCtrl(nil, 3)
		return nil
	})
	sc.Step(`^the reconciler archives the session as poll_unanswered$`, func() error {
		w.fireOut, w.fireErr = w.ctrl.ScheduleReCheck(ctx, "TG-1", 0, accNow.Add(time.Hour))
		return w.fireErr
	})
	sc.Step(`^a delayed re-check row is scheduled in the escalation queue carrying attempts, status, and eligible_at$`, func() error {
		if w.fireOut != escalation.Scheduled || w.queue.Len() != 1 {
			return fmt.Errorf("a re-check row must be scheduled, out=%v len=%d", w.fireOut, w.queue.Len())
		}
		it := w.queue.Items()[0]
		if it.Attempts != 1 || it.Status != persist.EscalPending || it.EligibleAt.IsZero() {
			return fmt.Errorf("re-check row missing attempts/status/eligible_at: %+v", it)
		}
		return nil
	})

	sc.Step(`^a queued re-check whose alert condition is still active$`, func() error {
		newCtrl(map[string]bool{"TG-1": true}, 3)
		_, err := w.ctrl.ScheduleReCheck(ctx, "TG-1", 0, accNow.Add(-time.Minute)) // due
		w.fire = func() error { _, e := w.ctrl.FireDue(ctx, accNow); return e }
		return err
	})
	sc.Step(`^a queued re-check whose alert condition has recovered$`, func() error {
		newCtrl(map[string]bool{"TG-1": false}, 3)
		_, err := w.ctrl.ScheduleReCheck(ctx, "TG-1", 0, accNow.Add(-time.Minute))
		w.fire = func() error { _, e := w.ctrl.FireDue(ctx, accNow); return e }
		return err
	})
	sc.Step(`^a re-check whose per-incident unanswered-poll cap has been reached$`, func() error {
		newCtrl(nil, 3)
		w.fire = func() error {
			out, e := w.ctrl.ScheduleReCheck(ctx, "TG-1", 3, accNow.Add(time.Hour)) // attempts == cap
			w.fireOut = out
			return e
		}
		return nil
	})
	sc.Step(`^the re-check fires$`, func() error { return w.fire() })

	sc.Step(`^the system re-escalates and pages the approver graph through an authenticated Temporal signal keyed by session$`, func() error {
		if len(w.pager.paged) != 1 || w.pager.paged[0] != "TG-1@approver-graph" {
			return fmt.Errorf("still-active re-check must re-escalate via the signal path and page the approver graph, got %v", w.pager.paged)
		}
		return nil
	})
	sc.Step(`^the system defers closure to the autocloser and does not page the approver graph$`, func() error {
		if len(w.pager.paged) != 0 {
			return fmt.Errorf("recovered re-check must defer, not page: %v", w.pager.paged)
		}
		res := w.ctrl.Results()
		if len(res) == 0 || res[len(res)-1].Outcome != escalation.Deferred {
			return fmt.Errorf("recovered re-check must record a defer outcome")
		}
		return nil
	})
	sc.Step(`^the system stands down to the fallback approver rather than retrying autonomously$`, func() error {
		if w.fireOut != escalation.StoodDown {
			return fmt.Errorf("cap reached must stand down, got %v", w.fireOut)
		}
		if w.queue.Len() != 0 {
			return fmt.Errorf("stand-down must not enqueue another re-check")
		}
		if len(w.pager.paged) != 1 || w.pager.paged[0] != "TG-1@fallback-approver" {
			return fmt.Errorf("stand-down must page the fallback approver, got %v", w.pager.paged)
		}
		return nil
	})
}
