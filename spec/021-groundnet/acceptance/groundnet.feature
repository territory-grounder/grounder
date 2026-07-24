# spec/021 — The groundnet contract acceptance oracles.
# Nothing is built yet: this is a far-future Draft CONTRACT (blocked on the flywheel graduating an artifact,
# the loadable-not-hardcoded prose migration, and the decision-tracer archive — docs/FEDERATION-VISION.md
# § 7). Every scenario specifies the born-compatible envelope + adapter seam + invariants a node carries and
# is tagged @pending, tracked as declared debt in acceptance/_test_mapping.json and skipped by the runner
# (Tags: "~@pending") until the owning task lands. The seam authorizes nothing and actuates nothing; an
# ingested chunk is a subordinate hint that re-graduates locally before it earns trust and never lifts the
# never-auto floor (INV-09), the interceptor/mutation keystone (INV-21), or the mode chokepoint.
Feature: A node is born compatible with the groundnet envelope, adapter seam, and subordinate-not-authority invariants

  The groundnet is the far-future federation of sovereign TG instances sharing re-validated remediation
  distillate. This contract is the stable compatibility surface: a versioned signed wisdom-chunk envelope
  around an evolvable payload, a typed Emit/Ingest seam sourced only from the spec/020 generalizable layer,
  pseudonymous attestation, a signed transparency-log provenance model (not a blockchain), verified-outcome
  reputation, opt-in default-off authenticated membership, and a subordinate-not-authority ingest path that
  re-graduates every foreign chunk locally before it earns standing. The full network mechanism lives in
  docs/FEDERATION-VISION.md.

  @REQ-2100 @pending
  Scenario: The wisdom-chunk envelope is a versioned signed unit with the stable field set
    Given a wisdom chunk carrying id payload_version two_layer_marker producer_attestation signature provenance_chain verified_outcome_evidence and payload
    When a node parses and validates the envelope
    Then the envelope fields and their invariants are the stable contract and the node validates the envelope independently of the payload it carries

  @REQ-2101 @pending
  Scenario: The two-layer marker keeps every chunk generalizable and the estate-specific layer has no export path
    Given a wisdom chunk assembled for sharing
    When the two_layer_marker and the payload are inspected
    Then the marker is generalizable the payload carries no estate identifier and the estate-specific layer has no export path in the contract

  @REQ-2102 @pending
  Scenario: The payload is versioned and evolvable while the envelope stays stable
    Given a consumer that understands a set of payload versions and a chunk carrying a newer payload_version
    When the consumer reads the chunk
    Then the envelope stays byte-stable across payload versions and the consumer rejects the unknown payload_version without rejecting the envelope

  @REQ-2103 @pending
  Scenario: The producer attestation is a stable pseudonym and reputation accrues to it not an identity
    Given a chunk whose producer_attestation is a stable pseudonymous keypair
    When reputation is attributed for the chunk
    Then the attestation carries no real-world or estate identity and reputation accrues to the pseudonym rather than to any estate

  @REQ-2104 @pending
  Scenario: A chunk whose signature does not verify is refused before ingest
    Given a chunk whose signature over the envelope binds the payload to the producer pseudonym and a second chunk whose signature is tampered
    When a consumer verifies each chunk before ingest
    Then the verifying chunk proceeds and the tampered chunk is refused before it reaches the local re-graduation path

  @REQ-2105 @pending
  Scenario: Provenance is anchored in a signed append-only transparency log and not a blockchain
    Given a chunk provenance_chain anchored in a multi-witness transparency log
    When tamper-evidence and censorship-resistance are established
    Then they derive from the log inclusion proofs on the Sigstore Rekor and certificate-transparency model and not from a blockchain global consensus

  @REQ-2106 @pending
  Scenario: The groundnet log extends the local hash-chained governance ledger
    Given the local hash-chained governance_ledger from migration 0015
    When a chunk provenance_chain is anchored
    Then the groundnet transparency log is the federated multi-witness extension of the local ledger and the chunk references the producing node local ledger anchor

  @REQ-2107 @pending
  Scenario: Reputation aggregates signed verified-outcome attestations weighted by quality not volume
    Given signed pseudonymous verified-outcome attestations from multiple nodes
    When reputation is aggregated
    Then reputation is a CRDT-style rollup weighted by verified-outcome quality is never an on-chain vote or token and is never weighted by contribution volume

  @REQ-2108 @pending
  Scenario: The adapter seam emits from the generalizable layer and ingests into local re-graduation
    Given a node implementing the typed Emit and Ingest adapter seam
    When Emit assembles a chunk and Ingest lands a foreign chunk
    Then Emit sources its chunk only from the spec/020 generalizable layer and Ingest lands the chunk into the local re-graduation path and neither side reads the estate-specific layer

  @REQ-2109 @pending
  Scenario: An ingested chunk is a subordinate hint that passes the full local gate stack
    Given a foreign chunk ingested as a hint
    When an action the hint influences reaches the actuation path
    Then the chunk re-runs local eval the graduation ladder and the policy gate and never bypasses the interceptor the never-auto floor or the mode chokepoint and the local constitution remains sovereign

  @REQ-2110 @pending
  Scenario: An ingested chunk re-graduates locally before it earns any local standing
    Given a foreign chunk that graduated on its producing node
    When the chunk enters the consuming node
    Then the chunk does not inherit the producer trust and re-graduates against local traffic and local verified outcomes before it earns any local standing and trust is re-earned per node never transferred

  @REQ-2111 @pending
  Scenario: Membership export and consumption are opt-in default-off and authenticated
    Given a fresh node with no groundnet configuration
    When membership export and consumption are considered
    Then each is opt-in default-off and authorized at org-admin authority members are authenticated and a public tier exists only for provably zero-estate-specific distillate

  @REQ-2112 @pending
  Scenario: Consumption is never gated behind contribution and no over-share is required
    Given a member that shares little or nothing
    When the member consumes from the groundnet
    Then consumption is not gated behind contribution the member is not throttled or penalized and there is no contribution-to-consumption ratio

  @REQ-2113 @pending
  Scenario: A node is born groundnet-compatible while the local tracer does not depend on the contract
    Given a node carrying the local decision tracer and the dormant groundnet seam
    When the local tracer persists reads and inspects and the export adapter targets the contract
    Then the local tracer does not depend on this contract only the export adapter does and groundnet build remains blocked until the flywheel graduates an artifact the artifacts are loadable and the tracer archive exists

  @REQ-2114 @pending
  Scenario: A shared chunk is treated as unrecallable and the export decision is the last point of control
    Given a chunk about to be emitted
    When the org admin makes the export decision
    Then the chunk is treated as unrecallable once emitted the export decision is the last point of control and the chunk declares its retention and provenance as a governed record

  @REQ-2115 @pending
  Scenario: A replayed or duplicate chunk is rejected by its id and provenance anchor
    Given a chunk already ingested and later re-emitted with the same id and provenance_chain anchor
    When the consumer receives the re-emitted chunk
    Then the consumer rejects the replay so it cannot inflate the pseudonym reputation or re-trigger ingest and one chunk earns local standing at most once per node
