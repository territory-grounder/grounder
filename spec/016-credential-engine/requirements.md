<!-- spec/016 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/016 — Credential / Identity Engine (per-target credential resolution + unified sync)

**Owning behavior family:** BEH-9 (see [`docs/GOVERNED-BEHAVIORS.md`](../../docs/GOVERNED-BEHAVIORS.md)).
**Constitution / invariants:** INV-01, INV-05, INV-09, INV-13, INV-16, INV-17, INV-19.
**Phase:** Phase 2 (governed actuation — the authentication half; read-only resolution + sync is Phase-1-safe).
**Status:** Draft.

The Credential / Identity Engine is the **authentication** half of governed actuation — a SIBLING of the
Policy Engine ([`spec/015`](../015-policy-engine/), which is **authorization**). The policy engine answers
*"may TG act on this target?"* (authZ); this engine answers *"with what identity does TG authenticate to
this target?"* (authN). They compose at actuation:
`agent proposes {op-class, target} → POLICY: may this? (authZ) → CREDENTIAL: resolve identity for target
(authN) → actuate`. TG's investigation and actuation today hardcode a SINGLE identity (`one_key` + root)
via env vars (`TG_HOSTDIAG_DEPLOYMENTS` keyref, `TG_ACTUATION_SSH_KEY`, `TG_SYSLOGNG_*`); real estates are
heterogeneous (host A = key X / root, host B = key Y / ops + become, device C = tacacs user + enable), so
the policy engine can AUTHORIZE an action on host B that TG then cannot AUTHENTICATE to. This engine
resolves the RIGHT credential per target, from a native store OR the estate's existing platforms, and is
being built FIRST (owner directive 2026-07-19) because actuation cannot reach the heterogeneous estate
without it. This document is the requirement source of record; the design is in `design.md`, the runnable
acceptance oracles are in `acceptance/`, and the engineering tasks are in `tasks.json`.

> **Two independent fail-closed layers — genuine defense-in-depth.** The policy engine fails closed
> (default Shadow / deny, spec/015 REQ-1504/1519) AND this engine fails closed (an unresolved credential →
> no reachable identity → no action, REQ-1602), INDEPENDENTLY. Credential-resolution fail-closed is a real
> host-side layer that composes with — and never substitutes for — the policy engine's own fail-closed
> default and the constitutional mechanical never-auto floor (INV-09), which both remain in force beneath
> this engine.

> **Two identity PLANES, kept apart (do not conflate).** (1) The **machine → host** plane — how TG logs in
> to a target (SSH key, user, become/enable, api token, port) — is served by the native store plus the
> AWX / Ansible / Semaphore / OpenBao / HashiCorp Vault sources. (2) The **human → console approver** plane
> — who may approve an action — is served by LDAP / OIDC and feeds spec/015 `approve_by` (TG-102). One
> unified sync framework serves both, routing each source to exactly one plane; a credential-plane source
> SHALL NOT populate approver identity and an approver-plane source SHALL NOT populate host credentials.

## Requirements

- **REQ-1600** — [R] paradigm-rule 4 · [O] INV-17.
  The engine SHALL resolve a target expressed as a `host`, a `host-glob`, a named `resource`, a `group`,
  or a `device-class` to exactly one credential bundle through operator-declared configuration data
  (config-not-code), generalizing the current `hostdiag` `site|hostglob|sshuser|keyref` allowlist into a
  full per-target resolver, and SHALL expose resolution as a typed `Resolve(target) → CredentialBundle`
  entry point that the actuation and investigation modules consume in place of a single env var.

- **REQ-1601** — [F] · [R] paradigm-rule 1.
  A credential bundle SHALL carry a required set of fields — `ssh_key_ref`, `user`, `become` (enable /
  become secret reference), `api_token_ref`, `port`, and `connection_scheme` (`ssh` / `api` / `winrm` /
  `netconf`) — WHERE every secret-bearing field is a `core/config.SecretRef` and the bundle is
  constructible only with all required fields populated, so an under-populated bundle is a Go type error
  rather than a silent blank identity.

- **REQ-1602** — [O] INV-09 · [R] paradigm-rule 4.
  IF no credential bundle resolves for a target, THEN the engine SHALL refuse — the target is neither
  investigable nor actuatable — and SHALL NOT fall back to a default, global, or last-used identity; the
  resolver's zero value is "unresolved / refuse", so any error, panic, or unmatched path fails closed by
  construction.

- **REQ-1603** — [O] INV-13.
  The engine SHALL store every credential value as a reference resolved at runtime through the #27
  sealed-secret store via the `env:` / `file:` / `store:` `SecretRef` schemes and the
  `core/config.RegisterStoreResolver` hook, and SHALL NOT hold a plaintext credential value in resolver
  configuration, the credential store, or any versioned or exportable artifact.

