# spec/007 — Content-aware spec↔code lockstep acceptance oracles.
# The gate lives in tools/specvalidate (REQ-701/702/704) and core/governance (REQ-703, the re-stamp
# authority). REQ-703's two scenarios are BOUND: they drive core/governance.AuthorizeRestamp against a
# real RBAC authority + audit ledger. REQ-701/702/704 remain until the tools/specvalidate hash
# mechanism is exposed as an importable surface. Scenarios tagged are skipped by the runner
# (Tags: "~") and tracked as declared debt in acceptance/_test_mapping.json.
Feature: Content-aware spec-to-code lockstep keeps governed files bound to their owning spec

  The lockstep manifest binds each governed safety-critical file to its owning EARS spec by a
  comment-insensitive content hash. Governed code cannot drift from its spec without failing CI, no
  governed file may be excluded from the hash-verified set, and a re-stamp is an authorized, audited act.

  @REQ-701
  Scenario: A governed safety-critical file is recorded with a content hash in the lockstep manifest
    Given a governed safety-critical file bound to its owning spec
    When the lockstep manifest is stamped
    Then the manifest records a content hash for the file bound to that owning spec

  @REQ-702
  Scenario: A governed file changed without its owning spec fails the lockstep check
    Given a governed file whose stamped hash is recorded in the manifest
    When the file's executable content changes but its owning spec does not
    Then the lockstep check reports spec drift and exits with a failing status

  @REQ-702
  Scenario: The lockstep manifest excludes no governed safety-critical file
    Given the set of governed safety-critical files for the classifier, prediction gate, verifier, suppression chain, actuation interceptor, ledger, and schema
    When the coverage invariant runs against the lockstep manifest
    Then every governed safety-critical file is present in the manifest bound to an existing spec

  @REQ-704
  Scenario: A cosmetic comment edit alone does not register as spec drift
    Given a governed Go file whose stamped hash is recorded in the manifest
    When only comments and formatting change while the executable tokens are unchanged
    Then the recomputed comment-insensitive hash equals the stamped hash and no drift is reported

  @REQ-704
  Scenario: A semantic code change registers as spec drift
    Given a governed Go file whose stamped hash is recorded in the manifest
    When an executable token in the file changes
    Then the recomputed comment-insensitive hash differs from the stamped hash and drift is reported

  @REQ-703
  Scenario: A re-stamp is accepted only via an authorized audited approval
    Given a manifest re-stamp raised inside an RBAC-gated approval by a spec-owner role that updates the owning spec
    When the re-stamp approval is applied
    Then the recorded content hashes are accepted and an immutable re-stamp record is appended to the governance ledger

  @REQ-703
  Scenario: An unauthorized re-stamp attempt is rejected
    Given a manifest edited outside the RBAC-gated approval flow with no ledger record
    When the lockstep check runs in continuous integration
    Then the re-stamp is rejected as spec drift and no ledger record exists for it
