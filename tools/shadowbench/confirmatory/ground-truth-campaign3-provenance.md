# Campaign #3 — ground-truth provenance & verdict

**Verdict (frozen `analyze.py`, PRE-REGISTRATION §7): TG MATCHES (non-inferior).**
Owner ruling 2026-08-26: **Accept MATCHES** — TG graduates; the predecessor moves to STANDBY
(Phase-4 pre-authorized arms fire; full retire remains owner-gated, Bucket D).

## Result
- PRIMARY (judge-free, injector ground truth): TG 12/32 correct, predecessor 4/32.
  TG heals device-down (8x start-guest) and correctly stands down on disk-fill (4x); the predecessor
  stood down (band POLL_PAUSE) on ALL 32, correct only on the 4 disk-fill (inverted polarity).
  McNemar exact on the 30-pair confirmatory population: TG-only 7, pred-only 0, p=0.016.
  Host-clustered rate difference +0.23 (CI +0.088..+0.423) — non-inferior (margin -0.10).
- SECONDARY (judged rubric): the predecessor is significantly stronger on correct_diagnosis,
  evidence_grounded, sensible_proposal, appropriate_band (counter-leg Holm p <= 0.0006, favours pred).
  The frozen §7 rule therefore blocks EXCEEDS; the composite is MATCHES.

## Ground-truth build (no automated builder exists; hand-assembled per PRE-REGISTRATION §1 rules)
- TG scored per injected fault over its live window [injected_at, restored_at], replicating
  `core/diagcorpus` ScoreFault on `session_triage.op_class` (any-correct for healable classes;
  all-decline for the disk-fill stand-down class). Reproducible via `cmd/diagscore`.
- Predecessor scored from its full `claude-run-<issue>.jsonl` triage conclusion + risk-audit band
  (POLL_PAUSE = stand-down); `sessions.last_response_b64` is a truncated preview and was NOT used.
- Cross-checked by three independent blind scorers. TG=12/32 was UNANIMOUS across all three plus the
  mechanical replay. The predecessor diverged on exactly two pairs (openwebui01 service-down): the
  committed GT takes the strict/symmetric reading (the predecessor stood down on the INJECTED :80 fault;
  its restore proposal targeted a DIFFERENT real :8080 outage) -> pred 4/32.
  `ground-truth-campaign3-sensitivity.json` is the lenient reading (pred 6/32); both yield MATCHES.

## Caveats (recorded, not hidden)
- Meta-contamination: the predecessor explicitly recognised 31/32 faults as synthetic — it read the
  injector `manifest.jsonl` and the "TG P4 ...target" `service_desc` names and noted the ~20-minute
  self-clear. Broader than PRE-REGISTRATION §5's "recognises hosts by name"; it does not change the
  composite (already only MATCHES) but is flagged for the record.
- Timing: TG triaged in-window on 31/32; the predecessor correlated 32 faults into 13 incidents whose
  open-timestamps mostly precede/follow the individual fault windows (its triages still covered them).
