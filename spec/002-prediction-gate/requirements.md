<!-- spec/002 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/002 — Fail-closed prediction gate + mechanical verdict

**Owning behavior family:** BEH-2 (see [`docs/GOVERNED-BEHAVIORS.md`](../../docs/GOVERNED-BEHAVIORS.md)).
**Constitution / invariants:** INV-06, INV-07, INV-10.
**Phase:** the `PredictActivity` / `VerifyActivity` pair lands in Phase 2; the content-hashed
`ActionManifest` binding they thread over is already built in `core/manifest` (Phase 0/1).
**Status:** Approved.

This is the **remediation lane** — it fails **CLOSED**. Before any approval poll opens, the orchestrator
commits a `plan_hash`-keyed machine consequence prediction computed outside the LLM by the infragraph
model; after execution a deterministic verifier writes the only `match`/`partial`/`deviation` verdict,
which the acting model can never author. A `deviation` (observed reality diverging from the committed
prediction) never auto-resolves. Every stage re-derives and asserts the same content-hashed `action_id`,
so "a prediction exists" can never be mistaken for "the prediction is for the executed action". This
document is the requirement source of record; the design is in `design.md`, the runnable acceptance
oracles are in `acceptance/`, and the engineering tasks are in `tasks.json`.

**A field on the `ActionManifest` is a promise the record carries that fact (TG-66).** The manifest is the
single immutable record the system replays, evaluates and audits against, so every exported field on it
SHALL be either PERSISTED — carrying a column in the manifest table — or declared IN-PROCESS with the
reason it cannot be durable, and a standing check SHALL fail on a field that is neither. A
declared-but-never-written field is worse than an absent one: it names exactly what an auditor is looking
for and always holds the zero value, so the auditor concludes the record does not capture it. `ToolCalls`
was such a field — zero writers, zero readers, no column — and was retired rather than populated, because
the executed argv is already bound by the interceptor and a second copy of a recorded fact can disagree
with the first. `Provenance` and `Stages` are the standing in-process exemptions: the manifest is sealed
ONCE at the predicted stage (first-wins), and persisting the lifecycle chain would require the post-seal
UPDATE the append-only design forbids.

## Requirements

- **REQ-101** — [F] spec/002 · [O] INV-10.
  BEFORE any approval poll activity starts, the orchestrator SHALL commit a `plan_hash`-keyed machine
  consequence prediction — computed outside the LLM by the infragraph model — to the append-only
  prediction store, enforced by the Temporal activity ordering
  `PredictActivity → ApprovalActivity → ExecuteActivity → VerifyActivity` under which an approval poll
  activity cannot start without a persisted prediction. The committed prediction SHALL render to a
  single judge-readable summary (`verify.Prediction.Summary()`) that the offline eval and the live
  Runner both use, so falsifiable_prediction is scored over an identical string in both (TG-61).

- **REQ-102** — [F] spec/002 · [O] INV-06/INV-07.
  IF a proposal has no committed prediction, THEN the gate SHALL DENY the approval poll (default-deny),
  and `BuildApprovalPoll` accepts only a `GatedProposal` — a type constructible only by the
  `PredictionGate` activity — so a poll without a committed prediction is uncompilable, closing the H-02
  alternate-grammar bypass.

- **REQ-103** — [F] spec/002 · [O] INV-10.
  AFTER execution, the deterministic verifier `computeVerdict(pred, observed)` SHALL be the sole writer
  of the mechanical `match`/`partial`/`deviation` verdict, diffing observed alerts against the committed
  prediction, where the acting LLM has no write path to the verdict columns (the prediction and
  verification tables grant no UPDATE or DELETE to the model or session role).

- **REQ-103a** — [F] spec/002 · [O] INV-10 (Gulli ch19 rubric / ch4 structured critic).
  In the SAME single pass that decides the verdict, the deterministic verifier SHALL also return a typed
  `VerdictDetail` — the `match`/`partial`/`deviation` verdict PLUS the structured breakdown that produced it:
  the surprise hosts (in-scope, non-target observed hosts the prediction never named, each a `deviation`
  trigger) and the rule mismatches (observed alerts on a predicted host carrying a rule the prediction did
  not name, each a `partial` trigger). The bare `computeVerdict` verdict SHALL be derivable from that detail
  and byte-identical to the pre-existing verdict for every input (deviation dominates partial dominates
  match), so no verdict decision changes; verify-time callers consume the typed detail rather than
  recomputing the surprise/mismatch breakdown from the raw prediction and observation.

