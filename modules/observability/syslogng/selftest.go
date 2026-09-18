// This file is the syslog-ng connector's answer to the console's TEST button (core/selftest.Tester).
//
// WHAT THE DESCRIPTOR PROMISES, made literally true: "open a host-key-verified SSH session to each
// configured syslog server and close it — checks the key reference, the ssh user and the host key; no log is
// read and nothing is written". The probe below opens the SSH transport, verifies the server's host key
// against the operator-declared known_hosts, authenticates with the in-memory-parsed key the row's REFERENCE
// resolves to, opens ONE session channel, and closes it without ever sending an exec request. No `tail`, no
// `grep`, no command of any kind: the remote host runs nothing on TG's behalf during a test.
//
// WHY THE HANDSHAKE IS THE RIGHT THING TO PROBE. Every way this connector fails in practice fails there: a
// key reference that resolves to nothing (or to a key the account's authorized_keys does not carry), an ssh
// user that is wrong for that site, a server rebuilt so its host key no longer matches known_hosts, a
// TG_SYSLOGNG_KNOWN_HOSTS that was never set — in which case the runner refuses every read, fail closed,
// which is correct and completely silent. None of these produce an error anywhere today: they produce a
// triage agent that answers "the syslog server was unreachable or the read errored" in the middle of an
// incident, which is the worst possible moment to learn that a settings row has been wrong for a month.
//
// WHY EVERY SERVER, AND WHY ONE BAD ROW IS A FAILURE. The server list is one row per SITE. A row that cannot
// be reached costs exactly that site its device logs, and nothing else changes: the other sites keep working,
// the tool still exists, and the only symptom is that hosts under one site-code prefix never have logs. So
// the probe reports every server by name and site, and returns an error if ANY of them refuses — a green
// that meant "most of your sites work" would be the same silent partial the module already suffers from.
// ParseServers additionally SKIPS a malformed row without a word, so a Summary that names the servers is
// also the only place an operator can see that the row they typed is not among them.
//
// WHAT A GREEN RESULT DOES NOT PROVE, repeated in the operator-facing Detail: it does not prove a log can be
// read. The base path, the per-device directory layout and file permissions are all discovered at read time,
// and checking them would mean running a command on the server — which is what this probe promises not to do.
package syslogng

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/territory-grounder/grounder/agent"
	"github.com/territory-grounder/grounder/core/selftest"
	"github.com/territory-grounder/grounder/core/sshhost"
)

// probeTimeout bounds ONE server's handshake.
//
// moduletest gives the whole activity 30 seconds with no retry, and the servers are probed concurrently
// (they are independent machines at different sites; a dead one must not spend another site's budget). This
// cap is the floor of that guarantee: it applies even when a caller passes a context with no deadline at
// all, because x/crypto's handshake API predates context and would otherwise block on a half-open TCP
// connection until the kernel gave up.
const probeTimeout = 12 * time.Second

// Module is the configured syslog-ng connector as one object: the parsed server list plus the transport its
// reads travel over.
//
// It exists because the console needs something to press TEST on. NewTools returns []agent.Tool — a slice of
// tools is not a module and cannot carry a capability — so before this there was no value a composition root
// could hand to core/selftest.Of. Holding the SAME Runner the tools use is the point: a probe that built its
// own transport would prove that a second, unused SSH client works.
type Module struct {
	servers []Server
	runner  Runner
}

// NewModule builds the connector from the parsed server list and the transport its reads use. A nil runner
// selects the production NATIVE in-process SSH runner with mandatory host-key verification, exactly as
// NewTools does — so a Module built the ordinary way probes the ordinary way.
func NewModule(servers []Server, runner Runner) *Module {
	if runner == nil {
		runner = NewNativeRunner(os.Getenv(KnownHostsEnv))
	}
	return &Module{servers: servers, runner: runner}
}

// Tools returns the agent's read-only syslog-ng tools over this module's servers and runner.
//
// It is here so a composition root can build ONE object and get both the tools and the probe from it. The
// alternative — constructing the tools from one runner and the prober from another — would let a green Test
// certify a transport the agent does not actually use.
func (m *Module) Tools() []agent.Tool {
	if m == nil {
		return nil
	}
	return NewTools(m.servers, m.runner)
}

// Servers returns the configured rows (a copy, so a caller cannot rewrite the connector's routing).
func (m *Module) Servers() []Server {
	if m == nil {
		return nil
	}
	return append([]Server(nil), m.servers...)
}

// sessionOpener is the capability the probe needs from a transport: open an authenticated, host-key-verified
// session and close it, RUNNING NOTHING.
//
// It is deliberately not part of Runner. Runner.Run exists to execute a fixed argv and refuses an empty one;
// a probe built on it would have to invent a command to run, and "just run `true`" is still a process started
// on the operator's syslog server from a settings dialog. Keeping the handshake as its own capability is what
// lets the descriptor's verb say "and close it" truthfully.
type sessionOpener interface {
	openSession(ctx context.Context, server Server) error
}

