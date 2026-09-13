---
name: live-workflow-edit-safety
class: runbook
version: 0.1.0-distilled
source: distill:docs/runbooks/n8n-code-node-safety.md
description: The unskippable sequence for editing code inside a live automation platform — snapshot, validate, push, re-fetch, test-fire — because dead code still parses
---

## Goal
Edit code that runs inside a LIVE automation platform (workflow nodes, embedded scripts) without the
class of outage where a push that "looked fine" kills the pipeline silently. The founding incident: a
parse-time error in DEAD code — unreachable but still parsed — took a production escalation pipeline
down for 14 hours, because nothing validated before the push and nothing failed loudly after it.

## Required evidence
- A rollback snapshot: the live definition fetched and saved BEFORE any edit — your undo is a file
  you hold, not a hope the platform versions things.
- A parse-equivalent validation of the edited code, matching the platform's runtime semantics (the
  same check the runtime applies, run locally), plus the platform-specific footgun checks: multiple
  top-level returns (everything after the first is dead code that still parses), duplicated top-level
  declarations (the copy-pasted-sibling-block signature).
- The re-fetched live definition AFTER the push, diffed against what was sent — live state is what
  runs; the push's success code is not evidence the platform stored what you meant.
- A test-fire: one synthetic or observed execution through the edited path before walking away.

## Decision rules
- The sequence is fixed and unskippable: snapshot → extract → edit locally → validate → splice →
  validate the whole artifact → push → re-fetch and compare → test-fire → export the verified state
  into version control. Each step exists because its absence has produced a real outage.
- Dead code is live risk: unreachable blocks are still parsed, so their syntax can kill the unit.
  Delete dead variants rather than keeping them "for reference" — reference belongs in version
  control history.
- Validation gates the push, not the review: a human eyeball on a large embedded script is not a
  parse check. Exit nonzero = do not push, no exceptions for "trivial" edits.
- A validator heuristic that false-positives on valid code gets removed, not tolerated — a gate the
  operators learn to override is no longer a gate.
- Loud-failure floor: the pipeline's execution failures must page (an alert on the platform's
  failure counter for the critical workflows). The 14-hour outage lasted 14 hours because detection
  was "someone notices" — an unvalidated edit plus silent failure is the full recipe.
- Platform sandboxes force config duplication (inline dicts mirroring canonical config files):
  treat every mirrored site as one change — updating fewer than all of them is a completed-looking
  edit that misroutes at runtime.

## Verification
- The record of any live edit shows the snapshot path, the validator's pass, the post-push diff
  (empty), and the test-fire result.
- The platform's failure-counter alert exists and has been proven to fire on a forced failing
  execution in a controlled window.
- Version control holds the exported post-edit definition, byte-matching the live one.
