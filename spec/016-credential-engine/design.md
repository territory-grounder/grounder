<!-- spec/016 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/016 — Design: Credential / Identity Engine (per-target resolution + unified sync)

How the requirements in `requirements.md` are realized on the Go / Temporal / PostgreSQL stack. Where this
design and the code disagree, the code is the bug and this document is the intent. The engine is the
AUTHENTICATION sibling of the policy engine (spec/015, authorization): the policy engine decides whether an
action may happen; this engine resolves the identity TG uses to reach the target. It COMPOSES over the
already-built controls (the sealed-secret `core/config.SecretRef` store, the actuation interceptor
spec/013, the ledger spec/006, the estate object-model) and replaces none of the mechanical floors — they
run beneath it, defense-in-depth.

## Components

- **`credential.Engine`** (`core/credential/engine.go`, `core/credential/resolver.go`) — the single entry
  point the actuation interceptor (spec/013) and the read-only investigation modules consult. `Resolve`
  takes a typed `Target` (`host` / `host-glob` / `resource` / `group` / `device-class`) and returns a
  required-field `CredentialBundle` or a typed `ErrUnresolved` — never a default identity (REQ-1600,
  REQ-1602). It runs AFTER the policy engine returns a non-deny verdict and BEFORE execute (REQ-1604).
- **`credential.Bundle`** (`core/credential/bundle.go`) — the resolved identity (REQ-1601):
  `ssh_key_ref`, `user`, `become`, `api_token_ref`, `port`, `connection_scheme`. Every secret-bearing
  field is a `core/config.SecretRef` (REQ-1603), and the struct is constructible only with all required
  fields set, so an under-populated bundle is a compile error — the type system enforces "no blank
  identity".
- **`credential.Match`** (`core/credential/match.go`, `core/credential/precedence.go`) — target matching
  and precedence (REQ-1605/1606). It matches over the SAME estate object-model the policy engine uses
  (`host` / `host-glob` / `group` / `device-class` + inventory), built once and referenced by both
  engines — the integration seam with spec/015. Precedence is deterministic most-specific-wins (exact host
  → narrowest glob → group → device-class); an equal-specificity conflict fails closed rather than picking
  arbitrarily.
- **`credential.SecretRef binding`** (`core/credential/secretref.go`) — the sealed-at-rest binding
  (REQ-1603). Reuses the existing `env:` / `file:` / `store:` schemes (`store:` wired via
  `RegisterStoreResolver`); adds the `vault:` / `bao:` scheme (REQ-1613) via `core/config.RegisterSchemeResolver`
  — a KEYED registry (scheme → resolver) so `vault:` and `bao:` coexist with `store:` without one clobbering
  another (each connector registers its own scheme, receiving the full reference so one client can serve
  several schemes). No plaintext credential value ever enters resolver config, the store, or an exportable
  artifact (INV-13).
- **`credential.Plane`** (`core/credential/plane.go`) — the two-plane router (REQ-1611). Each configured
  source declares exactly one plane: `machine` (AWX / Ansible / Semaphore / OpenBao / Vault + native) or
  `human` (LDAP / OIDC). The router refuses a source that writes across its plane, so a credential-plane
  sync can never populate `approve_by` and an approver-plane sync can never populate a host bundle.
- **`credential.Source`** (`core/credential/source.go`, `core/credential/sync.go`) — the unified sync
  framework (REQ-1607/1608/1609/1615). `CredentialSource` is `{ ID(); Plane(); Sync(ctx) ([]SourceEntry, error) }`
  — a source performs only a READ-ONLY pull returning its entries; the `SyncEngine` owns the diff/converge
  and produces the `SyncRun` record. This keeps sources store-agnostic and the framework oracle-testable
  without a store. Driven by a Temporal Schedule (scheduled) and a console action (on-demand); the pull is
  keyed by `(source_id, native_object_id)`, incremental and idempotent (a no-change re-sync converges with
  no duplicate or orphan). Cross-source precedence and the shadowed-source record live in the engine;
  `last_synced_at` + drift are recorded per run. The engine guards all shared slot state with an RWMutex —
  the pull runs outside the lock so a slow network sync never blocks the actuation-critical `Resolve`.
