// native.go — the production SSH actuation Runner: an IN-PROCESS crypto/ssh client.
//
// The distroless worker image carries NO ssh binary and no shell, so the LocalRunner subprocess path
// (adapters/actuation, exec.Command("ssh", …)) fails at the effect leaf with "ssh: executable not found"
// the instant mutation is enabled. This native runner replaces it: it honors the SAME canonical,
// host-key-verified, key-only, POSIX-quoted invocation the module's sshArgv builds, but over an in-process
// transport — knownhosts host-key verification (a missing or changed key ⇒ refuse; there is NO insecure
// callback), key-only auth from a secret REFERENCE resolved in memory (INV-13, never on disk), no PTY, and
// the remote command run as the single POSIX-quoted word the module already produced (INV-02 is preserved
// by sshArgv/remoteCommand, unchanged). Ported from the proven modules/observability/syslogng native reader.
package ssh

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	cryptossh "golang.org/x/crypto/ssh"

	actuation "github.com/territory-grounder/grounder/adapters/actuation"
	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/sshhost"
)

// KnownHostsEnv names the deployment knob holding the path of the OpenSSH known_hosts file the native
// actuation runner verifies each target host key against. Empty ⇒ every Exec fails closed before dialing.
const KnownHostsEnv = "TG_ACTUATION_SSH_KNOWN_HOSTS"

// defaultConnectTimeout bounds the TCP connect + SSH handshake for one actuation.
const defaultConnectTimeout = 10 * time.Second

// actuationSSHPort is the fixed sshd port actuation targets are reached on.
const actuationSSHPort = "22"

// nativeRunner executes an actuation's fixed argv over an in-process crypto/ssh client. It satisfies the
// package Runner interface, so the ssh.Module hands it the exact argv sshArgv builds; it routes to the same
// destination and runs the same POSIX-quoted remote command a forked ssh client would, over a native
// transport the distroless worker can actually use.
type nativeRunner struct {
	// knownHosts is the OpenSSH known_hosts path host-key verification reads. Empty ⇒ every Run fails closed
	// before dialing (no unverified connection is ever attempted).
	knownHosts string
	// keyRef is the actuation identity's private-key REFERENCE (env:/file:/store:), resolved in memory per Run.
	keyRef         config.SecretRef
	connectTimeout time.Duration
	// dial opens the transport (net.Dialer.DialContext in production; an in-process pair in the oracle). The
	// SSH handshake, auth, and host-key check always run on top of it.
	dial func(ctx context.Context, addr string) (net.Conn, error)
	// certSign, when non-nil, mints a short-lived OpenBao-signed USER CERTIFICATE for the actuation key's
	// public key per Run (TG-423, CA mode): the runner then presents the certificate instead of the bare key,
	// so the target trusts it via TrustedUserCAKeys and the exposure window becomes the cert TTL. Nil (the
	// default) ⇒ the pre-TG-423 bare-key path, byte-for-byte. A signing failure fails the Run CLOSED — it
	// never falls back to the bare key, which would defeat the point of a short-lived credential.
	certSign CertSigner
}

// CertSigner mints a short-lived SSH user certificate for pub, authorizing the login `principal`. The
// composition root wires it to an OpenBao ssh-engine Sign call; core/ must not import modules/, so the
// concrete signer is injected here rather than constructed in this package.
type CertSigner func(ctx context.Context, pub cryptossh.PublicKey, principal string) (*cryptossh.Certificate, error)

// NewNativeRunner returns the production native SSH actuation runner. knownHostsPath is the operator-declared
// OpenSSH known_hosts file (KnownHostsEnv) carrying each actuation target's host key; keyRef is the scoped
// actuation identity's SSH private-key reference. An empty known_hosts path OR key ref yields a runner that
// refuses every Exec (fail closed) rather than one that skips host-key verification.
func NewNativeRunner(knownHostsPath string, keyRef config.SecretRef) Runner {
	return &nativeRunner{
		knownHosts:     strings.TrimSpace(knownHostsPath),
		keyRef:         keyRef,
		connectTimeout: defaultConnectTimeout,
	}
}

