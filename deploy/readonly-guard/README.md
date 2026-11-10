# tg-readonly-guard

Constrains TG's host-**diagnostics** key in the target host's own sshd, so a stolen key can run TG's
read-only grammar and nothing else.

## Why it exists

TG read every estate host as **root** using `/secrets/one_key` — the unrestricted estate root key, mode
`0640 root:65532`, mounted readable into a worker that also processes untrusted alert and syslog content.
One `/secrets` read handed an attacker root on the whole fleet, with every TG gate bypassed, because
TG's gates bind only the commands TG constructs. The same key is AWX's estate credential.

This is the read-side twin of `deploy/actuation-guard/`, and it works the same way: `restrict,command=`
in `authorized_keys`, byte-exact allowlist matching, `exit 42` on refusal.

## Install

```sh
# 1. Generate the allowlist FROM the catalogue. Never hand-author it.
go run ./tools/readallow -log-paths "/var/log/syslog" > generated.allow

# 2. On each host, as root:
./install.sh --allow generated.allow --pubkey tg-hostdiag.pub
```

## Verify

```sh
ssh -i tg-hostdiag root@HOST uptime         # allowed  -> output
ssh -i tg-hostdiag root@HOST id             # refused  -> exit 42
ssh -i tg-hostdiag root@HOST                # refused  -> no shell
```

`exit 42` distinguishes a refusal from an unreachable host. That distinction is load-bearing: the
diagnostics lane reports a failed read as `(<host> was unreachable or the read errored)`, and TG-271 is
the record of what happens when the agent cannot tell blind from quiet.

## Do not hand-edit the allowlist

`tools/guardallow` exists because the *actuation* allowlist was hand-authored, drifted from the op-class
registry, and TG chose an action that cleared all six of its own gates and then died at the host with
exit 42. Here the drift would be quieter and therefore worse: a denied read looks like an ordinary
answer. Regenerate from the catalogue instead.
