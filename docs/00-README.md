# Territory Grounder — Documentation Index

> *"the one channel that is allowed to tell you no."*

**Territory Grounder** (TG; CLI `grounder`, alias `tg`) is an open-source, self-hosted,
**single-organization, multi-user** governed-autonomy SRE platform: a deterministic orchestrator that owns the
effect channel and drives an *untrusted* probabilistic model, escalating to the organization's
configured on-call/approver roles only for the genuinely irreversible or security-sensitive.
Built on **Go + Temporal + PostgreSQL/pgvector**, a **native Go agent loop** over a bundled
**LiteLLM** model-gateway, and a system of loadable **modules** for every integration surface.

This folder is the canonical documentation set. Start here.

---

## The three-layer model

Every substantive claim in these documents is tagged by provenance so the layering can never
be silently re-inverted. When two layers conflict, the ordering below (and the ratified
session corrections) decides which wins.

| Tag | Layer | What it is | Typical source ids |
|-----|-------|------------|---------------------|
| **[F]** | **Foundation** | The predecessor's *battle-tested* design that TG inherits — the deterministic-orchestrator/untrusted-model split, the incident lifecycle loop, the risk-appetite dials, the RAG/eval/self-monitoring IP. | `spec/00x`, subsystem-inventory items |
| **[R]** | **Reframe** | The product paradigm shift from a solo-operator, single-estate homelab tool to a **distributable, single-organization multi-user** product: humans-as-roles behind an RBAC/approver graph, model/vendor-agnostic adapters, API/RBAC controls, self-populating org estate knowledge. | `paradigm-rule N` |
| **[O]** | **Overlay** | The security + quality **audit hardening**: 22 invariants that make the injection/bypass/drift class *structurally uncompilable* — every good control re-founded behind a typed, authenticated interface bound to one immutable content-hashed action. | `INV-01..22`, `C-/H-/M-/P-` findings |

The predecessor — a ~300-machine, six-site homelab run by one absent human who voted 0/824
approval polls, on an n8n/Cronicle/OpenClaw/Matrix/YouTrack stack — is **provenance and
battle-test only, never the target market** [R paradigm-rule 1, mission]. Every solo /
single-user / single-vendor / host-local-sentinel assumption it carried is inverted here.

---

## Document map

| Document | Layer emphasis | One-line purpose |
|----------|----------------|------------------|
| [the-map-is-not-the-territory.md](the-map-is-not-the-territory.md) | — | The manifesto. The single organising principle: *predict → act → be surprised → update*, kept honest by an error channel the model does not author. Read for the *why*. |
| [CONSTITUTION.md](CONSTITUTION.md) | [F]+[O] | The inviolable mechanical safety core: deterministic orchestrator owns the effect channel; model is untrusted [F, INV-08]; reversibility never-auto floor [F/R paradigm-rule 8]; two-lane fail model; predict-then-verify with mechanical verdicts [INV-10]; `ActionManifest` content-hash binding [INV-07]. Non-configurable by any operator, role, or module. |
| [PRODUCT.md](PRODUCT.md) | [R] | Corrected mission, audience, and the multi-user model: who TG is for (homelab → multi-team production), humans-as-roles/approver-graph [paradigm-rule 2], org policy for cost/locality/retention, module system, first-class UX pillar. |
| [OPERATOR-QUICKSTART.md](OPERATOR-QUICKSTART.md) | [F] | **From a fresh deploy to governed actuation.** The honest, exact path: a fresh TG actuates NOTHING (Shadow); the config triad grants *authorization* (auto-eligible); the effect-leaf config (SSH host/identity/key/known_hosts + allowlist) grants *actuation scope*; the novelty poll earns autonomy per `(host,rule)`. What it will and will NOT auto-execute. |
| [ARCHITECTURE.md](ARCHITECTURE.md) | [F]+[R]+[O] | The runtime: Go control-plane, **Temporal** workflows/activities/schedules/signals, one Postgres DSN [INV-16], the native `agent/` loop over the **LiteLLM** model-gateway, the module system (out-of-process governed plugins, ADR-0005), the served operator console (`deploy/console/v2/`), and the substrate migration off n8n/Cronicle/OpenClaw. |
  - **§4.1 The actuation vocabulary** — the registry enumerates the AUTONOMOUS action space, not everything TG may do; T1 templated / T2 compiled / T3 operator-directed. "argv is code, never loaded" is an implementation choice, NOT law.
