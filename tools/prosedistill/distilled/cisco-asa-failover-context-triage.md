## Goal
ASA-specific triage for Active/Standby failover pairs and multiple-context deployments — detect
split-brain before forcing a failover, and run every context-scoped command from the right prompt so the
answer is about the firewall instance actually in question. NOTE — tool-gated content: TG has no Cisco
CLI tool wired yet (the interactive-SSH transport is separate, not-yet-built work); this runbook is
knowledge-library material until a vendor-official, read-only surface lands. Every command below is
read-only diagnostic guidance; the two write commands named in Decision rules are documented for
diagnostic completeness only and are never auto-executed.

## Required evidence
- `show failover` — the full state report: local/peer role (Active/Standby), per-interface monitoring
  status, and the failover link's own state.
- `show failover state` — the compact Peer/Grp/State summary, the fastest read for "which unit is active
  right now."
- `show failover history` — the timestamped sequence of state transitions with a reason per transition
  (for example a stated configuration mismatch, an interface failure, or a manual command) — the reason
  is usually more useful than the state alone.
- Multiple context mode, from the system execution space: `show context [detail]` — every configured
  context, its assigned interfaces, and the active/total context count.
- `changeto context <name>` to move into a context's own execution space BEFORE running any context-scoped
  show command — `show nameif`, `show interface`, `show conn`, and similar are answered per-context.
- `show running-config context <name>` (from system space) — a context's assigned interfaces and
  config-file location, for confirming a context's boundary without switching into it.

## Decision rules
- Split-brain check FIRST, before commanding any failover: run `show failover state` on BOTH units — if
  both report Active, this is split-brain (the failover link itself is down, or the peers otherwise
  cannot see each other), and forcing a failover on either unit does not fix it and can make the
  network-facing symptom worse (duplicate IP/MAC on the segment). The fix is restoring the failover
  link/heartbeat, not commanding a role change.
- A standby unit reporting "Cold Standby" (as opposed to "Standby Ready") in `show failover history` is
  not proof of a peer hardware failure by itself — the recorded reason is very often a stated
  configuration mismatch between the units; read the reason before assuming a link/hardware fault.
- Never run a context-scoped command from the system execution space, or from the wrong context, and
  trust the answer — `changeto context <name>` first; an "the interface looks fine" read taken from the
  wrong context is answering about the wrong virtual firewall entirely.
- Interface monitoring in a failover pair triggers role changes on its OWN criteria (a monitored interface
  losing link) independent of overall device health — an interface-triggered failover with the newly
  active unit otherwise healthy is working as designed, not a fault to reverse immediately; confirm which
  monitored interface tripped it before reacting.
- A manual failover (`no failover active` on the active unit, or `failover active` from standby) is a
  deliberate, disruptive action on a live pair — existing sessions may not all survive the role change.
  Treat it with the same care as any other actuation: confirm which unit you are actually on and which
  direction you are commanding, and expect a brief connection-state impact even in a healthy pair. This
  command is named here for diagnostic completeness only; TG does not execute it.
- Configuration sync is push-from-active by design — an edit made on (or believed to apply to) the standby
  unit directly does not persist and is overwritten by the next sync from active. All configuration
  changes belong on the active unit, or in the system context for items scoped there.

## Verification
- `show failover state` agrees on BOTH units about which one is active — never conclude "healthy" from
  reading only one side.
- `show failover history`'s most recent transition reason is understood and named, not just "state is now
  Active/Standby as expected."
- For a multi-context change, the verification commands were run after `changeto context <name>` into the
  SPECIFIC context the change targeted, confirmed by the resulting prompt before trusting the output.

## Doc basis
- Cisco: CLI Book 1, ASA General Operations CLI Configuration Guide — Failover for High Availability —
  https://www.cisco.com/c/en/us/td/docs/security/asa/asa919/configuration/general/asa-919-general-config/ha-failover.html
  (`show failover`, `show failover state`, failover states, manual failover commands).
- Cisco: Troubleshoot Split-Brain Issues on ASA Failover —
  https://www.cisco.com/c/en/us/support/docs/security/adaptive-security-appliance-asa-software/217691-troubleshoot-split-brain-issues-on-asa-f.html
- Cisco: ASA FAQ — Why does show failover history indicate a configuration mismatch? —
  https://www.cisco.com/c/en/us/support/docs/security/asa-5500-x-series-next-generation-firewalls/117906-qanda-asa-00.html
- Cisco: CLI Book 1, ASA General Operations CLI Configuration Guide — Multiple Context Mode —
  https://www.cisco.com/c/en/us/td/docs/security/asa/asa919/configuration/general/asa-919-general-config/ha-contexts.html
  (`show context`, `changeto context`).
