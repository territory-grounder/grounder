<!-- spec/024 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/024 — Secret plane: force a real backend, eliminate secret-zero

**Owning behavior family:** BEH-11 (Secret sourcing — no plaintext at rest, a real backend enforced).
**Constitution / invariants:** INV-01, INV-05, INV-09, INV-13, INV-16, INV-17, INV-19, INV-21, INV-22.
**Phase:** P2 (composes over spec/016 credential engine + spec/022 credential delivery).
**Status:** Draft — spec authored; implementation pending (INC-1..6).

Territory Grounder resolves secrets through `core/config.SecretRef` — `env:VAR`, `file:/path`, `store:…`,
and the pluggable `bao:` / `vault:` / `oidc:` schemes (spec/016/022). The current posture is **"plaintext
allowed, backend optional"**: `env:` is the default everywhere, and a fresh installation can run its entire
secret set as plaintext in a `.env` file injected into the container environment. That is the product-level
gap this spec closes: **a new installation SHALL be able to refuse plaintext secrets and require a real
secret backend**, with the estate-grade path (OpenBao / HashiCorp Vault) and two clearly-labeled homelab
backends (Vaultwarden, Passbolt), and with the substrate's own bootstrap credential removed from disk.

Three honest constraints shape the design and are encoded as requirements, not hidden:
1. The substrate cannot bootstrap its own credential from itself, and the database DSNs are needed before
   any resolver is wired — so a small, named, permanent set of refs is irreducibly non-backend (REQ-2401).
2. Vaultwarden and Passbolt are human password managers repurposed as machine-secret stores; they only
   **relocate** secret-zero (a master password / an OpenPGP private key on the host), never eliminate it,
   and offer no leased/scoped/individually-revocable secrets — so they are SECOND-TIER (REQ-2408).
3. Eliminating the OpenBao token-on-disk **relocates** the trust root (to an orchestrator or the k8s API
   server); it does not remove trust, and this is stated plainly (REQ-2407/REQ-2408).

## Requirements

- **REQ-2400** — [F] owner directive (no plaintext at rest) · [O] INV-13/INV-19.
  The system SHALL provide a boot-time secret-scheme policy selected by a single deployment control with
  the closed set {`off`, `warn`, `enforce`}, defaulting to `off` (behavior-preserving). WHEN the policy is
  `enforce`, the boot preflight SHALL refuse to start — a fail-closed fatal — IF any process secret
  reference outside the permanent exemption set (REQ-2401) resolves through a plaintext-bearing scheme
  (`env:` or an inline literal); WHEN `warn`, it SHALL log each such reference and continue; WHEN `off`,
  it SHALL behave exactly as before this feature.

- **REQ-2401** — [F] irreducible-bootstrap honesty · [O] INV-13/INV-16.
  The policy SHALL carry a documented, closed **permanent exemption set** of references that are
  irreducibly non-backend by construction — the substrate's own bootstrap credential (it cannot be
  resolved from the substrate it authenticates, per spec/022) and the database connection strings required
  before any resolver is wired — and SHALL allow ONLY those to remain `env:`/`file:` under `enforce`; the
  exemption set SHALL NOT be extensible by ordinary configuration.
  **The set SHALL BE CLOSED IN CODE, not asserted by the caller.** An entry claiming an exemption it does not
  hold SHALL be reported as a violation DISTINCT from an ordinary unmigrated reference, and SHALL NOT be
  granted. Membership SHALL be enumerable and a test SHALL assert that every exemption the shipped binaries
  claim is a member — the completeness direction, without which this hardening turns an `enforce` deployment
  into a boot failure.
  *Rationale (2026-07-29).* `SecretEntry.Exempt` was set by whoever built the entry list and the classifier
  honoured it unconditionally; the code's own comment claimed the set was "closed by construction" while
  nothing closed it. Any caller could mark ANY secret exempt and the gate would permit plaintext for it —
  measured concretely: `TG_ACTUATION_SSH_KEY`, the one key that mutates the estate, could be flipped to
  Exempt and the whole suite stayed green. The live deployment runs `TG_SECRET_POLICY=enforce`, so this gate
  is what stands between a plaintext business secret and a refused boot, and its only guard was the
  discipline of whoever edited the caller. Two corrections fell out of writing the set down: the prose named
  "the database connection strings" as exempt, and **no production entry has ever marked a DSN exempt** —
  the members are substrate bootstrap credentials, seal material, and public certificates.

- **REQ-2402** — [O] INV-19/INV-22 (a gate that cannot see a reference cannot enforce on it).
  The set of process secret references the boot policy inspects SHALL be COMPLETE — every `SecretRef` the
  worker and grounder resolve at runtime SHALL be enumerated for the policy — and a test SHALL assert the
  enumerated set matches the declared reference fields, so a newly-added reference cannot silently escape
  the gate.

- **REQ-2403** — [F] owner directive (complete the migration) · [O] INV-13/INV-17.
  Every business secret that is not in the permanent exemption set SHALL be resolvable from a secret
  backend (`bao:`/`vault:`/`store:`/a homelab scheme), migrated one reference at a time with per-secret
  verification that the resolved value is unchanged, so the migration never runs the estate on a partial
  read; the migration SHALL be deploy-configuration only where the resolver already exists.

