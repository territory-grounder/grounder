<!-- spec/001 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/001 — Three-band risk classification

**Owning behavior family:** BEH-1 (see [`docs/GOVERNED-BEHAVIORS.md`](../../docs/GOVERNED-BEHAVIORS.md)).
**Constitution / invariants:** INV-06, INV-07, INV-09, INV-10, INV-11.
**Phase:** classifier behavior lands in Phase 2; the mechanical fail-closed primitives land in Phase 0
(`core/safety`). **Status:** Approved.

The `RiskClassifier` is a typed, deterministic admission gate that writes exactly one
`session_risk_audit` row per classification and emits one of three autonomy bands —
**AUTO / AUTO_NOTICE / POLL_PAUSE**. Its zero value and every error path fail closed to POLL_PAUSE by
construction. This document is the requirement source of record; the design is in `design.md`, the
runnable acceptance oracles are in `acceptance/`, and the engineering tasks are in `tasks.json`.

## Requirements

- **REQ-001** — [F] spec/001 · [R] paradigm-rule 2.
  WHEN a session is admitted for a low-risk or reversible-and-prediction-eligible action that is not on
  an organization-designated criticality-tier host and whose predicted blast-radius is below the
  configured threshold, the classifier SHALL emit band **AUTO** and mark the proposal for `[AUTO-RESOLVE]`.

- **REQ-002** — [F] spec/001 · [R] paradigm-rule 3.
  WHEN the action is reversible-mixed on a criticality-tier host, or the predicted blast-radius is wide,
  the classifier SHALL emit band **AUTO_NOTICE**, proceed with `[AUTO-RESOLVE]`, and set
  `notify_required = true` so the on-call group receives an out-of-band veto notice in parallel.

- **REQ-003** — [F] spec/001 · [O] INV-09.
  WHEN the action is high-risk, irreversible, unpredicted, a verification deviation, or a
  novel-incident class (`ood:novel-incident`), the classifier SHALL emit band **POLL_PAUSE**, mark the
  proposal `[POLL]`, hold on durable pause/resume state, notify the approver graph, and SHALL NOT
  proceed on timeout.
  The **novelty signature** SHALL be `(incident subject host, canonical rule family)`, where the incident
  subject host is the ingest-validated alerted device (`env.Host`) — the SAME identity the confirmed-clean
  novelty writeback records and the retrieval/precedent plane queries. The LLM-expressed action target
  (which may name the guest or its hypervisor for the same fault) SHALL be consulted only as a LEGACY
  compatibility key, so a precedent under either key de-novels the incident; the subject key is the one
  that transfers across proposals. An incident is novel only when EVERY consulted key is a known-zero
  count; an unknown count (no knowledge store) SHALL be treated as NOT novel (novelty-unknown ⇒ do not
  fire), never inventing a poll from missing data.

- **REQ-004** — [F] spec/001 · [O] INV-09/INV-10 · [R] paradigm-rule 8.
  The classifier SHALL apply the inviolable mechanical NEVER-auto floor as a non-configurable
  precondition that clamps the band to **POLL_PAUSE** — regardless of confidence, organization policy, or any
  flag — for any action in the irreversible class (`mkfs`, `dropdb`, `zpool`/`zfs destroy`,
  `terraform`/`tofu destroy`, `kubectl delete`/`drain`, credential-revoke, reboot/halt, config-file
  overwrite, criticality-tier reboot, confirmed jailbreak). An unrecognized mutation SHALL be treated
  as never-auto (unknown action-class implies the never-auto ceiling), never as safe by omission.

- **REQ-005** — RETIRED. [R] paradigm-rule 4/7.
  The predecessor's "byte-identical legacy output while the autonomy sentinel is absent" behavior does
  not exist in Territory Grounder; autonomy is an org-global RBAC-gated audited feature-flag whose off-state is
  the non-autonomous read-only baseline, not a legacy code path.

- **REQ-006** — [F] spec/001 · [O] INV-06/INV-09.
  IF the model output is unparseable, non-manifest-expressible, or ambiguous, the classifier SHALL fail
  closed to **POLL_PAUSE** as its only defined behavior — the `Band` enum zero value is the
  most-restrictive band, so any error, panic, or unmatched path is fail-closed by construction — and
  the output SHALL NOT be routed through a looser fallback grammar.

- **REQ-007** — [F] spec/001.
  WHEN an incident class has no learned prior (no prediction-eligible history for this
  `(alert_rule, host)`), the classifier SHALL fail closed to **POLL_PAUSE**.

