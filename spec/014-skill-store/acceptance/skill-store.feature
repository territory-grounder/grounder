# spec/014 — skill store + graduation flywheel acceptance oracles.
# The scenarios drive the real core/skillstore state machine, the store-backed composer, and the trial
# engine with in-memory fakes (CI has no Postgres); pgx implementations are integration-tested under
# compose. Every scenario is @pending until its implementation task lands (the honest frontier).
Feature: Skills are versioned, console-editable, and graduate to production only through evaluation

  A skill version moves draft -> trial -> production -> retired through one audited state machine;
  composition stays a pure function of typed signals with a total compiled fallback; candidates enter
  trials only through the offline gate; graduation is statistical; a regression auto-rolls back.

  @REQ-1301

  Scenario: A version row is created only with a rationale and a valid predicate
    Given a skill store
    When a draft is created without a rationale or with an unknown execution class in its predicate
    Then the write is refused and no row exists

  @REQ-1301

  Scenario: Every status transition goes through Transition and lands in the ledger
    Given a draft version
    When it is admitted, graduated, and retired
    Then each transition carries a rationale and appends a governance-ledger entry

  @REQ-1302

  Scenario: A second production version structurally supersedes the first
    Given a skill with a production version
    When another version graduates
    Then the prior production version is retired in the same transaction and exactly one production row remains

  @REQ-1305

  Scenario: A draft targeting a pinned skill is rejected
    Given the pinned conservative-remediation skill
    When a draft version targets it
    Then the write is refused

  @REQ-1303
  Scenario: Composition from the store is a pure function of typed signals
    Given a production snapshot with phase- and class-scoped versions
    When the same typed context composes twice
    Then the same bodies compose in the same order both times

  @REQ-1304
  Scenario: A store failure composes the compiled registry in full and records the fallback
    Given a store that fails to load
    When a seed is composed
    Then every compiled skill composes and the record names the fallback reason

  @REQ-1305
  Scenario: A pinned skill composes its compiled body even when a store row targets it
    Given a store row that rewrites a pinned skill
    When a seed is composed
    Then the pinned skill's compiled body is used

  @REQ-1303
  Scenario: The composed skill list is recorded so the seed is reconstructable
    Given an active production snapshot
    When a session composes its seed
    Then the record lists each loaded skill's name, version, content hash, and trial arm

  @REQ-1311
  Scenario: The library read surface exposes versions with rationale and scores
    Given a skill with a version history
    When the library is read with a read-only principal
    Then each version carries its rationale log, eval scores, and ledger references

  @REQ-1311
  Scenario: An unauthenticated skill read is refused
    Given the skill routes
    When a request arrives without a principal
    Then the response is a refusal, not data

  @REQ-1311
  Scenario: A machine principal has no write route
    Given a machine principal
    When it attempts to create a draft
    Then the surface refuses before any backend call

  @REQ-1301
  Scenario: An operator write without a rationale is refused
    Given an operator session
    When a promote is requested without a rationale
    Then the surface refuses before any backend call

  @REQ-1306
  Scenario: Arm assignment is deterministic, idempotent, and rejects malformed refs
    Given an active trial with two candidates
    When the same external ref is assigned twice and a whitespace ref is assigned once
    Then the arm is identical across the two assignments and the malformed ref is rejected and counted

  @REQ-1308
  Scenario: The finalizer sweeps timeouts before finalizing
    Given an expired active trial and a completable active trial
    When the finalizer runs
    Then the expired trial is aborted-timeout before any graduation is considered

  @REQ-1308
  Scenario: Graduation requires samples, lift, the Welch test, and a healthy safety dimension
    Given a trial whose best candidate lacks samples, lift, significance, or safety-dimension health
    When the finalizer runs
    Then no version graduates

  @REQ-1309
  Scenario: A trial that cannot complete at the observed session rate is refused
    Given a judged-session rate too low to reach the sample minimum before the end date
    When a trial start is requested
    Then the start is refused with a stored reason

  @REQ-1310
  Scenario: A tripped breaker auto-retires the graduate and restores the prior production version
    Given a freshly graduated version under its regression watch
    When judged sessions score below the control threshold enough times to trip the breaker
    Then the graduate is retired, the prior production version is restored, and the demotion is ledger-recorded and escalated

  @REQ-1312
  Scenario: A generated draft carries its rationale and has no effect until admitted
    Given the candidate generator triggered by a regressed dimension
    When it produces drafts
    Then each draft records its source and rationale and composition is unchanged

  @REQ-1307
  Scenario: A draft that regresses the regression set is not admitted to a trial
    Given a draft whose offline run regresses the regression set
    When admission is evaluated
    Then the draft stays out of trial and the refusal reason is stored

  @REQ-1307
  Scenario: The sealed holdout is not read during admission
    Given an offline admission run
    When it completes
    Then the sealed holdout set has not been accessed

  @REQ-1314
  Scenario: The creation cron generates for a regressed dimension, admits, and starts one trial
    Given a production skill whose judged dimension has regressed
    When the creation-half cron runs with a passing offline gate and ample traffic
    Then draft candidates are generated, admitted to trial, and one trial is started while production is unchanged

  @REQ-1314
  Scenario: The creation cron refuses to start a trial that traffic cannot complete
    Given a production skill whose judged dimension has regressed
    When the creation-half cron runs with a passing offline gate but starved traffic
    Then the candidates are admitted but no trial is started

  @REQ-1313 @pending
  Scenario: The trial dashboard exposes arm health and assignment staleness
    Given an active trial with assignments
    When the trial state is read
    Then per-arm samples, means, the test statistic, projected completion, and newest-assignment age are exposed
