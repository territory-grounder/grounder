<!-- spec/023 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/023 — Actor-attribution grounding (WHO is the actor behind the observed change?)

**Owning behavior family:** BEH-10 (Actor attribution — the "who did this?" dimension of triage).
**Constitution / invariants:** INV-04, INV-05, INV-08, INV-09, INV-11, INV-13, INV-14, INV-17, INV-18, INV-19, INV-20, INV-21, INV-22.
**Phase:** Phase 2 (governed actuation — composes over the six-gate stack; the readers are Phase-1-safe read-only).
**Status:** Phase 1 landed + armed live (T-023-1..7). Phase 2 (T-023-8..11) generalises attribution beyond
Proxmox — additional read-only action-evidence readers plus a structurally-distinct identity/auth
enrichment seam (REQ-2314..2321); spec authored, implementation pending.

Before Territory Grounder proposes or actuates anything, it currently asks *what* changed, *how risky*
the fix is, and *whether* it has earned the right to act. It never asks **who made the observed change**.
That gap has three concrete failure classes, all live on the estate today:

1. **Undoing intentional work.** An admin stops a guest for maintenance; TG's heal ladder sees
   "guest-down" and restarts it under the sanctioned `start-guest` op-class — reversing a deliberate,
   authorized change.
2. **Redundant re-actuation of the already-healed.** A stale or re-fired alert re-triggers triage for a
   fault TG itself remediated minutes earlier; TG re-executes the heal it already verified, spending an
   actuation, a poll, or an approver's attention on nothing.
3. **Healing over an intrusion.** A change made by an unknown actor — or a mutation with no
   corresponding audit-trail entry — is the signature of a compromise. Auto-healing it restores service
   while *masking the intrusion and destroying forensic state*; the correct behavior is a loud human
   escalation, never a quiet fix.

This spec adds one typed triage dimension — **`ActorAttribution`** — derived by deterministic code from
per-domain read-only **actor-evidence readers**, mapped to a disposition by **rules-as-data** in the
versioned ruleset store, recorded on the governance ledger and `session_triage`, and surfaced by the
decision tracer. The structural safety property is REQ-2305: attribution can only **stand down, no-op,
or escalate** — it can never raise autonomy, lift the never-auto floor, or bypass the mode chokepoint.

Evidence *collection* is advisory-lane context and fails OPEN (a dead reader degrades to pre-feature
behavior — Constitution §3.3); the *dispositions* act only on the remediation lane and only in the
restrictive direction, so the fail-open collection can never open a mutation path. This is the same
composition the canary pin (spec/001 REQ-009) and the high-risk-category default already use: a signal
that can raise review, never lower it.

## Requirements

- **REQ-2300** — [F] owner epic: actor-attribution grounding · [O] INV-06/INV-08/INV-19.
  WHEN a session reaches risk classification, the system SHALL carry a typed `ActorAttribution` value
  from the closed enumeration {`unattributable`, `attributed-authorized`, `attributed-self`,
  `attributed-suspicious`, `authorized-test`} into the classifier input, derived by deterministic code
  from reader-captured evidence records — never from model narrative — and SHALL record the value and
  its evidence references in the classification signals.

- **REQ-2301** — [F] owner epic · [O] INV-09/INV-19.
  IF the fault-shaped change is attributed to a sanctioned principal that is not the platform's own
  actuation identity and no carve-out rule (REQ-2309) matches, THEN the system SHALL NOT propose or
  execute an actuation that reverses that change, and SHALL classify the session **POLL_PAUSE** routed
  to the approver graph with the attribution and its evidence recorded — coordinate with the actor,
  never undo an intentional change.

- **REQ-2302** — [F] owner epic · [O] INV-11/INV-19 · [R] paradigm-rule 5.
  IF actor evidence shows the platform's own actuation identity applied a remediation to the same
  target and fault class within the configured self-recognition window, THEN the system SHALL
  terminate the session with the terminal outcome `already-remediated` without proposing a new
  actuation, SHALL write the `session_triage` row and the governance `Decision` record with that
  outcome and the evidence references, and SHALL NOT re-execute the remediation.

- **REQ-2303** — [F] owner epic (evidence-gated honesty).
  WHEN no admissible actor evidence exists for the investigated subject — including when no reader is
  loaded for its domain — the attribution SHALL be `unattributable` and the classification and heal
  ladder SHALL proceed exactly as they would without this capability; the band SHALL NOT change on
  account of an absent attribution.

