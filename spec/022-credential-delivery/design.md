<!-- spec/022 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/022 — Design: credential delivery & secret substrate

Realizes [`requirements.md`](requirements.md) (REQ-2200..2207) on the Go/Temporal/Postgres stack, reusing the
Credential Engine ([`spec/016`](../016-credential-engine/)) and `core/seal` rather than inventing a new engine.
The change is where secrets LIVE and how the master key is protected — not how a per-target credential is
chosen (that stays spec/016's `Engine.Resolve`).

## 1. The substrate (REQ-2200, REQ-2204)

Secrets move from `env:`-scheme delivery to OpenBao KV, referenced by `bao:` `SecretRef`s that the existing
`modules/credsource/openbao` resolver dereferences at USE time. The compose/helm services stop receiving secret
VALUES in their environment and receive only `*_REF` strings plus the OpenBao address and an AppRole/token by
which the resolver authenticates. Because `SecretRef` resolution already fails closed (spec/016 REQ-1602,
resolver zero value = refuse), REQ-2204 is satisfied by construction: an unreachable OpenBao yields no
credential and the dependent op refuses — never a fallback to a stale or default value.

Migration is per-secret and cutover-safe: for each credential, (a) write the value into OpenBao KV under the
`TG_OPENBAO_KV_PREFIX` namespace, (b) flip its `SecretRef` from `env:NAME` to `bao:<mount>/<path>#<field>`,
(c) remove the `NAME` value from the environment. Ordering (write-then-flip-then-remove) guarantees no window
in which a running service has neither the env value nor a resolvable ref.

## 2. Master key off the worker (REQ-2201)

Today `core/seal` resolves a 32-byte AES master key from an `env:`/`file:` `SecretRef` and wraps per-secret
DEKs locally — the worker holds the master key. This spec replaces the local master key with **OpenBao Transit**
(encryption-as-a-service): `core/seal` gains a `MasterKeyProvider` seam whose Transit implementation calls
`transit/encrypt` and `transit/decrypt` on the OpenBao server for each DEK wrap/unwrap. The worker performs
seal/unseal by API and never holds master key material; the Transit key never leaves OpenBao. The existing
env/file provider is retained ONLY for the no-OpenBao oracle path and is version-gated so an operator opts into
Transit deliberately. INV-16's "trusted substrate" shrinks from "the whole worker host" to "the OpenBao
Transit key + the worker's OpenBao auth token" — and that token is itself short-lived (REQ-2207).

## 3. JIT actuation identity + plane split (REQ-2202, REQ-2203)

The actuation SSH key is the highest-blast-radius secret. Two composable moves:
- **JIT resolution:** the native-SSH actuator (`modules/actuation/ssh`) resolves the SSH identity from the
  substrate immediately before dialing and holds it only for the single connection's lifetime — never a
  long-lived `file:/secrets/one_key` mount. Where OpenBao's `ssh` secrets engine (SSH-CA / OTP) is available,
  the actuator requests a short-lived signed certificate or one-time credential per actuation (REQ-2207), so a
  harvested key is already expired.
- **Plane split:** the read-only triage worker and the actuation worker become distinct deployments with
  distinct OpenBao AppRoles — the triage role's policy grants NO path to the actuation SSH key or the mutation
  write-tokens (REQ-2203). A compromised triage plane can read what it triages but cannot mint an actuation
  identity. The mode chokepoint and interceptor are unchanged; this only narrows which process can even resolve
  an actuation credential.

## 4. Redaction everywhere (REQ-2205)

`core/screen`'s existing `Scrub` (PEM / bearer / provider-prefixed key / labeled key=value → `[REDACTED:<kind>]`)
already runs on model-bound text, logs, and the ledger; this spec extends the same scrub to the substrate
resolver's error paths (an OpenBao 4xx must never echo the requested path's value) and asserts, in acceptance,
that no resolved value reaches any sink.

## 5. Image-signing key in the substrate (REQ-2206)

cosign's signing key is generated once, its private half stored in OpenBao KV (never a plaintext CI variable
or repo file), and resolved via `SecretRef` for `cosign sign` at build and `cosign verify` before `up -d` /
helm apply. This unifies the supply-chain signing key with the same substrate as every other secret and closes
the wisdom-audit cosign gap without an owner-held key.

## 6. What is explicitly NOT changed

The decision plane — risk classifier, prediction gate, mechanical verdict, mode chokepoint, never-auto floor,
interceptor ordering, graduation, novelty — is untouched. This spec is authentication-substrate hardening
beneath an unchanged authorization/actuation core. `core/config.SecretRef`'s sealed-scheme set
(`env:/file:/store:/vault:/bao:/oidc:`) is unchanged; only the DEFAULT delivery of TG's own secrets moves off
`env:`.
