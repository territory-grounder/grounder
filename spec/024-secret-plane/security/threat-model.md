<!-- spec/024 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/024 — Threat model (STRIDE)

STRIDE slice for the secret plane. The driving adversary is an attacker who gains read access to the host
filesystem, the container environment (`docker inspect`, `/proc/1/environ`), or a container escape, and
harvests plaintext secrets — the trusted-substrate weakness (spec TG-153). The inverse adversary poisons the
secret-source or the bootstrap to redirect resolution or to make the process trust an attacker-controlled
value.

## Assets
- The business secrets themselves (API tokens, DB passwords, actuation credentials).
- The substrate bootstrap credential (the OpenBao/Vault token or its wrapped-AppRole/JWT replacement).
- The secret-scheme policy (the data that decides whether plaintext is refused).
- The homelab backends' irreducible on-host credential (Vaultwarden master credential / Passbolt private key).

## Threats → mitigations (INV references)

- **Information disclosure — plaintext secrets harvested from the container environment.** Every `env:`
  secret is readable via the container environment; a filesystem read of `.env` exposes them all.
  *Mitigations:* the `enforce` policy (REQ-2400) refuses to boot when a non-exempt business secret is
  plaintext, forcing resolution through a backend that returns the value just-in-time rather than holding
  it in the environment; migration to `bao:` (REQ-2403) removes the value from `.env` entirely; INV-13.

- **Elevation / bypass — a new secret escapes the gate by not being enumerated.** A reference the policy
  never inspects is a silent plaintext hole. *Mitigations:* enumeration completeness is a tested
  requirement (REQ-2402) — the inspected set must equal the declared reference fields; INV-19/INV-22.

- **Bypass by omission — a hardcoded `env:` default under enforce.** A reference left to a compiled `env:`
  default would fall back to plaintext silently. *Mitigations:* under `enforce` a defaulted `env:` is a
  violation exactly as an explicit one, and the literal linter stops blessing `env:` (REQ-2409); INV-13.

- **Spoofing — an attacker unwraps the AppRole SecretID before the process.** *Mitigations:* the wrapping
  token is single-use and short-TTL; if an attacker unwraps first, the process's own unwrap fails — a
  fail-closed tamper alarm rather than a silent compromise (REQ-2407); the SecretID is CIDR- and
  use-count-bound; INV-16.

- **Tampering — redirect resolution to an attacker-controlled backend.** *Mitigations:* a backend
  authenticates over a certificate-verified transport with a `SecretRef`-resolved credential (REQ-2404);
  an unregistered scheme fails closed; INV-13/INV-16.

- **Repudiation / audit.** OpenBao/Vault provides leased, individually-revocable credentials with server
  audit; the spec states plainly that the homelab backends do NOT (REQ-2408), so an operator chooses them
  knowing the reduced audit and revocation posture — no overselling.

## Residual risk (stated, not hidden)
- Secret-zero is RELOCATED, not eliminated: the wrapped-AppRole root is the orchestrator; the k8s-auth root
  is the cluster API; the homelab backends keep an unscopable master credential / OpenPGP private key on the
  host. The permanent exemption set (substrate bootstrap + DB DSNs) remains plaintext by construction
  (REQ-2401). These are documented trade-offs, minimized (tmpfs, single-use, hardware-key options), not
  claimed away.
