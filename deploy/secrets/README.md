# deploy/secrets — per-box secret material (NEVER committed)

This directory is mounted **read-only** into the worker (and the grounder) at `/secrets`. It holds the SSH
private key(s), known_hosts, and PEM CAs the control plane resolves via `file:/secrets/…` SecretRefs.

**Nothing in here is ever committed.** The `.gitignore` ignores `*` and tracks only itself and this
`README.md`. Do **not** add key material to the repo — the public-mirror leak guard
(`github-sync/denylist.txt`) and `scripts/lint-forbidden.sh` will refuse a real key. `[O] INV-13`

---

## Why this matters — the silent-kill this guards against (TG-113)

The distroless worker runs as **nonroot uid:gid 65532**. Two failure modes silently killed **all** native
SSH investigation + actuation while the worker still booted preflight-GREEN and advertised healthy:

1. **Key absent** — a re-provision dropped `/secrets/one_key` (this dir is gitignored, so the key is
   hand-restored on the box; a fresh provision re-arms the gap).
2. **Key unreadable** — `/secrets` provisioned root-owned `0750` and the key `0600`, so the 65532 worker got
   *permission denied*. It surfaced as misleading `"hostkey"` / `"no logs"`, not as "credential missing".

With mutation ON this is a **live-safety gap**. TG-113 makes it **fail LOUD**:

- **Deploy gate (hard fail):** `grounder --check` resolves + reads + `ssh.ParsePrivateKey`s the configured
  SSH key **in-process as the real runtime user** and exits **non-zero** if it is missing/unreadable/
  unparseable. A check run as root would falsely pass, so it must run as the distroless user against this
  mount:

  ```sh
  # On the box, from deploy/ — exits non-zero and fails the deploy if /secrets/one_key is bad:
  docker compose run --rm grounder --check
  ```

  (`TG_ACTUATION_SSH_KEY` is passed to the grounder service in `docker-compose.yml` for exactly this; the
  grounder does not otherwise use the key.)

- **Runtime signal (boot degraded + loud):** the worker still boots (so triage telemetry keeps flowing) but
  logs an `ERROR credential preflight DEGRADED` line and publishes `tg_ssh_credential_ready=0` on its
  `/metrics`, so the console/Prometheus show **"SSH credential missing/unreadable — native SSH investigation
  + actuation DISABLED"** instead of a false healthy.

---

## Durable provisioning requirement (owner / deploy-playbook action)

Because key material is owner-held and this directory is gitignored, the key **cannot** be provisioned from
the repo. The external deploy path (the AWX/Ansible deploy playbook that runs `docker compose pull → up -d`)
**MUST** place the key with the correct owner/group and mode:

| Requirement | Value |
| --- | --- |
| Path (on the box) | `deploy/secrets/one_key` → mounted at `/secrets/one_key` |
| Format | a PEM-format OpenSSH/PKCS#8 SSH private key (unencrypted, or supplied with its passphrase upstream) |
| Owner/group | readable by **uid:gid 65532** (the distroless nonroot user) |
| Mode | **0640** (never world-readable; never 0600 root-only) |
| Referenced by | `TG_ACTUATION_SSH_KEY=file:/secrets/one_key` in `.env` (plus any `TG_SYSLOGNG_DEPLOYMENTS` / `TG_HOSTDIAG_DEPLOYMENTS` / `TG_CREDENTIAL_NATIVE_RULES` keyrefs) |

The compose stack ships a one-shot **`secrets-perms`** service (`chgrp -R 65532 /secrets; chmod 750 /secrets;
find /secrets -type f -exec chmod 640`) that the worker and grounder both wait on, so it repairs the
group/mode automatically **once the key file exists**. It cannot create the key — placing the key material is
the playbook's job. If the playbook omits the key, the TG-113 gate above fails the deploy loudly instead of
letting the worker come up silently SSH-dead.

Any additional investigation identities (syslog-ng, host-diagnostics, native credential rules) that reference
`file:/secrets/<name>` follow the same rule: placed by the playbook, readable by 65532, mode 0640.
