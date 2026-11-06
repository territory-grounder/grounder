## Goal
Operate device-configuration backups as INDEPENDENT tiers with one authority order — so that when the
primary record path burns down, a last-known-good still exists somewhere with no shared failure mode,
and so that no backup ever gets mistaken for the truth.

## Required evidence
- The tier map: (1) the live device — the only source of truth; (2) the reviewed record path (the
  drift-sync into version control that IaC consumes); (3) an independent local capture tier with no
  network dependency on the record path's infrastructure and no shared credentials with it.
- Demonstrated freshness per tier: each tier's latest capture timestamp against its cadence — a
  backup tier is an assertion, and an unverified assertion about disaster recovery is the worst kind.
- The failure-independence argument, stated: which outages take out which tiers. Two tiers that die
  together are one tier with extra cost.
- The access path for the emergency case: how an operator retrieves last-known-good from the
  independent tier when the record-path infrastructure is down.

## Decision rules
- The live device outranks every stored copy: when any backup disagrees with the device, the device
  wins, and the disagreement is a drift finding for the record path — never a reason to "restore"
  over live from a stale copy.
- One writer per record path: exactly one mechanism pushes device config into the reviewed
  repository. A second pusher (a backup tier grown ambitions) produces conflicting commits; when a
  tier's push path is retired, retire it fully — dead sync scripts and their CI exceptions rot into
  confusion.
- The independent tier stays deliberately dumb: local capture, no push, no authority — its whole value
  is surviving the clever infrastructure's failure. Resist consolidating it "for consistency".
- Keep-or-retire is a periodic REVIEWED decision with named triggers (maintenance burden, upstream
  breakage, the primary proving sufficient), not a default drift into either keeping junk or deleting
  the parachute.
- Cheapness is part of the design: an independent tier justified at tens of megabytes of RAM is
  retained on a different standard than one costing real operational attention.

## Verification
- Each tier's freshness check passes on schedule, and the independent tier's retrieval procedure has
  been exercised — a backup that has never been read back is a hope.
- Exactly one writer is observable in the record path's history; the retired paths' remnants
  (scripts, CI filters) are confirmed gone.
- The periodic review is on the calendar with its trigger list, and the last review's decision is
  recorded.
