// Package journal is the systemd-journal / sudo actor-evidence reader (spec/023 REQ-2314, the SECOND
// domain reader after PVE). It answers "WHO ran a privileged action on this host?" from the host's own
// journal — the authoritative record of sudo invocations — so the attributor can name the actor behind a
// host-level change (an admin who ran a service restart, a package change, a config edit) rather than
// treating every "host changed" as an anonymous fault.
//
// It is READ-ONLY by construction and reuses the estate's hardened native SSH read transport
// (modules/observability/syslogng.Runner): a FIXED argv over a host-key-verified, key-only, no-PTY,
// distroless-safe golang.org/x/crypto/ssh session — no subprocess, no shell, no `sh -c`, no interpolation
// (INV-02). The per-host SSH identity is resolved THROUGH the credential engine (spec/016), gated by an
// operator allowlist and a MANDATORY known_hosts file (unset ⇒ fail closed, no trust-on-first-use). Unlike
// the syslogng/hostdiag investigation tools — which hand raw log text to the model as an untrusted
// observation — this reader DETERMINISTICALLY parses `journalctl -o json` into typed Evidence records and
// NEVER surfaces raw log text to the model (REQ-2312/2313). Evidence collection is ADVISORY and fails OPEN
// (REQ-2307): an unresolvable host, a refused connection, or a read error degrades the session to the
// pre-feature ladder, never blocks it.
//
// Provenance: [F] owner epic (actor-attribution beyond PVE) · [O] INV-02/INV-11/INV-13/INV-17, spec/023.
package journal

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/territory-grounder/grounder/adapters/actorevidence"
	"github.com/territory-grounder/grounder/core/attribution"
	"github.com/territory-grounder/grounder/core/credential"
	"github.com/territory-grounder/grounder/modules/observability/syslogng"
)

// KnownHostsEnv names the deployment knob holding the OpenSSH known_hosts file carrying each estate host's
// SSH host key. Empty ⇒ the native runner refuses every read (fail closed) rather than connecting unverified.
const KnownHostsEnv = "TG_JOURNAL_KNOWN_HOSTS"

// AllowlistEnv names the operator allowlist of hosts this reader may read (config-not-code, INV-17):
// ';'-separated "site|hostglob|sshuser|keyref" rows, parsed by syslogng-style rules. A host not covered by
// a rule is not read — the reader returns no evidence for it (it reads unattributable), never a wildcard.
const AllowlistEnv = "TG_JOURNAL_DEPLOYMENTS"

// maxTimeout is the compiled ceiling on a single host read (mirrors the PVE reader): a hung journal read
// cannot stall triage beyond this regardless of the configured timeout.
const maxTimeout = 15 * time.Second

// IdentityResolver resolves a target host to the read-only SSH identity (login user + key REFERENCE) TG
// authenticates with, THROUGH the credential engine (spec/016) — never a hardcoded fallback. A fail-closed
// refusal means the host is not investigable and the read yields no evidence. *credential.AuditedResolver
// satisfies it.
type IdentityResolver interface {
	Resolve(ctx context.Context, target credential.Target) (credential.Bundle, error)
}

// Module is the journal actor-evidence Reader.
type Module struct {
	allow    []Access
	runner   syslogng.Runner
	resolver IdentityResolver
	timeout  time.Duration
}

// Access is one operator-declared READ-ONLY host access rule (mirrors hostdiag.Access): a host whose
// canonical name matches HostGlob is readable. The SSH identity itself comes from the credential engine;
// this rule is the coarse allowlist GATE deciding whether the reader may look at the host at all.
type Access struct {
	Site     string
	HostGlob string
}

// ParseAccess parses the AllowlistEnv spec: ';'-separated "site|hostglob|sshuser|keyref" entries. Only the
// site + hostglob are retained here (identity is resolved through the credential engine); a row missing the
// hostglob is skipped (fail-safe: an unparseable rule grants no access, never a wildcard).
func ParseAccess(spec string) []Access {
	var out []Access
	for _, entry := range strings.Split(spec, ";") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		f := strings.Split(entry, "|")
		if len(f) < 2 {
			continue
		}
		a := Access{Site: strings.TrimSpace(f[0]), HostGlob: strings.TrimSpace(f[1])}
		if a.HostGlob == "" {
			continue
		}
		out = append(out, a)
	}
	return out
}

// Option configures the Module.
type Option func(*Module)

