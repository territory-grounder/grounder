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
