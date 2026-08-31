# tg-secretenv — resolve backend secrets into a third-party container's env (INC-4)

`cmd/tg-secretenv` is the TG-native, dependency-free equivalent of a Vault-Agent template. It resolves a
set of `SecretRef`s from a secret **backend** (OpenBao/Vault `bao:`, sealed `store:`, or `file:`) and writes
them as `ENVVAR=value` lines to a memory-backed (tmpfs) env file — so a **third-party container** whose image
reads its secrets straight from process env (the LiteLLM model gateway is the case in TG) can receive them
from the vault **without a plaintext value ever living in `.env`** (spec/024 REQ-2403, the other-container
secrets the in-process boot gate cannot cover).

## Why it exists

The `TG_SECRET_POLICY` boot gate (REQ-2400) governs the **TG binaries** (worker + grounder), which resolve
`SecretRef`s in-process. But `LITELLM_MASTER_KEY` and the provider keys are consumed by the **litellm
container** directly as env — a fixed upstream image that does not understand `bao:`. `tg-secretenv` bridges
that gap: it resolves the refs on the host at deploy time and hands litellm a tmpfs env file, so the plaintext
never sits in `.env`.

## Contract

```
tg-secretenv [-out <file>] ENVVAR=<secret-ref> [ENVVAR=<secret-ref> ...]
```

- Each arg is `ENVVAR=<ref>` where `<ref>` is `env:`/`file:`/`store:`/`bao:`.
- Reads `TG_OPENBAO_ADDR` / `TG_OPENBAO_TOKEN_REF` / `TG_OPENBAO_CA` to wire the `bao:` resolver (same as the
  worker/grounder).
- **Fail-closed:** if ANY ref does not resolve, it writes NOTHING and exits non-zero — the dependent
  container must never start with a partial/blank secret set.
- `-out` is written `0600` (owner-only). Omit `-out` to write to stdout (for `set -a; eval "$(tg-secretenv …)"`).
- A resolved value is NEVER logged; failures name the `ENVVAR` + a redacted reason only.

## Wiring litellm (the deferred live step — do with owner present)

This retires `LITELLM_MASTER_KEY` (+ provider keys) from plaintext `.env`. Sketch:

1. Put the values in the vault: `bao:secret/data/tg/litellm#master_key`, `…#kimi`, etc.
2. Add a one-shot `secrets-init`-style service (mirroring the existing `secrets-perms` init) that runs
   `tg-secretenv -out /run/tg/litellm.env LITELLM_MASTER_KEY=bao:…#master_key KIMI_API_KEY=bao:…#kimi …`
   onto a shared tmpfs mount, then the litellm service `env_file: /run/tg/litellm.env` + `depends_on` the init
   with `service_completed_successfully`.
3. Remove the plaintext `LITELLM_MASTER_KEY`/provider keys from `.env`.

Then `TG_SECRET_POLICY=enforce` becomes reachable for the whole stack (the last other-container plaintext
secret is gone).

**NOT wired live yet:** litellm is on the model-gateway path every triage depends on, so activation is a
deliberate owner-present step. The tool itself is complete + tested; only the compose wiring above is pending.
