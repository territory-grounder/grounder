<!-- spec/021 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/021 — Design: The groundnet contract (federation envelope + adapter seam + invariants)

How the requirements in `requirements.md` are realized on the Go / PostgreSQL / Sigstore stack WHEN the § 7
prerequisites (docs/FEDERATION-VISION.md) are met and this spec graduates from Draft to a build. Where this
design and a future implementation disagree, the code is the bug and this document is the intent. This spec
owns the STABLE CONTRACT — the envelope, the seam, the invariants — that a node is born compatible with; the
full network mechanism (transport, discovery, the coordinator, the reputation service) stays in
[`docs/FEDERATION-VISION.md`](../../docs/FEDERATION-VISION.md) and is out of scope here.

## What is a contract and what is a mechanism

| Concern | Where it lives | Status |
|---|---|---|
| The wisdom-chunk ENVELOPE + its invariants | **spec/021 (here)** | the stable contract a node is born with |
| The typed `Emit` / `Ingest` adapter SEAM | **spec/021 (here)** | the sole crossing point |
| The subordinate-not-authority ingest invariant | **spec/021 (here)** | re-graduation before trust |
| The GENERALIZABLE trace layer the chunk carries | spec/020 REQ-2017 | already authored (local) |
| The optional export lane that feeds `Emit` | spec/020 REQ-2020 | already authored (off by default) |
| Transport, member discovery, the coordinator | docs/FEDERATION-VISION.md | far-future mechanism |
| The live reputation SERVICE | docs/FEDERATION-VISION.md | far-future mechanism |

The line is deliberate: a node that carries the envelope + seam + invariants can join a groundnet built
later WITHOUT being re-cut. The contract is the compatibility surface; everything a running network needs on
top of it is mechanism.

## The wisdom-chunk envelope (REQ-2100, REQ-2101, REQ-2102)

```
Chunk {
  id                        // content-addressed chunk identity (stable, dedup + replay key, REQ-2115)
  payload_version           // names the payload schema; the consumer version-gates on this (REQ-2102)
  two_layer_marker          // = GENERALIZABLE, always; the estate-specific layer has no export path (REQ-2101)
  producer_attestation      // signature-bearing pseudonym public key (a keypair, NOT an identity, REQ-2103)
  signature                 // over the whole envelope, binds payload -> producer pseudonym (REQ-2104)
  provenance_chain          // transparency-log inclusion proof + local governance_ledger anchor (REQ-2105/2106)
  verified_outcome_evidence // the local verified-clean evidence that graduated this artifact (REQ-2107 input)
  payload                   // VERSION-TAGGED, opaque, evolvable body (REQ-2102)
}
```

The envelope fields and their invariants are FROZEN — this is the compatibility surface a node is born
with. The `payload` is the growth surface: it is an opaque, `payload_version`-tagged body. This is the HTTP
posture (a stable envelope wrapping an evolvable body): you can add a `payload_version` the day the flywheel
first graduates an artifact, and every already-shipped node parses the envelope, reads the version, and — if
it does not understand that version — rejects the PAYLOAD while still validating the ENVELOPE (REQ-2102). You
cannot freeze a payload the flywheel has never produced (zero graduated artifacts to date, docs/FEDERATION-
VISION.md § 7.3), which is precisely why the contract versions the payload instead of specifying its bytes.

**Why GENERALIZABLE-only (REQ-2101).** The `payload` is assembled ONLY from the spec/020 REQ-2017
generalizable projection — the abstracted tuple *(alert-rule-class → diagnosis → resolution op-class →
VERIFIED outcome)* plus graduated skills / runbooks / rubrics. It names KINDS, never INSTANCES: "a BGP
session-flap class on an edge-router role, resolved by the restart-service op-class, verified clean," never
"restart bgpd on dc1edge03." The estate-specific layer is not in the `Emit` input type at all, so there
is no anonymization to get wrong — the trace that IS a map of the estate simply has no export path.

## The adapter seam (REQ-2108)

