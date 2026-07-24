// Package syslogng is the loadable READ-ONLY syslog-ng investigation connector (spec/008 REQ-823).
//
// It gives the triage agent two read-only tools (tools.go) that read a named device host's
// syslog-ng log during an incident — the competence the predecessor's cisco-asa-specialist and
// triage-researcher agents had and TG lacked. There is NO write path here: the connector observes
// device logs, nothing more (mutation stays OFF; INV-08 — the returned log text is an untrusted
// observation that never becomes control flow).
//
// The estate's devices log to one syslog-ng server per site (e.g. dc1syslogng01, dc2syslogng01),
// each laying files out as <basepath>/<device-host>/<YYYY>/<MM>/<device-host>-<YYYY-MM-DD>.log plus a
// <device-host>/today.log current file. A device host is routed to its site's server by a
// configuration-derived site-code prefix (dc1… → the NL server, dc2… → the GR server) — a
// config-driven map, never a hardcoded hostname. Every remote read is a FIXED argv command over a
// host-key-verified, non-interactive SSH session opened by a NATIVE in-process client
// (golang.org/x/crypto/ssh) — the worker ships on a distroless image with no `ssh` binary and no
// shell, so there is no subprocess, no shell string, no `sh -c`, and no user pattern is ever
// interpolated into a command (INV-02). Key material is a resolved-at-read-time reference parsed in
// memory, never written to disk (INV-13), and the server's host key MUST verify against the
// operator-declared known_hosts file or the connection is refused. A single day's file can be
// hundreds of MB (a hot ASA firewall), so both reads are bounded at the SERVER side (`tail -n`,
// `grep -F -m`) and again in Go (line + byte caps) under an enforced context timeout.
//
// Provenance: [F] cisco-asa-specialist / triage-researcher syslog reads · [O] INV-02/INV-08/INV-13/INV-17, spec/008.
package syslogng

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"path"
	"regexp"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/territory-grounder/grounder/core/config"
)

// SourceType is the connector slug this module serves.
const SourceType = "syslogng"

// DefaultBasePath is the syslog-ng root when a config row omits it.
const DefaultBasePath = "/mnt/logs/syslog-ng"

// defaultConnectTimeout bounds the TCP connect + SSH handshake.
const defaultConnectTimeout = 10 * time.Second

// sshPort is the fixed sshd port every syslog server is reached on.
const sshPort = "22"

// KnownHostsEnv names the deployment knob holding the path of the OpenSSH known_hosts file the native
// runner verifies EVERY server host key against. One file covers every configured server (known_hosts
// is a multi-host format by design). Host-key verification is mandatory and fails closed: no file, no
// connection — there is no trust-on-first-use and no insecure bypass anywhere in this package.
const KnownHostsEnv = "TG_SYSLOGNG_KNOWN_HOSTS"

// Server is one configured syslog-ng host — a config row. The NL and GR servers are two of these behind
// ONE connector (INV-18); per-site variance (ssh host, user, key ref, base path, routing prefix) is
// configuration, never a fork. KeyRef is a secret REFERENCE (env:/file:/store:), never a literal
// (INV-13). Site is a descriptive estate label, not a security boundary (ADR-0010).
type Server struct {
	Site       string           // descriptive site label (e.g. "NL"/"GR") — ADR-0010, not a boundary
	SSHHost    string           // e.g. dc1syslogng01
	SSHUser    string           // e.g. root
	KeyRef     config.SecretRef // e.g. "env:SYSLOGNG_SSH_KEY" or "file:/run/secrets/syslogng_key"
	BasePath   string           // e.g. /mnt/logs/syslog-ng
	HostPrefix string           // routing site-code prefix (e.g. "dc1"); derived from SSHHost when omitted
}

// locCodeRe extracts a leading site-code prefix: letters then digits (e.g. "dc1" from
// "dc1syslogng01" or "dc1fw01"; "dc2" from "dc2sw01"). A host with no such prefix
// (e.g. a bare IP) routes to no server.
var locCodeRe = regexp.MustCompile(`^[a-z]+[0-9]+`)

// locCode returns the leading site-code prefix of a host, lowercased ("" if none).
func locCode(h string) string {
	return locCodeRe.FindString(strings.ToLower(strings.TrimSpace(h)))
}

