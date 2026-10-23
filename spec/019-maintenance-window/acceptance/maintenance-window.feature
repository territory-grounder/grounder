# spec/019 — Scheduling awareness & maintenance-window seam acceptance oracles.
# Every scenario drives the REAL modules/schedule/cronicle connector against an in-process fake Cronicle
# serving the exact REST wire shapes verified live (get_schedule/get_event, X-API-Key auth, code-0 envelope,
# timing arrays), so the window derivation and the fail-closed-safe default are proven end-to-end in CI.

Feature: TG derives maintenance windows and scheduled jobs from the estate scheduler and defers actuation outside a sanctioned window

  A read-only connector reads the estate scheduler read-only and derives sanctioned maintenance windows,
  change-freeze windows (which override maintenance), and already-scheduled jobs. The maintenance-window seam
  fails closed-safe: an unreadable schedule is treated as OUTSIDE any window, so TG never assumes it is safe to
  actuate. The API key is a sealed SecretRef; the connector actuates nothing.

  @REQ-1901 @REQ-1902
  Scenario: A maintenance window and scheduled jobs are derived from real scheduler events
    Given a scheduler serving a nightly maintenance-window event and recurring jobs
    When the connector reads and derives the schedule
    Then a sanctioned maintenance window and the already-scheduled jobs are derived and the target is in-window during the window

  @REQ-1904
  Scenario: A change-freeze overrides an overlapping maintenance window
    Given a scheduler whose change-freeze overlaps a maintenance window
    When the seam is evaluated inside the overlapping freeze
    Then the target is reported not in-window because the freeze denies over the maintenance window

  @REQ-1905 @REQ-1906
  Scenario: Outside a sanctioned window the actuation is deferred to POLL_PAUSE
    Given a scheduler serving a nightly maintenance-window event and recurring jobs
    When the seam is evaluated outside every window
    Then the target is reported not in-window and the actuation is clamped to the POLL_PAUSE band

  @REQ-1903
  Scenario: An unreadable schedule fails closed to outside the window
    Given a scheduler that cannot be read
    When the seam is evaluated
    Then the target is reported not in-window with the conservative unreadable reason

  @REQ-1900
  Scenario: The connector re-reads each event by id and reflects an upstream change
    Given a scheduler serving a nightly maintenance-window event and recurring jobs
    When the operator retimes the maintenance event upstream and the connector re-reads
    Then the derived window reflects the retimed event with no stale cached window

  @REQ-1907
  Scenario: The API key is a sealed SecretRef and a wrong key fails closed
    Given a scheduler that requires an API key
    When the connector authenticates with a sealed SecretRef and then with a wrong key
    Then the sealed key reads the schedule and the wrong key fails closed with no schedule read

  @REQ-1908
  Scenario: The connector reads over a native HTTP client and actuates nothing
    Given a scheduler serving a nightly maintenance-window event and recurring jobs
    When the connector reads the schedule over its native HTTP client
    Then the read completes with no subprocess and the connector exposes no actuation path
