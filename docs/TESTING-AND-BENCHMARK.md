# Testing & Benchmark Strategy — Territory Grounder

> **The definition of done.** A change to Territory Grounder (TG) ships only when it survives
> three independent bars: an honest quality signal it may never tune to, a whole-trajectory
> outcome benchmark that grades diagnosis *and* action *and* an independently-confirmed
> postcondition, and an adversarial boundary suite that drives every trust boundary with
> hostile input. Synthetic self-scoring is a smoke test — never the authority that lets a
> release out the door.

**Provenance tags** — `[F]` foundation (inherited design) · `[R]` product reframe (multi-tenant / de-solo) · `[O]` audit overlay (security + quality hardening). Source ids (INV-NN, spec/00x, paradigm-rule N) are cited inline.

**Sibling documents.** This file assumes the control-plane described in `ARCHITECTURE.md` (Go control-plane, Temporal workflows/activities, PostgreSQL+pgvector, the bundled LiteLLM model-gateway, the module system), the safety model in `CONSTITUTION.md` (the inviolable mechanical safety core, the 3 autonomy bands, the fail-closed prediction gate, the ActionManifest binding), and the data contracts in `DATA-MODEL.md` (tenant_id + RLS on every table, the append-only governance ledger vs. the purgeable operational body). Terminology is identical across all of them: **ActionManifest**; the execution classes **DETERMINISTIC / FAST_AGENT / STANDARD_AGENT / DEEP_INVESTIGATION / HUMAN_LED**; the three autonomy bands **AUTO / AUTO_NOTICE / POLL_PAUSE**.

---

## 0. Assurance philosophy — why testing is a safety control, not hygiene

TG inherits a governed-autonomy architecture whose safety story was, in the predecessor, *real in intent but false in binding*: the test suite and self-scorecard certified green by asserting on **source strings and synthetic streams** while the actual bypass lived in the exported runtime, uncovered [O, M-02/M-14]. The lesson TG is founded on: **a control is only as strong as its binding, and a test is only as strong as the code path it actually drives** [O, S8-preserve-meta].

Three consequences shape everything below:

1. **The system may never grade its own homework as the release gate.** The acting agent has no write path to its own outcome verdict [F]; symmetrically, the system's *synthetic* self-scorecard can never be the deployment authority [O, INV-22]. Ground truth comes from outside the generator — a sealed holdout [F], a mechanical verifier [F, spec/002], an independent judge that is itself audited [F], and adversarial coverage of every trust boundary [O, INV-22].
2. **Outcome, not answer, is the unit of success.** A session that produces a fluent diagnosis but takes the wrong action, or the right action whose postcondition never actually cleared, is a *failure* — measured as such by the whole-trajectory benchmark (§2) [O].
3. **Multi-tenancy is a test axis, not a footnote.** Isolation (no adapter, credential, rollback command, retrieval query, eval corpus, or promotion signal crosses a tenant boundary [R, paradigm-rule 1]) is a safety-critical behavior and therefore carries adversarial tests like any other trust boundary [O, INV-12, INV-22].

---

## 1. The eval flywheel & the sealed holdout — the honesty guarantee [F]

### 1.1 The closed Analyze → Measure → Improve loop

TG's quality process is a **closed flywheel**: analyze failures → measure against fixed corpora → improve prompts/RAG/policy → re-measure [F, eval-learning]. Behavioral adaptation is **prompt-policy iteration + RAG, never model fine-tuning** — an explicit ADR forbids weight updates; the handbook order is prompt-engineering → RAG → (fine-tune only if eval *proves* it necessary) [F]. Promotion of any prompt-patch variant happens **only through a one-sided Welch-t significance gate at minimum sample** [F], and the trial substrate is partitioned by `(tenant, surface, dimension)` with `tenant_id` + RLS so **one tenant's traffic can never promote a variant for another** [R, prompt-patch reframe].

### 1.2 The three corpora

| Corpus | Cadence | Role | May the system tune to it? |
|---|---|---|---|
| **Regression** | every merge request | Guardrail: no known-good behavior regresses | Yes — it is the working set |
| **Discovery** | weekly | Surfaces newly-degrading behavior before it reaches the gate | Indirectly (drives fixes) |
| **Sealed holdout** | monthly | The only honest quality signal | **Never** |

