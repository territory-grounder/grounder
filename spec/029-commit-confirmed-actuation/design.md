# spec/029 — design (DRAFT awaiting owner sign-off, TG-82)

> Reference draft for ratification. Nothing below is built; the tasks are all `pending`.

## Shape: the dead-man's switch as a Temporal child

```
ExecuteActivity (interceptor.Do, forward argv)
  └─ on Executed: ArmRevert — durable record (migration: commit_confirm table, armed state) +
     a child workflow holding ONE timer (the confirm window, per-class)
        ├─ ConfirmSignal (from the terminus verify ONLY: mechanical `match` over the committed
        │   prediction, the same reconcile.go tail that claims graduation credit)
        │     └─ cancel timer → state confirmed → ledger append → graduation credit (REQ-2907)
        └─ timer fires → ExecuteActivity(inverse argv via the registry rollback template)
              ├─ inverse Executed+verified → state reverted → ledger + alert + deviation demote
              └─ inverse refused/failed → state revert-failed → ledger + PAGE + breaker trip
```

- **Durability**: the armed state and window live on the workflow + a `commit_confirm` row (single-writer,
  the runtime_posture shape) so a worker restart resumes the watch — never an in-process `time.AfterFunc`.
- **The confirm source** is the EXISTING terminus verify (temporal/runner/reconcile.go) READ TOGETHER
  WITH its observability bit: confirm = `verdict == match AND verified == true`, where `verified` is the
  TG-182 `observedOK` the interceptor already computes. The spec/002 3-value verdict ALONE cannot express
  "could not observe" (an empty observed set folds into the quiet `match`), so the bit is load-bearing —
  the judgement already exists at the terminus; what is NEW is only the plumbing that routes
  (verdict, verified) to the child workflow as the confirm/refuse signal. Commit-confirmed adds the armed
  timer, that routing, and the fired-inverse arm.
- **The inverse** re-enters `interceptor.Do` as a first-class request (REQ-2903): same gates, its own
  action_id derived from the inverse action, cross-linked to the forward action_id on the ledger and in
  `commit_confirm`. The registry's `rollback_template` (spec/013) is the only argv source; `start` ↔
  `stop` is the standing example of a forward that is NOT its own inverse.
- **Eligibility + window as data** (REQ-2904): `commit_confirmed: {eligible: true, window: "10m"}` on
  OpClassSpec, validated in ValidateSpec (closed shapes, conservative floor), `omitempty` for overlay
  hash stability — the requires_target_state / TG-378 pattern repeated.
- **Canary coupling** (REQ-2905): the staged-canary allowlist consult (cmd/worker) gains "canary ⇒
  commit-confirmed mandatory"; refusal to arm refuses the forward.
- **Console**: the workflow timeline gains armed/confirmed/reverted chips from the `commit_confirm` row —
  read-only, the workflows-view pattern.

## Guardrails carried from the ticket

1. The inverse is argv-only + registry-gated + floor-checked (a rollback is still a mutation).
2. Non-reversible classes stay human-poll; never auto-confirm eligible.
3. The confirm comes from computeVerdict/the terminus, never the LLM.
4. Conservative default window (minutes), per-class tunable, ≥ 2× the monitoring poll cycle.

## Sign-off rulings (owner-ruled 2026-08-14, TG-488 B5 — this closes the TG-82 gate)

The four questions this section used to carry are RULED; each answer is binding on T-029-1..5:

- **TG-146-S3/S4 sequencing** — RULED: the durable graduation-ladder CAS / multi-worker demotion
  visibility (S3/S4) lands BEFORE commit-confirmed drives demotions from fired reverts at threshold>1.
- **Window vs the deferred-verify lane (spec/017)** — RULED: awx-launch classes are ELIGIBLE in v1, and
  each awx class's confirm window MUST exceed the spec/017 deferred-verify bound — validated as data at
  registry load alongside REQ-2904's per-class tunable.
- **Unverifiable post-state** — RULED: HOLD + page, revert only on observed deviation. REQ-2902 was
  amended at sign-off exactly as its draft flag provided; the decision lives in that one requirement.
- **Scope of v1** — RULED: restart-service / reload-service / start-guest (inverse: stop) — the wider
  option, chosen over restart-* only.

