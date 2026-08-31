// This file is the host-diagnostics connector's answer to the console's TEST button (core/selftest.Tester).
//
// WHAT THE DESCRIPTOR PROMISES, made literally true: "resolve and parse each row's SSH key reference, and
// open the known_hosts file the reads verify against, reporting its entry count — no host is dialled and
// nothing is executed". Unlike syslog-ng, this connector has no fixed servers to handshake with — its
// targets are whatever estate hosts alert next — so a probe that dialled anything would certify a host the
// next incident will not be on.
//
// WHY THESE TWO CHECKS AND NOT A GREEN "ok". Both of this connector's production failures on 2026-08-03
// would have been caught by exactly these:
//   - the key at file:/secrets/one_key sat at 0640 — the OpenSSH client refuses such a key outright, and a
//     probe that resolves AND PARSES the reference surfaces the class before an incident does;
//   - known_hosts covered 16 of 38 alerting hosts, so most diagnostics failed closed at verification. A
//     probe cannot know which hosts will alert, but an operator reading "66 entries" against a 38-host
//     estate can — which is why the ENTRY COUNT is the summary, not a boolean.
package hostdiag

import (
	"context"
	"fmt"
	"os"
	"path"
	"strings"

	"github.com/territory-grounder/grounder/core/credential"
	"github.com/territory-grounder/grounder/modules/observability/syslogng"

	"github.com/territory-grounder/grounder/core/selftest"
	"github.com/territory-grounder/grounder/core/sshhost"
	"golang.org/x/crypto/ssh"
)

// Module is the console-facing connector object: the parsed allowlist plus the known_hosts path its reads
// verify against. It exists so the composition root offers ONE object whose probe checks the SAME inputs the
// agent's tools use — a probe over different inputs would certify a configuration the agent does not run.
type Module struct {
	accs       []Access
	knownHosts string
	// runner and resolver make the probe able to perform ONE REAL READ (TG-300/TG-301). nil = the
	// config-only probe this module shipped with, which stays valid for callers that have no runner.
	runner   syslogng.Runner
	resolver IdentityResolver
	// yield reports the probe's read to the seam register, exactly as an agent-tool read does.
	yield func(produced bool)
}

// WithProbeRead gives the probe a runner, so its green means "I read a host", not "the config parses".
//
// ★ WHY THIS EXISTS (TG-301). The original probe deliberately dialled nothing, and the reasoning above is
// sound: this connector's targets are whatever host alerts next, so certifying one host certifies the wrong
// thing. What that reasoning did not price is the CONSEQUENCE. The seam-yield register is fed only by the
// agent-tool path, and agent tools run only during triage. So a lane that could not read ANY host reported
// `hostdiag.read: unobserved` indefinitely — indistinguishable from a lane nobody had needed yet.
//
// Measured on 2026-08-04: the configured key authenticated to 0 of 20 estate hosts, every read failed, the
// probe sweep reported "10 ran, 10 ok", and the register said UNOBSERVED. Three separate surfaces, none of
// them wrong on its own terms, and together they said nothing was broken.
//
// So the probe now reads. NOT to certify a host — the summary says exactly that — but to prove the LANE can
// produce at all, and to give the register an observation on every sweep instead of only during triage.
func WithProbeRead(runner syslogng.Runner, resolver IdentityResolver, yield func(produced bool)) ModuleOption {
	return func(m *Module) { m.runner, m.resolver, m.yield = runner, resolver, yield }
}

// ModuleOption configures the probe module.
type ModuleOption func(*Module)

// NewModule builds the connector for the probe registry. knownHosts is passed IN by the composition root —
// resolved through the config chokepoint like every other module key — never read from the environment here
// (modules/ reading os.Getenv is the TG-260 bypass this change removes).
func NewModule(accs []Access, knownHosts string, opts ...ModuleOption) *Module {
	m := &Module{accs: accs, knownHosts: strings.TrimSpace(knownHosts)}
	for _, o := range opts {
		o(m)
	}
	return m
}

