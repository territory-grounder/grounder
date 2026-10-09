## Goal
When an investigation fails partway, hand the next tier RESUMABLE state — what completed, what failed,
what was learned — so it picks up at the failed step instead of restarting. Re-doing completed work is
the expensive half of every escalation, and the failure itself is usually evidence.

## Required evidence
- The step that failed, with the RAW error text — not a paraphrase.
- The steps that completed, each with its finding.
- The incident identifier, if one was created before the failure.
- A discriminating observation about the failure mode itself: did the TARGET fail (host unreachable) or
  did the TOOLING fail (credential, missing resource, malformed query)? These imply different next steps.

## Decision rules
- Always hand off in this structure, in this order:
  - Failed at — step and description.
  - Completed steps — each with its one-line finding.
  - Error — the raw message.
  - Partial findings — what the completed steps established.
  - Incident id — or explicitly "not created".
  - Suggested next action — what the next tier should do FIRST.
- A read of <host> that times out is itself evidence: the host (or the path to it) may be down. Say so,
  and suggest checking its parent via `get-estate-context <host>` before retrying the same read.
- Discriminate tool-failure classes rather than reporting "it failed": an unreachable target, a refused
  credential, and an absent resource each name a different next action — and only one of them is about
  the incident.
- If the incident record could not be created, say that explicitly — otherwise the next tier posts
  findings into a void.
- Cap your stated confidence to reflect the incompleteness, and give the reason ("investigation
  incomplete — could not observe the guest's state").

## Verification
- The handoff alone is sufficient to resume: the next tier can start at the failed step using only what
  the handoff names — identifiers, findings, next action.
- Nothing already completed is re-run by the next tier; if it was, the handoff under-specified.
- The failure mode itself appears in the final record as evidence, not as an apology.
