## Goal
Triage judgment plus the exact IOS and ASA commands for "an interface or its line protocol is down" —
read the status PAIR before believing a cable story, name the err-disable cause before clearing it, and
never no-shut into a fault nobody has identified yet. NOTE — tool-gated content: TG has no Cisco CLI
tool wired yet (the interactive-SSH transport is separate, not-yet-built work); this runbook is
knowledge-library material until a vendor-official, read-only surface lands. Every command below is
read-only diagnostic guidance — config changes stay human-executed until that surface and the never-auto
floor land.

## Required evidence
- IOS: `show interfaces <if>` for the full physical-plus-protocol picture, including input/output error
  counters (CRC, collisions, giants/runts) that distinguish a marginal physical link from a clean one.
- IOS: `show ip interface brief` for the fleet-wide status/protocol pair across every interface at once —
  whether the fault is one port or many tells you whether to look at the port or upstream of it.
- IOS (Catalyst-style): `show interfaces status` for the connect/notconnect/disabled/err-disabled summary,
  and when a port reads err-disabled, `show interfaces status err-disabled` plus the specific reason.
- ASA: `show interface <if>` and `show interface ip brief` for the same status/protocol pair; also
  `show nameif` and `show running-config interface <if>` — an ASA interface can be physically up/up and
  still pass nothing because it has no `nameif`/security-level assigned yet.
- The remote end's own view of the same link where it is reachable: a local protocol-down against a
  remote up/up points at a local Layer 2 negotiation problem, not a cable.

## Decision rules
- Read the status pair, not one word: "up, line protocol is up" is fully operational; "up, line protocol
  is down" is a Layer 2/negotiation problem with the physical layer already confirmed fine — don't reach
  for a cable swap first. "down, line protocol is down" is physical (cable, SFP, remote shutdown).
  "administratively down" means something (a person or automation) shut it — check the change history
  before assuming a fault at all.
- Err-disabled ports: ALWAYS read the specific cause (`show interfaces status err-disabled`) before
  recovering, whether by `errdisable recovery cause <x>` or manual `shutdown` / `no shutdown`. A repeat
  security-violation or loop-guard trip is signal, not noise — clearing it blind hides the exact problem
  it will reproduce on the next trigger.
- ASA-only: a physically up/up interface passing no traffic is very often a missing `nameif` or
  security-level, or same-security-level traffic being denied by default — check config, not just link
  state, before escalating to a hardware theory.
- Many ports down together on the same switch or blade points upstream first — shared power, a
  supervisor, or an uplink — before any single-port root cause. Simultaneous multi-port alerts are a
  scope question, not N independent incidents.
- A protocol-down on a point-to-point or tunnel-style link where the physical layer is confirmed up
  usually means an encapsulation, keepalive, or MTU mismatch between the two ends — compare the
  configuration on both sides rather than re-testing the same end twice.

## Verification
- `show interfaces <if>` (or the ASA equivalent) reads up/up — or the intended state, for a deliberately
  administratively-down port — after the fix, not just immediately but held stable, not flapping.
- Any err-disable cause identified is named in the incident record, not just cleared silently.
- Error counters (CRC, collisions, input/output drops) are not still climbing on a retest — a fix that
  "took" stops incrementing them, not just clears the down state momentarily.

## Doc basis
- Cisco: Troubleshoot Switch Port and Interface Problems —
  https://www.cisco.com/c/en/us/support/docs/switches/catalyst-6500-series-switches/12027-53.html
  (status-pair meaning; duplex mismatch and cable causes).
- Cisco: Recover Errdisable Port State on Cisco IOS Platforms —
  https://www.cisco.com/c/en/us/support/docs/lan-switching/spanning-tree-protocol/69980-errdisable-recovery.html
  (`show interfaces status err-disabled`, `show errdisable recovery`, per-cause recovery timers).
- Cisco: Troubleshoot ASA using CLI commands —
  https://docs.manage.security.cisco.com/c_troubleshoot-asa-using-cli-commands.html
  (`show interface`, `show interface ip brief` field meanings on ASA).