// compile-time proof this module can honour the TEST button its descriptor advertises. Without it a rename
// in core/selftest would silently turn SelfTest back into an unreachable method — a capability that exists
// and is never called, which is the defect class this whole exercise is closing.
var _ selftest.Tester = (*Module)(nil)

// SelfTest opens and closes a host-key-verified SSH session on every configured syslog server.
//
// The operator argument is ignored: nothing is created on the remote hosts to attribute. (The servers' own
// sshd will log an accepted publickey authentication, as it does for every read the agent makes; that is a
// record of a connection, not a change to the estate.)
//
// It returns an error if any server refuses, and the Detail names, per server, the class of fault: an
// unusable known_hosts, a key reference that will not resolve or parse, a host that will not answer, a host
// key that does not match the pinned one, an account that refuses the key, or a session the account may not
// open.
func (m *Module) SelfTest(ctx context.Context, _ string) (selftest.Result, error) {
	if m == nil || len(m.servers) == 0 {
		// Not a defensive branch — this is the state an operator reaches by typing a row wrong. ParseServers
		// silently drops any row missing its ssh host, user or key reference, so "no servers" and "one typo"
		// are indistinguishable from the outside, and the agent simply has no syslog tools.
		return selftest.Result{
				Summary: "no syslog servers are configured, so the triage agent has no syslog-ng tools at all",
				Detail: "either TG_SYSLOGNG_DEPLOYMENTS is empty, or every row in it was SKIPPED as malformed — a " +
					"row is dropped without a word when it is missing its ssh host, ssh user or key reference, so a " +
					"single typo costs a site its logs with no error anywhere. Rows are " +
					"site|sshhost|sshuser|keyref|basepath|prefix, ';'-separated.",
			},
			errors.New("syslogng: self-test has no configured syslog server to open a session to")
	}
	opener, ok := m.runner.(sessionOpener)
	if !ok {
		// Production always has one (nativeRunner). A transport injected by a test or a future runner that
		// cannot open a session without running a command gets an honest "no probe" rather than a fabricated
		// pass or a command executed behind the operator's back.
		return selftest.Result{
				Summary: fmt.Sprintf("this connector is wired to a transport (%T) that cannot open a session without running a command", m.runner),
				Detail: "the probe opens an SSH session and closes it, running nothing; a transport that cannot do " +
					"that is not testable this way. No connection was attempted.",
			},
			fmt.Errorf("syslogng: self-test cannot probe transport %T read-only", m.runner)
	}

	// Concurrent because the servers are independent machines at different sites: probing them in sequence
	// would let one dead site consume the whole 30-second budget and report the healthy ones as unchecked.
	// Results are written by index, so the report is in configuration order every time — an ordering that
	// changed between presses would teach an operator to distrust the one control meant to settle an argument.
	results := make([]error, len(m.servers))
	var wg sync.WaitGroup
	for i, srv := range m.servers {
		wg.Add(1)
		go func(i int, srv Server) {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, probeTimeout)
			defer cancel()
			results[i] = opener.openSession(cctx, srv)
		}(i, srv)
	}
	wg.Wait()

	var (
		status   = make([]string, 0, len(m.servers))
		labels   = make([]string, 0, len(m.servers))
		problems = make([]string, 0, len(m.servers))
		firstErr error
		okCount  int
	)
	for i, srv := range m.servers {
		label := describeServer(srv)
		labels = append(labels, label)
		if err := results[i]; err != nil {
			if firstErr == nil {
				firstErr = err
			}
			status = append(status, label+" FAILED")
			problems = append(problems, label+": "+classifySessionFailure(err))
			continue
		}
		okCount++
		status = append(status, label+" ok")
	}

	notes := []string{
		"this proves, per server, that the key reference resolves and parses, that the host key verifies " +
			"against " + KnownHostsEnv + ", that the account accepts the key and that it may open a session. It " +
			"does NOT prove a log can be read: the base path, the per-device directory and file permissions are " +
			"discovered at read time, and checking them would mean running a command, which this test does not do",
	}

	if firstErr != nil {
		return selftest.Result{
				Summary: fmt.Sprintf("%d of %d configured syslog server(s) accepted a host-key-verified SSH session — %s; no command was run",
					okCount, len(m.servers), strings.Join(status, "; ")),
				// Failures first — they are what has to be fixed — then what the failure costs, then the ceiling.
				Detail: joinProbeNotes(append([]string{
					strings.Join(problems, ". "),
					"a syslog server that refuses here is a site with NO device logs during triage: the agent's " +
						"tools exist, route hosts to that server, and fail at read time with a message that looks " +
						"like a transient network fault",
				}, notes...)),
			},
			fmt.Errorf("syslogng: self-test could not open a session on %d of %d configured syslog server(s): %w",
				len(m.servers)-okCount, len(m.servers), firstErr)
	}

	return selftest.Result{
		Summary: fmt.Sprintf("opened and closed a host-key-verified SSH session on all %d configured syslog server(s): %s — no command was run and no log was read",
			len(m.servers), strings.Join(labels, ", ")),
		Detail: joinProbeNotes(notes),
	}, nil
}

