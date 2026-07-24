<!-- spec/022 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/022 — Threat model: credential delivery & secret substrate

STRIDE slice for the secret substrate. The driving adversary is the HuggingFace July-2026 chain (TG-153): an
attacker who achieves code-execution on the TG worker and pivots to node-level, then harvests credentials for
lateral movement. This spec's job is to make that harvest yield nothing reusable.

## Assets
- The seal master key (wraps every DEK — decrypts the whole sealed store).
- The actuation SSH identity (runs commands as `tg-actuator` on every estate host).
- Provider/API write-tokens (AWX / PVE / NetBox), the LDAP bind secret, the model-gateway key, admin bearers.
- The container-image signing key.

## Threats → mitigations (INV references)

- **Information disclosure — `/proc/self/environ` harvest (the audit's Critical #2).** A compromised worker
  reads its own environment. *Mitigation:* REQ-2200 — no high-value credential VALUE is delivered as env; only
  `bao:`/`file:` refs, so the environment holds nothing reusable. INV-13 redaction (REQ-2205) keeps values out
  of logs/exports even on the resolution error path.

- **Elevation — master-key theft decrypts everything (Critical #2 root).** *Mitigation:* REQ-2201 — the master
  key lives in OpenBao Transit / a KMS; the worker wraps/unwraps by API and never holds it, so a worker read
  cannot decrypt the sealed store offline. Shrinks INV-16's trusted substrate to the Transit key + a
  short-lived auth token.

- **Elevation / lateral movement — raw SSH key = arbitrary commands estate-wide (Critical #1).** TG's
  app-layer gates (mode, floor, allowlist, breaker) constrain commands TG BUILDS, not a stolen raw key.
  *Mitigation:* REQ-2202/REQ-2207 — the actuation identity is resolved just-in-time and, where OpenBao's SSH
  engine is available, is a short-lived signed certificate / OTP, so a key harvested between actuations is
  already expired; REQ-2203 removes the key from the triage plane entirely.

- **Spoofing / tampering — a swapped or tampered image runs at deploy (supply chain).** *Mitigation:* REQ-2206
  — cosign signs at build with a key held only in the substrate and the deploy verifies before `up -d`; INV-17
  (signed-manifest boot refusal) is the runtime companion.

- **Denial of service — the substrate is unreachable.** *Mitigation:* fail-closed by construction (REQ-2204,
  composing spec/016 REQ-1602): an unresolved credential refuses the dependent op — a safe stand-down, never a
  fallback to a plaintext or cached value, and never a decision-plane bypass.

## Residual risk
The worker's OpenBao auth token (AppRole/token) is itself a credential; REQ-2207 bounds its lifetime and
REQ-2203 scopes the triage role away from actuation paths, but a live compromise within a single short lease
window can still act within that role's scope. This is inherent to any online secret substrate and is
minimized, not eliminated — the design goal is "no reusable credential at rest," not "no credential ever in
memory." The decision-plane gates (spec/015 policy, INV-09 floor, mode chokepoint) remain the independent
authority over what may actuate, unchanged by this spec.
