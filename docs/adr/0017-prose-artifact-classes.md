# 0017 — Prose artifacts are classes on the ONE skill store, not a parallel store

Status: Accepted (2026-08-14)

## Decision
Every LLM-facing prose artifact (agent skills, the base prompt's trialable half, runbooks, the judge-rubric mirror) becomes a row on the EXISTING versioned skill store, discriminated by a closed `artifact_class` vocabulary {skill, prompt, runbook, rubric} (spec/014 REQ-1315/1316) — not a new `prose_artifact` table, not a second write path.

## Why
- The store's two structural invariants (one production version per name; one active trial per name) are exactly the invariants every prose class needs; a parallel table would re-implement them and drift (the predecessor's supersede logic lived in application code and drifted — the 0009 header's founding lesson).
- `skill_trial` rows FK into `skill(name)`/`skill_version(id)`; migrating live rows under ACTIVE trials to a new table breaks the flywheel mid-trial. An additive, defaulted column is a Postgres metadata-only change: zero row rewrites, live trials untouched (proven by the migration round-trip oracle).
- ADR-0012 already committed the store's rows to a 1:1 SKILL.md mapping; classes ride the same mapping (`class:` frontmatter later).

## Per-class treatment (the risk ladder, owner-scoped on epic TG-114)
- `skill`, `prompt`: first-class flywheel artifacts (generate → offline-admit → A/B trial → graduate). `prompt`'s wire-format half is a class-forced-PINNED row the flywheel can never draft against (lands with the base-prompt split).
- `rubric`: pinned mirror of the embedded judge rubric — never graduating, write-refused (lands with the rubric-mirror task).
- `runbook`: knowledge-library content — never composes into the agent seed, no trial verb (lands with the wiki task).
- Safety/screen prose (never-auto floor, injection patterns): DELIBERATELY NOT A CLASS — it never enters the database at all; "never in the flywheel" is enforced by absence, under lockstep hash + law trailer in its own MR.

## Consequences
- Per-class body caps are domain law over a widened schema ceiling (REQ-1316) — the schema alone can no longer prove the skill cap; the domain refusal oracle can.
- The Go package keeps its `skillstore` name: renaming to `proseartifact` would churn every call site for zero behavior; the class vocabulary, not the package name, carries the concept.

## Alternatives rejected
- A new `prose_artifact` table + view/adapter over the old store: dual write paths, FK migration under live trials, re-implemented invariants.
- Per-class tables: four copies of the same machinery.
