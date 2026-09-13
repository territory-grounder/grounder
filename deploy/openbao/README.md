# OpenBao credential for the console secret writer

`tg-console-writer.hcl` is the least-privilege policy for the identity the **grounder** uses to store a
module's secret when an operator saves one in the configuration dialog.

## Why a second identity

TG's own `tg` cert role holds **read** on `secret/data/tg/*` — it must, to resolve every connector's
credential at runtime. The writer must not inherit that. A settings dialog needs to *set* a secret; it
never needs to *read* one, and an identity that can do both turns a console compromise into credential
theft.

## Why write-only works

KV v2 permits `create`/`update` without `read`. So this identity can rotate `secret/data/tg/matrix` and
cannot read it back — not even the value it just wrote. The console's "current value" display therefore
shows *set / unset* and a last-rotated timestamp, never material.

## Provisioning (one command, run by an operator with admin rights)

```sh
bao policy write tg-console-writer deploy/openbao/tg-console-writer.hcl

# AppRole is used rather than a second cert role: the grounder can be handed a role_id at deploy time and
# fetch a short-lived secret_id, and revoking the writer does not touch TG's own cert identity.
bao auth enable approle            # if not already enabled
bao write auth/approle/role/tg-console-writer \
    token_policies=tg-console-writer \
    token_ttl=20m token_max_ttl=1h \
    secret_id_ttl=24h bind_secret_id=true
```

Then set on the grounder:

```
TG_OPENBAO_WRITER_ROLE_ID=<role_id from: bao read auth/approle/role/tg-console-writer/role-id>
TG_OPENBAO_WRITER_SECRET_ID_REF=bao:secret/data/tg/console-writer#secret_id
```

## Rotation

`secret_id_ttl=24h` forces the secret_id to be re-issued daily; `token_ttl=20m` bounds a stolen token's
usefulness. Neither rotation touches the `tg` role, so a writer rotation cannot break the runtime.

## Adding a module

**Nothing to do.** The policy is scoped to the `secret/data/tg/modules/*` prefix, and a module's lane is
DERIVED from its identity (`desc.ModuleSecretPath`) rather than declared — `desc.Validate` refuses any
descriptor that names its own path. So a new module's credential can be rotated from the dialog the moment
the module exists, with no OpenBao change.

An earlier version of this policy enumerated each module's path. That made the dialog work for exactly the
modules somebody had remembered to list here and return 403 for the rest, which is the defect the whole
surface exists to remove.

## Migrating an existing module

A module whose `*_TOKEN_REF` still points outside the prefix (e.g. `bao:secret/data/tg/matrix#token`)
keeps working, but its Save will write to the derived lane and the module will not read it. That pointer
is read from the environment at boot and nothing can rewrite it at runtime, so adoption is a one-time
change per module:

```
TG_MATRIX_TOKEN_REF=bao:secret/data/tg/modules/notifier/matrix#token
```

Rotate once through the dialog, update the ref, restart. Every rotation after that is a Save.

---

# OpenBao dynamic Postgres credentials (TG-422)

`tg-dyndb-postgres.hcl` is the least-privilege policy for the identity the **worker** uses to LEASE
short-lived Postgres logins from OpenBao's `database` secret engine, instead of holding TG's four
long-lived static DB passwords (`TG_MIGRATION_PASSWORD` / `TG_RUNTIME_PASSWORD` / `TG_TRIAGE_PASSWORD` /
`TG_ACTUATE_PASSWORD`) — the longest-lived, highest-value static secrets TG holds. A worker compromise then
harvests a credential that expires on its own rather than one valid until a human rotates it.

## Status: BUILT, OFF by default, arming gated on the pool-rotation follow-up

The `dyn:` SecretRef scheme and the lease lifecycle (mint → renew-before-TTL → revoke-at-shutdown,
fail-closed) ship in `core/credential/dyndb` and are wired at the worker composition root, but **stay
dormant until `TG_DYNDB_ADDR` is set** — unset, the scheme is unregistered and every `dyn:` ref fails
closed, so a deployment behaves exactly as before. Do **not** point a live DSN at `dyn:<role>` until the
pooled-connection mid-use re-credentialing lands (a lease that expires mid-connection must re-credential the
pool without dropping in-flight work): the slice-2 follow-up on TG-422. Merging the slice-1 code is safe;
*arming* it before slice 2 is not.

## Provisioning (run by an operator with admin rights — config-not-code)

```sh
# 1. The database engine + its connection to TG's Postgres. The connection user is a PRIVILEGED Postgres
#    role the engine logs in as to CREATE the ephemeral per-lease logins; it is never handed to the worker.
bao secrets enable database            # if not already enabled (mount defaults to "database")
bao write database/config/tg-postgres \
    plugin_name=postgresql-database-plugin \
    allowed_roles="tg_runtime,tg_triage,tg_actuate,tg_migration" \
    connection_url="postgresql://{{username}}:{{password}}@postgres:5432/grounder?sslmode=verify-full" \
    username="<engine_admin_user>" password="<engine_admin_password>"

# 2. One role per DB identity — the SQL each ephemeral login is granted, plus its TTLs. Keep the grants
#    identical to the static role today (least privilege is unchanged; only the credential's lifetime shrinks).
bao write database/roles/tg_runtime \
    db_name=tg-postgres \
    creation_statements="CREATE ROLE \"{{name}}\" LOGIN PASSWORD '{{password}}' VALID UNTIL '{{expiration}}'; GRANT tg_runtime TO \"{{name}}\";" \
    default_ttl=1h max_ttl=24h
# …repeat for tg_triage / tg_actuate / tg_migration with each one's grants.

# 3. The worker's lease identity (this policy). A token is shown; AppRole is equally valid (see above).
bao policy write tg-dyndb-postgres deploy/openbao/tg-dyndb-postgres.hcl
bao token create -policy=tg-dyndb-postgres -ttl=1h -period=24h -field=token   # -> TG_DYNDB_TOKEN_REF target
```

