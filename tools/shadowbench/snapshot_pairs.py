#!/usr/bin/env python3
"""Freeze the exact judged pairs a campaign accrued into a COMMITTED snapshot, so the verdict reproduces from
a clean clone.

`scorecard.jsonl` is append-only, git-ignored, and spans every campaign + tier-2 self-test, so it cannot be
the reproducibility record (this is the gap that left campaign #1's published 21/17/p=0.0020 unreproducible —
TG-249). This selects the judged pairs INJECTED at/after a campaign's ACCRUE_FROM boundary, using the SAME
incident-time rule analyze.py's population enforces (`analyze._incident_ts`: tg_created_at or pred_first_ts),
and writes them to a committable `confirmatory/pairs-campaign<N>.jsonl`.

Stdlib only, for the same clean-clone reason analyze.py has no numpy.
"""
from __future__ import annotations

import argparse
import datetime as _dt
import json
import sys

from analyze import _incident_ts, load_manifest_keys  # the pair-time + population-membership single sources


def select(rows: list[dict], accrue_from: str, manifest_keys: set[str] | None = None) -> list[dict]:
    """The pairs injected at/after the boundary. A row with no establishable incident time is dropped
    (fail-closed), mirroring enforce_population — an unprovable pair is not part of the confirmatory record.

    With `manifest_keys` (TG-526, §6 2026-08-25) the snapshot is additionally restricted to pairs the
    confirmatory manifest joined to a post-boundary INJECTED fault — the same membership rule analyze.py's
    population enforces, so the committed record IS the confirmatory set, organic pairs excluded."""
    boundary = _dt.datetime.fromisoformat(accrue_from.replace("Z", "+00:00"))
    kept = []
    for r in rows:
        ts = _incident_ts(r)
        if ts is None or ts < boundary:
            continue
        if manifest_keys is not None and (r.get("key") or "") not in manifest_keys:
            continue
        kept.append(r)
    return kept


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description="Freeze a campaign's post-boundary judged pairs into a snapshot.")
    ap.add_argument("scorecard", help="the mutable judged-pairs ledger (scorecard.jsonl)")
    ap.add_argument("--accrue-from", required=True, help="the campaign's ACCRUE_FROM boundary (ISO 8601)")
    ap.add_argument("--out", required=True, help="the committed snapshot path (confirmatory/pairs-campaign<N>.jsonl)")
    ap.add_argument("--manifest", default=None,
                    help="confirmatory manifest JSONL; default: confirmatory/manifest.jsonl beside this file. "
                         "The snapshot keeps only manifest-joined pairs (TG-526). An ABSENT manifest is a "
                         "hard error — a 'confirmatory' snapshot that cannot apply its membership rule "
                         "must not be written.")
    args = ap.parse_args(argv)

    rows: list[dict] = []
    with open(args.scorecard) as fh:
        for line in fh:
            line = line.strip()
            if not line:
                continue
            try:
                rows.append(json.loads(line))
            except json.JSONDecodeError:
                continue

    import os
    manifest_path = args.manifest or os.path.join(os.path.dirname(os.path.abspath(__file__)),
                                                  "confirmatory", "manifest.jsonl")
    manifest_keys = load_manifest_keys(manifest_path)
    if manifest_keys is None:
        print(f"ERROR: confirmatory manifest not readable at {manifest_path} — refusing to write a "
              "'confirmatory' snapshot whose membership rule cannot be applied (TG-526)", file=sys.stderr)
        return 2

    kept = select(rows, args.accrue_from, manifest_keys)
    with open(args.out, "w") as fh:
        for r in kept:
            fh.write(json.dumps(r) + "\n")
    print(len(kept))
    return 0


if __name__ == "__main__":
    sys.exit(main())
