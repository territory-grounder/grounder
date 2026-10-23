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
	"strconv"
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
	// desc is the tool's one-line ACI description (agent.ACITool), rendered into the catalog as prompt DATA.
	// It lives on the check, not on diagTool, because the four tools are ONE type parameterised by this
	// catalogue: a single description would have to say "runs a diagnostic", which tells the model nothing
	// about WHICH of the four to reach for — and choosing the wrong check costs a cycle the 5-cycle poll
	// budget does not have. Adopted in TG-197 (see diagTool.Description).
	desc string
	// synthesize (optional) derives a high-signal SUMMARY from the raw step outputs (keyed by step label),
	// prepended above the raw sections. It exists because some anomalies are only visible by CORRELATING two
	// raw lists that individually hide the signal — see downServicesSummary. Pure text derivation; nil for
	// checks whose raw output already names the fault.
	synthesize func(stepOut map[string]string) string
	// summaryHeader titles the synthesized block. It is per-check because the header states what the block IS,
	// and a disk summary printed under "down services" would misdescribe its own contents.
	summaryHeader string
}

// Step labels for check-host-services — shared between the step definitions and downServicesSummary so the
// correlation can never silently break if a label is reworded.
const (
	svcFailedLabel   = "failed systemd units"
	svcInactiveLabel = "inactive service units (down services the failed-list misses)"
	svcEnabledLabel  = "enabled service unit files (the should-run baseline)"
	// On a container host the "service" that is down is a CONTAINER, and systemd knows nothing about it — see
	// the step comment below for the measured gap this closes.
	svcContainersLabel = "docker containers (name|state|status — every container, running or not)"
	// rootBackingLabel names the backing device of / — the fact that decides whether disk-grow can work at all.
	rootBackingLabel = "backing device of / (SOURCE FSTYPE SIZE USED USE%)"
)

// growableVerdict is the plain-language answer diskRemedySummary gives the agent about the root filesystem.
const (
	growNotLoopback = "the root filesystem is NOT loopback-backed, so disk-grow MAY be applicable — confirm the " +
		"underlying volume can actually be extended before proposing it."
	growLoopback = "the root filesystem is a LOOPBACK device (/dev/loopN). disk-grow CANNOT grow a loop-mounted " +
		"rootfs in place — proposing it here is an error, not a heal. Reclaiming space is the only remedy, and " +
		"TG has NO declared op-class that prunes, trims or vacuums. The correct outcome is therefore a REASONED " +
		"STAND-DOWN that names the consuming path from the du/journal output above, so a human knows where to look."
	growUnknown = "the backing device of / could not be read, so disk-grow applicability is UNKNOWN. Treat it as " +
		"unproven rather than available: an unread constraint is not a satisfied one."
)

// diskRemedySummary states, in one place, whether the root filesystem can be grown — turning a column in a
// wide df table into an explicit verdict the agent cannot skim past.
//
// It answers UNKNOWN rather than "growable" when the backing device cannot be read. That direction matters:
// the failure mode being closed here is proposing an inapplicable remedy, and defaulting an unread constraint
// to "available" would reintroduce exactly that.
func diskRemedySummary(stepOut map[string]string) string {
	raw := strings.TrimSpace(stepOut[rootBackingLabel])
	if raw == "" {
		return growUnknown
	}
	fields := strings.Fields(raw)
	if len(fields) == 0 {
		return growUnknown
	}
	src := fields[0]
	verdict := growNotLoopback
	// Match the DEVICE PATH, not a substring of the whole line: a mount whose label merely contains "loop"
	// must not be misread as loopback-backed.
	if strings.HasPrefix(src, "/dev/loop") {
		verdict = growLoopback
	}
	return "root filesystem: " + raw + "\n" + verdict
}

