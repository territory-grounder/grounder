// Package k8saudit is the Kubernetes audit-log actor-evidence reader (spec/023 T-023-9,
// REQ-2306/REQ-2307, Phase 2). It answers "WHO changed this cluster object?" from the kube-apiserver's
// own audit log — the authoritative record of every authenticated API mutation — so the attributor can
// name the admin (or controller) behind a cluster change instead of treating every "k8s object changed"
// as an anonymous fault.
//
// TRANSPORT. The apiserver exposes no read API for its audit log, so this reader follows the journal
// reader's estate pattern exactly: a READ-ONLY, host-key-verified, key-only native SSH read
// (modules/observability/syslogng.Runner — no subprocess, no shell, no `sh -c`, INV-02) of the audit
// log FILE on each operator-declared control-plane host, with the per-host identity resolved THROUGH
// the credential engine (spec/016) and a MANDATORY known_hosts file. The remote read is one fixed
// bounded argv — `grep -m <cap> -F -- <target> <path>` — so the transfer is capped and the target
// string travels as a validated argv element, never interpolation.
//
// Unlike the raw-log investigation tools, this reader DETERMINISTICALLY parses audit.k8s.io/v1 Event
// JSON lines into typed Evidence records and never surfaces raw log text to the model (REQ-2312/2313).
// Evidence collection is ADVISORY and fails OPEN (REQ-2307): an undeclared cluster, an unresolvable
// identity, or a read error degrades the session to the pre-feature ladder, never blocks it.
//
// Provenance: [F] spec/023 Phase 2 · [O] INV-02/INV-11/INV-13/INV-17.
package k8saudit

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

// AllowlistEnv names the operator allowlist declaring which control planes this reader may read and
// where each keeps its audit log (config-not-code, INV-17): ';'-separated
// "site|controlplane-host|/absolute/audit.log" rows. Unset ⇒ the reader is not registered and k8s
// subjects read unattributable. The SSH identity itself comes from the credential engine.
const AllowlistEnv = "TG_K8SAUDIT_DEPLOYMENTS"

// KnownHostsEnv names the OpenSSH known_hosts file for host-key verification. It deliberately SHARES
// the journal reader's file (one estate, one host-key truth): unset ⇒ the native runner refuses every
// read (fail closed, no trust-on-first-use).
const KnownHostsEnv = "TG_JOURNAL_KNOWN_HOSTS"

// maxTimeout is the compiled ceiling on a single control-plane read (mirrors the journal reader).
const maxTimeout = 15 * time.Second

// maxMatchedLines caps the remote grep (and therefore the transfer and the parse) per control plane.
const maxMatchedLines = 2000

// IdentityResolver resolves a control-plane host to the read-only SSH identity TG authenticates with,
// THROUGH the credential engine (spec/016). *credential.AuditedResolver satisfies it.
type IdentityResolver interface {
	Resolve(ctx context.Context, target credential.Target) (credential.Bundle, error)
}

// Access is one operator-declared control plane: the host to SSH and the audit log path it keeps.
type Access struct {
	Site string
	Host string
	Path string
}

// pathAllow admits only an absolute, plainly-named log path: no "..", no whitespace, no shell
// metacharacter shapes — a mis-declared row grants no access rather than a strange read.
var pathAllow = regexp.MustCompile(`^/[a-zA-Z0-9._/-]{1,199}$`)

