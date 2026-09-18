#!/usr/bin/env python3
"""CI validator for the provisioned Grafana dashboards.

A malformed dashboard silently fails to provision (Grafana skips it and logs an error), and — because the
AWX deploy does not currently sync the monitoring/ subdir — a broken dashboard would not even fail the
deploy. This gate catches it at merge time: every JSON under provisioning/dashboards/ must parse and carry
the fields Grafana's file provider needs (uid, title, schemaVersion, panels), and every panel must name a
datasource and at least one target. Pure stdlib; no network.
"""
import glob
import json
import os
import sys

DASH_DIR = os.path.join(os.path.dirname(__file__), "..", "grafana", "provisioning", "dashboards")


def validate(path):
    errs = []
    try:
        d = json.load(open(path))
    except Exception as e:
        return ["%s: not valid JSON: %s" % (path, e)]
    for key in ("uid", "title", "schemaVersion", "panels"):
        if not d.get(key):
            errs.append("%s: missing/empty '%s'" % (path, key))
    for p in d.get("panels", []) or []:
        pid = p.get("id", "?")
        if not p.get("datasource"):
            errs.append("%s: panel %s has no datasource" % (path, pid))
        if not p.get("targets"):
            errs.append("%s: panel %s has no targets" % (path, pid))
    return errs


def main():
    files = sorted(glob.glob(os.path.join(DASH_DIR, "*.json")))
    if not files:
        print("no dashboards found under", DASH_DIR)
        return 1
    all_errs = []
    for f in files:
        errs = validate(f)
        if errs:
            all_errs += errs
        else:
            with open(f) as fh:
                n = len(json.load(fh).get("panels", []))
            print("OK  %s (%d panels)" % (os.path.relpath(f), n))
    for e in all_errs:
        print("FAIL", e)
    return 1 if all_errs else 0


if __name__ == "__main__":
    sys.exit(main())
