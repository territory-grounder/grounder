package librenms

import (
	"net/netip"
	"strings"
)

// deviceName picks the estate NAME for a LibreNMS device (TG-628).
//
// LibreNMS's `hostname` is whatever the device was ADDED by. Most of this estate was added by name, but a
// device added by IP address carries that IP as its hostname (measured 2026-11-06: GR device 62 is hostname
// 192.168.2.44, sysName dc2librespeed01). TG keyed the incident on the IP, so every downstream identity —
// the carve-out, known_hosts, the per-target actuation leaf, guest_liveness, the agent's own host tools — saw a
// host that does not exist by that name, and the live G2 drill's service-down stopped at "no corroborating
// evidence". The name is in the same payload: when hostname is a bare IP and sysName is set, sysName wins
// (its short form, the way the estate names hosts). A real hostname is never replaced.
func deviceName(hostname, sysName string) string {
	h, s := strings.TrimSpace(hostname), strings.TrimSpace(sysName)
	if h == "" {
		return shortName(s)
	}
	if _, err := netip.ParseAddr(h); err == nil && s != "" {
		if _, serr := netip.ParseAddr(s); serr != nil { // a sysName that is itself an IP adds nothing
			return shortName(s)
		}
	}
	return h
}

// shortName drops the domain from an FQDN sysName ("dc2librespeed01.example.net" ⇒ "dc2librespeed01").
func shortName(s string) string {
	if i := strings.IndexByte(s, '.'); i > 0 {
		return s[:i]
	}
	return s
}
