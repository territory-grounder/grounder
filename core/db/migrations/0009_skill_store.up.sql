-- 0009: the versioned skill store + graduation flywheel (spec/014, REQ-1301/1302/1306).
--
-- Skills move from the compiled registry to versioned rows the console edits and the flywheel
-- graduates. The predecessor's forensics shaped this schema: statuses are CHECK-constrained (its trial
-- table accumulated out-of-enum rows via raw SQL), one-production and one-active-trial are PARTIAL
-- UNIQUE INDEXES (its supersede logic lived in application code and drifted), rationale is NOT NULL at
-- the row (its rationale lived in a sidecar JSON file that split-brained), and assignments are UNIQUE
-- on (external_ref, trial) with the arm persisted (its read-before-hash idempotency, made structural).

CREATE TABLE skill (
    name       text PRIMARY KEY,
    kind       text NOT NULL DEFAULT 'behavioral' CHECK (kind IN ('behavioral', 'catalog')),
    -- pinned: the store may NEVER override the compiled body (the conservative-remediation hard floor).
    pinned     boolean NOT NULL DEFAULT false,
    -- position: the deterministic compose order (mirrors the compiled registry order).
    position   integer NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE skill_version (
    id             bigserial PRIMARY KEY,
    skill_name     text NOT NULL REFERENCES skill(name),
    version        text NOT NULL,
    status         text NOT NULL DEFAULT 'draft'
                   CHECK (status IN ('draft', 'trial', 'production', 'retired', 'rejected')),
    body           text NOT NULL CHECK (octet_length(body) BETWEEN 1 AND 8192),
    -- applies_when: the declarative selection predicate ({"phases":[...],"exec_classes":[...]}; {} = always).
    -- Validated at write time against the known phases/classes; evaluated in Go as a pure function (INV-08).
    applies_when   jsonb NOT NULL DEFAULT '{}'::jsonb,
    content_hash   text NOT NULL,
    author         text NOT NULL,
    source         text NOT NULL,
    -- rationale: the append-only transition log — REQUIRED at creation and grown at every transition.
    rationale      text NOT NULL CHECK (length(rationale) > 0),
    parent_version_id bigint REFERENCES skill_version(id),
    eval_offline   jsonb,
    eval_online    jsonb,
    ledger_seq     bigint,
    schema_version integer NOT NULL DEFAULT 1,
    created_at     timestamptz NOT NULL DEFAULT now(),
    status_changed_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (skill_name, version)
);

-- Exactly one production version per skill — the supersede invariant, structural (REQ-1302).
CREATE UNIQUE INDEX skill_version_one_production ON skill_version (skill_name) WHERE status = 'production';

CREATE TABLE skill_trial (
    id                  bigserial PRIMARY KEY,
    skill_name          text NOT NULL REFERENCES skill(name),
    candidate_ids       bigint[] NOT NULL,
    -- control_version_id: the production version at trial start; NULL = the compiled body is the control.
    control_version_id  bigint REFERENCES skill_version(id),
    dimension           text NOT NULL,
    min_samples_per_arm integer NOT NULL DEFAULT 15 CHECK (min_samples_per_arm > 0),
    min_lift            double precision NOT NULL DEFAULT 0.05,
    p_threshold         double precision NOT NULL DEFAULT 0.1,
    ends_at             timestamptz NOT NULL,
    status              text NOT NULL DEFAULT 'active'
                        CHECK (status IN ('active', 'completed', 'aborted_no_winner', 'aborted_timeout', 'aborted_operator')),
    winner_version_id   bigint REFERENCES skill_version(id),
    winner_mean         double precision,
    winner_p            double precision,
    -- note: the trial's append-only transition/rationale log (start reason, refusals, finalize verdicts).
    note                text NOT NULL DEFAULT '',
    schema_version      integer NOT NULL DEFAULT 1,
    created_at          timestamptz NOT NULL DEFAULT now(),
    finalized_at        timestamptz
);

-- At most one active trial per skill (REQ-1306).
CREATE UNIQUE INDEX skill_trial_one_active ON skill_trial (skill_name) WHERE status = 'active';

CREATE TABLE skill_trial_assignment (
    id           bigserial PRIMARY KEY,
    external_ref text NOT NULL CHECK (length(btrim(external_ref)) > 0),
    trial_id     bigint NOT NULL REFERENCES skill_trial(id),
    variant_idx  integer NOT NULL CHECK (variant_idx >= -1),
    assigned_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (external_ref, trial_id)
);