// SelfTest validates every allowlist row's key reference and the known_hosts file, without dialling anyone.
// One bad row is a FAILURE, not a footnote: a row that cannot authenticate costs exactly that site its
// diagnostics and nothing else changes — the same silent partial the syslog-ng probe refuses to bless.
func (m *Module) SelfTest(ctx context.Context, _ string) (selftest.Result, error) {
	if m == nil || len(m.accs) == 0 {
		return selftest.Result{}, fmt.Errorf("no host-diagnostics allowlist is configured (deployments is empty) — the agent has no host tools at all")
	}
	_ = ctx // both checks are local reads; nothing here can outlive a caller usefully

	var rows []string
	for _, a := range m.accs {
		material, err := a.KeyRef.Resolve()
		if err != nil {
			return selftest.Result{}, fmt.Errorf("row %s|%s: ssh key ref %q did not resolve (fail closed)", a.Site, a.HostGlob, string(a.KeyRef))
		}
		if strings.TrimSpace(material) == "" {
			return selftest.Result{}, fmt.Errorf("row %s|%s: ssh key ref %q resolved EMPTY (fail closed)", a.Site, a.HostGlob, string(a.KeyRef))
		}
		if _, err := ssh.ParsePrivateKey([]byte(material)); err != nil {
			return selftest.Result{}, fmt.Errorf("row %s|%s: ssh key ref %q resolved but did not parse as a private key — wrong file, or key material damaged in transit", a.Site, a.HostGlob, string(a.KeyRef))
		}
		rows = append(rows, a.Site+"|"+a.HostGlob+" as "+a.SSHUser)
	}

	// sshhost.New is the SAME constructor every real read goes through, so what this validates is what the
	// reads will actually accept — not a lookalike parse.
	if _, err := sshhost.New(m.knownHosts); err != nil {
		return selftest.Result{}, fmt.Errorf("%d allowlist row(s) ok, but every read would still be refused: %w", len(rows), err)
	}
	entries, err := KnownHostEntryCount(m.knownHosts)
	if err != nil {
		return selftest.Result{}, fmt.Errorf("known_hosts %s opened but could not be read: %w", m.knownHosts, err)
	}
	if entries == 0 {
		return selftest.Result{}, fmt.Errorf("known_hosts %s parses but holds ZERO entries — every diagnostic read on every host will be refused (fail closed)", m.knownHosts)
	}

	cfg := fmt.Sprintf("%d allowlist row(s) authenticate-ready; known_hosts %s holds %d host-key entr%s",
		len(rows), m.knownHosts, entries, map[bool]string{true: "y", false: "ies"}[entries == 1])
	detail := "Rows: " + strings.Join(rows, "; ") + ". The config half of this green proves the key " +
		"material and the known_hosts file, NOT per-host coverage: a host missing from known_hosts still " +
		"fails closed at read time. Compare the entry count against the size of your estate."

	// No runner supplied — the original config-only probe. Say so, rather than letting a green imply a read.
	if m.runner == nil || m.resolver == nil {
		return selftest.Result{
			Summary: cfg + "; NO read attempted (probe has no runner)",
			Detail:  detail,
		}, nil
	}

	host, out, err := m.probeRead(ctx)
	if m.yield != nil {
		m.yield(err == nil)
	}
	if err != nil {
		return selftest.Result{}, fmt.Errorf("%s — but the lane CANNOT READ: %w\n\nThis is the state that "+
			"looked healthy for weeks: keys parse, known_hosts loads, the boot log is cheerful, and every "+
			"actual read fails. Config being valid is not the lane working", cfg, err)
	}
	return selftest.Result{
		Summary: fmt.Sprintf("%s; READ %s ok (%d bytes)", cfg, host, len(out)),
		Detail: detail + fmt.Sprintf("\n\nThe read half dialled %s and ran a single read-only command. "+
			"It proves the LANE can produce — key, host-key verification, transport and the host's own "+
			"forced-command guard all end to end. It does NOT certify any other host; the next incident "+
			"will be on a host this probe never touched.", host),
	}, nil
}

// probeRead dials the first candidate that answers and runs ONE cheap read-only command.
//
// It walks candidates rather than pinning one host on purpose: a single pinned target turns an unrelated
// host reboot into a red probe, and an operator who learns to ignore a red probe has lost the control. Only
// when EVERY candidate fails does this report failure — that is the state worth alarming on, because it
// means the agent is blind to the whole estate.
func (m *Module) probeRead(ctx context.Context) (string, []byte, error) {
	cands := m.probeCandidates()
	if len(cands) == 0 {
		return "", nil, fmt.Errorf("no probe candidate: known_hosts %s holds %d entr(y/ies) but none matches "+
			"an allowlist host pattern — so the agent could not read a single one of them either", m.knownHosts, 0)
	}
	var lastErr error
	for _, h := range cands {
		bundle, err := m.resolver.Resolve(ctx, credential.Target{Host: h})
		if err != nil {
			lastErr = fmt.Errorf("%s: credential unresolved: %w", h, err)
			continue
		}
		// The SAME Server shape a real read builds (hostdiag.go:324), so the probe exercises the path
		// the agent uses rather than a lookalike.
		res, err := m.runner.Run(ctx, syslogng.Server{
			SSHHost: h, SSHUser: bundle.User(), KeyRef: bundle.SSHKeyRef(),
		}, []string{"uptime"})
		if err != nil {
			lastErr = fmt.Errorf("%s: %w", h, err)
			continue
		}
		return h, res.Stdout, nil
	}
	return "", nil, fmt.Errorf("every candidate refused (%d tried, last: %v)", len(cands), lastErr)
}

// probeCandidates lists known_hosts names that an allowlist row would admit, capped — the probe is a
// liveness check, not a sweep, and a sweep on a timer against the whole estate is its own problem.
func (m *Module) probeCandidates() []string {
	const maxCandidates = 4
	b, err := os.ReadFile(m.knownHosts)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, ln := range strings.Split(string(b), "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" || strings.HasPrefix(ln, "#") {
			continue
		}
		names := strings.Split(strings.Fields(ln)[0], ",")
		for _, n := range names {
			n = strings.TrimSpace(n)
			// A hashed known_hosts entry (|1|...) carries no readable name; skip rather than dial a hash.
			if n == "" || strings.HasPrefix(n, "|") || seen[n] {
				continue
			}
			for _, a := range m.accs {
				if ok, _ := path.Match(a.HostGlob, n); ok {
					seen[n] = true
					out = append(out, n)
					break
				}
			}
			if len(out) >= maxCandidates {
				return out
			}
		}
	}
	return out
}

// KnownHostEntryCount counts non-comment, non-blank lines — the number an operator compares against their
// estate. Counted directly rather than through the knownhosts package, which exposes no such number.
func KnownHostEntryCount(path string) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, ln := range strings.Split(string(b), "\n") {
		ln = strings.TrimSpace(ln)
		if ln != "" && !strings.HasPrefix(ln, "#") {
			n++
		}
	}
	return n, nil
}