```go
// Emit takes a chunk assembled ONLY from the spec/020 generalizable projection and publishes it.
// The estate-specific layer is not in the input type — Emit cannot read it (REQ-2101/2108).
Emit(ctx context.Context, chunk Chunk) (Receipt, error)

// Ingest lands a foreign chunk into the LOCAL re-graduation path as a subordinate hint.
// It returns no authority and gates nothing; the chunk earns standing only by re-graduating (REQ-2109/2110).
Ingest(ctx context.Context, chunk Chunk) (IngestOutcome, error)
```

`Emit` is fed by the spec/020 REQ-2020 export lane (off by default). Its input is the generalizable layer —
by TYPE, not by runtime filter — so the estate-specific cleavage of REQ-2017 is enforced at the seam's
signature, not by a scrubber that could be misconfigured. `Ingest` is the anti-poisoning frontier: it
verifies the signature (REQ-2104), rejects replays by `id` + provenance anchor (REQ-2115), and then hands the
chunk to local re-graduation. Neither side of the seam reaches an actuator, lifts a floor, or changes the
mutation posture; the seam is observe-and-propose only (INV-22).

### Composition with spec/020 (REQ-2017 / REQ-2020)

The decision tracer's two-layer split is the LOAD-BEARING dependency. spec/020 REQ-2017 makes the
generalizable layer a first-class schema projection so "share" can only read the shareable projection;
spec/020 REQ-2020 is the optional, off-by-default export lane. `Emit` is the groundnet-facing consumer of
that lane, and the ordering invariant (REQ-2113) is strict: the LOCAL tracer (persist, read, inspect) has
NO dependency on this contract — it stands alone — and ONLY the export adapter depends on the groundnet
envelope. A node ships the tracer and its archive with the groundnet seam DORMANT; nothing about the local
inspector needs the network to exist.

## The transparency log — not a blockchain (REQ-2105, REQ-2106)

Provenance, tamper-evidence, and censorship-resistance come from a signed, append-only, multi-witness
TRANSPARENCY LOG on the Sigstore / Rekor + Certificate-Transparency model (reusing Sigstore / in-toto per the
prior art), NOT a blockchain.

- **Why not a blockchain.** A blockchain exists to reach global consensus on a single shared truth. The
  groundnet has NO single truth to agree on: every node re-validates locally and is
  subordinate-not-authority (REQ-2109), so what is "true" is per-estate and re-earned, never global. Paying
  for global consensus buys nothing the groundnet uses, while adding metadata leakage (a public ordered
  ledger of who-shared-when is itself reconnaissance), latency / finality cost, and token / smart-contract
  attack surface to a security-critical control plane.
- **What the log gives instead.** Inclusion proofs (a chunk's `provenance_chain` proves it was logged and
  has not been altered), multi-witness gossip (no single witness can equivocate or silently drop entries —
  the CT split-view defense), and append-only tamper-evidence — WITHOUT a consensus protocol or a coin.
- **The local root.** TG's hash-chained `governance_ledger` (migration 0015) is already a per-estate
  append-only "blockchain of one." The groundnet log is its FEDERATED extension: a chunk's provenance chains
  from the producing node's local ledger anchor (REQ-2106) up into the multi-witness federated log, so the
  same tamper-evidence discipline runs end to end from the local decision to the shared chunk.

## Pseudonym + reputation (REQ-2103, REQ-2104, REQ-2107)

- **Pseudonym, not identity (REQ-2103).** The `producer_attestation` is a stable pseudonymous keypair.
  Reputation accrues to the pseudonym; no estate identity leaves. This is the fix to the reconnaissance-feed
  leak: a real producer identity would relabel who-had-which-incident even over a de-identified payload.
  Tradeoff, owner-acknowledged and layered on the SAME envelope: a stable pseudonym gives reputation
  CONTINUITY at the cost of per-chunk linkability; unlinkable-per-chunk signing (ring / group signatures)
  gives maximum privacy at the cost of continuity — default is the stable pseudonym.
- **Signature verify (REQ-2104).** The `signature` binds the payload to the pseudonym over the whole
  envelope; a consumer verifies BEFORE ingest and refuses a chunk that does not verify. Signature verify +
  replay rejection (REQ-2115) are the two guards `Ingest` runs before a chunk touches re-graduation.
