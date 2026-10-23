# spec/021 — The groundnet contract acceptance oracles.
# This is a far-future Draft CONTRACT: its implementation (core/groundnet envelope + adapter seam + the
# core/trace generalizable projection) is BUILT but DORMANT — the seam is opt-in default-off and reaches no
# actuator, and the contract stays Draft (not Ratified) pending the docs/FEDERATION-VISION.md § 7 blockers
# (the flywheel graduating an artifact, the loadable-not-hardcoded prose migration, and the decision-tracer
# archive). Every scenario now binds to that real dormant code and executes strictly — acceptance/_test_mapping.json
# records each scenario's oracle, and none is @pending (a future scenario would be tagged @pending and skipped
# by the runner's "~@pending" filter until its binding lands). The seam authorizes nothing and actuates nothing;
# an ingested chunk is a subordinate hint that re-graduates locally before it earns trust and never lifts the
# never-auto floor (INV-09), the interceptor/mutation keystone (INV-21), or the mode chokepoint.
Feature: A node is born compatible with the groundnet envelope, adapter seam, and subordinate-not-authority invariants

  The groundnet is the far-future federation of sovereign TG instances sharing re-validated remediation
  distillate. This contract is the stable compatibility surface: a versioned signed wisdom-chunk envelope
  around an evolvable payload, a typed Emit/Ingest seam sourced only from the spec/020 generalizable layer,
  pseudonymous attestation, a signed transparency-log provenance model (not a blockchain), verified-outcome
  reputation, opt-in default-off authenticated membership, and a subordinate-not-authority ingest path that
  re-graduates every foreign chunk locally before it earns standing. The full network mechanism lives in
  docs/FEDERATION-VISION.md.

  @REQ-2100
  Scenario: The wisdom-chunk envelope is a versioned signed unit with the stable field set
    Given a wisdom unit that is a SCITT Transparent Statement carrying the protected-header claims iss sub iat kid and content_type a payload and a Transparency Service Receipt
    When a node parses and validates the SCITT envelope
    Then the envelope is the canonical groundnet SCITT profile and the node validates it independently of the payload it carries

  @REQ-2101
  Scenario: The two-layer marker keeps every chunk generalizable and the estate-specific layer has no export path
    Given a wisdom statement assembled for sharing
    When the payload and the Emit input type are inspected
    Then the payload is generalizable-only and carries no estate identifier and the estate-specific layer has no export path in the contract

  @REQ-2102
  Scenario: The payload is versioned and evolvable while the envelope stays stable
    Given a consumer that understands a set of payload media types and a statement carrying a newer content_type
    When the consumer reads the statement
    Then the SCITT envelope stays stable across payload versions and the consumer rejects the unknown payload media type without rejecting the envelope

  @REQ-2103
  Scenario: The producer attestation is a stable pseudonym and reputation accrues to it not an identity
    Given a statement whose SCITT Issuer is a stable pseudonym the iss bound by kid with no x5t or x5chain
    When reputation is attributed for the statement
    Then the Issuer carries no real-world or estate identity and reputation accrues to the pseudonym rather than to any estate

  @REQ-2104
  Scenario: A chunk whose signature does not verify is refused before ingest
    Given a COSE_Sign1 statement whose signature binds the payload to the producer pseudonym and a second statement whose signature is tampered
    When a consumer verifies each statement before ingest
    Then the verifying statement proceeds and the tampered statement is refused before it reaches the local re-graduation path

  @REQ-2105
  Scenario: Provenance is anchored in a signed append-only transparency log and not a blockchain
    Given a statement Receipt from a SCITT Transparency Service over a multi-witness append-only VDS
    When tamper-evidence and censorship-resistance are established
    Then they derive from the Receipt inclusion proofs on the Sigstore Rekor and certificate-transparency model SCITT standardises and not from a blockchain global consensus

  @REQ-2106
  Scenario: The groundnet log extends the local hash-chained governance ledger
    Given the local hash-chained governance_ledger from migration 0015
    When a statement Receipt is anchored
    Then the groundnet Transparency Service is the federated multi-witness extension of the local ledger and the statement anchors in the producing node local ledger

  @REQ-2107
  Scenario: Reputation aggregates signed verified-outcome attestations weighted by quality not volume
    Given signed pseudonymous verified-outcome attestations from multiple nodes
    When reputation is aggregated
    Then reputation is a CRDT-style rollup weighted by verified-outcome quality is never an on-chain vote or token and is never weighted by contribution volume

  @REQ-2108
  Scenario: The adapter seam emits from the generalizable layer and ingests into local re-graduation
    Given a node implementing the typed Emit and Ingest adapter seam
    When Emit assembles a chunk and Ingest lands a foreign chunk
    Then Emit sources its chunk only from the spec/020 generalizable layer and Ingest lands the chunk into the local re-graduation path and neither side reads the estate-specific layer

  @REQ-2109
  Scenario: An ingested chunk is a subordinate hint that passes the full local gate stack
    Given a foreign chunk ingested as a hint
    When an action the hint influences reaches the actuation path
    Then the chunk re-runs local eval the graduation ladder and the policy gate and never bypasses the interceptor the never-auto floor or the mode chokepoint and the local constitution remains sovereign

  @REQ-2110
  Scenario: An ingested chunk re-graduates locally before it earns any local standing
    Given a foreign statement that graduated on its producing node
    When the statement enters the consuming node
    Then the statement may inform investigation as evidence but does not inherit the producer trust and re-graduates against local traffic and local verified outcomes before it earns any local standing and only local mechanical verification grants it authority

  @REQ-2111
  Scenario: Membership export and consumption are opt-in default-off and authenticated
    Given a fresh node with no groundnet configuration
    When membership export and consumption are considered
    Then each is opt-in default-off and authorized at org-admin authority members are authenticated and a public tier exists only for provably zero-estate-specific distillate

  @REQ-2112
  Scenario: Consumption is never gated behind contribution and no over-share is required
    Given a member that shares little or nothing
    When the member consumes from the groundnet
    Then consumption is not gated behind contribution the member is not throttled or penalized and there is no contribution-to-consumption ratio

  @REQ-2113
  Scenario: A node is born groundnet-compatible while the local tracer does not depend on the contract
    Given a node carrying the local decision tracer and the dormant groundnet seam
    When the local tracer persists reads and inspects and the export adapter targets the contract
    Then the local tracer does not depend on this contract only the export adapter does and groundnet build remains blocked until the flywheel graduates an artifact the artifacts are loadable and the tracer archive exists

  @REQ-2114
  Scenario: A shared chunk is treated as unrecallable and the export decision is the last point of control
    Given a chunk about to be emitted
    When the org admin makes the export decision
    Then the chunk is treated as unrecallable once emitted the export decision is the last point of control and the chunk declares its retention and provenance as a governed record

  @REQ-2115
  Scenario: A replayed or duplicate chunk is rejected by its id and provenance anchor
    Given a statement already ingested and later re-emitted with the same sub and Transparency Service Receipt
    When the consumer receives the re-emitted statement
    Then the consumer rejects the replay so it cannot inflate the pseudonym reputation or re-trigger ingest and one statement earns local standing at most once per node
