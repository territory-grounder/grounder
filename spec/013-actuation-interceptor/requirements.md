<!-- spec/013 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/013 — Wired-by-construction actuation interceptor + mutation gate

**Owning behavior family:** the pre/post interception chain (`core/actuate/`).
**Constitution / invariants:** INV-06, INV-07, INV-09, INV-10, INV-11, INV-19, INV-21.
**Phase:** Phase 2 (governed autonomy — the mutation keystone). **Status:** Approved.

This is the keystone that earns autonomous mutation. Every governed mutation passes through ONE
actuation chokepoint reachable only via the interceptor chain — admission → the mechanical never-auto
floor (at the adapter, defense in depth) → the structure gate (committed prediction + action identity) →
the evidence gate → execute → verify → audit. Every failed check REFUSES loud (surfaces an error/refusal
and records it), never observe-only via a swallowed exception. Mutation ships OFF by construction and can
be enabled only through the proven, wired gate. This document is the requirement source of record; the
design is in `design.md`, the runnable acceptance oracles are in `acceptance/`, and the engineering tasks
are in `tasks.json`.

## Requirements

- **REQ-1201** — [O] INV-21/S8-5.
  The actuation Execute chokepoint SHALL be reachable only through the interceptor chain (the underlying
  actuator is not exported), so a mutating side effect SHALL NOT bypass
  admission → floor → gate → evidence → execute → verify → audit.

- **REQ-1202** — [O] INV-21/S8-5.
  WHEN a governed collaborator (the mutation gate, the actuator, or the ledger) is not wired, the
  interceptor's self-test and its actuation SHALL fail loud — refuse or error — and SHALL NOT execute; a
  control that cannot execute is never left dark or observe-only.

- **REQ-1203** — [O] INV-09.
  WHILE mutation is off, the interceptor SHALL refuse every mutating request; and the mechanical
  never-auto floor SHALL be enforced at the actuation adapter as defense in depth — an irreversible or
  floor-class op SHALL be refused even when mutation is on, and no flag lifts it.

- **REQ-1204** — [O] INV-06/INV-07.
  The interceptor SHALL refuse an ungated action — one with no committed prediction — and SHALL refuse
  an action whose re-derived `action_id` does not match its sealed `ActionManifest`, gating on the
  committed plan and action identity rather than on a command-string blocklist. The fixed argv a reversible
  op_class resolves to SHALL come from ONE op-class schema registry (`core/actuate/opschema`) — the single
  declared source the propose-time screen, the sealed manifest, the runner, and the effect leaf all read, so
  no layer defines a second, drifting argv. The registry SHALL declare each registered reversible class's
  structured params and deterministic fixed-argv builder (currently `restart-service` → `systemctl restart
  <unit>`, `reload-service` → `systemctl reload <unit>`, and `restart-container` → `docker restart
  <container>`); nothing in the schema becomes control flow
  (INV-08) — dispatch is an exact normalized-slug lookup, never a model-token-driven branch.
  The op-class SCHEMA (each class's `op_class`/`op`/structured `params` — what the model READS, proposes
  against, and the prompt catalog renders) SHALL be LOADABLE rules-as-data (an embedded `opschema.json`,
  the prose-loadable rule), while the fixed-argv BUILDER — code that TOUCHES the estate (INV-02: a fixed
  vector, never a shell/string-built command) — SHALL stay COMPILED and keyed by `op_class`, so a loaded
  schema can describe/screen/steer a proposal but can NEVER define what actuates: a new actuatable op-class
  REQUIRES a compiled builder, and no operator-supplied schema adds an execution path. Each class SHALL
  declare a closed `effect_kind` — the effect CHANNEL its params translate into — defaulting (absent/blank)
  to `ssh-argv` so the existing schema stays behavior-preserving. An `effect_kind` carries TWO orthogonal
  properties — how the effect is ENCODED and (separately) how it is ROUTED to a lane: `ssh-argv` and
  `proxmox-lifecycle` are ARGV-ENCODED (their params build a fixed argv via a compiled builder), while
  `awx-launch` is LAUNCH-ENCODED (an AWX job-template launch the runner encodes from the op-class→template
  config + the params as typed `extra_vars` — a fixed template id + typed vars, never a command string, INV-02;
  so it has NO compiled builder), and `k8s-declarative` (TG-122 slice 3) is likewise LAUNCH-ENCODED — the
  runner translates the op-class + its typed params into a gitops-mr `ProposeSpec` (a repo id + CLOSED field
  edits naming operator-declared FieldRules, never free-form file bytes, INV-02) which the gitops-mr actuator
  opens as a merge request; it too has NO compiled builder. `ssh-argv` routes by the target's management
  regime; `awx-launch`, `proxmox-lifecycle`, and `k8s-declarative` route by KIND to their lane (spec/017,
  decided in the runner) — e.g. a guest that is native-ssh for a service restart is proxmox-mediated for
  start/stop, and a declarative change to a GitOps-managed source is Git-mediated regardless of the target's
  own regime. `effect_kind` is a fixed field resolved
  by exact op-class lookup, NEVER a model-token-driven branch (INV-08). The registry SHALL bind schema⟷builder
  in exact lockstep at load, failing CLOSED (refusing to boot a half-defined actuation surface) on malformed
  data, an ARGV-ENCODED class (`ssh-argv`/`proxmox-lifecycle`) with no compiled builder (unactuatable), a
  LAUNCH-ENCODED class (`awx-launch`/`k8s-declarative`) that carries a compiled argv builder (contradiction), an unknown
  `effect_kind`, a duplicate class, or a compiled builder with no schema / one that backs a launch-encoded
  class (unreachable/contradiction). The propose-time param validator SHALL remain EXACTLY as tolerant as the
  compiled builder for every ARGV-ENCODED class (validator-tolerance == builder-tolerance) so the loadable
  schema and the compiled builder can never silently diverge.