- **REQ-1604** — [O] INV-21 · [R] paradigm-rule 8.
  WHEN a classified, policy-authorized action reaches the actuation chokepoint, the engine SHALL resolve
  the target identity AFTER the policy engine (spec/015) has returned a non-deny verdict and BEFORE the
  spec/013 interceptor executes, so authentication is a distinct control layer that composes with — and
  neither replaces nor is replaced by — authorization.

- **REQ-1605** — [F] · [O] INV-18.
  The engine SHALL key resolution off the SAME `host` / `host-glob` / `group` / `device-class` and
  inventory primitives that the policy engine (spec/015 REQ-1505) matches on — a single estate
  object-model built once (fed by TG-88 inventory ingestion and the infragraph) and referenced by both the
  credential resolver and the policy rules — and SHALL NOT define a second, divergent inventory grammar.

- **REQ-1606** — [O] INV-09.
  WHEN a target matches more than one resolver rule, the engine SHALL select the bundle by a deterministic
  most-specific-wins precedence (exact host, then narrowest glob, then group, then device-class), and IF
  two rules of equal specificity conflict, THEN the engine SHALL fail closed (REQ-1602) rather than choose
  an arbitrary bundle.

- **REQ-1607** — [R] paradigm-rule 4 · [O] INV-05.
  The engine SHALL define a `CredentialSource` interface with a `Sync()` operation runnable on an operator
  schedule and on demand that performs a READ-ONLY pull from an external platform into the native store,
  and SHALL re-read each canonical credential or identity from its system-of-record by id at resolution
  rather than trusting a cached mutable copy.

- **REQ-1608** — [F] · [O] INV-16.
  WHILE a source re-syncs, the engine SHALL apply the pull incrementally and idempotently — a repeated
  `Sync()` over unchanged upstream data SHALL converge to the same store state with no duplicated identity
  and no orphaned bundle — keyed by `(source_id, native_object_id)` with a per-source durable cursor.

- **REQ-1609** — [O] INV-09.
  WHEN a target appears in more than one synced source, the engine SHALL resolve the bundle by a
  deterministic operator-declared source precedence, SHALL record the winning source and the shadowed
  sources on the resolved bundle, and SHALL fail closed (REQ-1602) WHEN the declared precedence does not
  disambiguate the sources.

- **REQ-1610** — [R] paradigm-rule 4.
  WHERE no synced source covers a target, the engine SHALL resolve from the native store as the standalone
  fallback, so TG resolves credentials with zero third-party dependency WHEN nothing is synced.

- **REQ-1611** — [O] INV-13 · [R] paradigm-rule 1.
  The engine SHALL route each configured source to exactly one identity plane — the AWX, Ansible,
  Semaphore, OpenBao, and HashiCorp Vault sources to the machine → host credential plane, and the LDAP and
  OIDC sources to the human → console approver plane — and a machine-plane source SHALL NOT populate
  approver identity and an approver-plane source SHALL NOT populate a host credential bundle.

- **REQ-1612** — [O] INV-05 · [R] paradigm-rule 4.
  The AWX / Ansible / Semaphore source SHALL pull inventory (hosts, groups, connection vars) and
  per-target credentials READ-ONLY through a native Go client against the platform REST API (AWX / Tower
  API, Semaphore API) or a parsed Ansible inventory plus Ansible Vault, SHALL NOT spawn a subprocess (the
  distroless worker has no shell), and SHALL be grounded in the vendor API documentation before build.
  WHERE an AWX host carries no inline connection identity, the AWX source SHALL additionally derive
  machine-plane bundles from the AWX job-template → inventory + Machine-credential binding: for each job
  template that has an inventory and at least one Machine-type credential, it SHALL emit a group-selector
  entry keyed by the inventory name whose bundle references the operator-configured `SecretRef` mapped to
  that AWX credential name and whose login user is the AWX credential's username or the operator-configured
  default, and it SHALL skip-with-record — never a blank or guessed identity — any job template whose
  Machine-credential name has no operator `SecretRef` mapping.

- **REQ-1613** — [O] INV-13 · [O] INV-05.
  The OpenBao / HashiCorp Vault source SHALL resolve secrets through a `vault:` / `bao:` `SecretRef` scheme
  (registered via `core/config.RegisterSchemeResolver`, the keyed scheme-resolver registry that lets
  `vault:` / `bao:` coexist with the built-in `store:` resolver) using a native Go client for KV
  v2 read under AppRole, JWT, or Kubernetes auth, SHALL fetch READ-ONLY, and SHALL fail closed
  (REQ-1602) — never a default or blank credential — WHEN the backend is unreachable, the read is denied,
  or a leased secret is expired.

- **REQ-1614** — [O] INV-12 · [R] paradigm-rule 1.
  The LDAP / OIDC source SHALL pull users and groups READ-ONLY into the human → console approver plane and
  SHALL expose them for the spec/015 `approve_by` resolution (REQ-1516, TG-102), and SHALL NOT write a
  machine → host credential bundle.

