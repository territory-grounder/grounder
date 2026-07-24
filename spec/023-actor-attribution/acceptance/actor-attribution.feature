Feature: Actor-attribution grounding (spec/023)
  Before proposing or actuating, TG asks WHO is the actor behind the observed change. Attribution can
  only stand down, no-op, or escalate — never raise autonomy. Every scenario is @pending — this is the
  design gate; scenarios bind to real code as tasks T-023-* land.

  @REQ-2300 @pending
  Scenario: Every classification carries a typed attribution derived from reader evidence
    Given a session admitted for classification
    When the classifier input is built
    Then it carries one closed-enum ActorAttribution value
    And the value was derived by deterministic code from reader-captured evidence
    And the value and its evidence references appear in the classification signals

  @REQ-2301 @pending
  Scenario: An admin-attributed fault stands down to the approver graph and is never auto-undone
    Given a guest stopped by a sanctioned non-TG principal with no matching carve-out
    When the session is classified
    Then the band is POLL_PAUSE routed to the approver graph
    And no actuation reversing the change is proposed or executed
    And the attribution and evidence are recorded

  @REQ-2302 @pending
  Scenario: A TG-self-remediated incident terminates already-remediated without re-actuation
    Given TG's own actuation identity remediated the same target and fault class inside the window
    When the re-fired alert is triaged
    Then the session terminates with outcome already-remediated
    And no new actuation is proposed or executed
    And the session_triage row and governance Decision record carry the outcome and evidence

  @REQ-2303 @pending
  Scenario: Absent evidence yields unattributable and the pre-feature ladder unchanged
    Given no admissible actor evidence exists for the investigated subject
    When the session is classified
    Then the attribution is unattributable
    And the band equals the band the pre-feature classifier emits for the same input

  @REQ-2304 @pending
  Scenario: An unsanctioned-actor change polls with security escalation and is never auto-healed
    Given admissible evidence positively identifies an unsanctioned actor for the observed change
    When the session is classified
    Then the band is POLL_PAUSE with security_escalation in its signals
    And no auto-heal executes
    And the notification routes to the security channel in addition to the approver graph

  @REQ-2304 @pending
  Scenario: A covered audit trail missing an entry for an observed mutation escalates as suspicious
    Given a domain reader that affirmatively covers the target's audit trail
    And the trail has no entry corresponding to the observed mutation
    When the session is classified
    Then the band is POLL_PAUSE with security_escalation in its signals

  @REQ-2305 @pending
  Scenario: An authorized-test outcome traverses every existing gate unchanged
    Given a fault attributed authorized-test by a valid carve-out
    When the heal ladder runs
    Then the ACL, min-confidence, band, graduation, mode, and floor gates all evaluate unchanged
    And no gate is lifted or bypassed by the attribution

  @REQ-2306 @pending
  Scenario: Actor evidence is collected only by registered read-only readers with least-privilege credentials
    Given the set of loaded actor-evidence readers
    When their registrations and credentials are inspected
    Then every reader is compiled in, config-gated, and explicitly registered at startup
    And every reader credential is a read-only SecretRef distinct from any actuation credential

  @REQ-2306 @pending
  Scenario: The PVE task-log reader returns actor evidence for a guest lifecycle change
    Given a guest lifecycle change recorded in the PVE task log
    When the PVE reader reads the attribution window for that guest
    Then it returns evidence carrying the actor token, the action kind, the timestamp, and the UPID

  @REQ-2306 @pending
  Scenario: The AWX job-history reader returns actor evidence for a playbook-driven change
    Given an AWX job that ran a playbook against the target host within the window
    When the AWX reader reads the attribution window for that host
    Then it returns evidence carrying the job launcher as actor, the job template and status, the timestamp, and the job id

  @REQ-2306 @pending
  Scenario: The NetBox changelog reader returns actor evidence for an inventory change
    Given a NetBox changelog entry recording a change to the target device
    When the NetBox reader reads the attribution window for that device
    Then it returns evidence carrying the changing user as actor, the change action, the timestamp, and the change id

  @REQ-2306 @pending
  Scenario: The GitOps MR-history reader returns actor evidence for a declarative change
    Given a merged MR to the deploy branch that changed a deployment manifest within the window
    When the GitOps MR reader reads the attribution window
    Then it returns evidence carrying the MR merger as actor, the MR-merged action, the merge timestamp, and the merge commit sha

  @REQ-2306 @pending
  Scenario: The PVE reader authenticates with a read-only token distinct from the actuation token
    Given the configured PVE reader credential
    When it is compared with the actuation token
    Then they are distinct identities and the reader token grants no write path

  @REQ-2307 @pending
  Scenario: A reader error is treated as absent evidence with a recorded warning and never blocks triage
    Given a reader that errors or exceeds its bounded timeout
    When the session is triaged
    Then the reader's evidence is treated as absent
    And a warning is logged and recorded in the session signals
    And the triage proceeds without attributed-suspicious arising from the failure alone

  @REQ-2308 @pending
  Scenario: The taxonomy-to-disposition mapping is loadable data with a closed disposition enum
    Given the versioned ruleset store
    When the actor-attribution mapping is loaded
    Then each row maps one taxonomy value to one closed-enum disposition
    And a row with an unknown disposition is a load-time error

  @REQ-2308 @pending
  Scenario: An absent or corrupt mapping escalates every non-unattributable taxonomy to a human
    Given the actor-attribution mapping is absent or fails validation
    When a session with a non-unattributable attribution is classified
    Then the band is POLL_PAUSE routed to the approver graph
    And an unattributable attribution leaves the ladder unchanged

  @REQ-2309 @pending
  Scenario: A valid carve-out classifies a harness fault authorized-test, heals, and records the attribution
    Given a currently-valid carve-out matching the harness actor and an allowlisted pool host
    When a fault attributed to that actor on that host is triaged
    Then the attribution is authorized-test
    And the heal ladder proceeds unchanged
    And the attribution is recorded on the session_triage row

  @REQ-2309 @pending
  Scenario: An expired carve-out rule no longer matches and the attribution stands down
    Given a carve-out whose valid_until has passed
    When a fault attributed to the harness actor on the pool host is triaged
    Then the attribution is attributed-authorized
    And the session stands down to the approver graph

  @REQ-2310 @pending
  Scenario: Contradictory non-suspicious evidence escalates to the approver graph with all candidates recorded
    Given admissible evidence supporting two contradictory non-suspicious taxonomy values
    When the session is classified
    Then the band is POLL_PAUSE routed to the approver graph
    And every candidate value and its evidence are recorded

  @REQ-2311 @pending
  Scenario: The attribution outcome and evidence references persist on session_triage and the governance ledger
    Given a classified session with a non-empty attribution
    When the session terminates
    Then the session_triage row carries the taxonomy value, matched rule id, and evidence blob
    And the governance Decision record's signals carry the attribution outcome

  @REQ-2311 @pending
  Scenario: The decision tracer surfaces the attribution as a named trace step
    Given a traced session with an attribution outcome
    When the session trace is read
    Then it contains an attribute step carrying the taxonomy value and evidence references

  @REQ-2312 @pending
  Scenario: Agent free-text can never set or alter the taxonomy value
    Given an agent narrative asserting an attribution for the incident
    And no reader-captured evidence supporting that assertion
    When the attribution is derived
    Then the taxonomy value is unattributable
    And the narrative assertion appears nowhere in the derivation inputs

  @REQ-2313 @pending
  Scenario: Persisted evidence is retention-bounded and redacted at every boundary
    Given a persisted actor-evidence record
    When its storage row and every log, export, and model-bound rendering are inspected
    Then the record carries a retention bound
    And secret-shaped material is redacted
    And seed-rendered evidence is delimited as data and input-screened

  @REQ-2314 @pending
  Scenario: The journal reader returns typed actor evidence for a privileged host action
    Given a host whose journal records a sudo action within the attribution window
    And the reader is registered with an operator allowlist and a known-hosts file
    When the journal reader reads the host over the native read-only SSH runner
    Then it returns a typed Evidence record naming the host as the target
    And no raw log text is surfaced to the model

  @REQ-2314 @pending
  Scenario: The journal reader fails closed when no known-hosts file is configured
    Given a journal reader whose known-hosts file is unset
    When it attempts to read a host
    Then the read is refused before any connection
    And the domain's evidence is treated as absent

  @REQ-2315 @pending
  Scenario: An identity fact can never by itself set a taxonomy value
    Given an identity resolver reporting a principal is a disabled account
    And no admissible action-evidence record names that principal
    When the attribution is derived
    Then the taxonomy value is unattributable
    And the identity fact appears in no candidate taxonomy

  @REQ-2316 @pending
  Scenario: Identity enrichment leaves the deterministic core attributor unchanged
    Given an identity resolver and a per-session copy of the attributor configuration
    When enrichment runs in the attribute activity
    Then only the per-session configuration copy is modified
    And the shared attributor configuration and the core attributor are untouched

  @REQ-2317 @pending
  Scenario: A confirmed live admin is promoted to a stand-down
    Given an action-evidence record attributing a change to an unlisted principal
    And a fresh directory fact confirming that principal is an enabled non-service member of the sanctioned admin group
    When the attribution is derived
    Then the taxonomy value is attributed-authorized
    And the disposition stands down to the approver graph

  @REQ-2318 @pending
  Scenario: A disabled admin credential in use is demoted to a security escalation
    Given an action-evidence record attributing a change to a statically-sanctioned principal
    And a fresh directory fact reporting that principal is locked or disabled
    When the attribution is derived
    Then the taxonomy value is attributed-suspicious
    And the classifier emits a security escalation

  @REQ-2319 @pending
  Scenario: A directory outage reproduces the Phase-1 classification
    Given an identity resolver that errors or times out
    When the attribution is derived over the same evidence
    Then the classification equals the static sanctioned-list result
    And no promotion and no demotion are applied

  @REQ-2320 @pending
  Scenario: Identity data is retention-bounded and redacted at every boundary
    Given a persisted enrichment value carrying usernames and group memberships
    When its storage row and every log, export, and resolver warning are inspected
    Then the record carries a retention bound
    And resolver warnings carry only the provider and domain
    And the classification signal records only the resolved taxonomy

  @REQ-2321 @pending
  Scenario: A missing authentication event never moves a classification
    Given an identity seam carrying an auth-observed annotation
    And no RADIUS record for a change made over a key-based login
    When the attribution is derived
    Then the absent authentication event moves no classification
    And the annotation mints no taxonomy
