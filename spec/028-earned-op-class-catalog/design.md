<!-- spec/028 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/028 — Design: the earned op-class catalog

Design provenance: TG-227 design workflow `wf_3a385a3f-a58` (three independent architectures —
risk-first, operator-first, reuse-first — synthesized; the arbitration record lives in the epic's
design output). Constitutional decisions: [ADR-0016](../../docs/adr/0016-earned-opclass-overlay.md).
Re-verify cited line numbers at implementation.

## The ladder, plainly

| Rung | Caption | Mechanism |
|---|---|---|
| R0 | "Proposes only — cannot execute, ever" | ABSENCE from the composed registry: nil sealedArgv (`activities.go:1012-1022`) + empty-argv leaf refusal + never-auto floor (`graduation.go:470-489`). Zero new code. |
| R1 | "Asks first — every run needs your click" | Ratified into the overlay at LevelApprove, last_outcome `ratified` (never promotes by itself — the OutcomeSeeded analog); UngraduatedClass opens the not-graduated poll (`classifier.go:95-104`) |
| R2 | "Acts and pages — you can veto" | NEW LevelAutoNotice: promote_threshold consecutive terminus-confirmed verified-clean runs, exactly-once credit via `graduation_credit`, AutoEligible(tier) before earned level, durable-or-refused |
| R3 | "Acts silently — verified after every run" | +10 verified-clean at auto_notice, zero vetoes, zero recurrence-≤24h — AND the class exists in the EMBEDDED lockstep-hashed opschema.json. Overlay-only classes CAP at R2; "the last rung requires a code release" (embed-export MR) |

Auto-demote: verified deviation at any rung → full drop to approve, streaks reset, via the
UNCHANGED immediate hook (`interceptor.go:836-863` — may demote, never promote); ledger reason
carries the typed SurpriseAlerts breakdown (the three false-deviation post-mortems). Operator
demote verb: any rung → approve, one click, rationale mandatory. Revoke: new revoked overlay row →
class exits the composed registry within one refresh ⇒ R0 by construction (the infragraph Phase-C
instant single-artifact deactivation). Only OutcomeFromVerdict feeds the machine; forecast-lane
verdicts render in dossiers and never touch graduation (INV-10; C4 one-writer-per-meaning).

## Candidate lifecycle

`observing → candidate` (mechanical, cron): ≥3 DISTINCT external_refs AND (≥2 hosts OR ≥7d span)
AND mean confidence ≥0.6 (between StopThreshold 0.5 and EscalateThreshold 0.7,
`agent/confidence.go:22-23`), rolling 30d. Confidence is a BAR, never a WEIGHT — nothing a model
can inflate buys candidacy faster. `candidate → ratify_ready` (mechanical completeness): ≥5
distinct refs, family/tier mechanically assigned from the closed sets, auto_barred stamped
server-side from IsNeverAuto/IsDestructiveOp (a model cannot under-declare), blast radius computed
for ≥80% of occurrence targets (estate path-product walk, `estate.go:272-357`), no active dismiss
TTL — the offline-before-online admission pattern (`core/skillstore/admission.go:9-66`).
`ratify_ready → ratified | dismissed(30d TTL)`; `observing/candidate → expired` after 60d silence
(ledgered; the key is re-observable fresh). auto_barred candidates stay RATIFIABLE at
any AutoEligible=false tier (TierIrreversible or TierVendorCritical) ⇒ ceiling R1 forever, visibly badged — a recurring
destructive desire is visible but structurally barred from climbing.

All lifecycle transitions flow through one audited Transition clone (`skillstore/transition.go:56-104`)
with ledger-before-row. The clustering cron is a singleton Temporal workflow in the finalizer shape;
per-item errors never abort the pass. DEAD-MAN: the pass refuses loudly when the newest occurrence
is >48h old while session volume is nonzero, computed from tables the cron does not write — the
dead-judge lesson (it died twice while process signals stayed green).

Cluster identity: `candidate_key = SHA-256("v1|"+norm(op_class)+"|"+norm(op)+"|"+sorted-param-NAMES)`
with opschema's own INV-08 normalization — the cluster key can never diverge from the lookup key it
materializes under. Host and rule_family are deliberately EVIDENCE, not identity: cross-host and
cross-rule recurrence is what generality means; keying on rule_family would fragment identical
remedies across alert families.

## Ratify: the T3-refusal core

The form is STRUCTURALLY EMPTY — no prefill code path exists; model text renders only as screened
read-only exhibits in a visually separate pane ("model suggested — never executable"). Server-side
admission = `mustBuildRegistry`'s panic checks refactored into error-returning
`opschema.ValidateSpec` (a live worker cannot panic): closed family/tier; literal argv[0]
(`opschema.go:298-312`); whole-element slots (`:313-338`); slots reference declared REQUIRED
params; validator tolerance == renderer tolerance (`:14-17,532-539`); PLUS overlay-never-shadows-
embedded; tier-vs-destructiveness contradiction refusal; and the LAUNDERING TRIPWIRE — refuse any
template element that byte-matches any occurrence's model text. The executed vector is always
operator-typed.

## Composed registry (ADR-0016)

`opschema.Lookup/Specs/Catalog` (`:486-604`) consult the embedded base THEN an injected
OverlayProvider snapshot (atomic swap; refresh ≤60s and on ratify signal). Each overlay row is
re-verified at load — entry_hash vs the ledgered hash; mismatch ⇒ row DROPPED loudly (fail closed
to FEWER capabilities) + page. The embedded `opschema.json` stays go:embed + lockstep-hashed
(`spec/.lockstep.lock:20-26`); the overlay is a SEPARATE tamper domain anchored in the ONE ledger.
Ratified classes flow into the agent preamble automatically through the single Catalog render
(`agent/loop.go:29-57`) — no second prompt surface. guardallow reads the composed registry.

