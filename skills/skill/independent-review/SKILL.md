---
name: independent-review
class: skill
version: 0.1.0-distilled
source: distill:openclaw/skills/cross-tier-review/SKILL.md
description: Review another analysis by independently re-verifying claims — five checks, then an evidence-bound verdict
---

## Goal
Give an independent, evidence-bound verdict on another analysis — a diagnosis and its proposed action —
by re-verifying it yourself, not by reading it and agreeing. Review that never touches a tool is
agreement theater; it adds confidence without adding verification.

## Required evidence
- The analysis under review: its factual claims, cited evidence, stated confidence, proposed action.
- YOUR OWN reads: at least one of the analysis's factual claims independently re-verified with a live
  observation (`get-device-status`, `get-tracker-history`, `get-estate-context` — whichever the claim
  is about).
- `get-estate-context` for the action's target — the dependency and topology facts that decide whether
  the action can cause secondary failures.
- `get-incident-history` for the host or rule — has this fix been applied before, and did it hold?

## Decision rules
Execute ALL five checks; a review that skips one says so.
1. CLAIM — independently re-verify at least one factual claim with your own tool call. Never verdict on
   prose alone.
2. ASSUMPTION — name the implicit assumptions the analysis rests on; flag any unsupported by its evidence.
3. ALTERNATIVE — name at least one plausible alternative cause that was NOT considered, even when you
   agree with the diagnosis.
4. RISK — could the proposed action cause secondary failures? Check dependencies and stateful workloads
   on the target. Found risk of collateral → the verdict is DISAGREE, always.
5. RECURRENCE — if precedent exists, does the proposal address the root cause or re-patch the symptom
   the same way as last time?
Then verdict, exactly one of:
- AGREE — with the reason and the claim you re-verified.
- DISAGREE — with the specific defect: wrong cause, unsafe action, or missing evidence.
- AUGMENT — the analysis stands, plus the context it missed.
When the analysis's own confidence is low (below 0.5), scrutinize hardest for invented fixes — low
confidence plus a tidy remedy is the classic hallucination shape. Review requests are terminal work:
answer them yourself, never re-escalate one.

## Verification
- The verdict cites the re-verified claim together with the observation that verified it.
- A DISAGREE names the exact defect, not a general unease.
- The record shows at least one claim verified and at least one alternative considered — a review
  without both is not one.
