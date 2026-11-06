<!-- spec/026 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/026 — Threat model: the open proposal plane (STRIDE slice)

Per-feature slice; the system-wide model is [`docs/THREAT-MODEL.md`](../../docs/THREAT-MODEL.md).

**Trust boundary.** Model-generated free text (op_class slug, rationale, undo sketch, actor-evidence
claims) crosses from the untrusted LLM output surface into three trusted sinks: the database
(session_triage), the org-global governance ledger, and the operator's console. The ASSET is twofold:
(1) the never-executable guarantee — a free-form proposal must have no path to an effect leaf; (2) the
integrity of what operators read — a proposal is an argument aimed at a human who can later ratify it
(spec/028), so persuasion-shaped text is the attack payload.

| STRIDE | Threat | Mitigation |
|---|---|---|
| Spoofing | Model fabricates actor-evidence ("root@pam stopped it") to steer banding | REQ-2610: evidence carries a source reference; REQ-2611 composes floors in the safe direction only — fabricated evidence can only RAISE the approval bar, never lower it; evidence text is screened (REQ-2606) |
| Tampering | Free-form op_class engineered to collide with a registered slug and inherit its executability | Divert predicate is exact-slug `opschema.Lookup` (REQ-2603); a collision does not divert and enters the gated lane where sealing and banding treat it as its registered self — no new capability is conferred by naming |
| Repudiation | Shadow proposals happen off the record | REQ-2604 durable row + REQ-2605 exactly-one withheld GovDecision on the ONE hash chain (INV-19) |
| Information disclosure | Secret-shaped text in rationale/undo_sketch persisted or rendered | REQ-2606 screen.Scrub before persist, ledger, and console |
| Denial of service | Proposal flood into the approval lane exhausts operator attention (the pre-spec hazard: humans polled to approve unexecutable actions) | REQ-2603 diverts BEFORE notify/projection/vote — shadow proposals never page, never open votes |
| Elevation of privilege | A shadow proposal reaches execution | REQ-2608 cites the five-layer refusal chain (nil sealedArgv → nil sealEffect → effect-kind refusal → empty-argv leaf refusal → never-auto floor → mode chokepoint) and pins it with a defense-in-depth oracle; no new actuation machinery exists in this spec |

**Residual risk.** Persuasion: a well-written wrong proposal read by a tired operator. Bounded by the
plane's construction — nothing in this spec grants execution; ratification (spec/028) is a separate
operator act behind an empty form, a laundering tripwire, and admission validation. INV references:
INV-06, INV-07, INV-08, INV-11, INV-15, INV-19.
