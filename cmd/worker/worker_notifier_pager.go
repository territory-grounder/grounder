package main

// The notifier PAGER adapter, carved out of main()'s composition root (TG-501 LOC-debt paydown). notifierPager
// implements core/escalation.Pager by paging an approver tier through the human notifier channel; its `notify`
// field carries the wiring:"required" tag (a non-nil struct asserts the seam is bound), and observe() reports the
// offered/produced counts to the wiring ledger. Pure relocation — construction stays at its two call sites in
// main() (the suppression two-phase verifier + the SeamEscalationPage bind); wiring_ledger_test (call-based) pins
// it. Behaviour is unchanged by the move.

import (
	_ "time/tzdata" // embed the IANA zoneinfo DB so time.LoadLocation resolves on distroless (no OS tzdata)

	"context"
	"log"

	notifier "github.com/territory-grounder/grounder/adapters/notifier"
)

// notifierPager pages an approver tier through the human notifier channel (core/escalation.Pager) — an
// escalation PAGE, not an approval poll (Approval=false). Paging is the Phase-0/1 human-in-the-loop
// channel, never an estate mutation and never mutation-gated. A nil notifier ⇒ a LOGGING pager: the
// re-escalation is recorded to the log (best-effort) rather than lost, so the FireDue lane still records
// its decisions when no channel is configured.
// notifierPager carries the `wiring:"required"` tag on notify because the struct being non-nil says
// nothing about whether a page reaches anyone: Page() returns nil — SUCCESS — when notify is nil, and
// FireDue has already marked the queue row fired by then, so the escalation is consumed and lost with no
// error and no retry. The tag is what lets Bind see the hole in an otherwise perfectly-wired value.
type notifierPager struct {
	notify func(ctx context.Context, n notifier.Notice) error `wiring:"required"`
	// yield reports (offered, produced) for the escalation.page seam. NOT wiring:"required": a pager
	// without telemetry still pages, and making the observer mandatory would let a missing gauge take
	// down the escalation path it exists to watch.
	yield func(offered, produced int)
}

func (p notifierPager) Page(ctx context.Context, externalRef, tier string) error {
	body := "escalation re-check for " + externalRef + " — paging " + tier
	if p.notify == nil {
		// This arm is why escalation.page is a CRITICAL seam: it returns success while the escalation
		// reaches a log file. The wiring:"required" tag makes Bind report it dark, and the yield pair
		// makes it countable — one page offered, none produced.
		log.Printf("escalation: %s (no notifier wired — page recorded to log only)", body)
		p.observe(1, 0)
		return nil
	}
	err := p.notify(ctx, notifier.Notice{DecisionID: externalRef, Body: body, Approval: false})
	p.observe(1, boolCount(err == nil))
	return err
}

// observe reports this page's yield, tolerating an unset hook so the pager stays usable in oracles.
func (p notifierPager) observe(offered, produced int) {
	if p.yield != nil {
		p.yield(offered, produced)
	}
}