- **REQ-103b** — [F] spec/002 · [O] INV-10, INV-19 (measured 2026-07-30, governance_ledger seq 6555).
  A `deviation` trigger SHALL be recorded WITH the alert rule that produced it, and that evidence SHALL reach
  the durable governance record — not only the in-memory detail. Concretely, `VerdictDetail` SHALL carry the
  surprise triggers as (host, rule) pairs in addition to the host list, deduplicated and sorted so the rendering
  is deterministic, and the audit-readable summary SHALL name them.
  RATIONALE: a `partial` trigger already recorded (host, rule) while a `deviation` trigger — the outcome that
  DEMOTES an op-class and TRIPS the estate-wide mutation breaker — recorded only a hostname, so the most
  consequential verdict carried the least evidence. Diagnosing one real deviation
  (`surprise-hosts=[dc2lte01]`, which demoted `restart-container` auto→approve and discarded ~80 hands-off
  clean runs) required six external queries across two monitoring instances and the discovery that one stores
  local time rather than UTC; the triggering alert proved to be an unrelated 59-second sensor flap at the other
  site. An irreversible-in-practice autonomy penalty must not be justified by evidence the system declines to
  retain.
  The host list SHALL remain byte-identical: it is the promotion-gating deviation-key signature
  (`core/falsify` "reproduces ≥ N") and the disproof-host key for recency decay, so widening it would silently
  redefine a promotion gate. This requirement adds evidence only and changes no verdict.

- **REQ-104** — [F] spec/002 · [O] paradigm-rule 8.
  IF the verdict is `deviation` — observed reality diverges from the committed prediction — THEN the
  session SHALL never auto-resolve regardless of band or confidence, and instead routes to POLL_PAUSE
  and the approver graph.

- **REQ-105** — [F] spec/002.
  WHILE the prediction gate is in analysis-only mode (an org-global RBAC-gated policy, the reframe of the predecessor
  `INFRAGRAPH_DISABLED=1`), the gate SHALL record the prediction and its shadow verdict for evaluation
  without blocking the approval, keeping the advisory lane fail-open.

  **DECLARED, NOT BUILT (TG-249 item 6, verified 2026-08-06).** This requirement is Approved and the
  analysis-only lane does not work. `ApprovalPoll.Blocking` is computed from the mode and read by NOBODY —
  written in `core/predict/gate.go`, copied into `GateResult` in `temporal/runner/activities.go`, and
  consulted nowhere in non-test code — so selecting analysis-only would set a flag and leave the poll
  blocking exactly as before. The mode is unreachable in any case: the one production construction is
  `cmd/worker/main.go` with `Mode: predict.ModeEnforce`, and nothing selects otherwise.
  Recorded HERE rather than as a dark wiring seam, deliberately: the wiring register measures lanes that
  RUN and report a yield, and a seam declared there with nothing to observe reports UNOBSERVED forever —
  a permanent complaint an operator learns to scroll past, which is worse than no register at all. An
  unimplemented requirement is a spec-versus-code gap, not a starved lane, and belongs beside the
  requirement it contradicts.
  Until it is built, the deployment has ONE posture: every committed prediction BLOCKS its approval poll.
  An operator reading this requirement alone would conclude otherwise. The zero value of `Mode` SHALL
  remain `ModeEnforce` so a construction that omits the field cannot land in a lane that is both fail-open
  and unimplemented, and `Blocking` SHALL continue to track the mode correctly while unread — a flag that
  is wrong as well as unread would silently invert the posture the day a reader is wired.

- **REQ-102b** — [O] INV-07 (overlay-added binding).
  The committed prediction, the approval choice, the executed tool-calls, and the verdict SHALL all be
  bound to the same immutable content-hashed `ActionManifest`; each stage re-derives and asserts
  `action_id`, a mismatch is a fail-closed hard abort, and any mid-session change to the Action yields a
  new `action_id` that invalidates the prior prediction and approval and forces a child-workflow
  re-gate (closing H-03: "a prediction exists" is not "the prediction is for the executed action").

## Persistence contract

Every gated proposal writes one immutable `infragraph_prediction` row into the append-only prediction
store, stamped `schema_version`, carrying the `plan_hash`, the bound `action_id`
(INV-07), the predicted cascade, the prediction window, and the negative-control columns
`control_tp` / `control_fp`. Every executed action writes one immutable verdict row, authored only by
`computeVerdict`, carrying the `match`/`partial`/`deviation` result, the observed-versus-predicted diff,
and the same `action_id`. Both rows are chained into the tamper-evident governance ledger (INV-19). See
[`docs/DATA-MODEL.md`](../../docs/DATA-MODEL.md).

