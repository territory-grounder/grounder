# tg-syslogng-guard

Pins TG's syslog-ng **read** key to exactly two commands, in the syslog host's own sshd.

## Why this one is a binary

Its siblings are shell scripts and can be: `deploy/actuation-guard/` and `deploy/readonly-guard/` match
`SSH_ORIGINAL_COMMAND` **byte-for-byte** against a generated allowlist, so by the time they `eval`, the
string is provably one of N vetted lines.

This lane cannot work that way. `search-host-logs` sends a **free-text grep pattern**:

```
'tail' '-n' '<lines>' '--' '<path>'
'grep' '-F' '-m' '<hits>' '--' '<pattern>' '<path>'
```

The command set is not enumerable, so the guard must validate **shape**, which means parsing untrusted
input *before* the gate. In POSIX shell that needs either `eval` on attacker-controlled input (arbitrary
execution) or a hand-rolled quote parser — the class of code this guard exists to defend against. So it is
Go: parse, validate, then `syscall.Exec` an argv **vector**. No shell exists at any point, so no
metacharacter in the pattern can mean anything.

## What it enforces

- binary is exactly `tail` or `grep`, resolved from a fixed directory list (not `$PATH`)
- exactly the two shapes above — no extra flags, no missing `--`
- numeric args are plain decimals within a cap (2000)
- the path resolves **inside** the log base *after* symlink resolution
- the pattern is never inspected: `-F` makes it literal, it sits after `--` so it cannot be read as a
  flag, and there is no shell for it to be a token in

## Install

```sh
# ALWAYS build with the stamp. Without -ldflags the binary reports `unstamped`, and an unidentifiable
# binary is how the TG-363 fix sat merged-but-undeployed on both syslog hosts for a day with nothing able
# to notice: this is a stripped static binary, so there is no other string in it to compare with the tree.
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build \
  -ldflags "-X main.buildStamp=$(git rev-parse --short HEAD)" \
  -o tg-syslogng-guard ./tools/syslogngguard
scp tg-syslogng-guard root@HOST:/usr/local/sbin/
ssh root@HOST 'chmod 0755 /usr/local/sbin/tg-syslogng-guard'

# Confirm WHICH BUILD is live — run this after every rollout, and to answer "is the host current?":
ssh root@HOST '/usr/local/sbin/tg-syslogng-guard -version'   # prints the git sha it was built from
# then in root's authorized_keys, in front of the syslog-ng key:
#   restrict,command="/usr/local/sbin/tg-syslogng-guard" ssh-ed25519 AAAA... tg-syslogng
```

Override the log base with `TG_SYSLOGNG_GUARD_BASE` (default `/mnt/logs/syslog-ng`).

## Verify

```sh
ssh -i tg-syslogng root@HOST "'tail' '-n' '5' '--' '/mnt/logs/syslog-ng/<host>/<file>.log'"   # output
ssh -i tg-syslogng root@HOST "'id'"                                                            # exit 42
ssh -i tg-syslogng root@HOST "'tail' '-n' '5' '--' '/etc/shadow'"                              # exit 42
ssh -i tg-syslogng root@HOST                                                                   # exit 42
```

`exit 42` is the shared refusal signal across all three guards, so a caller can tell a refusal from an
unreachable host. That distinction is load-bearing: these lanes report a failed read as
`(host was unreachable or the read errored)`, and TG-271/TG-300 are the record of what it costs when the
agent cannot tell blind from quiet.