// describeServer names a row the way an operator wrote it, so a pass against a MIS-configured row is still
// legible ("root@syslog01 [AA]" when the site should have been served by a different host). The routing
// prefix is included when it was not derived from the ssh host, because an explicitly pinned prefix that
// does not match any device's name is a row that will never be selected by anything.
func describeServer(s Server) string {
	label := s.SSHUser + "@" + s.SSHHost
	if site := strings.TrimSpace(s.Site); site != "" {
		label += " [" + site + "]"
	}
	if prefix := strings.TrimSpace(s.HostPrefix); prefix != "" && prefix != locCode(s.SSHHost) {
		label += " routing hosts starting " + prefix
	}
	return label
}

// probe stages. The stage is what makes the diagnosis specific: "the connection failed" is useless, while
// "the key reference did not resolve" and "the server's host key is not the pinned one" send an operator to
// two completely different places.
const (
	stageConfig     = "config"
	stageKnownHosts = "known_hosts"
	stageKey        = "key"
	stageDial       = "dial"
	stageHandshake  = "handshake"
	stageSession    = "session"
)

// handshakeError carries WHERE a probe stopped alongside the underlying fault, so classification keys off
// the SHAPE of the failure rather than off the text of an SSH library's message.
type handshakeError struct {
	stage string
	err   error
}

func (e *handshakeError) Error() string { return "syslogng: ssh " + e.stage + ": " + e.err.Error() }
func (e *handshakeError) Unwrap() error { return e.err }

// openSession implements sessionOpener for the production native runner: it performs the full connect —
// mandatory host-key verification, in-memory key auth, one session channel — and then closes it, HAVING RUN
// NOTHING.
//
// It deliberately mirrors Run's connect sequence instead of calling it. Run's contract is "execute this fixed
// argv" and it refuses an empty one; reusing it would mean choosing a command to run on the operator's syslog
// server, and the whole value of this probe is that it needs no such choice. The two sequences are identical
// up to the exec, and the invariants that matter are shared code: parseKey (resolves the REFERENCE at use
// time, parses in memory, never touches disk, and names only the reference on failure) and the mandatory
// knownhosts callback (an unknown or changed host key refuses the connection — there is no trust-on-first-use
// and no insecure bypass anywhere in this package).
func (r *nativeRunner) openSession(ctx context.Context, server Server) error {
	if server.SSHHost == "" || server.SSHUser == "" {
		return &handshakeError{stage: stageConfig, err: errors.New("the row has no ssh host or no ssh user")}
	}
	// Host-key verification is MANDATORY and fails closed: no known_hosts file, no connection.
	if r.knownHosts == "" {
		return &handshakeError{stage: stageKnownHosts, err: fmt.Errorf("no known_hosts file is configured (%s is unset)", KnownHostsEnv)}
	}
	verifier, err := sshhost.New(r.knownHosts)
	if err != nil {
		return &handshakeError{stage: stageKnownHosts, err: err}
	}
	signer, err := parseKey(server.KeyRef)
	if err != nil {
		return &handshakeError{stage: stageKey, err: err}
	}

	addr := net.JoinHostPort(server.SSHHost, sshPort)
	cfg := &ssh.ClientConfig{
		User:    server.SSHUser,
		Auth:    []ssh.AuthMethod{ssh.PublicKeys(signer)}, // key-only; no password method exists here
		Timeout: r.connectTimeout,
	}
	// BOTH host-key fields, together — see core/sshhost. Setting only the callback left the client
	// advertising Go's default algorithm order (ECDSA and RSA ahead of Ed25519), so against a stock
	// OpenSSH server it negotiated an algorithm the operator had not pinned and this probe reported an
	// UNMODIFIED server as a host-key MISMATCH.
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
		return &handshakeError{stage: stageDial, err: err}
	}

	// The ctx watchdog, as in Run: x/crypto's handshake and session APIs predate context, so the deadline is
	// enforced by closing the transport, which aborts everything downstream of it immediately.
	watchdogDone := make(chan struct{})
	defer close(watchdogDone)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-watchdogDone:
		}
	}()

	cc, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		_ = conn.Close()
		if ctx.Err() != nil {
			return &handshakeError{stage: stageHandshake, err: ctx.Err()}
		}
		// A knownhosts refusal (unknown or changed host key) and an authentication refusal both surface
		// here; classifySessionFailure tells them apart by type, never by message text.
		return &handshakeError{stage: stageHandshake, err: err}
	}
	client := ssh.NewClient(cc, chans, reqs)
	defer func() { _ = client.Close() }()

	sess, err := client.NewSession()
	if err != nil {
		if ctx.Err() != nil {
			return &handshakeError{stage: stageSession, err: ctx.Err()}
		}
		return &handshakeError{stage: stageSession, err: err}
	}
	// Closed immediately and WITHOUT an exec request: opening the channel proves the account may open one
	// (a shell-less or ForceCommand-restricted account refuses here, and the agent's reads would too), while
	// sending no request is what keeps the promise that a test runs nothing on the operator's server.
	_ = sess.Close()
	return nil
}

