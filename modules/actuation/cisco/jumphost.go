package cisco

// JUMP-HOST REACHABILITY (TG-85 component 2): reaching a device that is NOT routable from TG.
//
// Part of this estate's Cisco surface is only reachable through a site-local bastion — the GR ASA answers to
// a GR-local jump host and to nothing else. Without this, those devices are simply undiagnosable: the read
// transport would dial an address that does not route and time out, and the runbooks that name them could
// never run.
//
// THE SECURITY SHAPE THAT MATTERS. A jump host is, by construction, a machine-in-the-middle: every byte of
// the device session crosses it. So the hop is NOT a convenience wrapper around the dial — it is a second
// authenticated peer, and it is pinned exactly like the first:
//
//   - the jump host's OWN host key is verified against its OWN known_hosts (core/sshhost), and
//   - the DEVICE's host key is verified after the tunnel, over the tunnelled connection.
//
// Two independent verifications, neither weakened by the other. A compromised jump host can therefore drop or
// break the session, but it cannot impersonate the device: the device's key is checked end-to-end, through the
// tunnel, against a file TG holds. That is the whole reason this is written by hand rather than shelling out
// to `ssh -J` (which is also forbidden here — no shell, INV-02).
//
// FAIL CLOSED, ALWAYS. A partially-declared jump host (a host with no credential, no login, or no known_hosts)
// REFUSES the connection rather than falling back to a direct dial. The fallback is the dangerous default: it
// would silently turn "reach the ASA through the bastion" into "try to reach the ASA directly", which either
// fails confusingly or — worse, if the address happens to route — reaches something nobody vetted.

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	cryptossh "golang.org/x/crypto/ssh"

	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/sshhost"
)

// JumpHost is the bastion a device is reachable through. The zero value means "direct" — no hop.
type JumpHost struct {
	Host       string           // bastion address (no port)
	Port       string           // "" ⇒ 22
	Identity   string           // SSH login on the BASTION (distinct from the device login)
	KeyRef     config.SecretRef // the bastion credential REFERENCE, resolved in memory at use (INV-13)
	KnownHosts string           // known_hosts pinning the BASTION's host key — its own, not the device's
}

// declared reports whether an operator asked for a hop at all.
func (j JumpHost) declared() bool {
	return strings.TrimSpace(j.Host) != "" ||
		strings.TrimSpace(j.Identity) != "" ||
		strings.TrimSpace(string(j.KeyRef)) != "" ||
		strings.TrimSpace(j.KnownHosts) != ""
}

// validate refuses a partially-declared hop. Every field is load-bearing: without Host there is nothing to
// dial, without Identity/KeyRef the bastion cannot be authenticated TO, and without KnownHosts the bastion
// cannot be authenticated BY — and an unverified bastion is precisely the machine-in-the-middle this design
// exists to survive.
func (j JumpHost) validate() error {
	missing := []string{}
	if strings.TrimSpace(j.Host) == "" {
		missing = append(missing, "host")
	}
	if strings.TrimSpace(j.Identity) == "" {
		missing = append(missing, "identity")
	}
	if strings.TrimSpace(string(j.KeyRef)) == "" {
		missing = append(missing, "key_ref")
	}
	if strings.TrimSpace(j.KnownHosts) == "" {
		missing = append(missing, "known_hosts")
	}
	if len(missing) > 0 {
		return fmt.Errorf("cisco: jump host is partially declared (missing %s) — refusing to connect; a partial hop must NEVER fall back to a direct dial, which would either fail confusingly or reach an unvetted peer", strings.Join(missing, ", "))
	}
	return nil
}

// dialThroughJump opens a TCP connection to `target` FROM the bastion: it dials the bastion with the caller's
// dialer, completes a fully host-key-verified SSH handshake with it, then asks it to open a connection onward
// to the device. The returned net.Conn is the tunnelled device connection — the caller layers the DEVICE's own
// verified SSH handshake on top of it, so the device key is checked end-to-end.
//
// closeJump tears down the bastion client and its transport; the caller must call it once the device session
// is finished (or immediately, on any error after this returns).
func dialThroughJump(
	ctx context.Context,
	j JumpHost,
	target string,
	connectTimeout time.Duration,
	dial func(ctx context.Context, addr string) (net.Conn, error),
) (tunnelled net.Conn, closeJump func(), err error) {
	if err := j.validate(); err != nil {
		return nil, nil, err
	}
	verifier, err := sshhost.New(j.KnownHosts)
	if err != nil {
		return nil, nil, fmt.Errorf("cisco: jump host known_hosts: %w", err)
	}
	signer, err := resolveSigner(j.KeyRef)
	if err != nil {
		return nil, nil, fmt.Errorf("cisco: jump host credential: %w", err)
	}
	jumpAddr := net.JoinHostPort(strings.TrimSpace(j.Host), portOr(j.Port))
	cfg := &cryptossh.ClientConfig{
		User:    strings.TrimSpace(j.Identity),
		Auth:    []cryptossh.AuthMethod{cryptossh.PublicKeys(signer)},
		Timeout: connectTimeout,
	}
	// BOTH host-key fields, exactly as the device dial does — the bastion is pinned no more loosely than the
	// device behind it.
	verifier.Apply(cfg, jumpAddr)

	raw, err := dial(ctx, jumpAddr)
	if err != nil {
		return nil, nil, fmt.Errorf("cisco: dial jump host %s: %w", jumpAddr, err)
	}
	cc, chans, reqs, err := cryptossh.NewClientConn(raw, jumpAddr, cfg)
	if err != nil {
		_ = raw.Close()
		if ctx.Err() != nil {
			return nil, nil, fmt.Errorf("cisco: jump-host handshake with %s aborted by deadline: %w", jumpAddr, ctx.Err())
		}
		return nil, nil, fmt.Errorf("cisco: jump-host handshake with %s refused: %w", jumpAddr, err)
	}
	client := cryptossh.NewClient(cc, chans, reqs)

	// The onward hop. DialContext runs INSIDE the bastion's SSH connection: from TG's side this is an ordinary
	// net.Conn, and the device's own handshake is layered on top of it, so the device's host key is verified
	// end-to-end and the bastion never sees a usable device credential.
	onward, err := client.DialContext(ctx, "tcp", target)
	if err != nil {
		_ = client.Close()
		_ = raw.Close()
		return nil, nil, fmt.Errorf("cisco: jump host %s could not reach %s: %w", jumpAddr, target, err)
	}
	return onward, func() { _ = client.Close(); _ = raw.Close() }, nil
}