## Band bridge

`risk.GatedInput` gains a band floor, applied SAFE-DIRECTION-ONLY (computed AUTO + AUTO_NOTICE floor
⇒ AUTO_NOTICE; AUTO_NOTICE/POLL_PAUSE pass through — the gateway rule that extra signals only lower
autonomy, never raise it). BandAutoNotice plumbing already exists end-to-end (`safety.go:20-40`;
notices `workflow.go:362-379`; console). No new band names (constitutional terminology). A bug in the
bridge can only over-notify, never over-actuate.

**AS BUILT — one general floor, not a `NoticeFloor` bool.** spec/026 REQ-2611 explicitly deferred its
composition seam to this task, and it needs the same operation at a different height (authored-action
evidence ⇒ POLL_PAUSE floor). Two bespoke fields would put band adjustment in two places, and a second
place bands can be adjusted is a second place they can be adjusted *downward*. The seam is therefore
`BandFloor` / `BandFloorApplies` / `BandFloorReason`, clamped once in `Classify`. `BandFloorApplies` is
load-bearing rather than ceremonial: `safety.Band`'s zero value is `BandPollPause`, the strictest band, so
a floor field without an applies-flag would poll the entire estate. Full prose and the four proven
properties: spec/001 design.md § "The declared band floor".

**AS BUILT — the level-aware resolver.** The boolean `GraduatedForAuto` closure became
`LadderRungFor func(opClass string) runner.LadderRung`, with the exhaustive truth table in ONE named
function (`cmd/worker/main.go` `ladderRungFor`): approve ⇒ `UngraduatedClass`; auto_notice ⇒ AUTO_NOTICE
floor; auto ⇒ neither. One resolver rather than two predicates, because two independently wired predicates
can DISAGREE and the disagreement that matters is silent — "graduated" true with "needs a notice" false
makes an auto_notice class act with nobody paged, which is the rung's only guarantee. Deriving both answers
from a single rung value makes that state unrepresentable. The runner keeps its deliberate non-import of
`core/policy`: the rung crosses the seam as `runner.LadderRung`, whose zero value is `RungApprove`.

It is a NAMED function rather than an inline literal so the aliveness oracle
(`cmd/worker/ladder_rung_wiring_test.go`) drives the same code the shipped binary wires — a truth table
re-typed inside a test proves the test's copy is right and says nothing about the binary. That oracle takes
each rung EARNED through a real `policy.Ladder`, pushes it through the real runner predicates and the real
`risk.Classify`, and asserts the band. Its three RED controls break the three links separately (truth
table, floor predicate, poll predicate); controls 2 and 3 fail in OPPOSITE directions from the same
assertion, which is what makes the middle rung worth testing — it is the only band that can be missed both
by acting too freely and by acting too little.

## Dossier (read-model; five questions in order)

(1) what keeps happening — occurrence sparkline, N incidents / M hosts / span, exemplar
external_refs deep-linking to session detail; (2) what TG wants to do — screened quoted model text,
labeled untrusted; (3) what it could break — estate blast-radius walk with per-edge
provenance+confidence + cascade control ratio; (4) how good its predictions are —
prediction-verdict confusion matrix, DISPLAY ONLY; (5) what the operator must type — the empty
form beside the tier ceiling badge, nearest registered class's RollbackTemplate for reference,
recurrence-≤24h rate, poll-answer rate, SurpriseAlerts on any demotion.

## Console

EXTEND `/v1/policy/graduation` + polGradSection with the widened vocabulary and the plain-language
rung captions (`core/httpapi/policy.go:60-73`; `policy/js.txt:160-197`; MemPolicyReader for
no-Postgres CI). A second ladder view would be a parallel-implementation violation. ONE new
`candidates` module (nine-touchpoint recipe; live-only; data-keyed buttons + server caller_can_act).

## Oracles and RED controls (summary — one scenario per predicate)

O-2801 clustering distinct-ref dedup (RED: count events not refs) · O-2802 threshold-minus-one
(RED: lower the bar) · O-2803 no-prefill + laundering tripwire (RED: inject prefill / drop
tripwire) · O-2804 admission per-check refusals (RED: relax one check per scenario — the
mutation-per-predicate convention) · O-2805 overlay tamper drop (RED: skip hash verification) ·
O-2806 exactly-once promotion credit (RED: bypass the credit key) · O-2807 band-bridge truth table
(RED: break the closure mapping) · O-2808 deviation demote + forecast-verdict isolation (RED:
route forecast verdicts into OutcomeFromVerdict) · O-2809 the overlay AUTO ceiling (RED: drop the
embedded-membership guard) · O-2810 revoke-to-absence (RED: serve a stale snapshot) · O-2811
auto_barred tier floor (RED: lift the ordering) · O-2812 cron dead-man (RED: compute liveness from
the cron's own writes).

## Deliberate non-changes / v2 deferrals

No `propose_only` stored level (rung 0 = absence — the Level zero-value law holds,
`graduation.go:47-54`). No `action_execution.op_class` column in v1 (the write site is lockstep;
external_ref joins suffice for dossiers). Recurrence-≤24h as an AUTO-DEMOTE trigger is v2 (v1 uses
it as the R2→R3 promotion gate and renders it). Drop-one-rung demotion is v2 (OQ-3). Permanent
reject with lineage is v2 (OQ-6). Family-keyed ladders are v2 with their own evidence argument
(OQ-5). AUTO for overlay-only classes without a code release is v2 at best, behind a double key
(OQ-4).
