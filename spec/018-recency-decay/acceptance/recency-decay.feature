# spec/018 — Shared recency, decay & periodic reconciliation of the learned stores.
# Every scenario drives the REAL code (core/lessons, core/learn, core/estate, core/falsify + eval) — there is
# no @pending here. This is competence-plane maintenance: it ages LEARNED state only; it never touches the
# estate, actuates, or gates, and mutation stays OFF.
Feature: The three learned stores forget on a shared recency/decay discipline

  Learned knowledge that stops matching reality must lose influence: a stale lesson decays and is pruned, an
  ancient co-occurrence count halves on a half-life, and a learned estate edge a fresh observation contradicts
  loses confidence and ages out — while ground truth is never touched. The discovery corpus is drained to a
  durable file so captured deviations survive the rolling cap.

  @REQ-1800 @REQ-1801
  Scenario: A lesson carries provenance and an aged lesson is down-weighted and pruned
    Given a resolved-incident feed whose lessons carry a resolved_at provenance timestamp
    When the lessons are reconciled against a retention horizon
    Then the provenance timestamp round-trips and a lesson one half-life old is down-weighted
    And a lesson older than the horizon is pruned from the corpus while a fresh one is kept

  @REQ-1802
  Scenario: A learned co-occurrence count halves over a half-life and a faded pair ages out
    Given a co-occurrence pair observed enough times to promote to a learned edge
    When the half-life decay is applied for one half-life with no fresh evidence
    Then the count halves and the learned edge still survives
    And after several half-lives with no reinforcement the pair ages out of the tier

  @REQ-1803
  Scenario: An estate learned edge a fresh observation contradicts decays and ages out
    Given an estate graph with a learned edge and a ground-truth edge
    When a fresh verify disproof names the learned edge's host
    Then the learned edge confidence decays and the ground-truth edge is untouched
    And repeating the disproof at a floor ages the learned edge out of the blast radius

  @REQ-1804
  Scenario: Decay ages learned state only and never touches ground truth or the estate
    Given an estate graph with a learned edge and a ground-truth edge
    When a disproof names every host in the graph
    Then only the learned tier decays and the ground-truth edge keeps its confidence
    And the receiver graph is never mutated in place

  @REQ-1805
  Scenario: The discovery-corpus flush drains captured deviations to the durable corpus file
    Given an in-memory discovery corpus that captured a scored deviation
    When the flush drains the snapshot into the durable corpus via IngestCaptured and saves it
    Then reloading the durable corpus file yields the captured deviation case
