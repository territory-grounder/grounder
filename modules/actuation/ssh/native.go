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
	"golang.org/x/crypto/ssh/knownhosts"

	actuation "github.com/territory-grounder/grounder/adapters/actuation"
	"github.com/territory-grounder/grounder/core/config"
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
}

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
	hostKeys, err := knownhosts.New(r.knownHosts)
	if err != nil {
		return actuation.Result{}, fmt.Errorf("ssh: known_hosts file %s is unusable (fail closed): %w", r.knownHosts, err)
	}
	signer, err := parseActuationKey(r.keyRef)
	if err != nil {
		return actuation.Result{}, err
	}

	addr := net.JoinHostPort(host, actuationSSHPort)
	cfg := &cryptossh.ClientConfig{
		User:            identity,
		Auth:            []cryptossh.AuthMethod{cryptossh.PublicKeys(signer)}, // key-only; no password method exists
		HostKeyCallback: hostKeys,                                             // knownhosts: unknown/changed key ⇒ refuse
		Timeout:         r.connectTimeout,
	}

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
	// The ctx watchdog: x/crypto's handshake/session APIs predate context, so the deadline is enforced by
	// closing the transport, which aborts the handshake, the session, and any in-flight read immediately.
	watchdogDone := make(chan struct{})
	defer close(watchdogDone)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-watchdogDone:
		}
	}()

	cc, chans, reqs, err := cryptossh.NewClientConn(conn, addr, cfg)
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
	if err := sess.Run(remoteCmd); err != nil {
		var exitErr *cryptossh.ExitError
		if errors.As(err, &exitErr) {
			// The remote command ran and exited non-zero — a RESULT the caller interprets, not an error.
			return actuation.Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), ExitCode: exitErr.ExitStatus()}, nil
		}
		if ctx.Err() != nil {
			return actuation.Result{}, fmt.Errorf("ssh: remote exec on %s aborted by deadline: %w", addr, ctx.Err())
		}
		return actuation.Result{}, fmt.Errorf("ssh: remote exec on %s failed: %w", addr, err)
	}
	return actuation.Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), ExitCode: 0}, nil
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
