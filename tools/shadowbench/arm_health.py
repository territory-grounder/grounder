#!/usr/bin/env python3
"""arm_health.py — judge whether TG's TRIAGE arm is healthy this campaign cycle.

TG-545 (2026-08-26): TG's worker triage plane lost its dynamic Postgres credential and sat DEAD for ~3.5h,
yet the campaign — watching only the injector — kept looping as though both arms were live, and a frozen
pair count was misread as slow accrual. A shadowbench pair needs BOTH arms; a dead challenger accrues
nothing and every axis measured over that window is unpopulated, not a result. So the harness now checks
TG's arm each cycle too and says so LOUDLY when it is down (the same lesson as the injector's own
"CAMPAIGN BARREN" self-report, spec/025 §7a).

The DECISION is pure and lives here so it is unit-tested; run-campaign.sh does the impure gathering (ssh
+ docker inspect + psql) and passes the three signals in.
"""
import sys

# STALL_INJECT_FLOOR: below this many injects in the window there was too little to triage for a zero
# triage count to mean anything — so a genuinely quiet cycle is never mistaken for a dead arm.
STALL_INJECT_FLOOR = 2


def assess(worker_health, triage_recent, injects_recent):
    """Return (healthy: bool, reason: str) for TG's triage arm this cycle.

    worker_health  — docker health status of the triage worker ("healthy" when the arm can serve).
    triage_recent  — TG triage sessions banked in the last cycle window.
    injects_recent — faults injected in the same window (what there WAS to triage).
    """
    if worker_health != "healthy":
        return False, "worker triage plane is %r (not healthy) — TG's exam arm is degraded" % (worker_health,)
    if injects_recent > STALL_INJECT_FLOOR and triage_recent == 0:
        return False, (
            "injector fired %d fault(s) but TG triaged 0 this window — arm stalled though the worker "
            "reports healthy (e.g. a starved credential, TG-545)" % (injects_recent,)
        )
    return True, "ok"


def main(argv):
    if len(argv) != 4:
        print("usage: arm_health.py <worker_health> <triage_recent> <injects_recent>", file=sys.stderr)
        return 2
    try:
        triage_recent = int(argv[2])
        injects_recent = int(argv[3])
    except ValueError:
        print("triage_recent and injects_recent must be integers", file=sys.stderr)
        return 2
    healthy, reason = assess(argv[1], triage_recent, injects_recent)
    print("OK" if healthy else "DEGRADED: %s" % reason)
    return 0 if healthy else 1


if __name__ == "__main__":
    sys.exit(main(sys.argv))
