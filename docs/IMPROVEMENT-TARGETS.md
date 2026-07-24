# Improvement targets — what TG adopts from the competitive analysis

**Provenance:** [R] reframe / market-positioning layer. Derived from an external competitive analysis of
the AI-SRE field (July 2026), audited line-by-line against TG's actual code and live deployment. This
document is the *adoption backlog*: the report's still-valid recommendations, filtered through what TG
already is, prioritized, and each pinned to a YouTrack issue. It is a companion to
[`EXTERNAL-AUDIT-LESSONS.md`](EXTERNAL-AUDIT-LESSONS.md) (the "consolidate, don't sprawl" lessons) and
feeds [`ROADMAP.md`](ROADMAP.md).

The guiding filter: **spend effort on making TG's differentiator provable and legible, not on chasing
feature-parity with SaaS incumbents.** TG's moat is the committed-prediction + sole-mechanical-verifier +
degree-preserving falsifiability control + non-configurable never-auto floor. Every target below either
turns that moat into evidence, removes a concrete liability the report named, or de-risks the project —
and we explicitly *skip* the report's advice that would dilute the moat (petabyte-scale RCA parity, a
hosted SaaS, generic Datadog feature-matching).

---

## What the audit found already done (crossed off)

- **Browser-auth operator path.** The report's Stage-2b "hard blocker: machine-auth-only" is **shipped**
  (REQ-508: durable operator sessions, real login form, RBAC-gated read surfaces). No longer a target.
- **A live, wired console.** The report described the console as a static preview; it is now a deployed
  14+-surface live-wired operator console (sessions, alerts, governance, secrets, models, estate, events,
  contract, and now grounding + knowledge). No longer a target.
- **Live estate + functional LLM.** Estate graph is live (NetBox + Proxmox + LibreNMS, ~383 edges);
  the LiteLLM ladder is functional (DeepSeek → Mistral → z.ai). The report's "not yet wired" caveats are
  stale.
- **★ Grounding Scorecard (TARGET-01) — DONE this cycle.** See below.

---

## The adoption backlog (priority order)

### TARGET-01 · Grounding Scorecard — publish the differentiator as evidence ✅ DONE
**Why (report):** TG's headline claim — the mechanical verifier and falsifiability control — is
*"asserted, not externally audited."* The single highest-leverage move is to turn the claim into a number.
**Delivered this cycle:** governed read surface `GET /v1/grounding` (REQ-517) aggregating the real
verdict/prediction/audit tables — verifier match/partial/deviation distribution + match-rate, blast-radius
precision/recall, the **falsifiability signal** (avg real true-positives vs the degree-preserving
shuffled-graph control, INV-22), and the autonomy-band distribution + never-auto floor-hold count. Honest
zeros over an empty spine; fails closed. Plus a console **Grounding** surface rendering it. Meaningful
today in Phase 1 — predictions + verdicts already fire on self-recovered (AUTO-band) incidents.
**Follow-on:** publish a periodic signed transparency report generated from this surface once real
incident volume accumulates (depends on the LibreNMS ingest fan-out landing).
**Shipped:** REQ-517 backend (MR !221, merged) + console Grounding + Knowledge surfaces (MR !222).

### TARGET-02 · Benchmark harness — lead with the safety story · [TG-30]
**Why (report):** credibility in this field is earned by published evals (ITBench / ITBench-AA, Parity's
SREBench). TG can publish RCA numbers *and*, uniquely, its safety numbers — verifier match-rate,
never-auto-floor behavior, falsifiability delta — that no competitor can report.
**Scope:** an eval harness that drives TG's native agent loop over a standard incident-task set in a
sandbox, measuring RCA/resolution *and* emitting the Grounding-Scorecard metrics per run. Reuses the
REQ-517 aggregation. Lands under `docs/TESTING-AND-BENCHMARK.md` + a new `bench/` harness. Read-only;
mutation stays OFF (score the *proposals*, not executed effects, until Phase 2).
**First step:** stand up ITBench-AA task ingestion → agent-loop driver → scorecard emit for one task.

### TARGET-03 · Human vote-consuming approval loop — close the governed loop · [TG-31]
**Why (report/audit):** the approval *poll* is built (`BuildApprovalPoll`), but nothing consumes a human
vote to authoritatively release or deny a specific `action_id`. This is the missing half of "governed
autonomy" and a prerequisite for a credible Phase-2.
**Scope:** a vote intake bound to `action_id` that, on a valid approver vote, releases exactly that
POLL_PAUSE-held action (or records a deny) into the ledger — never a blanket unlock. Digest-only
approver identity, rate-limited, fully audited, idempotent. Still gated by mutation OFF: the loop records
the decision; execution stays disabled until Phase-2 sign-off.
**First step:** design the vote→release binding + ledger decision kind under spec/004 (governance).

