# spec/029 — Commit-confirmed actuation (auto-reverting mutations)

> **OWNER SIGN-OFF RECORDED 2026-08-14** (TG-488 B5) — the TG-82 gate is closed and the four flagged
> questions are answered (design.md § Sign-off rulings); T-029-1..5 are `pending` and now workable.
> The index row advances to **Ratified** only when the acceptance oracles land — that word is the
> lattice's terminal delivered-state, not this sign-off. Provenance: [F] h-ssh Junos
> `commit confirmed` (TG-81 audit) · [O] INV-02, INV-07, INV-09 · TG-82, TG-79 (#23 G2), TG-146-S3/S4
> adjacency.

## The inversion

Junos `commit confirmed N` arms a dead-man's-switch: the DEFAULT outcome of any change is REVERT, and the
change survives only if a follow-up commit positively confirms inside the window. Applied to TG: a
reversible mutation executes WITH a pre-armed self-revert, and survives ONLY if the mechanical verifier
positively confirms the committed prediction inside the confirm window. This flips the safety model from
"act → observe → roll back only if someone notices" to fail-safe by construction — a wrong action self-heals
without anyone catching it.

## Requirements

- **REQ-2901** — the armed revert is the default outcome. WHERE an op-class is commit-confirmed-eligible
  (REQ-2904), the actuation SHALL arm a durable revert timer (a Temporal timer on the action's workflow,
  never an in-process timer) BEFORE the effect executes, bound to the sealed action_id and the registry's
  compensating argv. WHEN the window elapses without a positive confirm, the system SHALL execute the
  inverse — with one carve-out: the unverifiable case HOLDS and pages instead (REQ-2902). An armed revert that cannot be durably recorded SHALL refuse the forward effect (fail closed —
  an unarmed "armed revert" is the pve03 shape wearing a timer).

- **REQ-2902** — only the MECHANICAL verifier confirms. The confirm signal SHALL be derived from a
  MECHANICAL, TG-captured observation — never from a model token, an operator free-text, or the absence
  of alerts alone. The PRIMARY confirm is the terminus verify's verdict AND its observability bit —
  `verdict == match AND verified == true`, where `verified` is the TG-182 `observedOK` seam the
  interceptor already carries (an empty observed set with `verified=true` is the legitimate quiet case;
  `verified=false` is "could not observe", a DISTINCT state the spec/002 3-value verdict alone cannot
  express). A `partial`/`deviation` SHALL NOT confirm — the timer fires and the inverse runs. An
  unverifiable post-state (`verified=false`) SHALL NOT confirm on the terminus read AND SHALL NOT revert
  on blindness alone: the window SHALL HOLD armed, the hold SHALL page (the same page-worthiness as a
  fired revert), and the inverse SHALL fire only on a subsequently OBSERVED deviation.
  **DURABLE-SUBSTITUTE CONFIRM (TG-499).** For a STATE-PRECONDITIONED op-class (`RequiresTargetState`,
  the TG-378 guest-lifecycle pattern) an unverifiable terminus MAY nonetheless confirm IF — and only if
  — a FRESH, POSITIVE, mechanical, TG-captured durable signal that fails DIFFERENTLY than the terminus
  monitor positively reads the target in its DESIRED END STATE. The sanctioned signal is `guest_liveness`
  (the PVE-sweep projection; it does not share the LibreNMS HTTP surface's failure mode, and its reader
  returns `ok=false` on any stale/unreadable projection). For a NON-state-preconditioned service-fault class
  (`RequiresTargetState == ""` — restart-service/start-service/reload-service/restart-container) the
  sanctioned signal is instead a POSITIVE captured provider recovery: `RecoveredSince` reports whether TG
  durably recorded an `ingest_transition` `recovery` row for this (host, rule-family) at/after the execution
  instant (spec/012's confirmed-clear-belt evidence, REQ-1122/1223; it likewise fails DIFFERENTLY than the
  terminus monitor and returns false on any empty-rule / no-alias / no-row / query-error path). This is a
  POSITIVE substitute reading, NEVER
  the absence of a signal: an absent / stale / errored / unestablishable durable read, or a target NOT in
  the desired end state (or, for the service class, NO captured recovery record), STILL HOLDs+pages — fail-closed on every unobservable path is preserved, and the
  substitute is mechanical and TG-captured (never a model token, operator free-text, or "no open
  incident"). Resolving a paged hold is operator workflow; a held run confirms nothing and credits
  nothing (REQ-2907's armed-never-counts covers it).
  (Unknown is not confirmed — the TG-378/REQ-112 discipline applied to the confirm edge. Unknown is not
  DEVIATION either: verification-blindness is a common benign estate state (TG-337/TG-461), and a revert
  fired on blindness re-mutates a possibly-healthy target. The durable-substitute carve-out does not
  weaken this: it confirms on a POSITIVE independent reading, never on blindness — "verification through
  a channel that can fail differently than the claim" (docs/the-map-is-not-the-territory.md), the same
  durable-independent-signal move spec/012's confirmed-clear belt already makes for incident-close when
  the LibreNMS re-pull lags, REQ-1122/REQ-1223.)
  **SIGN-OFF RULING 2026-08-14 (owner, TG-488 B5):** HOLD+page chosen over the draft's
  unverifiable→revert, amending this requirement at sign-off exactly as the draft flagged.
  **AMENDED 2026-08-16 (owner, TG-499):** positive durable-substitute confirm permitted for
  state-preconditioned classes (`guest_liveness`); positive-only, mechanical, fails-differently;
  fail-closed on every unobservable path preserved. Grounded: the predecessor confirms live heals on the
  target's own liveness (not the monitor that fired the alert), and spec/012 already confirms
  incident-close on a durable substitute when the monitor lags — TG was, on this edge, holding+paging
  where the incumbent confirms.
  **AMENDED 2026-08-16 (owner, TG-461 option-c):** the same durable-substitute confirm extended to
  NON-state-preconditioned service-fault classes, using spec/012's POSITIVE captured `ingest_transition`
  recovery (`RecoveredSince`, scoped to the incident's rule-family) as the positive signal; positive-only,
  fail-closed on every unobservable path preserved, never the absence of an open incident — the same posture
  as the TG-499 guest slice, on a signal already blessed for incident-close.

- **REQ-2903** — the inverse is a full mutation. The compensating action SHALL traverse the SAME
  interceptor chain as any mutation: fixed argv from the registry's rollback template (INV-02), never-auto
  floor, mode chokepoint (INV-09), evidence binding, and the ledger. A revert the chain refuses SHALL page
  (the alerting surface), never silently skip — an unrevertable armed revert is an incident, not a no-op.

- **REQ-2904** — eligibility is declared, closed, and conservative. Commit-confirmed eligibility SHALL be
  a per-op-class registry declaration (the `requires_target_state` pattern): only classes with a
  registry-defined inverse are eligible; classes without one SHALL remain human-poll and SHALL NOT be
  auto-confirmed. The confirm window SHALL be a per-class tunable with a conservative default (minutes,
  ≥ 2× the monitoring poll cycle), validated as data at registry load.

- **REQ-2905** — the canary mandate. WHILE an op-class holds a canary/staged posture (the staged-canary
  allowlist or its first N graduated runs), commit-confirmed SHALL be MANDATORY for it: the forward effect
  SHALL NOT execute without the armed revert. It LAYERS ON the existing canary law, never replaces it —
  the staged-canary POLL_PAUSE forcing still requires its human vote first; commit-confirmed arms AFTER
  that approval, on the approved execution. Worst case for a narrow reversible op becomes "auto-healed
  in T minutes", not "we broke it and must notice".

- **REQ-2906** — observability, nothing silent. Arm, confirm, fire, and inverse-outcome SHALL each append
  to the tamper-evident ledger bound to the action_id, and the workflow state (armed / confirmed /
  held-unverifiable / reverted / revert-failed) SHALL be queryable and rendered on the console's workflow timeline. A fired
  revert SHALL alert (the same page-worthiness as a breaker trip); a revert-FAILED state SHALL page
  immediately and trip the mutation breaker.

- **REQ-2907** — graduation reads the confirmed outcome only. A commit-confirmed run SHALL feed the
  graduation ladder ONLY after the confirm resolves: confirmed-clean → the terminus credit path
  (credits.Claim + the 0064 grounded trigger, spec/028); fired-revert → deviation (demote); revert-failed
  → deviation + breaker. The armed window SHALL NOT count as clean (the same discipline direction as
  spec/017 REQ-1711's pending-never-counts, applied to the armed state).

- **REQ-2908** — an empty diff is a free no-op guard. WHERE the pre-action plan/diff of a
  commit-confirmed class computes empty (the target already holds the desired state), the effect SHALL
  resolve as a refused no-op BEFORE arming anything — the TG-378 seal-time precondition generalized to
  apply-time for the classes that can compute a diff.
