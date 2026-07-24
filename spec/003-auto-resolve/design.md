<!-- spec/003 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/003 — Design: per-incident auto-resolve and escalation requeue

How the requirements in `requirements.md` are realized on the Go / Temporal / PostgreSQL stack. Where
this design and the code disagree, the code is the bug and this document is the intent. The predecessor
logic (`scripts/reconcile-completed-sessions.py`: band-aware close-out, per-incident best-outcome
accounting, orphaned-poll re-check) is re-expressed here as typed Go activities — never vendored.

## Components

- **`core/reconcile.Reconciler`** (Phase 2) is the close-out lane, driven by a Temporal Schedule that
  scans finished sessions across the estate. For each session it computes one `CloseOut` decision from a typed,
  already-validated input:

  ```
  Decide(ctx, SessionSnapshot) -> CloseOut
  ```

  `SessionSnapshot` carries the classified `Band` (spec/001), the committed prediction and its
  mechanical `Verdict` (spec/002), the orchestrator-captured `ToolResult` evidence set (INV-11), the
  terminal-result presence flag, and the session age. `CloseOut` is a required-field
  struct — `Action`, `TicketState`, `Outcome`, `ResolutionType` — so producing a decision with a
  missing field is a compile error, which is how the persistence contract is enforced at the type level.

- **`core/escalation.Controller`** owns the requeue lane: it appends `escalation_queue` rows, evaluates a
  fired re-check against the live alert condition, and either re-escalates through the approver
  graph or defers to the autocloser. A re-check re-enters execution only by emitting an authenticated
  internal Temporal signal keyed by `session_id` (INV-01) — the controller's own `SignalRequeue`; there is
  no unauthenticated re-trigger primitive, and the STORE never re-triggers on its own. The controller drives
  the queue through a store seam (`escalation.Store`: `Enqueue` / `DuePending` / `MarkFired`) that BOTH the
  in-memory oracle (`persist.EscalationQueue`) and the durable pgx twin (`db.EscalationStore`) satisfy — so an
  operator with `TG_DB_DSN` gets a requeue lane that survives a restart behind the same controller, while CI
  exercises the controller over the in-memory twin (no Postgres). `FireDue` reads the due batch from the
  store, marks each row fired (append-only — a fired row is transitioned in place, never deleted), THEN
  re-enters it through `SignalRequeue`. Marking BEFORE re-entering is load-bearing: a persistently-failing
  `MarkFired` (a stuck store) then produces NO page at all — no mark, no signal — instead of a paged-but-unmarked
  row that re-pages the approver graph every tick forever; a dropped re-entry is recovered by the reconcile
  loop scheduling a fresh re-check, bounded by the per-incident `Cap`. A failure on one row records the error
  and CONTINUES to the next (per-row isolation, errors joined), so one poisoned incident can never head-of-line
  block every later incident's due re-check. This brings escalation to the same durable-twin parity every other
  governed store already has (predictions, verdicts, the ledger).

## Safety primitives this spec composes over

- **The band (spec/001).** `Band == AUTO` is the precondition for a Done transition (REQ-202). A
  `POLL_PAUSE` session is owned by a human and is **never closed by the reconciler** — even on a confirmed
  clear it goes to **To Verify** (left open), never a silent `human_resolved` Done with no human in the loop.
- **The mechanical verdict (spec/002).** The reconciler distinguishes a read-only / confirm-close session
  (nothing executed — Phase 0/1) from one that actually EXECUTED a remediation (`Executed`, a committed action
  prediction reached the interceptor — Phase 2). An EXECUTED AUTO session transitions to Done only on
  `Verdict == match`; while its async blast-radius verdict has not yet landed it is **HELD** to To Verify,
  never auto-closed on the alert clearing alone. A `deviation` or `partial` verdict demotes the close-out to
  **To Verify** and notifies the approver graph — the acting model has no write path to the verdict.
- **Confirmed-clear evidence (INV-11).** A close to Done admits only on an orchestrator-captured
  `ToolResult` or independent post-condition check proving the alert cleared (REQ-201) — agent free-text
  is inadmissible.
- **Session isolation (INV-12).** Every session, close-out, outcome, and queue row is bound to its
  `session_id`; reads are authority-checked against the acting user/role under RBAC, and the acting
  model has no read path to another operator's session.

## Decision procedure (Phase 2)

1. Session younger than the minimum idle window → **skip** (leave for a later scan).
2. `POLL_PAUSE` / `[POLL]` / paused and awaiting a human vote → **skip** while the poll is live; once the
   poll is orphaned past the poll-age bound → **archive** as `poll_unanswered` and schedule an
   `escalation_queue` re-check row (REQ-206).
3. No terminal result → **leave open**, transition to **To Verify** (REQ-204).
4. `Band == AUTO` (or an `[AUTO-RESOLVE]` marker bound to this action) with a committed action
   prediction: transition to **Done** only when the mechanical verdict is `match` and confirmed-clear
   evidence is present (REQ-201, REQ-202); a `deviation` / `partial` / unevaluated-past-window verdict
   demotes to **To Verify** and pages (INV-11, spec/002).
5. A finished non-AUTO session with a terminal result → **archive** and transition to **To Verify** for
   human confirmation.
6. On every close, record `resolution_type` and append the immutable close-out record, then update the
   per-incident best-outcome rollup (REQ-203, REQ-205).

Steps are ordered so a human-owned or unverified session can never be composed into an auto-close by a
later permissive branch.

## Requeue procedure (Phase 2)

When a queued re-check reaches its `eligible_at`, `core/escalation.Queue` re-tests the underlying alert
condition. If the condition is still active it re-escalates and pages the approver graph; if it
has recovered it defers closure to the autocloser (REQ-207). Re-entry into the gated pipeline is an
authenticated internal Temporal signal keyed by `session_id` — never a bare re-trigger. A
growing `POLL_PAUSE` backlog of reversible work is treated as a policy failure: after the per-incident
unanswered-poll cap is reached, the lane stands down to the fallback approver or next on-call
tier rather than retrying autonomously (REQ-208).

## Out of scope

Band classification is owned by spec/001; the prediction and the mechanical `match`/`partial`/`deviation`
verdict are owned by spec/002; pre-model suppression that prevents a session from ever reaching the
reconciler is spec/005; the auth middleware and ledger-chaining mechanics that the requeue signal and
close-out record ride on are spec/006.