// classifySessionFailure turns a failed handshake into something an operator can act on.
//
// It classifies on the SHAPE of the failure — the stage it stopped at, and for the handshake the concrete
// error TYPE x/crypto returned — never by parsing SSH prose, which differs between server implementations
// and versions. Anything unrecognised falls through to the underlying error rather than a guessed diagnosis:
// a confident wrong diagnosis costs an operator more than none, because they go and fix the thing we named.
//
// It never carries key material: the key-stage errors from parseKey name only the REFERENCE (proven by
// native_test.go), and nothing else here has ever seen the private key.
func classifySessionFailure(err error) string {
	var he *handshakeError
	if !errors.As(err, &he) {
		return err.Error()
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return "the connection did not complete inside the test's time budget — the server is reachable but " +
			"very slow to answer, or something is holding the TCP connection open without speaking SSH"
	}
	switch he.stage {
	case stageConfig:
		return "the configured row is incomplete (" + he.err.Error() + ") — no connection was attempted"
	case stageKnownHosts:
		return he.err.Error() + ". Host-key verification is mandatory and fails closed, so EVERY read from " +
			"EVERY syslog server is refused before it is attempted while this is true — the agent will report " +
			"device logs as unavailable and nothing will say why. Point " + KnownHostsEnv + " at an OpenSSH " +
			"known_hosts file carrying each syslog server's host key"
	case stageKey:
		return he.err.Error() + " — the ssh key for this row could not be turned into a usable credential. " +
			"This is a TG-side secret problem, not a syslog-server one: the reference in the row points " +
			"somewhere the key is not, resolves to nothing, or resolves to something that is not a private key " +
			"(a public key or a passphrase-protected key both look like this)"
	case stageDial:
		var dnsErr *net.DNSError
		if errors.As(he.err, &dnsErr) {
			return "the syslog server's name did not resolve (" + dnsErr.Name + ") — the ssh host in this row is " +
				"wrong, or the worker's DNS cannot see it. No credential left this process"
		}
		return "nothing accepted a connection on port " + sshPort + " — the server is down, sshd is not " +
			"listening, or a firewall between the worker and it is dropping the connection (" + he.err.Error() + ")"
	case stageHandshake:
		var keyErr *knownhosts.KeyError
		if errors.As(he.err, &keyErr) {
			if len(keyErr.Want) > 0 {
				// The dangerous one. Say plainly what it means and do not suggest editing the file first.
				return "the server presented a host key that does NOT match the one pinned in known_hosts, so " +
					"the connection was refused. Either that machine was rebuilt or re-keyed, or something is " +
					"answering in its place — verify the new fingerprint out of band with whoever runs the site " +
					"BEFORE updating known_hosts"
			}
			return "the server's host key is not in the known_hosts file, so the connection was refused. TG " +
				"never trusts a host key on first sight; add this server's host key to the file " + KnownHostsEnv +
				" names and test again"
		}
		var revoked *knownhosts.RevokedError
		if errors.As(he.err, &revoked) {
			return "the server's host key is marked REVOKED in known_hosts — the connection was refused, and " +
				"that entry was put there deliberately. Do not remove it without knowing who did"
		}
		return "the SSH handshake was refused (" + he.err.Error() + ") — the usual cause is authentication: " +
			"the ssh user in this row is wrong for that server, or the account's authorized_keys does not carry " +
			"the public half of the key this row's reference resolves to"
	case stageSession:
		return "the server authenticated TG but refused to open a session channel (" + he.err.Error() + ") — " +
			"the account is restricted (a ForceCommand, a shell-less login, or a MaxSessions limit). The agent's " +
			"log reads need a session, so they would fail the same way"
	default:
		return he.err.Error()
	}
}

// joinProbeNotes assembles the operator-facing Detail, so the pass and fail paths cannot drift into two
// different layouts.
func joinProbeNotes(notes []string) string {
	kept := make([]string, 0, len(notes))
	for _, n := range notes {
		if strings.TrimSpace(n) != "" {
			kept = append(kept, n)
		}
	}
	return strings.Join(kept, ". ")
}