- **REQ-2304** — [F] owner epic · [O] INV-09 (fail toward the human on a security signal).
  IF admissible evidence positively identifies an unsanctioned actor for the observed change, OR a
  domain reader that affirmatively covers the target's audit trail returns that trail with no entry
  corresponding to an observed mutation, THEN the classifier SHALL emit **POLL_PAUSE** with
  `security_escalation` recorded in its signals, the system SHALL NOT auto-heal the change, and the
  notification SHALL route to the org-configured security channel in addition to the approver graph.

- **REQ-2305** — [O] INV-09/INV-21 · [R] paradigm-rule 8.
  The attribution dimension SHALL only stand down, no-op, or escalate a session; it SHALL NOT raise
  autonomy, lift the never-auto floor, bypass the mode chokepoint, or substitute for the policy
  verdict, graduation, ACL, band, or min-confidence gates — an `authorized-test` outcome traverses
  every existing gate unchanged.

- **REQ-2306** — [R] paradigm-rule 3/9 · [O] INV-13/INV-17/INV-18.
  Actor evidence SHALL be collected only by per-domain read-only reader modules behind one shared
  `adapters/actorevidence` interface, each compiled in, config-gated, and explicitly registered at
  startup, each authenticating with a least-privilege read-only credential resolved as a
  `core/config.SecretRef`; the first reader SHALL be the Proxmox VE task-log reader.

- **REQ-2307** — [F] owner epic (evidence-gated honesty) · [O] INV-21.
  IF a reader returns an error or exceeds its bounded timeout, THEN its evidence SHALL be treated as
  absent for that session, a warning SHALL be logged and recorded in the session signals, and the
  triage SHALL proceed; a reader failure SHALL NOT fail or block the session and SHALL NOT by itself
  produce `attributed-suspicious`.

- **REQ-2308** — [F] prose-loadable-not-hardcoded · [R] paradigm-rule 4 · [O] INV-09/INV-19.
  The taxonomy-to-disposition mapping SHALL be loadable data in the versioned ruleset store — a closed
  disposition enumeration {`ladder-unchanged`, `stand-down-coordinate`, `self-noop`,
  `security-escalate`} validated at load time — never a compiled constant; IF the mapping is absent,
  corrupt, or fails validation, THEN every taxonomy value except `unattributable` SHALL resolve to
  **POLL_PAUSE** (escalate to the approver graph) and `unattributable` SHALL resolve to
  `ladder-unchanged`.

- **REQ-2309** — [F] owner epic (sanctioned-pool carve-out) · [O] INV-20.
  WHERE a currently-valid sanctioned-pool carve-out rule — a temporally-bounded data row carrying
  `valid_from`, `valid_until`, an actor-pattern set, and a host allowlist — matches the fault's
  attributed actor and target host, the attribution SHALL resolve to `authorized-test`, the heal
  ladder SHALL proceed unchanged, and the attribution SHALL be recorded on the `session_triage` row;
  an expired, future, or invalid carve-out rule SHALL NOT match.

- **REQ-2310** — [F] owner epic (evidence-gated honesty) · [O] INV-09.
  IF admissible evidence supports two or more contradictory taxonomy values, none of which is
  `attributed-suspicious`, for the same observed change, THEN the attribution SHALL resolve to
  **POLL_PAUSE** (escalate to the approver graph) with every candidate value and its evidence
  recorded; WHERE one candidate is `attributed-suspicious` on positive evidence, REQ-2304 governs.

- **REQ-2311** — [O] INV-19 · [F] composes spec/020 (decision tracer).
  Every classification SHALL persist its attribution outcome — the taxonomy value, the matched
  mapping or carve-out rule id where one matched, and the evidence references — on the `session_triage`
  row and in the governance `Decision` record's signals, and the decision tracer SHALL surface the
  attribution as a named step in the session's trace.

- **REQ-2312** — [O] INV-04/INV-11.
  An attribution claim SHALL be admissible only when backed by at least one reader-captured evidence
  record that is timestamped inside the configured attribution window and names the investigated
  target; agent free-text SHALL NOT set or alter the taxonomy value.

- **REQ-2313** — [O] INV-08/INV-13/INV-14.
  A persisted actor-evidence record SHALL carry an `expires_at` retention bound and SHALL be redacted
  at every log and export boundary; evidence rendered into an agent seed SHALL be delimited as data,
  never instructions, and SHALL pass the input screen before rendering.

<!-- Phase 2 (REQ-2314..2321): generalise attribution beyond Proxmox — more action-evidence readers +
     a structurally-distinct identity/auth enrichment seam. Design gate: spec/023-actor-attribution/design.md §10. -->