- **REQ-008** — [F] spec/001 · [O] INV-11.
  WHEN the `silent_cognition_guard` policy is active, the classifier SHALL strip any `[AUTO-RESOLVE]`
  marker whose response lacks a bound post-state evidence block and downgrade the session to a poll,
  where evidence means one or more orchestrator-captured `ToolResult` IDs checked for provenance,
  recency, success, and target-relevance — a bare fenced block SHALL be rejected.

- **REQ-009** — [F] spec/001 · [R] paradigm-rule 2/8.
  WHEN a `(host, op_class)` matches the deployment-declared **canary allowlist**, the classifier SHALL
  force **POLL_PAUSE** regardless of the action's otherwise-auto-eligible reversibility/risk, so the FIRST
  staged mutations require a human vote (never AUTO). The pin is **safe-direction only** — it can raise an
  otherwise-AUTO action to a poll but SHALL never lower a poll — and runs AFTER the inviolable mechanical
  floors (REQ-004 et al., which record the more fundamental reason when they also apply). The allowlist is
  operator-declared config (config-not-code — no hostnames/op-classes in the binary); when unconfigured the
  classifier behaves identically to one without this rule (inert by default).

- **REQ-011** — [O] INV-09/INV-19 · consumes spec/023 REQ-2301/2304/2305/2310 (the actor-attribution
  dispositions made classifier-visible, canary-pin style).
  WHEN the attribute step resolves an actor-attribution disposition of `stand-down-coordinate`,
  `security-escalate`, or escalate (an unmapped disposition or a non-suspicious contradiction), the
  classifier SHALL force **POLL_PAUSE** — respectively `actor-attributed-authorized`,
  `actor-attributed-suspicious` (also recording `security_escalation` in the decision signals), or
  `actor-attribution-escalate`. The three inputs are set by the attribute activity from typed,
  reader-captured evidence — never from model narrative — and are **safe-direction only**: they can raise
  an otherwise-AUTO action to a poll but SHALL never lower one, run AFTER the inviolable mechanical floors
  (which record the more fundamental reason when they also apply) and BEFORE the auto-eligible branches,
  and an `unattributable` attribution (or absent evidence) sets none of them so the classification is
  byte-identical to the pre-feature ladder (spec/023 REQ-2303). They SHALL NOT raise autonomy, lift the
  never-auto floor, or bypass the mode chokepoint (spec/023 REQ-2305).


- **REQ-017** — [O] INV-08/INV-11 · TG-317, TG-154 §2/T7 (rationale-vs-argv semantic consistency).
  WHEN the model's stated rationale names at least one host-like token and none of them matches the sealed
  action target, the classifier SHALL force **POLL_PAUSE** with reason `rationale-names-a-different-host`
  and SHALL record the disagreement in the decision signals as `rationale_mismatch`.
  The comparison SHALL be a deterministic token comparison and SHALL NOT be an LLM-judged consistency
  score: the model is the component under suspicion, so it cannot also be the adjudicator.
  It SHALL **abstain** — never escalate — when the rationale names no host-like token, and when the sealed
  target is empty. Silence is not evidence, and an abstain-as-disagreement rule would poll every honest
  action whose prose does not happen to name a machine.
  It SHALL **escalate and never refuse**: a refusal on a text heuristic would take the estate offline on a
  wording change, whereas a poll costs one human glance and places the disagreement in front of the one
  reviewer who reads the rationale. Like REQ-009/REQ-011 it is **safe-direction only** — it may raise an
  otherwise-AUTO action to a poll and SHALL never lower one, and SHALL NOT lift the never-auto floor or
  bypass the mode chokepoint.
  Rationale for the rule at all: the rationale is the ONE field a human on a POLL_PAUSE actually reads, so
  without this check a poll notice can describe one machine while the sealed action touches another, and
  the vote authorizes the action rather than the prose.
- **REQ-010** — [O] INV-08/INV-11/INV-19 · design-wisdom #13 (SK 6.3 prompt-filter secret/PII removal).
  Before untrusted seed text (an alert narrative, an entry ticket, a CMDB record) reaches the model OR is
  written to any log or the governance ledger, the input screen SHALL redact high-confidence
  secret / credential shapes — a bearer or basic Authorization token, a labeled
  `password`/`token`/`api_key`/`secret`/`access_key`/`client_secret` value, a provider-prefixed key
  (GitLab `glpat-`, GitHub `gh*_`, AWS `AKIA*`), a PEM private-key block, basic-auth userinfo embedded in a
  URL, and a long high-entropy hex or base64 run in a key/secret/token context — replacing each with a
  deterministic `[REDACTED:<kind>]` marker. The redaction SHALL be deterministic (no model call) and
  conservative: a benign alert body carrying only hostnames, addresses, rule names, and numbers SHALL pass
  through unredacted, and a value that merely looks numeric SHALL NOT be redacted. WHEN the screen redacts
  a secret it SHALL flag the redaction on the same neutralize-and-flag channel the injection screen uses, so
  the caller emits the redacted (never the raw) text; redacting a secret SHALL NOT by itself force
  POLL_PAUSE (a leaked credential is a hygiene failure, not a jailbreak).

