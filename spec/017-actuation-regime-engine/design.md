<!-- spec/017 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/017 — Design: Actuation Regime Engine (regime-aware effect lanes, AWX-job first)

How the requirements in `requirements.md` are realized on the Go / Temporal / PostgreSQL stack. Where this
design and the code disagree, the code is the bug and this document is the intent. The engine COMPOSES over
the already-built controls (the actuation interceptor spec/013, the policy engine spec/015, the credential
engine spec/016, the mode chokepoint `core/safety`, the mechanical verifier spec/002, the ledger spec/006)
and replaces none of them — every lane is an effect leaf beneath the same gates.

## Components

- **`regime.Engine`** (`core/regime/engine.go`, `core/regime/resolver.go`) — the single entry point the
  actuation path consults before selecting an effect channel. `SelectLane` takes a typed `Target`
  (`host` / `host-glob` / `group` / `device-class`, the SAME shape spec/015 and spec/016 use, REQ-1703) and
  returns a typed `Lane` or a typed `ErrNoRegime` / `ErrAmbiguousRegime` — never an arbitrary lane
  (REQ-1700, REQ-1701). Regime resolution is config-not-code: operator-declared rules map estate selectors
  to exactly one of the six regimes; the resolver is deterministic most-specific-wins and fails closed on
  an equal-specificity conflict (reusing the spec/016 precedence model over the shared object-model).
  `LaneForRegime(Regime) (Lane, bool)` is the complementary accessor for EFFECT-KIND-driven routing: where
  `SelectLane` routes by the TARGET's management regime (a host managed by ssh / awx / …), some op-classes name
  their channel by their KIND — an `awx-launch` op runs through the awx-job lane regardless of the target host's
  regime (the AWX play runs against the host via AWX's inventory, not the host's own management channel), and a
  hypervisor guest-lifecycle op runs through the proxmox lane regardless of the guest's own regime. The runner
  (spec/012) maps the op-class's `effect_kind` to a regime and asks for that lane HERE; `ok=false` (a resolved-
  but-unwired regime) fails closed, exactly like `SelectLane`. `LaneForRegime` grants no authority — it only
  names the channel; the lane's effect leaf stays UNEXPORTED and reachable only through the spec/013 interceptor.
  **Operator scope note (which layer is the actuation fence):** kind-routed ops (`awx-launch`, future `proxmox-*`)
  deliberately BYPASS the target regime resolver — an AWX op reaches the awx lane regardless of the target's
  regime rule (or absence of one), including `TG_REGIME_DEFAULT_LANE=none`. This is a CHANNEL choice, not a
  permission one ("a lane is a channel, not a permission" — REQ-1700): per-target *permission* is enforced
  IDENTICALLY on the routed path by the policy-engine ACL (spec/015) + the mode chokepoint + the never-auto
  floor on the SAME freshly-built interceptor, plus each non-SSH lane's OWN allowlist (the AWX template
  allowlist + AWX inventory membership). So operators MUST scope kind-routed ops via those controls — the
  per-target regime rules + `default-lane=none` fence only *target-managed* (`ssh-argv`) ops, NOT kind-routed
  ones. Requiring `target-regime == effect-kind-regime` would defeat the whole point (a guest that is native-ssh
  for a service restart but hypervisor-mediated for start/stop), so the resolver bypass is by design.
- **`regime.Regime` / `regime.Lane`** (`core/regime/regime.go`, `core/regime/lane.go`) — the closed regime
  enum (`native-ssh`, `awx-job`, `gitops-mr`, `k8s-declarative`, `api`, `proxmox`) and the `Lane` interface. A `Lane`
  is `{ Regime() Regime; EffectLeaf() actuate.Actuator }` — its actuator is UNEXPORTED and reachable only
  by the interceptor's `Do` (REQ-1702), so a lane cannot mutate around the chain. `native-ssh` is the
  already-built spec/013 SSH effect leaf, re-expressed as a `Lane` so the existing path is one lane among
  several rather than a hardcoded default. The unknown/ambiguous cases fail closed to the operator-declared
  default lane (native-ssh) or refuse (REQ-1701).
- **`regime.LaneEffect`** (`core/regime/effect.go`) — the composition seam. It does NOT execute; it hands
  the selected lane's effect leaf to the spec/013 `actuate.Interceptor`, which runs the wired chain
  (admission → never-auto floor → policy authorize → credential authenticate → mode chokepoint → execute →
  verify). A standing structural check (the regime-composition invariant) asserts no lane exposes an
  exported effect path that skips the interceptor (REQ-1702).
