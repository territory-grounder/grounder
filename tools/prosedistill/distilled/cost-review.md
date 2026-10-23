## Goal
A periodic (weekly, and on suspicion) review of model spend against session quality: find the category
that quietly got expensive, the variant that got worse, the trend that outruns its workload — and route
each anomaly to an owner. Cost review without a quality axis optimizes the wrong thing: the cheapest
session is the one that fails fast.

## Required evidence
- Per-session cost over the review window, grouped by session category, from your model-usage
  observability (per-call token and cost metrics, session records).
- Per-variant comparison when more than one configuration serves the same category — cost AND outcome
  quality side by side.
- Daily totals across the window — the day-over-day trend line.
- The sessions whose recorded confidence fell below 0.5 — the quality floor, independent of cost.
- The alert volume for the same window, for correlating spend with workload.

## Decision rules
- A category whose AVERAGE cost per session exceeds your per-session threshold is a cost-optimization
  candidate — flag the category, not the single expensive outlier.
- One variant consistently worse than its sibling — costlier at equal quality, or weaker at equal cost —
  is an investigation, not a tolerated spread.
- A rising day-over-day trend WITHOUT matching alert-volume growth points at runaway sessions or a
  changed model mix — find which before the month ends, not after.
- Low-confidence sessions get a quality review regardless of cost; cheap failures are still failures,
  and a cluster of them in one category is a competence gap, not a cost item.
- Correlate with workload BEFORE attributing: an alert burst legitimately raises spend, and flagging it
  as waste teaches readers to ignore the review.

## Verification
- Every flagged anomaly leaves the review with an owner and a follow-up — a review that only observes
  is a dashboard, not a control.
- The next window's review cites this one: the flagged category's average moved, or the flag escalated;
  a flag that silently evaporates was noise, and the thresholds should say so.
- Thresholds themselves are reviewed against the false-flag rate — a review that cries every week gets
  read by no one.
