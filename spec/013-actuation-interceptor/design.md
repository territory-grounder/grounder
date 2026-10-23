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
  4c. **Verifiability gate**: a mutating action executes ONLY if a post-execution observer is wired
      (`Request.Observe` non-nil) — with no observer, `ComputeVerdict` would run against a nil observation
      and return `match` for everything, making the verifier theater; refuse BEFORE execute (TG-182).
      **Fail closed by design, not by accident (TG-234, 2026-08-03):** every later `Observe` call assumed
      4c had refused a nil observer, so DELETING 4c was a nil-pointer panic — no ledger entry, no verdict,
      no operator-legible reason, and a non-discriminating mutation control. A backstop at the baseline
      step now refuses a nil observer with its own distinct reason ("nil observer reached the baseline
      step"), and the gate drill pins 4c's OWN wording ("no post-execution observer wired"), so the two
      layers are separately provable: deleting 4c is a red drill, and a nil observer can never crash the
      chain.
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
      bypass. A nil decider is a documented pass-through (the mode chokepoint still gates). The `EvalInput` the
      interceptor hands `Decide` is enriched with the target host's **object-group membership** (TG-481, spec/016
      REQ-1618): the interceptor's `WithObjectGroupResolver` is wired to the credential engine's `GroupsFor`, the
      SAME accessor the credential resolver reads, so a group-scoped policy rule and a group-scoped credential
      rule consume ONE object-group definition — never a second. The resolver is optional and nil-safe: an
      unwired interceptor (or a host in no group) sends no groups, decision byte-identical to before, and since
      the default ruleset scopes no rule by group, wiring it changes nothing live until an operator authors a
      group-scoped rule.
  4d2. **AuthN compose** (spec/016 REQ-1604, T-016-5): resolve the TARGET IDENTITY as its own control
      layer — after 4d's authorization verdict, before anything executes — so authentication composes with
      authorization instead of hiding inside an effect leaf. Wired via `Interceptor.WithComposer` to
      `credential.Composer.Compose` (the audited resolver — every compose-time resolution appends a
      credential_resolution row). Fail-closed WHEN WIRED: a target the operator declared no identity for
      refuses at this named gate, placed between 4d and 4e in chain order. A nil composer is a documented
      pass-through whose gate row STATES the control is unarmed ("no composer wired — identity remains the
      effect leaf's static configuration"); the production composition root wires it only when the operator
      has declared credential rules, so an empty rule table can never brick the chain. Order is drilled:
      the gate-drill matrix refuses through it, and the compose oracles prove a DENY at 4d never reaches
      the composer (the executed killing mutation moved the gate above 4d and went red on exactly that).
  4j. **Pre-mutation state capture** (TG-58, a Phase-2 governed-autonomy prerequisite — NOT a gate): at the LAST
      pre-effect instant, after every safety gate above has passed and immediately before `Exec`, an OPTIONAL
      `Request.CaptureState` hook snapshots the target's rollback-relevant state (one bounded retry); after a
      confirmed real mutation an OPTIONAL `PreStateSink` persists it bound to the action_id (the no-op and refused
      paths return earlier, so only a real mutation records one). Nil hook / nil sink ⇒ dark, the pre-effect path
      byte-identical. A FAILED capture is audited but NEVER refuses — this is the deliberate contrast with 4c/4i,
      which fail closed on a nil/failed read because they establish SAFETY: pre-state capture is rollback PREP (a
      Phase-2 applied-undo's concrete restore point), and a missing capture makes a mutation un-restorable, never
      unsafe. The recorded inverse (execution_log, INV-07) says HOW to undo; this captures WHAT the world was.
  5. `actuator.Exec(ctx, argv, stdin)` — the single chokepoint, argv-only, no shell (INV-02).
  6. `verify.ComputeVerdict(pred, observed)` — the deterministic verifier writes the only verdict
     (REQ-1207, INV-10). When a `VerdictSink` is wired (the pgx `db.VerdictStore`) the verdict is persisted
     durably (one per action_id); a persist failure surfaces on the Outcome — the execution stands (it cannot
     be un-done), so the caller learns the verdict was not durably written and can reconcile. The interceptor
     records the `executed` then `verified` stages on the manifest's immutable lifecycle chain and asserts
     `VerifyChain` — the whole chain binds this one action_id in lifecycle order (INV-07); a chain gap on an
     already-executed action is surfaced, not swallowed.
  7. `ledger.Append` — the governed decision on the tamper-evident spine (REQ-1207, INV-19).
  8. **Graduation earn-path feed** (spec/015 REQ-1514, REQ-1223) — on the post-verify tail of an executed
     action the verified run outcome is fed to the per-op-class ladder, but this immediate feed acts on ONLY
     ONE outcome: a `deviation`. The ~1s verify runs against a monitoring surface whose poll cycle is minutes
     long and a baseline that subtracts every already-firing alert, so a divergence observed that fast is real —
     a fast demote+trip is exactly what safety wants. A `match`, a `partial`, and an UNOBSERVABLE post-state
     (verified=false) are all "too early to tell" and are DEFERRED to the decider that re-observes past the
     refresh: the session terminus (temporal/runner/reconcile.go), or a commit-confirmed class's window
     resolution. That decider is the SOLE promoter and the sole fail-closed-against-laundering point — an
     unobservable run cannot promote there (terminus clean=false ⇒ reset; commit-confirm confirmed-only ⇒
     demote/nothing), so TG-182 holds at its true home. Acting on any non-deviation HERE would RESET the
     consecutive-clean streak a slow-settling heal (a guest still booting at T+1s most of all) is about to
     earn — capping it below the promote bar forever (TG-550: 13 verified-clean start-guest heals stuck
     oscillating at clean_run_count 2). Because this path NEVER credits a clean run, deferring the reset opens
     no promotion path. A record failure is non-fatal to the already-executed, already-audited action.
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

`Outcome.AsyncHandle` (TG-122 slice 0, spec/017 REQ-1709/1718) is DATA the interceptor never writes: for a
handle-returning async launch, the regime composition seam fills it after `Do` returns, from the handle its
capture decorator observed at the leaf, so the deferred-verify producer can bind it. The chain itself stays
launch-shape agnostic — no gate reads or branches on the field, and it is empty for every synchronous lane
and every refusal.

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
  every completion it accrues `tokens × TG_COST_RATE_<model>_PER_1K` (falling back to
  `TG_COST_DEFAULT_RATE_PER_1K`) into a durable UTC-day accumulator and a per-session accumulator. This is the
  cleanest hook — right at the gateway boundary — and needs NO change to the runner activities or the
  interceptor. `AccrueActuation` adds the per-actuation increment (`TG_COST_PER_ACTUATION_USD`) to the same
  accumulators (inert while mutation is OFF, armed for the flip).
  - **`tokens` is the PROVIDER-REPORTED figure (TG-44), not an estimate.** This paragraph used to read
    "`approxTokens(request+response)` … the gateway returns no usage count". The gateway *does* return a
    usage count — LiteLLM sends the OpenAI-compatible `usage` block on every completion — and TG decoded
    responses into a struct with no field for it, so it was dropped and the guard billed a chars/4 guess.
    Measured against the live gateway on 2026-08-04: a 47-char prompt estimates 12 tokens and reported 166;
    a 3409-char prompt estimates 852 and reported 1607. The estimate is LOW and the error grows as the
    prompt shrinks, so a `$X` daily budget permitted roughly `$2X` of real spend before tripping. The
    chars/4 fallback survives for a provider that reports nothing — a spend guard must not go dark because
    a field is missing — but it is logged once per tier and counted on `tg_model_usage_missing_total`, so a
    budget being enforced on a guess is visible rather than assumed.
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

## The composed op-class registry — two unequal tamper domains (spec/028 REQ-2814/REQ-2815, ADR-0016)

`opschema` is the one place an actuatable op-class's argv is constructed, so it is the natural place a
capability is *granted*. Until spec/028 there was exactly one way to grant one: add an entry to
`opschema.json`, which is `go:embed`-compiled and lockstep-hashed — admission is a CODE RELEASE, reviewed and
hash-bound. That is the strongest tamper story available and it is why a fresh TG can actuate nothing until an
administrator hand-authors a catalog.

`core/actuate/opschema/overlay.go` adds the second, deliberately WEAKER domain: op-classes an operator
ratified at runtime from an evidence dossier, stored append-only in `opclass_ratified` (migration 0049) and
anchored in the ONE hash-chained ledger. The two domains compose under four rules, and the asymmetry between
them is the whole design:

1. **Embedded always wins a slug collision**, enforced at three seams: `ValidateRatification` refuses a
   shadowing slug at ADMISSION, `SetOverlay` drops one at LOAD, and `Lookup`/`overlaySpecs` prefer the
   embedded entry at RESOLUTION and at ENUMERATION. The resolution/enumeration pair is the second of two
   independent guards — it must hold when admission has been bypassed, which is why it is proven against a
   snapshot planted directly into `overlayPtr` rather than against one built through `SetOverlay`. Covering
   resolution without enumeration would leave the agent's prompt Catalog advertising a capability the reviewed
   registry never granted, even while `Lookup` correctly refused to resolve it.
2. **Every overlay row is hash-re-verified at load.** `entry_hash = CanonicalHash(spec)` is mirrored into the
   row's `opclass:ratify` GovDecision, so the ledger attests row CONTENT, not merely that a ratification
   happened. A mismatch DROPS the row loudly. The failure direction is therefore always toward FEWER
   capabilities: a tampered row removes a capability instead of granting a forged one, and the dropped class
   falls back to rung 0 (registry absence) where it seals to nil argv and every effect leaf refuses it.
3. **`ValidateSpec` is the error-returning form of the registry's own admission checks.** Those checks were
   written as init-time panics, which is correct for compiled-in entries: a malformed embedded entry is a build
   defect and dying at boot is right. Ratification changes the threat model — the spec now arrives at RUNTIME
   from an operator form, and a live worker that panics on operator input is a denial-of-service with an audit
   trail. Same rules, same messages, returned instead of thrown; `mustBuildRegistry` calls it and panics on
   error, so embedded behavior is preserved exactly.
4. **`IsEmbedded` is the AUTO ceiling** (ADR-0016 decision 2). It answers for the EMBEDDED registry alone,
   never the composed one. The graduation ladder consults it — not `Lookup` — before promoting a class to the
   SILENT rung: an overlay-only class caps at `auto_notice` ("acts and pages"), and reaching `auto` requires a
   code release via embed-export. Implementing it over the composed view would let ratification lift its own
   ceiling, which is precisely the tamper-domain collapse ADR-0016 exists to prevent. The ceiling lives here
   and in `core/policy` rather than as a column in `opclass_ratified` because a constraint in that table would
   be a rule the overlay applies to itself, and self-applied ceilings are the ones that get edited.

`ValidateRatification` adds the two admission rules that exist only because the spec arrived at runtime: a
claimed safety tier is refused when the server's own reading (`IsDestructiveOp`/`IsNeverAuto`) contradicts it —
a tier CLAIM may never soften what the op does, or the most dangerous class buys the fastest ladder climb — and
the **laundering tripwire** refuses any template element that byte-matches the model's screened text. TG's
constitutional line is that a model never writes its own tools (ARCHITECTURE §T3), but the operator ratifies
while LOOKING at the model's suggestion, and a copy-paste would launder model output into an executed argv
while every other check still passed, because the form was filled by a human and authorship looks satisfied.
The operator may express the same intent; they may not transcribe the model's string.

### The day-zero profile — making the EMPTY catalog reachable (`TG_DAYZERO_EMPTY_CATALOG`, ADR-0016)

ADR-0016's first consequence is that "day-zero TG (empty catalog) is a full-capability shadow adviser and can
execute nothing — the predecessor's shadow posture with zero configuration." That claim had **no reachable
code path**. `mustBuildRegistry` enforces the correspondence between the loadable schema and the compiled
builders in BOTH directions, so emptying `opschema.json` made all seven builders orphaned and the binary
panicked at init. The posture the earned-ladder epic exists to deliver could not be booted, let alone
measured against the predecessor — which runs open-world, with no hand-authored catalog, by construction.

`schemaForProfile` composes an EMPTY catalog when the profile is set, and the orphan-builder panic is
suspended in that one case. The suspension is scoped deliberately and removes no protection:

- The check catches a BUILD DEFECT — a builder someone forgot to give a schema. Under an explicitly
  requested empty catalog, an orphan is the requested state, not a defect.
- An orphaned builder is unreachable precisely BECAUSE nothing can look it up. Execution requires a spec;
  `Lookup` returns none; the four independent downstream refusals (nil `sealedArgv`, the empty-argv leaf
  refusal, the never-auto floor, the mode chokepoint) each stop an actuation on their own.
- The profile can only ever REMOVE capability, so it cannot make anything execute that would not have
  executed anyway. Its failure direction is toward fewer capabilities, like every other rule here.
- It is read once at init and matches an exact `"1"` — a profile that armed on any truthy-looking value
  could silently stop an estate healing.

An oracle asserts the orphan panic still fires when the profile is OFF, so the fail-closed correspondence
check is not weakened for normal deployments; that control is what keeps the suspension honest rather than
convenient.

## Amendment 2026-08-09 — gate-verdict margins (TG-178)

The OBSERVE-ONLY per-gate verdict trail (spec/020 T-020-7) that the interceptor emits now also carries an
optional signed MARGIN — how far a gate's decision was from its numeric threshold (value − threshold). The
first producer is the policy gate: when the confidence gate is active it records `confidence − min_confidence`
(read off the non-secret `PolicyDecision` refine record), so a decision that auto-authorized within ε of the
auto→approve clamp becomes a reviewable boundary case (the skill-store flywheel's preferential input). This
changes NOTHING about how any gate decides: the emit is a pure side effect on a nil-tolerant sink, the margin
is never read back, and an emit error still cannot change a gate outcome or let a refused action through. The
gate chain, its order, and every fail-closed correspondence are unchanged.

A SECOND producer now emits alongside it: the policy gate also records a `policy-band` row carrying the
band-composition margin — the rank distance from the policy verdict to the band's verdict floor
(`verdictRank(policy) − verdictRank(floor)`, on the non-secret `ComposeRecord.BandMarginRank`), where 0 means
the verdict landed exactly on the floor (the boundary). Same OBSERVE-ONLY contract as the min_confidence
margin: a pure side effect, never read back, an emit error changes no outcome.

A THIRD producer is the actuation-frequency gate (TG-166a): its `actuation-limit` pass row now carries the
RATE-BUDGET margin — the tightest slack that remained after the admission was charged, `min` over the session
and target scopes of `(per-window cap − trailing-window count)` (the `ActuationLease.headroom` the limiter
computes at admit time). Zero means this actuation consumed the last slot before the frequency throttle — the
boundary case a reviewer wants to see. It deliberately tracks the per-window RATE budget, not the in-flight
concurrency cap, which is a binary mutex (default 1) whose slack is always zero on the pass path and so carries
no "how close to the throttle" information. Same OBSERVE-ONLY contract: the margin is a pure side effect, never
read back, and cannot change whether the actuation is admitted (the refusal path is unchanged and emits no margin).

A FOURTH producer is the graduation gate (spec/015 step 5): the policy gate now leaves a `graduation` row
carrying how many verified-clean runs the op-class was from its NEXT earned-autonomy rung — the running
clean-run count minus the promote bar (on the non-secret `DecisionAudit.Graduation` record the policy engine
fills after `GraduatedVerdict` resolves). A climbing class sits at −1 or lower, where −1 is "one clean run
short of graduation" — the boundary case the ticket names. A class with no rung left to be short of (already
at `auto`, or not auto-eligible so the graduation hook never consults a rung) records the row with NO margin,
so a nil margin is never read as an at-threshold 0. Same OBSERVE-ONLY contract as the other three: the margin
is read AFTER the verdict resolves, is a pure side effect on the nil-tolerant sink, is never read back, and
an emit error cannot change any gate outcome. The gate chain gains one row (`graduation`, immediately after
`policy-band`); its order and every fail-closed correspondence are otherwise unchanged.

## Out of scope

The Runner workflow that assembles a Request and calls the interceptor in its execute activity is
spec/012. That seam is now WIRED: the execute activity reloads the sealed manifest + committed
prediction and calls `Interceptor.Do`; the worker boot constructs the chain and runs `SelfTest` as a
gate (a dark control fails the boot). While mutation is off the chain refuses at `GuardMutation` and
records the refusal, so the Runner still stops at propose — through the real chain. The mutating
actuator, the human-vote `Approved` binding, and the grounded-territory acknowledgement set are wired
by their own changes (TG-21/TG-31); until then the chain is triple-fail-closed. The RBAC/policy surface
that authorizes an operator to call `EnableMutation` is the console/API layer.

## State preconditions in the op-class registry (2026-08-11, TG-378)

`OpClassSpec` carries `requires_target_state` — a CLOSED per-class state-precondition vocabulary
(`not-running`, declared by `start-guest`; and since spec/029 T-029-3 `running`, declared by `stop-guest`,
the commit-confirmed inverse — the mirror guard: the fired revert of a start must observe its target
RUNNING, so a guest someone else already cycled is never blind-stopped; the auto-fire's seal establishes it
through the same GuestRunning seam with the same fail-closed semantics), validated in `ValidateSpec` exactly
like family/tier so an unknown value refuses at registry load rather than silently never firing. The
ENFORCEMENT lives upstream in the prediction gate (spec/002 REQ-112) for model-proposed actions: established
from the staleness-bounded guest_liveness projection before anything commits or seals, recorded on the
manifest, fail-closed on violated/unknown/unwired. The field is `omitempty` so every overlay that does not
use the vocabulary keeps its ratified CanonicalHash byte-stable (proven by
TestOmitemptyKeepsUndeclaredSpecsHashStable). `stop-guest` itself is registry data + a compiled `[stop,
<guest>]` builder on the proxmox lane (spec/029 REQ-2903: the inverse is a first-class mutation, never a
special path), inverse-only (no fault class provokes it — declared in spec/opcover-exemptions.json) and not
itself commit-confirmed eligible (no revert-of-revert chains). INVERSE-ONLY classes never render in the
proposal catalog (`opschema.Catalog()` derives the declared rollback_op_class targets and skips them):
registered = actuatable (the fired revert needs the compiled builder), rendered = proposable (nothing may
offer the model a stop verb) — the distinction the change gate enforced the day stop-guest entered the
registry, when the extra catalog entry measurably degraded the judged triage dimensions.

## Symmetric execute-time param re-check for awx-launch (2026-08-11, TG-152 L1)

The structure-schema gate re-ran `opschema.ValidateArgs` only on `len(Argv)==0` — the ssh-argv
"argv did not build" defect — while an awx-launch effect always builds `Argv=[LaunchVerb]`, so the one
effect kind whose params travel OUTSIDE the argv (AWX extra_vars) was never re-checked at execute. The
gate now re-runs the SAME registry validator for `EffectAWXLaunch` classes (validator-tolerance ==
builder-tolerance, so nothing the propose path accepted is rejected). Oracles:
`core/actuate/awx_param_recheck_test.go` (missing-param, TG-365 empty-params, and the anti-over-block
complete-params control), killing mutation executed red. The L3 disk-op activation guard remains
owner-gated on AWX configuration (TG-152).

## The adapter-level stateful/destructive floor sees the params (2026-08-11, TG-146 A3)

The never-auto floor's destructive derivation and a NEW adapter-level stateful floor both consume
`manifest.Action.SafetyParts()` — Target, Op, OpClass plus every param KEY and VALUE — the SAME shared
derivation the classify-time floor uses (temporal/runner delegates to it), so the two depths cannot drift.
The stateful arm is band-aware: a stateful identity under a NON-VOTED band (AUTO/AUTO_NOTICE) refuses as a
mis-recorded band; the human-voted POLL_PAUSE lane passes — a stateful mutation's legitimate path. Oracles:
`core/actuate/stateful_floor_test.go` (the measured mariadb-in-params shape, the destructive-param-value
arm, the voted-lane anti-over-block control — whose first draft passed vacuously on the admission refusal
and now carries the vote, and the benign-AUTO untouched arm); both killing mutations executed red.

## The cancelled-effect terminal (2026-08-22, TG-80 P1-4)

A caller cancellation (the execute activity's deadline, or an explicit cancel) that lands while the
effect leaf is RUNNING is now its own terminal, not a generic refusal. The SSH leaf signals the remote
command dead over the still-open channel — `SIGTERM`, a bounded grace, `SIGKILL`, a second grace — and
only then closes the transport, returning the typed `ErrRemoteCancelled` wrapped around the context
error. The interceptor's execute stage classifies that with `errors.Is` on the context sentinels (the
chain never imports a leaf), refuses the action, and sets `Outcome.Cancelled` with a reason that starts
with the stable `RefusalCancelled` token — so a cancelled run is machine-distinguishable from a bad host
key or a dead link, exactly as `RateLimited` separates a throttle from a failure. `Executed` stays false:
nothing completed, and the estate is not left with an orphaned process (the pre-P1-4 behavior closed
the TCP link and left the command running on the target). The runner carries the flag through
`ExecuteResult` into a `cancelled:remote-killed` session outcome recorded before return.

## Pre-flight reachability — gate 4h3 (2026-08-22, TG-81 b4)

When the effect leaf can prove its transport (the OPTIONAL `ReachabilityProber` capability, structural
type-assertion like HostBound/ExecRecorder), the interceptor proves it inside both admission leases and
before the first estate-touching step: an unreachable target refuses cleanly — "aborted before touching
anything" — instead of surfacing minutes later as an effect-leaf exit indistinguishable from a refused
mutation. Clean-room from h-network's all-or-nothing batch pre-flight (attribution:
SOURCE-BENCHMARK-CATALOG): today a manifest carries exactly one target so the blast-radius loop has one
member; when fleet manifests land the same gate iterates every target and one unreachable member aborts
the batch whole. Strict per-target validation rides the same gate: a probing leaf refuses an EMPTY
(whitespace) target outright — manifest.New seals such an action without complaint, so the gate is where
"nothing to probe" becomes a refusal rather than a pass. Deliberately AFTER the 4h budget charge (a free
pre-lease refusal would hand a subverted agent unlimited retries against a down host); its refusal
releases the 4h2 durable claim undisturbed. A leaf without the capability is a documented pass-through —
the exec failure downstream still fails closed; this gate only moves the refusal earlier and names it.

## Durable per-target admission + cooldown — gate 4h2 (2026-08-22, TG-81 b2)

The in-process frequency governor (gate 4h, TG-166a) leases session/target concurrency inside ONE
worker; gate 4h2 is the half a per-process lease cannot deliver. A new optional collaborator
(`TargetAdmission`, armed via `WithTargetAdmission`, backed by `actuation_target_state` / migration
0107, actuation-plane only) atomically claims the action's TARGET in the shared database directly after
the 4h lease and holds the claim for the whole pre-effect sequence — so "one actuation in flight per
target" is true across every actuation-capable process, the same durability argument that moved the
mutation breaker to migration 0021. The second tier is cooldown-on-error: a DISTURBED effect (transport
error, killed mid-flight, non-zero exit) releases the claim with a 120-second cooldown stamped on the
target — the next hand waits out the dust instead of piling onto a target in unknown state. A refusal
between admission and the effect releases undisturbed — including a gate-4h3 reachability refusal,
which sits directly after this claim. Posture is the h-ssh active-set INVERTED (attribution:
SOURCE-BENCHMARK-CATALOG, clean-room): once armed, a held claim, an active cooldown and an unreachable
store all REFUSE with the store's reason; a stale claim (crashed worker) ages out via the claim TTL
enforced in the claim SQL. A nil collaborator is the documented unarmed pass-through, the
mutation-breaker convention; the composition root shares ONE store across the direct chain and every
regime lane, exactly as it shares the 4h limiter.

## The static compensatability authority (2026-08-22, spec/030 T-030-1)

`OpClassSpec.SafelyCompensatable()` exports the STATIC half of the reversibility criterion that
`rollbackArgvFor` (temporal/runner) has enforced per-call since the manual-rollback endpoint: a class
has a safe inverse only when it is tier low-reversible AND either declares a `rollback_template` or its
op is a genuine idempotent-reconvergence verb (restart/reload). The transaction-plan recipe registry
(spec/030 REQ-3004) consults it at BUILD time so a step whose "rollback" would silently re-run the
forward — the start-with-no-declared-inverse shape — can never join an all-or-nothing plan. One
criterion, two enforcement points, declared beside the data it reads; the runtime half (params in hand,
forward sealed reversible, non-empty argv) stays with the rollback executor.