- **`awxjob` actuator** (`modules/actuation/awxjob/awxjob.go`, `client.go`, `extravars.go`) — the FIRST new
  lane and the buildable slice. It is an `actuate.Actuator` (unexported behind its `Lane`), a native Go
  `net/http` client (distroless, no subprocess), read-only for the sensor path and launch-capable only under
  the mode chokepoint:
  - **Allowlist + policy (REQ-1704).** The actuator holds an operator-declared `TemplateAllowlist`
    (`job_template_id → { op_class, extra_vars_schema }`). Before launch it asserts the template is
    allowlisted AND that the spec/015 `Decide` verdict for the bound `op_class` is non-deny. A non-allowlisted
    template or a policy deny refuses — the argv-equivalent is the *template + typed vars*, not an arbitrary
    template id chosen by the model.
  - **Typed extra_vars (REQ-1705).** `extravars.go` validates the requested `extra_vars` against the
    template's declared schema (key set + type) and rejects any unknown key. There is no free-form command
    string anywhere in the launch body — a job template is not a shell escape (INV-02 analogue for the
    argv-only chokepoint).
  - **Credential + mode compose (REQ-1706).** The AWX API token is resolved through the spec/016
    `credential.Resolve` (a `SecretRef`, never hardcoded) AFTER a non-deny policy verdict and BEFORE launch;
    the launch fires only WHILE `safety.MayActuate` is true. A resolved token is necessary, never sufficient.
  - **Sensor vs mutating (REQ-1707).** A read-only `setup` / fact-gathering job is a Phase-1-safe SENSOR
    (used by investigation + the knowledge lane); a mutating job template routes through the mode chokepoint
    and the constitutional never-auto floor and stays OFF (mode Shadow) until the owner-present flip.
- **`regime.AsyncVerify`** (`core/regime/asyncverify.go`, `core/regime/pending.go`) — the GLOBAL
  deferred-verify channel (REQ-1709/1710/1711/1712). An AWX launch is async: it returns a `job_id`
  immediately. The channel treats the launch as a PREDICTION, records the action in `pending-verification`,
  polls `GET /api/v2/jobs/{id}/` to a terminal status, then computes the spec/002 mechanical verdict against
  the prediction and feeds it to the spec/015 graduation ladder. A job that never reaches terminal within
  the operator-declared bound is recorded `unverified` and never counts as a clean run. The launch is bound
  to its `action_id` on the manifest lifecycle chain so a retry/re-poll never double-fires (REQ-1712). This
  channel is GLOBAL by design (the gitopsmr research): any lane whose effect is not synchronously observable
  (AWX-job today; GitOps-MR later) registers a deferred verify rather than declaring success at launch.
- **`awxplaybooks` knowledge lane** (`modules/knowledge/awxplaybooks/ingest.go`) — the read-only knowledge
  lane (REQ-1713/1714). It pulls job templates + descriptions + inventory READ-ONLY (re-read by id, INV-05)
  and ingests them into the wiki + the spec/013-adjacent RAG retrieval plane so the agent discovers
  sanctioned runbooks and PROPOSES them. It launches nothing; a surfaced runbook re-enters as a proposal
  subject to the full interceptor chain. It uses the read-only sensor token, distinct from any launch token
  (REQ-1708).
- **`regime.Audit`** (`core/regime/audit.go`, `core/db/migrations/0020_actuation_regime.up.sql`) — the
  append-only persistence (REQ-1715). One `regime_resolution` per lane selection, one `regime_actuation` per
  launch, one `deferred_verdict` per completed deferred verify; `tg_runtime` holds no UPDATE/DELETE; every
  row is chained into the governance ledger; no secret value is ever written (only `SecretRef` strings).
- **Console surfaces** (`frontend/src/surfaces/Regimes.tsx`, `PendingVerification.tsx`,
  `JobTemplateAllowlist.tsx`) — the operator UX (REQ-1716): the per-target regime/lane map, the AWX
  template-allowlist with each template's authorized op-class, the pending-verification queue, and the
  lane-coverage view. Real engine state only; allowlist edits are ledger-audited.

## AWX REST API shapes (grounded before build)

The AWX-job lane speaks the AWX / AWX-Tower REST API v2 (a native Go `net/http` client, Bearer OAuth2
token). The token is the sealed `SecretRef` resolved via spec/016. Shapes the connector depends on
(confirm against the live AWX at build time; the read-only token is sealed on the box):

- **List / read templates (allowlist + knowledge, read-only):**
  `GET /api/v2/job_templates/` → paginated `{ count, next, previous, results: [ { id, name, description,
  job_type, inventory, ask_variables_on_launch, ... } ] }`. `GET /api/v2/job_templates/{id}/` for one.
- **Launch a job template (the mutating effect, gated OFF until flip):**
  `POST /api/v2/job_templates/{id}/launch/` with body `{ "extra_vars": { ... }, "limit": "<host>" }`.
  On success returns `201` with a **job** resource `{ id, url, status, job_template, ... }`; the `id` is the
  async job handle. If the template does not set `ask_variables_on_launch`, AWX reports `extra_vars` under
  `ignored_fields` — the connector treats an ignored required var as a launch refusal, never a silent
  no-op. The launch body carries ONLY the typed, schema-validated `extra_vars` (REQ-1705) — no command.
