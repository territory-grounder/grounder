package escalation

import (
	"context"
	"time"
)

// ScheduleReCheck schedules a delayed re-check for an incident whose approval poll went unanswered. If
// the per-incident cap has already been reached it stands down to a human instead (REQ-206/208);
// otherwise it appends an append-only re-check row to the escalation_queue with an incremented attempt
// count and an eligible time (REQ-206).
func (c *Controller) ScheduleReCheck(ctx context.Context, externalRef string, attempts int, eligibleAt time.Time) (Outcome, error) {
	if attempts >= c.Cap {
		return c.standDown(ctx, externalRef, attempts)
	}
	if _, err := c.Store.Enqueue(ctx, externalRef, attempts+1, eligibleAt); err != nil {
		return Scheduled, err
	}
	c.results = append(c.results, ReCheckResult{ExternalRef: externalRef, Attempts: attempts + 1, Outcome: Scheduled})
	return Scheduled, nil
}

// SignalRequeue is the authenticated re-entry point: FireDue calls it for each due row, so a re-check
// re-enters the gated pipeline ONLY through this signal path (INV-01/INV-12, REQ-207), never a bare
// re-trigger. It decides on the LIVE condition: still active ⇒ re-escalate and page the approver graph;
// recovered ⇒ defer closure to the autocloser.
func (c *Controller) SignalRequeue(ctx context.Context, externalRef string) error {
	active, err := c.Condition.StillActive(ctx, externalRef)
	if err != nil {
		return err
	}
	if active {
		if err := c.Pager.Page(ctx, externalRef, "approver-graph"); err != nil {
			return err
		}
		c.results = append(c.results, ReCheckResult{ExternalRef: externalRef, Outcome: ReEscalated})
		return nil
	}
	c.results = append(c.results, ReCheckResult{ExternalRef: externalRef, Outcome: Deferred})
	return nil
}
