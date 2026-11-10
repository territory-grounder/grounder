ALTER TABLE session_judgment
    DROP COLUMN IF EXISTS judge_sampling,
    DROP COLUMN IF EXISTS judge_model;
