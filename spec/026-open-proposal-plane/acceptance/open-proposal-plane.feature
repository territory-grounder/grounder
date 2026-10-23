Feature: Open proposal plane — day-zero free-form proposals, never executable
  With an empty op-class catalog TG still triages fully and proposes remediations that render and
  ledger but can never execute. Each scenario names its RED mutation control in the design; a
  scenario whose control cannot go red is not an oracle.

  @REQ-2601
  Scenario: An action-warranted fault with an empty catalog yields a free-form proposal, not a stand-down
    Given a real-path session against an empty op-class catalog
    And observations confirm an action-warranted fault
    When the agent completes its triage
    Then the session terminates with outcome "proposed:shadow"
    And the triage row carries a free-form op_class naming the addressing action

  @REQ-2602
  Scenario: The one grammar accepts a free-form op_class and an optional undo sketch
    Given a proposal whose op_class matches no registry entry
    When the proposal is parsed
    Then parsing succeeds through the single proposal grammar
    And the undo sketch is available on the proposal record and absent from the manifest action

  @REQ-2603 @pending
  Scenario: An unregistered op_class diverts to shadow before notify, projection, and vote
    Given a parsed proposal whose op_class fails the exact-slug registry lookup
    When the runner workflow processes the proposal
    Then no notify activity is invoked and no pending decision row is projected
    And no vote wait is entered
    And the shadow branch records the triage row and appends the ledger entry

  @REQ-2603 @pending
  Scenario: A registered op_class does not divert
    Given a parsed proposal whose op_class resolves in the registry
    When the runner workflow processes the proposal
    Then the proposal enters the normal classify, gate, and poll lane

  @REQ-2604 @pending
  Scenario: A shadow proposal persists first-wins with its screened fields and shadow outcome
    Given a shadow-diverted proposal with rationale and undo sketch text
    When the triage row is recorded twice for the same session
    Then exactly one row exists with outcome "proposed:shadow" and screened field content

  @REQ-2605 @pending
  Scenario: Every shadow proposal appends exactly one withheld propose:open ledger row
    Given a shadow-diverted proposal
    When the shadow branch completes
    Then the org-global hash chain gains exactly one GovDecision with decision "propose:open" and withheld true

  @REQ-2606 @pending
  Scenario: Screening neutralizes hostile free text on the new fields
    Given a proposal whose rationale contains jailbreak-shaped and secret-shaped text
    When the proposal is persisted, ledgered, and rendered
    Then every surface carries the neutralized-and-flagged form

  @REQ-2606 @pending
  Scenario: An uncited free-form proposal never reaches a shadow record
    Given a free-form proposal that cites no evidence identifiers
    When the citation gate evaluates the proposal
    Then the agent receives the mechanical citation re-prompt and no shadow record exists

  @REQ-2607 @pending
  Scenario: The proposals read surface fails closed with a nil reader
    Given the proposals API handler with no database reader
    When GET /v1/proposals is requested
    Then the response is 503 and no fabricated rows are returned

  @REQ-2607 @pending
  Scenario: The proposals view renders live states honestly and offers no mutating control
    Given the served console with the proposals view
    When the view renders the undefined, empty, and populated live states
    Then each state renders honestly and no actuation control is present in any state

  @REQ-2608
  Scenario: A force-routed free-form action is refused at the empty-argv leaf and the refusal is ledgered
    Given a sealed action for an unregistered op_class routed to execution against the safety chain
    When the effect leaf receives the action
    Then execution is refused on empty argv and the refusal is ledgered

  @REQ-2609
  Scenario: Actor evidence never suppresses the proposal itself
    Given observations confirming an action-warranted fault
    And actor evidence showing an authored stop by a named actor
    When the agent completes its triage
    Then the proposal names the addressing op-class

  @REQ-2610 @pending
  Scenario: Actor evidence is captured as a structured field and screened
    Given a proposal emitted with authored-stop actor evidence
    When the proposal record is persisted
    Then the record carries the evidence class, the actor, and the source reference as structured data

  @REQ-2611 @pending
  Scenario: Authored-action evidence raises the band to a POLL_PAUSE floor and never lowers a computed band
    Given a proposal carrying authored-action actor evidence
    When the banding path composes the evidence floor with the computed band
    Then a computed AUTO band becomes POLL_PAUSE
    And a computed POLL_PAUSE band stays POLL_PAUSE
