---
name: diverged-replica-triage
class: runbook
version: 0.1.0-distilled
source: distill:docs/runbooks/seaweedfs-cross-site-replication.md
description: Intermittent success through a load balancer means diverged replicas — read each replica, find the stalled checkpoint, search for siblings, treat backfill as separate work
---

## Goal
Diagnose replicated-state systems whose symptom is INTERMITTENT — succeeds, then fails, then succeeds
for the same request — which is the signature of replicas that have silently diverged behind a load
balancer. The founding incident: a cross-site sync stuck for 145 days, and a SECOND, independent copy
of the same defect found inside one site only because the per-replica check was run anyway. NOTE —
tool-gated content: TG has no distributed-storage or cluster read tools wired; knowledge-library
material per ADR-0012.

## Required evidence
- Per-replica state, read from EACH replica individually — never only through the balanced service
  endpoint. List the same object from every replica; differing answers ARE the diagnosis. The
  balancer's rotation is what converts divergence into "flaky".
- The replication component's own logs, looking for a RETRY LOOP signature: the same resume-point
  error repeating every few seconds (a persisted checkpoint referencing data the source has since
  garbage-collected is a permanent stall that logs forever and progresses never).
- The stall's start time, from the oldest repeated error — divergence age bounds how much data is
  affected.
- A discriminating check against OTHER causes (mesh/network path health, replica process health)
  before concluding stale-checkpoint: two failure modes can look superficially identical from the
  symptom.

## Decision rules
- Flapping success through a balancer means DIVERGED REPLICAS until proven otherwise: the request is
  fine, the replicas disagree. Go straight to per-replica reads.
- When one instance of a failure shape is found, SEARCH FOR SIBLINGS: the same pattern (persisted
  offset → source data expired → permanent retry) recurs on every surface that persists a resume
  point against a garbage-collected log. Check the other sync relationships in the same system
  before closing.
- Diagnose before fixing: superficially similar modes live in different components with different
  fixes; applying the cross-site fix to an intra-cluster divergence repairs nothing and resets real
  state.
- Forward-fix and backfill are SEPARATE work: restarting replication from now restores the future and
  does nothing for the gap — objects written during the broken window exist on one side only and
  keep flapping until reconciled. Closing the incident on the forward fix alone re-opens it as a
  mystery later; the reconciliation is its own tracked item with its own options (snapshot-restore,
  reduce to one replica, rebuild the diverged one) and costs.
- A resume-point override should be applied through the system's declared config path where one
  exists, and designed to be harmless once passed (applied only when ahead of the stored offset) —
  a fix you must remember to remove is a second incident.

## Verification
- Post-fix, the retry-loop signature count in the replication logs is zero, and a fresh write
  round-trips in BOTH directions within the expected lag.
- A repeated-read stress of the balanced endpoint returns consistent success — the flapping is gone,
  not rarer.
- The divergence-window reconciliation item exists with the affected range, or the record states the
  measured conclusion that no data landed during the window.