// checks is the read-only diagnostic catalogue, cloned from the predecessor's triage commands. Every argv is
// FIXED — the model chooses only WHICH named check to run and on which host, never the command itself.
var checks = []check{
	{name: "check-host-disk", summaryHeader: "derived: can the root filesystem be GROWN? (disk-grow applicability)", synthesize: diskRemedySummary,
		desc: "SSH a host and read its DISK state: usage per mount, the backing device of / (with an explicit " +
			"verdict on whether disk-grow can apply — a loopback-backed rootfs cannot be grown in place), the top " +
			"space consumers two levels deep, and the systemd journal's size. Use it on any disk-full alert: it " +
			"is what lets you NAME the consuming path instead of escalating blind.",
		steps: []step{
			{"df -h (filesystem usage per mount)", []string{"df", "-h"}},
			// The BACKING DEVICE of /, named on its own line. `df -h` already carries it in the Filesystem column,
			// but buried in a wide table it is routinely missed: measured over 96 disk-fill faults, TG cited the
			// loopback constraint and correctly stood down 63 times, and proposed an inapplicable disk-grow the
			// other 33. The fact that decides applicability was present every time and read two times in three.
			// Stating it alone, with the verdict spelled out in diskRemedySummary, is the same technique that made
			// restart-container proposable (MR !579) — surface the deciding fact, do not hope it is noticed.
			{rootBackingLabel, []string{"findmnt", "--noheadings", "--output", "SOURCE,FSTYPE,SIZE,USED,USE%", "/"}},
			// Two levels deep, not one: depth-1 names only WHICH top dir is big (/var), which is not actionable —
			// attributing the consumer needs the level below (/var/log, /var/lib/docker). A groundable disk-full
			// that "confirmed 98% but could not name the space consumer" stood down for exactly this blind spot.
			// -x stays on the / filesystem; output is bounded by maxOutputBytes (256 KiB, ample for two levels).
			{"du (top consumers, two levels, on /)", []string{"du", "-xh", "--max-depth=2", "/"}},
			// The systemd journal is the single most common runaway consumer; --disk-usage names it in one line so
			// the agent can attribute (and, later, a vacuum-journals op-class can act) rather than guess a reboot.
			{"journalctl --disk-usage (systemd journal size)", []string{"journalctl", "--disk-usage"}},
		}},
	{name: "check-host-memory",
		desc: "SSH a host and read its MEMORY state: total/used/free/available memory and swap, plus the " +
			"processes holding the most resident memory. Use it on a memory-pressure alert to attribute the " +
			"usage to a named process rather than reporting that memory is high.",
		steps: []step{
			{"free -m (memory)", []string{"free", "-m"}},
			{"top processes by memory", []string{"ps", "-eo", "pid,comm,%mem,rss", "--sort=-%mem", "--no-headers"}},
		}},
	{name: "check-host-services",
		desc: "SSH a host and read its SERVICE state: failed units, inactive units, the enabled (should-be-" +
			"running) unit files, and the Docker containers with their status — plus a derived list naming the " +
			"services that are DOWN. Use it on a service-down alert: a cleanly-stopped service is inactive, not " +
			"failed, and on this estate an app is usually a container rather than a unit, so this is what names " +
			"a concrete restart target.",
		steps: []step{
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
			// CONTAINERS ARE SERVICES TOO (grounded 2026-07-27 on a real container-down). The three systemctl lists
			// above are the whole picture only on a host whose services are systemd units. Measured on this estate,
			// they are not: of the 13 pool guests, ZERO run their app as a systemd unit — every one runs plain Docker
			// containers, so `systemctl` sees `docker.service` UP and reports nothing wrong while the app is down.
			// The consequence was not a worse answer, it was NO answer: `restart-container` requires params.container,
			// nothing in the agent's tool surface could name a container, and so the op-class — fully built, policy
			// ruled, allowlisted and lockstep-bound — was STRUCTURALLY UNPROPOSABLE. Verified end-to-end: stopping the
			// `mealie` container produced a LibreNMS Service-up/down alert in 2.5 min that TG could detect but never
			// act on. This step is the missing name. Pure fixed argv (the runner renders each element as one
			// shell-safe word — no pipes/sh -c, INV-02); read-only; `|` is a literal separator INSIDE one quoted argv,
			// never a shell pipe. A host without Docker exits 127, which the caller already renders as "may not apply
			// on this host" — so this is a no-op on the estate's non-container hosts rather than a false finding.
			{svcContainersLabel, []string{"docker", "ps", "-a", "--format", "{{.Names}}|{{.State}}|{{.Status}}"}},
		}, synthesize: downServicesSummary, summaryHeader: "derived: down services (NOT running — restart candidates)"},
	{name: "check-host-load",
		desc: "SSH a host and read its CPU/LOAD state: uptime and the 1/5/15-minute load averages, plus the " +
			"processes burning the most CPU. Use it on a load or CPU alert — and note the uptime, which tells " +
			"you whether the host rebooted recently.",
		steps: []step{
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
	// yield reports each read's outcome to the seam-yield register. Nil in tests and in any deployment
	// that has not wired it — a nil observer is a no-op, never a panic and never a changed result.
	yield func(produced bool)
}

func (t diagTool) Name() string   { return t.c.name }
func (t diagTool) ReadOnly() bool { return true }

// Description and Params publish the ACI schema (agent.ACITool): prompt DATA in the catalog, and the schema
// the loop screens a call against before it opens an SSH connection. ADOPTED IN TG-197 — these four tools
// shipped with neither, so the catalog rendered four bare names ("- check-host-disk", "- check-host-load", …)
// and the model had to GUESS both what each one reads and that they take a `host`. This is the lane where
// guessing costs most: an unresolvable credential and a wrong argument name produce the same shape of empty
// answer, and the lane already failed silently for weeks (TG-271). Screening the call against the schema
// means a mis-keyed call is refused with an actionable message instead of being spent as a read that
// produced nothing. Neither method's output becomes control flow (INV-08): the model still chooses only
// WHICH named check runs, never the command, and every argv stays fixed.
func (t diagTool) Description() string { return t.c.desc }

func (diagTool) Params() []agent.ParamSpec {
	return []agent.ParamSpec{{
		Name: "host", Type: "host", Required: true, Example: "app01",
		// ParamSpec carries no Aliases field, so the tolerated alternatives are named here — and note this
		// lane reads `target`/`hostname` but NOT `device`, unlike the LibreNMS tools. Stating the reader's
		// real key set is the point: a schema that promised an alias this tool ignores would send the model
		// down a silently-empty read.
		Description: "the host to SSH and diagnose — pass it under the key `host` (target/hostname are read " +
			"as fallbacks). It must be covered by a credential rule, or the read is refused",
	}}
}

func (t diagTool) Invoke(ctx context.Context, args map[string]string) (agent.ToolResult, error) {
	raw := firstNonEmpty(args["host"], args["target"], args["hostname"])
	res := agent.ToolResult{ID: t.c.name + "-" + sanitizeID(raw), Tool: t.c.name}

	// EVERY exit reports the pair, including the two refusals below. A refusal is a read that was
	// ATTEMPTED and produced nothing; counting only the paths that reach SSH would bias the ratio toward
	// health exactly when the lane is least usable — an unresolvable credential is as blind as a failed
	// handshake, and the register exists to say so.
	produced := false
	defer func() {
		if t.yield != nil {
			t.yield(produced)
		}
	}()

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
			header := t.c.summaryHeader
			if header == "" {
				header = "derived"
			}
			fmt.Fprintf(&sb, "\n\n=== %s ===\n%s", header, summary)
		}
	}
	sb.WriteString(sections.String())
	res.Success = anyOK
	res.Output = sb.String()
	produced = anyOK
	return res, nil
}

