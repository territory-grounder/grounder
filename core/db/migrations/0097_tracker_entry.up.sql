-- TG-490: TG's OWN entry tickets. One row per alert-sourced incident (external_ref) that TG has
-- filed into its ticket tracker — the deterministic WRITE half the incumbent does agent-side and
-- TG deliberately does NOT (INV-08: no model token reaches an effect path; the renderer is pure
-- data from the ingest record). The row is the idempotency key (a re-fired alert never files
-- twice), the join the entry-tracker seam resolves TG's own tickets through, and the cursor the
-- recovery-comment pass advances (last_comment_transition_id against ingest_transition.id).
CREATE TABLE tracker_entry (
    external_ref               TEXT        NOT NULL PRIMARY KEY,
    issue_id                   TEXT        NOT NULL,  -- the tracker's readable id (e.g. TGOPS-123)
    project                    TEXT        NOT NULL,
    source_type                TEXT        NOT NULL DEFAULT '',
    created_at                 TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_comment_transition_id BIGINT      NOT NULL DEFAULT 0
);

COMMENT ON TABLE tracker_entry IS 'plane: both';
