<!-- spec/021 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/021 — Threat model: The groundnet contract (STRIDE slice)

Per-feature threat slice for the groundnet CONTRACT — the wisdom-chunk envelope, the typed `Emit` / `Ingest`
adapter seam, and the born-compatible invariants. The system-wide model is
[`docs/THREAT-MODEL.md`](../../docs/THREAT-MODEL.md); this file scopes the contract's own trust boundary and
is the security half of the spec's definition-of-done. Federation is a federation of an infrastructure
control plane's BRAIN, which is genuinely dangerous — this model states the hazards plainly so no reader
mistakes the contract for a free lunch. The full network mechanism (transport, coordinator, reputation
service) is threat-modelled in [`docs/FEDERATION-VISION.md`](../../docs/FEDERATION-VISION.md) § 5–6; here the
scope is the contract each node is born carrying.

**Trust boundary.** The contract sits ABOVE the local platform (the decision tracer spec/020, the graduation
ladder spec/015, the actuation interceptor spec/013, the mode chokepoint `core/safety`, the hash-chained
`governance_ledger` migration 0015) and BELOW the far-future network mechanism. Its inputs on the `Emit`
side are a chunk assembled ONLY from the spec/020 REQ-2017 generalizable projection; on the `Ingest` side, a
foreign, signed, pseudonymously-attested wisdom chunk. Its outputs are the emitted chunk (published to the
transparency log) and, on ingest, a SUBORDINATE hint entering local re-graduation — never an authority. The
assets are (1) the estate map latent in a raw trace (which the contract must NEVER let leave), (2) the
integrity of the local decision brain against a poisoned foreign chunk, and (3) the pseudonym's signing key.
Adversaries of interest: (a) a hostile contributor trying to get a bad diagnosis or destructive op-class
adopted as trusted automation across the network (poisoning); (b) a passive observer reconstructing an
estate from what a node shares (the reconnaissance-feed leak); (c) an attacker tampering with or censoring a
chunk in transit or in the log; (d) a Sybil flooding the network with fake pseudonyms to manufacture
reputation; (e) a replay of an old chunk to inflate reputation or re-trigger ingest.

**The deliberate posture (threat-modelled honestly).** Sharing a control plane's wisdom is powerful — but
the shared unit is NARROWER than a trace, not wider: it is the generalizable *(class → diagnosis → op-class
→ verified-outcome)* distillate that names kinds, never instances (REQ-2101), carried by TYPE across the
seam so there is no anonymization to get wrong. It is safe ONLY because (1) the estate-specific layer has no
export path in the contract (REQ-2101) and the producer is a de-identified pseudonym, not an identity
(REQ-2103); (2) an ingested chunk is SUBORDINATE — it re-graduates locally and passes the full local gate
stack unchanged, so a poisoned chunk can only propose (REQ-2109, REQ-2110); (3) provenance is a signed,
multi-witness transparency log with inclusion proofs, not a blockchain and not a bare feed (REQ-2104,
REQ-2105, REQ-2115); and (4) reputation is verified-outcome-weighted over authenticated members, so volume
buys nothing (REQ-2107, REQ-2111). A shared chunk grants a consumer no authority its own constitution did
not already grant.

