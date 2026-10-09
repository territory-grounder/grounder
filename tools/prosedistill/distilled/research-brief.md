## Goal
Produce a research brief: the facts a decision needs, gathered and structured BEFORE anyone reasons
about a fix. The research seat proposes nothing — a fix proposed from the research seat skips the risk
classification every proposal owes — and it hides nothing: what could not be observed is part of the
brief, not a private disappointment.

## Required evidence
- `get-estate-context <host>` — identity and topology: role, site, parents, siblings.
- `get-device-status <host>` and `get-device-eventlog <host>` — live state and the events around the
  alert window.
- `get-active-alerts` — what else is firing, and this alert's own age and flap count.
- `get-incident-history <host>` — prior incidents on this target and its siblings.
- `get-tracker-history` — open work items that may already explain the condition.

## Decision rules
- Facts only. The brief ends at evidence; the proposal seat, with the risk machinery behind it, decides
  what to do. If a fix becomes obvious mid-research, record the observation that makes it obvious — not
  the fix.
- Source state questions by liveness: the device's own answer outranks the monitor's, and the monitor's
  outranks the inventory's — the inventory is maintained by hands and drifts. (Identity questions run
  the other way: the inventory says WHAT a thing is — see identity-first-lookup.)
- Name targets fully and unambiguously — site-qualified, never a bare short name. Multi-site estates
  have burned real diagnoses on ambiguous names.
- Vet the alert itself before treating it as real: a high flap count, an active maintenance window, or
  a first-seen predating the incident all change what the brief means. Say which applies.
- The brief carries an OBSTACLES section: unreachable hosts, reads that returned nothing, sources that
  were stale, workarounds used. Negative evidence steers the next tier's first move; omitting it
  converts your dead end into their repeated dead end.
- Structure is fixed — identity, live state, alert context, history, obstacles — so the consumer can
  find each answer without re-reading prose.

## Verification
- Every claim in the brief cites the read that produced it; no section is filled from memory.
- The brief contains zero proposal language — no "recommend", no "should", no op-class.
- The obstacles section is present even when empty ("all sources answered") — its absence is
  indistinguishable from an unexamined path.
