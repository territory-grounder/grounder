// Package sshhost is host-key verification done so that it verifies the key the operator actually pinned.
//
// THE BUG THIS EXISTS TO PREVENT, found in production on 2026-08-02. TG's SSH lanes built their client as:
//
//	hostKeys, _ := knownhosts.New(path)
//	cfg := &ssh.ClientConfig{HostKeyCallback: hostKeys, ...}
//
// which looks complete and is not. ClientConfig.HostKeyAlgorithms was left unset, so the client advertised
// golang.org/x/crypto/ssh's DEFAULT preference order — which puts ECDSA and RSA AHEAD of Ed25519 — and the
// server picked its own most-preferred match. Against a stock OpenSSH server offering
// rsa-sha2-512, rsa-sha2-256, ecdsa-sha2-nistp256, ssh-ed25519, the pair negotiated ECDSA. The known_hosts
// file pinned only ssh-ed25519 for that host, so the callback found the host, found no ECDSA key for it,
// and returned a *knownhosts.KeyError with Want non-empty — which is the "REMOTE HOST IDENTIFICATION HAS
// CHANGED" case.
//
// So a correctly pinned, unmodified server was reported as a host-key MISMATCH: the alarm that means
// somebody may be impersonating a machine. Both syslog servers failed this way at once, which is the shape
// of a configuration bug rather than an attack — but only if someone is in a position to notice, and the
// operator-facing message correctly told them to verify the fingerprint out of band before touching
// known_hosts. The cost of the false alarm is real: the honest response to it is expensive and alarming.
//
// It also fails CLOSED in the worst possible way — silently. The syslog tools stayed registered and routed,
// and failed at read time with what looked like a transient network fault, so both sites ran with no device
// logs during triage until a probe finally asked.
//
// THE FIX. Ask the known_hosts database which algorithms it holds for THIS host and advertise exactly
// those. Then negotiation can only land on an algorithm that is pinned, and a mismatch means what it says.
//
// x/crypto/ssh/knownhosts has no accessor for this — its callback is a bare func. But KeyError.Want is
// documented to carry "the accepted host keys" for the host, so calling the callback once with a key that
// cannot match yields the pinned set without touching the network. That is what Algorithms does.
package sshhost

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"net"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// Verifier is a known_hosts database prepared for one connection.
type Verifier struct {
	// Callback is the ssh.ClientConfig.HostKeyCallback. Unknown or changed key ⇒ refuse.
	Callback ssh.HostKeyCallback
	path     string
}

// New loads a known_hosts file. An empty path is refused rather than defaulted: connecting unverified is
// never the safe fallback, and every caller here already treats a missing file as fail-closed.
func New(knownHostsPath string) (*Verifier, error) {
	if knownHostsPath == "" {
		return nil, errors.New("sshhost: no known_hosts file configured — refusing to connect unverified")
	}
	cb, err := knownhosts.New(knownHostsPath)
	if err != nil {
		return nil, fmt.Errorf("sshhost: known_hosts file %s is unusable (fail closed): %w", knownHostsPath, err)
	}
	return &Verifier{Callback: cb, path: knownHostsPath}, nil
}

// probeAddr is a placeholder remote address for the offline lookup below. The knownhosts callback matches
// on the hostname string it is given; the net.Addr is only reported back inside errors.
var probeAddr = &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 22}

// unmatchableKey is a valid Ed25519 public key of all zero bytes. It is used ONLY to provoke a KeyError so
// the pinned set can be read out of Want; it is never sent anywhere and never trusted.
//
// A real server presenting this exact key would be indistinguishable from the probe — which is why the
// result is used solely to CONSTRAIN the advertised algorithms and never to accept anything. Verification
// still runs in full through Callback during the handshake.
var unmatchableKey = func() ssh.PublicKey {
	k, err := ssh.NewPublicKey(ed25519.PublicKey(make([]byte, ed25519.PublicKeySize)))
	if err != nil {
		return nil
	}
	return k
}()

// Algorithms returns the host-key algorithms pinned for hostport, most-specific first, or nil when the
// host is not in the file at all.
//
// A nil result must NOT be turned into "advertise everything" by the caller — that is the defect this
// package exists to remove. Leaving ClientConfig.HostKeyAlgorithms unset for an unknown host is harmless
// only because the callback then refuses the connection anyway; the value of setting it is entirely in the
// known-host case.
func (v *Verifier) Algorithms(hostport string) []string {
	if v == nil || v.Callback == nil || unmatchableKey == nil {
		return nil
	}
	err := v.Callback(hostport, probeAddr, unmatchableKey)
	if err == nil {
		// Structurally unreachable: an all-zero Ed25519 key is not a real host key. If it ever happens,
		// advertise nothing and let the handshake's own verification decide.
		return nil
	}
	var ke *knownhosts.KeyError
	if !errors.As(err, &ke) || len(ke.Want) == 0 {
		// Want empty means the host is UNKNOWN, not that it has no algorithms. Returning nil lets the
		// caller dial and be refused by the callback with the honest "unknown host" error, rather than
		// inventing an algorithm list for a host nobody pinned.
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(ke.Want))
	for _, w := range ke.Want {
		if w.Key == nil {
			continue
		}
		t := w.Key.Type()
		if seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
		// An RSA host key may be verified under the SHA-2 signature algorithms, which are distinct
		// strings in the algorithm list even though they name the same key. Omitting them would refuse a
		// modern OpenSSH server that has dropped the legacy ssh-rsa signature algorithm — the same class
		// of self-inflicted failure this package exists to remove, in the other direction.
		if t == ssh.KeyAlgoRSA {
			for _, extra := range []string{ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSASHA512} {
				if !seen[extra] {
					seen[extra] = true
					out = append(out, extra)
				}
			}
		}
	}
	return out
}

// Apply sets BOTH host-key fields on a client config for one destination.
//
// It exists so the two can never be set apart. They are a pair — a callback without a matching algorithm
// list verifies a key the server was never going to present — and every site that set only the callback
// was broken in exactly the same way. Calling this instead of assigning HostKeyCallback directly is what
// makes that mistake unavailable rather than merely discouraged.
func (v *Verifier) Apply(cfg *ssh.ClientConfig, hostport string) {
	if v == nil || cfg == nil {
		return
	}
	cfg.HostKeyCallback = v.Callback
	cfg.HostKeyAlgorithms = v.Algorithms(hostport)
}