// downServicesSummary derives check-host-services' high-signal anomaly by CORRELATING its three raw lists: a
// service that is ENABLED (configured to start at boot) yet currently FAILED or INACTIVE is a down service —
// a concrete `restart-service <unit>` candidate. Neither raw list names it alone: `systemctl --failed` is
// empty for a cleanly stopped unit, and the inactive list buries the one down service among dozens of
// normally-inactive units with no enable-state to tell them apart (grounded 2026-07-24 on a real nginx-down:
// 0 failed, 58 inactive). The enabled unit-files are the should-run baseline; the intersection names the
// culprits. Returns "" when there is no baseline to reason from (older systemd / a read gap) — never a guess.
// It reports TWO INDEPENDENT families, because a host can run either kind of service (or both) and the two
// have different should-run baselines: systemd units correlate against `enabled`, containers do not correlate
// at all (Docker exposes no restart-policy field through `ps --format`). They are derived separately and
// SEPARATELY GATED — a host whose systemd baseline is unreadable must still get its container answer, and vice
// versa, or the one signal that names the fault is suppressed by the absence of the other.
func downServicesSummary(stepOut map[string]string) string {
	var parts []string
	if s := downUnitsSummary(stepOut); s != "" {
		parts = append(parts, s)
	}
	if s := downContainersSummary(stepOut); s != "" {
		parts = append(parts, s)
	}
	return strings.Join(parts, "\n")
}

