-- 0010: the durable judge spine (task #26 / TG-37, spec/012 REQ-1106 + spec/014 REQ-1310).
--
-- session_triage is the Runner's compact terminal record — one row per triage session, written
-- best-effort at the workflow's terminal outcome and idempotent on external_ref (an activity retry can
-- never duplicate a session). session_judgment is the judge cron's output: one row per (session,
-- dimension), shaped to satisfy the queries core/db/skillstore_trial.go already runs against it
-- (armScoresForDim joins on external_ref + dimension with score > 0; JudgedSessionRate counts
-- DISTINCT external_ref by judged_at). skill_watch persists the post-graduation regression watch
-- (core/skillstore/graduation.go WatchState); open watches are the closed_at IS NULL rows.

CREATE TABLE session_triage (
    external_ref   text PRIMARY KEY,
    host           text NOT NULL DEFAULT '',
    alert_rule     text NOT NULL DEFAULT '',
    band           text NOT NULL DEFAULT '',
    outcome        text NOT NULL DEFAULT '',
    proposed       boolean NOT NULL DEFAULT false,
    op             text NOT NULL DEFAULT '',
    evidence_ids   text[] NOT NULL DEFAULT '{}',
    conclusion     text NOT NULL DEFAULT '',
    -- skill_loads: the composed-seed provenance strings verbatim (name@version#id:origin[:arm]) — the
    -- judge cron parses the store version ids out of them for the regression-watch feed.
    skill_loads    jsonb NOT NULL DEFAULT '[]',
    judged         boolean NOT NULL DEFAULT false,
    schema_version integer NOT NULL DEFAULT 1 CHECK (schema_version > 0),
    created_at     timestamptz NOT NULL DEFAULT now()
);

-- The judge cron's batch read: unjudged sessions in the trailing window, oldest first.
CREATE INDEX session_triage_unjudged ON session_triage (created_at) WHERE NOT judged;

CREATE TABLE session_judgment (
    id             bigserial PRIMARY KEY,
    external_ref   text NOT NULL,
    dimension      text NOT NULL,
    score          double precision NOT NULL,
    comment        text NOT NULL DEFAULT '',
    judged_at      timestamptz NOT NULL DEFAULT now(),
    schema_version integer NOT NULL DEFAULT 1 CHECK (schema_version > 0),
    -- One verdict per (session, dimension); a re-judge upserts, never duplicates.
    UNIQUE (external_ref, dimension)
);

-- JudgedSessionRate's trailing-window count (REQ-1309).
CREATE INDEX session_judgment_judged_at ON session_judgment (judged_at);

CREATE TABLE skill_watch (
    version_id    bigint PRIMARY KEY REFERENCES skill_version(id),
    skill_name    text NOT NULL,
    control_mean  double precision NOT NULL,
    min_lift      double precision NOT NULL,
    failures      integer NOT NULL DEFAULT 0,
    threshold     integer NOT NULL DEFAULT 5 CHECK (threshold > 0),
    -- dimension: the trial's target dimension — the judged score the watch compares (REQ-1310).
    dimension     text NOT NULL DEFAULT '',
    expires_at    timestamptz NOT NULL,
    -- prior_version: the version retired at graduation (0 = none) — restored on a trip.
    prior_version bigint NOT NULL DEFAULT 0,
    closed_at     timestamptz,
    close_reason  text NOT NULL DEFAULT ''
);
