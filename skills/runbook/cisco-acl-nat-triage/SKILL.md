---
name: cisco-acl-nat-triage
class: runbook
version: 0.1.0-distilled
source: distill:https://www.cisco.com/c/en/us/support/docs/security/asa-5500-x-series-next-generation-firewalls/116388-technote-nat-00.html
description: Cisco IOS and ASA ACL and NAT misconfiguration triage — hit counts over intuition, packet-tracer before touching a rule, implicit-deny and NAT-address gotchas
---

## Goal
Triage judgment plus the exact IOS and ASA commands for "an ACL is blocking something it shouldn't" or
"NAT isn't translating right" — hit counts over intuition, and the packet-trace tools that show the
actual per-packet verdict instead of a config read and a guess. NOTE — tool-gated content: TG has no
Cisco CLI tool wired yet (the interactive-SSH transport is separate, not-yet-built work); this runbook is
knowledge-library material until a vendor-official, read-only surface lands. Every command below is
read-only diagnostic guidance.

## Required evidence
- IOS: `show access-lists` (or `show ip access-list <name>`) — each ACE's own match counter, printed as
  `(N matches)`; `show ip interface <if>` and `show running-config interface <if>` to confirm which ACL
  is bound, in which direction, on the interface actually carrying the traffic.
- ASA: `show access-list <name>` — per-ACE `hitcnt` counters; `show running-config access-group` for the
  interface binding and direction; object-group membership (`show running-config object-group <name>`)
  since a group edit changes ACL behavior with no ACL rewrite visible.
- ASA NAT: `show nat` and `show nat detail` — `translate_hits` / `untranslate_hits` per rule; `show xlate`
  for the live translation table (which flows are actually translated right now, not just configured to
  be).
- ASA `packet-tracer input <if> <protocol> <src-ip> <src-port> <dst-ip> <dst-port>` — the single most
  decisive tool here: a phase-by-phase verdict (ACCESS-LIST, NAT, ROUTE-LOOKUP, and more) ending in an
  explicit allow or drop naming the exact rule that decided it.

## Decision rules
- Hit count first, always: an ACE with zero matches after traffic that should hit it has run is either
  never reached (an earlier ACE already matched — first-match-wins, order matters) or never applied in
  the right place or direction. Confirm which rule is even being evaluated before debugging the rule's
  own logic.
- Implicit deny is invisible: an IOS ACL with no explicit trailing `permit`/`deny ip any any` still ends
  in an unwritten deny-all with no counter of its own to read on many platforms — traffic silently
  falling through to that invisible line looks identical to "the ACL just isn't doing anything."
- On ASA, run `packet-tracer` with the exact reported 5-tuple before touching any config — it names the
  deciding phase and rule ID directly, and it also catches NAT, routing, and inspection interactions that
  an ACL-only read would miss entirely.
- NAT-before-ACL address trap: an ACE authored against the wrong address family (the real address instead
  of the translated one, or vice versa, depending on where in the path the ACL is applied) is a classic
  silent-match-nothing bug — if a NAT'd flow's ACL entry never accumulates hits, check which address the
  ACE was actually written against.
- A NAT rule reading `translate_hits = 0` is not yet proven broken — confirm traffic that should exercise
  it was actually sent (via `packet-tracer` or a live test) before concluding the rule is misconfigured; a
  correct-but-unexercised rule and a broken rule look identical from the counter alone.
- Object-group edits take effect immediately with no ACL line changing — "the ACL looks unchanged" does
  not rule out a group membership edit as the cause; check the group's own content and history.

## Verification
- Re-run `packet-tracer` (ASA) or an equivalent live test with the exact reported 5-tuple and confirm the
  phase-by-phase verdict now gives the intended outcome (allow, or deny if the fix closed something).
- The specific ACE/NAT-rule counter that should be hit increments on a real retest, not just "the config
  looks right now."
- The interface binding and direction (`show running-config access-group` / `show ip interface`) still
  name the exact ACL you edited — confirm a fix wasn't applied to the wrong ACL name or number.

## Doc basis
- Cisco: Configure IP Access Lists —
  https://www.cisco.com/c/en/us/support/docs/security/ios-firewall/23602-confaccesslists.html
  (IOS ACL syntax, direction/binding, implicit deny).
- Cisco: Configure ASA Access Control List for Various Scenarios —
  https://www.cisco.com/c/en/us/support/docs/security/adaptive-security-appliance-asa-software/217679-asa-access-control-list-configuration-ex.html
  (ASA named ACLs, object-groups, `access-group` binding).
- Cisco: Troubleshoot ASA Network Address Translation (NAT) Configuration —
  https://www.cisco.com/c/en/us/support/docs/security/asa-5500-x-series-next-generation-firewalls/116388-technote-nat-00.html
  (`show nat`, `show xlate`, `packet-tracer` for NAT verdicts).
