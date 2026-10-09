#!/usr/bin/env bash
# deadcode-gate.sh — the TG-4 leg-2 "nothing ships retired-but-present" mechanization.
#
# Runs golang.org/x/tools/cmd/deadcode over EVERY binary in the module (cmd/ AND tools/) and compares the normalized result
# against the committed baseline (scripts/deadcode-baseline.txt). The contract is a RATCHET:
#
#   - NEW dead code cannot land: any function unreachable from every main that is not already in the
#     baseline FAILS the gate. A merge that orphans code must either wire it (see the deadcode-intersection
#     technique that found the TG-250 unwired controls) or delete it — never ship it silently dark.
#   - FIXED dead code shrinks the baseline: any baseline entry no longer reported also FAILS, with the
#     instruction to regenerate — so the baseline can only go DOWN and cannot silently rot into a
#     grandfather-everything list (the same one-way discipline as the main() LOC ratchet and `tally`).
#
# Normalization strips line/column (they churn with every unrelated edit): `path/file.go: FuncName`.
# A file rename re-surfaces its dead entries — that is a feature (a rename is exactly when a dead
# function should be looked at), fixed by regenerating.
#
# Usage:  scripts/deadcode-gate.sh check    # CI / pre-merge: non-zero on ANY drift from the baseline
#         scripts/deadcode-gate.sh write    # regenerate the baseline (commit the result)
#
# Vacuity (a check that cannot report "nothing to check"): the gate FAILS if no mains are found or the
# analyzer itself fails — an empty result is compared against the baseline like any other, so a silently
# broken analyzer reads as massive drift, never as a pass.
#
# The analyzer is version-pinned (not @latest): reproducible across CI and dev boxes. Bump deliberately.
#
# LC_ALL=C is LOAD-BEARING, not hygiene: `sort` collates by locale, and `comm` REQUIRES both inputs sorted
# the SAME way. The committed baseline is generated on a dev box (en_US.UTF-8) while CI runs on a minimal
# image (C/POSIX) — different collation of the same entries makes `comm -13`/`comm -23` report dozens of
# phantom "new"/"fixed" lines, so the gate passed locally and RED-failed in CI on identical content. Pinning
# the locale here makes the sort — and the committed baseline below — byte-identical everywhere.
set -euo pipefail
export LC_ALL=C
cd "$(dirname "$0")/.."

TOOL="golang.org/x/tools/cmd/deadcode@v0.49.0"
BASELINE="${DEADCODE_GATE_BASELINE:-scripts/deadcode-baseline.txt}"
MODE="${1:-check}"

# DRILL SEAM (scripts/deadcode-gate_test.sh only, never CI). DEADCODE_GATE_CURRENT supplies a precomputed
# "current" list so the drill can exercise the RATCHET DECISION — new entry / unratcheted shrink / clean /
# missing baseline — without a fixture Go module. The real path always runs the analyzer; this seam cannot
# make a real run pass, because when it is unset nothing below changes.
CURRENT_OVERRIDE="${DEADCODE_GATE_CURRENT:-}"

# ROOT AT EVERY MAIN IN THE MODULE, not just ./cmd/... . The module ships 26 binaries and only 9 live under
# cmd/; the other 17 are tools/ (evalgate, rejudge, specvalidate, gencontracts, loadharness, …). Rooting at
# ./cmd/... alone made every function reachable ONLY from a tool binary read as unreachable — 43 entries of
# LIVE code in the first baseline — and this gate's own failure text tells the developer to "wire it or delete
# it". A gate whose advice is to delete running code is worse than no gate. Rooting at all mains also brings
# tools/ itself under the ratchet for the first time (its own dead code was never analysed).
mains=""
if [ -z "${DEADCODE_GATE_CURRENT:-}" ]; then
  mains="$(go list -f '{{if eq .Name "main"}}{{.ImportPath}}{{end}}' ./... 2>/dev/null | grep -v '^$' || true)"
fi
if [ -n "${DEADCODE_GATE_CURRENT:-}" ]; then
  mains="drill"; root_count=99
fi
if [ -z "$mains" ] && [ -z "${DEADCODE_GATE_CURRENT:-}" ]; then
  echo "deadcode-gate: FAIL — no main packages found in the module; the gate would be examining nothing (vacuity floor)" >&2
  exit 2
fi
# A SECOND vacuity floor on the ROOT SET: the module has many binaries, and a `go list` that silently returned
# one or two (a build break in a tool, a bad pattern) would shrink the reachable set and manufacture a wall of
# fake "new dead code". Fewer roots than this means the enumeration itself is broken, not that code died.
root_count="${root_count:-$(printf '%s\n' "$mains" | grep -c .)}"
if [ "$root_count" -lt 10 ]; then
  echo "deadcode-gate: FAIL — only $root_count main package(s) enumerated; the module has far more, so the root set is broken (vacuity floor)" >&2
  exit 2
fi

current="$(mktemp)"
trap 'rm -f "$current"' EXIT
if [ -n "$CURRENT_OVERRIDE" ]; then
  sort -u "$CURRENT_OVERRIDE" > "$current"
else
# shellcheck disable=SC2086 — $mains is intentionally word-split into the analyzer's package arguments
if ! go run "$TOOL" $mains 2>/dev/null \
  | sed -E 's/^([^:]+):[0-9]+:[0-9]+: unreachable func: (.*)$/\1: \2/' | sort -u > "$current"; then
  echo "deadcode-gate: FAIL — the analyzer ($TOOL) errored; refusing to compare a partial result" >&2
  exit 2
fi
fi

if [ "$MODE" = "write" ]; then
  cp "$current" "$BASELINE"
  echo "deadcode-gate: baseline regenerated — $(wc -l < "$BASELINE") entrie(s); commit $BASELINE"
  exit 0
fi

if [ ! -f "$BASELINE" ]; then
  echo "deadcode-gate: FAIL — no baseline at $BASELINE; run 'scripts/deadcode-gate.sh write' and commit it" >&2
  exit 2
fi

new="$(comm -13 "$BASELINE" "$current" || true)"
fixed="$(comm -23 "$BASELINE" "$current" || true)"

if [ -n "$new" ]; then
  echo "deadcode-gate: FAIL — NEW dead code (unreachable from every binary) not in the baseline:" >&2
  printf '%s\n' "$new" | sed 's/^/    /' >&2
  echo "  Wire it or delete it. If it is genuinely intentional dormant machinery, say WHY in the MR and" >&2
  echo "  regenerate: scripts/deadcode-gate.sh write   (the baseline is reviewed like any other diff)" >&2
  exit 1
fi
if [ -n "$fixed" ]; then
  echo "deadcode-gate: FAIL — baseline entries no longer dead (good!) but the baseline was not ratcheted down:" >&2
  printf '%s\n' "$fixed" | sed 's/^/    /' >&2
  echo "  Run: scripts/deadcode-gate.sh write   and commit the shrunken baseline (one-way ratchet)" >&2
  exit 1
fi
echo "deadcode-gate: PASS — $(wc -l < "$BASELINE") known entrie(s), 0 new, 0 unratcheted"
