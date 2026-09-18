#!/usr/bin/env bash
# lint-forbidden_test.sh — the forbidden-pattern gate's own drill.
#
# scripts/lint-forbidden.sh is the repo's most safety-critical lint: it is what makes the predecessor's
# injection/leak class uncompilable by policy (INV-02 no shell, INV-03 parameterized SQL only, the
# migration-pair rule, and the two mirror-safety rules that keep a private key or a provider credential out
# of the public GitHub mirror). It had NO oracle. TG-283's lesson is that a gate can be broken while every
# pipeline stays green — and a gate nobody can prove still FAILS is an assertion, not a control.
#
# HERMETIC BY CONSTRUCTION, and with no change to the gate itself: lint-forbidden.sh begins with
# `cd "$(dirname "$0")/.."`, so a COPY of it placed at <fixture>/scripts/ scans <fixture> and nothing else.
# Each case plants exactly one violation in an otherwise clean fixture, so a PASS in the clean case is the
# anti-vacuity floor for every FAIL below.
#
# The two mirror-safety markers are ASSEMBLED AT RUNTIME from split literals, never written literally here —
# this file is itself scanned by the real gate, and a literal marker in the drill would trip the very rule it
# is testing (and the mirror's abort-on-survivor guard).
set -uo pipefail
cd "$(dirname "$0")/.."
GATE="$PWD/scripts/lint-forbidden.sh"

fails=0
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# fixture <name> -> prints a clean fixture root with the gate copied in
fixture() {
  local root="$tmp/$1"
  mkdir -p "$root/scripts" "$root/core/db/migrations" "$root/adapters" "$root/cmd" "$root/temporal" "$root/tools" "$root/deploy"
  cp "$GATE" "$root/scripts/lint-forbidden.sh"
  # Rules 5 and 6 read sibling support files (the compose shell-safety checker and the STONITH baseline).
  # Copy exactly those two — copying all of scripts/ would drag in files that legitimately NAME the
  # forbidden patterns and trip the very rules under test.
  cp "$PWD/scripts/check-compose-shellsafe.py" "$root/scripts/" 2>/dev/null || true
  cp "$PWD/scripts/stonith-baseline.txt"      "$root/scripts/" 2>/dev/null || true
  # One clean Go file so the code rules have a real subject (a scan over zero files proves nothing).
  printf 'package core\n\nimport "os/exec"\n\nfunc Run() error { return exec.Command("systemctl", "restart", "x").Run() }\n' \
    > "$root/core/clean.go"
  # A properly paired migration, so rule 3 has a subject that satisfies it.
  printf 'CREATE TABLE t (id int);\n' > "$root/core/db/migrations/0001_t.up.sql"
  printf 'DROP TABLE t;\n'            > "$root/core/db/migrations/0001_t.down.sql"
  printf '%s' "$root"
}

run() { # run <root> -> rc
  ( cd "$1" && bash scripts/lint-forbidden.sh >/dev/null 2>&1 )
}

check() { # check <name> <want-rc> <root>
  local name="$1" want="$2" root="$3" rc
  run "$root"; rc=$?
  if [ "$rc" = "$want" ]; then
    echo "  ok: $name (rc=$rc)"
  else
    echo "  FAIL: $name — want rc=$want got rc=$rc" >&2
    fails=$((fails + 1))
  fi
}

echo "== forbidden-pattern gate drill =="

# ANTI-VACUITY FLOOR: a clean tree must PASS. Without this, every case below could be "detecting" a gate
# that simply always fails.
clean="$(fixture clean)"
check "a clean tree PASSES (anti-vacuity floor)" 0 "$clean"

# 1/7 — shell exec. THE rule that makes INV-02 mechanical: actuation is a fixed argv vector, never a shell.
sh_root="$(fixture shellexec)"
printf 'package core\n\nimport "os/exec"\n\nfunc Bad(c string) error { return exec.Command("sh", "-c", c).Run() }\n' \
  > "$sh_root/core/bad.go"
check "shell exec (sh -c) is REFUSED (INV-02)" 1 "$sh_root"

# 2/7 — string-built SQL (INV-03). The injection class the predecessor shipped.
sql_root="$(fixture sql)"
printf 'package core\n\nimport "fmt"\n\nfunc Q(t string) string { return fmt.Sprintf("SELECT * FROM %%s", t) }\n' \
  > "$sql_root/core/bad.go"
check "SQL assembled from strings is REFUSED (INV-03)" 1 "$sql_root"

# 3/7 — an up-migration with no down. A migration that cannot be rolled back must not merge.
mig_root="$(fixture migration)"
printf 'CREATE TABLE u (id int);\n' > "$mig_root/core/db/migrations/0002_u.up.sql"
check "an up-migration with no down is REFUSED" 1 "$mig_root"

# 4/7 — a contiguous PEM private-key marker. Assembled at runtime; never a literal in this file.
pem_root="$(fixture pem)"
marker="-----BEGIN OPENSSH ""PRIVATE KEY-----"
printf 'package core\n\nconst K = `%s\nnotarealkey\n`\n' "$marker" > "$pem_root/core/bad.go"
check "a contiguous PEM private-key marker is REFUSED (mirror-safety)" 1 "$pem_root"

# A SPLIT marker is the sanctioned escape for fixtures that legitimately need the shape — it must still PASS,
# or the rule would force test fixtures to lie about the value they build.
split_root="$(fixture pemsplit)"
printf 'package core\n\nconst K = "-----BEGIN OPENSSH " + "PRIVATE KEY-----"\n' > "$split_root/core/bad.go"
check "a SPLIT marker still passes (the sanctioned fixture escape)" 0 "$split_root"

if [ "$fails" -gt 0 ]; then
  echo "forbidden-pattern gate drill: FAIL ($fails)" >&2
  exit 1
fi
echo "forbidden-pattern gate drill: PASS"