Discovery cases **promote into regression only after 4+ stable weeks, and never enter the holdout** [F]. Every corpus row carries `tenant_id`; the holdout is drawn from real trajectories that have been **de-identified and sealed**, consistent with the purgeable-operational-body vs. immutable-audit-spine split [R, paradigm-rule 5] — sealing a holdout row is an archival act, not an indefinite hoard.

### 1.3 The overfitting invariant (the definitional honesty guarantee)

> **The only honest quality signal is a sealed holdout the system may never tune to.** Improving the regression score while the holdout stagnates — **or a regression-vs-holdout gap exceeding 20 points** — is **DEFINED as overfitting failure** and blocks release, regardless of how good the regression numbers look [F].

This is not a soft target. The gap is computed each monthly holdout run (per tenant *and* pooled), recorded with a non-null `generated_at` + source hash + coverage scope [O, INV-15/INV-22], and a `>20pt` gap is a hard CI/CD deployment-gate failure. It exists precisely to catch the failure mode the predecessor's synthetic scorecard exhibited — a headline 1.0 that tested none of the real boundaries [O, M-14].

### 1.4 The judge is audited and can fail

The flywheel's automated measurement is an **LLM-as-a-Judge (5-dimension session scoring: investigation / evidence / actionability / safety / completeness)** [F], resolved through the model-agnostic router like every other model call [R, paradigm-rule 3]. The judge itself is a monitored, fallible component:

- **Calibration floors:** judge TPR/TNR must stay **≥ 0.70**; a deliberately *non-local, higher-capability* frontier anchor cross-checks local-judge drift and is **skip-if-down, never local-fallback** (a weaker model must never silently backstop the auditor) [F]. The anchor is a per-tenant model-routing choice, not a hardcoded vendor [R, judge-anchor reframe].
- **Judge-death detection [F, spec/004]:** the fraction of recently-ended sessions carrying a real local judgment is computed using **only tables the judge does not write** (REQ-305); a **judge-death warning fires if < 50% of > 3 eligible sessions are judged** (REQ-306). "No judgments" is treated as a problem, not as "everything passed" [F, no-data-is-a-problem].
- The judge is a *measurement instrument for the flywheel*, and its output is advisory to promotion — it never becomes control flow or an action authority [O, INV-08].

---

## 2. The whole-trajectory benchmark — outcome, not answer [O]

The predecessor measured fluent text and per-dimension rubric scores; it did not have a single metric for *did the incident actually get resolved correctly and safely, end to end*. TG's headline benchmark is **whole-trajectory** [O]: it grades the complete `event → triage → context → plan/risk-band → commit-prediction → propose → approve → execute → verify → learn` loop [F] as one unit, against corpora replayed through the **actual** control-plane code path, never against source strings [O, INV-22].

### 2.1 Verified Incident Success Rate (VISR) — the primary metric

A replayed incident counts as a **Verified Success** only if **all three** hold:

1. **Correct diagnosis** — the root cause the trajectory settled on matches the corpus ground-truth root cause (judged, with the mechanical/label check taking precedence over the LLM judge where a label exists).
2. **Appropriate action** — the action taken (or the decision to hold) is the *right band and right class* for the incident: reversible+predicted work in scope was acted on; anything irreversible/novel/deviation correctly landed **POLL_PAUSE** [F, spec/001]. Acting where the system should have held, *or* holding where the policy says a backlog of reversible work is a policy failure [F], both fail this leg.
3. **Independently-confirmed postcondition** — the mechanical verifier (never the acting model) diffed observed state against the committed `plan_hash`-keyed prediction and wrote **match** (not partial, never deviation), i.e. the fix is confirmed to have actually cleared the condition [F, spec/002; O, INV-10/INV-11].

> **VISR = (Verified Successes) / (replayed incidents).** A "confidently wrong" or "right answer, action never confirmed" trajectory is scored **0**, by construction. This is the metric the 20-point overfitting invariant (§1.3) is computed over on the sealed holdout.

