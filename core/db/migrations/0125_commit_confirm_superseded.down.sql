-- Restores the pre-TG-664 state vocabulary. Fails (by design) while any row is `superseded`: such a row is history
-- and must not be rewritten to fit the narrower check.
ALTER TABLE commit_confirm DROP CONSTRAINT commit_confirm_state_check;
ALTER TABLE commit_confirm ADD CONSTRAINT commit_confirm_state_check CHECK (state IN
    ('armed','aborted','elapsed_unconfirmed','confirmed','held_unverifiable','reverted','revert_failed'));
