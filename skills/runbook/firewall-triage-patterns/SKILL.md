---
name: firewall-triage-patterns
class: runbook
version: 0.1.0-distilled
source: distill:.claude/agents/cisco-asa-specialist.md
description: Perimeter-firewall triage judgment — uplink first on multi-site bursts, reboot-window awareness, protection-state cleanup, delete-then-recreate on stateful config
---

## Goal
Triage judgment for perimeter-firewall incidents: which cause to suspect first, which state survives a
recovery to bite later, and which edit shapes are unsafe on stateful security config. NOTE —
tool-gated content: TG has no firewall CLI tools wired; this runbook is knowledge-library material
until a vendor-official, read-only surface lands. High-risk network devices are re-author-only and
floored at never-auto per ADR-0012.

## Required evidence
- Device uptime — held against any known scheduled-reboot timer for the device (many estates run
  watchdog reboots on a fixed cadence; the timer PREDICTS an outage window).
- Tunnel/session state per peer, with traffic counters in both directions — a tunnel "up" with a
  one-directional counter is half-broken.
- WAN uplink state for each provider path, checked FIRST when alerts from both ends of a site link
  fire simultaneously.
- The device's own recent log — protection events (shuns/blocks), negotiation failures, interface
  flaps.
- `get-estate-context <host>` and `get-incident-history <host>` for topology position and precedent.

## Decision rules
- Simultaneous multi-site alerts point at the shared WAN leg before any device: check the uplink's own
  state (address assignment present, physical path up) before diagnosing the boxes behind it.
- A reboot inside a known scheduled window is the explanation, not a fault — but verify the window
  from the timer, and treat an OFF-schedule reboot as a real incident (see
  scheduled-event-suppression).
- After ANY recovery, clear the device's protection state: threat-detection systems hold grudges —
  peers blocked during the outage stay blocked after it, turning a recovered fault into a lingering
  partial one. The post-recovery checklist includes the protection table, not just the tunnels.
- Stateful security config is edited by DELETE-then-RECREATE, never in place: in-place edits to peer
  lists and matching tables leave stale compiled state that fails traffic long after the config reads
  correct.
- Where automation syncs device config to a repository, the LIVE DEVICE is the source of truth and the
  sync direction is device-to-repo; pre-writing the repo creates drift the sync then fights.
- Access-path constraints are facts of the estate (a device reachable only through a specific
  stepstone, only with legacy ciphers): record them in the estate graph and the brief's obstacles —
  an unreachable diagnosis path is a finding.

## Verification
- The record shows the uplink check preceding device-level diagnosis on any multi-alert burst.
- Post-recovery, the protection table was read and shown clean — or the entries found were cleared and
  named.
- Any config-change proposal on stateful security surfaces names the delete-then-recreate shape
  explicitly.
