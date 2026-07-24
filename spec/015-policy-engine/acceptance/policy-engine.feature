# spec/015 — Operator-managed policy engine acceptance oracles.
# Nothing is built yet: every scenario specifies behavior of the not-yet-built core/policy.Engine
# (Phase 2) and is tagged @pending, tracked as declared debt in acceptance/_test_mapping.json and
# skipped by the runner (Tags: "~@pending") until the owning task lands. The engine composes ABOVE the
# constitutional mechanical floor (spec/001, spec/013) and never lifts it.
Feature: The policy engine is one operator-managed graduated access-control layer over actuation

  Four global modes govern only the actuation branch; the Rego evaluator is fixed and deny-overrides;
  rules are data; the operator owns the paranoia dial (warn, don't block); and the constitutional
  never-auto floor stays in force beneath every policy.

  @REQ-1503
  Scenario: Operator rules enter the fixed Rego module as data only
    Given the fixed audited Rego evaluator module and an operator rule list
    When the engine evaluates an action
    Then the operator rules were supplied as input data and no operator-authored Rego was compiled

  @REQ-1504
  Scenario: A matching deny wins regardless of rule order
    Given a rule list where a deny rule and a permissive auto rule both match an action
    When the engine evaluates the action under any permutation of the rule order
    Then the resolved verdict is "deny"

  @REQ-1506
  Scenario: A rule resolves an action to one of auto approve or deny
    Given an action matched by a single rule
    When the engine evaluates the action
    Then the verdict is one of "auto" "approve" or "deny"

  @REQ-1507
  Scenario: An unset rule param inherits from the global-default rule
    Given a rule whose min_confidence is unset and a global-default rule that sets it
    When the engine resolves the rule params
    Then the rule uses the global-default min_confidence

  @REQ-1507
  Scenario: A below-min-confidence action is clamped to approve
    Given an auto-verdict rule and an action whose confidence is below the resolved min_confidence
    When the engine evaluates the action
    Then the verdict is clamped to "approve"

  @REQ-1508
  Scenario: An over-rate-limit action is clamped to approve
    Given a rule with a rate_limit of N per minute that has already auto-executed N times this minute
    When the engine evaluates the next matching action
    Then the verdict is clamped to "approve"

  @REQ-1500
  Scenario: Exactly one of the four modes is active
    Given the engine in any of the four global modes
    When the active mode is queried
    Then exactly one of "Shadow" "HITL" "Semi-auto" or "Full-auto" is active

  @REQ-1502
  Scenario: A mode transition is gated and audited to the ledger
    Given an authenticated operator with mode-change authority
    When the operator changes the active mode
    Then the transition is gated and a mode-transition record is appended to the governance ledger before the new mode takes effect

  @REQ-1502
  Scenario: The wired RBAC admits a flip-authorized operator to escalate with a green preflight
    Given the production mode-transition authority admits the operator "operator:kyriakosp"
    When that operator transitions the mode from Shadow to Full-auto with a green preflight
    Then the mode is Full-auto and the mode-transition record is appended to the governance ledger

  @REQ-1502
  Scenario: The wired RBAC denies and audits an unauthorized operator's flip
    Given the production mode-transition authority does not admit the operator "operator:mallory"
    When that operator attempts to transition the mode from Shadow to Full-auto
    Then the transition is refused and the mode stays Shadow and a mode-transition-refused record is appended to the governance ledger

  @REQ-1502
  Scenario: A flip is refused when the boot preflight is not green
    Given the production mode-transition authority admits the operator "operator:kyriakosp" but the boot preflight is not green
    When that operator attempts to escalate the mode into Full-auto
    Then the escalation is refused and the mode stays Shadow

  @REQ-1519
  Scenario: An absent persisted mode fails closed to Shadow
    Given the persisted mode is absent or unreadable
    When the engine resolves the active mode
    Then the active mode is "Shadow"

  @REQ-1520
  Scenario: The mode is the sole actuation chokepoint that absorbs the retired gate
    Given no TG_MUTATION_ENABLED knob and no standalone toggle and no separate MutationGate object
    When the mode answers whether an action may actuate
    Then actuation is permitted only while the mode is Semi-auto or Full-auto and a breaker trip or halt forces the mode to Shadow

  @REQ-1521
  Scenario: Absorbing the mutation gate is a deliberate audited safety-core refactor
    Given the mutation-gate obligations of INV-09 and INV-21
    When the gate is absorbed into the mode chokepoint
    Then the constitution breaker halt preflight spec/013 and lockstep are re-expressed in mode terms and every fail-closed property the gate guaranteed is preserved

  @REQ-1501
  Scenario: The investigation pipeline is identical across all four modes
    Given the same alert evaluated in each of the four modes
    When the pipeline runs ingest reason rationale propose and risk-classify
    Then those stages take the same code path in every mode and only the actuation branch differs

  @REQ-1509
  Scenario: Respect band_mode emits the more-restrictive of verdict and risk band
    Given a rule with band_mode respect whose verdict is auto and an action whose risk band is POLL_PAUSE
    When the engine composes the band
    Then the emitted decision is the more-restrictive POLL_PAUSE and not auto

  @REQ-1510
  Scenario: Force band_mode overrides the risk band and double-warns
    Given a rule with band_mode force whose verdict is auto and an action whose risk band is AUTO_NOTICE
    When the engine composes the band
    Then the policy verdict is applied and a double-confirmation warning is recorded

  @REQ-1511
  Scenario: A floor-class action is still proposed with rationale in every mode
    Given a floor-class irreversible action in any mode
    When the pipeline reasons about the action
    Then the action is suggested with its rationale and the deny takes effect only at auto-execute

  @REQ-1512
  Scenario: The conservative template loads the predecessor deny-patterns and governor
    Given the conservative policy template
    When the operator loads it as rule data
    Then the loaded rules carry the predecessor argv deny-patterns and a 30-per-minute governor

  @REQ-1513
  Scenario: Removing the conservative template warns but is not blocked
    Given the conservative template is loaded
    When the operator removes it behind a double-confirmation
    Then the removal is permitted and the constitutional floor still clamps floor-class ops

  @REQ-1514
  Scenario: An op-class promotes to auto after N clean verified runs
    Given an op-class in graduation state approve with N consecutive verified match runs and no deviation
    When the engine re-evaluates that op-class
    Then the op-class graduates to verdict "auto"

  @REQ-1514
  Scenario: An op-class demotes to approve on the first deviation
    Given an op-class graduated to auto
    When a verification returns a deviation verdict
    Then the op-class is demoted to verdict "approve"

  @REQ-1515 @pending
  Scenario: An auto verdict still runs predict execute verify breaker
    Given the engine resolves an action to auto
    When the action actuates
    Then the predict execute verify breaker sequence runs and an unverifiable post-state refuses

  @REQ-1516
  Scenario: A vote from an approve_by member is admitted
    Given a pending decision whose approve_by set includes the voting principal
    When the principal votes on /v1/vote
    Then the vote is admitted

  @REQ-1516
  Scenario: A vote from a non-member is rejected
    Given a pending decision whose approve_by set excludes the voting principal
    When the principal votes on /v1/vote
    Then the vote is rejected

  @REQ-1516 @pending
  Scenario: A federated principal resolves through the configured provider
    Given a configured LDAP or OIDC provider and a federated approve_by principal
    When the engine resolves the principal
    Then the principal resolves through the configured provider

  @REQ-1517
  Scenario: A single allow-all policy is permitted behind a red double-confirmation
    Given an operator applying an allow-all policy
    When the engine receives the change behind a red double-confirmation
    Then the engine warns and applies the policy and never refuses it

  @REQ-1518
  Scenario: Every policy decision is appended to the governance ledger
    Given the engine evaluates an action to any verdict
    When the decision is produced
    Then one policy_decision record with the matched rule verdict band action_id and actor is appended to the tamper-evident ledger

  @REQ-1519
  Scenario: Forcing the engine on in Shadow warns and disabling it in Semi-auto double-warns
    Given an administrator overriding the per-mode engine default
    When the engine is forced on in Shadow and later disabled in Semi-auto
    Then the force-on records a warning and the disable records a double-confirmation warning

  @REQ-1505 @pending
  Scenario: The console edits rules as data and renders the packet-tracer explanation
    Given the console policy surface and an operator rule list
    When the operator edits a rule and traces an action
    Then the rule is edited as data and the OPA decision explanation renders in the packet-tracer

  @REQ-1518
  Scenario: The policy_decision table is append-only with no runtime UPDATE or DELETE
    Given the policy_decision persistence migration
    When the runtime database role attempts to update or delete a policy_decision row
    Then the operation is denied by the grants

  @REQ-1522
  Scenario: Every decision records the bundle version and the full matched-rule list
    Given a rule bundle where an auto rule and a deny rule both match one host and a second auto rule matches another host
    When the engine resolves an allow decision on the other host and a deny decision on the shared host
    Then each decision records the loaded bundle version and the deny decision surfaces the full matched-rule list
