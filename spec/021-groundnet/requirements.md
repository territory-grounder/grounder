<!-- spec/021 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/021 — The groundnet contract (federation envelope + adapter seam + invariants)

**Owning behavior family:** — (no narrative `BEH-N`; the north-star narrative is
[`docs/FEDERATION-VISION.md`](../../docs/FEDERATION-VISION.md). This spec is the STABLE CONTRACT half of
that vision — the born-compatible envelope + adapter seam + invariants a node carries — not the full network
mechanism.)
**Constitution / invariants:** INV-01, INV-08, INV-09, INV-10, INV-13, INV-14, INV-19, INV-21, INV-22.
**Phase:** far-future / P-network, now under active implementation. **Un-deferred 2026-08-28 by owner directive
(TG-128): "deliver groundnet; solve yourself all blockers."** The three §7 prerequisites (a real flywheel
graduation, the loadable-not-hardcoded prose migration, and the spec/020 decision-tracer archive + its
generalizable projection) are being DELIVERED as part of this work rather than awaited as external
preconditions — see [`docs/FEDERATION-VISION.md`](../../docs/FEDERATION-VISION.md) § 7.
**Status:** Draft (un-deferred 2026-08-28; was **DEFERRED**, formally recorded 2026-07-27 P7-7, and re-deferred
to a post-cutover "v2 conversation" 2026-08-25 — both reversed by the owner directive above). The
born-compatible contract is built task by task, DORMANT.

*What ships, and how.* The contract is implemented as inert, gated code that authorizes nothing and actuates
nothing: the SCITT statement layer, the transparency Receipt, the pseudonym, the reputation rollup, and the
typed Emit/Ingest seam land dormant, with the seam opt-in and default-off (REQ-2111). This does NOT alter the
§4.14 single-ReAct-loop topology: groundnet federates sovereign ESTATES (separate instances), not agents
within one instance, and no live federation runs until an org admin turns the seam on. While un-wired the
package is outside every binary's reachability graph, so `deadcode-gate` does not analyze it; the Phase-C
wiring brings it into the graph.

*Rigour is unchanged.* The requirements below remain law for anything that implements them, and
`specvalidate ratify --check` holds this spec to the same bar as every other — every requirement here has a
route to an oracle or a declared gap, and spec/021 passes that check today. Acceptance scenarios bind to real
code as each owning task lands; until a scenario is bound it stays `@pending`, tracked as declared debt in
`acceptance/_test_mapping.json`.

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

> **DECISION (owner): the envelope is the canonical groundnet SCITT profile (RFC 9943); a VERSIONED,
> evolvable PAYLOAD rides inside it.** The wire envelope is NOT defined here — it is the SCITT Signed
> Statement + Receipt (a **Transparent Statement**) specified by the canonical groundnet protocol
> (`products/ground-net/spec`, groundnet.net), which is AUTHORITATIVE; this spec conforms to it and adds only
> TG's implementation bindings. A groundnet wisdom unit is a `COSE_Sign1` Signed Statement whose protected
> header carries a pseudonymous Issuer (`iss`), the content-addressed subject (`sub`), issuance time (`iat`),
> the key id (`kid`), and the payload media type (`content_type`), augmented with a SCITT Transparency
> Service Receipt. The PAYLOAD (what a graduated artifact carries, `application/vnd.groundnet.wisdom+json`)
> is a VERSIONED, evolvable body that grows as the flywheel learns what a graduated artifact IS — a stable
> IETF-standard envelope wrapping an evolvable body; the envelope is the compatibility surface, the payload
> is the growth surface.
>
> *(Amendment, owner-directed 2026-08-28: reconciled from the earlier custom 8-field `gn/0` envelope to the
> IETF SCITT profile after an audit found TG's envelope had diverged from the published canonical wire
> contract. The canonical spec is now the single source of truth for the envelope; TG conforms.)*

> **DECISION (owner): PSEUDONYMOUS attestation, NOT identity.** The SCITT Issuer (`iss` bound by `kid`, no
> `x5t` / `x5chain`) is a stable PSEUDONYM (a keypair), never a real-world or estate identity. Reputation accrues to the pseudonym;
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
  The contract SHALL adopt, as its wire envelope, the canonical groundnet SCITT profile (RFC 9943): a wisdom
  unit is a SCITT **Transparent Statement** — a `COSE_Sign1` Signed Statement whose protected header carries
  the pseudonymous Issuer (`iss`), content-addressed subject (`sub`), issuance time (`iat`), key id (`kid`),
  and payload media type (`content_type`), augmented with a Transparency Service Receipt — and a node SHALL be
  born able to parse and validate this SCITT envelope independently of the payload it carries. The envelope is
  defined by the canonical spec (`products/ground-net/spec`), which is authoritative; this spec SHALL NOT
  redefine it.