// downUnitsSummary is the systemd half: ENABLED ∩ (FAILED ∪ INACTIVE).
func downUnitsSummary(stepOut map[string]string) string {
	enabled := unitSet(stepOut[svcEnabledLabel])
	if len(enabled) == 0 {
		return "" // no should-run baseline captured — do not fabricate a verdict from the noisy lists alone
	}
	// The harness filter runs BEFORE the enabled-∩, and that ordering is the fix, not a detail. A
	// systemd-run transient unit — which is exactly what `tg-restore-*` is — is NEVER in the enabled set, so
	// with the check inside the intersection it could not fire for the one unit it was written for. The test
	// that "proved" it only passed because its fixture listed the harness unit as enabled, which production
	// never does. Filtering first covers every not-running unit regardless of its enabled state.
	var harness int
	keep := func(set map[string]struct{}) map[string]struct{} {
		out := make(map[string]struct{}, len(set))
		for u := range set {
			if isHarnessUnit(u) {
				harness++
				continue
			}
			out[u] = struct{}{}
		}
		return out
	}
	failed := keep(unitSet(stepOut[svcFailedLabel]))
	inactive := keep(unitSet(stepOut[svcInactiveLabel]))

	// A FAILED unit is reported whether or not it is in the enabled baseline. The enabled-∩ exists to suppress
	// the noise of an enabled oneshot that ran and exited — an ambiguity that applies to INACTIVE units only.
	// `failed` carries no such ambiguity: systemd is asserting the unit tried to run and could not. Filtering
	// it through the baseline dropped real faults (a unit started by a timer or a dependency is failed-but-not-
	// enabled) and then let the block below announce "every enabled service is currently running" — an
	// all-clear covering a host with a failed unit on it.
	var down []string
	for u := range failed {
		down = append(down, u)
	}
	for u := range inactive {
		if _, ok := enabled[u]; ok {
			if _, dup := failed[u]; !dup {
				down = append(down, u)
			}
		}
	}
	sort.Strings(down)
	var summary string
	if len(down) == 0 {
		summary = "systemd units: none — no failed unit, and every enabled service is currently running"
	} else {
		// A oneshot service can legitimately be enabled+inactive after it ran and exited, so the agent still
		// confirms each candidate before acting; this list just makes the true down service impossible to miss.
		summary = "systemd units NOT running (restart-service / start-service candidates — the unit name is the `unit` param):\n" + strings.Join(down, "\n")
	}
	// A withheld candidate is DISCLOSED, never silently dropped — a list that quietly omits things reads as
	// "these are all of them" when it is not.
	if harness > 0 {
		summary += fmt.Sprintf("\n(withheld %d unit(s) belonging to TG's own fault-injection harness — "+
			"they are not estate services and must never be actuated)", harness)
	}
	// If any source list hit the per-step size cap, the intersection may be INCOMPLETE — say so rather than let
	// a truncated list read as an authoritative verdict (a silent cap reads as "covered everything" — it didn't).
	if hitOutputCap(stepOut[svcEnabledLabel]) || hitOutputCap(stepOut[svcInactiveLabel]) || hitOutputCap(stepOut[svcFailedLabel]) {
		summary += "\n(note: a systemctl list was truncated at the size cap — a down service beyond the cap may be missing above)"
	}
	return summary
}

// harnessUnitPrefixes names the transient systemd units TG's OWN fault injector creates on a target host to
// discharge a restore obligation. They are not estate services, and offering one as a remediation candidate is
// a category error: TG would be proposing to actuate its own test harness.
//
// Observed live — TG proposed `start-service` on `tg-restore-diskfill-104101701-1785159115.service`, an
// injector restore unit, and the action reached a sealed manifest and a human approval before the host-side
// allowed-units guard refused it. Only the fail-closed allowlist stopped it. Had that unit been allowlisted,
// TG would have "healed" by triggering its own restore and been credited for it — a false heal, and exactly
// the kind of self-referential loop that makes an autonomy number meaningless.
var harnessUnitPrefixes = []string{"tg-restore-", "tg-fault-", "tg-inject-"}

