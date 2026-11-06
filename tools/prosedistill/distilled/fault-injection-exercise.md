## Goal
The discipline of deliberately injecting faults to prove recovery machinery works: every scenario has
pre-checks, a staged expectation, time-bound validation, a rehearsed recovery, and an abort path — and
the exercise never risks more than it is scoped to prove. Operator-run by definition: fault injection
is actuation and lives outside the agent's lane entirely.

## Required evidence
- Pre-checks proving the SURVIVING path is healthy before the primary is killed: an exercise that
  breaks the only working leg is an outage with paperwork. Verify every backup path, baseline
  reachability, and access to the injection and recovery points.
- A written expected-behavior timeline (what changes at 0-5s, 5-30s, 30-120s) — the exercise's
  falsifiable prediction.
- Machine-checkable validation markers with time bounds ("recovered metric X within Ns"), evaluated
  during and after.
- A declared maintenance/exercise state visible to alerting BEFORE injection, and a notification to
  the owning channel.

## Decision rules
- One redundant leg at a time: never disrupt both members of a redundant pair in one exercise. The
  widest scenarios (full site isolation) are operator-attended, never unattended.
- Sequence multi-fault scenarios with cooldowns between phases; simultaneous injection tests nothing —
  it only obscures which fault caused which symptom.
- Maintain an exclusion list of systems too fragile or too stateful to chaos-test, and honor it — the
  exercise program's scope is a reviewed decision, not a session choice.
- Blast-radius floor: never inject into, or "recover" via, the layer that carries production upstream
  connectivity for everyone (control-plane resets on shared transit are how a drill becomes a real
  outage). Recovery uses the narrow, scoped action.
- Every exercise runs under a dead-man: if the framework stalls past its timer, all faults roll back
  automatically; a manual abort path exists and is rehearsed.
- Distinguish degradation drills (latency/loss injection) from outage drills: degraded runs are graded
  against protocol-timer reasoning (a hold timer well above injected latency should ride through) and
  DEGRADED — transient flaps that self-recover — can be an acceptable grade for stress scenarios; FAIL
  is reserved for needing manual intervention.
- Recovery includes cleaning the side effects of the fault, not just reverting it: protection systems
  (shun/block tables) triggered during the drill, connection-state staleness, suppression flags.
- Every exercise ends by walking recovery-revalidation, and the widest ones end with a post-mortem
  within a fixed window whose findings land where the next triage will retrieve them.

## Verification
- The exercise record shows pre-check results, per-phase validation marker outcomes, and the final
  revalidation — PASS/DEGRADED/FAIL assigned by the markers, not by impression.
- The declared exercise state was removed afterward and shown absent.
- Any surprise (quorum behavior, protection-state grudges, stale connections) is recorded as a
  finding with a follow-up item, not left as tribal memory.