## Amendment 2026-11-07 — confirm and credit require the terminus facts (TG-662, G4A findings F3/F8)

**What was wrong.** The window confirmed on the execute-time verdict `match` alone. That verdict EXCLUDES the
action's own target host (core/verify/verdict.go), so a heal whose target kept alerting confirmed, credited and
graduated; the parent's ConfirmedClear and the TG-483 collateral re-check never reached the window — the
confirm-input nudge was sent BEFORE the clear loop, and the window (600 s) elapses before most clears settle
(3-min ticks, 30-min bound). ConfirmedClear itself was host-quiet only: a quiet host with the unit still down
read as clear. The terminus withheld credit for commit-confirmed classes (T-029-4) and so two authors with two
predicates decided autonomy.

**What changed.**
- **The terminus is the ONE credit author for every class** (`recordGraduationCredit`): clean ⇔ poll answered ∧
  verdict match ∧ confirmed clear ∧ collateral OBSERVED quiet. A commit-confirmed class is credited only once
  its window has resolved `confirmed`; otherwise the terminus withholds and the window's own confirmed feed
  credits (the `graduation_credit` claim key dedups the race). The T-029-4 early return is gone.
- **ConfirmedClear requires an object read** on top of host-quiet (`ObserveObjectStateActivity`, the clear
  loop): unit active / container running / guest running, read through the triage plane's read-only
  diagnostic lane (hostdiag's own allowlisted `check-host-services` argvs — nothing new reaches a host) and
  the guest-liveness projection. Observed=false (unmonitored target, refused/failed read, unknown family) is
  INDETERMINATE: never a clear; the loop keeps watching until its bound.
- **The terminus facts are persisted** (migration 0122: `confirmed_clear_at`, `collateral_observed` NULL when
  unknown, `terminus_facts_at`) by `MarkTerminusFactsActivity`, after the collateral re-check and before a
  second confirm-input nudge.
- **The consult confirms only on the facts**: `match ∧ confirmed_clear ∧ collateral_observed = false`. New
  readings: `pending-clear` (facts not settled — stay armed; at elapse AWAIT them, 12×3 min, then
  `elapsed_unconfirmed`), `unconfirmed` (settled, never cleared, or collateral unknown — `elapsed_unconfirmed`,
  no page, no credit, nothing fires), `collateral` (positively observed — HOLD + page; the inverse still fires
  only on an observed deviation). An unknown collateral reading never credits (TG-668).
- The TG-499 / TG-461 durable-substitute confirms remain and are gated by the same facts.
- **Review fixes (fresh-eyes review of !1819, 2026-11-07).** (a) The facts branch is reachable only for a
  history whose clear loop ran the object read (`objectReadV1`): an in-flight pre-TG-662 history replays
  `terminus-object-read` at DefaultVersion and takes the legacy mark, so a host-quiet-only clear is never
  persisted as a fact. (b) The consult treats facts settled BEFORE the judged execution as PENDING (a re-triaged
  external_ref keeps an earlier run's facts until this run's terminus writes its own). (c) A test pins the
  facts-await bound (12×3 min after the window) above the clear-loop bound (30 min + debounce + margin) for every
  commit-confirmed class. (d) The window's confirmed feed documents why it need not re-check `!PollUnanswered`
  and `HasVerdict`: both are structurally true for an armed window.
- **Causality and explicit classification (the owner's second prompt of 2026-11-07 §2.D).**
  `MarkTerminusFactsActivity` classifies the heal's causality (`causality.go`) from TG's own durable evidence —
  `injected_fault` restores/injections on the host (and the target) over the interval, `ingest_transition`
  recoveries between the incident and the execution, and the actor-evidence readers' attribution over
  [execute, clear] through the deterministic core (`attribution.Attribute`: TG's own identity is SELF, the
  carve-out is AUTHORIZED-TEST = the injector's hand, a sanctioned or unknown principal is another actor) — and
  persists it with the facts (`session_triage.causality`, `causality_evidence`). `recordGraduationCredit`
  classifies every executed terminus explicitly (`clean` | `withheld:causality-<v>` | `withheld:window-<state>` |
  `not-clean:<first failing conjunct>`), feeds the ladder only for `clean` and `not-clean`, requires `tg-caused`
  for `clean`, and appends the APPEND-ONLY `terminus_record` row (migration 0122; tg_runtime holds SELECT,
  INSERT; a table CHECK refuses `credit_clean` without `tg-caused`). The precedent lesson uses the same
  `targetVerified` predicate plus `tg-caused`; the caution lane is its target-state negation, so a withheld
  causality teaches nothing in either direction. `ResolveCommitConfirmActivity`'s confirmed feed reads the
  persisted causality and withholds anything but `tg-caused`. The composition root wires the readers in
  `cmd/worker/causality_wire.go`; without a pool every causality reads indeterminate (no clean credit, no clean
  lesson). The actor reading is attributed PER HOST (the incident subject and the action's target — the core admits
  only records naming the subject it is asked about) and the worst candidate taxonomy wins (the core leaves
  `Taxonomy` at its zero value with more than one candidate, so `{self, authorized}` is read from `Candidates`, never
  as nobody); it counts as COVERED only when a reader emitted a coverage marker or a record for the subject — a
  reader that answered without covering is blind, not quiet, and reads indeterminate (review of !1819, round 2).
  The WINDOW's decision is a row of its own (`terminus_record.source = 'window'`, final review of !1819): a
  commit-confirmed class is normally `withheld:window-armed` at the terminus and credited or demoted by
  `ResolveCommitConfirmActivity`, which appends `clean:window-confirmed` | `withheld:window-confirmed-causality-<v>` |
  `withheld:window-confirmed-facts-unreadable` | `not-clean:window-<reverted|revert_failed>` | `withheld:window-<state>`
  with what it actually fed. At most one window row lands per (external_ref, action_id) — commit_confirm's own key —
  through a partial unique index and `ON CONFLICT DO NOTHING`, so a retried resolve tail appends nothing twice. A
  `confirmed` window is NOT a credit: anything that needs "TG healed it" reads `credit_clean`, never the window state.

**Requirements touched.** REQ-2902 (the confirm predicate), REQ-2907 (the credit author, causality, the explicit
classification). The feature file's scenarios keep their names; the mapping names the new oracles
(terminus_facts_test.go, causality_test.go).

## Amendment 2026-11-08: a superseded window never fires its inverse (TG-664, G4A F6)

An armed window held nothing, and its inverse never checked whether a LATER execution had moved its resource. A
stale start-guest window could fire stop-guest after another session healed a unit inside that guest.

**Where it is checked.** `SealCommitConfirmInverseActivity` now reads every execution recorded after the window
ARMED, through `ActionExecutionStore.ExecutionsSince`, with the sealed action of each.

**What supersedes.** Any execution other than the window's own forward (same incident) whose resource path overlaps
the forward's (spec/013 gate 4h2: same guest, unit or container, or one containing the other). An execution with no
readable manifest counts as its whole host.

**The superseded state.** The window resolves `superseded` (migration 0125), a new terminal state:
- the inverse never fires;
- nothing is credited (`withheld:window-superseded`);
- it pages;
- it is NOT a revert failure, so the mutation breaker does not trip.

**When the check cannot be answered** (a read error, or a read that comes back full), the window resolves
`held_unverifiable` and pages. It never fires blind. The production reader carries the capability; a compile-time
assertion pins it.

## Amendment 2026-11-08: who moved the guest decides the inverse (TG-702, G4A audit A2)

Ledger 23624/23625: an armed start-guest window, in its hold-watch, saw its target alerting because a person had shut the guest down. The inverse (stop-guest) failed its precondition, the window read that as an unrevertable revert, and the breaker forced the deployment to Shadow. The same path would have stopped the guest if that person had left it running. A state-only precondition cannot tell these apart.

**The check.** `SealCommitConfirmInverseActivity`, inside the class-inverse precondition block, reads the guest's actor evidence from the window's arm time to now through the terminus's causality reader.

| Actor reading | Precondition met | Precondition violated |
|---|---|---|
| Authorized or test carve-out actor | `superseded` | `superseded` |
| Only TG, or nothing | inverse fires | `revert_failed`, breaker trips |
| Unsanctioned actor | `held_unverifiable`, pages | `revert_failed`, breaker trips |
| Unreadable, uncovered, or a record bound to no host | `held_unverifiable`, pages | `revert_failed`, breaker trips |

An unreadable guest state stays `revert_failed`, as before. Kill-switch, operator and cost trips are unchanged.