- **REQ-2404** — [R] paradigm-rule 3/9 · [O] INV-13/INV-16/INV-17.
  A new secret backend SHALL plug in ONLY by registering a scheme resolver at the existing keyed scheme
  registry, SHALL be read-only, SHALL authenticate with a credential resolved as a `SecretRef` (never a
  literal), SHALL be native-Go with no subprocess or vendor CLI (the worker is distroless — INV-02), and
  SHALL fail closed on any resolution error; an unregistered scheme SHALL remain a fail-closed error.

- **REQ-2405** — [F] homelab backend (Vaultwarden) · [O] INV-13.
  The system MAY provide a Vaultwarden scheme resolver that retrieves a field of a named vault item over
  the Bitwarden Password Manager API using a `SecretRef`-resolved account credential, performing the
  Bitwarden end-to-end decryption in native Go. It SHALL be documented as a SECOND-TIER backend whose
  irreducible on-host credential is an unscopable account master credential (REQ-2408). Bitwarden Secrets
  Manager (the machine-account / access-token product) is a NON-GOAL — it is not implemented by Vaultwarden
  and SHALL NOT be assumed; a `bw serve` local endpoint SHALL NOT be used (it exposes an unauthenticated
  unlocked vault).

- **REQ-2406** — [F] homelab backend (Passbolt) · [O] INV-13.
  The system MAY provide a Passbolt scheme resolver that retrieves a field of a resource over the Passbolt
  API using an OpenPGP robot identity whose private key and passphrase resolve as `SecretRef`s, preferring
  a session token with a re-authentication fallback for the long-lived worker. It SHALL be documented as a
  SECOND-TIER backend whose irreducible on-host credential is the robot's OpenPGP private key (REQ-2408).

- **REQ-2407** — [F] owner directive (no secret-zero on disk) · [O] INV-13/INV-16 · composes spec/022.
  The system SHALL provide an OpenBao/Vault bootstrap that does not require a durable secret token on
  disk: a response-wrapped, single-use, short-lived AppRole SecretID delivered by a trusted orchestrator
  to a memory-backed path for the Compose deployment, AND a Kubernetes-service-account-JWT auth for the
  pod deployment; the delivery-config validation SHALL accept these bootstraps as satisfying the
  "not from the substrate itself" invariant while remaining fail-closed, and the durable on-disk substrate
  token SHALL be retired where a bootstrap is configured.

- **REQ-2408** — [O] INV-09 (honest trust posture — do not oversell).
  The homelab backends SHALL be labeled SECOND-TIER and SHALL NOT be the default for a new installation;
  the documentation SHALL state that they relocate rather than eliminate secret-zero and provide no
  leased, scoped, or individually-revocable secrets, and that the OpenBao secret-zero bootstrap relocates
  the trust root to the orchestrator or the cluster API rather than removing trust. The primary
  recommended backend SHALL remain OpenBao/Vault.

- **REQ-2409** — [O] INV-13/INV-22 (no silent plaintext default under enforce).
  WHEN the policy is `enforce`, a secret reference whose scheme is left to a hardcoded `env:` default
  SHALL be treated as a violation exactly as an explicit `env:` reference, and the inline-literal-secret
  linter SHALL stop blessing the `env:` scheme, so a fresh installation cannot fall back to plaintext by
  omission.

- **REQ-2410** — [F] owner directive (no plaintext at rest) · [O] INV-13/INV-21 (a control that cannot see
  its subject SHALL NOT report success over it).
  REQ-2402 makes the ENUMERATED reference set complete; this makes the gate independent of enumeration.
  On 2026-08-04 the live worker ran `enforce`, booted green, and held `LIBRENMS_GR_TOKEN` (64 chars) and
  `LIBRENMS_TOKEN` (32 chars) as raw values in the same process: a credential that was neither a reference
  nor a declared name was not merely unpoliced, it was invisible, and the green result asserted its absence.
  The boot policy SHALL therefore ALSO inspect the real process environment. WHEN a variable's NAME declares
  a credential (`TOKEN`/`KEY`/`PASS`/`PASSWORD`/`SECRET`) AND its VALUE carries no reference scheme
  (`env:`/`file:`/`store:`/`bao:`/`vault:`/`oidc:`), that variable SHALL be a violation under `enforce` and a
  logged warning under `warn`. The classification SHALL be structural — the gate SHALL NOT use a length or
  entropy heuristic, which would require reading the value and would drift with every credential format.
  False positives SHALL be excluded by two declared mechanisms only: a raw value reached by a PERMANENTLY
  EXEMPT reference (REQ-2401) inherits that exemption, and a closed allowlist SHALL name each variable that
  holds no credential at all with the reason it cannot (a key NAME, a filesystem PATH, this policy's own
  MODE, an endpoint URL, public material). The gate SHALL report at boot the count of variables examined and
  the count matching the credential shape, so a scan that has stopped matching is distinguishable from a
  clean deployment. No part of any variable's VALUE SHALL appear in the report, the boot log, or the error.
