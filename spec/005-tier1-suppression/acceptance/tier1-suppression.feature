# spec/005 — Tier-1 known-transient & scheduled-reboot suppression acceptance oracles.
# Every scenario is tagged: the core/suppression.Chain and its registries (Phase 2) are not
# built yet, so the runner skips these (Tags: "~@pending") and they are tracked as declared debt in
# acceptance/_test_mapping.json. No step definitions exist until the owning task binds them to real code.
Feature: The tier-1 suppression chain fails open and never darkens a critical or unknown alert

  The suppression chain is a deterministic pre-model filter: dedup, blast-radius fold,
  scheduled-reboot phase SR, host-agnostic known-pattern, active-memory. Every phase fails open to
  escalation, and a critical or unknown alert is never suppressed. Suppression rules live only in a
  temporally-bounded, live-verified, org-global registry.

  @REQ-405
  Scenario: A phase exception fails open to escalation
    Given a suppression phase raises an error while evaluating an alert
    When the suppression chain decides the alert
    Then the outcome is escalate

  @REQ-405
  Scenario: An unconfirmed suppression match fails open to escalation
    Given a candidate suppression match that is not confirmed against the live registry
    When the suppression chain decides the alert
    Then the outcome is escalate

  @REQ-407
  Scenario: A critical-severity reboot is never suppressed
    Given a reboot-class alert with severity "critical" on a host with a live matching schedule
    When the suppression chain decides the alert
    Then the outcome is escalate

  @REQ-407
  Scenario: An unknown-severity alert is never suppressed
    Given an alert whose severity is not a recognised severity value
    When the suppression chain decides the alert
    Then the outcome is escalate

  @REQ-408
  Scenario: A future-dated triage entry is rejected and the alert escalates
    Given a prior triage-log entry timestamped after the current time
    When the dedup stage evaluates the entry
    Then the entry is rejected at the envelope boundary
    And the outcome is escalate

  @REQ-408
  Scenario: A negative-age triage entry is rejected and the alert escalates
    Given a prior triage-log entry whose age relative to now is negative
    When the dedup stage evaluates the entry
    Then the entry is rejected at the envelope boundary
    And the outcome is escalate

  @REQ-408
  Scenario: A prior entry within the window is accepted for dedup evaluation
    Given a prior triage-log entry timestamped inside the window before now
    When the dedup stage evaluates the entry
    Then the entry is accepted as a dedup candidate

  @REQ-402
  Scenario: A currently-valid suppression-policy record activates the fold
    Given a suppression-policy record whose valid_from and valid_until bracket now and whose last_verified_at is fresh
    And a child alert within the record's declared host and rule scope
    When the suppression chain decides the alert
    Then the blast-radius fold is activated

  @REQ-403
  Scenario: An active fold posts the child alert as a notice without a session
    Given an active blast-radius fold for a matched child alert
    When the suppression chain decides the alert
    Then the alert is posted as a notice
    And no remediation session is spawned

  @REQ-402
  Scenario: An expired suppression-policy record fails open
    Given a suppression-policy record whose valid_until is before now
    And a child alert within the record's declared scope
    When the suppression chain decides the alert
    Then the outcome is escalate

  @REQ-402
  Scenario: A stale-verified suppression-policy record fails open
    Given a suppression-policy record whose last_verified_at is past its freshness bound
    And a child alert within the record's declared scope
    When the suppression chain decides the alert
    Then the outcome is escalate

  @REQ-404
  Scenario: An on-schedule reboot with a live registered schedule is suppressed
    Given a live un-killed un-expired schedule whose DST-correct window contains the alert time
    And a reboot-class alert on that host
    When the suppression chain decides the alert
    Then the outcome is suppressed in phase SR

  @REQ-404
  Scenario: An observing schedule does not suppress
    Given a schedule in the observing state whose window contains the alert time
    And a reboot-class alert on that host
    When the suppression chain decides the alert
    Then the outcome is escalate

  @REQ-404
  Scenario: A reboot outside the schedule window escalates
    Given a live schedule whose window does not contain the alert time
    And a reboot-class alert on that host
    When the suppression chain decides the alert
    Then the outcome is escalate

  @REQ-407
  Scenario: A critical reboot with a matching schedule is not suppressed
    Given a live schedule whose window contains the alert time
    And a reboot-class alert with severity "critical" on that host
    When the suppression chain decides the alert
    Then the outcome is escalate

  @REQ-401
  Scenario: A host-agnostic transient rule matches by alert rule
    Given a host-agnostic transient pattern keyed on the alert rule within the estate
    And an alert carrying that alert rule on a host with no host-specific row
    When the suppression chain decides the alert
    Then the outcome is suppressed as a known transient pattern

  @REQ-401
  Scenario: A transient rule does not match a different alert rule
    Given a host-agnostic transient pattern keyed on one alert rule
    And an alert carrying a different alert rule
    When the suppression chain decides the alert
    Then the outcome is escalate

  @REQ-406
  Scenario: A dirty boot after a suppressed reboot reopens and pages
    Given a suppressed scheduled reboot whose recorded boot reason is not a clean systemd-reboot
    When the two-phase verifier runs
    Then the incident is reopened
    And the approver graph is paged

  @REQ-406
  Scenario: A clean systemd-reboot stays suppressed
    Given a suppressed scheduled reboot whose recorded boot reason is a clean systemd-reboot
    When the two-phase verifier runs
    Then the suppression is confirmed and the incident stays closed

  @REQ-404
  Scenario: A schedule promotes to live after two in-window boots
    Given an observing schedule with two recorded boots inside its window
    When the promotion writer runs
    Then the schedule status becomes live

  @REQ-404
  Scenario: A single in-window boot does not promote the schedule
    Given an observing schedule with one recorded boot inside its window
    When the promotion writer runs
    Then the schedule status stays observing

  @REQ-404
  Scenario: A removed schedule drifts to disabled
    Given a live schedule whose cron is no longer present on the host
    When the promotion writer runs
    Then the schedule status becomes disabled