| [CAPABILITY-INVENTORY.md](CAPABILITY-INVENTORY.md) | [F]+[R] | Every inherited subsystem with its carry-forward disposition (inherit-core / inherit-improved / reduce-scope / drop), the module surfaces (ingest / tracker / notifier+approval / CMDB / actuation / model-provider / observability), and the **execution classes** `DETERMINISTIC / FAST_AGENT / STANDARD_AGENT / DEEP_INVESTIGATION / HUMAN_LED`. |
| [DATA-MODEL.md](DATA-MODEL.md) | [F]+[R]+[O] | The Postgres/pgvector schema: the append-only tamper-evident **audit spine** (governance ledger, `session_risk_audit`, prediction log) vs the **purgeable operational body** [paradigm-rule 5]; org-global tables with RBAC on the governance layer [paradigm-rule 1]; schema-version discipline; single-source generation [INV-15/16]. |
| [GOVERNED-BEHAVIORS.md](GOVERNED-BEHAVIORS.md) | [F]+[O] | The EARS-specified behavior families in narrative form — e.g. the 3-band gate (`AUTO / AUTO_NOTICE / POLL_PAUSE`, spec/001), the fail-closed prediction gate + verdict (spec/002), spec-code lockstep (spec/007) — each hardened by its overlay invariant. The **narrative** plane of the executable `spec/` lattice, which now spans **BEH-1..14 across spec/001–028** (authoritative index: [../spec/00-INDEX.md](../spec/00-INDEX.md)). |
| [SDD-WORKFLOW.md](SDD-WORKFLOW.md) + [../spec/00-INDEX.md](../spec/00-INDEX.md) | [F]+[O] | **How the project is built.** The spec-driven-development lattice: the fixed `spec/NNN/` shape (EARS requirements → `tasks.json` DAG → godog acceptance oracles → STRIDE), the `tools/specvalidate` CI gate, and the spec↔code lockstep. Read before writing code or specs. Decision: [ADR-0009](adr/0009-spec-driven-development-lattice.md). |
| [PORTING-GUIDE.md](PORTING-GUIDE.md) | [F]+[O] | **How to port from the predecessor.** The predecessor repo path (`claude-gateway`), the reimplement-not-vendor rule, the per-spec map to predecessor source + spec files, and how the external audit is *already applied* as the `[O]` overlay (INV-01..22). Read before re-implementing a behavior. |
| [THREAT-MODEL.md](THREAT-MODEL.md) | [O] | The 15 threats and the invariant that closes each: unauth ingress, OS/SQL injection, forged-payload, prediction-gate bypass, approval-of-X→execute-Y, session hijack, cross-room misattribution, dead-path re-invocation, credential leak, fabricated evidence, synthetic-eval theatre, stored-XSS [INV-01..22]. |
| [SAFETY-CONTROLS-FRAMEWORK-MAP.md](SAFETY-CONTROLS-FRAMEWORK-MAP.md) | [O] | The external citable anchor: each invariant INV-01..22 mapped to the NIST AI RMF function it serves, with the MITRE ATLAS / OWASP Agentic references the threat model already makes and the ISO/IEC 42001 scope position from ADR-0009. States which recognised control a mechanism implements — never a risk outcome (TG-80 P3-9). |
| [AREA-OWNERSHIP.md](AREA-OWNERSHIP.md) | [O] | "Stay in your area, flag, don't reach": the one rule binding the four existing ownership layers — task↔files (`files_owned`), session↔branch (claim-before-touch), session↔ticket (the partition record), human↔law surfaces (CODEOWNERS + protected paths) — and what to do instead of reaching (TG-80 P3-9). |
| [ROADMAP.md](ROADMAP.md) | [O]+[R] | The five build phases: **Phase 0** secure read-only foundation → **Phase 1** typed spine + action binding → **Phase 2** governed autonomy behind the proven gate → **Phase 3** anti-drift/single-source/decommission → **Phase 4** adversarial assurance gate. The Phase-2 flip was executed 2026-07-20 — "mutation stays globally disabled" is the historical plan posture, not the live one; actuation is governed by the mode chokepoint [INV-09; see ROADMAP.md's status note and BOARD.md § Live posture]. |
| [EXECUTION-PLAN.md](EXECUTION-PLAN.md) | [R]+[O] | The concrete engineering sequencing under the roadmap — milestones, module-delivery order, reference-adapter set, and the multi-user/RBAC rollout. |
| [TESTING-AND-BENCHMARK.md](TESTING-AND-BENCHMARK.md) | [F]+[O] | The assurance discipline: the 3-set eval flywheel (regression / discovery / **sealed holdout**) [F], adversarial boundary-coverage with a shared negative-fixture/fuzz corpus [INV-22], the orchestration invariant benchmark (I1 safety-composition / I2 determinism / I3 completeness / I4 zero-graph-gaps), synthetic canary as *advisory-only*. |
| [IMPROVEMENT-TARGETS.md](IMPROVEMENT-TARGETS.md) | [R] | The adoption backlog derived from the July-2026 competitive analysis, audited against the code: make the differentiator *provable* (★ Grounding Scorecard, done — REQ-517), a safety-first benchmark harness, the human vote-consuming approval loop, OTLP ingest, positioning + a SOC2-style controls narrative. Companion to EXTERNAL-AUDIT-LESSONS.md; each target tracked on YouTrack **TG**. |
| [SOURCE-BENCHMARK-CATALOG.md](SOURCE-BENCHMARK-CATALOG.md) + [source-audits/](source-audits/README.md) | [O] | **TG-38 source benchmark.** TG audited line-by-line against 30 external sources (books, courses, certs, vendor guides, foundational papers) — aggregate adherence, per-source scorecard, cross-source recurring gaps, where-TG-exceeds, and the de-duplicated remediation backlog (the TG-38 child issues). |
| [adr/](adr/) | [R]+[O] | Architecture Decision Records. Notably **ADR-0005** (out-of-process governed plugins as the default module mechanism) and the **no-fine-tuning** ADR (adaptation is prompt-policy iteration + RAG, never weight updates) [F]. |

Sibling documents reference each other by filename (e.g. *see ARCHITECTURE.md*). The
`CONTRIBUTING.md` at the repo root holds the **build-culture** half (honesty-over-marketing,
self-scorecards → remediation, verify-agent-claims-in-audits, spec↔code lockstep *as a build
gate*) that was deliberately split out of the runtime constitution [paradigm-rule 10].

---

## Reading order

**Mandatory (two files, <10k tokens):** the repo-root **[../AGENTS.md](../AGENTS.md)** and
**[BOARD.md](BOARD.md)** — orientation and the only live board. That is the entire entry ritual.

**Everything below is reference — read the document the task at hand actually needs:**

- Writing or changing a spec, or touching a lockstep-governed file → [SDD-WORKFLOW.md](SDD-WORKFLOW.md) + [../spec/00-INDEX.md](../spec/00-INDEX.md)
- Touching safety, actuation, or the gate → [CONSTITUTION.md](CONSTITUTION.md) (+ [THREAT-MODEL.md](THREAT-MODEL.md))
- Understanding the runtime → [ARCHITECTURE.md](ARCHITECTURE.md); where state lives → [DATA-MODEL.md](DATA-MODEL.md)
- Changing prompts/skills/models → [EVAL-GATE.md](EVAL-GATE.md)
- Porting predecessor behavior → [PORTING-GUIDE.md](PORTING-GUIDE.md) + [PORT-FIDELITY-AUDIT.md](PORT-FIDELITY-AUDIT.md)
- Benchmarking vs the predecessor → [BENCHMARK-AXES.md](BENCHMARK-AXES.md) + [BENCHMARK-LADDER.md](BENCHMARK-LADDER.md) + `tools/shadowbench/PRE-REGISTRATION.md`
- The *why* → [the-map-is-not-the-territory.md](the-map-is-not-the-territory.md); the *who for* → [PRODUCT.md](PRODUCT.md)
- A past decision's rationale → [adr/](adr/); contributing culture → [../CONTRIBUTING.md](../CONTRIBUTING.md)

## Reference shelf (indexed so nothing is invisible; none of it is entry reading)

- **Live board:** [BOARD.md](BOARD.md) — the ONLY live status surface. History: [history/](history/).
- **Deployment:** [OPERATOR-QUICKSTART.md](OPERATOR-QUICKSTART.md), [PHASE-2-READINESS.md](PHASE-2-READINESS.md), [LOCAL-MODEL-RUNBOOK.md](LOCAL-MODEL-RUNBOOK.md) (gpu01 hardware recon, the local-model ladder plan, and the eval-gated migration path off hosted models; facts verified live 2026-07-31)
- **Quality & audits:** [EVAL-GATE.md](EVAL-GATE.md), [TESTING-AND-BENCHMARK.md](TESTING-AND-BENCHMARK.md), [CONSOLE-UX-AUDIT-REGISTER.md](CONSOLE-UX-AUDIT-REGISTER.md) (closed register), [EXTERNAL-AUDIT-LESSONS.md](EXTERNAL-AUDIT-LESSONS.md), [SOURCE-BENCHMARK-CATALOG.md](SOURCE-BENCHMARK-CATALOG.md) + [source-audits/](source-audits/README.md), [PORT-FIDELITY-AUDIT.md](PORT-FIDELITY-AUDIT.md), [PREDECESSOR-MECHANISM-INVENTORY.md](PREDECESSOR-MECHANISM-INVENTORY.md) (the predecessor's intelligence layer — retrieval, prompt assembly, skills, memory, which lived *outside* its spec tree and was invisible to the port by construction — enumerated against re-derived denominators with per-mechanism PORTED/PARTIAL/ABSENT status; complements the spec-keyed PORTING-GUIDE.md and PORT-FIDELITY-AUDIT.md)
- **Benchmarks:** [BENCHMARK-AXES.md](BENCHMARK-AXES.md), [BENCHMARK-LADDER.md](BENCHMARK-LADDER.md)
- **Design references:** [SYSTEM-MAP.md](SYSTEM-MAP.md), [ARCHITECTURE-DESIGN-WISDOM.md](ARCHITECTURE-DESIGN-WISDOM.md), [CAPABILITY-INVENTORY.md](CAPABILITY-INVENTORY.md), [CONNECTOR-INVENTORY.md](CONNECTOR-INVENTORY.md), [GOVERNED-BEHAVIORS.md](GOVERNED-BEHAVIORS.md), [KNOWLEDGE-CORPUS-SEED.md](KNOWLEDGE-CORPUS-SEED.md), [IMPROVEMENT-TARGETS.md](IMPROVEMENT-TARGETS.md), [ROADMAP.md](ROADMAP.md), [EXECUTION-PLAN.md](EXECUTION-PLAN.md)
- **Vision documents — NOT build input** (aspirational design, no current implementation claim):
  [GITOPS-REGIME-ACTUATOR-SPEC.md](GITOPS-REGIME-ACTUATOR-SPEC.md), [FEDERATION-VISION.md](FEDERATION-VISION.md), [TGCTL-DECLARATIVE-VISION.md](TGCTL-DECLARATIVE-VISION.md), [K8S-MANAGEMENT-REGIME-DESIGN.md](K8S-MANAGEMENT-REGIME-DESIGN.md), [PRIOR-ART-tracer-and-federation.md](PRIOR-ART-tracer-and-federation.md)
