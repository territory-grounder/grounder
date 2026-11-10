-- 0117: harness_render — the rendering behind each harness fingerprint (TG-600, Harness² learnings H2-1c;
-- spec/020 REQ-2025).
--
-- 0115 gave every session its configuration identity (session_triage.harness_fp) and nothing else: the
-- rendering was hashed at compose time and discarded, so a row could say "same configuration or not",
-- never what it was. This table keeps, once per distinct fingerprint, the four EFFECTIVE components the
-- fingerprint hashes — the protocol template, the rendered tool catalog, the op-class catalog (registry +
-- ratified overlay, or the day-zero posture text) and the guidance half as the model received it — plus a
-- SHA-256 per component so "which part moved" is a column comparison. core/harnessid.Assemble turns the
-- four parts back into the exact system message, and the store REFUSES any row whose key does not derive
-- from its parts under its preamble version (a rendering stored under the wrong fingerprint would poison
-- every later diff). harness_render_build records which worker builds rendered a fingerprint (the binary's
-- own vcs.revision), so "which image was running" is a join, not deploy-history archaeology.
--
-- Written on a configuration's FIRST session by the investigate activity (either plane may run it:
-- plane: both); INSERT-only under the 0105 floor (ON CONFLICT DO NOTHING — a rendering never changes
-- under its key). NON-SECRET by construction: the system half carries no incident data (the seed is hashed
-- separately), and the guidance half is the same skill-store text already held in this database.
-- Cardinality: one row per deploy that changes the surface. Nothing on the decision path reads it.
CREATE TABLE harness_render (
    harness_fp      text PRIMARY KEY CHECK (harness_fp ~ '^[0-9a-f]{64}$'),
    prompt_version  text NOT NULL CHECK (prompt_version <> ''),
    prose           text NOT NULL CHECK (prose <> ''),
    tool_catalog    text NOT NULL,
    opclass_catalog text NOT NULL,
    guidance        text NOT NULL,
    prose_hash      text NOT NULL CHECK (prose_hash ~ '^[0-9a-f]{64}$'),
    tools_hash      text NOT NULL CHECK (tools_hash ~ '^[0-9a-f]{64}$'),
    opclass_hash    text NOT NULL CHECK (opclass_hash ~ '^[0-9a-f]{64}$'),
    guidance_hash   text NOT NULL CHECK (guidance_hash ~ '^[0-9a-f]{64}$'),
    first_seen      timestamptz NOT NULL DEFAULT now(),
    CHECK (octet_length(prose) + octet_length(tool_catalog) + octet_length(opclass_catalog) + octet_length(guidance) <= 1048576)
);
COMMENT ON TABLE harness_render IS 'plane: both';

CREATE TABLE harness_render_build (
    harness_fp text NOT NULL REFERENCES harness_render(harness_fp),
    build_sha  text NOT NULL,
    first_seen timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (harness_fp, build_sha)
);
COMMENT ON TABLE harness_render_build IS 'plane: both';
