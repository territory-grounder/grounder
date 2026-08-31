---
name: drift-check
class: skill
version: 0.1.0-distilled
source: distill:.claude/skills/drift-check/SKILL.md
description: Compare declared configuration against running state, report drift per surface, and let drift reshape the fix
---

## Goal
Establish whether running state matches declared state, per surface, and let the answer reshape the fix.
A drift-aware fix is cheaper than a drift-blind one: when live has silently diverged from the declared
configuration, patching the symptom re-breaks on the next reconcile.

## Required evidence
- The DECLARED side: the configuration source of truth for the surface under review (infrastructure
  declarations, the estate inventory, the deployment manifest — whichever owns <host>).
- The RUNNING side, read from the live system, never from a stamp in a document:
  `get-device-status <host>`, `check-host-services <host>` for service posture, and
  `get-estate-context <host>` for what the inventory believes the host is.
- For each compared surface: the exact declared value and the exact observed value, side by side.

## Decision rules
- Report drift as a per-surface count — "X of Y resources drifted", never a bare "drift detected".
  An unquantified drift claim cannot be verified or prioritized.
- Drift that MATCHES the active alert is a root-cause candidate: decide deliberately whether the
  declaration or the runtime is right, and fix that side — not whichever is easier to touch.
- Drift with NO matching alert is surfaced, not silently reconciled: someone changed something, and
  reconciling over it destroys the evidence of what and why.
- Expected drift exists: immediately after a recovery or a deliberate manual intervention, live may
  legitimately lead the declaration. Check recent-change context before flagging.
- No drift found does not close the incident — it eliminates one cause class. Say so and move on.

## Verification
- After any reconciliation, re-run the same comparison: the touched surface's drifted count is zero,
  and the declared and observed values now agree — cite both.
- The next scheduled comparison stays clean: drift that returns on its own points at an unmanaged
  writer, which is its own incident.