// ParseServers parses the operator-declared syslog-ng server list — `site|sshhost|sshuser|keyref|basepath`
// rows, `;`-separated (config-not-code). basepath is optional (defaults to DefaultBasePath); an optional
// 6th field pins the routing prefix explicitly, otherwise it is derived from the ssh host. A row missing
// ssh host, user, or key ref is skipped (no partial server). An empty spec yields no servers (the agent
// simply has no syslog-ng tools). The key ref is kept as a reference and resolved only at read time.
//
// Example: "NL|dc1syslogng01|root|env:SYSLOGNG_SSH_KEY|/mnt/logs/syslog-ng;GR|dc2syslogng01|root|env:SYSLOGNG_SSH_KEY|/mnt/logs/syslog-ng".
func ParseServers(spec string) []Server {
	var out []Server
	for _, row := range strings.Split(spec, ";") {
		row = strings.TrimSpace(row)
		if row == "" {
			continue
		}
		f := strings.Split(row, "|")
		for i := range f {
			f[i] = strings.TrimSpace(f[i])
		}
		if len(f) < 4 || f[1] == "" || f[2] == "" || f[3] == "" {
			continue // need at least site|sshhost|sshuser|keyref, with host/user/keyref present
		}
		s := Server{Site: f[0], SSHHost: f[1], SSHUser: f[2], KeyRef: config.SecretRef(f[3]), BasePath: DefaultBasePath}
		if len(f) >= 5 && f[4] != "" {
			s.BasePath = strings.TrimRight(f[4], "/")
		}
		if len(f) >= 6 && f[5] != "" {
			s.HostPrefix = strings.ToLower(f[5])
		} else {
			s.HostPrefix = locCode(s.SSHHost)
		}
		out = append(out, s)
	}
	return out
}

// resolveServer routes a validated device host to the one syslog-ng server whose site-code prefix it
// carries. It returns ok=false with an HONEST reason (never a path or an error that leaks the layout)
// when the host has no site prefix, matches no configured server, or ambiguously matches several.
func resolveServer(servers []Server, host string) (Server, bool, string) {
	want := locCode(host)
	if want == "" {
		return Server{}, false, fmt.Sprintf("no syslog-ng log source for host %q: it carries no site-code prefix (expected e.g. dc1… or dc2…)", host)
	}
	var matches []Server
	for _, s := range servers {
		if s.HostPrefix == want {
			matches = append(matches, s)
		}
	}
	switch len(matches) {
	case 0:
		return Server{}, false, fmt.Sprintf("no syslog-ng log source for host %q: its site prefix %q matches no configured syslog server", host, want)
	case 1:
		return matches[0], true, ""
	default:
		return Server{}, false, fmt.Sprintf("ambiguous syslog-ng routing for host %q: site prefix %q matches %d configured servers", host, want, len(matches))
	}
}

// ---- input validation (the model-chosen args are the untrusted surface) ----

// hostAllow is the strict host allowlist: letters, digits, dot, underscore, dash only. It structurally
// excludes a path separator; the extra checks below reject a parent-directory reference and a leading
// dash/dot so a device-host arg can never traverse the tree or be read as a flag.
var hostAllow = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

// dateRe is the strict YYYY-MM-DD shape; validateDate additionally requires a real calendar date.
var dateRe = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)

// normHost normalizes a host the same way the LibreNMS tools do: lowercase, first whitespace-free token,
// and the bare label with any DNS domain suffix stripped (a name-like first segment only, never a dotted
// IP). So "dc1fw01.example.net" and "dc1fw01" both become "dc1fw01".
func normHost(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	if i := strings.IndexAny(h, " \t#"); i >= 0 {
		h = strings.TrimSpace(h[:i])
	}
	if i := strings.Index(h, "."); i >= 0 && strings.ContainsAny(h[:i], "abcdefghijklmnopqrstuvwxyz") {
		h = h[:i]
	}
	return h
}

// validateHost normalizes and then hard-validates a model-chosen host. It returns the safe canonical host
// or an error whose message names the class of problem WITHOUT echoing the raw arg (the caller quotes it).
func validateHost(raw string) (string, error) {
	h := normHost(raw)
	if h == "" {
		return "", errors.New("no host provided (pass args.host)")
	}
	if len(h) > 100 {
		return "", errors.New("host is too long")
	}
	if !hostAllow.MatchString(h) {
		return "", errors.New("host has a disallowed character (allowed: letters, digits, '.', '_', '-')")
	}
	if strings.Contains(h, "..") {
		return "", errors.New("host contains a parent-directory reference")
	}
	if strings.HasPrefix(h, "-") || strings.HasPrefix(h, ".") {
		return "", errors.New("host has a leading '-' or '.'")
	}
	return h, nil
}

