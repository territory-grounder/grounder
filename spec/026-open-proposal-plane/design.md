<!-- spec/026 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/026 — Design: the open proposal plane

Design provenance: TG-227 design workflow `wf_3a385a3f-a58` (six ground-map readers, three
independent P3 architectures, one synthesis), 2026-07-31. Every seam below was verified against the
tree at design time; re-verify line numbers at implementation.

## The controlling insight

The epic's safety machinery already exists. An unregistered op_class already parses
(`core/proposal/parse.go:61` — no registry lookup at parse), already seals to nil argv
(`temporal/runner/activities.go:1012-1022`), is refused by every effect leaf on empty argv
(`adapters/actuation/actuation.go:58-69`; `modules/actuation/ssh/ssh.go:135-145`), is floored
never-auto (`core/policy/graduation.go:470-489`), and already renders as "none declared" in the
agent preamble (`agent/loop.go:36-39`). What is missing is not safety — it is that the steering
surface calls itself a "stand-down generator" (measured 100% stand-down pre-alignment on
action-warranted incidents with no registered class), that such proposals today flow into the REAL
approval lane where a human can be polled to approve an unexecutable action (the ground-map hazard
this spec removes), and that nothing records or renders them.

## What changes (and what deliberately does not)

| Surface | Change |
|---|---|
| `agent/skills/skills.go:174-199` | conservative-remediation v1.3.0: name the addressing op-class (free-form allowed) when observations confirm the fault; eval-gated |
| `agent/loop.go:36-39` | preamble empty-catalog text: free-form proposing is the declared duty, execution stays impossible |
| `core/proposal/parse.go`, `proposal.go` | ONE additive optional field `undo_sketch`; actor-evidence REUSES the existing `session_triage.actor_evidence` column from migration 0035 (`[]attribution.Evidence` — the evidence-class member is an additive element-shape extension, REQ-2610); spec/002 prose + lockstep restamp in the same MR |
| `temporal/runner/workflow.go` | THE new code: shadow divert branch before notify/projection/vote, `workflow.GetVersion`-guarded (pattern: "pending-projection" at `workflow.go:391`) |
| `core/db/migrations/0046_*` | `session_triage.undo_sketch` + outcome `proposed:shadow` — additive, defaulted (NO actor-evidence column: it exists since migration 0035 and is reused, REQ-2604/REQ-2610) |
| `core/audit` (call site only) | one `propose:open` GovDecision per shadow proposal, `withheld:true`, on the ONE chain |
| `core/proposal/evidence.go` (new) | actor-evidence class table + band-floor resolution (REQ-2611), safe-direction-only composition mirroring the NoticeFloor pattern (spec/028) |
| `core/httpapi/proposals.go` + console `proposals` module | read-only surface; nine-touchpoint recipe; no actuation controls |

