-- 0081 down: restore the 0075 column comment (the pre-TG-444 label that reads as live confidence).
COMMENT ON COLUMN edge_disproof.decayed_to IS 'the edge confidence AFTER this pass''s decay';
