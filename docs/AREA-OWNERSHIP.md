# Area ownership — stay in your area, flag, don't reach

> Reference, not a work queue (TG-80 P3-9, 2026-08-22). This page names the ownership primitives that
> ALREADY exist and the one rule that binds them. It invents no new scheme: every primitive below is
> lint-enforced or on the record somewhere else, and this page is the map to them.

## The rule

An agent session (human or model) works **inside the area it holds** and, when it finds something
outside that area, **flags it — on the record — instead of reaching over and fixing it**. Reaching is
how two sessions end up editing one file, how a "quick fix" lands without its spec, and how a finding
gets silently absorbed into an unrelated MR where no review will look for it.

"Flag" is concrete: file or comment the YouTrack ticket (a finding with no ticket is lost), or message the
owning session and **record the outcome on the ticket** — never only in the message channel
(AGENTS.md § Parallel sessions, rule 6).

## What "your area" means — four layers, each with its own enforcement

| Layer | Unit of ownership | Where it is declared | Who enforces it |
|---|---|---|---|
| **Task ↔ files** | one SDD task owns an exact file list | `spec/<nnn>/tasks.json` → `files_owned` (schema: `docs/SDD-WORKFLOW.md` § tasks.json) | `tools/specvalidate` — a file owned by two tasks is a hard error ("parallel collision"); a completed task owning a phantom path is a warning ratcheted to zero (TG-416) |
| **Session ↔ branch** | one session holds one worktree branch under a claim | `.claude/worktrees/.claims/<url-encoded-branch>.claim/meta` (`scripts/claim-worktree.sh`) | `scripts/lint-claim-before-touch.sh` inside `make all` — an unclaimed worktree branch fails at its first local gate (TG-81a) |
| **Session ↔ ticket** | the partition of queue items between concurrent sessions | TG-488 and its partition comments; the board's owner list | social + the board (`docs/BOARD.md`); the autonomy-boundary lint requires every owner-list entry to cite its `[R#]` clause |
| **Human ↔ law surfaces** | the files an agent operates UNDER may not be rewritten by an agent alone | `CODEOWNERS`; the list in `scripts/lint-protected-paths.sh` | CI `protected-paths` job, no `allow_failure`: a listed path changes only with a `Law-Change-Approved-By:` trailer |

Two more primitives complete the picture: the **content-hash lockstep manifest** (`spec/007`) ties every
governed Go file to its owning spec, so a file that changes without its spec is reported as drift (the
spec is part of the area); and the **shared-resource mutexes** (`/tmp/tg-gateway.lock` for box-heavy
runs, `TG_EVAL_PORT` for session-pinned eval tunnels — AGENTS.md § Parallel sessions rule 5) are the
ownership of a *resource* for the duration of a run.

## How to tell where a file belongs

1. `grep -rn '"<path>"' spec/*/tasks.json` — the task (and so the spec) that owns it.
2. `git log --format=%s -5 -- <path>` — which ticket series has been landing in it.
3. `.claude/worktrees/.claims/` — whether a live session currently holds a branch touching it.
4. If it is on the protected list, it is **nobody's** area to change alone — it needs the trailer.

## What reaching looks like, and what to do instead

| You find… | Don't | Do |
|---|---|---|
| a bug in a file another task owns | fix it inside your MR | file the ticket; if it blocks you, message the owning session and record their answer on your ticket |
| a drift-guard refusing a file you added to a generated tree | exempt the tree | make the package declare its own residents and have the guard consult that (the `skillcorpus.PackageFiles` pattern, TG-529) |
| a protected path that needs a one-line change | slip it into an unrelated MR | its own MR, the trailer, the provenance quoted in the commit body |
| a sibling session's branch that is "obviously" abandoned | push to it | ask; a stalled claim is released by its holder or by the nightly reset, not by a peer |
| a finding outside the product (estate, CI runner, a peer's tool) | fix the estate | file it where its owner will see it; the repo's tickets are TG-*, the estate has its own |

## Why this page exists

The predecessor's sibling-session races (two sessions in one tree, one `files_owned` list that only
checked inside a spec) are on the record in `docs/history/BOARD-JOURNAL-2026-08.md` (the TG-416 phantom-
ownership incidents) and in AGENTS.md's parallel-sessions protocol. h-conductor's "scoped-directory
ownership + claim-before-touch" was the external pattern that confirmed the shape (TG-81, borrow 7);
TG already had the primitives — what was missing was one page saying they are the same rule.
