-- 0090: the canonical artifact DESCRIPTION on the skill identity row (TG-55's SKILL.md interchange
-- half, TG-476; ADR-0012, spec/014 REQ-1315 family).
--
-- SKILL.md frontmatter maps 1:1 onto store rows ("conformance is an export format, not a rewrite" —
-- ADR-0012); its `description:` field had nowhere to live. It lands on skill (the IDENTITY), not
-- skill_version: it describes the artifact, and a per-version copy would split-brain the moment one
-- version's text was edited — the exact sidecar drift the 0009 header's founding lesson names.
--
-- LIVE-SAFETY: catalog-only. ADD COLUMN with a constant default on PostgreSQL 11+ rewrites no rows
-- (attmissingval); skill holds dozens of rows and nothing else is touched. The write path preserves a
-- stored non-empty description across the boot importer's idempotent re-upserts (core/db PutSkill),
-- so this column cannot be silently blanked by a restart.
ALTER TABLE skill
  ADD COLUMN description text NOT NULL DEFAULT '';
