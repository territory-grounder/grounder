package escalation

import "context"

// standDown escalates to a human when the per-incident unanswered-poll cap is reached: rather than
// retrying autonomously, it pages the fallback approver / next on-call tier and records the stand-down
// (REQ-208, paradigm-rule 2). It does NOT enqueue another re-check — the retry lane is exhausted.
func (c *Controller) standDown(ctx context.Context, externalRef string, attempts int) (Outcome, error) {
	if err := c.Pager.Page(ctx, externalRef, "fallback-approver"); err != nil {
		return StoodDown, err
	}
	c.results = append(c.results, ReCheckResult{ExternalRef: externalRef, Attempts: attempts, Outcome: StoodDown})
	return StoodDown, nil
}