- **REQ-2314** — [F] owner epic (generalise beyond PVE) · [R] paradigm-rule 3/9 · [O] INV-02/INV-13/INV-17.
  The system SHALL provide additional read-only action-evidence readers — beginning with a
  systemd-journal / sudo / auditd reader — that implement the unchanged `adapters/actorevidence.Reader`
  seam, read privileged-action records over the native host-key-verified read-only SSH runner with a
  fixed argument vector and no shell, authenticate through the credential engine's per-host
  least-privilege read-only identity gated by an operator allowlist and a mandatory known-hosts file
  (an unset known-hosts file SHALL fail closed with no trust-on-first-use), deterministically parse each
  record into an `Evidence` value naming the read host as `Target`, and SHALL NOT surface raw log text
  to the model.

- **REQ-2315** — [F] owner epic (identity dimension) · [O] INV-04/INV-11/INV-19.
  The identity/auth dimension SHALL be a seam structurally distinct from action-evidence: an identity
  resolver SHALL produce a per-principal enrichment value keyed by domain and actor that names no
  target, action, or time, SHALL NOT be admissible as an `Evidence` record, and SHALL NOT by itself
  set a taxonomy value. Taxonomy authority SHALL remain solely with action-evidence readers; identity
  enrichment MAY only refine the classification of an actor already captured by an admissible,
  target-naming, in-window action-evidence record.

- **REQ-2316** — [O] INV-19/INV-21/INV-22 · [R] paradigm-rule 8.
  The identity enrichment SHALL execute only in the side-effecting attribute activity over a per-session
  copy of the attributor configuration — never in the deterministic core attributor, which SHALL remain
  unchanged, and never in the workflow body. Each directory query SHALL be bounded by a compiled timeout
  and a result-size ceiling and SHALL reuse the credential source's LDAPS transport, certificate-authority
  verification, secret-reference binds, and ordered replica failover.

- **REQ-2317** — [F] owner epic (dynamic sanctioning) · [O] INV-09/INV-20 · [R] paradigm-rule 4.
  WHERE a fresh directory fact affirmatively confirms an actor named by an admissible action record is
  an enabled, non-service member of the domain's configured sanctioned admin group — a loadable
  rules-as-data set validated at load time — the enrichment MAY reclassify that actor's record from
  `attributed-suspicious` to `attributed-authorized` (stand-down); the enrichment SHALL NOT move the
  classification beyond stand-down-coordinate, and a live promotion SHALL be equivalent to, and as
  auditable as, a statically-sanctioned entry.

- **REQ-2318** — [F] owner epic (compromised-credential detection) · [O] INV-09.
  IF a fresh directory fact affirmatively reports that a statically-sanctioned or live-promoted principal
  named by an admissible action record is locked, disabled, expired, or a service principal, THEN the
  enrichment SHALL reclassify that actor's record to `attributed-suspicious` (security-escalate) — a
  disabled or decommissioned admin credential in use is a compromise indicator. This demotion SHALL fire
  only on affirmative positive evidence for an actor already named by an admissible action record, and
  SHALL NOT be produced by the absence of a fact, an ambiguous result, or a resolver error.

- **REQ-2319** — [F] owner epic (evidence-gated honesty) · [O] INV-21 · [R] paradigm-rule 8.
  IF an identity resolver returns an error, exceeds its timeout, resolves ambiguously, or yields no
  affirmative fact, THEN the enrichment SHALL degrade to absent — the per-session configuration copy
  SHALL equal the static sanctioned list (no promotion and no demotion), reproducing the Phase-1
  classification. A dead or unreachable resolver SHALL NOT by itself produce `attributed-suspicious` and
  SHALL NOT grant a stand-down; a directory outage SHALL at most withhold a promotion or forgo a
  demotion, never invert either — so a directory restart can never escalate every session.

- **REQ-2320** — [O] INV-08/INV-13/INV-14.
  Identity enrichment values and journal-derived evidence carrying identity data — usernames, group
  common-names, distinguished names, commands, and session or network-access-server identifiers — SHALL
  be retention-bounded with an `expires_at` and redacted at every log and export boundary; resolver
  warnings SHALL carry the provider and domain only, never the principal or its group memberships; and
  the classification signal SHALL record only the resolved taxonomy value, never the raw principal.

- **REQ-2321** — [F] owner epic (scope discipline) · [O] INV-04/INV-11.
  RADIUS and any authentication-event source SHALL enter only as an advisory `auth-observed` correlation
  on the identity seam, never as a taxonomy-minting `Evidence` reader and never as a classification
  mover, because such a record names a network-access-server or login rather than the mutated target and
  cannot satisfy REQ-2312 admissibility. A missing authentication event SHALL NOT move a classification —
  absence is not admissible proof of absence, since key-based, console, and local logins bypass RADIUS;
  any future promotion of an `auth-observed` annotation to a classification mover SHALL be gated on
  proven affirmative authentication-path coverage.
