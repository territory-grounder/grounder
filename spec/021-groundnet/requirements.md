<!-- spec/021 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/021 — The groundnet contract (federation envelope + adapter seam + invariants)

**Owning behavior family:** — (no narrative `BEH-N`; the north-star narrative is
[`docs/FEDERATION-VISION.md`](../../docs/FEDERATION-VISION.md). This spec is the STABLE CONTRACT half of
that vision — the born-compatible envelope + adapter seam + invariants a node carries — not the full network
mechanism.)
**Constitution / invariants:** INV-01, INV-08, INV-09, INV-10, INV-13, INV-14, INV-19, INV-21, INV-22.
**Phase:** far-future / P-network (blocked on the flywheel graduating an artifact, on the loadable-not-hardcoded
prose migration, and on the decision-tracer archive — see [`docs/FEDERATION-VISION.md`](../../docs/FEDERATION-VISION.md)
§ 7). This spec bears no build obligation until those land; its tasks are planned-not-active.
**Status:** Draft.

The **groundnet** (grounder + net) is the far-future federation of sovereign Territory Grounder instances,
each running its own estate under its own constitution, that share distilled, RE-VALIDATED remediation
DISTILLATE — never raw estate data. The full network mechanism (transport, member discovery, the
reputation service, the coordinator) is the far-future thesis in
[`docs/FEDERATION-VISION.md`](../../docs/FEDERATION-VISION.md). THIS document is a narrower, harder thing:
the **stable contract** — the wisdom-chunk ENVELOPE, the typed adapter SEAM, and the invariants — that a
node must be BORN compatible with, so the network can be built later without re-minting every node. A node
that ships today carrying this contract is a node the groundnet can admit tomorrow; a node that does not is a
node that must be re-cut. The contract is the compatibility surface; the mechanism is out of scope here.

This contract COMPOSES on top of the already-authored local platform and RELAXES nothing in it. The decision
tracer (spec/020) owns the two-layer trace schema whose GENERALIZABLE layer (REQ-2017) is the only thing a
chunk may carry and whose optional export lane (REQ-2020) is the local seam this contract targets; the
graduation ladder (spec/015) is the trust machine an ingested chunk must re-clear locally; the actuation
interceptor (spec/013), the constitutional never-auto floor (INV-09), and the mode chokepoint (`core/safety`)
are the gates a foreign chunk NEVER lifts; the hash-chained `governance_ledger` (migration 0015) is the local
tamper-evident spine the federated transparency log extends. The groundnet contract adds a compatibility
envelope and a seam — it authorizes nothing, actuates nothing, and adjudicates no member's actions.

## Owner decisions (OWNER-LOCKED)

The four consequential design choices are owner-locked and lifted here as decision callouts. They are
subordinate to, and never override, [`docs/CONSTITUTION.md`](../../docs/CONSTITUTION.md) §3 (the inviolable
mechanical safety core).

> **DECISION (owner): a STABLE signed ENVELOPE around a VERSIONED, evolvable PAYLOAD.** The wisdom-chunk
> envelope — `{ id, payload_version, two_layer_marker, producer_attestation, signature, provenance_chain,
> verified_outcome_evidence, payload }` — and its invariants are the STABLE contract a node is born
> compatible with. The PAYLOAD (what a graduated artifact actually carries) is a VERSIONED, evolvable field
> that grows as the flywheel learns what a graduated artifact IS. This is the HTTP posture: a stable
> envelope wrapping an evolvable body. A payload the flywheel has NEVER produced (zero graduated artifacts
> to date) cannot be frozen, so this contract freezes the envelope and its invariants and versions the
> payload — the envelope is the compatibility surface, the payload is the growth surface.