- **Reputation rollup (REQ-2107).** Reputation is a federated aggregation — a CRDT-style, commutative,
  idempotent rollup — of signed pseudonymous VERIFIED-OUTCOME attestations: each attestation says "pseudonym
  P's chunk, when re-graduated on my estate, produced a verified-clean outcome." Influence is weighted by
  that outcome quality, never by how many chunks a pseudonym dumped, and it is never an on-chain vote or a
  token. Because it is a CRDT rollup over signed attestations, it converges without a consensus round and
  without a coordinator holding authority.

## The ingest gate stack — subordinate-not-authority (REQ-2109, REQ-2110)

An ingested chunk is a HINT. The order a future implementation runs, so a foreign chunk can never become
authority:

1. **Verify + de-replay (REQ-2104, REQ-2115).** Reject an unsigned / tampered chunk and a replay by `id` +
   provenance anchor BEFORE anything else.
2. **Land as retrieval context / candidate artifact.** The chunk becomes additional RAG context, a candidate
   skill, or a suggested op-class — with NO privileged path.
3. **Re-graduate locally (REQ-2110).** The candidate re-enters the consuming estate's OWN graduation ladder
   (spec/015) and must earn trust against LOCAL traffic and LOCAL verified outcomes. Trust is re-earned per
   node, never transferred.
4. **Every action still runs the full local gate stack (REQ-2109).** Any action the hint influences passes,
   unchanged, the never-auto floor (INV-09), the policy verdict (spec/015), the credential resolution
   (spec/016), the actuation interceptor and mutation keystone (spec/013 / INV-21), the mode chokepoint
   (`core/safety`), and local verify (INV-10). The local constitution is sovereign.

A maliciously crafted, perfectly-reputed chunk can therefore do at most what any local proposal can: propose.
A poisoned chunk that cannot produce local verified-clean outcomes never re-graduates and never gains
reputation — the attack fails closed on every consuming estate independently.

## Membership + posture (REQ-2111, REQ-2112, REQ-2114)

- **Opt-in, default-off, org-admin (REQ-2111).** Joining, exporting, and consuming are each an explicit,
  authenticated, org-admin-authority action audited to the ledger — never a default, never a per-user
  toggle. A fresh node federates nothing.
- **Authenticated members + public tier (REQ-2111).** The default groundnet is a closed set of
  authenticated members; a public tier exists ONLY for distillate that is provably zero-estate-specific, and
  even then a public chunk enters a consumer as a subordinate hint under re-graduation like any other.
- **No over-share (REQ-2112).** Consumption is decoupled from contribution; a share-little member is never
  throttled; there is no ratio to game. The safe choice — consume-only, or share only clean distillate — is
  never punished.
- **Un-deletability (REQ-2114).** A shared chunk is unrecallable; the export decision is the last point of
  control, and each chunk declares its retention + provenance as a governed record (INV-14). The org admin
  is told this at opt-in.

## Persistence

Draft: no groundnet state is persisted and no seam is wired until this spec graduates. The durable shape a
future implementation writes is in `requirements.md` § Persistence / interface-contract — one immutable,
ledger-anchored, transparency-log-anchored, non-secret, de-identified `groundnet_emit` row per `Emit` and
`groundnet_ingest` row per `Ingest`, on tables the runtime role cannot UPDATE or DELETE (INV-19), with every
credential a `SecretRef` reference (INV-13).

## Out of scope

The transport, member discovery, the coordinator, and the live reputation service are the far-future
MECHANISM in [`docs/FEDERATION-VISION.md`](../../docs/FEDERATION-VISION.md), not this contract. The
generalizable trace layer and the optional export lane are spec/020 (REQ-2017 / REQ-2020). The graduation
ladder is spec/015; the actuation interceptor + mutation keystone + mode chokepoint are spec/013 +
`core/safety`; the mechanical verdict is spec/002; the tamper-evident ledger is spec/006 + migration 0015.
This spec owns the wisdom-chunk envelope, the typed adapter seam, the transparency-log provenance model, the
pseudonym + reputation model, the versioned-payload approach, and the subordinate-not-authority ingest
invariant — the born-compatible contract, nothing more.