- **REQ-1205** — [O] INV-11.
  The interceptor SHALL refuse a mutating action that cites no bound orchestrator-captured tool-result
  evidence, WHERE bound evidence is captured, successful, recent, and target-relevant.

- **REQ-1206** — [O] INV-09/INV-21 · [R] paradigm-rule 4/8.
  Autonomous mutation SHALL be enabled only through the single enable path, which SHALL require the
  interception chain to be proven wired (self-test) before it marks the preflight green and flips the
  gate; mutation SHALL default off (ships dark, observe-before-live).
  **Absorbed into the mode chokepoint (spec/015 REQ-1520/1521; ADR 0013).** The standalone `MutationGate`
  object and the `TG_MUTATION_ENABLED` env knob this requirement described are RETIRED: the active mode is now
  the sole mechanical actuation chokepoint. The proof obligation is preserved — `Chokepoint.ProvePreflight`
  requires the interceptor's self-test to pass before it marks the preflight green — but proving the chain
  wired no longer enables actuation: "may this actuate?" is `mode ∈ {Semi-auto, Full-auto} && preflight-green`,
  and **enabling** is an authenticated, authority-checked, ledger-audited `policy.ModeController` transition
  gated on that same green preflight. Mutation still defaults off (the zero-value mode is Shadow), and the
  deviation breaker / `/halt` kill-switch force the mode to Shadow (the absorbed `gate.Disable()`).

- **REQ-1207** — [O] INV-10/INV-19.
  AFTER an execution the deterministic verifier SHALL write the only `match`/`partial`/`deviation`
  verdict — the acting model has no write path — and the interceptor SHALL append the governed decision
  (execute or refuse) to the tamper-evident hash-chained ledger.

- **REQ-1207b** — [O] INV-10/INV-19 · roadmap P2-1 (migration 0043).
  The interceptor SHALL record ONE DURABLE ROW PER EXECUTION, carrying the FRESH verdict computed against
  THAT execution's own post-state, in addition to (never replacing) the per-action-shape verdict of REQ-1207.
  An execution whose post-state was UNOBSERVABLE SHALL be recorded with NO verdict and an explicit
  unverifiable marker, so "executed but unverified" is durable rather than absent and can never later read as
  a clean result. The per-execution record is EVIDENCE, not a gate: the action has already executed and
  cannot be un-run, so a write failure SHALL be audited as a control gap and SHALL NOT change the outcome.
  The sink is optional; absent one, per-execution recording is dark and behaviour is unchanged.
  An execution MAY additionally name the forward action it INVERTS (`inverts_action_id`, migration 0071 —
  TG-404): a compensating rollback is itself an execution, recorded with its own fresh verdict, so "the
  revert ran and succeeded" and "ran and FAILED" are distinct durable records rather than the same silence.
  A NULL reference marks a forward action; the reference is NOT part of `action_id` (an inverse has its own
  content-addressed identity — the reference only names what it undoes), so INV-07 is untouched.
  *Rationale:* `action_id` is content-addressed over the operation alone (INV-07 — it is the plan-adherence
  fingerprint), and the REQ-1207 store is keyed by it first-wins, so the second and every later execution of
  one action shape persisted nothing. Measured live: **113 `execute|pass` events collapsed into 28 distinct
  `action_id`s**, leaving roughly three quarters of all executions with no durable outcome and making
  "N INDEPENDENT hands-off heals of class X" unrecordable. This requirement does NOT alter `action_id`, its
  computation, or its assertion at any stage — INV-07 is untouched — and it does NOT re-key the REQ-1207
  store, whose first-wins semantics spec/012 relies on as the shape's FIRST verified outcome (TG-124).

- **REQ-1207c** — [O] INV-09/INV-21 · roadmap P2-3.
  WHEN the mode chokepoint reports an ACTUATING posture and NO policy authorizer is wired, the interceptor
  SHALL refuse the action and record the refusal. A missing policy authorizer SHALL NOT be a pass-through in
  any posture that permits actuation. The actuating posture SHALL be read from the mode chokepoint — a
  REQUIRED collaborator bound to the live mode authority — and NOT from the optional mode reader installed
  alongside the decider, since that reader is absent in exactly the failure this guards.
  *Rationale:* the decider was documented as optional because "the mode chokepoint still gates". That holds at
  Shadow and is FALSE in Semi-auto/Full-auto, where the chokepoint permits: the whole policy layer — the
  graduation ladder, per-op-class authorization, the confidence clamp — would vanish silently. The state is
  REACHABLE, not theoretical: `cmd/worker/main.go` logs "policy engine: build failed … actuation falls back to
  the mode chokepoint + never-auto floor only (fail closed)" and CONTINUES BOOTING with a nil decider, so a
  malformed ruleset would have produced a worker that actuates with no policy layer. This is the same
  fail-OPEN class TG-182 closed for the verifier. Boot without a policy engine remains legal (a read-only
  deployment is legitimate); actuating without one does not.