## Falsifiability contract

The prediction store SHALL retain a degree-preserving shuffled-graph negative control alongside every
real prediction (`control_tp` / `control_fp`), so the gate's predictive value is falsifiable by
construction: if real predictions do not beat the shuffled control, the eval fails. INV-22 property
tests assert the control columns are present and populated for every prediction row.

- **REQ-106** — [O] INV-10 · the verdict author subtracts what was already broken, at two granularities.
  The deterministic verdict author SHALL accept, alongside the (host,rule) pair baseline, a HOST-level
  pre-anomalous set — hosts that already held an OPEN incident (a raise with no recovery, bounded by the
  open-incident staleness) when the action executed, read from the durable ingest ledger anchored at-or-before
  the execution instant. An observed alert on a pre-anomalous host SHALL NOT be a surprise or a mismatch. Both
  sets absent SHALL reproduce the un-baselined author byte-for-byte, and every production caller of the
  un-baselined author SHALL be a declared, justified exception enumerated by an oracle over the source tree.
  *Rationale:* the 2026-07-28 false deviation. The pair baseline is a point sample of the SAME live monitoring
  surface as the post-read, so its failure mode is total and simultaneous with the moments it is most needed;
  the host arm is drawn from TG's own database and anchored so nothing the action caused can launder into it.
  Host granularity is deliberate: an open incident evolving its rule label is the same incident, not a new
  cascade — the residual (a real cascade landing on an already-broken host) is owned by the settle-window
  reconcile, which re-observes after the incident clears.

