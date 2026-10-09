package cisco

import (
	"context"
	"fmt"
	"io"
	"net"
	"regexp"
	"strings"
	"time"

	cryptossh "golang.org/x/crypto/ssh"

	"github.com/territory-grounder/grounder/adapters/actuation"
	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/sshhost"
)

// KnownHostsEnv names the OpenSSH known_hosts knob a wiring slice sets; kept here so the fail-closed refusal
// can point the operator at it, mirroring the ssh leaf.
const KnownHostsEnv = "TG_ACTUATION_CISCO_KNOWN_HOSTS"

const (
	defaultConnectTimeout = 15 * time.Second // ASA/IOS handshakes are slower than a Linux host's
	defaultIOTimeout      = 20 * time.Second // one send/expect round; a `show tech` is capped by the ctx above this
	defaultPort           = "22"
)

// defaultPromptRE matches a device prompt at the END of accumulated output: a hostname (word chars, dots,
// hyphens), optionally a mode suffix like "(config)", then '#' (enable) or '>' (user), then optional trailing
// space. It is deliberately a DEFAULT — a wiring slice can pin a device's exact prompt to remove any chance
// of a mid-output false match. The transport is read-only, so the worst a loose prompt can do is return early.
var defaultPromptRE = regexp.MustCompile(`[\w.\-]+(?:\([\w.\-]+\))?[#>] ?$`)

// Device is one Cisco target's connection profile. Carried as a value (like syslogng.Server) so a later
// per-target routing slice can build it per action; this slice constructs one and drives it.
type Device struct {
	Host         string           // the device address (no port)
	Port         string           // "" ⇒ 22
	Identity     string           // the SSH login name
	KeyRef       config.SecretRef // the SSH private key REFERENCE, resolved in memory at use (INV-13)
	KnownHosts   string           // path to the OpenSSH known_hosts pinning this device's host key
	LegacyCrypto bool             // arm deprecated KEX/ciphers for old ASA/IOS (off by default — modern first)
	PagerOffCmd  string           // "" ⇒ "terminal length 0" (IOS); ASA wiring sets "terminal pager 0"
	// Jump (TG-85 component 2) routes the connection through a site-local bastion for a device that is not
	// reachable from TG directly. The zero value is a direct dial. A PARTIALLY declared hop refuses the
	// connection — it never falls back to direct. Both hops are independently host-key verified.
	Jump   JumpHost
	Prompt *regexp.Regexp // nil ⇒ defaultPromptRE
}

// interactiveRunner is the production Runner: it opens a PTY shell to the Device, disables paging, and drives
// one read-only command through a prompt-anchored send/expect exchange.
type interactiveRunner struct {
	dev            Device
	connectTimeout time.Duration
	ioTimeout      time.Duration
	// dial is injectable so the fail-closed and end-to-end tests run against an in-process server with no
	// network; nil ⇒ a real net.Dialer.
	dial func(ctx context.Context, addr string) (net.Conn, error)
}

// NewInteractiveRunner builds the production send/expect runner for a device.
func NewInteractiveRunner(dev Device) *interactiveRunner {
	return &interactiveRunner{dev: dev, connectTimeout: defaultConnectTimeout, ioTimeout: defaultIOTimeout}
}

// legacyKEX / legacyCiphers are the deprecated algorithms x/crypto omits from its secure defaults but old
// ASA/IOS may be the only thing they speak. Armed ONLY when Device.LegacyCrypto is set, so a modern device is
// never downgraded silently. Host-key verification is UNAFFECTED (sshhost still pins the key + its algorithm).
var (
	legacyKEX     = []string{"diffie-hellman-group14-sha1", "diffie-hellman-group1-sha1", "diffie-hellman-group-exchange-sha1"}
	legacyCiphers = []string{"aes128-cbc", "aes192-cbc", "aes256-cbc", "3des-cbc"}
)

