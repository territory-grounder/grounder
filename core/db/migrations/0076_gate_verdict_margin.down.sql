-- Down 0076: drop the per-gate decision margin column (TG-178).
ALTER TABLE interceptor_gate_verdict DROP COLUMN margin;