// WithTimeout bounds each Read (config, with the compiled maxTimeout ceiling).
func WithTimeout(d time.Duration) Option {
	return func(m *Module) {
		if d > 0 && d <= maxTimeout {
			m.timeout = d
		}
	}
}

// New returns the journal actor-evidence reader over an operator allowlist, a syslogng read runner, and a
// credential-engine identity resolver.
func New(allow []Access, runner syslogng.Runner, resolver IdentityResolver, opts ...Option) *Module {
	m := &Module{allow: allow, runner: runner, resolver: resolver, timeout: maxTimeout}
	for _, o := range opts {
		o(m)
	}
	return m
}

// Domain identifies this reader's evidence family (keys the sanctioned-principal and self-identity config).
func (m *Module) Domain() string { return "journal" }

// ReadOnly is always true — the seam is read-only by construction.
func (m *Module) ReadOnly() bool { return true }

var _ actorevidence.Reader = (*Module)(nil)

// hostAllow is the strict host allowlist: a name-shaped label only (structurally excludes a path separator,
// a leading dash, and a shell metacharacter), so a target can never traverse or be read as a flag.
var hostAllow = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,99}$`)

// Read returns the privileged-action actor-evidence records for a target HOST within [since, until]: it
// gates the host against the operator allowlist, resolves the read-only SSH identity through the credential
// engine, runs a FIXED `journalctl -o json` argv for sudo records in the window over the native runner, and
// deterministically parses each record into a typed Evidence naming the host as Target. An unallowed host,
// an unresolvable credential, or a read error is advisory: the caller treats this domain's evidence as
// absent (REQ-2307).
func (m *Module) Read(ctx context.Context, target string, since, until time.Time) ([]attribution.Evidence, error) {
	host := strings.ToLower(strings.TrimSpace(target))
	if !hostAllow.MatchString(host) || strings.Contains(host, "..") {
		return nil, fmt.Errorf("journal: target %q is not a valid host label", target)
	}
	if !m.allowed(host) {
		return nil, fmt.Errorf("journal: host %q is not in the operator read allowlist (%s)", host, AllowlistEnv)
	}
	if m.resolver == nil || m.runner == nil {
		return nil, fmt.Errorf("journal: reader not fully configured (no resolver or runner)")
	}

	ctx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()

	bundle, err := m.resolver.Resolve(ctx, credential.Target{Host: host})
	if err != nil {
		return nil, fmt.Errorf("journal: no resolvable read-only SSH credential for %q (fail closed): %w", host, err)
	}
	server := syslogng.Server{SSHHost: host, SSHUser: bundle.User(), KeyRef: bundle.SSHKeyRef()}

	// FIXED argv (INV-02): journalctl in JSON, bounded to the window and to sudo records only. The times are
	// rendered RFC3339 in UTC — validated, never model text. `--no-pager` keeps it non-interactive; a non-zero
	// remote exit (e.g. no matching records) is a RunResult, not an error.
	argv := []string{
		"journalctl", "-o", "json", "--no-pager",
		"--since", since.UTC().Format("2006-01-02 15:04:05"),
		"--until", until.UTC().Format("2006-01-02 15:04:05"),
		"SYSLOG_IDENTIFIER=sudo",
	}
	rr, err := m.runner.Run(ctx, server, argv)
	if err != nil {
		return nil, fmt.Errorf("journal: read on %q failed: %w", host, err)
	}
	if rr.ExitCode != 0 && len(rr.Stdout) == 0 {
		// A non-zero exit with no output is "no matching records" (or the unit doesn't apply) — not an error,
		// simply no evidence for this host in the window.
		return nil, nil
	}
	return parseJournal(rr.Stdout, host, since, until), nil
}

// allowed reports whether the host is covered by any operator allowlist rule (a simple site-code / glob
// match; an empty allowlist covers nothing — fail-safe).
func (m *Module) allowed(host string) bool {
	for _, a := range m.allow {
		if hostGlobMatch(a.HostGlob, host) {
			return true
		}
	}
	return false
}

// hostGlobMatch matches a host against an operator glob: exact, a leading `*` suffix match, or a trailing
// `*` prefix match. Kept deliberately small — the allowlist is a coarse gate, not a routing table.
func hostGlobMatch(glob, host string) bool {
	glob = strings.ToLower(strings.TrimSpace(glob))
	switch {
	case glob == "" || host == "":
		return false
	case glob == "*":
		return true
	case strings.HasPrefix(glob, "*"):
		return strings.HasSuffix(host, glob[1:])
	case strings.HasSuffix(glob, "*"):
		return strings.HasPrefix(host, glob[:len(glob)-1])
	default:
		return glob == host
	}
}

// journalRow is the subset of `journalctl -o json` fields the reader consumes. Every value in JSON journal
// output is a string (or an array of strings for multi-value fields); the reader reads only these.
type journalRow struct {
	Message      string `json:"MESSAGE"`
	Comm         string `json:"_COMM"`
	RealtimeUsec string `json:"__REALTIME_TIMESTAMP"`
	Cursor       string `json:"__CURSOR"`
	Identifier   string `json:"SYSLOG_IDENTIFIER"`
}

// sudoInvoker parses the invoking principal from a sudo journal MESSAGE. Two shapes cover the events that
// name an actor:
//   - a COMMAND line: "kp : TTY=pts/0 ; PWD=/x ; USER=root ; COMMAND=/bin/foo" ⇒ "kp"
//   - a session line: "pam_unix(sudo:session): session opened for user root by kp(uid=0)" ⇒ "kp"
//
// It returns "" when no principal is named (e.g. a "session closed for user root" line carries no invoker).
// The principal character class includes '@' and '$' so an LDAP/Kerberos realm form ("alice@SEC.REALM")
// and a machine account ("host$") parse — this matters because the identity/auth seam (REQ-2315) resolves
// exactly these directory principals; a class that dropped '@' would leave a directory-enrolled admin
// unattributable. The class deliberately stops at whitespace and '(' so it can never swallow the rest of
// the message.
var (
	sudoCommandRe = regexp.MustCompile(`^\s*([A-Za-z0-9._@$-]+)\s+:\s`)
	sudoByRe      = regexp.MustCompile(`\bby\s+([A-Za-z0-9._@$-]+)\(uid=`)
)

func sudoInvoker(message string) string {
	if m := sudoByRe.FindStringSubmatch(message); m != nil {
		return m[1]
	}
	if m := sudoCommandRe.FindStringSubmatch(message); m != nil {
		return m[1]
	}
	return ""
}

// actionKind classifies a sudo record into a domain verb from its message, so the attributor and the
// downstream identity seam see a stable verb rather than raw text.
func actionKind(message string) string {
	switch {
	case strings.Contains(message, "COMMAND="):
		return "sudo-command"
	case strings.Contains(message, "session opened"):
		return "sudo-session-open"
	case strings.Contains(message, "session closed"):
		return "sudo-session-close"
	default:
		return "sudo"
	}
}

// parseJournal deterministically parses newline-delimited `journalctl -o json` output into Evidence records.
// It reads ONLY the typed fields it needs, extracts the invoking principal itself (never the model), keys the
// timestamp on __REALTIME_TIMESTAMP (microseconds), names the read host as Target, and re-checks the window
// (defense in depth against a clock skew or a journalctl that over-returns). A record naming no principal, or
// outside the window, or unparseable is skipped — never fatal.
func parseJournal(stdout []byte, host string, since, until time.Time) []attribution.Evidence {
	var out []attribution.Evidence
	for _, line := range strings.Split(string(stdout), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var r journalRow
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			continue // a malformed line proves nothing; skip it
		}
		if r.Identifier != "sudo" && r.Comm != "sudo" {
			continue
		}
		// Evidence requires a NAMED invoking principal (the sudo message's invoker) — the principal the
		// downstream identity seam resolves. A bare numeric login uid names no useful actor (a
		// "session closed for user root" line is not a mutation, and "root as root" is not attribution), so a
		// record naming no invoker proves nothing about WHO caused a change and is skipped.
		actor := sudoInvoker(r.Message)
		if actor == "" {
			continue
		}
		usec, err := strconv.ParseInt(strings.TrimSpace(r.RealtimeUsec), 10, 64)
		if err != nil || usec <= 0 {
			continue
		}
		at := time.UnixMicro(usec).UTC()
		if at.Before(since) || at.After(until) {
			continue
		}
		out = append(out, attribution.Evidence{
			Domain:     "journal",
			Actor:      actor,
			ActionKind: actionKind(r.Message),
			Target:     host,
			ObservedAt: at,
			Ref:        r.Cursor,
			Covered:    true,
		})
	}
	return out
}