- **REQ-2101** — [O] INV-13 · [R] federation-stance 4.1.
  Every shareable statement SHALL be GENERALIZABLE-only (de-identified): the ESTATE-SPECIFIC layer (hosts,
  IPs, topology, credential identities, raw traces) SHALL have NO export path in the contract, and the
  `payload` SHALL carry no estate identifier — the generalizable projection of the spec/020 REQ-2017 schema
  is the only content a statement may carry, so "share" is structurally incapable of reading estate-specific
  data. De-identification is an INVARIANT enforced by the `Emit` input TYPE, not an envelope field — the
  earlier `two_layer_marker` field is subsumed by the SCITT profile's de-identified payload.

- **REQ-2102** — [R] federation-stance 4.1.
  The `payload` SHALL be VERSIONED by its media type (`content_type`, e.g.
  `application/vnd.groundnet.wisdom+json`) and the SCITT envelope SHALL remain stable across payload versions;
  a consumer SHALL reject a payload media type it does not understand WITHOUT rejecting the envelope, so the
  standard envelope holds while the payload evolves as the flywheel learns what a graduated artifact contains.

- **REQ-2103** — [O] INV-13 · [R] federation-stance 4.5.
  The SCITT Issuer SHALL be a stable PSEUDONYM: the `iss` claim SHALL be a `gnpub:` value bound by `kid`, and
  `x5t` / `x5chain` (which root in a real-world identity) SHALL NOT be used. No envelope or payload field SHALL
  carry a real-world or estate identity, and reputation SHALL accrue to the pseudonym rather than to any
  estate — the de-identified payload plus the pseudonymous Issuer close the reconnaissance-feed leak a real
  producer identity would open.

- **REQ-2104** — [O] INV-13 · [O] INV-08.
  Every statement SHALL be a `COSE_Sign1` signed by the producer pseudonym's key (binding the payload to the
  Issuer), and a consumer SHALL verify the COSE signature (RFC 9052) BEFORE ingest and SHALL refuse any
  statement whose signature does not verify, so an unsigned or tampered statement never reaches the local
  re-graduation path.

- **REQ-2105** — [O] INV-19 · [R] federation-stance 4.5.
  Provenance SHALL be a SCITT **Transparency Service** (RFC 9943): the producer registers the Signed Statement
  and receives a **Receipt** (a signed inclusion proof over the append-only VDS), forming a Transparent
  Statement; a consumer SHALL verify the Receipt before granting reputation weight. This SHALL NOT be a
  blockchain — tamper-evidence and censorship-resistance derive from the log's inclusion proofs (the
  Certificate-Transparency / Sigstore-Rekor model SCITT standardises), not global consensus, because the
  groundnet is subordinate-not-authority and has no single global truth to agree.

- **REQ-2106** — [O] INV-19.
  The groundnet Transparency Service SHALL extend the local hash-chained `governance_ledger` (migration 0015):
  a groundnet TS MAY anchor its VDS in the producing node's local ledger, so the "blockchain of one" that
  already runs on each estate is the root the federated, multi-witness transparency chains from.

- **REQ-2107** — [R] federation-stance 4.4.
  Reputation SHALL be the federated aggregation (a CRDT-style rollup) of signed pseudonymous
  VERIFIED-OUTCOME attestations — the `confirmation` statement type of the canonical profile, each saying the
  fix verified clean when ANOTHER node applied it — weighted by verified-outcome quality, SHALL NOT be an
  on-chain vote or token, SHALL NOT be a producer-asserted count, and SHALL NOT be weighted by contribution
  volume.

- **REQ-2108** — [R] federation-stance 4.1 · [O] INV-22.
  The node SHALL implement a typed adapter SEAM exposing `Emit(chunk)` and `Ingest(chunk)`, WHERE `Emit`
  sources its chunk ONLY from the spec/020 REQ-2017 generalizable layer and `Ingest` lands a chunk into the
  local re-graduation path (REQ-2110) — the seam is the sole crossing point, and neither side reads the
  estate-specific layer.

