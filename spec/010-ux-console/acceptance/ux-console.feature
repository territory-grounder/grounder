# spec/010 — Operator console acceptance oracles.
# Every scenario is: the frontend/ console (React/Vite) is not built yet, so there are no step
# definitions to compile. They are spec-ahead-of-code, tracked as `pending` in _test_mapping.json and
# skipped by the runner (Tags: "~") until the panels are implemented.
Feature: The operator console renders governed autonomy without holding safety authority

  The console is API-first (one generated OpenAPI client), read-only until mutation is earned,
  RBAC-gated for every operational control, real-time over SSE, and accessible by construction. It
  displays the server's decisions; it never makes one.

  @REQ-601
  Scenario: The console builds its API layer only from the generated OpenAPI client
    Given the frontend api layer
    Then every request type, response type, and endpoint URL comes from the generated OpenAPI client
    And no hand-written wire contract exists in frontend/

  @REQ-613
  Scenario: A dropped SSE stream shows a disconnected indicator and reconnects
    Given the console is subscribed to the Server-Sent Events stream
    When the stream disconnects
    Then the console shows a disconnected indicator
    And the console attempts to reconnect

  @REQ-614
  Scenario: Interactive console components conform to WAI-ARIA
    Given any panel of the console
    Then every interactive component is keyboard-operable and exposes WAI-ARIA roles, names, and states
    And a visible focus indicator and a WCAG 2.1 AA contrast ratio are maintained

  @REQ-615
  Scenario: The console never enforces a safety decision client-side
    Given a risk band, a verification verdict, and an authorization grant from the server
    When the console renders them
    Then the console does not enforce the band, verdict, never-auto floor, or grant in browser code
    And a control the API denies remains unavailable in the console

  @REQ-616
  Scenario: An API error renders the restrictive state
    Given a control-plane call returns an authorization error or a transport error
    When the affected panel resolves its state
    Then the panel renders no data and no mutating control
    And the panel does not fall back to an optimistic or cached-permissive view

  @REQ-602
  Scenario: In read-only mode the console exposes no mutating control
    Given the control-plane reports mutation_enabled is false
    When the console renders its panels
    Then investigation, timeline, and audit views render read-only
    And no approve, veto, band-change, kill-switch, or administrative write control is present

  @REQ-603
  Scenario: When mutation is enabled an authorized operator gains operational controls
    Given the control-plane reports mutation_enabled is true
    And the authenticated caller holds the RBAC role the API requires for a control
    When the console renders that control
    Then the control is enabled
    And for a caller without the required role the control stays disabled

  @REQ-604
  Scenario: The approval console shows the plan, prediction, and reversibility signals for a pending decision
    Given a pending POLL_PAUSE or AUTO_NOTICE decision in the approval feed
    When the console renders the decision card
    Then it displays the proposed plan with its two-or-more approaches
    And it displays the committed machine prediction and the reversibility and blast-radius signals

  @REQ-605
  Scenario: An approver action is gated by role and re-checked server-side
    Given an approver acts on a pending decision
    When the console issues approve, veto, or handoff through the generated client
    Then the API re-checks the caller's RBAC role and on-call assignment server-side
    And the console treats the server authorization result as final

  @REQ-605
  Scenario: A non-approver cannot act on a pending decision
    Given the authenticated caller lacks the required approver role
    When the console renders a pending decision
    Then no approve, veto, or handoff control is presented for that decision

  @REQ-606
  Scenario: The ActionManifest timeline renders one chain keyed by the action id
    Given a completed ActionManifest with predicted, approved, executed, and verified stages
    When the console renders the timeline
    Then the four stages render as one visual chain keyed by the single content-hashed action_id
    And a stage carrying a different action_id renders a mismatch state

  @REQ-607
  Scenario: Replaying a completed manifest reconstructs the chain without re-executing
    Given a completed ActionManifest
    When an operator replays it
    Then the chain is reconstructed from persisted governance records through the generated client
    And no re-execution of the action is triggered

  @REQ-608
  Scenario: The ledger view surfaces the server-computed chain verification status
    Given the governance ledger and the server-side LedgerVerifier result
    When the console renders the ledger view
    Then it displays the chain-verification status returned by the LedgerVerifier
    And it does not compute the chain verdict in the browser

  @REQ-608
  Scenario: A broken ledger chain is shown as tamper-detected
    Given the LedgerVerifier reports a broken chain
    When the console renders the ledger view
    Then a tamper-detected state is shown

  @REQ-609
  Scenario: The explainability panel shows the banding signals, execution class, and tool-result evidence
    Given a session with a risk banding, an execution class, and auto-resolve evidence
    When an operator opens the explainability view
    Then it shows the retrieval context, the risk band with its signals, the execution class, and the confidence trajectory
    And it shows the orchestrator-captured ToolResult evidence IDs that backed any auto-resolve

  @REQ-610
  Scenario: Autonomy-band and kill-switch controls call the policy API and are RBAC-gated
    Given an administrator changes an autonomy-band control or a per-layer kill-switch
    When the console applies the change
    Then the change is written through the policy API as RBAC-gated organization policy and audited on change
    And no host-local file is read or written to effect the control
    And each autonomy layer is independently disableable

  @REQ-611
  Scenario: A kill-switch reflects the disabled state only after the API confirms it
    Given an administrator activates a kill-switch
    When the API has not yet confirmed the write
    Then the console does not display the control as disabled
    And the console shows the disabled state only after the API confirms it

  @REQ-612
  Scenario: The admin panel manages users, roles, on-call, and modules
    Given the authenticated caller holds the administrator role
    When the console renders the admin panel
    Then it manages users, RBAC role assignments, the on-call rotation and escalation policy, and per-module enablement through the generated client
    And the admin panel is absent for a caller without the administrator role