- **Per-source connectors** (`modules/credsource/*`) — each a native Go client, distroless (no subprocess),
  read-only, fail-closed, grounded in the vendor API docs before build:
  - `modules/credsource/awx` (AWX / Ansible Tower REST API), `modules/credsource/ansible` (parsed inventory
    + Ansible Vault), `modules/credsource/semaphore` (Semaphore API) — inventory + machine credentials
    (REQ-1612). The AWX connector carries TWO mapping modes: (1) per-host/group ansible connection vars →
    a host/group bundle; (2) job-template → inventory + Machine-credential — because AWX attaches a machine
    credential to a JOB TEMPLATE (not a host) and never reveals its key, the connector maps each job
    template's inventory to a group-selector bundle whose `SecretRef` is the one the OPERATOR mapped to that
    AWX credential NAME (`TG_AWX_CRED_REF_MAP`, `AWX cred name=SecretRef` pairs). A job-template Machine
    credential with no operator mapping is skipped-with-record (no blank/guessed identity); with no map the
    job-template walk does not run. A job template that binds more than one mapped Machine credential to one
    inventory emits one entry per credential at equal specificity, so the resolver's equal-specificity
    conflict rule (REQ-1606) fails that inventory closed rather than picking arbitrarily.
  - `modules/credsource/vault` (HashiCorp Vault) and `modules/credsource/openbao` (OpenBao) — KV v2 read
    under AppRole / JWT / Kubernetes auth, the `vault:` / `bao:` backend (REQ-1613). As a `Source` it imports
    per-host bundles from a scoped KV subtree (`Prefix`, e.g. `tg/hosts`); each entry must carry a `user` (the
    host-bundle shape) or the sync fails closed. An UNCONFIGURED (empty) `Prefix` DISABLES the source (0
    entries, honestly empty) rather than listing the whole KV mount root — on a shared substrate the root
    holds other tenants and flat non-bundle process secrets, none of which are host bundles. The `bao:` /
    `vault:` DELIVERY resolver (dereferencing individual `SecretRef`s) is independent of this source `Prefix`.
  - `modules/credsource/oidctoken` — the machine-plane OIDC token MINTER, the `oidc:` `SecretRef` scheme
    (REQ-1619). It is a sibling of `vault:` / `bao:`: instead of DEREFERENCING a stored secret it MINTS a
    short-lived Bearer token via the OAuth2 client-credentials grant (RFC 6749 §4.4) against a configured
    token endpoint over verified TLS, caches it by `expires_in` with a refresh skew, and fails closed
    (unreachable / denied / untrusted cert / no token). `client_id` / `client_secret` are `SecretRef`s. It is
    a distinct capability from the human-plane OIDC user/group sync (`modules/credsource/oidc`, REQ-1614): a
    client-credentials token authenticates TG-as-a-service to a machine target, so it lives on the machine
    plane. Registered resolver-only (no `Sync` source) in the bootstrap composition when `TG_OIDC_TOKEN_URL`
    is configured.
  - `modules/credsource/ldap` and `modules/credsource/oidc` — human-plane users / groups feeding spec/015
    `approve_by` (REQ-1614).
  - `modules/credsource/native` — the NATIVE, standalone machine-plane source (REQ-1600/1607/1610): it
    exposes the operator's read-only hostdiag SSH allowlist (`site|hostglob|sshuser|keyref`,
    `TG_HOSTDIAG_DEPLOYMENTS`) as a `CredentialSource`, reusing `hostdiag.ParseAccess` so there is ONE
    allowlist grammar (not a second parser). Each row becomes one machine-plane entry — selector = the
    host-glob, bundle = `{user, port 22, ssh, ssh_key_ref}` carrying a SecretRef reference only — and it is
    registered at the LOWEST precedence, so it is the standalone fallback (design step 2) that any synced
    system-of-record shadows. It is the config-not-code native store the resolution procedure falls back to.
- **`credential.Sensor / Actuator`** (`modules/actuation/ansible`) — the documented SECONDARY capability
  (REQ-1616). Read-only Ansible `setup` / read-only ad-hoc modules are a Phase-1-safe SENSOR; governed
  playbook / job-template actuation is a mutating channel that runs through the spec/015 mode chokepoint and
  the constitutional never-auto floor. The PRIMARY scope stays credential / identity resolution + sync.

## Resolution procedure (per authorized action)

The engine runs AFTER the spec/015 policy engine returns a non-deny verdict and its bundle feeds the
spec/013 interceptor's execute step. Ordered so a missing identity can never silently become a default:

0. **Plane select (REQ-1611).** A host-reachability resolution uses the machine plane; an approver
   resolution uses the human plane. The planes never cross.
1. **Match over the shared object-model (REQ-1605/1606).** Collect every resolver rule whose selector
   matches the target; pick the most-specific; an equal-specificity conflict → refuse (step 4).
   *Estate host↔group reconciliation (REQ-1605).* A caller builds a `Target` from a host NAME alone
   (`Target{Host: host}`), but a `group`-selector bundle — e.g. an AWX job-template bundle keyed by its
   inventory NAME — only matches a target whose `Groups` carry that group. So BEFORE matching, the
   `SyncEngine` enriches the target with the estate groups that host belongs to: a source that also
   implements `MembershipSource` (the AWX connector walks `/inventories/` + each inventory's `/hosts/`,
   read-only, NON-SECRET — host and group names only) contributes a host→[groups] map the engine indexes
   per-source and consults to populate `Target.Groups`. This is what makes the machine-plane group(inventory)
   bundles actually RESOLVE for a host in that inventory. It is ADDITIVE and fail-closed: a host with no
   known membership gets no extra groups and resolves EXACTLY as before (host / host-glob / native rules
   unchanged, still most-specific-wins); the reconciliation only ADDS the ability to match a group selector,
   it never overrides a more-specific match. Because the enrichment lives in the shared resolver core, the
   read-only investigation path (hostdiag) gets it TODAY and the spec/013 actuation effect leaf gets it for
   free through the SAME seam. Identical `(group selector, bundle)` entries emitted by more than one
   job-template are deduped at the source (identical identity is not a real equal-specificity conflict).