> **DECISION (owner): PSEUDONYMOUS attestation, NOT identity.** The `producer_attestation` is a signature by
> a stable PSEUDONYM (a keypair), never a real-world or estate identity. Reputation accrues to the pseudonym;
> no real estate identity ever leaves the instance; the payload is de-identified (the estate-specific layer
> is stripped). A real "producer identity" would relabel who-had-which-incident and re-open the
> reconnaissance-feed leak that the two-layer split (spec/020 REQ-2017) closes — pseudonymity is the fix, not
> a nicety. Tradeoff (owner-acknowledged): a stable pseudonym buys reputation CONTINUITY at the cost of
> per-chunk linkability, whereas unlinkable-per-chunk signing (ring or group signatures) buys maximum
> privacy at the cost of continuity. The default is the stable pseudonym; unlinkable signing is a later
> option layered on the SAME envelope.

> **DECISION (owner): PROVENANCE = a signed, append-only TRANSPARENCY LOG, NOT a blockchain.** Provenance,
> tamper-evidence, and censorship-resistance come from a signed, append-only, multi-witness TRANSPARENCY LOG
> — the Sigstore / Rekor + Certificate-Transparency model, reusing Sigstore / in-toto per the prior art —
> and explicitly NOT from a blockchain. WHY: a blockchain solves global-consensus-on-a-single-truth, which
> the groundnet does NOT need — every node re-validates locally and is subordinate-not-authority, so there
> is no single global truth to agree on. A blockchain also leaks metadata by construction, imposes
> latency / finality cost, and adds token and smart-contract attack surface to a security-critical control
> plane. TG's local hash-chained `governance_ledger` (migration 0015) is already the "blockchain of one";
> the groundnet log is its FEDERATED, signed, multi-witness EXTENSION. Reputation is a federated aggregation
> of signed pseudonymous verified-outcome attestations (a CRDT-style rollup), never an on-chain vote or
> token.

> **DECISION (owner): SUBORDINATE-NOT-AUTHORITY.** An ingested chunk is a HINT, never an authority. Before
> it can influence any action it re-runs the consuming node's OWN eval, autonomy-graduation ladder, and
> policy gate, and it passes — unchanged — the constitutional never-auto floor (INV-09), the actuation
> interceptor and mutation keystone (INV-21), and the mode chokepoint (`core/safety`). The local
> constitution is sovereign; a foreign chunk's provenance or reputation lifts no local gate. This is the
> primary anti-poisoning defense: a poisoned chunk can only PROPOSE, and a proposal dies at a gate stack
> that trusts no model output — imported or local (INV-08).

## Requirements

- **REQ-2100** — [R] federation-stance 4.1 · [O] INV-14.
  The contract SHALL define one versioned, signed wisdom-chunk ENVELOPE whose fields are `id`,
  `payload_version`, `two_layer_marker`, `producer_attestation`, `signature`, `provenance_chain`,
  `verified_outcome_evidence`, and `payload`, WHERE the envelope fields and their invariants are the STABLE
  contract, and a node SHALL be born able to parse and validate this envelope independently of the payload it
  carries.

- **REQ-2101** — [O] INV-13 · [R] federation-stance 4.1.
  The `two_layer_marker` SHALL mark every shareable chunk GENERALIZABLE, the ESTATE-SPECIFIC layer (hosts,
  IPs, topology, credential identities, raw traces) SHALL have NO export path in the contract, and the
  `payload` SHALL carry no estate identifier — the generalizable projection of the spec/020 REQ-2017 schema
  is the only content a chunk may carry, so "share" is structurally incapable of reading estate-specific
  data.

- **REQ-2102** — [R] federation-stance 4.1.
  The `payload` SHALL be a VERSIONED field named by `payload_version` and the envelope SHALL remain byte-stable
  across payload versions; a consumer SHALL reject a `payload_version` it does not understand WITHOUT rejecting
  the envelope, so the envelope is frozen and the payload evolves as the flywheel learns what a graduated
  artifact contains.

