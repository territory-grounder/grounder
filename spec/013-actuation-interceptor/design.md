<!-- spec/013 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/013 — Design: wired-by-construction actuation interceptor + mutation gate

How the requirements in `requirements.md` are realized on the Go stack. Where this design and the code
disagree, the code is the bug and this document is the intent. This composes the already-built controls
(the mechanical safety core, the prediction gate, the mechanical verdict, the ledger) into the effect
channel; none of the predecessor's swallowed-exception "observe-only" degradation is carried.

## Components

- **`actuate.Interceptor`** (`core/actuate/interceptor.go`) — the wired chain. Its `actuator` field is
  UNEXPORTED, so the only way to reach `Exec` is `Do`; there is no exported bypass (REQ-1201, S8-5).
  `SelfTest` fails loud if any collaborator is nil (REQ-1202). `Do` runs, in order:
  1. `MutationGate.GuardMutation` — off ⇒ refuse (REQ-1203, INV-09).
  1b. **Admission:** a `POLL_PAUSE`-band action may auto-execute ONLY with a recorded approval
      (`Request.Approved`) — the vote binds the decision (INV-12); an `AUTO`/`AUTO_NOTICE` band was already
      admitted by the classifier. A poll band reaching execute unapproved is a control gap ⇒ refuse.
  2. `safety.IsNeverAuto(op) || !Action.Reversible || safety.IsDestructiveOp(op, op_class)` — the never-auto
     floor at the adapter, defense in depth ⇒ refuse even with mutation on (REQ-1203, INV-09). The floor also
     re-derives destructiveness from the ACTUAL command, so a model that UNDER-declares its op_class (a
     `kubectl delete pvc` sealed as a benign reversible "restart-service") cannot slip the chokepoint — the
     adapter floor must not be weaker than the admission classifier, which applies the same override. A plan
     cannot hide a mutation.
  3. `Gated` false ⇒ refuse; `Manifest.Assert(action_id)` mismatch ⇒ refuse — structure gate, not a
     command-string blocklist (REQ-1204, INV-06/07).
  4. no bound `Evidence` ⇒ refuse (REQ-1205, INV-11).
  4b. **Territory gate** (the namesake control): a mutating action inside a high-stakes territory
      (`core/territory` — k8s/network/edge/pve/native/docker) whose operating manual was not acknowledged this
      session (`Request.Acknowledged`) ⇒ refuse; a confirmed infra write the gate cannot place fails CLOSED.
      This composes the grounding prerequisite into the chokepoint (INV-21).
  4d. **Policy authorize** (`PolicyDecider.Decide`, spec/015 REQ-1506, audited via the `AuditedEngine`,
      REQ-1518): honor the per-action verdict by its REQ-1506 meaning — `deny` refuses unconditionally (no
      recorded approval lifts a deny); `approve` is "route to a human vote", so it PROCEEDS only when that vote
      is on file (`Request.Approved`, bound by RecordVote, INV-12) and otherwise refuses (fail closed — a second
      floor beneath admission 1b); `auto` proceeds. Honoring a recorded approval on an `approve` verdict is how
      an ungraduated op-class earns its verified-clean runs toward `auto` (spec/015 REQ-1514) — an unseen class
      always resolves to `approve`, so without this the graduation ladder dead-locks and no class can ever
      execute its first human-approved run. This is an INDEPENDENT layer from the mode chokepoint (REQ-1521):
      even a proceed here cannot actuate while the mode is not Semi-auto/Full-auto. The never-auto floor (step 2)
      already refused every irreversible/destructive op BEFORE here, so honoring an approval opens no floor
      bypass. A nil decider is a documented pass-through (the mode chokepoint still gates).
  5. `actuator.Exec(ctx, argv, stdin)` — the single chokepoint, argv-only, no shell (INV-02).
  6. `verify.ComputeVerdict(pred, observed)` — the deterministic verifier writes the only verdict
     (REQ-1207, INV-10). When a `VerdictSink` is wired (the pgx `db.VerdictStore`) the verdict is persisted
     durably (one per action_id); a persist failure surfaces on the Outcome — the execution stands (it cannot
     be un-done), so the caller learns the verdict was not durably written and can reconcile. The interceptor
     records the `executed` then `verified` stages on the manifest's immutable lifecycle chain and asserts
     `VerifyChain` — the whole chain binds this one action_id in lifecycle order (INV-07); a chain gap on an
     already-executed action is surfaced, not swallowed.
  7. `ledger.Append` — the governed decision on the tamper-evident spine (REQ-1207, INV-19).
  Every refusal returns a `Refused` outcome AND records it — never an observe-only pass.
- **`actuate.EnableMutation`** (`core/actuate/mutation.go`) — the SOLE path to turn mutation on. It
  requires `Interceptor.SelfTest` to pass, then `MutationGate.MarkPreflightGreen` + `TryEnableMutation`.
  Mutation defaults off; enabling is an explicit, audited operational act earned by the wired chain
  (REQ-1206, INV-09/21).

## Fail-closed / fail-loud composition

The `Outcome` zero value is neither executed nor a verdict — an unhandled path yields a non-executing
outcome. `Do` returns an error only for an unwired chain (REQ-1202); every policy refusal is a recorded
`Refused` outcome, so a control can never be silently skipped. Because `EnableMutation` is the only path
to the enabled state and it gates on `SelfTest`, mutation cannot be turned on onto an unwired base — the
constitutional "gate must be trustworthy before anything mutates" made structural.

## Decision procedure (per actuation)