- **REQ-1208** — [O] INV-10 · [R] readiness §4.A (the blind-verifier correctness fix).
  The interceptor SHALL refuse a mutating action that reaches the execute step without a wired
  post-execution observer — it SHALL NOT execute an action whose post-state it cannot verify — so the
  mechanical verdict is never computed against a nil observation; and WHEN an action executes with the
  observer wired the verdict SHALL be computed against the observation that observer returns, WHERE an
  observed alert names a host the committed prediction never named the verdict SHALL be `deviation`.

- **REQ-1209** — [O] INV-07/INV-19.
  AFTER an execution, WHERE the effect leaf can derive a compensating inverse, the interceptor SHALL record
  one execution_log — the forward command and its inverse, bound to the executed `action_id` — into the
  tamper-evident ledger; WHILE mutation is off no action executes, so the interceptor SHALL record no
  execution_log.

- **REQ-1210** — [O] INV-09/INV-21 · [R] readiness §4.B (cross-process shared kill) · CONSTITUTION.md:130
  (circuit breakers with persisted state).
  The armed mutation breaker's state SHALL be held in a persisted, cross-process store, so a deviation or
  chain-integrity trip recorded by one worker is visible to every worker that reads the same store. WHEN the
  shared breaker is OPEN, a worker SHALL refuse a mutating request and force its own mode to Shadow BEFORE it
  actuates — so a trip in one worker force-Shadows every sibling worker (the shared kill a multi-worker canary
  depends on, which a per-process breaker never delivered). WHERE the breaker store cannot be read, the worker
  SHALL treat the breaker as OPEN and refuse (fail closed), never actuating on an unobservable safety breaker.

- **REQ-1211** — [O] INV-09/INV-15 · the spend-guard sibling of the mutation breaker (REQ-1210).
  The system SHALL accrue an approximate US-dollar cost for each model-gateway completion — the call's
  approximate token count (request text plus response text, at the conventional 4-characters-per-token
  approximation) multiplied by the configured per-model rate (`TG_COST_RATE_<model>_PER_1K`, or
  `TG_COST_DEFAULT_RATE_PER_1K` for a model with no explicit rate) — into a durable UTC-day-keyed accumulator
  and a durable per-session accumulator that every worker shares; and the per-actuation increment
  (`TG_COST_PER_ACTUATION_USD`) SHALL accrue into the same accumulators when an actuation runs.

- **REQ-1212** — [O] INV-09/INV-21.
  WHEN the day-keyed accrued cost reaches the configured daily budget (`TG_COST_DAILY_BUDGET_USD`) or a
  session's accrued cost reaches the configured session ceiling (`TG_COST_SESSION_CEILING_USD`), the cost
  breaker SHALL trip: it SHALL force the active mode to Shadow (`ForceShadow`, the same kill wire the mutation
  breaker uses) and SHALL append a `cost:breaker-trip` decision to the tamper-evident hash-chained ledger.

- **REQ-1213** — [O] INV-09/INV-21 · CONSTITUTION.md:130 (circuit breakers with persisted state).
  The cost breaker's accumulators and its open/closed state SHALL be held in a persisted, cross-process store
  (migration 0023), so a budget trip recorded by one worker is visible to every worker that reads the same
  store; and WHEN a worker reads the shared cost breaker as OPEN it SHALL force its own mode to Shadow before
  it continues — so a budget trip in one worker force-Shadows every sibling worker.

- **REQ-1214** — [O] INV-09.
  WHERE the daily budget and the session ceiling are both 0 or absent, the cost breaker SHALL NOT trip on any
  accrued spend — a spend guard that is not configured never enforces.

- **REQ-1215** — [O] INV-15 · the deliberate inverse of the mutation breaker's fail-closed (REQ-1210).
  IF the cost store cannot be read, the cost breaker SHALL treat itself as NOT tripped (fail OPEN), SHALL log
  the read error, and SHALL NOT force the mode to Shadow on that read error — because it guards spend and not a
  safety floor, a cost-store outage SHALL NOT halt operations.

- **REQ-1216** — [O] INV-12 · [O] INV-09 · consumes spec/015 REQ-1506/REQ-1514.
  BEFORE the mode chokepoint, WHERE a policy authorizer is wired the interceptor SHALL consult it
  (`PolicyDecider.Decide`) and SHALL honor the resolved verdict by its REQ-1506 meaning: a `deny` verdict SHALL
  be refused unconditionally (no recorded approval lifts a deny); an `approve` verdict SHALL execute ONLY WHEN a
  human approval is recorded on the request (`Request.Approved`, the vote binding of INV-12) and SHALL otherwise
  be refused; an `auto` verdict SHALL proceed; and any other or unresolved verdict — including a policy-engine
  evaluation error — SHALL be refused (fail closed). This policy layer SHALL remain INDEPENDENT of the
  mechanical mode chokepoint (REQ-1206) and SHALL NOT weaken the never-auto floor: an irreversible or
  destructive op SHALL still be refused at the adapter floor (REQ-1203) even when it is human-approved and the
  policy verdict is `auto`. Honoring a recorded approval on an `approve` verdict is the mechanism by which an
  ungraduated op-class accrues its verified-clean runs toward `auto` (spec/015 REQ-1514) — without it an unseen
  class, which always resolves to `approve`, could never execute its first human-approved run.

