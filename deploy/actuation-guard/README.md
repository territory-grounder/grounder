# tg-actuator-guard — server-side blast-radius floor for the actuation SSH key

**TG-156 / TG-153 Critical#1 / spec/022 T-022-3 (fast path).** TG's mode chokepoint, reversible-op allowlist,
never-auto floor and mutation breaker are all **application-layer** gates on commands *TG itself constructs*.
The raw `tg-actuator` private key is mounted readable in the worker; a worker compromise that reads it can
drive a **stock SSH client** to run **arbitrary** commands as `tg-actuator` on every host in its
`authorized_keys` — the HuggingFace worker→node→lateral pivot, with every TG gate bypassed. That is the single
highest-blast-radius finding.

This guard constrains the **key itself**, in each target's `sshd`, *below and independent of* TG's code: a
leaked `tg-actuator` key can run **only** TG's reversible actuation grammar (`systemctl restart|reload
<allowlisted-unit>`, `docker restart <allowlisted-container>`) and **nothing else** — no arbitrary command, no
shell, no forwarding, no other unit. It is defense-in-depth: even if every TG-side gate is bypassed, this holds.

## How it works

`modules/actuation/ssh/ssh.go` single-quotes each argv element, so the target receives
`SSH_ORIGINAL_COMMAND = 'systemctl' 'restart' 'nginx'`. The guard matches that **full string byte-for-byte**
against an operator-owned allowlist (`/etc/tg-actuator-guard.allow`). A match is provably one of the vetted
lines, so it is exec'd; **anything else is denied without evaluation** (no word-split / glob / command
substitution touches the untrusted input before the gate). Interactive sessions (empty command) are denied.

## Install (per actuation target host, as root — or via AWX/ansible)

```sh
UNITS="nginx nginx.service" CONTAINERS="" sh install.sh   # installs the guard + generates the allowlist
```

## Pin it as the forced command on the tg-actuator key

Edit ONLY the `tg-actuator` line in the target user's `~/.ssh/authorized_keys` (identify it by its comment /
fingerprint — never touch the operator's own admin key). Back up first, then prepend the restricting options:

```
restrict,command="/usr/local/sbin/tg-actuator-guard" ssh-ed25519 AAAA…tg-actuator… tg-actuator@TG-canary
```

`restrict` disables pty, agent/port/X11 forwarding and user-rc; `command="…"` forces the guard regardless of
what the client requests (the requested command is passed to the guard as `SSH_ORIGINAL_COMMAND`).

## Verify (the guard must ALLOW TG's grammar and DENY everything else)

```sh
# ALLOW — a real TG command string:
SSH_ORIGINAL_COMMAND="'systemctl' 'restart' 'nginx'" /usr/local/sbin/tg-actuator-guard   # runs; rc 0
# DENY — arbitrary command / shell / injection / non-allowlisted unit:
SSH_ORIGINAL_COMMAND="cat /etc/shadow"                  /usr/local/sbin/tg-actuator-guard   # refused; rc 42
SSH_ORIGINAL_COMMAND="'systemctl' 'restart' 'sshd'"     /usr/local/sbin/tg-actuator-guard   # refused; rc 42
ssh -i tg-actuator root@<target> 'id'                                                        # refused by sshd
```

Then fault-inject the allowlisted unit and confirm TG still heals it through the guard, and that the operator's
own admin key is unaffected. The allowlist MUST stay in sync with the worker's `TG_ACTUATION_ALLOWED_UNITS/
_CONTAINERS` — the guard is the server-side mirror of that same allowlist (belt to TG's suspenders).

## Fuller fix (T-022-3, tracked separately)

This is the fast, zero-TG-code blast-radius cut. The complete remediation replaces the standing key with a
just-in-time, bounded-lifetime SSH **certificate** (SSH-CA / OTP, TTL seconds) so the credential is not a
standing secret at all — see spec/022 REQ-2202 and `modules/actuation/ssh/jit.go`.