2. **Source precedence (REQ-1609/1610).** WHEN the target is covered by more than one synced source, apply
   the operator-declared source precedence and record the winning + shadowed sources; WHEN nothing is
   synced, fall back to the native store; WHEN precedence does not disambiguate → refuse.
3. **Seal-resolve the bundle (REQ-1601/1603).** Resolve each `SecretRef` field through the store at use
   time (`env:` / `file:` / `store:` / `vault:` / `bao:`); a failed `SecretRef` resolution → refuse.
4. **Fail-closed refuse (REQ-1602).** Any unmatched, ambiguous, or unresolvable path returns
   `ErrUnresolved` — the target is not investigable or actuatable, with no default identity.
5. **Audit (REQ-1617).** One `credential_resolution` row (non-secret identity metadata only — target,
   matched rule/selector, winning source or native fallback, resolved user, connection scheme, and the key
   reference SCHEME, never key material or a full SecretRef value) is appended per resolution to the
   append-only `credential_resolution` table (migration `0018`; `tg_runtime` holds no UPDATE/DELETE), and the
   interceptor executes with the resolved bundle only on `resolved`.

The shared entry point is `credential.AuditedResolver`: it runs the target through the `SyncEngine`, appends
the single audit row, and returns the bundle or the typed fail-closed refusal. The read-only investigation
modules consume it TODAY — the hostdiag SSH df/free/systemctl tools resolve per-host identity through it (an
`ErrUnresolved`/`ErrAmbiguous` host is not investigable — refuse, never a hardcoded `one_key`+root fallback)
— and the spec/013 actuation effect leaf consumes the SAME resolver when the Phase-2 mutating path is wired.

## Sync procedure (per source, scheduled or on-demand)

`Sync()` opens the source's native client, pulls the read-only inventory / credential / identity set,
re-reads each canonical object by id from its system-of-record (INV-05, no trust of a cached mutable copy),
upserts by `(source_id, native_object_id)` against the per-source durable cursor (incremental, idempotent),
computes drift (added / changed / removed) against the prior run, and writes one immutable
`credential_sync_run` row. Secrets are stored as `SecretRef` references only (a Vault path, an AWX
credential id) — never the secret value (INV-13). A failed or partial sync leaves the prior converged
state intact and is recorded with a `partial` / `failed` outcome; a target that a partial sync did not
cover still resolves fail-closed (REQ-1602), never fail-open.

## How this composes with the Policy Engine (spec/015)

| Question | Engine | Layer |
|---|---|---|
| May TG act on this target? | Policy (spec/015) | authorization, fails closed to deny / Shadow |
| With what identity does TG reach it? | Credential (spec/016) | authentication, fails closed to refuse |
| Who may approve the action? | Credential human plane → Policy `approve_by` | shared identity, spec/015 REQ-1516 |

Both engines match on the ONE shared estate object-model (REQ-1605); object-groups and inventory are built
once and referenced by both. The two fail-closed layers are independent defense-in-depth: neither
substitutes for the other, and both sit above the constitutional mechanical never-auto floor (INV-09),
which is untouched by this spec. **TG-89 (this resolver) is a hard dependency of spec/015's interceptor
integration (TG-98)** — the policy engine can only actuate at estate scale once per-target credentials
resolve, so this engine lands alongside or just ahead of that integration (owner directive: build FIRST).

## Persistence & audit

Every `Resolve` appends one `credential_resolution` row and every `Sync()` one `credential_sync_run` row,
each stamped `schema_version` and chained into the governance ledger (INV-19). The runtime DB role holds no
UPDATE/DELETE on these append-only tables (spec/006, migration `0017_credential_engine`). No secret value
is ever written — only `SecretRef` references (INV-13); gitleaks CI backs this.

## Out of scope

The authorization decision (may TG act) is spec/015; the actuation chokepoint and mutation keystone are
spec/013; the risk classifier is spec/001; the ledger mechanics and RBAC/auth surface are spec/006; the
sealed-secret `SecretRef` primitive itself is #27 (`core/config`). This spec owns per-target credential /
identity resolution, the unified sync framework, and the per-source connectors that feed them.
