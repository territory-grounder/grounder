<!-- spec/022 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/022 — Credential delivery & secret substrate (master key off the worker, JIT resolution, blast-radius split)

**Owning behavior family:** BEH-9 (Credential / Identity — the *delivery* half; [`spec/016`](../016-credential-engine/) owns the *resolution* half).
**Constitution / invariants:** INV-01, INV-05, INV-09, INV-13, INV-16, INV-17, INV-19.
**Phase:** Phase 2 (governed actuation — hardens the trusted-substrate boundary).
**Status:** Draft.

This spec closes the **operational-substrate** gap the two 2026-07-22 security audits ([`TG-153`] HuggingFace
July-2026 AI-driven-intrusion scenario, [`TG-154`] wisdom-sources conformance) both converged on: TG's
*decision-plane* governance is best-in-class, but the worker holds the whole credential set — the actuation SSH
key and every provider/API token as PLAINTEXT process environment, plus the seal MASTER KEY — so a single
worker compromise (the HF `worker → node → harvest → lateral` chain) yields the entire estate. `core/seal`
already names this boundary as INV-16's "trusted substrate"; this spec REDUCES that assumption to near-zero.

It is the **delivery** counterpart to the existing Credential Engine ([`spec/016`](../016-credential-engine/)):
that engine already RESOLVES a per-target credential from a `core/config.SecretRef` and already ships a working
`bao:`/`vault:` resolver (`modules/credsource/openbao`) — the defect this spec fixes is that TG still DELIVERS
secrets as `env:` plaintext instead of resolving them from OpenBao at use-time. OpenBao is live and unsealed on
the estate (`TG_OPENBAO_ADDR` configured). No new engine is built; this spec moves the secret SUBSTRATE under
the engine we already have, and moves the master unlock key OFF the worker entirely.

> **Two independent fail-closed layers remain in force.** This spec is a HOST-SIDE hardening; it never
> substitutes for the policy engine's fail-closed default (spec/015) or the constitutional never-auto floor
> (INV-09). A credential that cannot be resolved from the substrate refuses the operation (REQ-2204),
> independently of every decision-plane gate.

---

## Requirements

- **REQ-2200** — [O] TG-153/TG-154 · [O] INV-13/INV-16.
  The system SHALL deliver every high-value credential — the actuation SSH identity, provider/API tokens
  (AWX, PVE, NetBox, LibreNMS), the LDAP bind secret, the model-gateway key, and administrative bearer tokens
  — to a runtime process ONLY as a resolvable `core/config.SecretRef` (`bao:` or `file:` backed by a sealed
  mount), and SHALL NOT deliver any such credential's VALUE as a plaintext process-environment variable, so a
  read of `/proc/self/environ` yields no reusable high-value credential.

- **REQ-2201** — [O] INV-16 · [O] TG-153 Critical#2.
  The envelope-encryption master key SHALL be held by an external key service (OpenBao Transit or an external
  KMS), and the worker SHALL perform every seal and unseal by calling that service over an authenticated
  channel, so the worker process SHALL NEVER hold the master key material in its environment or at rest.

- **REQ-2202** — [O] TG-153 Critical#1 · [O] INV-16.
  WHEN an actuation executes, the actuation SSH identity SHALL be resolved just-in-time for that single
  actuation from the secret substrate, and SHALL NOT persist as a long-lived readable file or environment
  value on the actuation worker between actuations, so a worker compromised between actuations holds no
  reusable actuation key.

- **REQ-2203** — [O] TG-153 High#3.
  The read-only triage plane SHALL NOT co-hold the actuation SSH identity or any mutation write-token; a
  compromise of the triage plane SHALL NOT yield an actuation-capable credential, so the read and mutate
  blast-radii are disjoint.

- **REQ-2204** — [O] INV-09 · [F] composes spec/016 REQ-1602.
  IF a credential cannot be resolved from the secret substrate, THEN the dependent operation SHALL refuse and
  SHALL NOT fall back to a plaintext, default, cached, or last-used credential value; the resolver's zero value
  is "unresolved / refuse".

- **REQ-2205** — [O] INV-13.
  The system SHALL NOT log, echo to any output, persist to any artifact, or transmit a resolved secret value
  beyond its single in-memory use, and SHALL redact secret-shaped material at every log, export, and
  model-bound boundary.

- **REQ-2206** — [O] TG-154 supply-chain · [O] INV-17.
  The container-image signing key SHALL be stored in the secret substrate (OpenBao) and resolved via
  `SecretRef` for signing at build and verification before deploy, and SHALL NOT reside in the repository or as
  a plaintext CI variable.

- **REQ-2207** — [O] TG-153 High#8 · WHERE the substrate supports leased or dynamic credentials.
  WHERE the secret substrate issues leased or dynamic credentials, the actuation and API identities SHALL be
  issued with a bounded lifetime and least-privilege scope, so a harvested credential expires and cannot be
  reused indefinitely.
