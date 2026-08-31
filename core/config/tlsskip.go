package config

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
)

// tlsskip.go — IS THIS TLS-VERIFICATION SKIP ACTUALLY NECESSARY? (TG-367)
//
// THE MEASURED GAP. TG addressed Proxmox as `https://dc1pve01:8006` and therefore ran with
// TG_PVE_INSECURE=true on the triage plane and TG_PROXMOX_INSECURE=1 on the actuation plane. Both
// planes logged, in prose, that this was "the usual setting for a PVE node serving its self-signed
// cert on :8006".
//
// The endpoint was not serving a self-signed cert. It serves a publicly-trusted Let's Encrypt
// wildcard (`CN=*.example.net`). The short hostname simply cannot match a wildcard — there is
// no domain to wildcard against — so the handshake failed on HOSTNAME MISMATCH and someone turned
// verification off. Addressing the same IP as `dc1pve01.example.net:8006` verifies.
//
// For however long that stood, whatever answered on that address received TG's Proxmox tokens: the
// estate READ token on triage, and the guest-lifecycle WRITE token on actuation.
//
// WHY A MEASUREMENT AND NOT A COMMENT. The prose was confident, plausible, reviewed, and wrong, and
// nothing in the system could contradict it — a skip flag is honoured silently and looks identical
// whether it is load-bearing or vestigial. SkipIsNecessary answers the question the comment was
// guessing at: dial the endpoint WITH verification and see. A skip that survives this probe is a real
// workaround for a real self-signed endpoint (TG's two LibreNMS deployments genuinely are, verified
// 2026-08-06). A skip that fails it is an unnecessary hole, and the boot log can now say so.
//
// This is deliberately NOT a gate. It reports; it never refuses a boot and never flips the flag. A
// probe that can take the deployment down on a transient network fault would be a worse defect than
// the one it detects, and the fix is a config change an operator must make deliberately.

// TLSSkipVerdict is the result of asking whether an insecure-transport flag is earning its keep.
type TLSSkipVerdict struct {
	Endpoint  string // the endpoint as configured, host:port, no scheme
	Necessary bool   // true when a verifying handshake genuinely fails
	Reason    string // why — the verification error, or the fact that it verified fine
	Probed    bool   // false when the probe could not run at all (see Reason); Necessary is then meaningless
}

// String renders the verdict for a boot log, leading with the actionable case.
func (v TLSSkipVerdict) String() string {
	switch {
	case !v.Probed:
		return fmt.Sprintf("TLS-skip for %s: NOT PROBED (%s) — cannot say whether the skip is needed", v.Endpoint, v.Reason)
	case v.Necessary:
		return fmt.Sprintf("TLS-skip for %s: NECESSARY — %s", v.Endpoint, v.Reason)
	default:
		return fmt.Sprintf("TLS-skip for %s: UNNECESSARY — %s. This skip is an open hole: whatever answers at "+
			"this address receives the credential TG sends it. Remove the insecure flag.", v.Endpoint, v.Reason)
	}
}

// SkipIsNecessary dials endpoint with full verification and reports whether the skip is earned.
//
// endpoint may be a full URL or a bare host[:port]; a missing port defaults to 443. dialTLS is
// injected so the decision logic is testable without a network — production passes nil.
func SkipIsNecessary(ctx context.Context, endpoint string, dialTLS func(ctx context.Context, hostPort, serverName string) error) TLSSkipVerdict {
	hostPort, serverName, err := splitEndpoint(endpoint)
	v := TLSSkipVerdict{Endpoint: hostPort}
	if err != nil {
		// Unparseable config is not evidence either way. Say so rather than defaulting to "necessary",
		// which would quietly bless every skip behind a typo.
		v.Reason = err.Error()
		return v
	}
	if dialTLS == nil {
		dialTLS = verifyingDial
	}
	v.Probed = true
	if err := dialTLS(ctx, hostPort, serverName); err != nil {
		v.Necessary = true
		v.Reason = fmt.Sprintf("a verifying handshake fails: %v", err)
		return v
	}
	v.Reason = "a verifying handshake SUCCEEDS against this endpoint"
	return v
}

// verifyingDial performs a real handshake with the system trust store and default verification.
func verifyingDial(ctx context.Context, hostPort, serverName string) error {
	d := &tls.Dialer{Config: &tls.Config{ServerName: serverName, MinVersion: tls.VersionTLS12}}
	dctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	c, err := d.DialContext(dctx, "tcp", hostPort)
	if err != nil {
		return err
	}
	return c.Close()
}

// splitEndpoint accepts a URL or a bare host[:port] and returns the dial target and the SNI name.
func splitEndpoint(endpoint string) (hostPort, serverName string, err error) {
	// Trimmed before the emptiness check: a whitespace-only value is a blank config field, and treating
	// it as a host produced a confident "the skip is UNNECESSARY" verdict about the endpoint "   ".
	// Caught by this file's own vacuity floor.
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return "", "", fmt.Errorf("endpoint is empty")
	}
	raw := endpoint
	if u, uerr := url.Parse(endpoint); uerr == nil && u.Host != "" {
		raw = u.Host
	}
	host, port, serr := net.SplitHostPort(raw)
	if serr != nil {
		host, port = raw, "443"
	}
	host = strings.TrimSpace(host)
	if host == "" {
		return "", "", fmt.Errorf("endpoint %q has no host", endpoint)
	}
	return net.JoinHostPort(host, port), host, nil
}
