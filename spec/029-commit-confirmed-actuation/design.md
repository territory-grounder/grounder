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