// ParseAccess parses AllowlistEnv: ';'-separated "site|host|path" rows. A row missing the host or
// carrying an invalid path is skipped (fail-safe: an unparseable rule grants no access, never a
// wildcard).
func ParseAccess(spec string) []Access {
	var out []Access
	for _, entry := range strings.Split(spec, ";") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		f := strings.Split(entry, "|")
		if len(f) < 3 {
			continue
		}
		a := Access{Site: strings.TrimSpace(f[0]), Host: strings.ToLower(strings.TrimSpace(f[1])), Path: strings.TrimSpace(f[2])}
		if a.Host == "" || !pathAllow.MatchString(a.Path) || strings.Contains(a.Path, "..") {
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

// Module is the k8s audit-log actor-evidence Reader.
type Module struct {
	planes   []Access
	runner   syslogng.Runner
	resolver IdentityResolver
	timeout  time.Duration
}

// New returns the k8s audit reader over the declared control planes, a syslogng read runner, and a
// credential-engine identity resolver.
func New(planes []Access, runner syslogng.Runner, resolver IdentityResolver, opts ...Option) *Module {
	m := &Module{planes: planes, runner: runner, resolver: resolver, timeout: maxTimeout}
	for _, o := range opts {
		o(m)
	}
	return m
}

// Domain identifies this reader's evidence family (keys the sanctioned-principal config).
func (m *Module) Domain() string { return "k8s-audit" }

// ReadOnly is always true — the seam is read-only by construction.
func (m *Module) ReadOnly() bool { return true }

var _ actorevidence.Reader = (*Module)(nil)

// targetAllow is the strict subject allowlist: a DNS-label/object-name shape only (structurally excludes
// a leading dash, whitespace, and every shell metacharacter), so the grep argument can never read as a
// flag or traverse anything.
var targetAllow = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,199}$`)

// mutatingVerbs is the closed set of audit verbs that CHANGE cluster state — the only records that name
// an actor worth attributing. Reads (get/list/watch) are noise for attribution and are dropped.
var mutatingVerbs = map[string]bool{
	"create": true, "update": true, "patch": true, "delete": true, "deletecollection": true,
}

// Read returns the actor-evidence records for a cluster object/node name within [since, until]: for each
// operator-declared control plane it resolves the read-only SSH identity, runs one FIXED bounded
// `grep -F` argv for lines naming the target, and deterministically parses each audit Event into a typed
// Evidence. Failure directions are advisory (REQ-2307): the caller treats the domain's evidence as
// absent. A clean read that finds no in-window mutation emits the affirmative coverage marker
// (REQ-2304 half 2) so a cluster mutation with no audit entry can reach attributed-suspicious instead of
// reading as an unobserved blind spot.
func (m *Module) Read(ctx context.Context, target string, since, until time.Time) ([]attribution.Evidence, error) {
	subject := strings.ToLower(strings.TrimSpace(target))
	if !targetAllow.MatchString(subject) || strings.Contains(subject, "..") {
		return nil, fmt.Errorf("k8saudit: target %q is not a valid object/node name", target)
	}
	if len(m.planes) == 0 {
		return nil, fmt.Errorf("k8saudit: no control plane declared (%s)", AllowlistEnv)
	}
	if m.resolver == nil || m.runner == nil {
		return nil, fmt.Errorf("k8saudit: reader not fully configured (no resolver or runner)")
	}

	ctx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()

	var out []attribution.Evidence
	covered := false
	var lastErr error
	for _, cp := range m.planes {
		bundle, err := m.resolver.Resolve(ctx, credential.Target{Host: cp.Host})
		if err != nil {
			lastErr = fmt.Errorf("k8saudit: no resolvable read-only SSH credential for %q (fail closed): %w", cp.Host, err)
			continue
		}
		server := syslogng.Server{SSHHost: cp.Host, SSHUser: bundle.User(), KeyRef: bundle.SSHKeyRef()}
		// FIXED argv (INV-02): a bounded fixed-string grep — the validated subject travels as its own argv
		// element behind `--`, never interpolation; -m caps the transfer; -F disables pattern semantics.
		argv := []string{"grep", "-m", strconv.Itoa(maxMatchedLines), "-F", "--", subject, cp.Path}
		rr, err := m.runner.Run(ctx, server, argv)
		if err != nil {
			lastErr = fmt.Errorf("k8saudit: read on %q failed: %w", cp.Host, err)
			continue
		}
		if rr.ExitCode != 0 && len(rr.Stdout) == 0 {
			// grep exit 1 with no output = the log holds NO line naming the subject: the read SUCCEEDED,
			// so this control plane affirmatively covers the subject with a clean miss.
			covered = true
			continue
		}
		covered = true
		out = append(out, parseAuditLines(rr.Stdout, subject, since, until)...)
	}
	if len(out) > 0 {
		return out, nil
	}
	if covered {
		return []attribution.Evidence{attribution.CoverageMarker(m.Domain(), subject, until)}, nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("k8saudit: no declared control plane could be read for %q", subject)
}

// auditEvent is the audit.k8s.io/v1 Event subset this reader consumes. Deterministic: unknown fields are
// ignored, a line that is not an audit Event is dropped.
type auditEvent struct {
	Kind    string `json:"kind"`
	AuditID string `json:"auditID"`
	Stage   string `json:"stage"`
	Verb    string `json:"verb"`
	User    struct {
		Username string `json:"username"`
	} `json:"user"`
	ObjectRef struct {
		Resource  string `json:"resource"`
		Namespace string `json:"namespace"`
		Name      string `json:"name"`
	} `json:"objectRef"`
	RequestReceivedTimestamp time.Time `json:"requestReceivedTimestamp"`
	StageTimestamp           time.Time `json:"stageTimestamp"`
}

// parseAuditLines deterministically parses audit-log JSON lines into Evidence: only completed
// (ResponseComplete) MUTATING verbs whose objectRef NAMES the subject, inside the window. Everything
// else — reads, other objects the grep coarse-matched (an annotation mentioning the subject, a
// same-prefix name), other stages, out-of-window records, non-JSON lines — is dropped, never guessed at.
func parseAuditLines(raw []byte, subject string, since, until time.Time) []attribution.Evidence {
	var out []attribution.Evidence
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.HasPrefix(line, "{") {
			continue
		}
		var ev auditEvent
		if json.Unmarshal([]byte(line), &ev) != nil || ev.Kind != "Event" {
			continue
		}
		if ev.Stage != "ResponseComplete" || !mutatingVerbs[ev.Verb] {
			continue
		}
		if !strings.EqualFold(ev.ObjectRef.Name, subject) {
			continue
		}
		at := ev.RequestReceivedTimestamp
		if at.IsZero() {
			at = ev.StageTimestamp
		}
		if at.IsZero() || at.Before(since) || at.After(until) {
			continue
		}
		kind := ev.Verb + ":" + ev.ObjectRef.Resource
		if ev.ObjectRef.Namespace != "" {
			kind += "/" + ev.ObjectRef.Namespace
		}
		out = append(out, attribution.Evidence{
			Domain:     "k8s-audit",
			Actor:      ev.User.Username,
			ActionKind: kind,
			Target:     subject,
			ObservedAt: at.UTC(),
			Ref:        ev.AuditID,
			Covered:    true,
		})
	}
	return out
}