// NewNativeRunnerWithCASigner is NewNativeRunner plus OpenBao CA/signed-cert auth (TG-423): each Run mints a
// short-lived certificate for the actuation key via certSign and presents it instead of the bare key. A nil
// certSign is identical to NewNativeRunner (the bare-key path); the composition root passes nil when the
// ssh-CA flag is unset, so an un-armed deployment behaves exactly as before.
func NewNativeRunnerWithCASigner(knownHostsPath string, keyRef config.SecretRef, certSign CertSigner) Runner {
	return &nativeRunner{
		knownHosts:     strings.TrimSpace(knownHostsPath),
		keyRef:         keyRef,
		connectTimeout: defaultConnectTimeout,
		certSign:       certSign,
	}
}

// Run honors the Runner contract: it receives the canonical ssh invocation sshArgv built
// (["ssh", "-o", …, "identity@host", <POSIX-quoted remote command>]) and executes that remote command over
// an in-process crypto/ssh connection to identity@host, host-key-verified against known_hosts. The remote
// command word is passed to the login shell UNCHANGED — it is already the per-argument POSIX-quoted form the
// module produced, so this native transport runs EXACTLY the vector the subprocess client would have (INV-02
// injection-safety lives in sshArgv/remoteCommand, untouched here). A non-zero REMOTE exit is a RESULT
// (reported in ExitCode), not an error; host-key, auth, and transport failures fail closed. stdin is unused
// (an allowlisted reversible op takes none).
func (r *nativeRunner) Run(ctx context.Context, argv []string, _ []byte) (actuation.Result, error) {
	identity, host, remoteCmd, ok := parseSSHArgv(argv)
	if !ok {
		return actuation.Result{}, errors.New("ssh: native runner received a non-canonical ssh argv (fail closed)")
	}
	// Host-key verification is MANDATORY and fails closed: no known_hosts file, no connection.
	if r.knownHosts == "" {
		return actuation.Result{}, fmt.Errorf("ssh: no known_hosts file configured (set %s to the OpenSSH known_hosts carrying each actuation target's host key): refusing to connect unverified", KnownHostsEnv)
	}
	verifier, err := sshhost.New(r.knownHosts)
	if err != nil {
		return actuation.Result{}, fmt.Errorf("ssh: %w", err)
	}
	signer, err := parseActuationKey(r.keyRef)
	if err != nil {
		return actuation.Result{}, err
	}
	// TG-423: when the ssh-CA lane is armed (certSign set), present a short-lived OpenBao-signed CERTIFICATE
	// for the actuation key instead of the bare key — the target trusts it via TrustedUserCAKeys and the
	// exposure window becomes the cert TTL. A signing failure fails the actuation CLOSED; it never falls back
	// to the long-lived bare key, which would defeat the short-lived credential. Nil certSign ⇒ bare key, as before.
	authSigner := signer
	if r.certSign != nil {
		cert, cerr := r.certSign(ctx, signer.PublicKey(), identity)
		if cerr != nil {
			return actuation.Result{}, fmt.Errorf("ssh: ssh-CA certificate signing failed (fail closed): %w", cerr)
		}
		certSigner, cerr := cryptossh.NewCertSigner(cert, signer)
		if cerr != nil {
			return actuation.Result{}, fmt.Errorf("ssh: ssh-CA cert signer construction failed (fail closed): %w", cerr)
		}
		authSigner = certSigner
	}

	addr := net.JoinHostPort(host, actuationSSHPort)
	cfg := &cryptossh.ClientConfig{
		User:    identity,
		Auth:    []cryptossh.AuthMethod{cryptossh.PublicKeys(authSigner)}, // bare key, or the CA-signed cert
		Timeout: r.connectTimeout,
	}
	// BOTH host-key fields, together — see core/sshhost. Setting only the callback left the client
	// advertising Go's default algorithm order, which puts ECDSA and RSA ahead of Ed25519. Against a stock
	// OpenSSH server that negotiates an algorithm the operator may not have pinned, and an UNMODIFIED
	// target is then refused as a host-key MISMATCH. On THIS lane that is an actuation that silently
	// cannot run: the heal is refused for what reads as an impersonation alarm.
	verifier.Apply(cfg, addr)

	dial := r.dial
	if dial == nil {
		dial = func(ctx context.Context, addr string) (net.Conn, error) {
			d := net.Dialer{Timeout: r.connectTimeout}
			return d.DialContext(ctx, "tcp", addr)
		}
	}
	conn, err := dial(ctx, addr)
	if err != nil {
		return actuation.Result{}, fmt.Errorf("ssh: dial %s: %w", addr, err)
	}
	// The ctx watchdog covers the DIAL/HANDSHAKE phase ONLY (TG-80 P1-4). x/crypto's handshake predates
	// context, so a deadline during it is enforced by closing the transport. Once the handshake completes
	// the watchdog stands down: closing the transport under a RUNNING command would orphan the remote
	// process on the target — the defect this fixed (TG used to close the TCP link and leave the command
	// running) — so cancellation of a running command goes through the signal escalation below instead.
	handshakeDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-handshakeDone:
		}
	}()

	cc, chans, reqs, err := cryptossh.NewClientConn(conn, addr, cfg)
	close(handshakeDone)
	if err != nil {
		_ = conn.Close()
		if ctx.Err() != nil {
			return actuation.Result{}, fmt.Errorf("ssh: connect to %s aborted by deadline: %w", addr, ctx.Err())
		}
		// A knownhosts refusal (unknown or changed host key) surfaces here, by design.
		return actuation.Result{}, fmt.Errorf("ssh: handshake with %s refused: %w", addr, err)
	}
	client := cryptossh.NewClient(cc, chans, reqs)
	defer func() { _ = client.Close() }()

	sess, err := client.NewSession()
	if err != nil {
		return actuation.Result{}, fmt.Errorf("ssh: session on %s: %w", addr, err)
	}
	defer func() { _ = sess.Close() }()

	var stdout, stderr bytes.Buffer
	sess.Stdout = &stdout
	sess.Stderr = &stderr
	if err := sess.Start(remoteCmd); err != nil {
		return actuation.Result{}, fmt.Errorf("ssh: remote exec on %s failed to start: %w", addr, err)
	}
	waitErr := make(chan error, 1)
	go func() { waitErr <- sess.Wait() }()

	select {
	case err := <-waitErr:
		if err != nil {
			var exitErr *cryptossh.ExitError
			if errors.As(err, &exitErr) {
				// The remote command ran and exited non-zero — a RESULT the caller interprets, not an error.
				return actuation.Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), ExitCode: exitErr.ExitStatus()}, nil
			}
			return actuation.Result{}, fmt.Errorf("ssh: remote exec on %s failed: %w", addr, err)
		}
		return actuation.Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), ExitCode: 0}, nil
	case <-ctx.Done():
		// VERIFIED REMOTE-KILL (TG-80 P1-4): the caller cancelled (deadline or explicit) while the command
		// is running. Escalate over the still-open channel — SIGTERM, a bounded grace, SIGKILL, a second
		// grace — and ONLY THEN let the deferred closes drop the session and transport. The remote process
		// is ended by the target's kernel, not abandoned by a vanished TCP peer. Whether the remote
		// acknowledged (Wait returned) or not is carried in the typed error, so the record can say which.
		_ = sess.Signal(cryptossh.SIGTERM)
		select {
		case werr := <-waitErr:
			return actuation.Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}, remoteCancelled(addr, "SIGTERM", ctx.Err(), werr)
		case <-time.After(killGrace):
		}
		_ = sess.Signal(cryptossh.SIGKILL)
		select {
		case werr := <-waitErr:
			return actuation.Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}, remoteCancelled(addr, "SIGKILL", ctx.Err(), werr)
		case <-time.After(killGrace):
			return actuation.Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}, remoteCancelled(addr, "SIGKILL (unacknowledged before the transport closed)", ctx.Err(), nil)
		}
	}
}