- **REQ-012** — [O] INV-09 · [R] a safety pattern must match THIS estate, not the estate it was ported from.
  WHERE a safety classifier matches identities or op-classes by PATTERN, the pattern SHALL be validated
  against REAL identifiers from the deployed estate and against the LIVE op-class registry, by a standing
  check. A pattern that matches no real identifier SHALL be treated as a defect, not as an absence of risk.
  Where a hand-maintained list must stay in step with a registry, a standing check SHALL FAIL when the
  registry gains a member the list does not cover.
  *Rationale:* two shipped controls were INERT in production and nothing said so. (1) `statefulDenyRE` was
  ported verbatim from the predecessor with `\b` anchors tuned to ITS naming; this estate's hostnames are
  unbroken, so `IsStatefulWorkload("dc1cl01mariadb01")` returned FALSE for a real MariaDB host while
  returning TRUE only for the bare word "mariadb". A clamp whose whole purpose is stopping TG auto-restarting
  a database mid-sync had almost certainly never fired on a real target. The trailing anchor had the same
  defect at the other end — `openbao01` has no boundary after "openbao" — and the estate's secret store was
  not in the list at all. (2) `restartClassRE` was a hand-maintained slug list whose own comment said it
  "MUST list every op-class the effect leaves can actuate", and it omitted `start-guest` — the class with
  219 hands-off heals across 14 hosts — silently exempting it from the self-protected control-plane veto.
  Both failures are the same shape: a pattern is asserted against examples chosen by its author, never
  against the estate or the registry it must actually cover, and it fails SILENTLY and PERMISSIVELY. Hence
  the check runs over real identifiers and over the live registry, not over hand-picked strings. Over-matching
  is the correct direction here: a false positive costs one extra human review, a false negative costs a
  database.
  The same rule binds the deterministic input screen's LANGUAGE and SCRIPT coverage. Its persona-shift and
  retroactive-authorization patterns SHALL cover the languages the DEPLOYED estate's operators actually
  write in, and its homoglyph fold SHALL NOT fold a script that is itself a supported operator language —
  folding one mangles honest prose in the record a human reads. Coverage SHALL be pinned by a standing
  check in both directions: that a derailment written in each operator language is neutralized, and that
  ordinary prose in that language is not. Neither is a barrier: the screen is hygiene over the RECORD, and
  the security property rests on INV-08 (model output is data, never authority) plus the mechanical gate —
  a miss is a hygiene gap to be patched, never a breach, because prose is unbounded and no pattern set can
  be complete. *Rationale:* the screen matched English and Greek while this estate runs two operator sites,
  dc1 and dc2; the Dutch half of its alert and ticket prose passed through un-neutralized. That is
  REQ-012's own failure shape — a pattern asserted against examples its author chose, never against the
  estate it must cover.

- **REQ-014** — [O] INV-19/INV-22 · [R] a gate that records its verdict but not its evidence cannot be audited.
  WHERE the classifier bands a session POLL_PAUSE for `ood-novel-incident`, it SHALL record, in the same
  committed signals the audit row persists, the novelty SIGNATURE it consulted and the prior-incident COUNT
  that signature returned. The signature SHALL be the DECIDING key — the consulted key that had no precedent —
  and not merely the last key examined. The evidence SHALL be recorded ONLY for decisions the novelty rule
  actually drove, so it identifies them rather than decorating every row, and an absent signature SHALL be
  omitted rather than written blank.
  *Rationale:* measured 2026-07-28. `ood-novel-incident` is the second-largest driver of POLL_PAUSE — 140
  decisions over 7 days — and NONE of them mutated: every one asked a human and none produced an action.
  Whether that is 140 correct refusals or 140 spurious escalations is unanswerable, because the classifier
  recorded the verdict and never the reading behind it. The evidence cannot be recovered afterwards either:
  the prior-incident corpus is a MUTABLE FILE with no history and no per-decision snapshot, so what the count
  said at a past moment has no answer. An investigation into those 140 compared them against `session_triage`
  precedent and would have reported a 70% false-positive rate — but novelty reads the KNOWLEDGE corpus, which
  only gains a row on a confirmed-clean closure, so that comparison used the wrong reference set and the
  number was unsupported. Recording the key and count makes the decision checkable, and it also names the
  writeback target: the key that must gain a precedent row for the next same-shape incident to stop polling.
  This requirement adds EVIDENCE ONLY. It does not change when the gate fires, and in particular it does not
  let op-class graduation de-novel a host — whether class-level competence should override host-level
  unfamiliarity is a risk decision for the owner, and taking it to raise the autonomy axis is precisely the
  move this project forbids.

