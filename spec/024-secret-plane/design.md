<!-- spec/024 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/024 — Design

Synthesized from a judged research pass (Vaultwarden, Passbolt, OpenBao secret-zero) against current
vendor docs and TG's real resolution seams. Nothing here invents a new dispatch path — it INVERTS the
current "plaintext allowed, backend optional" posture on the mechanisms that already exist.

## 1. Where the pieces already are (REQ-2400/2404)

- `core/config.SecretRef.Resolve()` (core/config/config.go) is a scheme switch: built-in `env:`/`file:`/
  `store:`, plus a mutex-guarded keyed registry `RegisterSchemeResolver(scheme, fn)` that `vault:`/`bao:`/
  `oidc:` already use. A new backend registers there; an unregistered scheme fails closed.
- `core/credential.DeliveryConfig.Validate()` already enforces a SCHEME POLICY on the one bootstrap token
  (it must be `env:`/`file:` — "the substrate cannot bootstrap its own credential"). The force-a-backend
  gate is the INVERSE of this same check applied to business secrets.
- `core/preflight/secrets.go` already boot-resolves SSH-key refs (`CheckSSHKeys`); the scheme-policy check
  is a sibling function run from the grounder + worker preflight.

## 2. The forcing function (REQ-2400/2401/2402/2409)

A deployment control `TG_SECRET_POLICY ∈ {off, warn, enforce}` drives a preflight check that:
1. **Enumerates** every process `SecretRef` (REQ-2402). This requires first completing the enumeration —
   the current `configSecrets.SecretRefs` lists a subset; a reference the gate cannot see is a policy hole,
   so completeness is a tested requirement.
2. **Classifies** each by scheme: BACKEND (`bao:`/`vault:`/`store:`/`vw:`/`passbolt:`) = compliant;
   PLAINTEXT (`env:` or an inline literal) = violation UNLESS in the permanent exemption set.
3. **Acts** by policy: `enforce` ⇒ `log.Fatalf` (fail-closed boot refusal, the same shape as the SSH-key
   deploy gate); `warn` ⇒ log and continue; `off` ⇒ no-op (behavior-preserving default).

The **permanent exemption set** (REQ-2401) is closed and code-defined, not config-extensible: the
substrate's own bootstrap credential (spec/022 — it cannot resolve from the substrate it authenticates) and
the database DSNs (needed before any resolver is wired). Under `enforce` the inline-literal linter's
allow-list stops blessing `env:` (REQ-2409) so a hardcoded `env:` default is a violation by omission.

## 3. Complete the OpenBao migration (REQ-2403) — config-only, ships first

The `bao:` resolver already works (netbox migrated live, negative-control-proven). Each remaining business
secret is an `env:`→`bao:` reference flip in deploy config, one at a time, each verified that the resolved
value is byte-identical before moving on — never a partial read. This needs zero new engine and is the
substance of the directive; it is INC-1.

## 4. Two homelab backends, honestly scoped (REQ-2405/2406/2408)

Both register at the scheme registry like `bao:`; both are read-only, native-Go, fail-closed. They are
SECOND-TIER — labeled, never the greenfield default — because each only relocates secret-zero:

- **Vaultwarden** (`vw:<collection>/<item>#<field>`): Vaultwarden implements only the Bitwarden Password
  Manager API — Secrets Manager (`bws` machine tokens) is unsupported and a stated NON-GOAL (REQ-2405).
  So TG authenticates as a human account (identity API key) and does the Bitwarden end-to-end decryption in
  Go (KDF → HKDF stretch → AES-256-CBC + HMAC-SHA256 encrypt-then-MAC cipherstring). Irreducible on-host
  credential: the account MASTER credential — unscopable, all-vault, no per-machine revoke. `bw serve` is
  ruled out (unauthenticated unlocked vault). This is the worst irreducible of the three; support it only
  for homelabbers who already run Vaultwarden.
- **Passbolt** (`passbolt:<resourceID>#<field>`): an OpenPGP robot identity (a human-shaped account; no
  service-account primitive). Session token preferred with a GPGAuth re-login fallback. Irreducible
  on-host credential: the robot's OpenPGP private key + passphrase — best kept off the filesystem on a
  smartcard/YubiKey (decrypt on-device), which is the smallest attack surface of the three homelab options.

Neither offers leased/scoped/individually-revocable secrets; that remains OpenBao-only (REQ-2408).

## 5. Secret-zero for the substrate token (REQ-2407)

Two auth VARIANTS inside `modules/credsource/vault` (the `bao:` grammar and resolver seam are untouched —
only the branch that mints the client token changes):

- **Compose/LXC (primary):** AppRole with a RESPONSE-WRAPPED, single-use, short-TTL SecretID. A trusted
  orchestrator (AWX) calls `secret-id` with a wrap TTL and lands ONLY the single-use wrapping token on a
  memory-backed (tmpfs) path; at boot TG unwraps → AppRole login → client token, and the wrap is spent
  (tamper-evident: if an attacker unwraps first, TG's unwrap fails = alarm). RoleID is a non-secret `env:`.
  Harden with `secret_id_num_uses=1`, short TTLs, and CIDR-bound role. Residual trust root = the
  orchestrator (named honestly — relocated, not removed).
- **Pod (enterprise):** Kubernetes auth — the kubelet-projected ServiceAccount JWT, validated by Vault via
  TokenReview. No static secret on the box; residual trust root = the k8s API server + the RBAC binding.
  OpenBao already runs in-cluster.

`DeliveryConfig.Validate` (spec/022) is relaxed to accept a wrapped-AppRole bootstrap (RoleID `env:`, wrap
token `file:` on tmpfs) or a k8s-JWT bootstrap as satisfying the "not from the substrate itself" invariant,
staying fail-closed. The durable `file:/secrets/tg-openbao-token` is retired where a bootstrap is configured.

## 6. Cross-spec lockstep note

The scheme-policy check touches `core/config` (the scheme registry + literal linter), `core/preflight`
(the boot gate), and `core/credential/delivery.go` (the exemption + relaxed bootstrap validation) — spec/016
/ spec/022-governed files. The MRs that land REQ-2400/2407 carry the companion spec/016/022 amendment and
re-stamp `spec/.lockstep.lock` (SDD-WORKFLOW §6). The new backends live under `modules/credsource/*` (not in
any governed core file). The migration (REQ-2403) is deploy-config only and touches no governed source.

## 7. What is explicitly NOT changed

The `SecretRef` grammar and the built-in `env:`/`file:`/`store:` schemes stay. The `off` default preserves
today's behavior exactly. No secret value is ever logged; resolution stays fail-closed everywhere. This spec
adds a boot-time policy, two optional resolvers, and two bootstrap auth variants — it removes no capability.