// killGrace is how long the runner waits after each signal before escalating (TERM → KILL → close). A
// package variable, not a constant, so the oracle can shrink it; the production default keeps a
// well-behaved service's shutdown hooks inside the window.
var killGrace = 5 * time.Second

// ErrRemoteCancelled marks an actuation the caller cancelled whose remote command was SIGNALLED to death
// over the SSH channel before the transport closed (TG-80 P1-4). It wraps the context error too, so the
// interceptor — which must not import this package — classifies it with errors.Is(err, context.Canceled /
// DeadlineExceeded) and records a CANCELLED terminal instead of a generic "execute failed".
var ErrRemoteCancelled = errors.New("ssh: remote command cancelled")

func remoteCancelled(addr, signal string, cause, waitErr error) error {
	detail := ""
	if waitErr != nil {
		detail = " (remote reported: " + waitErr.Error() + ")"
	}
	return fmt.Errorf("%w: %s sent to %s before closing the transport%s: %w", ErrRemoteCancelled, signal, addr, detail, cause)
}

// parseSSHArgv reads the destination identity/host and the single POSIX-quoted remote-command word from the
// canonical argv sshArgv builds: ["ssh", "-o", …, "identity@host", <remote command>]. It is the contract the
// native runner honors so it routes to the SAME host and runs the SAME command the subprocess client would;
// a malformed argv (wrong prologue, no destination) fails closed rather than mis-routing. identity is the
// part before the FIRST "@" (a scoped username carries no "@").
func parseSSHArgv(argv []string) (identity, host, remoteCmd string, ok bool) {
	// The canonical shape is EXACTLY: "ssh" + sshCanonicalOpts + "identity@host" + <remote command>. Validate
	// the whole prologue verbatim — not just argv[0] — so a non-canonical argv handed to the public Runner
	// directly (a shorter vector naming a different host, or one that downgrades StrictHostKeyChecking) fails
	// closed here rather than dialing an unintended destination or connecting with weakened verification.
	if len(argv) != 1+len(sshCanonicalOpts)+2 || argv[0] != "ssh" {
		return "", "", "", false
	}
	for i, opt := range sshCanonicalOpts {
		if argv[1+i] != opt {
			return "", "", "", false
		}
	}
	dest := argv[len(argv)-2]
	remoteCmd = argv[len(argv)-1]
	at := strings.IndexByte(dest, '@')
	if at <= 0 || at >= len(dest)-1 {
		return "", "", "", false
	}
	return dest[:at], dest[at+1:], remoteCmd, true
}

// parseActuationKey resolves the actuation key REFERENCE at read time and parses it in memory (INV-13): key
// material never touches a filesystem path here. Every failure names the REF only — never a byte of what it
// resolved to — and fails closed.
func parseActuationKey(ref config.SecretRef) (cryptossh.Signer, error) {
	material, err := ref.Resolve()
	if err != nil {
		return nil, fmt.Errorf("ssh: actuation key ref %q did not resolve (fail closed)", string(ref))
	}
	if strings.TrimSpace(material) == "" {
		return nil, fmt.Errorf("ssh: actuation key ref %q resolved empty (fail closed)", string(ref))
	}
	signer, err := cryptossh.ParsePrivateKey([]byte(material))
	if err != nil {
		return nil, fmt.Errorf("ssh: actuation key ref %q did not parse as a private key (fail closed)", string(ref))
	}
	return signer, nil
}