- **REQ-013** — [O] INV-12 · [R] a refusal nobody was asked about is a dead end, not a gate.
  WHERE an op-class has NOT earned autonomy on the graduation ladder — so the policy engine will compose a
  verdict requiring a recorded human approval — the classifier SHALL band the session POLL_PAUSE with the
  reason `op-class-not-graduated`, so that the approval can actually be given. The rule SHALL only ever RAISE
  review: it SHALL NOT lower a band another rule already raised, and it SHALL NOT displace a stronger poll
  reason in the audit row. It SHALL be inert when the graduation resolver is unwired.
  *Rationale:* the BAND decides whether a poll exists; the POLICY VERDICT decides whether approval is needed.
  They are set in different places, so an ungraduated class landing in an AUTO-banded incident was refused at
  the interceptor — `"policy verdict approve — needs a human approval, none recorded (no auto-execute)"` —
  with no poll open and therefore nobody who could approve it. Measured over 24h: 175 AUTO/auto sessions
  executed correctly, 11 POLL_PAUSE/approve sessions opened a poll and were resolvable, and **13 AUTO/approve
  sessions dead-ended**, each a wasted fault and a graduation opportunity the class can never recover.
  Consequence: a new op-class could only accrue clean runs when the classifier HAPPENED to band the incident
  POLL_PAUSE — and the band is decided by incident properties (reversibility, blast radius, confidence,
  attribution) that have nothing to do with whether the verb is new, so graduation progress was left to
  chance. Found when TG selected the newly-added `start-container` verb entirely on its own for a
  container-down, correctly distinguishing it from `restart-container`, and the session died anyway. This adds
  a human ask where the action was previously dropped silently, so it weakens nothing. The ordering clause is
  not cosmetic: placed above the safety rules it MASKED them, and an operator reading the audit row would see
  "this verb is new" where the true cause was "a suspicious actor touched this host" — the band was right and
  the recorded REASON was a misreport, which on the security path is the worse failure.

