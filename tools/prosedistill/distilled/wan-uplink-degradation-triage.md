## Goal
Decide whether a WAN/internet alert is an UPLINK/ISP fault — where the correct response is failover and
escalation, never touching anything downstream — or an internal service fault, and size its blast radius
before proposing anything. The defining trap of this plane: a WAN interface can be PHYSICALLY UP (line
protocol up) while carrying NO usable service, because the session on top of it (a PPPoE session, a DHCP
lease, the ISP's own upstream) has died — so "the interface is up" is not "the internet works," and a single
uplink death can take down internet, EVERY cross-site VPN tunnel, and all downstream/tenant connectivity at
once. NOTE — plane-gated content: a WAN failover or an ISP escalation is a network-device / vendor-critical
action, never TG's to take from triage; everything below is read-only diagnosis whose output is a precise
hand-off that names the plane and the blast radius.

## Required evidence
- The WAN interface's TWO separate states: the physical/line-protocol state AND whether it holds a usable IP
  address. `line protocol up` with NO IP assigned is the signature of a session/ISP fault, not a cable/link
  fault — do not conflate them.
- The session on top of the interface: for a PPPoE uplink, the session state (`show vpdn` / `show pppoe
  session` — a session "deleted and pending reuse" or stuck re-dialing) and whether the modem/ONT is
  responding to the session-discovery packets (PADI/PADO); for a DHCP/static uplink, the lease/reachability
  to the ISP gateway. A modem/ONT that has stopped answering discovery is an upstream fault, not the router's.
- The BLAST RADIUS, read before any action: which S2S/VPN tunnels traverse this uplink, whether internet
  egress for downstream/tenant networks depends on it, and how many alerts it is fanning out into. A WAN
  fault presents as a STORM of unrelated-looking downstream alerts — recognise the common upstream cause.
- The BACKUP uplink's state AND its PARITY: is a second ISP present, is it up, and — critically — does it
  carry COMPLETE crypto-map / NAT-exemption / route parity with the primary? A backup that is online but not
  parity-complete CANNOT auto-compensate; that gap is the real single-point-of-failure behind "we have two
  ISPs."
- Degradation vs loss: latency/jitter/packet-loss (a quality problem — BGP timers may hold, services slow)
  is a different condition from a full session drop (an availability problem — failover territory).

## Decision rules
- Interface UP but no IP / dead session / modem-not-answering-discovery = an UPLINK/ISP fault. The fault is
  UPSTREAM of the router. Do NOT propose restarting any downstream service, guest, or tenant path — nothing
  there is broken; they are victims of the upstream loss.
- A full uplink loss WITH a parity-complete backup = a failover situation (a network-device action, human/
  escalation, never auto). A full loss with a backup that LACKS parity = the SPOF is exposed: the backup
  cannot carry the tunnels, and this is an escalation naming exactly which parity is missing.
- Degradation (latency/loss, not a full drop) = verify the routing session timers are holding and services
  are degrading gracefully; this is usually watch-and-report, not failover — flapping a healthy-but-slow
  uplink to a backup can be worse than the degradation.
- A storm of downstream alerts that all began at one timestamp on hosts sharing one uplink is ONE incident
  (the uplink), not many — correlate to the common cause before proposing anything per-host.

## Verification
- Recovery of the PRIMARY is the session re-establishing, not the interface coming up: the signal is the
  first real traffic/NAT build back out the primary interface (a fresh outbound connection log via it), and
  the ISP gateway answering again — an interface that is "up" again with the session still dead has not
  recovered.
- A failover is verified by PARITY on the backup: every S2S tunnel that traversed the primary is re-formed
  over the backup and every downstream NAT/egress path resolves — a backup that "took over" but drops a
  subset of tunnels is a partial failover that must be named as such.
- The blast-radius alerts clear at their SOURCE as the uplink returns (internet, VPN, downstream all
  recover together) — confirming they were one upstream fault, not many.

## Never do
- NEVER propose restarting, rebooting, or reconfiguring a downstream service, guest, container, or tenant
  path to "fix" a WAN/ISP fault — the fault is upstream and the downstream is healthy; you would add a second
  outage to the first.
- NEVER assume two ISPs means redundancy — verify the backup's crypto-map/NAT/route parity before treating a
  failover as automatic; the unverified-parity assumption is exactly what turns a survivable uplink loss into
  a multi-hour isolation.
- NEVER flap or reconfigure the WAN edge device from triage (a wrong change on the internet-facing router or
  firewall can cut the estate off entirely, including TG's own path) — that is the never-auto floor.

## Hand-off shape
Name the plane (uplink/ISP vs internal), the specific signature (line-up-no-IP / dead session / modem not
answering discovery / degradation-not-loss), the BLAST RADIUS (internet + which VPN tunnels + which
downstream networks), the backup's state AND parity gap if any, and the one thing a human must do (fail to a
parity-complete backup, or escalate the SPOF, or watch a degradation). The value is a correct upstream
attribution that stops anyone from chasing the downstream victims — not an action.