// validateDate returns the date to read: "" (default ⇒ the current today.log) or a real YYYY-MM-DD.
func validateDate(raw string) (string, error) {
	d := strings.TrimSpace(raw)
	if d == "" {
		return "", nil
	}
	if !dateRe.MatchString(d) {
		return "", errors.New("date must be YYYY-MM-DD")
	}
	if _, err := time.Parse("2006-01-02", d); err != nil {
		return "", errors.New("date is not a real calendar date")
	}
	return d, nil
}

// validatePattern validates a fixed-string search pattern. It is passed as a distinct argv element after
// `--` to a `grep -F`, so it is never a flag and never a shell token; the checks here are defense in depth
// (reject a control character, a leading dash, and an over-long pattern).
func validatePattern(raw string) (string, error) {
	p := raw
	if strings.TrimSpace(p) == "" {
		return "", errors.New("no search pattern provided (pass args.pattern)")
	}
	if len(p) > 256 {
		return "", errors.New("search pattern is too long (max 256)")
	}
	if strings.ContainsAny(p, "\x00\n\r") {
		return "", errors.New("search pattern contains a control character")
	}
	if strings.HasPrefix(p, "-") {
		return "", errors.New("search pattern must not start with '-'")
	}
	return p, nil
}

// logPath builds the remote log path from validated components only — no model text reaches it except the
// already-allowlisted host and the already-validated date. path.Join cleans the result (a further defense).
// An empty date selects the current file (<host>/today.log); a date selects the dated file.
func logPath(basePath, host, date string) (p, label string) {
	base := strings.TrimRight(basePath, "/")
	if base == "" {
		base = DefaultBasePath
	}
	if date == "" {
		return path.Join(base, host, "today.log"), "today (today.log)"
	}
	yyyy, mm := date[0:4], date[5:7]
	return path.Join(base, host, yyyy, mm, host+"-"+date+".log"), date
}

// ---- the remote-read seam ----

// RunResult is the raw outcome of a remote read. ExitCode carries the REMOTE command's status (a grep
// exit of 1 means "no match", not a failure), so each tool interprets it rather than the runner guessing.
type RunResult struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

// Runner runs a FIXED remote argv on a server and returns its result. The production impl is the
// native in-process SSH client (NewNativeRunner); the oracle injects a recording fake so the whole
// tool path runs in CI with no live SSH. Implementations MUST NOT build a shell command string.
type Runner interface {
	Run(ctx context.Context, server Server, argv []string) (RunResult, error)
}

// nativeRunner is the production runner: a NATIVE Go SSH client (golang.org/x/crypto/ssh). There is no
// subprocess — the worker's distroless image carries no `ssh` binary and no shell — and structurally no
// weaker posture is expressible: the key reference resolves and parses IN MEMORY (never a temp file,
// INV-13), the server host key MUST verify against the operator-declared known_hosts file (unknown or
// changed key ⇒ the handshake is refused; there is no insecure callback in this package), auth is
// key-only (no password method is ever offered), no PTY is requested, and the remote command is the
// per-element POSIX-quoted rendering of the fixed argv (INV-02).
type nativeRunner struct {
	// knownHosts is the OpenSSH known_hosts file path host-key verification reads. Empty ⇒ every Run
	// fails closed before dialing.
	knownHosts     string
	connectTimeout time.Duration
	// dial opens the transport (net.Dialer.DialContext in production; an in-process pair in the
	// SSH-server oracle). The SSH handshake, auth, and host-key check always run on top of it.
	dial func(ctx context.Context, addr string) (net.Conn, error)
}

// NewNativeRunner returns the production native SSH runner. knownHostsPath is the operator-declared
// OpenSSH known_hosts file (KnownHostsEnv); an empty path yields a runner that refuses every read
// (fail closed) rather than one that skips host-key verification.
func NewNativeRunner(knownHostsPath string) Runner {
	return &nativeRunner{knownHosts: strings.TrimSpace(knownHostsPath), connectTimeout: defaultConnectTimeout}
}

