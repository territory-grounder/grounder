## Goal
The phase map of an alert-triggered incident session: every phase has an entry question and an exit
criterion, and no phase starts before the previous one's exit criterion is met. Skipping a phase is how
a five-minute fix becomes a multi-hour outage — the skipped phase is where the surprise was hiding.

| Phase | Answers | Exit criterion |
|-------|---------|----------------|
| 0 Triage | Who, where, what state — pure fact-gathering | One sentence: what is broken, and where |
| 1 Drift | Does live match the declared configuration? | Drift posture known, correlated with the alert or ruled out |
| 2 Context | What else is firing? History? Resource posture? | Isolated vs burst vs recurrence is decided |
| 3 Propose | What fix, on what evidence, at what confidence? | A proposal with cited evidence and stated confidence |
| 4 Approve | Who may act — and in which mode? | The governed gate has ruled; the band is recorded |
| 5 Execute | Apply — one change at a time | Fix applied, post-state captured |
| 6 Learn | What does the estate now know that it did not? | Outcome recorded where the next triage will find it |

## Required evidence
- Phase 0 consumes the identity and live-state reads (`get-estate-context`, `get-device-status`,
  `get-device-eventlog`); phase 1 the declared-vs-running comparison; phase 2 the queue
  (`get-active-alerts`) and precedent (`get-incident-history`); phase 3 consumes phases 0–2 — a
  proposal that cites no phase-0 observation is not ready.
- Phase 5 consumes ONLY an approved proposal; phase 6 consumes the post-fix observations.

## Decision rules
- Do not start phase N+1 before phase N's exit criterion is met; when a later phase invalidates an
  earlier conclusion, RETURN to that phase rather than patching forward.
- Phase 4 belongs to the governed approval gate. Never self-override the risk verdict; if the
  classification looks wrong, say so in the proposal — do not quietly force a lower band.
- In phase 5, apply one change at a time and capture the post-state after each — a multi-change step
  has no bisection target when it regresses.
- Re-consult this map at every phase boundary; long sessions lose their place, and the map is cheaper
  than the outage.
- The behavioral protocols this map leans on — the debugging sequence, evidence-citation duty, the
  shortcut list, loop red-flags — are the agent's compiled seed skills; this runbook is the map that
  orders them, not a restatement of them.

## Verification
- A completed session's record shows each phase's exit artifact: the one-sentence fact, the drift
  posture, the context verdict, the evidenced proposal, the gate's ruling, the post-state capture, the
  recorded outcome.
- The next similar incident FINDS the phase-6 record via precedent — that retrieval is the proof that
  phase 6 actually completed.
