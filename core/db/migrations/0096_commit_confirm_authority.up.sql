-- spec/029 T-029-3 (REQ-2901/2903): the fired inverse's AUTHORIZATION BASIS, captured durably AT
-- ARM TIME. The inverse re-enters the full interceptor chain as a first-class mutation; the basis
-- it carries is the FORWARD action's own live band and recorded approval — an auto-admitted heal's
-- revert rides the same envelope that admitted the heal, while a poll-approved forward extends its
-- vote to the armed revert the approved ceremony declared. The chain still judges everything fresh
-- (mode, floors, territory, necessity), so a basis the chain rejects refuses and PAGES (REQ-2903).
-- alert_rule carries the incident signature the REQ-2902 hold-watch re-observes the target with.
ALTER TABLE commit_confirm
    ADD COLUMN forward_band     TEXT    NOT NULL DEFAULT '',
    ADD COLUMN forward_approved BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN alert_rule       TEXT    NOT NULL DEFAULT '';
