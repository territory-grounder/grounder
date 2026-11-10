#!/usr/bin/env bash
# TG-86 slice 1c — produce the estate-doc GROUNDING corpus, RUNNER-SIDE, with a fail-closed no-leak gate.
#
# WHY RUNNER-SIDE. The estate's IaC documentation (the CLAUDE.md / runbook tree) lives on the runner box;
# the deployed worker is DISTROLESS — no shell, no git, no checkout — so it cannot ingest that tree itself.
# This script runs the already-shipped cmd/estatedoc-ingest HERE, where the docs are, to produce the
# scrubbed corpus JSON. Only that corpus is then shipped to the box and mounted read-only for the worker
# (set TG_ESTATE_DOC_CORPUS to its path). This is the PREFERRED producer transport; a second one exists —
# the worker's own TG_ESTATE_DOCS_DIR walk (cmd/worker/estate_doc_coverage.go) — which persists a corpus
# only when TG_ESTATE_DOC_REDACT_PATTERNS arms the same redaction, and REFUSES the write otherwise.
#
# WHAT THE INGEST SCRUBS. cmd/estatedoc-ingest passes every chunk through core/screen, which redacts SECRETS
# (credentials/tokens/keys → [REDACTED:*]) and neutralizes injection (→ [SCREENED:*]) — and, armed below via
# -redact-patterns, ALSO redacts estate IDENTIFIERS: any token matching the mirror denylist's patterns
# (site-codes, domain, paths) or the IP/MAC shapes in estate-redact-extra.txt becomes [REDACTED:estate-id],
# whole-token. So the persisted corpus passes gate 2 BY CONSTRUCTION — the floor stays; the artifact is
# clean (TG-86 follow-up; the owner-ruled alternative to relaxing the denylist). The corpus remains
# PRIVATE, per-install, on-box data that MUST NEVER be committed or public-mirrored (the rule
# core/estatedoc/corpus_io.go states). Two gates enforce that mechanically, both fail-closed:
#   1. the output path is REFUSED if it resolves inside this repo working tree (so it can never be committed);
#   2. the produced corpus is scanned against the public-mirror denylist (github-sync/denylist.txt) and, on
#      ANY surviving match, DELETED and the run FAILED — the same abort-on-survivor floor the mirror sync and
#      tools/prosedistill use, reused here so a corpus still carrying a denied token is never shippable.
set -euo pipefail

usage() {
  cat >&2 <<'EOF'
usage: estate-docs-corpus.sh <iac-docs-dir> <output-corpus.json>
   or: TG_ESTATE_DOCS_DIR=<dir> TG_ESTATE_DOC_CORPUS=<out> estate-docs-corpus.sh

Produce the scrubbed estate-doc grounding corpus from <iac-docs-dir> into <output-corpus.json>.
The output MUST be OUTSIDE this repo: the corpus is private, on-box data and is never committed.
EOF
  exit 2
}

docs_dir="${1:-${TG_ESTATE_DOCS_DIR:-}}"
out="${2:-${TG_ESTATE_DOC_CORPUS:-}}"
if [ -z "$docs_dir" ] || [ -z "$out" ]; then
  usage
fi

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(git -C "$script_dir" rev-parse --show-toplevel)"
denylist="$repo_root/github-sync/denylist.txt"

if [ ! -d "$docs_dir" ]; then
  printf 'estate-docs-corpus: docs dir "%s" is not a directory\n' "$docs_dir" >&2
  exit 1
fi
if [ ! -f "$denylist" ]; then
  printf 'estate-docs-corpus: denylist "%s" is missing — refusing to run without the no-leak floor\n' "$denylist" >&2
  exit 1
fi

# STONITH gate 1 — the corpus can NEVER land in a committable path. Resolve the output's PARENT (the file
# may not exist yet) to an absolute dir and refuse if it is the repo tree or under it. The trailing slash on
# both sides makes this a true path-prefix test, so a sibling like "<repo>-staging" is NOT refused.
out_parent="$(cd -- "$(dirname -- "$out")" 2>/dev/null && pwd)" || {
  printf 'estate-docs-corpus: output directory for "%s" does not exist\n' "$out" >&2
  exit 1
}
case "$out_parent/" in
  "$repo_root/"*)
    printf 'estate-docs-corpus: output "%s" is inside the repo (%s) — REFUSED.\n' "$out" "$repo_root" >&2
    printf '  The estate-doc corpus is private, on-box data and must never be committed. Write it to a\n' >&2
    printf '  staging path OUTSIDE the repo, then ship that file to the box knowledge mount.\n' >&2
    exit 1
    ;;
esac

extra_patterns="$repo_root/scripts/estate-redact-extra.txt"
if [ ! -f "$extra_patterns" ]; then
  printf 'estate-docs-corpus: extra pattern file "%s" is missing — refusing to run with half the redaction vocabulary\n' "$extra_patterns" >&2
  exit 1
fi

printf 'estate-docs-corpus: ingesting %s -> %s (identifier redaction: denylist + extra shapes)\n' "$docs_dir" "$out" >&2
(
  cd -- "$repo_root"
  go run ./cmd/estatedoc-ingest -root "$docs_dir" -out "$out" -redact-patterns "$denylist,$extra_patterns"
)

if [ ! -f "$out" ]; then
  printf 'estate-docs-corpus: ingest produced no output at "%s"\n' "$out" >&2
  exit 1
fi

# STONITH gate 2 — abort-on-survivor no-leak scan against the public-mirror denylist. Each non-comment,
# non-blank line is one grep -E pattern; ANY match means a denied token survived into the corpus, so the
# artifact is DELETED and the run fails closed. A denylist that yields zero patterns is also a failure — half
# a floor is no floor (the prosedistill sanitizer-floor discipline).
printf 'estate-docs-corpus: no-leak scan against %s\n' "$denylist" >&2
patterns=0
survivors=0
while IFS= read -r pat || [ -n "$pat" ]; do
  case "$pat" in
    '' | '#'*) continue ;;
  esac
  patterns=$((patterns + 1))
  if grep -E -q -e "$pat" -- "$out"; then
    printf '  LEAK: denylist pattern still matches the corpus: %s\n' "$pat" >&2
    survivors=$((survivors + 1))
  fi
done < "$denylist"

if [ "$patterns" -eq 0 ]; then
  printf 'estate-docs-corpus: denylist yielded zero patterns — refusing to trust an empty floor\n' >&2
  rm -f -- "$out"
  exit 1
fi
if [ "$survivors" -gt 0 ]; then
  printf 'estate-docs-corpus: %d denied token(s) survived scrubbing — corpus DELETED, not shipped.\n' "$survivors" >&2
  printf '  core/screen redacts SECRETS only; it does not strip estate hostnames/IPs/site-codes. If those\n' >&2
  printf '  survived, the loader needs estate-identifier scrubbing before this corpus can be treated as\n' >&2
  printf '  denylist-clean (a TG-86 finding). A private on-box corpus that INTENTIONALLY retains estate\n' >&2
  printf '  specifics for grounding is an owner decision — relax this gate only under that ruling.\n' >&2
  rm -f -- "$out"
  exit 1
fi

printf 'estate-docs-corpus: OK — %d denylist patterns clear; corpus at %s (private, on-box; do NOT commit).\n' "$patterns" "$out" >&2
