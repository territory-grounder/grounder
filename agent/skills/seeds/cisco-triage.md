## Cisco ASA/IOS triage
This skill sharpens WHICH fault and the SHAPE of the fix on a Cisco incident. It does NOT lower the risk
band — the band is machine-enforced and every proposal is poll-by-default. Cisco is an INTERACTIVE CLI, not
a shell: read state through the show catalog, never assume a config write applied without confirming it.

DIAGNOSE (read-only show ladder — always allowed):
  - VPN/tunnel down: 'show crypto ipsec sa' (SA up? encaps/decaps incrementing?), 'show crypto ikev2 sa'
    (IKE phase-1), 'show vpn-sessiondb' — then the peer/PSK/transform-set, in that order. A BGP-up over the
    tunnel can MASK an ACL-binding drift, so check 'show access-list <name>' hitcounts even when BGP is fine.
  - ACL / access problem: 'show access-list' with hitcounts. An ACE appended BELOW a deny matches NOTHING
    (hitcnt 0) — order is the bug, not the rule. An fqdn object that NXDOMAINs silently blocks.
  - ASABindingDrift / config drift: compare the golden fingerprint to 'show running-config'; the drift is
    the finding, not a thing to auto-correct.
  - interface/BGP: 'show interface', 'show bgp summary' — a deliberately security-shut interface staying
    down is CORRECT, not a fault.

PROPOSAL SHAPE (each mutation verb has an explicit inverse; name the fix, do not execute — the write lane
ships dark until the operator arms it, so these are shadow-proposal shapes for operator review):
  interface shut/no-shut (IOS 'shutdown'/'no shutdown', ASA 'shut'/'no shut' — the verb DIFFERS by device);
  ACL 'line N' add/remove; object-group member add/remove; 'shun <ip>'/'no shun' (EXEC, non-persistent,
  invisible to config-diff — verify with 'show shun'); route add/'no route'. Reversibility-by-not-saving:
  never 'write memory' a security-affecting change until it is verified.

NEVER auto (human poll, always — the Cisco never-auto floor):
  reload / 'reload in'; 'write erase' / 'clear configure all'; any crypto change (ipsec/ikev2/map, tunnel
  PSK, cert/trustpoint) — a crypto-map peer change is delete+recreate, an in-place edit leaves a stale IKEv2
  SA; 'no access-group' / unbinding a SECURITY ACL; no-shut of a deliberately-security-shut interface;
  'clear shun' with NO arg (releases ALL blocks); boot/format/delete flash:; shun/deny of an IP in the
  infra never-shun set (routers, VTI /30s, VPS loopbacks, overlay /24s). When none of the read-only ladder
  points to a safe reversible proposal, STOP and name what a human needs from the show output.