- **REQ-2109** — [O] INV-08 · [O] INV-09 · [O] INV-21.
  An ingested statement SHALL enter as a HINT subject to the full local gate stack: it SHALL re-run local
  eval, the graduation ladder, and the policy gate, and SHALL NOT bypass the actuation interceptor, the
  constitutional never-auto floor, or the mode chokepoint; the local constitution SHALL remain sovereign
  regardless of the statement's provenance or reputation.

- **REQ-2110** — [F] flywheel · [O] INV-10.
  An ingested statement SHALL NOT inherit the producer's trust; it SHALL RE-GRADUATE against local traffic and
  local verified outcomes before it earns any local standing, and trust SHALL be re-earned per node and never
  transferred across the boundary. Re-graduation SHALL follow graduated influence levels (resolving the
  apparent circularity that a statement permitted to influence nothing could never be tested and so never
  graduate): a foreign statement MAY inform investigation ordering and MAY be cited as evidence for a local
  hypothesis, but SHALL NOT be an executable instruction and SHALL NOT satisfy a local mutation-authorization;
  a LOCAL planner SHALL mint a fresh estate-specific action that independently clears the full local gate
  stack (REQ-2109); and ONLY the consumer's own mechanical verification SHALL graduate the imported statement
  into local knowledge — federated graduation is the reason a poisoned statement cannot propagate as
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
  A consumer SHALL reject a replayed or duplicate statement by its subject (`sub`) and its Transparency
  Service Receipt (the VDS position), so a re-emitted statement cannot inflate a pseudonym's reputation or
  re-trigger ingest — one statement earns local standing at most once per node.

## Persistence / interface-contract

This is a Draft CONTRACT: the node persists no new groundnet state and wires no seam until the § 7
prerequisites land and a build task graduates this spec. The contract fixes the SHAPES a future
implementation SHALL honor.

**The typed seam.** The node exposes `Emit(ctx, stmt) (Receipt, error)` and `Ingest(ctx, stmt)
(IngestOutcome, error)`. `Emit` accepts ONLY a statement assembled from the spec/020 REQ-2017 generalizable
projection — the estate-specific layer is not in its input type (REQ-2101, REQ-2108). `Ingest` returns an
`IngestOutcome` that records the statement entering the local re-graduation path as a subordinate hint; it
returns no authority and gates nothing (REQ-2109, REQ-2110). The statement is a SCITT Transparent Statement
(REQ-2100) carrying a media-type-versioned opaque `payload` (REQ-2102).

**The durable shape a future implementation writes.** One immutable `groundnet_emit` row per `Emit` (the
statement subject `sub`, the payload media type, the producer pseudonym `iss` / `kid`, the Transparency
Service Receipt, and the declared retention — never a secret, never an estate identifier), and one immutable
`groundnet_ingest` row per `Ingest` (the statement `sub`, the producer pseudonym, the COSE-signature-verify
result, and the re-graduation disposition). Each row is a required output of its function, is appended to the
tamper-evident governance ledger (INV-19) and anchored into the SCITT Transparency Service (REQ-2105,
REQ-2106), and carries only non-secret, de-identified fields — the AWX / SSH / member credentials remain
`core/config.SecretRef` references held elsewhere (INV-13). See
[`docs/DATA-MODEL.md`](../../docs/DATA-MODEL.md).

## Standing-check invariant

A standing check SHALL FAIL if `Emit` reads the estate-specific layer or a `payload` carries an estate
identifier (REQ-2101); if an ingested statement reaches an actuator without traversing the interceptor, the
never-auto floor, and the mode chokepoint (REQ-2109); if a statement authorizes an action before it
re-graduates locally (REQ-2110); if a statement is ingested without a verified COSE signature (REQ-2104) or a
duplicate statement earns local standing twice (REQ-2115); if reputation is computed by contribution volume,
from a producer-asserted count, or minted as an on-chain vote or token (REQ-2107); if membership, export, or
consumption defaults to ON or bypasses org-admin authority (REQ-2111); if consumption is gated behind
contribution (REQ-2112); if the local decision tracer (persist, read, inspect) is made to depend on this
contract (REQ-2113); or if any emitted or ingested row carries a plaintext secret rather than a `SecretRef`
reference (INV-13). The subordinate-not-authority property (REQ-2109) SHALL hold under every ingested
statement, and membership, export, and consumption (REQ-2111) SHALL remain default-off until an org admin
enables them.
</invoke>