Then arm the worker (only after the slice-2 pool rotation lands):

```
TG_DYNDB_ADDR=<openbao-address>
TG_DYNDB_TOKEN_REF=file:/run/secrets/dyndb-token      # or env:/bao: — never an inline literal (INV-13)
TG_DYNDB_MOUNT=database                                # optional; this is the default
TG_DYNDB_CA=/etc/tg/openbao-ca.pem                     # optional; the substrate's private-CA cert
TG_DYNDB_DSN_TEMPLATE=postgres://postgres:5432/grounder?sslmode=verify-full   # userinfo-less; dyn: fills it
TG_DB_DSN=dyn:tg_runtime                               # the runtime pool now leases its login per boot
```

The template must carry **no** userinfo — embedding a static credential is the thing `dyn:` removes, and
`NewProvider` refuses such a template. The worker injects the leased `username:password` into the userinfo
with URL-escaping, so a password with URL-significant bytes cannot corrupt the DSN.

---

# OpenBao SSH CA / signed-cert credentials for actuation (TG-423)

`tg-sshca-actuate.hcl` is the least-privilege policy for the identity the **worker** uses to SIGN its
actuation key's public key into a short-lived SSH user certificate from OpenBao's `ssh` secret engine
(CA mode), instead of holding the estate-MUTATING private key (`TG_ACTUATION_SSH_KEY`) on disk indefinitely.
A worker compromise then harvests a certificate that expires on its own — bounded by the role's `max_ttl` —
rather than a key valid until a human rotates it.

## Status: BUILT, OFF by default, arming needs an estate roll

The `sshca` engine + the cert-signer runner hook ship in `core/credential/sshca` +
`modules/actuation/ssh`, wired at the worker composition root, but **stay dormant until `TG_SSHCA_ADDR` is
set** (per plane — `TG_ACTUATE_SSHCA_ADDR` on the split actuation plane). Unset, actuations present the
static key exactly as before. Arming is a **two-part** operator step: (1) provision the engine below, and
(2) the ESTATE roll — add the CA's public key to every actuation target's sshd `TrustedUserCAKeys` and
reload sshd (~26 hosts). Merging the slice-1 code is safe; *arming* it before the estate trusts the CA
would make every actuation fail hostname/cert verification.

## Provisioning (operator with admin rights — config-not-code)

```sh
# 1. The ssh secret engine + its CA signing key (generate in-engine; the private key never leaves OpenBao).
bao secrets enable -path=ssh-client-signer ssh
bao write ssh-client-signer/config/ca generate_signing_key=true
bao read -field=public_key ssh-client-signer/config/ca     # -> this is what each target must TrustedUserCAKeys

# 2. The sign role: which principals a cert may authorize, the cert TTL, and the extensions. Keep it MINIMAL
#    — allowed_users is the actuation identity, TTL is minutes, and only the extensions the lane needs.
bao write ssh-client-signer/roles/tg-actuate \
    key_type=ca \
    allow_user_certificates=true \
    allowed_users="<actuation-identity>" \
    default_user="<actuation-identity>" \
    ttl=5m max_ttl=15m \
    default_extensions='{"permit-pty":""}'

# 3. The worker's signing identity (this policy). A token is shown; AppRole is equally valid.
bao policy write tg-sshca-actuate deploy/openbao/tg-sshca-actuate.hcl
bao token create -policy=tg-sshca-actuate -ttl=1h -period=24h -field=token   # -> TG_SSHCA_TOKEN_REF target

# 4. THE ESTATE ROLL (do this BEFORE arming the worker): on every actuation target, add the CA public key
#    from step 1 to sshd's TrustedUserCAKeys and reload sshd. Until a host trusts the CA, an armed worker's
#    certificate is refused there — which the runner reports as a clean, named auth failure, never a fallback.
```

Then arm the worker (only after the estate trusts the CA):

```
TG_SSHCA_ADDR=<openbao-address>           # or TG_ACTUATE_SSHCA_ADDR on the split actuation plane
TG_SSHCA_ROLE=tg-actuate
TG_SSHCA_TOKEN_REF=file:/run/secrets/sshca-token   # env:/file:/bao: — never an inline literal (INV-13)
TG_SSHCA_MOUNT=ssh-client-signer          # optional; this is the default
TG_SSHCA_CA=/etc/tg/openbao-ca.pem        # optional; the substrate's private-CA cert
```

The cert TTL/principals live on the OpenBao ROLE, not in the worker — the worker only presents its key for
signing. A per-actuation signing failure fails that actuation CLOSED; it never falls back to the static key.