// isHarnessUnit reports whether a unit belongs to TG's own harness rather than to the estate.
func isHarnessUnit(unit string) bool {
	u := strings.ToLower(strings.TrimSpace(unit))
	for _, p := range harnessUnitPrefixes {
		if strings.HasPrefix(u, p) {
			return true
		}
	}
	return false
}

// downContainersSummary is the container half: every container whose state is not `running`, named so the agent
// can fill `restart-container`'s required `container` param.
//
// There is deliberately NO should-run baseline here, and the caveat is stated rather than guessed. Docker's
// `ps --format` exposes no restart-policy field, and the only way to read one is `docker inspect` over explicit
// container IDs — which cannot be expressed as ONE fixed argv (it needs command substitution, banned by
// INV-02). So rather than invent a baseline, this names the candidates and tells the agent exactly which
// false positive to rule out: a one-shot/batch container that has legitimately run to completion looks
// identical to a crashed service in this list. That mirrors the oneshot caveat the systemd half already
// carries. The agent has the alert (which names the failing service) and the container's own name/status to
// discriminate; it must confirm before acting.
func downContainersSummary(stepOut map[string]string) string {
	raw, ok := stepOut[svcContainersLabel]
	if !ok {
		return "" // the step did not run or the host has no Docker — say nothing rather than imply "all clear"
	}
	var down []string
	total := 0
	for _, ln := range strings.Split(raw, "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		f := strings.Split(ln, "|")
		if len(f) < 2 {
			continue
		}
		name, state := strings.TrimSpace(f[0]), strings.TrimSpace(f[1])
		if name == "" {
			continue
		}
		total++
		if !strings.EqualFold(state, "running") {
			status := ""
			if len(f) > 2 {
				status = " (" + strings.TrimSpace(f[2]) + ")"
			}
			down = append(down, name+" ["+state+"]"+status)
		}
	}
	if total == 0 {
		return "" // no parsable container inventory — not a container host, or the read returned nothing
	}
	sort.Strings(down)
	if len(down) == 0 {
		return "docker containers: none — all " + strconv.Itoa(total) + " containers are running"
	}
	summary := "docker containers NOT running (restart-container candidates — the container NAME is the `container` param):\n" +
		strings.Join(down, "\n") +
		"\n(confirm before acting: a one-shot/batch container that finished normally also appears here, and Docker" +
		" exposes no restart-policy through this read — use the alert and the container's role to tell them apart)"
	if hitOutputCap(raw) {
		summary += "\n(note: the container list was truncated at the size cap — a stopped container beyond the cap may be missing above)"
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
// Option configures the tool set without breaking the existing three-argument call.
type Option func(*diagTool)

// WithYield reports the outcome of EVERY read to the caller: attempted always, produced only when the
// read actually returned host output.
//
// It exists because this lane failed on every call for weeks with nothing saying so (TG-271). A tool that
// answers on every invocation and answers NOTHING is invisible to any check that counts invocations —
// the failure path returns a "(host was unreachable or the read errored)" sentinel, which is a perfectly
// good return value. Only the produced/attempted PAIR distinguishes a quiet estate from a blind agent.
func WithYield(fn func(produced bool)) Option {
	return func(t *diagTool) { t.yield = fn }
}

func NewTools(accs []Access, runner syslogng.Runner, resolver IdentityResolver, opts ...Option) []agent.Tool {
	if len(accs) == 0 || resolver == nil {
		return nil
	}
	if runner == nil {
		runner = syslogng.NewNativeRunner(os.Getenv(KnownHostsEnv))
	}
	tools := make([]agent.Tool, 0, len(checks))
	for _, c := range checks {
		t := diagTool{c: c, resolver: resolver, runner: runner, timeout: defaultTimeout}
		for _, o := range opts {
			o(&t)
		}
		tools = append(tools, t)
	}
	return tools
}
