# OpenBao policy for the WORKER's DYNAMIC-POSTGRES-CREDENTIAL token (TG-422, the self-contained slice of
# TG-320). This is the identity `TG_DYNDB_TOKEN_REF` points at. It can ask the `database` engine to MINT a
# short-lived Postgres login for one of TG's four DB roles, and renew/revoke the leases it holds — nothing
# else. It deliberately CANNOT read the engine's connection config (which holds the privileged DB user the
# engine logs in as to create the ephemeral roles), cannot read or edit the role definitions, and cannot
# reach any other secret. Compromise of the worker yields the ability to mint a login that EXPIRES ON ITS
# OWN, never the long-lived password it replaces.
#
# SCOPED TO THE ROLE PREFIX, NOT ENUMERATED. `database/creds/tg_*` covers migration/runtime/triage/actuate
# and any future TG DB role without a policy edit — the same lesson as tg-console-writer.hcl: a credential
# that 403s because someone forgot to add a path by hand is the failure the whole surface exists to remove.

# MINT: read a fresh {username,password} lease for any TG database role.
path "database/creds/tg_*" {
  capabilities = ["read"]
}

# KEEP-ALIVE + HAND-BACK: renew a held lease before its TTL, and revoke it at shutdown. Renewal/revocation is
# BY lease_id (a capability the holder has over its OWN leases); this grants no visibility into other tokens'
# leases.
path "sys/leases/renew" {
  capabilities = ["update"]
}
path "sys/leases/revoke" {
  capabilities = ["update"]
}

# TOKEN SELF-RENEWAL (TG-422 slice 2). The identity's token is PERIODIC and nothing else renews it — the
# engine calls renew-self on a timer or every mint eventually 403s on an expired token. This must be an
# EXPLICIT exact-path grant: the auth/* deny below beats the default policy's renew-self allow (deny wins
# across policies), and an exact path beats the glob.
path "auth/token/renew-self" {
  capabilities = ["update"]
}

# EVERYTHING ELSE IS DENIED, explicitly, so a future broadening of a parent path cannot silently grant it.
#
# The engine's CONNECTION CONFIG is the crown jewel — it holds the privileged Postgres user the engine uses
# to CREATE the ephemeral roles. A token that could read it could mint arbitrary roles, not just TG's four.
path "database/config/*" {
  capabilities = ["deny"]
}
# The ROLE DEFINITIONS (the SQL each ephemeral login is granted) must not be readable or editable from here —
# editing `creation_statements` is how you would escalate a leased login's grants.
path "database/roles/*" {
  capabilities = ["deny"]
}
# TG's static operational secrets and the whole KV tree are untouchable from this identity.
path "secret/*" {
  capabilities = ["deny"]
}
path "sys/*" {
  capabilities = ["deny"]
}
path "auth/*" {
  capabilities = ["deny"]
}
