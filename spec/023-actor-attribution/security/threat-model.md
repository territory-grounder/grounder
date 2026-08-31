<!-- spec/023 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/023 — Threat model (STRIDE)

STRIDE slice for actor-attribution grounding. The driving adversary is an intruder on an estate host
whose changes TG would otherwise auto-heal over (destroying forensics), plus the inverse adversary: one
who poisons attribution to make TG stand down or to smuggle a hostile change through as "authorized".

## Assets
- The taxonomy→disposition mapping and carve-out rows (data that shapes stand-down/heal behavior).
- The reader credentials (read-only, but they see audit trails — reconnaissance value).
- The attribution record on the ledger/triage row (forensic value; must be trustworthy).
- The audit trails themselves (external systems of record TG reads but does not own).

## Threats → mitigations (INV references)

- **Spoofing — forge "authorized" to smuggle a hostile change past TG.** An attacker who can write a
  domain's audit trail could dress an intrusion as a sanctioned principal's change, making TG stand
  down instead of escalate. *Mitigations:* `attributed-authorized` requires a match against the
  `sanctioned_principals` data rows (never "any named actor"); the disposition is stand-down-to-human
  (POLL_PAUSE) — a human sees it, TG never silently accepts it; and REQ-2305 means a forged
  attribution can at most *withhold* TG's heal, never make TG act (INV-09). An attacker with
  audit-trail write access on the domain controller is outside this spec's trust boundary and is
  named residual risk.

- **Spoofing — forge "attributed-self" to suppress healing (heal-suppression DoS).** Faking TG's own
  token in a task log would no-op real heals. *Mitigations:* self-recognition prefers TG's OWN
  append-only governance ledger (a `verified_clean` actuation record) as the authoritative source
  (design §1b); the ledger is no-UPDATE/no-DELETE (INV-19) and outside the estate attacker's reach;
  a task-log-only self claim with no ledger correlate is a contradiction → escalate-to-human
  (REQ-2310), never a silent no-op.

- **Tampering — delete audit entries to hide an actor.** *Mitigation:* deletion yields absence, and
  absence is `unattributable` (REQ-2303) — the pre-feature governed ladder, no autonomy gained or
  lost; where the reader affirmatively covers the trail (`Covered=true`), an observed mutation with
  no entry is the REQ-2304 suspicious signal — tampering *raises* scrutiny rather than lowering it.

- **Tampering — poison the mapping/carve-out data.** A hostile mapping row could route `suspicious`
  to `ladder-unchanged`. *Mitigations:* the section lives in the versioned, audited ruleset store
  (spec/015 pattern — authenticated writes, ledger-recorded versions); the disposition enum is closed
  at load time; and the worst reachable state is still fully governed: every disposition either
  withholds actuation or leaves the six-gate stack intact — no mapping value can express "actuate
  more" (REQ-2305 is enforced in code, not data).

- **Repudiation.** Every attribution outcome, matched rule id, and evidence reference is on the
  `session_triage` row and the governance ledger signals (REQ-2311, INV-19) — the stand-down, the
  no-op, and the escalation are all reconstructable, and the carve-out leaves an honest
  `authorized-test` trail distinguishing manufactured faults from organic ones.

- **Information disclosure — readers surface sensitive audit/log content.** Journal and auth logs
  carry usernames, source IPs, and command lines. *Mitigations:* evidence records are minimized to
  typed fields before persist, carry retention bounds, and pass `core/screen` scrubbing at every log,
  export, and model-bound boundary (REQ-2313, INV-13/INV-14); reader credentials are read-only,
  least-privilege, `SecretRef`-resolved (REQ-2306, spec/022 substrate).

- **Denial of service — a slow or dead audit endpoint stalls triage.** *Mitigation:* bounded
  per-reader timeout; a reader failure is absent evidence plus a recorded warning, never a blocked
  session (REQ-2307) — the advisory lane fails OPEN by constitutional design (§3.3), and the failure
  is loud (logged + signal-recorded, INV-21), never a swallowed exception.

- **Elevation — use attribution to widen autonomy.** The structural counter is REQ-2305 plus the
  closed disposition enum: no taxonomy value, mapping row, or carve-out can lift the never-auto
  floor, flip the mode, or skip the policy verdict; `authorized-test` merely *declines to stand
  down*, and the action still traverses ACL · min_confidence · band · graduation · mode · floor.

- **Injection — evidence text steers the model.** Reader-captured actor strings and refs are
  attacker-influenceable (a hostile actor can choose a process name or MR title). *Mitigations:* the
  taxonomy is computed from typed fields by deterministic code (REQ-2300/REQ-2312 — INV-08: model
  output and untrusted text never become control flow); evidence rendered into the agent seed is
  delimited data and input-screened like precedent/CMDB blocks (REQ-2313).

## Residual risk
An adversary with write authority over a domain's system-of-record audit trail (e.g. root on the PVE
node writing the task log) can shape what TG *reads*; TG's ceiling of exposure is a stand-down or an
escalation seen by a human — never an actuation the six-gate stack would have refused. Conversely, a
sustained reader outage degrades the platform to pre-feature behavior (no attribution), which is
today's governed baseline: the capability adds a detection surface and removes none.