VISR is measured per tenant and pooled; per-tenant scoring guards against a benchmark that looks healthy in aggregate while one tenant's estate is quietly regressing [R, paradigm-rule 1].

### 2.2 The replay corpus — 200–300 incidents across six strata

The benchmark replays a curated corpus of **200–300 real incidents** [O] (drawn from de-identified operational history, sealed for the holdout slice, tenant-tagged), stratified so a high aggregate score cannot hide a weak stratum:

| Stratum | What it probes | Why it must be its own bucket |
|---|---|---|
| **Recurrent** | Incidents seen before with a learned prior | Baseline competence; RAG/memory hit path |
| **Generalization** | Same class, unseen host/estate/tenant | Did it learn the *pattern* or memorize the *instance*? |
| **Novel** | No learned prior (`ood:novel-incident`) | Must force **POLL_PAUSE**, never guess-and-auto [F, spec/001 REQ-007] |
| **Correlated** | Multi-host bursts / cascades | Blast-radius fold + suppression + one-incident accounting under a storm [F, spec/003 REQ-205] |
| **Ambiguous** | Under-specified / conflicting signals | Low-confidence must terminate/escalate, not confabulate [F, confidence-as-scalar] |
| **Negative controls** | Known-benign, or "correct action = do nothing" | Catches an over-eager agent that manufactures action; a false auto-resolve here is a hard fail |

Negative controls are load-bearing: they make the benchmark **falsifiable** — a system that "resolves" everything scores *badly*, mirroring the shuffled-graph negative control that keeps the prediction gate honest [F, spec/002]. Stratum-level VISR floors are enforced independently so no single stratum can be traded off against another.

### 2.3 The 5-mode comparative experiment (ablation)

To attribute outcome to the parts of the system that produce it — and to prove each is earning its place — every corpus run executes in **five modes** and reports VISR + the composite (§2.4) for each:

| Mode | Configuration | What it isolates |
|---|---|---|
| **M1 — Full** | All signals, full autonomy banding, prediction gate on | Production reference |
| **M2 — RAG-disabled control** | 5-signal RRF retrieval OFF; agent runs on the prompt + live tools only | The value of retrieval/memory; guards against "RAG is decorative" |
| **M3 — Deterministic-only** | No agent loop; Tier-1 deterministic triage + suppression only [F] | The floor the agent must beat; how much is *code* already solving |
| **M4 — Single-agent** | Manager pattern with sub-agents-as-tools disabled | Does multi-agent topology actually help, or just add cost? [F, least-autonomous-topology] |
| **M5 — Prediction-gate-in-advisory** | `INFRAGRAPH_DISABLED`-equivalent analysis-only: record-without-gating [F, spec/002 REQ-105] | The gate's *cost* (latency/holds) vs. its *safety* contribution |

The **RAG-disabled control (M2)** is mandatory [O]: it is the experiment that keeps the retrieval stack falsifiable — if Full does not beat RAG-disabled on the generalization and correlated strata, the retrieval investment is not paying for itself and the result must be reported honestly [F, honesty-over-marketing]. Each mode resolves models through the same per-tenant router config so a mode comparison never silently swaps providers [R, paradigm-rule 3].

### 2.4 The Agentic Utility composite

VISR is the headline, but a one-number **Agentic Utility** composite lets modes and releases be ranked on the full cost/benefit surface. Weights are fixed (changing them is a governed, audited change so a release cannot re-weight its way to a better score):

| Component | Weight | Definition |
|---|---:|---|
| **Outcome** | **0.40** | VISR (§2.1) — correct diagnosis ∧ appropriate action ∧ confirmed postcondition |
| **Autonomy** | **0.15** | Fraction of *eligible* (reversible + predicted) work actually auto-handled without a needless POLL_PAUSE — a growing reversible backlog scores *down* [F] |
| **Reliability** | **0.15** | Determinism/repeatability of the gated spine, graceful degradation under retrieval/dependency failure, no fail-open on the mutation lane [F, two-lane] |
| **Evidence** | **0.10** | Share of AUTO/high-confidence claims backed by orchestrator-captured, provenance-bound ToolResult IDs — not free-text or a bare code fence [O, INV-11] |
| **Latency** | **0.10** | Time-to-verified-resolution across the trajectory |
| **Cost** | **0.10** | Real per-token spend from the `llm_usage` ledger — real tokens only, no estimation [F]; per-tenant chargeback units [R, paradigm-rule 6] |