// dialSession opens the verified PTY shell to the Device, disables paging, and returns the session ready to
// drive: the stdin writer, a prompt-anchored expecter positioned at a clean post-pager-off prompt, and a
// cleanup func the caller MUST defer. The whole dial / host-key / watchdog / PTY / banner / pager-off sequence
// lives here once so RunShow (the read lane) and RunConfig (the separately-gated write lane) share exactly one
// connect path. On any error it tears down whatever it opened before returning.
func (r *interactiveRunner) dialSession(ctx context.Context) (stdin io.WriteCloser, exp *expecter, cleanup func(), err error) {
	if r.dev.KnownHosts == "" {
		return nil, nil, nil, fmt.Errorf("cisco: no known_hosts configured (set %s to the OpenSSH known_hosts carrying this device's host key): refusing to connect unverified", KnownHostsEnv)
	}
	verifier, err := sshhost.New(r.dev.KnownHosts)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("cisco: %w", err)
	}
	signer, err := resolveSigner(r.dev.KeyRef)
	if err != nil {
		return nil, nil, nil, err
	}
	addr := net.JoinHostPort(r.dev.Host, portOr(r.dev.Port))
	cfg := &cryptossh.ClientConfig{
		User:    r.dev.Identity,
		Auth:    []cryptossh.AuthMethod{cryptossh.PublicKeys(signer)},
		Timeout: r.connectTimeout,
	}
	if r.dev.LegacyCrypto {
		cfg.Config.KeyExchanges = append(cryptossh.SupportedAlgorithms().KeyExchanges, legacyKEX...)
		cfg.Config.Ciphers = append(cryptossh.SupportedAlgorithms().Ciphers, legacyCiphers...)
	}
	// BOTH host-key fields together (core/sshhost): old ASA/IOS host keys are often RSA/ECDSA, exactly the
	// case the Algorithms() pinning handles — setting only the callback would report an unmodified device as a
	// host-key MISMATCH and refuse as if it were an impersonation.
	verifier.Apply(cfg, addr)

	dial := r.dial
	if dial == nil {
		dial = func(ctx context.Context, addr string) (net.Conn, error) {
			d := net.Dialer{Timeout: r.connectTimeout}
			return d.DialContext(ctx, "tcp", addr)
		}
	}
	// The hop, when declared: dial the bastion, verify ITS host key, and open the onward connection from
	// there. The device handshake below then runs over that tunnel, so the DEVICE's key is verified
	// end-to-end and a compromised bastion cannot impersonate it.
	var closeJump func()
	var conn net.Conn
	if r.dev.Jump.declared() {
		tunnelled, cj, jerr := dialThroughJump(ctx, r.dev.Jump, addr, r.connectTimeout, dial)
		if jerr != nil {
			return nil, nil, nil, jerr
		}
		conn, closeJump = tunnelled, cj
	} else {
		c, derr := dial(ctx, addr)
		if derr != nil {
			return nil, nil, nil, fmt.Errorf("cisco: dial %s: %w", addr, derr)
		}
		conn = c
	}
	// The ctx watchdog closes the transport on deadline/cancel — x/crypto's handshake/session APIs predate
	// context, so closing the conn is what aborts a stalled handshake, PTY request, or blocked expect read.
	watchdogDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-watchdogDone:
		}
	}()
	// Until we hand back a cleanup func, ANY error path must stop the watchdog and close the conn (closing conn
	// tears down the SSH client/session layered on it). A deferred guard keyed on success keeps that in one place.
	ok := false
	defer func() {
		if !ok {
			close(watchdogDone)
			_ = conn.Close()
			if closeJump != nil {
				closeJump()
			}
		}
	}()

	cc, chans, reqs, err := cryptossh.NewClientConn(conn, addr, cfg)
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil, nil, fmt.Errorf("cisco: connect to %s aborted by deadline: %w", addr, ctx.Err())
		}
		return nil, nil, nil, fmt.Errorf("cisco: handshake with %s refused: %w", addr, err)
	}
	client := cryptossh.NewClient(cc, chans, reqs)
	sess, err := client.NewSession()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("cisco: session on %s: %w", addr, err)
	}
	stdinPipe, err := sess.StdinPipe()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("cisco: stdin pipe: %w", err)
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("cisco: stdout pipe: %w", err)
	}
	// A minimal VT100 PTY: Cisco needs a PTY to present its CLI, but we never render — echo off would be ideal
	// yet ASA ignores it, so the expect layer strips the echoed command instead.
	modes := cryptossh.TerminalModes{cryptossh.ECHO: 0, cryptossh.TTY_OP_ISPEED: 14400, cryptossh.TTY_OP_OSPEED: 14400}
	if err := sess.RequestPty("vt100", 200, 80, modes); err != nil {
		return nil, nil, nil, fmt.Errorf("cisco: request pty on %s: %w", addr, err)
	}
	if err := sess.Shell(); err != nil {
		return nil, nil, nil, fmt.Errorf("cisco: start shell on %s: %w", addr, err)
	}

	e := newExpecter(stdout, r.prompt(), r.ioTimeout)
	// Consume the login banner + first prompt so the device is at a known state before any command.
	if _, err := e.until(ctx); err != nil {
		return nil, nil, nil, fmt.Errorf("cisco: never saw the initial device prompt on %s: %w", addr, err)
	}
	// Disable paging FIRST (the runner sends this, never the model — `terminal` is a forbidden model token).
	if err := sendLine(stdinPipe, r.pagerOff()); err != nil {
		return nil, nil, nil, fmt.Errorf("cisco: send pager-off: %w", err)
	}
	if _, err := e.until(ctx); err != nil {
		return nil, nil, nil, fmt.Errorf("cisco: no prompt after pager-off on %s: %w", addr, err)
	}

	ok = true
	cleanup = func() {
		_ = sess.Close()
		_ = client.Close()
		close(watchdogDone)
		if closeJump != nil {
			closeJump() // the bastion outlives the device session; tear it down last
		}
	}
	return stdinPipe, e, cleanup, nil
}

// RunShow opens the session, disables paging, runs the one read-only command, and returns its captured output.
func (r *interactiveRunner) RunShow(ctx context.Context, commandLine string) (actuation.Result, error) {
	stdin, exp, cleanup, err := r.dialSession(ctx)
	if err != nil {
		return actuation.Result{}, err
	}
	defer cleanup()

	// Send the one read-only command and capture its output up to the next prompt.
	if err := sendLine(stdin, commandLine); err != nil {
		return actuation.Result{}, fmt.Errorf("cisco: send command: %w", err)
	}
	captured, err := exp.until(ctx)
	if err != nil {
		return actuation.Result{}, fmt.Errorf("cisco: no prompt after %q on %s: %w", commandLine, net.JoinHostPort(r.dev.Host, portOr(r.dev.Port)), err)
	}
	// Best-effort exit; the deferred cleanup handles a device that ignores it.
	_ = sendLine(stdin, "exit")

	return actuation.Result{Stdout: cleanOutput(captured, commandLine, r.prompt())}, nil
}

func (r *interactiveRunner) prompt() *regexp.Regexp {
	if r.dev.Prompt != nil {
		return r.dev.Prompt
	}
	return defaultPromptRE
}

func (r *interactiveRunner) pagerOff() string {
	if strings.TrimSpace(r.dev.PagerOffCmd) != "" {
		return r.dev.PagerOffCmd
	}
	return "terminal length 0"
}

func portOr(p string) string {
	if strings.TrimSpace(p) == "" {
		return defaultPort
	}
	return p
}

var _ Runner = (*interactiveRunner)(nil)