- **REQ-015** — [O] INV-09/INV-10/INV-19 · [R] paradigm-rule 4 (graduated ladder) ·
  [F] the predecessor's R0 verdict gate (`reconcile-completed-sessions.py`) + its governance-demotion
  consumer (`lib/tier1_suppression.py`), re-sited to the band engine.
  WHERE the classifier's target carries a RECENT, RELEVANT durable mechanical verdict of `deviation` — or
  any verdict the deterministic verifier did not validly produce — the classifier SHALL band the session
  **POLL_PAUSE** with the reason `verdict-deviation-or-invalid`, and SHALL record on the same committed
  signals the `(host, canonical rule family)` signature the deciding verdict was read under and the verdict
  itself.
  **Relevance is two BOUNDS, never a broadener.** (i) *Rule-family scoped*: a verdict counts only when the
  alert rule of the session that produced it folds to the SAME family as this incident's rule, through the
  single family authority (`core/knowledge.CanonicalRule` — the same map the novelty gate, the verdict
  author's REQ-108 sibling rule, and the recovery belt match on). An unmapped rule SHALL keep EXACT
  (case-insensitive) matching. (ii) *Recency bounded*: only verdicts inside an operator-declared window
  (`TG_PRIOR_VERDICT_WINDOW`, config-not-code, default 48h) count, evaluated at read time.
  The verdict SHALL be read from the durable ACTUATION ledger (`action_execution` / `action_verdict`,
  executed rows only) and SHALL NOT be read from the propose-path world-model scores (`prediction_verdict`),
  which grade a different claim (migration 0042).
  **The rule is FAIL-TOWARD-CAUTION and safe-direction only.** An absent, unknown, or UNREADABLE verdict
  SHALL yield exactly the pre-feature classification — never a laxer band, and never a poll invented from
  missing data. A read error SHALL NOT fail the classification (neither open nor closed) and SHALL be
  logged. A `match` or `partial` verdict SHALL NOT tighten the band: the graduation ladder maps `partial`
  to `OutcomeUnverified` (neither promoting nor demoting), so treating it as adverse here would make the
  two gates disagree about the same verdict. The rule SHALL NOT raise autonomy, lift the never-auto floor,
  or displace a stronger poll reason recorded by an earlier branch.
  *Rationale:* TG already holds that a deviation can never auto-resolve again — the graduation ladder
  demotes an op-class on the first `deviation` and requires N consecutive verified-clean runs to re-earn
  `auto`. That memory is keyed on the CLASS and only arrives after the ladder records the run, so the very
  next incident on the host TG just deviated on could still be banded AUTO and executed before the class
  moved. `HasVerdict`/`Verdict` had been consumed by the classifier since it was written and populated by
  NOTHING — an unreachable branch whose deferral rationale ("mutation OFF ⇒ no verdict exists to classify
  on") expired when mutation reached Semi-auto and post-C4 verdicts became meaningful (commit-time
  baselines, the forecast/action split, estate-scoped siting). This applies the existing rule one step
  earlier, at classification rather than only at graduation, and costs at most one extra human review when
  it is wrong. The two bounds are what keep it from degenerating: without the family fold a disk-full
  deviation would pin an unrelated host-down restart, and without the window an ancient deviation would
  park a host on POLL_PAUSE forever — while the long-horizon memory (the ladder) is already stricter and
  unbounded in time.

- **REQ-016** — [O] INV-08/INV-09 · [R] a safety pattern must know a verb BEFORE the verb is registered ·
  [F] the predecessor's `irreversible:network-catastrophic` floor entry and its HELD `code-deploy-or-repo-write`
  class.
  The server-side destructiveness derivation (`safety.IsDestructiveOp`, which reads the ACTUAL op independent
  of the model's declared class) and the non-configurable never-auto floor slug list SHALL cover two further
  categories: **network-catastrophic** — config erase, routing teardown, interface reset, and route / ACL /
  trunk / spanning-tree / bridge / ruleset teardown — and **code-deploy / repo-write** — force-push, ref
  (branch/tag) delete, history rewrite, merge-and-deploy, deploy-key revoke, and pipeline / environment /
  runner destruction.
  An op-class belonging to either category SHALL NOT become auto-eligible by REGISTRATION alone: autonomy for
  such a class SHALL require an explicit floor decision — a declared `safety_tier` that
  `opschema.AutoEligible` refuses, or membership of the never-auto floor — and a standing check SHALL assert
  this through the REAL graduation decision hook, not against the eligibility helper in isolation.
  Two exclusions are DECLARED rather than left to drift, and SHALL be asserted as non-matches: a bare
  `reload` (it collides with `systemctl reload`, a conservative-carve remediation verb on this estate — the
  predecessor recorded the same decision) and the prose-plausible `no interface` / `no vlan` / `no router`
  (the derivation is fed the proposal's RATIONALE as well as its op, so these are carried as floor SLUGS
  instead of as patterns).
  *Rationale:* this adds NO behavior today by construction — actuation is registry-only argv (INV-02/INV-08),
  so an op-class that is not in the catalogue has no builder and no template and can never reach an argv
  vector. It is written now because the deriver must already know these verbs on the DAY a network or deploy
  op-class is first registered; a pattern learned after the class ships is a pattern that was absent for the
  window that mattered. This is the same failure shape REQ-012 records twice over: a safety pattern validated
  only against examples its author chose, failing silently and permissively.

## Persistence contract

Exactly one immutable `session_risk_audit` row per classification, carrying
`risk_level`, `band`, `auto_approved`, `auto_proceed_on_timeout`, `notify_required`, `signals_json`,
`operator_override`, the `plan_hash` that joins to the prediction gate (spec/002), and the canonical
`action_id` (INV-07). The row is a required output of the decision function — omitting a field is a Go
type error — and is appended to the tamper-evident governance ledger (INV-19). See
[`docs/DATA-MODEL.md`](../../docs/DATA-MODEL.md).

## Band-aware audit invariant

A standing check SHALL FAIL if any `auto_approved` row is outside `{AUTO, AUTO_NOTICE}` or carries a
floor signal (`irreversible:*`, `criticality:reboot`, `deviation`). An `[AUTO-RESOLVE]` is valid only
for the exact `action_id` / `plan_hash` it was classified against (INV-07).
