---
name: alert-deduplication
class: skill
version: 0.1.0-distilled
source: distill:openclaw/skills/infra-triage/SKILL.md
description: One incident per fault — reuse open incidents, reopen ones under verification, link related, escalate recurrence
---

## Goal
One incident per fault. Before opening a new incident for an alert, establish whether this fault is
already being worked — and when it is, join that work instead of duplicating it. A queue with three
incidents for one fault splits the evidence three ways and triples the noise.

## Required evidence
- `get-incident-history <host>` — open and recent incidents on this host, judged against a 24-hour window.
- `get-tracker-history` for any incident found — its current state and how recently it moved.
- A related-incident scan: the same <alert-rule> firing on OTHER hosts within the last 12 hours.

## Decision rules
- An OPEN incident for the same host within 24 hours → reuse it. Append the new alert's evidence there;
  do not create a sibling.
- An incident recently closed but still under verification → REOPEN it. A fault that returns while its
  fix is being verified is the same incident, and the return is evidence about the fix.
- The same rule on other hosts within 12 hours → LINK them as related and say so in the findings — it
  may be a shared-cause burst wearing per-host clothing.
- Reuse MEANS recurrence, and recurrence changes the job: do not re-run the identical investigation to
  the identical conclusion. Escalate with the prior outcome attached — the first fix did not hold, and
  that fact is the most important evidence in the record.
- A new alert joining an open incident inherits that incident's context and severity floor; it does not
  restart triage from zero.

## Verification
- After triage, exactly one open incident exists for the fault, and its record names every constituent
  alert row.
- A recurrence carries the recurrence marker and cites the prior incident's outcome — a reader can see
  it is the second occurrence without archaeology.
- The related links survive: the burst, if it was one, is discoverable from any of its members.