- **REQ-1615** — [O] INV-19 · [R] paradigm-rule 4.
  The engine SHALL record `last_synced_at`, the sync outcome, and a drift indicator (upstream objects
  added / changed / removed since the prior sync) per source, and SHALL surface last-synced and drift in
  the operator console.

- **REQ-1616** — [O] INV-09 · [O] INV-21.
  WHERE the AWX / Ansible / Semaphore source is also used as an effect channel, the engine SHALL treat
  read-only fact-gathering (Ansible `setup`, read-only ad-hoc modules) as a Phase-1-safe SENSOR and SHALL
  route governed playbook / job-template actuation through the spec/015 mode chokepoint and the
  constitutional never-auto floor (INV-09) as a mutating channel, WHERE credential / identity resolution
  and sync remain the primary scope of this spec and the sensor / actuator use is a documented secondary
  capability.

- **REQ-1617** — [O] INV-19 · [O] INV-17.
  The engine SHALL append every resolution decision (the target, the matched rule or winning source, the
  selected bundle's non-secret identity metadata, and the resolved-or-refused outcome) and every sync run
  to the tamper-evident governance ledger as a required output, SHALL NOT log a secret value, and SHALL
  admit a source only WHEN its connector adapter is compiled in and registered at startup (no runtime
  activation of an unregistered backend), noting that TG-89 (this resolver) is a hard dependency of
  spec/015's interceptor integration (TG-98).

- **REQ-1618** — [R] paradigm-rule 4 · [O] INV-13 · [O] INV-19.
  The operator console SHALL provide a first-class (Phase-1) credential surface that configures, tests,
  schedules, and triggers on-demand ("Sync now") each sync source (LDAP, OIDC, AWX, Ansible + Vault,
  Semaphore, OpenBao, HashiCorp Vault) with its `last_synced_at` and drift shown, renders the per-target
  credential map (`host` / `host-glob` / `group` / `device-class` → bundle) showing the winning source and
  precedence for each bundle and the native-store fallback entry, renders a COVERAGE view that answers "can
  TG reach target X?" (which targets have a resolved credential versus refuse) paired with the policy
  engine's packet-tracer "may TG act on target X?", edits the estate object-groups SHARED with the policy
  engine (built once, consumed by both), and writes every secret value write-only — a stored secret is
  never echoed back (reusing the #27 sealed pattern) — appending every credential edit to the
  tamper-evident ledger, WHERE the surface renders only real engine state (no fabricated data), themed and
  responsive.

- **REQ-1619** — [O] INV-13 · [O] INV-05 · [R] paradigm-rule 1.
  The OIDC client-credentials token source SHALL mint a machine → service Bearer access token through an
  `oidc:` `SecretRef` scheme (registered via `core/config.RegisterSchemeResolver`, the keyed scheme-resolver
  registry alongside `vault:` / `bao:`) by POSTing the OAuth2 client-credentials grant (RFC 6749 §4.4:
  `grant_type=client_credentials` with the requested `scope`) to a configured OIDC token endpoint through a
  native Go client over VERIFIED TLS, SHALL cache the minted token by its advertised `expires_in` with a
  refresh skew and re-mint on expiry, SHALL carry `client_id` and `client_secret` as `SecretRef` references
  (`env:` / `file:` / `store:`) and never as literal values, SHALL route the minted identity to the machine
  plane (REQ-1611) and never to the human → console approver plane that REQ-1614 serves, and SHALL fail
  closed (REQ-1602) — never a default or blank token — WHEN the token endpoint is unreachable, the grant is
  denied, the server certificate does not verify, or the response carries no access token.

## Persistence contract

The engine writes exactly one immutable `credential_resolution` row per resolution, carrying the `target`,
the resolved `plane`, the matched `rule_id` or winning `source_id`, the shadowed sources, the non-secret
`identity_meta` (`user`, `connection_scheme`, `port`, and the `SecretRef` string — never a secret VALUE),
the `outcome` (`resolved` / `refused`), and the acting context. Each source writes one immutable
`credential_sync_run` row per `Sync()` (`source_id`, `plane`, `started_at`, `last_synced_at`, counts of
objects added / changed / removed, `outcome`). Every row is a required output of its function — omitting a
field is a Go type error — and is appended to the tamper-evident governance ledger (INV-19). No secret
value is ever persisted; only `SecretRef` references are stored (INV-13). See
[`docs/DATA-MODEL.md`](../../docs/DATA-MODEL.md).

## Credential-composition invariant

A standing check SHALL FAIL if any `credential_resolution` row carries a plaintext secret in any field, if
any resolution resolved to a bundle for a target that no rule or source covers (a fabricated identity), if
a machine-plane source populated an approver identity or an approver-plane source populated a host bundle
(REQ-1611), or if an equal-specificity or precedence-ambiguous target resolved to a bundle instead of
refusing (REQ-1606/1609). The fail-closed property (REQ-1602) SHALL hold under any partially-synced or
empty store state.
