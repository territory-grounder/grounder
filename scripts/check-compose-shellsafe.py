#!/usr/bin/env python3
"""Guard: every `sh -c` init command embedded in a compose file must be POSIX-sh parseable.

Rationale: a busybox init service (secrets-perms) once shipped `echo ... (secrets ro, knowledge rw)`
with UNQUOTED parens. `make all` was green — the compose YAML parsed fine — but at deploy time busybox
`sh -c` read `(` as a subshell and exited 2, and because the core services `depend_on` that init with
`service_completed_successfully`, the ENTIRE stack wedged in `Created` (a full outage). The compose
parser cannot catch this; only the shell can. So we extract each `["sh","-c",<script>]` command/entrypoint
and run `sh -n <script>` (parse-only, never executes) to catch the syntax-error class at branch time.

Stdlib only (json, not pyyaml): our init commands are single-line JSON-array form, so we scan for
`command:`/`entrypoint:` lines whose value is a JSON array and json.loads them. Multi-line YAML block
commands are not covered (we have none); if one is added, extend this guard rather than route around it.
"""
import json
import re
import subprocess
import sys
from pathlib import Path

SHELLS = {"sh", "/bin/sh", "bash", "/bin/bash", "ash", "/bin/ash"}
# Matches e.g.   command: ["sh","-c","..."]   or   entrypoint: ['sh','-c','...']
LINE = re.compile(r'^\s*(command|entrypoint)\s*:\s*(\[.*\])\s*$')


def scripts_in(path: Path):
    """Yield (lineno, script) for every sh -c command/entrypoint JSON array in the file."""
    for i, line in enumerate(path.read_text().splitlines(), 1):
        m = LINE.match(line)
        if not m:
            continue
        try:
            arr = json.loads(m.group(2))
        except json.JSONDecodeError:
            continue  # not a JSON-array form (e.g. YAML block/flow) — out of this guard's scope
        if not isinstance(arr, list) or len(arr) < 3:
            continue
        if str(arr[0]) in SHELLS and str(arr[1]) == "-c":
            yield i, arr[2]


# A mapping KEY line: leading indent, a key (no leading '-'/'#', up to the first ':'), then ':' + space/EOL.
# Non-greedy key stops at the first ':', so `image: ghcr.io/x:tag` and `TG_X: ${V:-d}` key correctly on the
# first colon. Lines whose first non-space char is '-' (sequence items) or '#' (comments) are excluded.
KEYLINE = re.compile(r'^(\s*)(?![-#])([^\s:][^:]*?)\s*:(?:\s|$)')


def duplicate_keys_in(path: Path):
    """Yield (lineno, key, first_lineno) for a mapping key defined TWICE in the SAME scope (same parent,
    same indent). `docker compose` REJECTS a duplicate key, but Go's yaml.v3 (used by the env-parity test)
    silently keeps the last — so a duplicate passes `make all` yet wedges the real deploy (the
    TG_LDAP_USER_BASE outage). This stdlib, scope-aware scan closes that gap at branch time.

    Scope model: a stack of (indent, {key: first_lineno}) frames. A key at indent N belongs to the frame at
    indent N (a fresh child frame is pushed when N exceeds the top frame's indent); a repeat within a frame
    is the duplicate. Keys in DIFFERENT services (different parent frames) never collide — each mapping gets
    its own frame. Adequate for compose's scalar-valued env blocks (no flow-mapping / multiline-scalar keys).
    """
    stack = [(-1, {})]  # (indent, seen: key -> first lineno)
    for i, raw in enumerate(path.read_text().splitlines(), 1):
        if not raw.strip() or raw.lstrip().startswith(('#', '- ')) or raw.strip() == '-':
            continue
        m = KEYLINE.match(raw)
        if not m:
            continue
        indent, key = len(m.group(1)), m.group(2).strip()
        while len(stack) > 1 and stack[-1][0] > indent:
            stack.pop()
        if stack[-1][0] < indent:
            stack.append((indent, {}))
        seen = stack[-1][1]
        if key in seen:
            yield i, key, seen[key]
        else:
            seen[key] = i


def main() -> int:
    files = [Path(p) for p in sys.argv[1:]] or list(Path("deploy").glob("*.yml"))
    checked = 0
    bad = 0
    dupbad = 0
    for f in files:
        if not f.exists():
            continue
        for lineno, script in scripts_in(f):
            checked += 1
            # sh -n = parse the script for syntax errors WITHOUT executing anything.
            r = subprocess.run(["sh", "-n"], input=script, text=True,
                               stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
            if r.returncode != 0:
                bad += 1
                print(f"  UNPARSEABLE sh -c command at {f}:{lineno}")
                print(f"    error: {r.stdout.strip()}")
                print(f"    script: {script}")
        for lineno, key, first in duplicate_keys_in(f):
            dupbad += 1
            print(f"  DUPLICATE compose key {key!r} at {f}:{lineno} (first defined at line {first})")
    if bad:
        print(f"  FORBIDDEN: {bad} compose sh -c command(s) fail `sh -n` — quote shell metacharacters")
        print("  (parens/globs/etc.) so the init runs under busybox; an init that exits non-zero wedges")
        print("  every service that depend_on's it with service_completed_successfully.")
    if dupbad:
        print(f"  FORBIDDEN: {dupbad} duplicate mapping key(s) — `docker compose` REJECTS a duplicate key")
        print("  (Go's yaml.v3 silently keeps the last, so make all is green while the real deploy wedges).")
        print("  Remove the redundant definition.")
    if bad or dupbad:
        return 1
    print(f"  ok ({checked} sh -c init command(s) parse clean; no duplicate compose keys)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
