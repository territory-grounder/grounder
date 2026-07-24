# spec/014 — threat model (STRIDE slice)

The skill store is a **prompt-content control plane**: whoever writes it shapes what the agent is told.
The bounding fact: skill bodies change COMPETENCE, never AUTHORITY — bands, the never-auto floor, the
prediction gate, and the actuation interceptor are machine-enforced outside the seed (spec/001/002/013),
and mutation is OFF in this phase. A hostile skill body can degrade triage quality; it cannot mint
capability. INV-08/INV-11/INV-19/INV-22 referenced throughout.

## Spoofing
- **Write-route principal.** Draft/promote/retire require the operator-session principal (spec/006
  REQ-508); machine principals (HMAC) have no write route (REQ-1311). A stolen ingest credential
  therefore cannot touch skills.
- **Trial-arm identity.** Arms key on the ingest-validated `external_ref`; empty/whitespace refs are
  rejected pre-hash and counted (REQ-1306), so a forged blank identity cannot pool arms.

## Tampering
- **Prompt injection via the store** (the ClawHub/ToxicSkills class, ADR-0012). Mitigations: writes are
  authenticated + ledger-recorded with mandatory rationale (REQ-1301); a graduated body got there only
  through the offline gate + statistical trial (REQ-1307/1308); the pinned floor skill is not
  overridable (REQ-1305); and enforcement lives outside the seed entirely.
- **Raw-SQL state bypass** (the predecessor's out-of-enum forensics): CHECK constraints + the single
  `Transition` mutator make an out-of-band status row unrepresentable or loudly invalid (REQ-1301).
- **Ledger integrity.** Every transition appends to the hash-chained governance ledger (INV-19); a
  rewritten history breaks the chain.

## Repudiation
- Every version carries author, source, and a rationale log; every transition carries a ledger seq; the
  per-session skill_load record makes any composed seed byte-reconstructable (REQ-1303/1313). There is
  no anonymous path to production.

## Information disclosure
- Skill bodies contain operational guidance, never secrets (env:/file: refs only estate-wide; the write
  validator caps and stores plain markdown). Read routes require authentication like every route
  (spec/006). Eval scores and trial stats disclose no credentials.

## Denial of service
- **Compose path**: the store read is one snapshot per session with a TOTAL compiled fallback
  (REQ-1304) — a down store cannot stall triage.
- **Storage**: 8 KiB body cap, dedup on generation, ≤3 drafts per trigger, ≤2 concurrent trials; the
  generator is rate-bound by its cron cadence (REQ-1312).
- **Trial starvation as DoS-by-patience** (the predecessor's real failure): traffic-aware start refusal
  (REQ-1309) + dead-man staleness metrics keep un-completable trials from squatting the pipeline.

## Elevation of privilege
- **The model cannot promote itself**: generation is draft-only (REQ-1312); admission is a deterministic
  eval run (REQ-1307); graduation is a statistical function + typed guards (REQ-1308); selection is a
  pure typed-signal predicate (REQ-1303). No model token anywhere becomes control flow (INV-08).
- **A regressing graduate is bounded**: the post-graduation breaker auto-retires and restores
  (REQ-1310), so even a skill that passed its trial cannot silently persist degradation.
- **Write-tier hardening path**: the write surface moves to the admin session tier when #27 Phase B
  lands (noted in design; the routes are registered so the tier swap is a constant change).
