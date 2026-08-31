<!-- spec/028 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/028 — Threat model: the earned op-class catalog (STRIDE slice)

Per-feature slice; the system-wide model is [`docs/THREAT-MODEL.md`](../../docs/THREAT-MODEL.md).

**Trust boundary.** This spec is where model-shaped desire becomes operator-granted capability. The
ASSET is the actuation catalog itself: a wrongly-admitted overlay row is a standing capability, not
a one-off action. Three boundaries stack: model free-text → candidate evidence (untrusted →
recorded); candidate → ratified template (recorded → executable-on-approval, crossed ONLY by
operator authorship); ratified → auto rungs (executable → autonomous, crossed only by verified
evidence).

| STRIDE | Threat | Mitigation |
|---|---|---|
| Spoofing | Model manufactures recurrence (re-proposing the same incident) to force candidacy | Occurrence PK (candidate_key, external_ref): one incident counts once (REQ-2802); distinct-ref + multi-host/span thresholds (REQ-2811); confidence is a bar, never a weight |
| Tampering | Direct edit of an overlay row after ratification | entry_hash chain-covered in the ratify GovDecision; re-verified at every load; mismatch drops the row loudly — fail closed to FEWER capabilities (REQ-2815); UPDATE/DELETE revoked (REQ-2803) |
| Tampering | Prompt-injection launders a model command string into the executed template | Structurally empty form (no prefill code path); laundering tripwire refuses byte-matching templates; error-returning admission validation (REQ-2813/2814) |
| Repudiation | "Who granted this capability, on what evidence?" | Every transition ledgered with rationale + dossier_hash snapshot on the ONE chain (REQ-2817); revocation is a new row, never an erasure |
| Information disclosure | Dossiers exhibit raw model text | screen.Scrub on every exhibit; prediction matrices display-only; INV-13 projections |
| Denial of service | Candidate flood buries the operator; a dead cron silently stops the pipeline | Mechanical thresholds + 60d expiry + 30d dismiss TTL bound the queue; the cron dead-man refuses loudly on stale intake computed from tables it does not write (REQ-2812) |
| Elevation of privilege | A class climbs beyond its evidence | Terminus-only exactly-once credit (graduation_credit); AutoEligible(tier) before earned level; durable-or-refused promotion; overlay ceiling at AUTO_NOTICE — the silent rung requires the embedded lockstep-hashed registry via an embed-export code release (REQ-2807/2808); deviation → full drop via the may-demote-never-promote hook (REQ-2810) |
| Elevation of privilege | Forecast-lane verdicts (never executed) laundered into run evidence | INV-10 one-writer-per-meaning: only OutcomeFromVerdict feeds the ladder; forecast verdicts render only (REQ-2810) |

**Residual risk.** An operator who authors a bad template with full authority — mitigated by
admission validation, tier-vs-destructiveness contradiction refusal, blast-radius rendering at
ratify, and the AUTO_NOTICE ceiling keeping a human page in the loop until the class survives an
embed-export review. INV references: INV-08, INV-10, INV-14, INV-19, INV-22.