- **REQ-1217** — [O] INV-10/INV-19 · consumes spec/015 REQ-1514/REQ-1515 · closes the earn-path REQ-1216 opens.
  AFTER a governed action has EXECUTED and its post-state has been VERIFIED, WHERE a graduation recorder is
  wired the interceptor SHALL feed that run's outcome to the per-op-class graduation ladder so a verified-clean
  run accrues toward `auto` — the WRITE-BACK half of the earn-path whose admission half is REQ-1216. The
  verify verdict SHALL map to a graduation run-outcome as the deterministic verifier authored it (INV-10): a
  `match` SHALL count as a verified-clean run (the only promoting outcome), a `deviation` SHALL demote the
  class and reset its clean-run count, and a `partial` or any non-clean verified outcome SHALL break the
  clean-run streak WITHOUT promoting or demoting. This record SHALL be reached ONLY on the executed-and-verified
  tail — a refused or withheld action SHALL NOT touch the ladder — so autonomy is only ever earned by an action
  that actually ran and verified. The record SHALL be a WRITE of ladder state ONLY: it SHALL NOT authorize any
  action, create an actuation path, or weaken any gate (the never-auto floor, the evidence/territory/
  verifiability gates, the policy verdict, the breaker, and the mode chokepoint all run BEFORE execute and are
  untouched). A record failure SHALL be NON-FATAL to the already-executed action — recorded to the tamper-evident
  ledger (INV-19) and otherwise swallowed, never failing a mutation that already happened. WHERE no recorder is
  wired the interceptor SHALL proceed unchanged (a documented no-op, no regression); in the real worker the
  recorder SHALL be the SAME ladder the policy engine reads (REQ-1216), so the earn-loop closes and an op-class
  can actually graduate.

- **REQ-1218** — [O] INV-12 · TG-126 (the admission/authorization band-freshness fix).
  The interceptor's band-sensitive controls — the 1b human-approval admission AND the 4d policy authorization's
  `EvalInput.Band` — SHALL evaluate the CURRENT incident's classification band carried on the governed request
  (`Request.Band`), NOT the sealed `ActionManifest`'s band. The manifest is content-addressed by `action_id` and
  persisted first-seal-wins (append-only, `ON CONFLICT (action_id) DO NOTHING`), so its band is FROZEN at the
  first sealing of an action identity; a later incident of the same action shape re-classifies to a fresh band
  the frozen manifest cannot carry. WHEN the fresh request band is `POLL_PAUSE` — INCLUDING an absent or zero
  band, which is `BandPollPause` by design (fail closed) — the 1b admission SHALL refuse a request carrying no
  recorded human approval, and the 4d policy authorization SHALL compose that band to at least `approve` (a human
  is required), NEVER `auto`; WHEN it is `AUTO` or `AUTO_NOTICE` the 1b admission SHALL admit without an approval
  and the 4d authorization SHALL compose it as the classifier decided, so a graduated op-class resolves `auto`
  and self-heals hands-off. The fresh band SHALL be authoritative ALONE at BOTH gates: a frozen `AUTO` manifest
  band SHALL NEVER admit or auto-authorize past a fresh `POLL_PAUSE`, and a frozen `POLL_PAUSE` manifest band
  SHALL NEVER block or floor a fresh `AUTO`. The sealed manifest's band SHALL feed NO admission or authorization
  decision — it is retained ONLY as the action's content-addressed identity and audit record. This change SHALL
  NOT weaken the never-auto floor (REQ-1203), the evidence, territory, or verifiability gates, the deny-overrides
  semantics of the policy verdict (REQ-1216), the mutation breaker (REQ-1210), or the mode chokepoint (REQ-1206)
  — each SHALL run unchanged.

