-- Revert 0075: drop the durable per-edge disproof record. The learned edges themselves are unaffected.
DROP TABLE IF EXISTS edge_disproof;