- **REQ-2103** — [O] INV-13 · [R] federation-stance 4.5.
  The `producer_attestation` SHALL be a signature by a stable PSEUDONYMOUS keypair, SHALL NOT carry a
  real-world or estate identity, and reputation SHALL accrue to the pseudonym rather than to any estate — the
  de-identified payload plus the pseudonymous producer close the reconnaissance-feed leak a real producer
  identity would open.

- **REQ-2104** — [O] INV-13 · [O] INV-08.
  Every chunk SHALL carry a cryptographic `signature` over the envelope that binds the `payload` to the
  producer pseudonym, and a consumer SHALL verify the signature BEFORE ingest and SHALL refuse any chunk whose
  signature does not verify, so an unsigned or tampered chunk never reaches the local re-graduation path.

- **REQ-2105** — [O] INV-19 · [R] federation-stance 4.5.
  The `provenance_chain` SHALL be anchored in a signed, append-only, multi-witness TRANSPARENCY LOG (the
  Sigstore / Rekor + Certificate-Transparency model, reusing Sigstore / in-toto), SHALL NOT be a blockchain,
  and tamper-evidence and censorship-resistance SHALL derive from the log's inclusion proofs rather than from
  global consensus — the groundnet is subordinate-not-authority, so there is no single global truth to
  agree.

- **REQ-2106** — [O] INV-19.
  The groundnet transparency log SHALL be the FEDERATED, signed, multi-witness EXTENSION of the local
  hash-chained `governance_ledger` (migration 0015), and a chunk's `provenance_chain` SHALL reference the
  producing node's local ledger anchor, so the "blockchain of one" that already runs on each estate is the
  root the federated log chains from.

- **REQ-2107** — [R] federation-stance 4.4.
  Reputation SHALL be the federated aggregation (a CRDT-style rollup) of signed pseudonymous
  VERIFIED-OUTCOME attestations weighted by verified-outcome quality — whether the fix verified clean when
  OTHER nodes applied it — SHALL NOT be an on-chain vote or token, and SHALL NOT be weighted by contribution
  volume.

- **REQ-2108** — [R] federation-stance 4.1 · [O] INV-22.
  The node SHALL implement a typed adapter SEAM exposing `Emit(chunk)` and `Ingest(chunk)`, WHERE `Emit`
  sources its chunk ONLY from the spec/020 REQ-2017 generalizable layer and `Ingest` lands a chunk into the
  local re-graduation path (REQ-2110) — the seam is the sole crossing point, and neither side reads the
  estate-specific layer.

- **REQ-2109** — [O] INV-08 · [O] INV-09 · [O] INV-21.
  An ingested chunk SHALL enter as a HINT subject to the full local gate stack: it SHALL re-run local eval,
  the graduation ladder, and the policy gate, and SHALL NOT bypass the actuation interceptor, the
  constitutional never-auto floor, or the mode chokepoint; the local constitution SHALL remain sovereign
  regardless of the chunk's provenance or reputation.

- **REQ-2110** — [F] flywheel · [O] INV-10.
  An ingested chunk SHALL NOT inherit the producer's trust; it SHALL RE-GRADUATE against local traffic and
  local verified outcomes before it earns any local standing, and trust SHALL be re-earned per node and never
  transferred across the boundary — federated graduation is the reason a poisoned chunk cannot propagate as
  authority.

- **REQ-2111** — [R] federation-stance 4.5 · [O] INV-01.
  Groundnet membership, export, and consumption SHALL each be opt-in, DEFAULT-OFF, and authorized at
  org-admin authority audited to the ledger; members SHALL be authenticated; and a PUBLIC tier SHALL exist
  ONLY for distillate that is provably zero-estate-specific — a fresh node federates nothing until an org
  admin deliberately enables it.

- **REQ-2112** — [R] federation-stance 4.4.
  Consumption SHALL NOT be gated behind contribution; a member that shares little or nothing SHALL NOT be
  throttled or penalized on what it consumes; and there SHALL be no contribution-to-consumption ratio, so a
  sensitive-estate operator is never pressured to over-share.

