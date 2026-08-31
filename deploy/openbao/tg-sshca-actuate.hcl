# OpenBao policy for the WORKER's SSH-CA SIGNING token (TG-423, the SSH slice of TG-320). This is the identity
# `TG_SSHCA_TOKEN_REF` points at. It can ask the `ssh` secret engine to SIGN the actuation key's public key
# into a short-lived user certificate — and nothing else. It deliberately CANNOT read the CA's private signing
# key (the engine's config), cannot read or edit the sign role (which bounds allowed principals + cert TTL +
# extensions), and cannot reach any other secret. Compromise of the worker yields the ability to mint a
# certificate that EXPIRES ON ITS OWN, bounded by the role's max_ttl — never the CA key, and never a cert for
# a principal the role does not allow.
#
# A signed certificate is STATELESS: unlike the database engine there is no lease to renew or revoke, so this
# policy needs no sys/leases capability. The cert simply expires at its TTL.

# SIGN: mint a short-lived user certificate. Signing is an update on the role's sign endpoint.
path "ssh-client-signer/sign/tg-actuate" {
  capabilities = ["update"]
}

# EVERYTHING ELSE IS DENIED, explicitly, so a future broadening of a parent path cannot silently grant it.
#
# The CA CONFIG holds the private signing key — the crown jewel. A token that could read or write it could
# forge certificates for any principal, not just what the role allows.
path "ssh-client-signer/config/*" {
  capabilities = ["deny"]
}
# The ROLE bounds allowed_users, ttl, and extensions. Editing it (e.g. widening allowed_users or adding a
# permit-* extension) is how a leased cert's authority would be escalated.
path "ssh-client-signer/roles/*" {
  capabilities = ["deny"]
}
# The KV tree and everything else is untouchable from this identity.
path "secret/*" {
  capabilities = ["deny"]
}
path "sys/*" {
  capabilities = ["deny"]
}
path "auth/*" {
  capabilities = ["deny"]
}