### TARGET-04 · OpenTelemetry (OTLP) ingest — speak the universal language · [TG-32]
**Why (report):** the report's most concrete integration liability is enterprise observability
(Datadog/Splunk/New Relic/Dynatrace). Filtered through TG's sovereign thesis, the highest-value,
non-diluting add is **OTLP ingest** — vendor-neutral, self-hostable, the universal standard — ahead of
any single SaaS adapter. Optional Datadog/PagerDuty *read-only alert* adapters can follow without
compromising sovereignty; we do **not** chase Datadog feature-parity.
**Scope:** an OTLP receiver connector (spec/008 family) normalizing OTLP signals to the
`IncidentEnvelope`. Read-only ingest; registry-gated (INV-17).
**First step:** an `modules/ingest/otlp` adapter mapping OTLP logs/metrics/traces alerts → envelope.

### TARGET-05 · Positioning reframe — name the market · [TG-33]
**Why (report):** TG reads as a Resolve/Datadog competitor to the casual observer; it is not. Its market
is **governed autonomy for sovereign / regulated / air-gapped estates.** Say so explicitly.
**Scope:** a positioning pass over `README`, `docs/PRODUCT.md`, and `docs/00-README.md` making the
sovereign/regulated/self-hosted stance and the "the one channel allowed to say no" thesis the lead. Docs
only; no code.
**First step:** a one-paragraph positioning statement in PRODUCT.md + README hero.

### TARGET-06 · SOC2-style controls narrative — for the regulated buyer · [TG-34]
**Why (report):** TG's actual buyer (regulated/sovereign) needs a controls story. TG already has the raw
material: the SHA-256 hash-chained ledger, the 22 structural invariants, the STRIDE threat model, the
never-auto floor. Map them to a controls matrix.
**Scope:** a `docs/CONTROLS.md` mapping ledger + invariants + THREAT-MODEL to a SOC2-flavored control
matrix (change management, access control, audit logging, segregation of duties). Narrative + evidence
pointers; not a certification. Docs only.
**First step:** draft the control-to-evidence matrix skeleton citing INV-01..22 + the ledger.

### TARGET-07 · Verify/enforce match-only memory admission — make a stated differentiator real · [TG-29]
**Why (report/audit):** the report claims TG's memory admits only confirmed-*match* incidents (so the
learned tier can never be poisoned by unverified or deviation outcomes). The audit could **not** verify an
enforced admission gate in the learned/knowledge tier — it may be design intent, not enforcement. If real,
it is a genuine differentiator; if merely intended, it is a latent claim to build or drop.
**Scope:** audit the knowledge/learned tier (`knowledge/`, the co-occurrence/learned store) for a
verdict-gated write path. If absent, add a match-verdict admission gate on memory writes; if present,
document it and add an oracle test so the guarantee is executable, not asserted.
**First step:** trace every write into the learned tier and check whether a non-match verdict can reach it.

### DEFERRED · Bus factor / open governance (not code)
**Why (report):** a single-maintainer project is a bus-factor risk; the report suggests recruiting
committers, open governance, and a CNCF-Sandbox-style path. Real, but a people/community track outside
this engineering backlog. Noted here so it is not lost. The spec-lattice + lockstep make onboarding a
second maintainer tractable (a `CONTRIBUTING.md` + "good first spec" path is the cheap first move).

---

## Explicitly NOT adopting (would dilute the moat)
- Petabyte-scale RCA parity with log-analytics incumbents — different game, different cost structure.
- A hosted multi-tenant SaaS — contradicts the single-org, self-hosted, sovereign thesis (ADR-0010).
- Per-vendor feature-matching (Datadog-clone dashboards) — TG integrates *open* observability first.

---

*Each TARGET above is tracked as a YouTrack issue on project **TG**; TARGET-01 is complete
(REQ-517 + console Grounding surface). Update this doc as targets land.*

**YouTrack:** TARGET-02 → [TG-30], TARGET-03 → [TG-31], TARGET-04 → [TG-32], TARGET-05 → [TG-33], TARGET-06 → [TG-34], TARGET-07 → [TG-29] (project TG). TARGET-01 complete (REQ-517 + console Grounding/Knowledge, MRs !221/!222).
