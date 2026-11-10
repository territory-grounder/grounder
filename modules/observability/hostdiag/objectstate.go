package hostdiag

// The OBJECT-STATE reader (TG-662): a positive read of the one thing a heal was supposed to fix — a systemd
// unit's active state or a docker container's state — over the SAME read-only diagnostic lane, identity and
// FIXED argvs the agent's check-host-services tool already runs. Nothing new reaches a host: every argv here is
// one of the allowlisted check steps (stepArgv looks them up by label, so this file cannot define a second,
// drifting shape), which is what lets the terminus confirm an object without any guard rollout.
//
// WHY. ConfirmedClear used to mean "the incident host carries no active alert" — and a quiet host can still
// have the unit down (the alert simply has not re-fired yet, or the monitor never watched that unit). The G4A
// review (F8) named the fail-open: unmonitored or unmapped reads certified heals. Observed=false here is the
// honest answer for every such case — not covered by a credential rule, read refused or failed, object not
// listed — and the caller treats it as INDETERMINATE, never as clear.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/territory-grounder/grounder/core/credential"
	"github.com/territory-grounder/grounder/modules/observability/syslogng"
)

// Object kinds the reader understands. The composition root maps an op-class FAMILY onto one of these.
const (
	ObjectUnit      = "unit"
	ObjectContainer = "container"
)

// ObjectReading is one read of one object. Healthy is meaningful ONLY when Observed is true; Observed=false
// carries the reason in Detail and must be treated as indeterminate by every consumer.
type ObjectReading struct {
	Healthy  bool
	Observed bool
	Detail   string
}

func unobserved(detail string) ObjectReading { return ObjectReading{Detail: detail} }

// NewObjectStateReader returns the reader, or nil when either collaborator is absent (an unwired lane reads
// nothing, honestly — the consumer's nil check reports "no object-state reader wired"). The identity resolves
// through the credential engine per read (fail closed: a host no rule covers is NOT investigable and reads
// unobserved — the "unmonitored target cannot be confirmed clear" rule in executable form).
func NewObjectStateReader(runner syslogng.Runner, resolver IdentityResolver) func(ctx context.Context, host, kind, name string) ObjectReading {
	if runner == nil || resolver == nil {
		return nil
	}
	return func(ctx context.Context, host, kind, name string) ObjectReading {
		h, err := validateHost(host)
		if err != nil {
			return unobserved("refused: " + err.Error())
		}
		name = strings.TrimSpace(name)
		if name == "" {
			return unobserved("no object name in the sealed params — nothing to read")
		}
		bundle, rerr := resolver.Resolve(ctx, credential.Target{Host: h, Purpose: credential.PurposeDiagRead})
		if rerr != nil {
			return unobserved("no resolvable read-only SSH credential for " + h + " — the target is outside the diagnostic lane (unmonitored)")
		}
		server := syslogng.Server{SSHHost: h, SSHUser: bundle.User(), KeyRef: bundle.SSHKeyRef()}
		read := func(label string) (string, bool) {
			argv, ok := stepArgv("check-host-services", label)
			if !ok {
				return "", false
			}
			rr, rerr := runBounded(ctx, runner, server, argv)
			if rerr != nil || rr.ExitCode != 0 {
				return "", false
			}
			return boundOutput(rr.Stdout), true
		}
		switch kind {
		case ObjectContainer:
			out, ok := read(svcContainersLabel)
			if !ok {
				return unobserved("the container list could not be read on " + h)
			}
			return containerReading(out, name)
		case ObjectUnit:
			ps, ok1 := read(svcProcStateLabel)
			failed, ok2 := read(svcFailedLabel)
			inactive, ok3 := read(svcInactiveLabel)
			if !ok1 || !ok2 || !ok3 {
				return unobserved("the systemd/process reads could not all be performed on " + h)
			}
			return unitReading(ps, failed, inactive, name)
		default:
			return unobserved("unknown object kind " + kind)
		}
	}
}