- **REQ-1219** — [O] INV-02/INV-09.
  A single-host-bound effect leaf — one that executes its fixed argv on a CONFIGURED host it does NOT receive
  per-action (the native-SSH mutating leaf wraps the argv as `identity@<configured-host>` and never reads the
  action's `Target`) — SHALL declare that bound host, and the interceptor SHALL refuse a mutating action whose
  `Target` does not EXACTLY match it, BEFORE the execute chokepoint (fail closed: a target mismatch blocks the
  heal, it is NEVER mis-routed onto the configured host). A leaf that is not single-host-bound — an empty
  declared host, or a per-target / resource-id leaf (the Proxmox-lifecycle / k8s leaves route by their own
  target) — SHALL be unaffected; the gate is a no-op for it. This host-match gate SHALL run AFTER the mode
  chokepoint (REQ-1206) and BEFORE execute, and SHALL NOT weaken the never-auto floor (REQ-1203), the
  structure/evidence/territory/verifiability gates, the policy verdict (REQ-1216), the mutation breaker
  (REQ-1210), or the mode chokepoint — each SHALL run unchanged. It makes arming a single-host canary safe;
  per-target host+identity resolution (routing the argv to the action's OWN target) is the follow-on that
  retires the single-host binding.

- **REQ-1220** — [O] INV-09/INV-10/INV-19 · the execute chokepoint's SECOND failure channel.
  A mutating effect leaf reports the TARGET's own refusal or failure as a NON-ZERO EXIT STATUS on an otherwise
  successful call — a Result carrying a nil Go error — because the transport succeeded and only the remote
  command failed (the native-SSH leaf returns the remote exit status; the Proxmox-lifecycle leaf returns a
  non-OK task exitstatus as exit 1). A Go error from the same call means a TRANSPORT failure (handshake, auth,
  deadline). The interceptor SHALL interpret BOTH channels and SHALL refuse at the execute chokepoint when the
  exit status is non-zero, recording that status in the refusal reason so the target's own verdict is durably
  attributable (INV-19). A refused execution SHALL NOT advance the manifest lifecycle chain, SHALL NOT compute
  or persist a mechanical verdict (INV-10), and SHALL NOT feed the graduation ladder (REQ-1217): an op-class
  SHALL NEVER earn autonomy from an action its target refused.
  *Rationale:* the verifier scores the POST-STATE, not the effect. WHERE the goal state already holds for an
  unrelated reason — the unit was never down, the alert was stale, another actor recovered it first — a refused
  mutation verifies `match`, and `match` is the ONLY promoting graduation outcome (spec/015 REQ-1514).
  Discarding the exit status therefore converts a non-event into earned autonomy, inverting the earn path.
  Observed live on 2026-07-26: a host-side forced-command guard denied a `start-service` argv with exit 42
  while, in the same second, the governance ledger recorded `actuate:execute:match` and the op-class's
  consecutive-clean count advanced. This gate SHALL NOT weaken the never-auto floor (REQ-1203), the
  structure/evidence/territory/verifiability gates, the policy verdict (REQ-1216), the mutation breaker
  (REQ-1210), the mode chokepoint (REQ-1206), or the host-match gate (REQ-1219) — each SHALL run unchanged.

- **REQ-1221** — [O] INV-10/INV-19 · the third execute outcome: NOTHING CHANGED.
  An effect leaf that detects the target is ALREADY in the requested state SHALL report that condition as a
  distinct NO-OP result, and the interceptor SHALL treat a no-op as neither a failure nor a heal: it SHALL
  record the no-op to the tamper-evident ledger (INV-19), SHALL NOT advance the manifest lifecycle chain, SHALL
  NOT compute or persist a mechanical verdict (INV-10), SHALL NOT write an execution record, and SHALL NOT feed
  the graduation ladder (REQ-1217). The reported outcome SHALL declare the action NOT executed, since no estate
  mutation occurred.
  *Rationale:* this is the one case an exit status cannot express — a real mutation and a no-op both exit 0 —
  and collapsing it in EITHER direction corrupts the evidence. Reporting it as a FAILURE was the original
  defect (measured: 50 of 72 Proxmox refusals in one week were this race, each recorded as "execute failed"
  while the estate was exactly as TG wanted). Reporting it as a HEAL is the opposite error and the more
  dangerous one: the verifier scores the POST-STATE, a target already in goal state verifies `match` BY
  CONSTRUCTION, and `match` is the only promoting graduation outcome (spec/015 REQ-1514) — so an op-class would
  climb toward AUTO on mutations it never performed. Credit belongs to whatever actually changed the estate,
  which by definition was not this action. WHERE a leaf cannot detect the condition (a `systemctl start` on an
  already-running unit exits 0 silently and is indistinguishable at the leaf), this requirement imposes no
  obligation on that leaf; the gap SHALL be treated as a known limit of that lane rather than assumed absent.
  This SHALL NOT weaken the never-auto floor (REQ-1203), the structure/evidence/territory/verifiability gates,
  the policy verdict (REQ-1216), the mutation breaker (REQ-1210), the mode chokepoint (REQ-1206), the
  host-match gate (REQ-1219), or the non-zero-exit refusal (REQ-1220) — each SHALL run unchanged.

- **REQ-1223** — [O] INV-02 · [O] INV-09 · [R] a verb with no group and no ceiling is governed by nothing.
  Every registered op-class SHALL declare a **FAMILY** and a **SAFETY TIER**, each drawn from a CLOSED
  enumeration, normalized before comparison, and validated at registry construction such that an
  unrecognised value FAILS CLOSED (the actuation surface SHALL NOT boot half-defined). The safety tier
  SHALL FLOOR the band an op-class may reach and SHALL be safe-direction only — a tier may lower the
  permitted band, never raise it. Tiers `irreversible` and `vendor-critical` SHALL NOT be auto-eligible
  under any accrual of clean runs, and an UNKNOWN tier SHALL NOT be auto-eligible.
  *Rationale:* the registry is about to grow from 6 verbs toward a real remediation vocabulary, and the two
  things that make that safe are grouping and a ceiling. FAMILY is the unit a graduation ladder can be keyed
  on, so verbs that share a blast radius and a rollback story earn autonomy together instead of one slug at a
  time — without it, ~100 classes means ~100 ladders and 5 clean runs each, which is not a scale anyone
  supervises. An unrecognised family is worse than a typo: it silently opens a ladder nobody is watching, so
  a class could reach autonomy through a group no operator ever reviewed. SAFETY TIER exists because clean
  runs answer "did it work?" while the dangerous verbs pose "what if it does not?" — a prune that succeeds
  1000 times has proved nothing about the run that deletes the wrong thing, and the estate's single ASA
  carries every inter-site tunnel, so a wrong network verb can partition the estate away from the agent
  fixing it. Hence those tiers are floored by construction rather than by accrual. This adds DATA to the
  loadable schema only; argv construction remains COMPILED (INV-02) — a loaded schema may describe, screen
  and steer a proposal but SHALL NEVER define what actuates.

- **REQ-1224** — [O] INV-22 · [R] a verb nothing can provoke is autonomy theatre.
  Every registered op-class SHALL be provoked by at least ONE declared fault class, or its absence SHALL be
  DECLARED with a reason in an operator-authored exemptions file, and a standing check SHALL FAIL on any
  undeclared gap. A fault class SHALL declare, machine-readably, which op-classes it provokes; a declaration
  naming an unregistered op-class SHALL be a finding. An exemption for an op-class that IS provoked, or that
  is no longer registered, SHALL be reported STALE. An absent exemptions file SHALL mean ZERO exemptions.
  *Rationale:* an op-class with no fault source is registered, validated, argv-buildable, rendered in the
  prompt catalog and holding a graduation-ladder row — and can never earn autonomy, because nothing in the
  estate will ever produce the condition it answers. Nothing fails; the class simply sits at `approve`
  forever while the ladder reports it as "not yet graduated" rather than "unreachable". Measured when this
  shipped: **3 of 6 op-classes had no fault source at all**, `reload-service` had never been proposed once
  across the entire ledger, and A5 breadth was therefore capped at 2 op-classes permanently — a cap no check
  anywhere reported, discovered only because a graduation drive kept failing for reasons that looked like bad
  luck. The absent-file clause matters as much as the rest: a checker that passes when its input is missing
  is the vacuous pass this lattice has been bitten by before, so no file means every gap is a finding rather
  than none. As with INV-22 and `ratify`, the DECLARED gap is legitimate and reviewable; the UNDECLARED one
  is the defect.

- **REQ-1225** — [O] INV-02 · [O] INV-07 · [R] argv construction being *compiled* is an implementation
  choice; argv being *operator-authored and fixed* is the law.
  An op-class MAY declare its effect EITHER as a compiled `ArgvBuilder` OR as an operator-authored
  `argv_template` in `opschema.json`, and SHALL declare exactly one: declaring BOTH SHALL fail closed at
  registry build (two contradictory definitions of what a class actuates), and declaring NEITHER SHALL fail
  closed as unactuatable. A template element SHALL be EITHER a literal OR a WHOLE-element `${param}` slot;
  an element that embeds a slot inside a larger string SHALL fail closed at registry build. The FIRST element
  of `argv_template` and of `rollback_template` — the PROGRAM — SHALL be a literal: a template whose element
  zero is a slot SHALL fail closed at registry build. Every slot SHALL
  name a param the class DECLARES and marks REQUIRED. Rendering SHALL substitute each slot with the param
  value VERBATIM as exactly one argv element, SHALL NOT interpret the value, and SHALL NOT invoke a shell.
  The renderer's tolerance SHALL equal `ValidateArgs`' tolerance in BOTH directions: a params set the
  validator accepts SHALL always render, and one it rejects SHALL never render. A verb migrated from a
  compiled builder to a template SHALL render byte-identical argv to the builder it replaces.
  *Rationale on the PROGRAM clause (added 2026-07-28 after an adversarial audit):* the equivalence argument
  for templates — "an operator-authored template with typed, validated slots is exactly as safe as the
  hand-written builder" — holds for every element EXCEPT the first. A compiled `ArgvBuilder` STRUCTURALLY
  cannot let a param supply argv[0]; it is a literal in Go source. Without this clause
  `["${unit}", "--now"]` passed registry validation and rendered `argv[0]` from proposal data — a
  model-chosen executable, which is INV-02's central prohibition wearing a data costume. It was LATENT and
  never live: all seven shipped templates begin `systemctl` or `docker`. That is precisely why it belongs in
  the boot-time gate rather than in review — INV-02 must rest on the structure making the bad template
  UNREPRESENTABLE, not on nobody having written one yet.

  *Rationale:* the comment `"argv construction is code, never loaded"` sat in exactly one place —
  `core/actuate/opschema/opschema.go` — and was read as constitutional law for the life of the project. It
  is not in the CONSTITUTION and never was. INV-02 requires that no LLM-produced token becomes a command
  string and that actuation is fixed argv vectors; an operator-authored template with typed, validated slots
  satisfies both exactly as a hand-written Go function does — same fixed vector, no shell, no model token —
  and `opschema.json` is already embedded and lockstep-hashed, so the tamper boundary is identical. The
  over-reading cost roughly **120 lines across 8 files per verb** where ~10 lines of data would do, and that
  cost is the direct cause of the measured product defect: **41 sessions in 7 days** ended naming a
  capability TG does not have and **441 of 1 333 (33 %)** ended `no-proposal:stop` — TG's reasoning is
  agentic while its action space is a workflow. The whole-element clause is not pedantry: the first cut of
  this renderer anchored its slot pattern to the whole element, so `"--unit=${unit}"` matched nothing, fell
  through as a literal, and rendered the eight characters `${unit}` onto the wire with no error raised — the
  same silent-and-permissive shape that left three shipped safety regexes inert (REQ-012). The
  validator/renderer tolerance clause guards the aliases hazard: if the thing that ADMITS a params set and
  the thing that TOUCHES the estate disagree about what is legal, one of them is wrong at 3am.

- **REQ-1226** — [O] INV-02 · [O] INV-07 · [R] the thing that BUILDS an argv and the thing that RECOGNISES it
  must be one source, or they disagree at the effect leaf.
  The effect leaf's argv→op-class classification SHALL be derived from the op-class registry, not from a
  hand-maintained list. A classification SHALL be a STRUCTURAL match — element count, then literal equality at
  every non-slot position — and SHALL NOT parse, prefix-match, case-fold, or accept an empty slot value. An
  argv claimed by MORE THAN ONE op-class SHALL be refused entirely rather than resolved to either. A class
  with no template, or with a template carrying zero or multiple slots, SHALL NOT be classifiable. Every
  templated class SHALL round-trip: the argv it builds SHALL classify back to that same class and value.
  *Rationale:* `guardMutatingArgv` classifies an argv, re-derives the canonical argv from the class it gets
  back, and refuses on mismatch — so classification is not a convenience, it is half of an integrity check.
  The classifier was a linear if-chain naming four verbs beside a registry that builds the same four shapes:
  two lists that must agree, maintained separately. A verb missing from the chain is silently unexecutable on
  that leaf, and a verb classified as the WRONG class is caught only by the luck of the re-derivation failing.
  Neither failure raises anything. The ambiguity clause is the sharp one: with two claimants the round-trip
  re-derives whichever the loop happened to return, so the check PASSES while the action is recorded, allowed
  and governed as a verb it is not — a same-shape, different-governance substitution that every downstream
  control (allowlist, graduation ladder, verdict) then applies to the wrong verb. Refusing both is the only
  answer that cannot be wrong. This requirement also converts breadth from code into data: with the registry
  as the single source, a new verb is a JSON block rather than an edit in two files that can drift.

- **REQ-1227** — [O] INV-07 · [R] a rollback that lives away from the forward it undoes will drift from it.
  An op-class MAY declare its compensating action as an operator-authored `rollback_template` in the registry,
  under the SAME rules as `argv_template` (whole-element slots naming declared REQUIRED params, verbatim
  substitution, no shell). An absent rollback template SHALL mean the compensating action is a re-run of the
  forward argv. A class whose forward action is NOT its own inverse SHALL declare the inverse explicitly.
  *Rationale:* INV-07 requires a BOUND rollback, not a perfect one, and re-running an idempotent verb
  reconverges to the known-good state — which is why the default is safe for restart and reload. It is exactly
  wrong for `start`: re-running a start is not the inverse of a start, it is the same action again, so a
  ledger would record a compensating action that compensated for nothing. That pairing previously lived in the
  effect leaf as a hand-written special case sitting one function away from the forward it undid. Declaring
  both in the registry keeps them in one place, gives the rollback the same validation the forward gets, and
  means a new verb carries its own inverse instead of relying on someone remembering to add a branch.
  Extended 2026-08-14 (T-029-1; spec/029 REQ-2904, owner sign-off TG-488 B5): the registry additionally
  carries `commit_confirmed` (per-class eligibility + confirm window, validated floors: ≥300s, and for
  awx-launch classes strictly greater than the spec/017 deferred-verify bound) and `rollback_op_class`
  (the compensating CLASS for non-argv effects, cross-validated to resolve at load — a dangling inverse
  refuses the whole registry). Eligibility semantics live in spec/029; THIS spec owns the data shape and
  its load-time refusals.

## Out of scope

The classifier that bands an action is spec/001; the prediction gate and the verdict function are
spec/002; the ledger mechanics are spec/006. This spec owns the interception chain, the single
chokepoint, and the earned mutation flip — the composition that turns the proven controls into a safe
effect channel.

- **REQ-1222** — [O] INV-10 · the immediate post-execution observation may DEMOTE, never PROMOTE.
  The interceptor's post-execution verdict SHALL continue to drive the demoting and streak-breaking graduation
  outcomes — a deviation SHALL demote and arm the breaker at once — but a VERIFIED MATCH SHALL NOT record a
  promoting outcome; the clean run SHALL be asserted later, by the orchestrator, from a post-settle
  re-observation (spec/012 REQ-1223). A verified match SHALL record NOTHING here rather than record an
  unverified outcome.
  *Rationale:* the verdict is computed within seconds of the effect, against a monitoring surface whose own
  refresh cycle is minutes long, and against a baseline that subtracts every alert already firing — so the
  candidate set for a deviation is close to empty by construction and `match` is very nearly guaranteed. That
  is a sound basis for reacting to the BAD case (a deviation visible that fast is real, and a fast demote is
  what safety wants) and an unsound basis for the GOOD one: it cannot distinguish a heal that worked from one
  whose consequences have not surfaced yet. Recording the match as UNVERIFIED instead would be worse than
  silence, because an unverified outcome RESETS the consecutive-clean count — every successful heal would wipe
  the streak it should have advanced and no op-class could ever graduate. This SHALL NOT weaken the never-auto
  floor (REQ-1203), the mode chokepoint (REQ-1206), the mutation breaker (REQ-1210), the non-zero-exit refusal
  (REQ-1220), or the no-op rule (REQ-1221) — each SHALL run unchanged.

- **REQ-1228** — [O] INV-10 · the BASELINE GATE: no execution without an established pre-action baseline, and
  no verdict computed outside one.
  Immediately before execution the interceptor SHALL capture the verify baseline in two independent arms —
  the (host,rule) pair snapshot from the post-state observer, and the HOST-level open-incident set from the
  durable ingest ledger (the pre-anomalous arm) — each with one bounded retry, RESPECTING each read's
  observability result: a failed read SHALL contribute nothing, and SHALL NOT be conflated with an empty
  result. If NEITHER arm can be established, the interceptor SHALL REFUSE to execute at a named `baseline`
  gate. The post-execution verdict SHALL be computed against every established arm, and an
  `execute:deviation` ledger reason SHALL record the baseline provenance it was computed against (each arm's
  observability and size).
  *Rationale:* the 2026-07-28 false deviation (governance ledger 5153–5155). The pre-baseline read's
  observability bool was discarded on the claim that an empty baseline "only widens the surprise set,
  fail-safe"; that reasoning is inverted — losing the baseline changes the deviation test's subject from
  "what my action caused" to "everything already wrong anywhere", and REQ-1222's licence for the instant
  demote+trip is expressly premised on the baseline it had just lost. One manufactured verdict tripped the
  estate-wide breaker, force-Shadowed every worker, demoted start-guest, and halted actuation for 1h49m —
  on an estate where the "surprise" host had been recovered for four minutes and its alert was simply stale.
  The two arms fail independently (live HTTP vs TG's own database); refusal when both are lost is the same
  discipline as the verifiability gate — we do not execute what we cannot adjudicate.

- **REQ-1229** — [O] INV-10 · consumes spec/002 REQ-107 · Phase C4 (verdict scoping on the execute path).
  The interceptor SHALL thread an ESTATE-DERIVED host→site authority (`Request.HostSite`, satisfied by
  `estate.Graph.SiteOf` over the live refreshable holder) into the deterministic verdict author
  (`verify.ComputeVerdictDetailScoped`) alongside both REQ-1228 baseline arms, so the post-execution verdict
  excludes a surprise candidate ONLY when the authority knows BOTH the candidate host's site AND the action
  target's site and the two differ. A host whose site the authority does not know SHALL NEVER be excluded, the
  alert's self-reported ingest `Site` label SHALL NEVER be consulted, and a nil authority SHALL exclude
  nothing. This threading SHALL NOT weaken any gate: admission, the floors, the baseline gate (REQ-1228), the
  mode chokepoint (REQ-1206), the breaker (REQ-1210), and the graduation feed (REQ-1217) each run unchanged —
  the authority only narrows what counts as this action's cascade EVIDENCE, per spec/002 REQ-107's fail-closed
  rules.
  *Rationale:* governance_ledger seq 6555 — an unrelated 59-second sensor flap at the OTHER site scored
  `execute:deviation`, demoted restart-container auto→approve and discarded ~80 hands-off clean runs. The
  REQ-1228 baselines cannot catch it (the flap APPEARED after execution), and the predecessor's `_host_site()`
  exclusion — both sites known and different, unknown never excluded — is the mechanic that does, now keyed on
  the estate graph instead of a hard-coded vocabulary (config-not-code).

- **REQ-1230** — [O] INV-09/INV-21 · TG-166 (TG-153 Medium#11, TG-154 "no action-frequency governor").
  The interceptor SHALL enforce a per-SESSION and per-TARGET actuation-frequency and in-flight-concurrency
  governor immediately before the pre-effect sequence, and SHALL refuse an actuation that would exceed either
  scope's trailing-window cap or either scope's in-flight cap. Every gate above it is per-ACTION and therefore
  blind to a SEQUENCE: an in-grammar, allowlisted, reversible, evidence-bound, target-relevant proposal that
  passes once passes every time, so a subverted agent that can produce ONE admissible restart could produce an
  unbounded number of them. The governor SHALL fail closed on every edge — a non-positive configured cap SHALL
  take the conservative default rather than disable the scope (there SHALL be no spelling of "unlimited"), an
  absent session ref or target SHALL share ONE unattributed bucket rather than be exempt, and an unwired
  governor SHALL refuse. The budget SHALL be charged when an actuation is ADMITTED to the effect, not when it
  succeeds, so a refused or failed attempt still consumes it. A throttled refusal SHALL be DISTINGUISHABLE
  from an execution failure: it SHALL carry a stable rate-limit token, the scope, key, count and cap in its
  reason, and a machine-readable flag on the outcome. A composition root that builds more than one interceptor
  (the direct chain plus one per spec/017 regime lane) SHALL share ONE governor, so the cap does not multiply
  by the number of lanes. The window is held IN PROCESS; the fleet-wide cap is therefore (workers x cap), and
  a durable cross-process window is out of scope here (as it was for the breaker, REQ-1210).
  *Rationale:* there was no rate limit at any scope on the mutating path. The only rate control in the tree,
  core/policy's `RateGovernor` (REQ-1508), is keyed by op-class, clamps `auto`→`approve` rather than refusing
  (so an approved POLL_PAUSE action is never charged), charges at policy-decide time rather than at the effect,
  and has NO production caller — `Engine.WithRateGovernor` is invoked only from a spec acceptance test, so the
  `rate_limit` in the conservative template has been counting nothing.

- **REQ-1231** — [O] INV-10/INV-11 · TG-166.
  Before it executes, the interceptor SHALL re-observe whether the fault the action answers is STILL PRESENT,
  at the LAST pre-effect instant (after the REQ-1228 baseline arms, immediately before the effect), and SHALL
  refuse when the fault is no longer present. Every other gate proves the action SAFE; none of them asks
  whether it is still NECESSARY, so an action justified by evidence captured minutes earlier would fire onto a
  target that had already recovered — mutating a healthy host and crediting its op-class (REQ-1217) for a
  non-event. The check SHALL be a necessity FALSIFIER, never a prover: a NOT-PRESENT answer withdraws the
  licence, a PRESENT answer means only "not refuted" and SHALL license nothing. It SHALL fail closed on both
  remaining edges — a probe that could not READ (an unobservable monitoring surface) SHALL refuse with its own
  distinct reason rather than be read as a clear, and an UNWIRED probe SHALL refuse, exactly as REQ-1213's
  verifiability gate refuses a nil post-execution observer. The runner SHALL satisfy the probe with the SAME
  live active-alert reader the spec/012 clear-check uses, asked the same host-quiet question before the
  mutation rather than after.
  *Rationale:* the check establishes the fault at T_recheck, not at T_execute — the race with the effect is
  irreducible — and host-quiet is coarser than the specific fault in both directions. Both limits are recorded
  at the seam rather than implied away; the value is that T_recheck is seconds rather than minutes before the
  effect, which is where the realistic drift between justification and mutation lives.
