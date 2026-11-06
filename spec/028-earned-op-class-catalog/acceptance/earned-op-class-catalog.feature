Feature: Earned op-class catalog — candidates, dossiers, and the widened ladder
  Autonomy is earned per op-class: recurring evidence makes a candidate, an operator authors the
  template into an empty form, and the class climbs the existing ladder one verified rung at a
  time. RED mutation controls are named per oracle in design.md.

  @REQ-2801 @pending
  Scenario: The same incident re-proposed five times counts once
    Given five shadow proposals for one candidate key sharing one external ref
    When occurrences are recorded
    Then the occurrence journal holds exactly one row for that key and ref

  @REQ-2811 @pending
  Scenario: Three distinct incidents across two hosts advance a key to candidate
    Given three occurrences with distinct external refs across two hosts within thirty days
    When the clustering cron runs
    Then the key advances to candidate and the transition is ledgered

  @REQ-2811 @pending
  Scenario: Threshold-minus-one stays observing
    Given two distinct refs, or three refs on one host inside seven days
    When the clustering cron runs
    Then the key stays observing

  @REQ-2812 @pending
  Scenario: The cron refuses its pass when occurrence intake is stale while sessions flow
    Given a newest occurrence older than forty-eight hours and nonzero session volume
    When the clustering cron runs
    Then the whole pass refuses loudly and mints nothing

  @REQ-2813 @pending
  Scenario: Ratify demands a rationale and every transition lands on the one chain
    Given a ratify-ready candidate
    When the operator ratifies with a rationale
    Then the overlay row exists and the opclass:ratify decision is on the org-global chain

  @REQ-2813 @pending
  Scenario: The ratify form renders empty beside screened model exhibits
    Given a candidate whose dossier contains a model-suggested command string
    When the ratify form renders
    Then every form input is empty and the model text renders only as a screened read-only exhibit

  @REQ-2814 @pending
  Scenario: Admission refuses a slotted argv0 an unknown family and an embedded-slug collision
    Given operator templates each violating one admission predicate
    When each template is submitted
    Then each is refused with a stored reason naming the violated predicate

  @REQ-2814 @pending
  Scenario: The laundering tripwire refuses a byte-matching template
    Given an operator template whose argv element byte-matches an occurrence's model text
    When the template is submitted
    Then the template is refused

  @REQ-2815 @pending
  Scenario: A tampered overlay row is dropped loudly at refresh
    Given a ratified overlay row edited out-of-band after ratification
    When the overlay snapshot refreshes
    Then the row is dropped, the class resolves to absence, and a page is raised

  @REQ-2805
  Scenario: An absent class seals to nothing executable
    Given an op-class present in no registry surface
    When an action for it is force-routed toward execution
    Then sealing yields no argv and the effect leaf refuses on empty argv

  @REQ-2806 @pending
  Scenario: A ratified class enters at approve and opens the not-graduated poll
    Given a freshly ratified class
    When an action for it is proposed
    Then the class sits at approve with last outcome ratified and the not-graduated poll opens

  @REQ-2807 @pending
  Scenario: Promotion requires the full threshold and persists durably or refuses
    Given a ratified class one run short of its promote threshold
    When one further terminus-confirmed verified-clean run credits
    Then the class promotes to auto notice, and a failed persist refuses the promotion entirely

  @REQ-2807 @pending
  Scenario: A replayed graduation activity cannot double-credit
    Given a credited run whose graduation activity replays
    When the replay executes
    Then the credit key blocks a second increment

  @REQ-2809 @pending
  Scenario: The per-rung band truth table holds on the real path
    Given classes at approve, auto notice, and embedded auto
    When one action per class flows through banding
    Then approve opens a poll, auto notice acts with a notice, and auto acts silently

  @REQ-2810 @pending
  Scenario: A deviation at auto notice drops the class to approve and resets streaks
    Given a class at auto notice
    When a verified deviation verdict lands
    Then the level is approve, both streaks are zero, and the demotion ledger reason carries the surprise breakdown

  @REQ-2810
  Scenario: A forecast-lane verdict never feeds graduation
    Given a forecast-lane prediction verdict for a ratified class
    When the verdict is processed
    Then no ladder transition occurs

  @REQ-2808 @pending
  Scenario: An overlay-only class caps at auto notice
    Given an overlay-only class with ten clean notice runs, zero vetoes, and zero recurrence
    When promotion is evaluated
    Then the class stays at auto notice, and after an embed-export test build it promotes

  @REQ-2810 @pending
  Scenario: A revoked class exits the composed registry within one refresh
    Given a ratified class and a revoke click
    When the overlay refreshes
    Then the class is absent from lookup and the preamble, and an in-flight sealed action is refused on empty argv

  @REQ-2811 @pending
  Scenario: A destructive candidate is ratifiable only at a never-auto tier and never climbs
    Given a candidate whose op is destructive
    When it is ratified at a never-auto tier (TierIrreversible or TierVendorCritical)
    Then promotion to auto notice is refused by the tier floor and the ceiling badge renders

  @REQ-2816 @pending
  Scenario: The dossier answers the five operator questions in order
    Given a ratify-ready candidate with occurrences, blast radius, and prediction history
    When the dossier renders
    Then the five sections render in order with the prediction matrix marked display-only

  @REQ-2817 @pending
  Scenario: The graduation console extends the existing ladder rather than adding a second one
    Given the served console
    When the policy ladder renders the widened vocabulary
    Then auto notice appears with its caption in the existing graduation view

  @REQ-2818 @pending
  Scenario: Overlay classes carry a declared opcover exemption
    Given a ratified overlay-only class
    When opcover coverage is evaluated
    Then the class carries a ledgered exemption naming the embed-export path
