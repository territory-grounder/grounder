---
name: credential-rotation-safety
class: runbook
version: 0.1.0-distilled
source: distill:docs/runbooks/openobserve-credential-rotation.md
description: Rotate credentials without lockout — recovery path first, verify the new value before cutting consumers over, and a secret that touched history is burned
---

## Goal
Rotate a credential without locking yourself out and without leaving the old secret trusted anywhere.
Rotation is a two-sided change — the authority that accepts the credential and every consumer that
presents it — and the failure modes on both sides are silent until the worst moment.

## Required evidence
- The recovery-path answer BEFORE anything changes: if this rotation misbehaves, what still gets you
  in? A sole-admin account with no second administrative path and no out-of-band recovery makes
  rotation an unrecoverable-lockout risk — that rotation is operator-coordinated by definition, never
  automated.
- The consumer inventory: every service, script, and store that presents this credential, with where
  each reads it from. The consumer you forgot is the one that starts failing at the next restart,
  days later.
- The exposure history: has this credential's VALUE ever entered version control, a log, or a public
  mirror? A secret that touched history is burned — de-hardcoding the file does not un-publish the
  value; rotation is still owed.
- A verification probe per consumer (an authenticated call that returns success) runnable before and
  after.

## Decision rules
- Stage the rotation so both credentials are never simultaneously invalid: set the new credential at
  the authority; VERIFY new-credential access through the primary interface; only then cut consumers
  over; only then (where the authority supports it) retire the old value.
- Verify before you burn the bridge: the step order exists so that a failure at any point leaves a
  working credential somewhere. Confirming the new login BEFORE updating consumers is the load-
  bearing step, not politeness.
- Prefer the authority's durable configuration path (an environment the service re-applies on boot)
  over a one-shot mutation that a restart reverts — a rotation that survives only until the next
  reboot is a time bomb.
- New values are generated, never composed: full-entropy from a generator, landing only in the
  secret store or ignored-by-VCS config — never in source, never in the shell history of a shared
  box.
- Where rotation cannot be made safe (no recovery path, vendor refuses a second admin), record THAT
  as the finding with the compensating controls, rather than pretending the rotation is routine.

## Verification
- Post-rotation, every consumer's probe returns success, and a probe with the OLD value fails where
  the authority supports invalidation.
- The new value appears in exactly the intended stores; a repository search for it returns nothing.
- The exposure note for the old value is closed out: burned, rotated, and the history reference
  updated so nobody "restores" it later.