Outcome dominates by design (0.40); autonomy and reliability are co-equal at 0.15 because *acting safely* and *not stranding safe work* are both first-class; evidence/latency/cost round out the surface at 0.10 each. The composite is computed per mode and per stratum, with the same non-null `generated_at` + source-hash + coverage-scope provenance stamp every generated artifact carries [O, INV-15].

### 2.5 Capture before score — the eval flywheel's phasing [O; F]

The whole-trajectory benchmark (§2.1–§2.4) is a **Phase-4** gate, but the eval flywheel is a
**data-capture problem before it is a scoring problem**, so its pieces phase in far earlier. Deferring
the *scoring* is correct; deferring the *capture* would arrive at Phase 4 with no corpus to score — a
cold start. Two distinct things get conflated under the word "eval," and only the second is deferred:

- **Per-feature acceptance oracles are not deferred.** From Phase 0, every `spec/NNN` ships godog
  `.feature` oracles that CI runs — the execution-based definition-of-done (ADR-0009,
  `docs/SDD-WORKFLOW.md`). This is the highest-leverage measurement and is present from day one.
- **The whole-trajectory outcome benchmark is deferred, and here is exactly why.** VISR requires
  *correct diagnosis ∧ appropriate action ∧ independently-confirmed postcondition* (§2.1). Through
  Phase 0/1 the platform is read-only (`mutation_enabled=false`; execute/verify are no-op stubs), so
  **two of the three VISR legs are structurally N/A** — the system has taken no action and cleared no
  postcondition. Standing up VISR then would score a system on a metric where 2/3 is always blank, and
  a green number there is precisely the predecessor's synthetic-1.0 theatre this document is founded
  against [O, M-14]. The 200–300-incident corpus (§2.2) is drawn from real trajectories, which do not
  exist until the loop runs; the sealed-holdout honesty gate (§1.3) needs a tuning flywheel to overfit
  *from*; the boundary-coverage gate (§3) hardens adapters/manifest/ledger that only fully exist by
  then.

**So the phasing is:**

| Eval piece | Phase | Why then |
|---|---|---|
| Per-feature acceptance oracles (godog, per `spec/NNN`) | **P0-onward** | the execution-based done-check; the agent-drivability engine |
| **Trajectory-capture schema** (every session persisted replayable, labelable, tenant-tagged) | **P1** | co-designed with `IncidentEnvelope` (P1-1) + `ActionManifest` (P1-4/P1-7); the corpus must *accumulate* from the first real run — cheap now, expensive to retrofit |
| **Diagnosis-only VISR** (leg 1 of §2.1) | **P1→P2** | measurable as soon as the read-only investigation loop exists; action + postcondition legs stay N/A |
| **Full 3-leg VISR + Agentic Utility** (§2.1–§2.4) | **P2** | once mutation is earned, action + confirmed-postcondition become real |
| **Sealed holdout + 20-pt overfitting gate** (§1.3) + **adversarial boundary-coverage gate** (§3) | **P4** | needs a tuning flywheel and a working, mutating system to harden |

The capture schema (P1) is the load-bearing early investment: it is what makes the P4 benchmark a
*flip-on*, not an archaeology project. Nothing about deferring the *score* excuses deferring the
*capture*.

---

## 3. The adversarial boundary & fuzz suite — the gate that actually certifies [O, INV-22]

The whole-trajectory benchmark proves the system does the *right* thing on realistic incidents. The adversarial suite proves it cannot be made to do the *wrong* thing by a hostile input at any trust boundary. **This is what replaces test theatre.** Per INV-22: no-op steps, source-string/schema-presence-only assertions, and partial-coverage hash manifests are **prohibited** for safety-critical behavior, and **governed code cannot be excluded from the runnable suite**.

### 3.1 Boundary-coverage gate — one adversarial test per trust boundary

Release is gated by a **boundary-coverage map** that requires **at least one adversarial test per declared trust boundary** [O, INV-22]; CI fails if any declared boundary lacks one. The declared boundaries and their hostile-input classes:

- **Ingest auth** [O, INV-01] — unauthenticated / unsigned / replayed (stale timestamp, reused nonce) requests must be rejected *before body-parse*; a route registered with `auth=none` must **fail to register at boot**. Privileged control ops (replay, chaos, self-heal, session control) are driven only as **internal Temporal signals**, never HTTP — tests assert there is no HTTP path to them [O, INV-01, threat: unauthenticated privileged op].
- **Injection boundaries** [O, INV-02/INV-03] — the shared fuzz corpus (below) is interpolated into every field that reaches an actuation adapter or a query; the assertion is *observed argv/bound-parameter shape*, proving no string ever became OS or SQL syntax. A CI lint/grep gate additionally bans `sh -c`, `fmt.Sprintf`-built commands, and string-built SQL.
- **Envelope validation** [O, INV-04] — each identifier field (hostname / IP / rule / issue-id / enum) is fuzzed against its explicit grammar; a missing required field must be a **loud validation error, never a silently-empty interpolation** (the predecessor's empty-RAG-query and inert-sanitizer bugs) [O, M-10/H-05].
- **Webhook-as-claim** [O, INV-05] — a forged payload with mutated fields must be discarded and the canonical entity **re-read by ID** with the platform's own credential before any dispatch.
- **Proposal parsing — enumerate every parser path** [O, INV-06] — the prediction-gate bypass property test **enumerates every path through the proposal parser** and asserts there is exactly one grammar shared by parser and gate: any output recognized by the parser must be constructible only as a typed `GatedProposal`, making "approval poll without a committed prediction" **uncompilable**. This directly kills the predecessor's crown-jewel bypass (a looser fallback grammar that ran *after* the gate) [O, H-02].
- **ActionManifest binding** [O, INV-07] — mutate the Action after approval and assert the new `action_id` **invalidates prior authorization and re-enters the gate**; assert execution refuses any tool call whose `action_id` is not the approved one (approval-of-X-replayed-to-execute-Y) [O, H-03].
- **Banding fail-closed** [O, INV-09] — feed the RiskClassifier ambiguous/unparseable/panic-inducing input and assert the Band zero-value is the **most-restrictive band (POLL_PAUSE)**; assert irreversible/stateful classes can never reach the auto ceiling regardless of confidence or any flag — the **inviolable mechanical safety core** holds under fuzz [F/R, paradigm-rule 8].
- **Verdict integrity** [O, INV-10] — assert the acting model has no write path to the prediction/verification verdict columns; a **deviation is never auto-resolved**.
- **Evidence provenance** [O, INV-11] — feed a fabricated / empty / stale / target-mismatched evidence fence and assert the auto-resolve guard **rejects it** (the predecessor accepted any triple-backtick line) [O, M-13].
- **Cross-tenant / cross-session isolation** [O, INV-12; R, paradigm-rule 1] — attempt cross-tenant reads/writes (blocked by RLS + NOT NULL FK), cross-room approval misattribution, and bystander cancel; assert cancel = `TerminateWorkflow(id)` targets exactly one workflow and no process-wide lock/`pkill` exists.
- **Concurrency & partial failure** — the same boundaries under concurrent, delayed, and partial-failure inputs, using Temporal's deterministic replay testsuite so a mid-tool crash resumes correctly [F, per-turn snapshots; R, continue-as-new].

### 3.2 The shared negative-fixture / fuzz corpus

A single reusable corpus — **metacharacters, separators, newlines, Unicode, oversized inputs, duplicate/replay events** [O, INV-22, P3-2] — is fired at **every ingress and actuation boundary** by construction, so a newly-added adapter inherits the full hostile battery automatically. The corpus lives in one package imported by every boundary test; adding an adapter without wiring it to the corpus fails the boundary-coverage gate.

### 3.3 Ledger tamper-replay

The append-only, SHA-256 hash-chained **governance ledger** [F; O, INV-19] is exercised by a **tamper-replay test**: mutate/reorder/drop a row and assert the `LedgerVerifier` **rejects the chain**. Both *broken* and *stale* chain states page critical [F]. In the multi-tenant build, per-tenant chain isolation is asserted so no tenant can read or verify another's chain [R, paradigm-rule 5/governance reframe]. The ledger is preserved by integrity-preserving **archival/sealing under a per-tenant/compliance TTL + legal-hold policy — never broken to satisfy TTL** [R].

### 3.4 Round-trip contract conformance

Because every wire contract, DDL, validator, and human-facing count is **generated from one typed source per entity** [O, INV-15], a **round-trip test writes a real row via the production path → reads it back → validates it against the generated OpenAPI/JSON-Schema contract** [O, INV-22, M-01]. This catches the predecessor's class of published schemas that required fields real rows never had. CI fails on any drift, uncovered endpoint path, hand-written number, or missing `generated_at`/source-hash provenance.

### 3.5 Isolation & module-registration assurance

Two structural properties get their own gates:

- **No dead capability path** [O, INV-17; R, module system] — the manifest reconciler asserts live registered adapters/workflows exactly match the signed declared manifest; a CI grep+build gate forbids retired identifiers; a `find_dead_code` gate fails on unreachable code. A **disabled or unregistered module has no execution path** — the test proves the "dead OpenClaw path still executable" class is uncompilable, not merely discouraged.
- **Suppression rules are temporally bounded and live-verified** [O, INV-20] — assert an expired, unverified, or config-contradicted suppression/maintenance rule **fails OPEN** (incident investigated), and that suppression knowledge is never hardcoded into a prompt.

---

## 4. The synthetic canary — advisory only, never the deployment authority [O]

TG keeps the predecessor's **synthetic-incident canary** [F] — a probe that exercises the classify → predict spine daily — but with its authority explicitly bounded [O, INV-22]:

- It runs against an **isolated, ephemeral Postgres (never the live/tenant DB)**, with a **live-DB-leak counter that must stay 0** [F; O, INV-22]. It structurally cannot pollute real tables, collide a real fail-closed gate, or trigger real remediation.
- It is a **low-weight, advisory Prometheus metric** [O, INV-22]. Its green does **not** authorize a release.
- The **CI/CD deployment gate requires** the adversarial e2e / negative suites (§3) **plus a production-like canary** to pass — the synthetic self-score is a smoke test, demoted from the "headline safety claim" role it wrongly held in the predecessor [O, M-14].

This closes the "synthetic self-eval masquerades as production safety proof" threat [O]: the canary tells you the spine is *breathing*; the boundary suite and the whole-trajectory benchmark tell you it is *safe* and *effective*, and only the latter two gate the door.

---

## 5. Definition of Done — the release gate

A change is **done**, and TG may deploy it, only when **all** of the following are green:

1. **Regression** corpus (§1.2) passes with no known-good regression, and **no `>20pt` regression-vs-holdout gap** on the most recent monthly sealed-holdout run (§1.3) [F].
2. **Whole-trajectory VISR** (§2.1) holds at or above the release floor **per stratum and per tenant**, and the **5-mode comparative experiment** (§2.3) shows Full ≥ the RAG-disabled and deterministic-only controls where the strata say it should — reported honestly if not [O; F].
3. **Boundary-coverage map** (§3.1) shows **≥ 1 adversarial test per declared trust boundary**, all green; the shared fuzz corpus, ledger tamper-replay, parser-path enumeration, ActionManifest re-gate, round-trip contract conformance, and cross-tenant isolation tests pass [O, INV-22 and family].
4. **Judge calibration** floors (TPR/TNR ≥ 0.70) hold and no judge-death warning is active [F, spec/004].
5. **Provenance** — every generated artifact (scorecard, contract, count, coverage map) carries a non-null `generated_at` + source hash + coverage scope; CI fails on any hand-written number or missing provenance [O, INV-15/INV-22].
6. The **synthetic canary is green but is not counted as authority** (§4) [O].

The mechanical safety core, the fail-closed prediction gate, and the never-auto floor are **not tunable by any of the above** — they are asserted invariant under adversarial fuzz (§3.1) and hold regardless of tenant policy, confidence, or any config [F/R, paradigm-rule 8; O, INV-09]. See `CONSTITUTION.md` for the core itself and `ARCHITECTURE.md` for where each gate sits in the Temporal workflow.