| STRIDE | Threat | Control | Requirement / invariant |
|---|---|---|---|
| **Spoofing** | A hostile node forges a chunk as a reputable producer, or a Sybil mints many pseudonyms to manufacture reputation and drown honest signal | Every chunk carries a signature over the envelope bound to a producer pseudonym and is verified before ingest; membership is authenticated and default-off (not a public dump); reputation is weighted by verified-outcome quality across authenticated members, so a fresh Sybil pseudonym with no verified outcomes carries no weight and volume earns nothing | REQ-2103, REQ-2104, REQ-2107, REQ-2111, INV-01/INV-13 |
| **Tampering (poisoning)** | A hostile contributor gets a bad diagnosis or a destructive op-class adopted as trusted automation across the network — a supply-chain attack on the control plane's brain | An ingested chunk is a SUBORDINATE hint: it does not inherit the producer's trust, it re-graduates against the consumer's OWN local traffic and verified outcomes before it earns any standing, and any action it influences still passes the never-auto floor, the policy verdict, the interceptor / mutation keystone, and the mode chokepoint unchanged; a poisoned chunk that cannot produce local verified-clean outcomes never re-graduates and fails closed on every estate independently | REQ-2109, REQ-2110, INV-08/INV-09/INV-10/INV-21 |
| **Tampering** | A chunk is altered in transit or in the provenance store to change its diagnosis or op-class | The signature over the envelope binds the payload to the producer pseudonym and is verified before ingest; the provenance_chain is anchored in an append-only, multi-witness transparency log whose inclusion proofs make tampering evident, chained from the producing node's local hash-chained governance_ledger | REQ-2104, REQ-2105, REQ-2106, INV-19 |
| **Repudiation** | A producer denies emitting a chunk, or a chunk cannot be tied to the graduation evidence that produced it | The chunk is signed by the producer pseudonym and logged in the append-only transparency log with an inclusion proof; verified_outcome_evidence and the local ledger anchor tie the chunk to the graduation that produced it; every Emit and Ingest is an immutable, ledger-anchored row on a table the runtime role cannot UPDATE or DELETE | REQ-2100, REQ-2105, REQ-2106, REQ-2114, INV-19 |
| **Information disclosure (reconnaissance feed)** | A passive observer reconstructs an estate — topology, naming, addressing, posture — from what a node shares, or a producer identity relabels who-had-which-incident | The estate-specific layer (hosts, IPs, topology, credential identities, raw traces) has NO export path in the contract; Emit sources ONLY the spec/020 generalizable projection by type; the payload carries no estate identifier; the producer is a stable de-identified pseudonym, never a real or estate identity; a public tier exists only for provably zero-estate-specific distillate | REQ-2101, REQ-2103, REQ-2111, INV-13/INV-14 |
| **Information disclosure** | A secret (credential, member token, signing key) leaks into an emitted chunk, the transparency log, or an audit row | An emitted / ingested row carries only non-secret, de-identified fields; every credential and the member / signing material remain core/config.SecretRef references held outside the chunk; no plaintext secret is ever in a chunk, the log, or a row; gitleaks CI backs the no-literal-secret rule | REQ-2101, INV-13 |
| **Denial of service (replay)** | An old or duplicate chunk is re-emitted to inflate a pseudonym's reputation or re-trigger ingest | A consumer rejects a replayed or duplicate chunk by its id and provenance_chain anchor; one chunk earns local standing at most once per node; reputation aggregates de-duplicated signed attestations (a CRDT rollup is idempotent) | REQ-2107, REQ-2115, INV-19 |
| **Denial of service (un-recall)** | A chunk shared in error cannot be recalled from every peer's local store, leaking distillate permanently | A shared chunk is treated as UNRECALLABLE by construction; the export decision is the last point of control, is opt-in / default-off / org-admin-authority, and is constrained to zero-estate-specific distillate; the org admin is told at opt-in that there is no delete after export | REQ-2111, REQ-2114, INV-14 |
| **Elevation of privilege** | The Emit or Ingest seam, or a foreign chunk's provenance / reputation, is used to reach an actuator or lift a local gate | Neither side of the seam reaches an actuator, lifts a floor, or changes the mutation posture; a foreign chunk's provenance and reputation lift NO local gate; the local constitution is sovereign; a standing check fails if an ingested chunk reaches an actuator without traversing the interceptor, the never-auto floor, and the mode chokepoint, or influences an action before it re-graduates locally | REQ-2108, REQ-2109, REQ-2113, INV-09/INV-21/INV-22 |

**Adversarial acceptance (boundary tests, Phase 4 — WHEN this spec graduates from Draft).** Assert Emit's
input type cannot express the estate-specific layer and a payload carrying an estate identifier is rejected
(REQ-2101); assert a tampered or unsigned chunk is refused before ingest and a replayed chunk earns standing
at most once (REQ-2104, REQ-2115); assert an ingested chunk influences no action before it re-graduates
locally and never lifts the never-auto floor, the interceptor, or the mode chokepoint under a replayed
decision with the hint present and absent (REQ-2109, REQ-2110); assert reputation is unchanged by
contribution volume and by a Sybil flood of unverified pseudonyms (REQ-2107); assert membership, export, and
consumption are default-off and refuse without org-admin authority (REQ-2111); and assert no emitted or
ingested row carries a plaintext secret (INV-13). These drive the actual code path (INV-22) — see
[`docs/TESTING-AND-BENCHMARK.md`](../../docs/TESTING-AND-BENCHMARK.md).
