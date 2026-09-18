## Goal
Periodic maintenance of an operational knowledge base — precedents, lessons, captured session
knowledge — so retrieval stays sharp as the corpus grows. The failure mode is quiet: near-duplicates
dilute ranking, frozen snapshots outlive their truth, and low-trust captures crowd out hard evidence.
The remedy is a REVIEW, not an automation: the audit surfaces candidates; a human (or an owning
review) decides.

## Required evidence
- A duplicate-cluster report: entries grouped by topic IDENTITY (name/description), not body text —
  identity is what near-duplicates share; bodies are exactly what diverges between them.
- An expiry report scoped by durability class: point-in-time snapshots older than their window are
  candidates; DURABLE GUIDANCE (feedback rules, standing constraints) is exempt — staleness does not
  apply to "always do X", only to "the state of Y was Z".
- Provenance and trust class per entry: which capture path produced it (curated incident record vs
  bulk session capture), so retrieval weighting has something to key on.
- Retrieval-side symptoms: known-good queries whose expected entry no longer ranks, as the live
  measure of dilution.

## Decision rules
- Never auto-delete: the audit proposes, the review disposes. For clusters — distill into one
  canonical entry and remove the rest, or record that the overlap is intentional (one per scope).
  For expiry — archive rather than delete when historical reference plausibly matters.
- Never merge across durability classes: a snapshot and a standing rule about the same topic are
  different objects with different lifecycles; merging them gives the rule an expiry or the snapshot
  immortality.
- Weight, don't exclude, lower-trust sources at retrieval: bulk-captured knowledge gets a discount so
  curated records win ties — exclusion throws away the long tail; equal weight lets noise outvote
  evidence. Below a substance threshold, don't capture at all: near-empty summaries generate
  retrieval noise and review burden with no recall value.
- Roll back capture mistakes by retrieval suppression FIRST (weight to zero), deletion only after
  review — suppression is reversible and preserves the audit trail.
- Do not hand-edit machine-extracted entries to "improve" them; fix the extractor or let the entry's
  own confidence field do its job. Hand-edits create a class of entries whose provenance lies.
- Automate the audit's CADENCE only after manual runs prove the signal quality — a noisy weekly
  report trains its readers to skip it.

## Verification
- After a review round, the known-good retrieval queries rank their expected entries again.
- The index/size pressure that triggered the round is measurably reduced, and no durable-guidance
  entry was expired.
- Every deletion in the round traces to a review decision; suppressed-not-deleted entries still
  resolve in the audit trail.