- **REQ-107** — [F] the predecessor `_host_site()` cross-site exclusion (infragraph.py:905–914) · [O] INV-10 ·
  Phase C4 (verdict scoping, restored on estate-derived data).
  The deterministic verdict author SHALL accept an ESTATE-DERIVED host→site authority
  (`verify.SiteAuthority`; `estate.Graph.SiteOf` — declared `member_of` site membership, else a registered
  site entity's name prefix — is the production implementation) and SHALL exclude an observed alert from the
  SURPRISE (deviation) evidence ONLY when the authority knows BOTH the alerting host's site AND the action
  target's site and the two DIFFER. A host whose site the authority does not know SHALL NEVER be excluded, the
  alert's self-reported ingest `Site` label SHALL NEVER be consulted, and the exclusion SHALL apply only to
  surprise candidates — an alert on a PREDICTED host is inside the causal claim regardless of its site. A nil
  authority, and an estate with no seeded site entities, SHALL reproduce the unscoped baselined author exactly
  (nothing excluded — the fail-closed floor this lane had before the vocabulary existed).
  *Rationale:* the predecessor excluded coincidental cross-site noise from a CLOSED host-identity vocabulary
  and never excluded a site-less host; TG shipped without the vocabulary and paid for it at governance_ledger
  seq 6555 — an unrelated 59-second sensor flap at the other site scored a deviation, demoted an op-class
  auto→approve and discarded ~80 hands-off clean runs. Deriving the vocabulary from the estate graph keeps it
  config-not-code while preserving the fail direction (unknown ⇒ never excluded).

- **REQ-108** — [F] the predecessor `rule_family()` family-granular scoring (infragraph.py:461–488) · [O]
  INV-10 · Phase C4.
  WHEN an observed alert lands on a PREDICTED host carrying a rule the prediction did not name exactly, the
  verdict author SHALL score it as the predicted failure mode (contributing toward `match`, recording no
  mismatch) IF the observed rule and a rule the prediction named FOR THAT HOST share a family under the single
  rule-family authority (`core/knowledge.CanonicalRule` over the embedded `rulefamily.json` — the same map the
  novelty gate and the recovery belt match on); otherwise the alert SHALL remain a `partial` trigger. The
  family judgment SHALL key on that ONE authority — introducing a second family table is forbidden (one
  vocabulary answers "is this the same condition" everywhere) — and family matching SHALL NEVER downgrade a
  surprise HOST: it compares rules per predicted host, never across a global rule pool.
  *Rationale:* the same physical fault surfaces under N source spellings ("HostDown", "Devices-up/down",
  "Device-Down-SNMP-unreachable"); exact string matching graded a correctly-predicted cascade `partial` for
  naming the condition in another monitoring system's vocabulary.

- **REQ-109** — [O] INV-10/INV-22 · Phase C4 (the adjudication split — migration 0042's meaning made
  mechanical in the scorer).
  The verify-time falsifiability scorer SHALL distinguish EXECUTED from never-executed predictions via the
  per-execution record (`action_execution`), SHALL write the confusion-matrix falsifiability score for every
  due prediction, and SHALL author a FORECAST verdict (`prediction_verdict`) ONLY for a never-executed
  prediction and ONLY against an ESTABLISHED commit-time baseline — the (host,rule) pairs and open-incident
  hosts already firing at the prediction's CommittedAt, read back from the durable ingest ledger with both
  arms cut at received_at ≤ CommittedAt. An executed prediction's adjudication SHALL remain the actuation
  interceptor's alone, and NO scorer-authored verdict SHALL feed op-class graduation or demotion. WHEN the
  baseline read fails, the scorer SHALL leave the prediction unscored for retry rather than adjudicate outside
  a baseline. The confusion-matrix score and the INV-22 shuffled-control comparison SHALL remain UN-baselined
  and UN-scoped: ambient noise strikes the real prediction and its degree-preserving control symmetrically, so
  the control ratio stays the tripwire that verdict scoping is not laundering noise.
  *Rationale:* a forecast of "what will cascade IF this action runs" diffed against the ambient estate — no
  execution, no baseline — can only produce deviation; the live prediction_verdict table read 19/19 deviation
  all-time, a statement about the adjudication's structure, not the world model. Splitting the sinks keeps
  action_verdict at exactly one writer per meaning (TG-184) while the world-model grade becomes computable.

- **REQ-110** — [F] the predecessor `infragraph.py expected_cascade()` dynamic verification window
  (`window = max(DEFAULT_WINDOW_S /* 900 */, int(2 × max_p95))`, `_percentile()` nearest-rank,
  `SAMPLE_CAP = 64`) · [O] INV-22 · TG-220 / port-fidelity finding #20.
  The verify-time falsifiability scorer SHALL adjudicate each prediction in an observation window LEARNED per
  EDGE from OBSERVED cascade latency, not in a fixed constant: `window = max(FLOOR, 2 × p95(observed
  latency))` per edge, and the prediction's window SHALL be the MAXIMUM over the edges it claims — so a
  prediction is scored only once its SLOWEST claimed cascade has had time to manifest. The learning key SHALL
  be the ordered (primary host → dependent host) pair, the same edge identity `estate.CoOccurrence` uses, and
  the latency samples SHALL come from TG's OWN durable record of observed cascades (the append-only front-door
  ledger `ingest_alert`), bounded to the most recent `SAMPLE_CAP` observations per edge. The percentile and the
  window SHALL be computed by deterministic code — NEVER by a model call.
  BOTH BOUNDS SHALL FAIL SAFE AND BE OPERATOR-VISIBLE. An edge with NO observations, an unreadable durable
  read, and an unwired latency seam SHALL ALL resolve to the FLOOR (900s), which SHALL NOT be shorter than the
  fixed window it replaces; and the learned window SHALL be clamped to a configured MAX, so a widened window
  may DEFER a prediction but SHALL NEVER strand it unscored. A prediction inside its learned window SHALL be
  left unscored and retryable, NEVER scored against a post-state its cascade has not reached.
  The INV-22 CONTROL SHALL BE THE TRIPWIRE ON THIS CHANGE: widening the window SHALL NOT raise the
  degree-preserving shuffled control's match rate, and an oracle SHALL assert that `control_ratio` does not
  rise as the window widens — a widening that lifts the random control in lockstep is laundering ambient
  noise, not recovering causal signal, and is a defect in this requirement's implementation.
  *Rationale:* TG scored every prediction in a fixed 10-minute window (`TG_FALSIFIABILITY_WINDOW`) while the
  predecessor learned it. A cascade slower than 10 minutes therefore adjudicated as a MISS in TG and a HIT in
  the predecessor — a known-direction bias in exactly the falsifiability and forecast numbers the predecessor
  head-to-head is measured on. This is MEASUREMENT INFRASTRUCTURE for the comparison itself, so it is corrected
  before the comparison is run rather than pinned as a caveat. Two DELIBERATE deviations from the original
  (§1.7 — port the logic, not the bugs): the upper clamp, which the predecessor lacks (one pathological sample
  there yields a 12-hour window and the row never scores), and computation at SCORE time rather than commit
  time, which needs no `window_seconds` column and lets the newest evidence reach a prediction still waiting.

