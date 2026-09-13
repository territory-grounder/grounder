-- Down-migration for 0001 baseline. Every up-migration ships with a down-migration (P0-8 CI gate).
DROP TABLE IF EXISTS action_manifest;
DROP TABLE IF EXISTS auth_nonce;
DROP TABLE IF EXISTS sources;
DROP TYPE IF EXISTS verdict;
DROP TYPE IF EXISTS band;