// Run connects user@host:22 in process, verifies the server host key against known_hosts, authenticates
// with the in-memory parsed key, and executes the fixed remote argv as ONE POSIX-quoted command word
// (the remote sshd's login shell re-parses it into exactly the fixed vector — the same contract the
// exec'd ssh client had). The context deadline is enforced end to end: the TCP dial honors ctx, and a
// watchdog closes the transport on ctx.Done, which aborts the handshake, the session, and any in-flight
// read. A non-zero REMOTE exit is a result (grep 1 = "no match"), not an error.
func (r *nativeRunner) Run(ctx context.Context, server Server, remoteArgv []string) (RunResult, error) {
	if len(remoteArgv) == 0 {
		return RunResult{}, errors.New("syslogng: empty remote argv")
	}
	if server.SSHHost == "" || server.SSHUser == "" {
		return RunResult{}, errors.New("syslogng: server missing ssh host or user")
	}
	// Host-key verification is MANDATORY and fails closed: no known_hosts file, no connection.
	if r.knownHosts == "" {
		return RunResult{}, fmt.Errorf("syslogng: no known_hosts file configured (set %s to the OpenSSH known_hosts file carrying each syslog server's host key): refusing to connect unverified", KnownHostsEnv)
	}
	hostKeys, err := knownhosts.New(r.knownHosts)
	if err != nil {
		return RunResult{}, fmt.Errorf("syslogng: known_hosts file %s is unusable (fail closed): %w", r.knownHosts, err)
	}
	signer, err := parseKey(server.KeyRef)
	if err != nil {
		return RunResult{}, err
	}

	addr := net.JoinHostPort(server.SSHHost, sshPort)
	cfg := &ssh.ClientConfig{
		User:            server.SSHUser,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)}, // key-only; no password method exists here
		HostKeyCallback: hostKeys,                                 // knownhosts: unknown/changed key ⇒ refuse
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
		return RunResult{}, fmt.Errorf("syslogng: dial %s: %w", addr, err)
	}

	// The ctx watchdog: x/crypto's handshake and session APIs predate context, so the deadline is
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
			return RunResult{}, fmt.Errorf("syslogng: ssh connect to %s aborted by deadline: %w", addr, ctx.Err())
		}
		// A knownhosts refusal (unknown or changed host key) surfaces here, by design.
		return RunResult{}, fmt.Errorf("syslogng: ssh handshake with %s refused: %w", addr, err)
	}
	client := ssh.NewClient(cc, chans, reqs)
	defer func() { _ = client.Close() }()

	sess, err := client.NewSession()
	if err != nil {
		return RunResult{}, fmt.Errorf("syslogng: ssh session on %s: %w", addr, err)
	}
	defer func() { _ = sess.Close() }()

	var stdout, stderr bytes.Buffer
	sess.Stdout = &stdout
	sess.Stderr = &stderr
	if err := sess.Run(remoteCommand(remoteArgv)); err != nil {
		var exitErr *ssh.ExitError
		if errors.As(err, &exitErr) {
			// The remote command ran and exited non-zero — a RESULT the tool interprets, not an error.
			return RunResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), ExitCode: exitErr.ExitStatus()}, nil
		}
		if ctx.Err() != nil {
			return RunResult{}, fmt.Errorf("syslogng: remote read on %s aborted by deadline: %w", addr, ctx.Err())
		}
		return RunResult{}, fmt.Errorf("syslogng: remote read on %s failed: %w", addr, err)
	}
	return RunResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), ExitCode: 0}, nil
}

// parseKey resolves the server's key REFERENCE at read time and parses it in memory (INV-13): key
// material never touches a filesystem path here. Every failure names the REF only — never a byte of
// what it resolved to — and fails closed.
func parseKey(ref config.SecretRef) (ssh.Signer, error) {
	material, err := ref.Resolve()
	if err != nil {
		return nil, fmt.Errorf("syslogng: ssh key ref %q did not resolve (fail closed)", string(ref))
	}
	if strings.TrimSpace(material) == "" {
		return nil, fmt.Errorf("syslogng: ssh key ref %q resolved empty (fail closed)", string(ref))
	}
	signer, err := ssh.ParsePrivateKey([]byte(material))
	if err != nil {
		return nil, fmt.Errorf("syslogng: ssh key ref %q did not parse as a private key (fail closed)", string(ref))
	}
	return signer, nil
}

// remoteCommand renders a command argv as ONE POSIX-shell-safe word for the remote login shell. Each
// element is single-quoted (an embedded single quote escaped as the classic `'\”`), so no element's
// spaces or shell metacharacters can inject or word-split on the remote host — the remote sshd parses
// this identically whether the transport is the exec'd ssh client or this native one. It mirrors the
// SSH actuation module's remote-quoting.
func remoteCommand(argv []string) string {
	quoted := make([]string, len(argv))
	for i, a := range argv {
		quoted[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(quoted, " ")
}
