-- Reverse 0018: drop the table (which removes the table and every grant/revoke scoped to it in one step —
-- the REVOKE in the up is created and destroyed with the table, so no separate GRANT restore is needed).
DROP TABLE IF EXISTS credential_resolution;
