# Safety controls → external frameworks

> Reference, not a work queue (TG-80 P3-9, 2026-08-22). This maps Territory Grounder's 22 structural
> invariants (INV-01..22, the `[O]` overlay in `docs/CONSTITUTION.md`) onto the NIST AI Risk Management
> Framework's four functions and, where the threat model already cites them, MITRE ATLAS and the OWASP
> Agentic Top 10. It is the citable anchor for an external reader: "which recognised control does this
> mechanism implement?" It adds no new control — every row points at a mechanism that exists and is
> enforced in this repo, and the mechanism's own documentation stays authoritative.

## How to read the mapping

- **NIST AI RMF 1.0** organises risk work into GOVERN (policies, roles, accountability), MAP (context,
  intended use, impact), MEASURE (test, evaluate, verify) and MANAGE (respond, prioritise, recover).
  TG's invariants are *implemented controls*, so most land in MANAGE and MEASURE; the few that are
  organisational law (what an agent may change, how evidence is kept) land in GOVERN.
- **ISO/IEC 42001** is a management-system standard; TG's ADR-0009 records the deliberate position that
  TG builds the *runtime half* (mechanisms) and leaves the management-system half to the operator's
  organisation. Rows therefore reference the 42001 control *family* only where TG supplies its evidence.
- MITRE ATLAS / OWASP Agentic references are reproduced **only** where `docs/THREAT-MODEL.md` already
  makes them (§5.2 dual-use read lane: ATLAS AML.T0098/AML.T0025, OWASP ASI10; §5.3 unconstrained
  outbound: AML.T0025, NIST SP 800-207). Nothing is attributed to those catalogues beyond that.

## The mapping

| INV | Mechanism (one line) | NIST AI RMF function · subcategory family | Other anchors already cited | Where it is enforced |
|---|---|---|---|---|
| INV-01 | Every HTTP route authenticated by non-bypassable middleware; an `auth=none` route refuses to register | MANAGE · access control / MAP · intended interfaces | NIST SP 800-207 (PEP) | `core/httpapi` router registration; `core/auth` |
| INV-02 | No shell: fixed argv vectors, never `sh -c`; CI lint | MANAGE · unsafe-action prevention | OWASP ASI (tool misuse class) | `scripts/lint-forbidden.sh`; `modules/actuation/ssh` argv builder |
| INV-03 | No string-built SQL; parameterised queries only; CI lint | MANAGE · unsafe-action prevention | — | P0-8 CI lint gate; `core/db` |
| INV-04 | One schema-validated typed envelope at ingest; identifiers pass explicit grammars | MAP · input boundary / MEASURE · validation | — | `core/ingest` envelope + grammars |
| INV-05 | Webhook payloads are claims: re-read the entity by ID from its system of record before dispatch | MEASURE · provenance verification | — | ingest re-read path; `core/ingest` |
| INV-06 | One canonical parser, one grammar; a poll is constructible only from a gated proposal | MANAGE · single-path decision | — | `core/proposal` + `PredictionGate` types |
| INV-07 | One content-hashed `action_id` threaded unchanged; any change re-enters the gate | MANAGE · integrity of the approved action | — | `core/manifest` (`ActionManifest`), interceptor re-assertion |
| INV-08 | The model is untrusted: no model token becomes control flow, a command or a query | GOVERN · accountability for model output / MANAGE | OWASP Agentic (injection class) | `agent/` typed protocol; argv-as-grammar leaf |
| INV-09 | Mechanical never-auto floor, non-configurable, zero value fails closed | MANAGE · human oversight floor | — | `core/safety` band enum + adapter floor |
| INV-10 | Predict-then-verify outside the LLM; deterministic verdict; deviation never auto-resolves; negative control | MEASURE · pre/post verification | — | `core/predict`, `core/safety` verdict |
| INV-11 | Evidence-bound claims: only cited orchestrator-captured tool results are admissible | MEASURE · provenance of claims | — | `core/actuate` evidence gate |
| INV-12 | Session and approval isolation keyed by decision id; one workflow per session | MANAGE · isolation / GOVERN · decision binding | — | Temporal workflow IDs, `SignalWithStart` |
| INV-13 | No credential value in any artifact; references resolved at runtime from a secret store | GOVERN · secrets policy / MANAGE | — | `core/config` SecretRef schemes; gitleaks CI |
| INV-14 | Declared retention per data class, automated audited purge, minimisation before write | GOVERN · data policy / MANAGE · retention | — | `expires_at` + purge workers; SECURITY DEFINER reapers |
| INV-15 | Single-source generation of every contract, DDL, validator; CI fails on drift | GOVERN · configuration management / MEASURE | — | `tools/gencontracts`, `specvalidate` lockstep |
| INV-16 | One database, migrations only at deploy, DML-only runtime role, integrity by construction | MANAGE · data integrity | — | `core/db/migrations`; append-only-by-default privileges (0105, TG-80) |
| INV-17 | A capability exists only if compiled in and registered; manifest reconciler refuses mismatch | GOVERN · capability inventory / MAP | — | module registry + boot reconciler |
| INV-18 | Exactly one implementation per stage; sites are configuration, never forks | GOVERN · configuration management | — | registry duplicate-abort at boot |
| INV-19 | Every governance decision is a required typed record on a tamper-evident ledger; no-UPDATE/DELETE grants; verifier re-walk; Temporal-history witness | GOVERN · accountability & records / MEASURE · audit | — | `core/audit`, `governance_ledger`, ledger-anchor witness |
| INV-20 | Suppression rules are temporally bounded registry rows; expired/unverified fails open | MANAGE · controlled exceptions | — | `core/suppression` registry |
| INV-21 | Interception lifecycle wired by construction through one chokepoint; a control that cannot run fails loud | MANAGE · enforcement by construction | — | `core/actuate` interceptor chain, `SelfTest` |
| INV-22 | Synthetic self-scoring is never the release authority; adversarial boundary coverage + canaries gate release | MEASURE · evaluation governance | — | eval gate, `TestEveryTrustBoundaryHasAdversarialCoverage` (TG-5) |

## Coverage notes an external reader should have

- **What TG does not claim.** No row above asserts a NIST AI RMF *outcome* (e.g. "risk is acceptable");
  the mapping states which function a mechanism serves. Synthetic benchmark numbers are explicitly not a
  release authority (INV-22), and the source catalogue records that agentic benchmark scores mis-estimate
  by up to 100% relative — see `docs/TESTING-AND-BENCHMARK.md`.
- **Residual risks** are kept in `docs/THREAT-MODEL.md` §5 (the dual-use read lane, unconstrained
  outbound, the ledger-witness squat window) with their ATLAS/OWASP references; they are not re-stated
  here so that one place stays authoritative.
- **Management-system scope.** Per ADR-0009, the organisational controls ISO/IEC 42001 asks for (roles,
  competence, management review) belong to the operating organisation; this repo supplies the runtime
  evidence those controls consume — the ledger, the eval history, the lockstep manifest, the generated
  contracts.
- **Publication.** This file ships to the public mirror with the rest of `docs/`; it names mechanisms and
  paths, never estate identities.

## Cross-references

`docs/CONSTITUTION.md` (the invariants, inline), `docs/THREAT-MODEL.md` (threat → control table and the
residual-risk sections), `spec/00-INDEX.md` (spec → invariant matrix), `docs/GOVERNED-BEHAVIORS.md`
(BEH-1..14 tagged by invariant), `docs/adr/0009-spec-driven-development-lattice.md` (the standards
position), `docs/EXTERNAL-AUDIT-LESSONS.md` (predecessor failure → invariant).
