# The skill-store lifecycle — draft, trial, production, retired, watch

Skills are the versioned behavioral instructions composed into the agent's seed. The store is
append-only: a version's body and predicate are immutable once created; a rework is always a NEW
draft row. Every status change passes through one audited state machine.

## The state machine

```
draft ──► trial ──► production ──► retired
  │         │
  └►rejected◄┘        (trial may also fall back to draft)
```

Anything not drawn is refused — including self-transitions and any path that would resurrect a
retired or rejected version. Every transition requires a **rationale** (the store refuses without
one); the rationale is appended to the version's append-only log and the decision is recorded on the
hash-chained governance ledger *before* the row is updated — a crash leaves an over-recorded ledger,
never an unrecorded state change.

## Where versions come from

- **compiled-import** — the boot importer seeds the store from the compiled registry.
- **hand** — an operator drafts a version in the console (session-authenticated; machine principals
  have no write route).
- **flywheel** — the system generates a candidate when an eval dimension regresses.

## Pinned floor skills

A pinned skill (e.g. `conservative-remediation`) is the floor: composition always uses the compiled
body, and the write path refuses a draft targeting it. The store can never override the floor.

## Trials — admission by evidence

A draft is admitted to trial only through the offline gate (the regression set must hold). An active
trial assigns matching sessions to candidate arms and a concurrent production **control**. After each
arm reaches its minimum sample count, a one-sided Welch t-test decides: the candidate graduates only
if it beats the control by at least the minimum lift at the configured p-threshold. Graduation
structurally retires the incumbent production version in the same logical step — the one-production
invariant is a database index, not a convention.

## The post-graduation watch

Graduation is not the end of scrutiny. Each graduate is watched for a bounded window (default 7
days): every judged session that composed the new version scores against the trial's control mean,
and enough CONSECUTIVE below-threshold sessions trip an automatic demotion — the graduate retires,
the prior production version is restored, the demotion is ledgered and escalated to a human. A skill
that passed its trial cannot silently persist a degradation.
