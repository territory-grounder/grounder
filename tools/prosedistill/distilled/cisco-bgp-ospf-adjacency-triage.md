## Goal
Reading the BGP and OSPF neighbor state machines correctly before guessing at a cause on IOS or ASA
routed mode — a stuck state names its own likely fault, and most "routing is down" incidents are
answered by the specific state a neighbor is stuck in, not a fresh packet capture. NOTE — tool-gated
content: TG has no Cisco CLI tool wired yet (the interactive-SSH transport is separate, not-yet-built
work); this runbook is knowledge-library material until a vendor-official, read-only surface lands. Every
command below is read-only diagnostic guidance.

## Required evidence
- BGP: `show ip bgp summary` — the State/PfxRcd column: a number means Established (working, receiving
  prefixes); any word (Idle, Connect, Active, OpenSent, OpenConfirm) is a stuck pre-Established state that
  names the layer to check next.
- BGP: `show ip bgp neighbors <ip>` — full detail: last-reset reason, negotiated hold time, and the
  underlying TCP connection state beneath the BGP state itself.
- OSPF: `show ip ospf neighbor` — the STATE column: FULL is the only converged state; Down, Attempt, Init,
  2-Way, ExStart, Exchange, and Loading are each a specific stuck point, not interchangeable "not working."
- OSPF: `show ip ospf interface <if>` — hello/dead timers, area, and network type actually configured on
  this side, to diff against the neighbor's.
- `show ip protocols` — confirms the process is even configured to speak to the expected neighbor, area,
  or AS at all; a neighbor left out of the process configuration never appears as a "stuck" state, it
  simply never appears.

## Decision rules
- BGP stuck in Idle or Active (not yet Connect/OpenSent) most often means no TCP reachability yet — check
  the underlying path (an ACL, NAT, a down interface, or a wrong neighbor IP/remote-AS) before touching
  BGP configuration at all; BGP here is reporting a Layer 3/4 problem, not a BGP one.
- BGP stuck in OpenSent/OpenConfirm points at a parameter mismatch the two sides can't agree on (AS
  number, BGP identifier/router-id collision, capability negotiation) — read the exact notification in
  `show ip bgp neighbors <ip>` (or the `%BGP-3-NOTIFICATION` log line) rather than re-guessing the
  neighbor statement from scratch.
- OSPF stuck in INIT: this router sees the neighbor's hellos, but the neighbor is not seeing this one's —
  very often a one-way ACL, a hello/dead timer mismatch, or mismatched authentication, not a fully-down
  link (traffic in the other direction may look completely fine).
- OSPF stuck in EXSTART/EXCHANGE: near-always an MTU mismatch on the shared segment (DBD packets cannot
  fully exchange) — confirm with a large, don't-fragment ping between the two OSPF-enabled interfaces
  before touching any other configuration.
- A neighbor that never appears at all (not even in a stuck state) is a configuration-scope problem —
  wrong area, wrong network statement, wrong AS in the neighbor line, or the process not enabled on the
  interface — check `show ip protocols` and interface process membership before assuming a live-state
  fault.
- Convergence flapping (repeatedly reaching FULL/Established then dropping) is a stability problem, not a
  one-time configuration problem — check for an unstable underlying link, a duplicate router-id/BGP
  identifier on the segment, or a timer too aggressive for the path, rather than re-applying whatever
  already "worked" once.

## Verification
- `show ip bgp summary` / `show ip ospf neighbor` shows the expected steady state (Established / FULL)
  held stable across at least one full hold/dead-timer interval, not just an instantaneous read right
  after the fix.
- The specific stuck-state cause identified (MTU, ACL, timer, AS/router-id mismatch) is named in the
  record, not just "adjacency restored."
- No new `%BGP-3-NOTIFICATION` or OSPF adjacency-change log entries appear for the peer after the fix.

## Doc basis
- Cisco: Troubleshoot BGP Neighborship Connection Issues —
  https://www.cisco.com/c/en/us/support/docs/ip/border-gateway-protocol-bgp/13752-24.html
  (BGP state machine, common Idle/Active causes).
- Cisco: Troubleshoot Common BGP Issues —
  https://www.cisco.com/c/en/us/support/docs/ip/border-gateway-protocol-bgp/22166-bgp-trouble-main.html
- Cisco: Why Does the show ip ospf neighbor Command Reveal Neighbors in the Init State? —
  https://www.cisco.com/c/en/us/support/docs/ip/open-shortest-path-first-ospf/13702-7.html
- Cisco: Troubleshoot OSPF Neighbors Stuck in Exstart/Exchange State —
  https://www.cisco.com/c/en/us/support/docs/ip/open-shortest-path-first-ospf/13684-12.html
- Cisco: Troubleshoot Common Problems with OSPF —
  https://www.cisco.com/c/en/us/support/docs/ip/open-shortest-path-first-ospf/12151-trouble-main.html