- **REQ-111** — [F] the typed, source-bound CLAIM · [O] INV-08, INV-11 · TG-201 (axis A2).
  A parsed proposal MAY carry a typed `Diagnosis` — `{root_cause, mechanism, supporting[], contradicting[],
  ruled_out[]}` — in which every assertion is bound to an orchestrator-captured `ToolResult.ID` or is
  explicitly marked UNCITED. It SHALL be OPTIONAL: a proposal without one behaves exactly as before, so the
  field cannot regress the existing gate on the day it ships.

  Binding SHALL be performed against the ids the ORCHESTRATOR captured, never against the model's own claim
  about them: `Cited` is true only when the id matches a gathered `ToolResult`. A model naming an id nobody
  captured is the fabricated-citation failure INV-11 exists for, and a `Cited` derived from `ID != ""` would
  let the model author its own proof.

  An UNCITED assertion SHALL be retained, never dropped. Dropping it hides that the model asserted something
  it could not ground — the same invisibility the pre-existing all-or-nothing citation gate suffered from,
  which could assert "at least one cited id was gathered" and could express neither "assertion 2 of 4 is
  uncited" nor "this observation CONTRADICTS the stated cause".

- **REQ-112** — [F] the 2026-08-06 pve03 postmortem (TG-378) · [O] INV-07, INV-09.
  WHERE an op-class declares a state precondition (`requires_target_state` in the op-class registry — a
  CLOSED vocabulary: `not-running`, and since spec/029 T-029-3 also `running`, declared by `stop-guest`,
  the commit-confirmed inverse whose blind-stop guard is this precondition MIRRORED — never stop a guest
  someone else already cycled), the prediction gate SHALL establish that precondition from a
  live, staleness-bounded observation BEFORE committing the prediction or sealing the manifest, SHALL
  record the satisfying observation on the sealed `ActionManifest` (`precondition_observation` — beside
  `plan_hash`, never inside the content-hashed `action`), and SHALL REFUSE the seal — leaving NO committed
  prediction behind — when the observed state violates the precondition, when it cannot be established
  (target never observed, projection stale), or when no state reader is wired: an action whose
  precondition cannot be established fails CLOSED, because unknown is not not-running. A class declaring
  no precondition SHALL be untouched. Rationale, measured: three of the four manifests sealed during the
  pve03 cascade proposed `start` on guests running with 897h/2,103h uptimes, and the only thing that
  stopped them was an unrelated global band.

  A grounded contradiction SHALL be RECORDED, not enforced. It is DATA (INV-08): the model can be wrong
  about what contradicts what, so the diagnosis never vetoes a proposal on its own — it makes the
  contradiction visible to the gate, the judge and the operator. This is the recorded A2 root cause: on the
  same incident the predecessor reads PVE task history, sees the guest was stopped DELIBERATELY and stands
  down, while TG holds the identical observation and proposes a restart — because nothing bound a piece of
  evidence to the assertion it bears on. It was never fixable by prompt engineering; there was no field to
  put the contradiction in.

  The claim SHALL be SCORED (TG-201 part 1). "Recorded, not enforced" is not "recorded, and free": a
  structured claim nobody grades is decoration, and an agent that can emit a contradicted diagnosis at no
  cost will keep emitting one. The diagnosis is therefore persisted onto the session record (migration 0056)
  and graded as the `diagnosis_grounded` judge dimension, DETERMINISTICALLY in Go rather than by the judge
  model — `Cited` is a fact the orchestrator decided against ids the model could not author, and handing it
  to a model to re-adjudicate would re-open a checkable property to free text. The dimension SHALL NOT
  penalise honest uncertainty: a diagnosis that names no cause and cites the observations that ruled
  alternatives out scores at the top of the scale, because a rubric that graded "root cause unknown" as
  failure would pay the agent to fabricate the confidence this whole type exists to expose. It SHALL be
  N/A — omitted, never floored — for a session that predates the field and for a stand-down that claimed no
  cause, the same discipline `falsifiable_prediction` follows.


## Derivation note (2026-08-11, TG-146 A3)

`manifest.Action.SafetyParts()` is the SHARED safety-predicate derivation — Target, Op, OpClass plus every
param KEY and VALUE (sorted; the predicates OR over parts) — consumed by BOTH depths of the
stateful/destructive floor: the classify-time caller (temporal/runner, the A3 first half) and the
actuation-adapter floor (spec/013, the A3 second half). One source, two depths, no drift. It lives on the
manifest Action because that is the sealed, content-hashed identity both depths already hold; the method
reads the hashed fields and adds nothing to the hash.
