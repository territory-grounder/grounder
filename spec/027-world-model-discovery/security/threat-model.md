<!-- spec/027 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/027 — Threat model: the auto-drafted world model (STRIDE slice)

Per-feature slice; the system-wide model is [`docs/THREAT-MODEL.md`](../../docs/THREAT-MODEL.md).

**Trust boundary.** Estate observations (systemd, docker, PVE, NetBox, LibreNMS) cross from
semi-trusted infrastructure into a store whose APPROVED rows widen what the actuation leaves will
accept. The ASSET is the allowlist: a wrongly-adopted entry expands the blast surface of every
future ratified op-class; a wrongly-retired entry silently removes an operator's grant.

| STRIDE | Threat | Mitigation |
|---|---|---|
| Spoofing | A compromised or misconfigured source (rogue container names, forged unit descriptions) drafts entries that look adoptable | Closed entity/relation vocabularies loud-reject unknowns (REQ-2701); per-source provenance + confidence render on the review diff; adoption is a human act with mandatory rationale (REQ-2703); free-text from sources is screened before render |
| Tampering | Direct DB edit of manifest_entry to smuggle an allowlist entry past review | Transitions only via the audited Transition chokepoint with ledger-before-row (REQ-2702); materialization reads approved rows whose adopt decisions are on the hash chain — an unledgered approved row is an audit finding (INV-19) |
| Repudiation | "Who allowed actions on this host?" | Every adopt/reject/retire carries a server-derived principal + rationale + ledger_seq on the ONE chain |
| Information disclosure | Manifest rows leak topology or secret-bearing unit descriptions | INV-13 projections; screening on source free text; console is AuthReadOnly for reads, AuthSession for writes |
| Denial of service | Discovery flood or a flapping source churns drafts and buries the reviewer | Per-source isolation (a failing source contributes nothing, loudly — REQ-2705); drift presents incremental diffs, not full re-drafts; stale-not-retired keeps approved state stable under source flap |
| Elevation of privilege | Adoption widens actuation beyond the operator's intent | Leaf default-deny gates are byte-untouched (REQ-2704) — adoption only feeds them; two independent grants (adopted target AND ratified class, REQ-2707) are never merged; guardallow host guard remains final authority |

**Residual risk.** Reviewer fatigue rubber-stamping large first-run manifests. Bounded by
per-section approve, provenance labels, and the fact that adoption alone executes nothing —
capability requires the second, separately-earned grant (spec/028). INV references: INV-13, INV-14,
INV-15, INV-17, INV-19.