// stepArgv returns a COPY of the fixed argv of one named check step — the single source the reader is allowed
// to run. ok=false for an unknown check/label (the reader then reports unobserved rather than improvising).
func stepArgv(checkName, label string) ([]string, bool) {
	for _, c := range checks {
		if c.name != checkName {
			continue
		}
		for _, s := range c.steps {
			if s.label == label {
				return append([]string(nil), s.argv...), true
			}
		}
	}
	return nil, false
}

// runBounded mirrors diagTool.runStep's budget rule: a nearly-exhausted inherited deadline must not starve the
// read into a false "unreachable", so a short residual runs on a detached, hard-bounded context.
func runBounded(ctx context.Context, runner syslogng.Runner, server syslogng.Server, argv []string) (syslogng.RunResult, error) {
	base := ctx
	if dl, ok := ctx.Deadline(); ok && time.Until(dl) < defaultTimeout {
		base = context.WithoutCancel(ctx)
	}
	cctx, cancel := context.WithTimeout(base, defaultTimeout)
	defer cancel()
	return runner.Run(cctx, server, argv)
}

// containerReading reads `docker ps -a --format {{.Names}}|{{.State}}|{{.Status}}` for ONE container. Healthy iff
// its state is `running`. A container not in the list is UNOBSERVED (absent or renamed — the heal's object
// cannot be confirmed), never healthy and never "down".
func containerReading(raw, name string) ObjectReading {
	for _, ln := range strings.Split(raw, "\n") {
		f := strings.Split(strings.TrimSpace(ln), "|")
		if len(f) < 2 || strings.TrimSpace(f[0]) != name {
			continue
		}
		state := strings.TrimSpace(f[1])
		return ObjectReading{Healthy: strings.EqualFold(state, "running"), Observed: true,
			Detail: fmt.Sprintf("docker ps: container %s is %s", name, state)}
	}
	return unobserved("container " + name + " is not listed on the host — absent, renamed or unreadable")
}

// unitReading reads one systemd unit positively from the three check-host-services lists it already has:
//   - named in `systemctl --failed`                        ⇒ observed, NOT healthy
//   - named in `systemctl list-units --state=inactive`     ⇒ observed, NOT healthy (cleanly stopped)
//   - otherwise, its processes in `ps -eo pid,stat,unit,comm`:
//     all stopped (state T)                              ⇒ observed, NOT healthy (FROZEN — TG-624 G3)
//     at least one running                               ⇒ observed, healthy
//     none                                               ⇒ UNOBSERVED (a oneshot, a unit systemd does not
//     load, a different spelling — unestablished)
//
// The unit name is canonicalised with a `.service` suffix when it carries no type, matching every list's
// first column and ps's `unit` column.
func unitReading(ps, failed, inactive, unit string) ObjectReading {
	u := canonicalUnit(unit)
	if _, down := unitSet(failed)[u]; down {
		return ObjectReading{Observed: true, Detail: u + " is FAILED (systemctl --failed)"}
	}
	if _, down := unitSet(inactive)[u]; down {
		return ObjectReading{Observed: true, Detail: u + " is inactive (systemctl list-units --state=inactive)"}
	}
	procs, stopped := 0, 0
	for _, ln := range strings.Split(ps, "\n") {
		f := strings.Fields(ln)
		if len(f) < 4 || f[2] != u {
			continue
		}
		procs++
		if strings.HasPrefix(f[1], "T") {
			stopped++
		}
	}
	switch {
	case procs == 0:
		return unobserved(u + " is in no systemd list and owns no process — state unestablished")
	case stopped == procs:
		return ObjectReading{Observed: true, Detail: fmt.Sprintf("%s is FROZEN: all %d process(es) stopped (state T)", u, procs)}
	default:
		return ObjectReading{Healthy: true, Observed: true, Detail: fmt.Sprintf("%s is active: %d running process(es)", u, procs-stopped)}
	}
}

// canonicalUnit gives a bare unit name its `.service` type suffix; a typed name (`foo.service`, `bar.socket`)
// is left as spelled.
func canonicalUnit(unit string) string {
	unit = strings.TrimSpace(unit)
	if unit != "" && !strings.Contains(unit, ".") {
		unit += ".service"
	}
	return unit
}
