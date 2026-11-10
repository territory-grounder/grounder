## Goal
The exact ASA commands and phase-by-phase reading for "the site-to-site VPN tunnel is down" — Phase 1
before Phase 2, counters over a bare "up" flag, and generating real interesting traffic before blaming
the peer. Companion to firewall-triage-patterns (the judgment layer this pack supplies the command layer
for). NOTE — tool-gated content: TG has no Cisco CLI tool wired yet (the interactive-SSH transport is
separate, not-yet-built work); this runbook is knowledge-library material until a vendor-official,
read-only surface lands. Every command below is read-only diagnostic guidance.

## Required evidence
- `show crypto ikev2 sa` (or `show crypto isakmp sa` for IKEv1) — Phase 1. A `READY` status entry for the
  peer means Phase 1 completed; no entry at all means Phase 1 never completed, and nothing past this
  point can be a Phase 2 problem.
- `show crypto ipsec sa` (optionally `peer <ip>`) — Phase 2. Per-peer SAs with `encaps`/`decaps` packet
  counters, read TOGETHER: an SA present with `encaps` incrementing but `decaps` flat is a half-open
  tunnel — traffic is leaving but nothing is coming back — and reads as "up" on a status board while
  actually broken in one direction.
- `show vpn-sessiondb l2l` (add `detail` for the fuller per-SA breakdown) — the session-level view: tunnel
  duration and bytes in/out, independent confirmation of whether any traffic has actually crossed since
  the tunnel formed.
- `show running-config crypto map` and `show running-config tunnel-group <peer>` — the configured
  proposal set, PSK/certificate binding, and the ACL defining "interesting traffic" for that peer, to
  diff against what the peer expects.

## Decision rules
- Phase 1 before Phase 2, always: don't debug IPsec SAs, ACL matching, or routing while
  `show crypto ikev2 sa` shows no Phase 1 SA for the peer — nothing downstream is reachable until Phase 1
  completes (peer unreachable on udp/500 or udp/4500, a PSK/identity mismatch, or a proposal mismatch are
  the usual causes at this layer).
- An IPsec SA existing is not proof the tunnel is "up" — read `encaps`/`decaps` together. Encaps-only
  incrementing points at a return-path problem on the peer's side (peer routing, a peer ACL, or NAT
  between the sites), not this ASA's own configuration.
- No traffic crossing a policy-based (crypto-map) tunnel is often not a crypto fault at all: the SA only
  builds in response to traffic matching the crypto ACL. If nothing has been sent that matches it, there
  may simply be no SA yet — generate real traffic (or a controlled ping sourced from inside) before
  concluding the tunnel is broken.
- Treat a peer-side "our end is fine" claim as one input, not the verdict — confirm locally with the
  commands above before escalating; a peer that only checked its own Phase 1 state has not ruled out a
  Phase 2 or a routing asymmetry.
- Delete-then-recreate on crypto-map peer or ACL edits, never in place: an in-place edit to a crypto-map
  peer list or its matching ACL leaves stale IKEv2 traffic-selector state that keeps failing Phase 2
  (TS_UNACCEPTABLE) even after the running config reads correct. Remove the entry, then re-add it.
- Post-recovery, check the protection/shun table: a peer address shunned by threat-detection during an
  outage does not clear itself when the tunnel recovers, and a "recovered" tunnel that stays half-broken
  is often exactly this.

## Verification
- `show crypto ikev2 sa` shows `READY` for the peer AND `show crypto ipsec sa` shows both `encaps` and
  `decaps` incrementing on a retest — a Phase-1-only check is not sufficient closure.
- `show vpn-sessiondb l2l` for the peer shows nonzero data in AND out since the fix, not just "session
  active."
- Any shun/block entry for the peer address, if present, was found and cleared, and that is named in the
  record.

## Doc basis
- Cisco: Troubleshoot IOS IKEv2 Debugs for Site-to-Site VPN with PSKs —
  https://www.cisco.com/c/en/us/support/docs/ip/internet-key-exchange-ike/115934-technote-ikev2-00.html
  (Phase 1/Phase 2 command sequence, debug flow).
- Cisco: Dynamic Site to Site IKEv2 VPN Tunnel Between an ASA and an IOS Router Configuration Example —
  https://www.cisco.com/c/en/us/support/docs/ip/internet-key-exchange-ike/118743-configure-asa-00.html
  (ASA-to-IOS IKEv2 command set).
- Cisco: CLI Book 3, ASA VPN CLI Configuration Guide — General VPN Parameters —
  https://www.cisco.com/c/en/us/td/docs/security/asa/asa922/configuration/vpn/asa-922-vpn-config/vpn-params.html
  (`show vpn-sessiondb` syntax and fields).
