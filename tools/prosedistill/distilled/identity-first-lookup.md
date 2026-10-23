## Goal
Resolve what the target IS before reasoning about what is wrong with it. Role, site, topology parents,
and addresses come from the estate's source of truth, first — a diagnosis built on a misidentified
device is worse than no diagnosis, because it carries false confidence into the proposal.

## Required evidence
- `get-estate-context <host>` — the inventory's identity for the target: role, site, parents, addresses,
  and its relations to the rest of the estate.
- `get-device-status <host>` — the live counterpart, confirming the monitored device matches the
  inventory identity.
- The alert's own naming of the target — hostname or address — held up against both.

## Decision rules
- Identity FIRST, on any unfamiliar or ambiguous target: run the inventory read before any diagnostic
  read. Never answer an identity question from memory — memory is stale, and estates move.
- The inventory read is authoritative for WHAT a thing is; the live read is authoritative for HOW it is.
  Do not let either answer the other's question.
- MISMATCH is a stop condition: the alert names a host the inventory does not know, or an address the
  inventory assigns elsewhere → stop and resolve the identity before diagnosing anything. The mismatch
  itself may be the incident (an inventory gap, a re-used address, a renamed guest).
- Given only an <ip>, resolve it to its owning host and service before treating it as a host — an
  address is a pointer, not an identity.
- Identity questions are terminal work: answer them from the read; never escalate a lookup.
- Use the resolved topology to aim the next read: the parent and siblings named by
  `get-estate-context` are where a cascade check goes, not a guessed neighbor list.

## Verification
- The record OPENS with the resolved identity — role, site, parent — cited from the inventory read.
- Every later claim about the target is consistent with that identity; a contradiction reopens the
  identity question before anything else proceeds.
- Any identity ambiguity found was resolved (or escalated as an inventory-gap finding) BEFORE the
  diagnosis, never papered over after it.
