-- TG-664 (G4A F6): a commit-confirm window whose resource was moved by a LATER execution resolves `superseded`.
-- Its armed inverse would undo that later action (a stale stop-guest killing a unit heal inside the guest), so the
-- inverse never fires: the window feeds the ladder nothing, pages, and is not a revert failure (no breaker trip).
ALTER TABLE commit_confirm DROP CONSTRAINT commit_confirm_state_check;
ALTER TABLE commit_confirm ADD CONSTRAINT commit_confirm_state_check CHECK (state IN
    ('armed','aborted','elapsed_unconfirmed','confirmed','held_unverifiable','reverted','revert_failed','superseded'));