- **REQ-2113** — [O] INV-22 · [R] federation-stance 7.
  A node SHALL be born groundnet-COMPATIBLE such that the spec/020 export adapter (REQ-2020) targets THIS
  envelope and seam, WHILE the local decision tracer (persist, read, inspect) SHALL NOT depend on this
  contract and ONLY the export adapter SHALL; groundnet build SHALL remain blocked until the flywheel
  graduates an artifact, the shareable artifacts are loadable-not-hardcoded, and the decision-tracer archive
  exists (docs/FEDERATION-VISION.md § 7).

- **REQ-2114** — [O] INV-14.
  A shared chunk SHALL be treated as UNRECALLABLE once emitted, the export decision SHALL be the last point
  of control, and every emitted chunk SHALL declare its retention and provenance as a governed record
  (INV-14), so the org admin is told at opt-in that there is no delete after export.

- **REQ-2115** — [O] INV-19 · [O] INV-08.
  A consumer SHALL reject a replayed or duplicate chunk by its `id` and `provenance_chain` anchor, so a
  re-emitted chunk cannot inflate a pseudonym's reputation or re-trigger ingest — one chunk earns local
  standing at most once per node.

## Persistence / interface-contract

This is a Draft CONTRACT: the node persists no new groundnet state and wires no seam until the § 7
prerequisites land and a build task graduates this spec. The contract fixes the SHAPES a future
implementation SHALL honor.

**The typed seam.** The node exposes `Emit(ctx, chunk) (Receipt, error)` and `Ingest(ctx, chunk)
(IngestOutcome, error)`. `Emit` accepts ONLY a chunk assembled from the spec/020 REQ-2017 generalizable
projection — the estate-specific layer is not in its input type (REQ-2101, REQ-2108). `Ingest` returns an
`IngestOutcome` that records the chunk entering the local re-graduation path as a subordinate hint; it
returns no authority and gates nothing (REQ-2109, REQ-2110). The `Chunk` is the envelope of REQ-2100 with a
version-tagged opaque `payload` (REQ-2102).

**The durable shape a future implementation writes.** One immutable `groundnet_emit` row per `Emit` (the
chunk `id`, `payload_version`, the producer pseudonym public key, the `signature`, the transparency-log
inclusion proof, and the declared retention — never a secret, never an estate identifier), and one immutable
`groundnet_ingest` row per `Ingest` (the chunk `id`, the producer pseudonym, the signature-verify result,
and the re-graduation disposition). Each row is a required output of its function, is appended to the
tamper-evident governance ledger (INV-19) and anchored into the federated transparency log (REQ-2105,
REQ-2106), and carries only non-secret, de-identified fields — the AWX / SSH / member credentials remain
`core/config.SecretRef` references held elsewhere (INV-13). See
[`docs/DATA-MODEL.md`](../../docs/DATA-MODEL.md).

## Standing-check invariant

A standing check SHALL FAIL if `Emit` reads the estate-specific layer or a `payload` carries an estate
identifier (REQ-2101); if an ingested chunk reaches an actuator without traversing the interceptor, the
never-auto floor, and the mode chokepoint (REQ-2109); if a chunk influences an action before it re-graduates
locally (REQ-2110); if a chunk is ingested without a verified signature (REQ-2104) or a duplicate chunk earns
local standing twice (REQ-2115); if reputation is computed by contribution volume or minted as an on-chain
vote or token (REQ-2107); if membership, export, or consumption defaults to ON or bypasses org-admin
authority (REQ-2111); if consumption is gated behind contribution (REQ-2112); if the local decision tracer
(persist, read, inspect) is made to depend on this contract (REQ-2113); or if any emitted or ingested row
carries a plaintext secret rather than a `SecretRef` reference (INV-13). The subordinate-not-authority
property (REQ-2109) SHALL hold under every ingested chunk, and membership, export, and consumption
(REQ-2111) SHALL remain default-off until an org admin enables them.
</invoke>
