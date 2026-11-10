## Goal
Never triage as if for the first time. Before diagnosing, ask what happened the last time this host or
this rule was in trouble — then weigh that precedent against TODAY's evidence. Precedent raises the
right hypotheses early; adopted uncritically, it re-applies last month's fix to this month's fault.

## Required evidence
- `get-incident-history <host>` — prior incidents on this host: date, what was found, how it resolved,
  at what confidence. Query by <alert-rule> too when the rule is the recurring element.
- The CURRENT live state: `get-device-status <host>` and `get-device-eventlog <host>` — the evidence
  that decides whether the precedent applies now.

## Decision rules
- Consult precedent BEFORE forming the diagnosis, not after — it is cheap, and a recurring (host, rule)
  pair puts a high prior on the recurring cause.
- Precedent is high-signal, NOT ground truth. Before adopting a prior cause, confirm its signature in
  today's evidence (the same event shape in the eventlog, the same unit down). A precedent adopted
  without a live match is a guess wearing history.
- When the precedent does NOT apply, say that it was checked and why it fails to match — ruling out the
  obvious repeat is real triage work and belongs in the record.
- The SAME resolution appearing twice or more for the same (host, rule) means the root cause is
  unaddressed: flag it explicitly instead of applying the fix a third time in silence.
- EMPTY history is a finding, not a dead end: this incident is novel for the host — widen the evidence
  gathered and lower the prior confidence accordingly. Present results honestly: cite the incident's
  identifier, date, and outcome when history exists; say clearly when it does not.

## Verification
- The conclusion cites the precedent's identifier AND the live observation that confirmed or refuted it
  — never the precedent alone.
- A repeat resolution carries the root-cause-unaddressed flag, visible to the next reader.
- On novel incidents, the record states that history was consulted and found empty — so the next
  occurrence of this fault HAS a precedent to find.
