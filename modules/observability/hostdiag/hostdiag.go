// Package hostdiag gives the triage agent the predecessor's READ-ONLY host investigation: SSH to the
// alerting host and run a FIXED read-only diagnostic (df, du, free, systemctl --failed, ps, uptime) — the
// ability the predecessor's storage-specialist / triage-researcher had and TG lacked. Without it the agent
// could not GROUND a disk-full it could have answered with one `df`, so it escalated instead of proposing,
// starving the predict/verify loop.
//
// Every tool is: READ-ONLY (ReadOnly()=true; the ToolSet refuses a mutating tool), argv-only (a FIXED command
// vector — no shell, no model-supplied command string, INV-02), host-key VERIFIED (native crypto/ssh against
// the operator-declared known_hosts; no known_hosts ⇒ every read fails closed), routed to an SSH identity by
// an operator allowlist (config-not-code), output-bounded, and returns an UNTRUSTED observation (INV-08 —
// nothing in the returned text becomes control flow). Each check is a SEPARATELY NAMED tool taking only a
// {host} arg, because the protocol preamble lists tool NAMES to the model: the name states what it does.
//
// It reuses the syslog-ng module's native in-process SSH runner (the distroless worker carries no ssh binary).
// Provenance: [F] the predecessor triage-researcher/storage-specialist SSH `df -h` / `free` / `systemctl`
// investigation, re-expressed as fixed-argv read-only tools under the typed spine.
package hostdiag

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/territory-grounder/grounder/agent"
	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/credential"
	"github.com/territory-grounder/grounder/modules/observability/syslogng"
)

// IdentityResolver resolves a target host to the SSH identity (login user + key REFERENCE) TG authenticates
// with, THROUGH the credential engine (spec/016) — the read-only investigation path no longer reads identity
// straight off the allowlist. *credential.AuditedResolver satisfies it. On a fail-closed refusal
// (ErrUnresolved / ErrAmbiguous) the host is not investigable and the tool refuses — there is NO hardcoded
// one_key+root fallback.
type IdentityResolver interface {
	Resolve(ctx context.Context, target credential.Target) (credential.Bundle, error)
}

// KnownHostsEnv names the deployment knob holding the OpenSSH known_hosts file carrying each estate host's
// SSH host key. Empty ⇒ the native runner refuses every read (fail closed) rather than connecting unverified.
const KnownHostsEnv = "TG_HOSTDIAG_KNOWN_HOSTS"

const (
	defaultTimeout = 25 * time.Second
	maxOutputBytes = 1 << 18 // 256 KiB per check step
)

// Access is one operator-declared READ-ONLY SSH access rule (config-not-code, INV-17): a host whose canonical
// name matches HostGlob is reachable as SSHUser with KeyRef. KeyRef is a secret REFERENCE (env:/file:/store:),
// never a literal.
type Access struct {
	Site     string
	HostGlob string
	SSHUser  string
	KeyRef   config.SecretRef
}

// ParseAccess parses TG_HOSTDIAG_DEPLOYMENTS: ';'-separated "site|hostglob|sshuser|keyref" entries. A row
// missing a field is skipped (fail-safe: an unparseable rule simply grants no access, never a wildcard).
func ParseAccess(spec string) []Access {
	var out []Access
	for _, entry := range strings.Split(spec, ";") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		f := strings.Split(entry, "|")
		if len(f) < 4 {
			continue
		}
		a := Access{
			Site:     strings.TrimSpace(f[0]),
			HostGlob: strings.TrimSpace(f[1]),
			SSHUser:  strings.TrimSpace(f[2]),
			KeyRef:   config.SecretRef(strings.TrimSpace(f[3])),
		}
		if a.HostGlob == "" || a.SSHUser == "" || a.KeyRef == "" {
			continue
		}
		out = append(out, a)
	}
	return out
}

var hostRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9.-]{0,62}$`)

func validateHost(h string) (string, error) {
	h = strings.TrimSpace(h)
	if h == "" {
		return "", fmt.Errorf("no host given")
	}
	if !hostRe.MatchString(h) {
		return "", fmt.Errorf("host %q is not a valid hostname", h)
	}
	return h, nil
}

// globMatch does a simple case-insensitive glob against a leading/trailing '*' (enough for site prefixes like
// "dc1*"). No '*' ⇒ exact match. "*" ⇒ any host.
func globMatch(glob, host string) bool {
	glob = strings.ToLower(strings.TrimSpace(glob))
	host = strings.ToLower(strings.TrimSpace(host))
	switch {
	case glob == "*":
		return true
	case strings.HasPrefix(glob, "*") && strings.HasSuffix(glob, "*"):
		return strings.Contains(host, strings.Trim(glob, "*"))
	case strings.HasSuffix(glob, "*"):
		return strings.HasPrefix(host, strings.TrimSuffix(glob, "*"))
	case strings.HasPrefix(glob, "*"):
		return strings.HasSuffix(host, strings.TrimPrefix(glob, "*"))
	default:
		return glob == host
	}
}

var idRe = regexp.MustCompile(`[^a-zA-Z0-9]+`)

func sanitizeID(s string) string {
	s = idRe.ReplaceAllString(strings.TrimSpace(s), "-")
	if len(s) > 48 {
		s = s[:48]
	}
	return strings.Trim(s, "-")
}

func boundOutput(b []byte) string {
	if len(b) > maxOutputBytes {
		return string(b[:maxOutputBytes]) + "\n…(truncated to the response cap)"
	}
	return string(b)
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// step is one fixed read-only remote argv with a human label.
type step struct {
	label string
	argv  []string
}

// check is a named diagnostic (one agent tool) and the fixed read-only steps it runs.
type check struct {
	name  string
	steps []step
	// synthesize (optional) derives a high-signal SUMMARY from the raw step outputs (keyed by step label),
	// prepended above the raw sections. It exists because some anomalies are only visible by CORRELATING two
	// raw lists that individually hide the signal — see downServicesSummary. Pure text derivation; nil for
	// checks whose raw output already names the fault.
	synthesize func(stepOut map[string]string) string
}

// Step labels for check-host-services — shared between the step definitions and downServicesSummary so the
// correlation can never silently break if a label is reworded.
const (
	svcFailedLabel   = "failed systemd units"
	svcInactiveLabel = "inactive service units (down services the failed-list misses)"
	svcEnabledLabel  = "enabled service unit files (the should-run baseline)"
)

// checks is the read-only diagnostic catalogue, cloned from the predecessor's triage commands. Every argv is
// FIXED — the model chooses only WHICH named check to run and on which host, never the command itself.
var checks = []check{
	{name: "check-host-disk", steps: []step{
		{"df -h (filesystem usage per mount)", []string{"df", "-h"}},
		// Two levels deep, not one: depth-1 names only WHICH top dir is big (/var), which is not actionable —
		// attributing the consumer needs the level below (/var/log, /var/lib/docker). A groundable disk-full
		// that "confirmed 98% but could not name the space consumer" stood down for exactly this blind spot.
		// -x stays on the / filesystem; output is bounded by maxOutputBytes (256 KiB, ample for two levels).
		{"du (top consumers, two levels, on /)", []string{"du", "-xh", "--max-depth=2", "/"}},
		// The systemd journal is the single most common runaway consumer; --disk-usage names it in one line so
		// the agent can attribute (and, later, a vacuum-journals op-class can act) rather than guess a reboot.
		{"journalctl --disk-usage (systemd journal size)", []string{"journalctl", "--disk-usage"}},
	}},
	{name: "check-host-memory", steps: []step{
		{"free -m (memory)", []string{"free", "-m"}},
		{"top processes by memory", []string{"ps", "-eo", "pid,comm,%mem,rss", "--sort=-%mem", "--no-headers"}},
	}},
	{name: "check-host-services", steps: []step{
		{svcFailedLabel, []string{"systemctl", "--failed", "--no-legend", "--no-pager"}},
		// Service-fault grounding (MR !529 follow-up): a service that was CLEANLY stopped (or masked) is
		// `inactive`, NOT `failed`, so the `--failed` list above is EMPTY for the very service that is down —
		// the agent then had NO target unit and stood down EMPTY on a real service-down. Also list the
		// INACTIVE service units: that surfaces the down service by name (e.g. `nginx.service`). Pure argv (the
		// SSH runner renders each element as one shell-safe word — NO pipes/sh -c, which are banned); read-only.
		{svcInactiveLabel, []string{"systemctl", "list-units", "--type=service", "--state=inactive", "--no-legend", "--no-pager"}},
		// The DISCRIMINATOR (grounded 2026-07-24 on a real nginx-down: 0 failed, 58 inactive units). The
		// inactive list buries the ONE down service among dozens of normally-inactive units, and neither list
		// shows enable-state — so the agent cannot tell "nginx should be running but isn't" from noise. The
		// enabled unit-files are the should-run baseline; downServicesSummary intersects them with the
		// failed+inactive sets to name the actual down services as concrete restart-service candidates.
		{svcEnabledLabel, []string{"systemctl", "list-unit-files", "--type=service", "--state=enabled", "--no-legend", "--no-pager"}},
	}, synthesize: downServicesSummary},
	{name: "check-host-load", steps: []step{
		{"uptime / load average", []string{"uptime"}},
		{"top processes by cpu", []string{"ps", "-eo", "pid,comm,%cpu", "--sort=-%cpu", "--no-headers"}},
	}},
}

// diagTool is one read-only SSH diagnostic tool.
type diagTool struct {
	c        check
	resolver IdentityResolver
	runner   syslogng.Runner
	timeout  time.Duration
}

func (t diagTool) Name() string   { return t.c.name }
func (t diagTool) ReadOnly() bool { return true }

func (t diagTool) Invoke(ctx context.Context, args map[string]string) (agent.ToolResult, error) {
	raw := firstNonEmpty(args["host"], args["target"], args["hostname"])
	res := agent.ToolResult{ID: t.c.name + "-" + sanitizeID(raw), Tool: t.c.name}

	host, err := validateHost(raw)
	if err != nil {
		res.Output = fmt.Sprintf("refused: %v", err)
		return res, nil
	}
	// Resolve the SSH identity THROUGH the credential engine (spec/016), not straight off the allowlist. A
	// fail-closed refusal (no covering rule/source, or an ambiguous match) means the host is not investigable —
	// refuse, NEVER fall back to a hardcoded identity. The winning bundle carries a SecretRef reference only;
	// the key is loaded by the native runner at read time. The resolver appends the credential_resolution audit
	// row (REQ-1617) as a side effect of this call.
	bundle, rerr := t.resolver.Resolve(ctx, credential.Target{Host: host})
	if rerr != nil {
		res.Output = fmt.Sprintf("no resolvable SSH credential for %s — it is not covered by any credential rule/source (or the match is ambiguous), so I cannot investigate it directly", host)
		return res, nil
	}

	server := syslogng.Server{SSHHost: host, SSHUser: bundle.User(), KeyRef: bundle.SSHKeyRef()}
	// Raw per-step sections are built here; the synthesized summary (if any) is composed AHEAD of them at the
	// end so the correlated high-signal line is the first thing the agent reads.
	var sections strings.Builder
	stepOut := make(map[string]string, len(t.c.steps))
	anyOK := false
	for _, s := range t.c.steps {
		rr, runErr := t.runStep(ctx, server, s.argv)
		if runErr != nil && ctx.Err() == nil {
			// One bounded retry — a transient blip or brief SSH contention must not make the agent escalate a
			// disk-full it could ground on a second attempt. The read is idempotent and read-only, so a retry is
			// safe (INV-21). Skip it only when the PARENT context is already cancelled (respect real cancellation).
			rr, runErr = t.runStep(ctx, server, s.argv)
		}
		fmt.Fprintf(&sections, "\n\n=== %s ===\n", s.label)
		switch {
		case runErr != nil:
			// Operator diagnostic (worker log, NOT agent-visible): the error CATEGORY plus a BOUNDED detail so a
			// recurring SSH failure is actually traceable — a category alone ("hostkey") hid whether the cause was
			// a name-form the known_hosts didn't cover, a real key change, or a dial failure. Bounded to keep any
			// stderr/path out of the AGENT's recorded observation; this line goes only to the worker's own log.
			log.Printf("hostdiag: %s on %s: ssh read failed (%s): %s", t.c.name, host, classify(runErr), boundErr(runErr))
			fmt.Fprintf(&sections, "(%s was unreachable or the read errored)", host)
		case rr.ExitCode != 0:
			fmt.Fprintf(&sections, "(command exited %d — it may not apply on this host)", rr.ExitCode)
		default:
			anyOK = true
			out := strings.TrimRight(boundOutput(rr.Stdout), "\n")
			stepOut[s.label] = out
			sections.WriteString(out)
		}
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "%s on %s (read-only, via %s@%s):", t.c.name, host, bundle.User(), host)
	// A synthesized summary correlates raw lists that individually hide the fault (see downServicesSummary).
	// Only emit it when at least one step succeeded — a summary over nothing would read as a false "all clear".
	if anyOK && t.c.synthesize != nil {
		if summary := t.c.synthesize(stepOut); summary != "" {
			fmt.Fprintf(&sb, "\n\n=== derived: down services (enabled but NOT running — restart candidates) ===\n%s", summary)
		}
	}
	sb.WriteString(sections.String())
	res.Success = anyOK
	res.Output = sb.String()
	return res, nil
}

// downServicesSummary derives check-host-services' high-signal anomaly by CORRELATING its three raw lists: a
// service that is ENABLED (configured to start at boot) yet currently FAILED or INACTIVE is a down service —
// a concrete `restart-service <unit>` candidate. Neither raw list names it alone: `systemctl --failed` is
// empty for a cleanly stopped unit, and the inactive list buries the one down service among dozens of
// normally-inactive units with no enable-state to tell them apart (grounded 2026-07-24 on a real nginx-down:
// 0 failed, 58 inactive). The enabled unit-files are the should-run baseline; the intersection names the
// culprits. Returns "" when there is no baseline to reason from (older systemd / a read gap) — never a guess.
func downServicesSummary(stepOut map[string]string) string {
	enabled := unitSet(stepOut[svcEnabledLabel])
	if len(enabled) == 0 {
		return "" // no should-run baseline captured — do not fabricate a verdict from the noisy lists alone
	}
	notRunning := unitSet(stepOut[svcFailedLabel])
	for u := range unitSet(stepOut[svcInactiveLabel]) {
		notRunning[u] = struct{}{}
	}
	var down []string
	for u := range notRunning {
		if _, ok := enabled[u]; ok {
			down = append(down, u)
		}
	}
	sort.Strings(down)
	var summary string
	if len(down) == 0 {
		summary = "none — every enabled service is currently running"
	} else {
		// A oneshot service can legitimately be enabled+inactive after it ran and exited, so the agent still
		// confirms each candidate before acting; this list just makes the true down service impossible to miss.
		summary = strings.Join(down, "\n")
	}
	// If any source list hit the per-step size cap, the intersection may be INCOMPLETE — say so rather than let
	// a truncated list read as an authoritative verdict (a silent cap reads as "covered everything" — it didn't).
	if hitOutputCap(stepOut[svcEnabledLabel]) || hitOutputCap(stepOut[svcInactiveLabel]) || hitOutputCap(stepOut[svcFailedLabel]) {
		summary += "\n(note: a systemctl list was truncated at the size cap — a down service beyond the cap may be missing above)"
	}
	return summary
}

// hitOutputCap reports whether a step's stored output was truncated by boundOutput (its cap marker is present).
func hitOutputCap(s string) bool { return strings.Contains(s, "truncated to the response cap") }

// unitSet extracts systemd unit names — the first whitespace field, minus any leading status glyph (● * ○) —
// from one `systemctl` list. It is uniform across --failed / list-units / list-unit-files, whose first column
// is the unit name; a line without a dotted unit token (a stray footer, a blank) is skipped. The leading glyph
// strip is NOT dead code: `--no-legend` still prefixes not-found/masked units with `●` (grounded on a live
// host: `● apparmor.service   not-found inactive dead`), so it must be trimmed to recover the unit token.
func unitSet(raw string) map[string]struct{} {
	set := map[string]struct{}{}
	for _, ln := range strings.Split(raw, "\n") {
		f := strings.Fields(strings.TrimLeft(ln, "●*○ \t"))
		if len(f) == 0 {
			continue
		}
		if u := f[0]; strings.Contains(u, ".") {
			set[u] = struct{}{}
		}
	}
	return set
}

// runStep runs one fixed read-only argv with a GUARANTEED budget. The disk check is cheap (<1s) and read-only,
// but the agent's context can arrive nearly exhausted after many slow model cycles; inheriting that residual
// would starve the SSH into a false "unreachable" (observed in prod: `df` aborted by a ~120ms residual deadline
// while a fresh 25s context to the same host completes in <200ms). When the inherited budget is below the step
// timeout, run on a DETACHED but still hard-bounded (t.timeout) context so the critical read gets its full
// window; when there is ample budget, keep the parent context so triage cancellation still propagates.
func (t diagTool) runStep(ctx context.Context, server syslogng.Server, argv []string) (syslogng.RunResult, error) {
	base := ctx
	if dl, ok := ctx.Deadline(); ok && time.Until(dl) < t.timeout {
		base = context.WithoutCancel(ctx)
	}
	cctx, cancel := context.WithTimeout(base, t.timeout)
	defer cancel()
	return t.runner.Run(cctx, server, argv)
}

// boundErr renders an error as a single bounded line for the operator log: newlines collapsed and length capped,
// so a multi-line/oversized underlying error can't flood the worker log or smuggle formatting into it.
func boundErr(err error) string {
	if err == nil {
		return ""
	}
	s := strings.ReplaceAll(strings.ReplaceAll(err.Error(), "\n", " "), "\r", " ")
	const cap = 160
	if len(s) > cap {
		s = s[:cap] + "…"
	}
	return s
}

// classify reduces a runner error to a bounded operator category (no secrets, paths, or stderr) for logging, so
// a recurring failure is diagnosable as deadline/hostkey/auth/dial rather than the swallowed generic reason.
func classify(err error) string {
	if err == nil {
		return "ok"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline"
	}
	s := strings.ToLower(err.Error())
	switch {
	case strings.Contains(s, "deadline"):
		return "deadline"
	case strings.Contains(s, "unusable") || strings.Contains(s, "permission denied") || strings.Contains(s, "no such file"):
		// The known_hosts or key FILE can't be READ (perms/missing) — a deploy/config fault, NOT a host-key
		// change. Kept DISTINCT from "hostkey": a non-root worker that can't read root-owned /secrets surfaced as
		// "known_hosts …unusable… permission denied", which the old classifier bucketed as "hostkey" and sent a
		// perms bug masquerading as a key mismatch (that misdirection cost real investigation time).
		return "secrets-unreadable"
	case strings.Contains(s, "knownhosts") || strings.Contains(s, "key mismatch") || strings.Contains(s, "host key") || strings.Contains(s, "known_hosts"):
		return "hostkey"
	case strings.Contains(s, "handshake") || strings.Contains(s, "authenticate") || strings.Contains(s, "no supported methods") || strings.Contains(s, "parse private key"):
		return "auth-or-handshake"
	case strings.Contains(s, "dial") || strings.Contains(s, "no such host") || strings.Contains(s, "connection refused") || strings.Contains(s, "network is unreachable") || strings.Contains(s, "i/o timeout"):
		return "dial"
	default:
		return "other"
	}
}

// NewTools returns the read-only host-diagnostics tools bound to the allowlist gate, the runner, and the
// credential resolver. accs gates whether the agent has host-diagnostics tools AT ALL (an empty allowlist ⇒
// nil, no tools); the per-host SSH IDENTITY is resolved at invoke time through the resolver (spec/016), not
// read off accs. A nil resolver ALSO yields nil (no identity source ⇒ nothing is investigable — fail closed,
// never a hardcoded identity). A nil runner selects the production native in-process SSH runner (host-key
// verified against KnownHostsEnv; unset ⇒ every read fails closed).
func NewTools(accs []Access, runner syslogng.Runner, resolver IdentityResolver) []agent.Tool {
	if len(accs) == 0 || resolver == nil {
		return nil
	}
	if runner == nil {
		runner = syslogng.NewNativeRunner(os.Getenv(KnownHostsEnv))
	}
	tools := make([]agent.Tool, 0, len(checks))
	for _, c := range checks {
		tools = append(tools, diagTool{c: c, resolver: resolver, runner: runner, timeout: defaultTimeout})
	}
	return tools
}