- **Poll a job to terminal (the deferred verify):**
  `GET /api/v2/jobs/{id}/` → `{ id, status, failed, started, finished, ... }`. `status` transitions
  `pending → waiting → running → { successful | failed | error | canceled }`. The terminal set drives the
  spec/002 verdict (REQ-1709/1710).
- **Job output (evidence for the verdict):** `GET /api/v2/jobs/{id}/stdout/?format=txt` or
  `/api/v2/jobs/{id}/job_events/` — read-only, for the observed-outcome side of the mechanical verdict.
- **Inventory (knowledge + host↔group reconciliation):** `GET /api/v2/inventories/`,
  `GET /api/v2/inventories/{id}/hosts/`, `GET /api/v2/inventories/{id}/groups/` — read-only, non-secret
  (host + group names only), the SAME shared object-model spec/016 already reconciles against.

> If the live AWX API differs from the above at build time (endpoint path, pagination envelope, launch
> body, or the terminal `status` vocabulary), the connector is grounded in the live API and this section is
> corrected in the same change — the vendor source is truth, not this doc.

## Selection + actuation procedure (per authorized action)

Ordered so a wrong or missing regime can never silently become a direct write:

1. **Regime resolve (REQ-1700/1701/1703).** Match the target over the shared object-model; pick the
   most-specific regime rule; an unknown regime → operator default lane (native-ssh) or refuse; an
   equal-specificity / multi-regime conflict → refuse. Output a typed `Lane`.
2. **Interceptor chain (REQ-1702).** Hand the lane's effect leaf to `actuate.Interceptor.Do`, which runs:
   admission → never-auto floor (INV-09) → policy `Decide` (authZ) → credential `Resolve` (authN) → mode
   chokepoint `MayActuate` → execute → verify. The lane adds NO gate and skips NONE.
3. **AWX-job execute (REQ-1704/1705/1706/1707).** For the awx-job lane the "execute" step asserts the
   template is allowlisted and its op-class is policy-non-deny, validates the typed extra_vars, and — only
   while the mode chokepoint permits and with the credential-resolved token — POSTs the launch and captures
   the async `job_id`. A mutating template with the mode in Shadow refuses (Phase-2 OFF until the flip).
4. **Deferred verify (REQ-1709/1710/1711/1712).** Record `pending-verification` bound to `action_id`; poll
   the job to terminal; compute the spec/002 verdict against the launch prediction; feed the spec/015
   graduation ladder; a non-terminal-within-bound job is `unverified` and counts as no clean run. A second
   launch for the same `action_id` is refused.
5. **Audit (REQ-1715).** One `regime_resolution` + (on launch) one `regime_actuation` + (on verify) one
   `deferred_verdict`, each appended to the append-only tables + the governance ledger, non-secret only.

## How this composes with the platform beneath it

| Question | Engine | Layer |
|---|---|---|
| May TG act on this target? | Policy (spec/015) | authorization, fails closed to deny / Shadow |
| With what identity? | Credential (spec/016) | authentication, fails closed to refuse |
| Through which effect channel? | **Regime (spec/017)** | **lane selection, fails closed to refuse / default** |
| Is the effect wired + gated? | Interceptor + mode chokepoint (spec/013 + core/safety) | the keystone, never bypassed |
| Did the effect match the prediction? | Verifier (spec/002) via the deferred-verify channel | the mechanical verdict |
| Has the op-class earned auto? | Graduation ladder (spec/015 REQ-1514) fed by the deferred verdict | earned per-class trust |

The regime engine adds the "through which channel" row and NOTHING else. It composes above the interceptor
and the mode chokepoint (which stay the sole actuation keystone), consumes the policy verdict and the
credential bundle rather than re-deciding them, and feeds the verifier + graduation ladder rather than
scoring itself. The AWX-job lane is the first non-SSH channel; GitOps-MR, k8s-declarative, and API are later
lanes on the SAME seam (each an effect leaf, each with a deferred-verify channel where its effect is async).

## Out of scope

The authorization decision (may TG act) is spec/015; the per-target credential resolution is spec/016; the
actuation chokepoint + mutation keystone + mode chokepoint are spec/013 + `core/safety`; the mechanical
verdict is spec/002; the ledger + RBAC are spec/006. This spec owns regime resolution, the lane abstraction,
the AWX-job lane (the buildable slice), the GLOBAL async-verify channel, the read-only knowledge lane, and
the console surface. The GitOps-MR, k8s-declarative, and API lanes are named here as the seam's future
tenants; their connectors are their own later changes.
