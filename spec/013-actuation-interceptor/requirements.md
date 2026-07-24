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
  so it has NO compiled builder). `ssh-argv` routes by the target's management regime; `awx-launch` and
  `proxmox-lifecycle` route by KIND to their lane (spec/017, decided in the runner) — e.g. a guest that is
  native-ssh for a service restart is proxmox-mediated for start/stop. `effect_kind` is a fixed field resolved
  by exact op-class lookup, NEVER a model-token-driven branch (INV-08). The registry SHALL bind schema⟷builder
  in exact lockstep at load, failing CLOSED (refusing to boot a half-defined actuation surface) on malformed
  data, an ARGV-ENCODED class (`ssh-argv`/`proxmox-lifecycle`) with no compiled builder (unactuatable), a
  LAUNCH-ENCODED class (`awx-launch`) that carries a compiled argv builder (contradiction), an unknown
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

## Out of scope

The classifier that bands an action is spec/001; the prediction gate and the verdict function are
spec/002; the ledger mechanics are spec/006. This spec owns the interception chain, the single
chokepoint, and the earned mutation flip — the composition that turns the proven controls into a safe
effect channel.