1. Chain unwired ⇒ error, no execution (REQ-1202).
2. Mutation off ⇒ refuse (REQ-1203).
3. Floor/irreversible op ⇒ refuse, even with mutation on (REQ-1203).
4. Ungated / action_id mismatch ⇒ refuse (REQ-1204).
5. Evidence unbound ⇒ refuse (REQ-1205).
6. Else execute → verify → audit (REQ-1207).

## Cost/budget spend guard — the $-ceiling breaker (REQ-1211..1215)

The cost breaker (`core/cost.Accountant` over `core/cost.Store`; pgx `core/db.CostStore` + migration 0023,
in-memory `cost.MemStore` twin) is the INDEPENDENT spend-guard sibling of the mutation breaker. It composes
over the SAME kill wire — `ShadowForcer.ForceShadow` on the mode chokepoint — but guards money, not a safety
invariant, so it inverts one thing deliberately: it FAILS OPEN.

- **Accrual hook (REQ-1211).** `cost.MeteringCompleter` WRAPS the model gateway the agent calls, composed at
  the worker composition root (`cmd/worker/main.go`) around `gw` before it becomes `runner.Deps.Model`. On
  every completion it accrues `approxTokens(request+response) × TG_COST_RATE_<model>_PER_1K` (falling back to
  `TG_COST_DEFAULT_RATE_PER_1K`) into a durable UTC-day accumulator and a per-session accumulator. This is the
  cleanest hook — right where TG already sees the request/response text (the gateway returns no usage count) —
  and needs NO change to the runner activities or the interceptor. `AccrueActuation` adds the per-actuation
  increment (`TG_COST_PER_ACTUATION_USD`) to the same accumulators (inert while mutation is OFF, armed for the
  flip).
- **Trip → force-Shadow (REQ-1212).** When the shared day total reaches `TG_COST_DAILY_BUDGET_USD` or a
  session total reaches `TG_COST_SESSION_CEILING_USD`, the breaker forces the mode to Shadow and appends a
  `cost:breaker-trip` decision to the tamper-evident ledger (`costLedgerTripRecorder`). Under Shadow the force
  is a no-op (nothing to halt), so — like the mutation breaker — the HALT is inert today; unlike it, the guard
  still ACCRUES under Shadow (read-only investigation spends tokens), so it can trip and record now.
- **Cross-process (REQ-1213).** The accumulators and the `cost_breaker_state` row are durable and shared
  (migration 0023, latest-wins/additive upsert like `mutation_breaker_state`). Every completion first reads the
  shared OPEN state and force-Shadows its own mode if a sibling already tripped — so a budget trip in one
  worker force-Shadows every sibling on its next spend, delivered through the metering path (no interceptor
  consult needed).
- **Disabled (REQ-1214).** Both budgets default to 0 = DISABLED; an unset budget never trips (a spend guard
  that is not configured must not block work). Unconfigured entirely ⇒ the gateway is left un-wrapped (zero
  overhead).
- **Fail-OPEN (REQ-1215).** An unreadable cost store is treated as NOT tripped and LOGGED loudly; it never
  force-Shadows on a read error. This is the DELIBERATE inverse of the mutation breaker's fail-CLOSED
  (REQ-1210): the mutation breaker guards a SAFETY floor (an unobservable safety breaker reads OPEN so a
  sibling can never actuate on it), while the cost breaker guards SPEND (a cost-store outage is not a safety
  event, so it must degrade to "no enforcement", never to a halt). The threat-model confirms this is the right
  call — a fail-CLOSED cost breaker would turn a metrics/DB blip into a self-inflicted global outage.

It never enables actuation, never weakens the never-auto floor / mutation breaker / chokepoint, and does not
route through the interceptor's Execute path — it is a purely additive spend halt.

## Mutation-breaker recovery — `MutationBreaker.Rearm` (spec/015 REQ-1525)

The interceptor consults `MutationBreaker.Tripped()` before it executes (REQ-1210, fail-CLOSED), and a
deviation trip opens the durable, cross-process `mutation_breaker_state` row. That trip was previously
IRREVERSIBLE — the breaker only ever `RecordFailure`s (Trip) and reads state; it never calls `Allow`, so it
has no automatic open→half-open→closed recovery, and a single trip (even a false one) refused every actuation
forever. `MutationBreaker.Rearm` (over `breaker.Breaker.Reset`) is the governed recovery: it force-closes the
row and resets the deviation counter. It is NEVER called automatically from the interceptor or the breaker
itself — the SOLE caller is the mode controller re-arming on an owner-gated escalation into an actuating mode
(spec/015 REQ-1525), so the safety breaker still cannot self-heal; recovery is a deliberate, ledgered
(`safety:breaker-rearm`), operator-authorized action symmetric with the trip. Fail-safe: a re-arm that cannot
persist leaves the breaker OPEN.

## Out of scope

The Runner workflow that assembles a Request and calls the interceptor in its execute activity is
spec/012. That seam is now WIRED: the execute activity reloads the sealed manifest + committed
prediction and calls `Interceptor.Do`; the worker boot constructs the chain and runs `SelfTest` as a
gate (a dark control fails the boot). While mutation is off the chain refuses at `GuardMutation` and
records the refusal, so the Runner still stops at propose — through the real chain. The mutating
actuator, the human-vote `Approved` binding, and the grounded-territory acknowledgement set are wired
by their own changes (TG-21/TG-31); until then the chain is triple-fail-closed. The RBAC/policy surface
that authorizes an operator to call `EnableMutation` is the console/API layer.