Deliberate non-changes: no new stop reason (`loop.go:101-120` is a closed vocabulary — the shadow
proposal is an OUTCOME via the orchestrator-computed machinery, migration 0044 pattern); no second
grammar or fallback parser (INV-06; H-02 is the predecessor's crown-jewel bypass); nothing added to
`manifest.Action` (INV-07 — field additions change every action identity); the judge prompt stays
byte-pinned (open question 7 defers `undo_sketch` rendering); the mode chokepoint and every effect
leaf are cited, not touched.

## Actor-evidence banding (owner-ratified policy)

The propose duty is absolute and band-independent (REQ-2609): actor-evidence never suppresses the
proposal — it RAISES THE BAR for executing it. `core/proposal/evidence.go` declares the closed
evidence-class table (v1: `authored-action`, `maintenance-window`, `declared-chaos`) and the
operator-policy mapping evidence class → band floor (v1: authored-action ⇒ POLL_PAUSE floor).
Composition is safe-direction-only over the constitutional band vocabulary (AUTO / AUTO_NOTICE /
POLL_PAUSE — no other names), exactly like spec/028's NoticeFloor: computed AUTO or AUTO_NOTICE +
authored-action floor ⇒ POLL_PAUSE; a computed POLL_PAUSE passes through unchanged; a floor never
lowers a computed band. The composition seam (the `core/risk` GatedInput floor field + classifier
safe-direction clamp) is delivered by spec/028 T-028-4 [TRAILER + lockstep restamp]; this spec
supplies only the evidence→floor table it consumes (REQ-2611). Rationale (fault 1406): the agent
that reads a `root@pam vzstop` trail and asks first is behaving correctly — the policy makes that
correctness explicit, separable, and scoreable (`appropriate_band`), while the named op-class stays
comparable across models and architectures. Benchmark interaction: the per-fault-type `accept` list
in expectations.json names the correct op-class(es); band scored separately (ADR-0016 §Benchmark).

## Oracles (acceptance/open-proposal-plane.feature) and their RED mutation controls

- O-2601 empty-catalog proposal: real-path session, empty catalog, action-warranted fault →
  outcome `proposed:shadow`, triage row with free-form op_class + screened undo_sketch, one
  `propose:open` ledger row, ZERO pending_decisions rows, ZERO notify calls.
  RED: delete the divert branch → "no pending_decisions row" goes red.
- O-2602 registered class does NOT divert → normal classify→gate→poll lane. RED: invert the
  Lookup condition → red.
- O-2603 defense-in-depth: force-route a sealed free-form action → refused at empty argv, refusal
  ledgered. RED (executed 2026-07-31; the originally-named sealedArgv stub does NOT redden this oracle —
  the execute path re-derives the effect, so the effective seams are): (a) remove the empty-argv branch
  from the LocalReadOnly leaf → the refusal loses the argv signature and the oracle fails on it;
  (b) the registered-class signature control (a registered restart-service through the same chain fails
  the oracle's refusal-signature discrimination).
- O-2604 citation gate: uncited free-form proposal → mechanical re-prompt, never a shadow record.
  RED: disable the citation gate → red.
- O-2605 console e2e: three liveState states + mid-boot undefined window, no mutating control
  present, deeplink auto-coverage. RED per `deeplink-every-view.mjs:96-125` convention.
- O-2606 screening: jailbreak/secret-shaped rationale → neutralized-and-flagged in persist/render.
  RED: bypass Scrub on the new field → red.
- O-2607 actor-evidence: fixture with authored-stop actor evidence → the proposal NAMES `start-guest`
  AND carries the POLL_PAUSE floor. RED: drop the evidence→floor mapping → the floor assertion goes red;
  second RED: make evidence suppress the proposal → the naming assertion goes red.
- Eval gate: skill v1.3.0 passes the on-box change gate. What the committed record
  (eval/history/2026-07-31-change-4462999432c7) actually evidences: non-regression with improvement
  (overall +0.25, appropriate_band +0.38, evidence_grounded +0.75), proposal_recall 1.00 on the
  REGISTERED-class labeled corpus (floor 0.50), and 5 negative controls with zero manufactured actions.
  The unregistered-slug stand-down→propose conversion is NOT measurable on that corpus (every labeled
  action-warranted incident has a registered fix; expectations.json carries no unregistered-slug entry) —
  that behavior is evidenced by the REQ-2601/REQ-2603 oracles instead. DECLARED FOLLOW-UP: add an
  unregistered-slug eval incident to the labeled corpus so the conversion becomes eval-measurable
  (an expectations.json change — operator-declared rubric, its own governed step, not this spec's edit).

## Dependencies and interfaces

- Provides: `RecordProposalOccurrence` Deps seam (`temporal/runner/activities.go:56-349` pattern;
  nil = documented inert), wired at `cmd/worker/main.go` (`runner.NewActivities`, :3351) — consumed
  by spec/028's clustering; stubbed nil-inert here.
- Reads: `opschema.Lookup` for the divert predicate (exact slug, embedded+overlay composed registry
  once spec/028 lands; embedded-only until then — behavior identical).
- Console: `GET /v1/proposals` (AuthReadOnly, nil-reader 503 — clone of `grounding.go:55-79`).

## Open questions

Owner-decision-pending items for this plane live in ADR-0016 §Open-questions: OQ-1 (SeedDefaults vs
day-zero empty catalog — recommendation: opt-in profile flag through the audited config-write lane)
and OQ-7 (judge surface enrichment — recommendation: not in v1).
