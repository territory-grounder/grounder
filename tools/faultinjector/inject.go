package faultinjector

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// FillPath is where a disk-fill allocates. It deliberately matches the path the previous bash engine used, so
// this engine's reconciler can also clean up fills stranded by its predecessor.
const FillPath = "/var/tmp/tg-tier1-fill.img"

// fillTargetPercent is the root-usage level a disk-fill aims for: inside the alerting band (LibreNMS rule 22
// fires at >=90%) but short of the 95% where a guest starts failing in ways that are not the fault under test.
const fillTargetPercent = 91

// DiskFillPlan is the computed allocation for one disk-fill, kept separate from its execution so the sizing
// arithmetic — the part that can wedge a guest if it is wrong — is unit-testable.
type DiskFillPlan struct {
	SizeBytes  int64
	UsedBytes  int64
	AllocBytes int64 // what to fallocate to reach the target
	FreeAfter  int64
}

// planDiskFill computes how much to allocate to bring usage to fillTargetPercent.
//
// It refuses rather than guesses whenever the result would be unsafe or pointless: a guest already at or above
// target needs no fault, and an allocation that would leave less than minFreeAfterBytes is refused because a
// guest with no headroom cannot run the very diagnostics TG is supposed to perform on it — the fault would be
// testing our ability to break a machine, not TG's ability to notice a disk filling.
func planDiskFill(sizeBytes, usedBytes int64) (DiskFillPlan, error) {
	const minFreeAfterBytes = 128 << 20 // 128 MiB
	if sizeBytes <= 0 {
		return DiskFillPlan{}, fmt.Errorf("unusable filesystem size %d", sizeBytes)
	}
	target := sizeBytes * fillTargetPercent / 100
	alloc := target - usedBytes
	if alloc <= 0 {
		return DiskFillPlan{}, fmt.Errorf("already at or above %d%% (used %d of %d) — no fault needed", fillTargetPercent, usedBytes, sizeBytes)
	}
	freeAfter := sizeBytes - target
	if freeAfter < minFreeAfterBytes {
		return DiskFillPlan{}, fmt.Errorf("target would leave only %d bytes free — refusing to wedge the guest", freeAfter)
	}
	return DiskFillPlan{SizeBytes: sizeBytes, UsedBytes: usedBytes, AllocBytes: alloc, FreeAfter: freeAfter}, nil
}

// readRootUsage reads the guest's root filesystem size/used in bytes via a fixed argv (no shell).
func readRootUsage(ctx context.Context, run Runner, host string) (size, used int64, err error) {
	out, code, err := run.Run(ctx, host, []string{"df", "-B1", "--output=size,used", "/"})
	if err != nil {
		return 0, 0, fmt.Errorf("df %s: %w", host, err)
	}
	if code != 0 {
		return 0, 0, fmt.Errorf("df %s exited %d", host, code)
	}
	return parseDF(out)
}

// parseDF extracts size and used from `df -B1 --output=size,used /` output.
func parseDF(out string) (size, used int64, err error) {
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		s, e1 := strconv.ParseInt(f[0], 10, 64)
		u, e2 := strconv.ParseInt(f[1], 10, 64)
		if e1 == nil && e2 == nil {
			return s, u, nil
		}
	}
	return 0, 0, fmt.Errorf("could not parse df output %q", out)
}

// InjectDeviceDown stops a guest. The caller MUST have name-asserted and recorded the obligation first.
// ErrPreEffect marks an injection that aborted PROVABLY BEFORE any remote effect could have run — a refused
// precondition, not a failed command. Only such a failure may close a restore obligation early.
//
// The distinction is load-bearing and was previously absent. `performInjection` returning an error was read as
// "nothing was broken", but SSHRunner reports a ctx kill as -1 and a transport loss as 255, both AFTER the
// remote `pct stop` / `docker stop` / `fallocate` may already have committed. Closing on that assumption is
// how a stopped guest gets recorded as restored and its quarantine released. An ambiguous failure must leave
// the obligation open; only a refusal that never reached the host may close it.
var ErrPreEffect = errors.New("faultinjector: injection aborted before any effect")

func InjectDeviceDown(ctx context.Context, run Runner, node, vmid string) error {
	_, code, err := run.Run(ctx, node, []string{"pct", "stop", vmid})
	if err != nil {
		return fmt.Errorf("pct stop %s@%s: %w", vmid, node, err)
	}
	if code != 0 {
		return fmt.Errorf("pct stop %s@%s exited %d", vmid, node, code)
	}
	return nil
}

// InjectDiskFill allocates a bounded file inside the guest to push root usage into the alerting band. It
// returns the plan actually applied so the caller can log the real numbers rather than the intended ones.
func InjectDiskFill(ctx context.Context, run Runner, host string) (DiskFillPlan, error) {
	size, used, err := readRootUsage(ctx, run, host)
	if err != nil {
		return DiskFillPlan{}, err
	}
	plan, err := planDiskFill(size, used)
	if err != nil {
		return DiskFillPlan{}, err
	}
	// fallocate is instant and reserves real blocks, so usage moves immediately and predictably.
	_, code, err := run.Run(ctx, host, []string{"fallocate", "-l", strconv.FormatInt(plan.AllocBytes, 10), FillPath})
	if err != nil {
		return plan, fmt.Errorf("fallocate on %s: %w", host, err)
	}
	if code != 0 {
		return plan, fmt.Errorf("fallocate on %s exited %d", host, code)
	}
	return plan, nil
}

// InjectLogFill grows an OPERATOR-DECLARED application log until root usage reaches the alerting band — the
// runaway-log shape a real estate produces, and the only disk-pressure fault an honest reclaim verb can heal.
//
// It reuses planDiskFill's arithmetic verbatim, which means it inherits the same two refusals: a guest already
// at/above target needs no fault, and an allocation leaving less than the free-space floor is refused rather
// than wedging a guest that then cannot run the diagnostics TG exists to perform on it.
//
// The path is validated AGAIN here even though the pool loader validates at declaration time. That is not
// belt-and-braces decoration: this function is exported and takes a path, so a future caller that skips the
// loader would otherwise be able to append to /var/log/journal — and the restore would then TRUNCATE it. A
// destructive default must be unreachable from every entry point, not just the current one.
//
// The growth uses `fallocate` on the log file itself: instant, real blocks, no shell, and — unlike `dd` or a
// redirect — it needs no shell metacharacters, which keeps the fixed-argv contract intact.
func InjectLogFill(ctx context.Context, run Runner, host, logPath string) (DiskFillPlan, error) {
	if err := ValidLogPath(logPath); err != nil {
		return DiskFillPlan{}, fmt.Errorf("%w: log-fill on %s: %v", ErrPreEffect, host, err)
	}
	size, used, err := readRootUsage(ctx, run, host)
	if err != nil {
		return DiskFillPlan{}, err
	}
	plan, err := planDiskFill(size, used)
	if err != nil {
		return DiskFillPlan{}, err
	}
	// Ensure the directory exists before growing into it — an operator-declared log for a service that has not
	// written yet is a legitimate declaration, not an error. mkdir -p is idempotent and takes a fixed argv.
	if idx := strings.LastIndex(logPath, "/"); idx > 0 {
		if _, code, mkErr := run.Run(ctx, host, []string{"mkdir", "-p", "--", logPath[:idx]}); mkErr != nil || code != 0 {
			return plan, fmt.Errorf("mkdir for %s on %s: err=%v code=%d", logPath, host, mkErr, code)
		}
	}
	_, code, err := run.Run(ctx, host, []string{"fallocate", "-l", strconv.FormatInt(plan.AllocBytes, 10), logPath})
	if err != nil {
		return plan, fmt.Errorf("fallocate %s on %s: %w", logPath, host, err)
	}
	if code != 0 {
		return plan, fmt.Errorf("fallocate %s on %s exited %d", logPath, host, code)
	}
	return plan, nil
}

// InjectContainerDown stops ONE operator-declared docker container inside a guest. The guest itself stays UP,
// which is the whole point: the service goes down while the host remains reachable, so the fault is detected
// as a SERVICE fault and healed by `restart-container` — no other class in the rotation reaches that op-class
// (device-down stops the whole guest, healed by start-guest; disk-fill never touches a service).
//
// Placement note (the docuseal01 lesson): a device-down restore must be armed on the Proxmox NODE because the
// guest is about to stop. A container-down restore runs on the GUEST, which stays up throughout, so there is
// no such hazard — the undo is the plain inverse on the same host.
//
// The container name is never chosen here; it comes from the operator's pool declaration.
func InjectContainerDown(ctx context.Context, run Runner, host, container string) error {
	if strings.TrimSpace(container) == "" {
		return fmt.Errorf("%w: container-down on %s: no container declared — refusing to pick one", ErrPreEffect, host)
	}
	_, code, err := run.Run(ctx, host, []string{"docker", "stop", container})
	if err != nil {
		return fmt.Errorf("docker stop %s@%s: %w", container, host, err)
	}
	if code != 0 {
		return fmt.Errorf("docker stop %s@%s exited %d", container, host, code)
	}
	return nil
}

// VerifyContainerRunning reports whether a container is running, read from the guest. It VERIFIES a
// container-down repair rather than trusting `docker start`'s exit code — the same discipline
// VerifyGuestRunning applies, for the same reason: an obligation that cannot be verified discharged must
// leave its target quarantined rather than be assumed complete (spec/025 REQ-2508).
func VerifyContainerRunning(ctx context.Context, run Runner, host, container string) (bool, error) {
	if strings.TrimSpace(container) == "" {
		return false, fmt.Errorf("verify container on %s: no container name", host)
	}
	out, code, err := run.Run(ctx, host, []string{"docker", "inspect", "-f", "{{.State.Status}}", container})
	if err != nil {
		return false, err
	}
	if code != 0 {
		return false, fmt.Errorf("docker inspect %s@%s exited %d", container, host, code)
	}
	return strings.TrimSpace(out) == "running", nil
}

// InjectServiceDown stops ONE operator-declared systemd unit inside a guest. The guest itself stays UP, so
// the fault presents as a SERVICE fault rather than a host outage — which is the only way to reach the
// `restart-service` and `start-service` op-classes. Container-down's systemd twin, and it carries the same
// placement property: the undo runs on the GUEST, which never goes down, so there is no node-vs-guest hazard.
//
// The unit name is never chosen here; it comes from the operator's pool declaration.
func InjectServiceDown(ctx context.Context, run Runner, host, unit string) error {
	if strings.TrimSpace(unit) == "" {
		return fmt.Errorf("%w: service-down on %s: no unit declared — refusing to pick one", ErrPreEffect, host)
	}
	_, code, err := run.Run(ctx, host, []string{"systemctl", "stop", unit})
	if err != nil {
		return fmt.Errorf("systemctl stop %s@%s: %w", unit, host, err)
	}
	if code != 0 {
		return fmt.Errorf("systemctl stop %s@%s exited %d", unit, host, code)
	}
	return nil
}

// VerifyServiceActive reports whether a unit is active, read from the guest. It VERIFIES a service-down
// repair rather than trusting `systemctl start`'s exit code, for the same reason as its siblings: an
// obligation that cannot be verified discharged must leave its target quarantined rather than be assumed
// complete (spec/025 REQ-2508).
//
// `systemctl is-active` exits NON-ZERO for every inactive state, so the exit code is not an error here — it
// is the answer. Treating it as a failure would make a genuinely-still-stopped unit report as "unverifiable"
// and quarantine a host that is simply not fixed yet.
func VerifyServiceActive(ctx context.Context, run Runner, host, unit string) (bool, error) {
	if strings.TrimSpace(unit) == "" {
		return false, fmt.Errorf("verify service on %s: no unit name", host)
	}
	out, _, err := run.Run(ctx, host, []string{"systemctl", "is-active", unit})
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "active", nil
}

// VerifyGuestRunning reports whether a guest is running, read from the owning node. Used to VERIFY a
// device-down repair rather than trusting `pct start`'s exit code (it exits non-zero on an already-running
// guest, which is a success for our purposes).
func VerifyGuestRunning(ctx context.Context, run Runner, node, vmid string) (bool, error) {
	out, code, err := run.Run(ctx, node, []string{"pct", "status", vmid})
	if err != nil {
		return false, err
	}
	if code != 0 {
		return false, fmt.Errorf("pct status %s@%s exited %d", vmid, node, code)
	}
	return strings.Contains(out, "running"), nil
}

// VerifyAppHealthy runs the guest's OPERATOR-DECLARED app-health probe and reports whether it passed.
// TG-226 — this is the check that was missing from a device-down restore.
//
// `pct status` returning "running" proves the GUEST is back. It proves nothing about the applications
// inside, which after a hard stop may come back with their downstream connection pools wedged: live
// 2026-07-31, a Node app's Mongoose pool buffered every DB operation to a 10s timeout for ~5 hours while
// the database was up, `mongo ping` answered, and static endpoints returned 200. ICMP, LibreNMS
// device-status and pct status all reported healthy for the whole outage. Only a request that goes
// THROUGH the data path can tell the difference, and only the operator knows what that request is.
//
// FIXED ARGV, NEVER A SHELL (AGENTS.md non-negotiable). ValidHealthProbe already refuses a declaration
// carrying shell metacharacters, so the split here cannot silently turn a pipeline into literal args.
//
// A NON-ZERO EXIT IS THE ANSWER, NOT AN ERROR — the same reasoning as VerifyServiceActive: probes signal
// failure by exit code, and treating that as a transport error would report a genuinely wedged app as
// "unverifiable" instead of "not repaired". A transport failure still surfaces, because Runner returns a
// real error for that.
func VerifyAppHealthy(ctx context.Context, run Runner, host, probe string) (bool, error) {
	if err := ValidHealthProbe(probe); err != nil {
		return false, fmt.Errorf("verify app health on %s: %w", host, err)
	}
	_, code, err := run.Run(ctx, host, strings.Fields(strings.TrimSpace(probe)))
	if err != nil {
		return false, err
	}
	return code == 0, nil
}

// VerifyFillRemoved reports whether the fill file is gone from a guest. Used to VERIFY a disk-fill repair —
// `rm -f` succeeds on an absent file, so its exit code alone proves nothing.
func VerifyFillRemoved(ctx context.Context, run Runner, host, path string) (bool, error) {
	_, code, err := run.Run(ctx, host, []string{"test", "-e", path})
	if err != nil {
		return false, err
	}
	// ONLY exit 1 proves absence. `code != 0` was FAIL-OPEN: SSHRunner deliberately converts a non-zero remote
	// exit into (out, code, nil) — "a non-zero remote exit is DATA, not a transport failure" — and the ssh
	// client itself exits 255 when it cannot connect or authenticate, while a CommandContext kill yields -1.
	// Both therefore arrived here as a nil error with a non-zero code and were read as "the fill file is gone".
	//
	// That inverted the one verification this package calls its guarantee: an UNREACHABLE guest — the state in
	// which a fill is most likely still present — reported as repaired, MarkRestored closed the obligation
	// permanently (Outstanding selects only 'pending'/'failed', so the row is never re-read), and busyHosts
	// released the quarantine so the planner could stack another fault on a guest still at 91%.
	//
	// Both sibling verifiers already fail closed on a non-zero code; this was the only one that inverted it,
	// and the only one that targets the GUEST rather than a surviving Proxmox node.
	switch code {
	case 0:
		return false, nil // the path exists — the fill is still there
	case 1:
		return true, nil // `test -e` says absent — the ONLY proof of removal
	default:
		return false, fmt.Errorf("test -e %s@%s exited %d — unknown, not proof of removal (255=unreachable, "+
			"-1=timed out); the obligation stays open and the host stays quarantined", path, host, code)
	}
}

// VerifyLogTruncated confirms a log-fill was repaired: the declared log is back to (near) empty.
//
// SIZE, not existence — a truncated log still exists, so VerifyFillRemoved's `test -e` would report a
// correctly-repaired estate as permanently stranded. `test ! -s` is true for an empty OR absent file, which
// covers both the truncate and the case where the service's own rotation removed the file first.
//
// It fails CLOSED on any unexpected exit code, for the exact reason documented on VerifyFillRemoved above:
// SSHRunner turns a non-zero remote exit into (out, code, nil), the ssh client exits 255 when it cannot
// connect, and a context kill yields -1 — so treating "not 0" as proof would report an UNREACHABLE guest
// (precisely the state where the fill is most likely still there) as repaired, close the obligation
// permanently, and release the quarantine so the planner could stack another fault on a guest still at 91%.
// Only exit 0 is proof.
func VerifyLogTruncated(ctx context.Context, run Runner, host, path string) (bool, error) {
	if err := ValidLogPath(path); err != nil {
		return false, fmt.Errorf("log-fill verify on %s: unsafe path %q: %w", host, path, err)
	}
	_, code, err := run.Run(ctx, host, []string{"test", "!", "-s", path})
	if err != nil {
		return false, err
	}
	switch code {
	case 0:
		return true, nil // empty or absent — the ONLY proof the fill is gone
	case 1:
		return false, nil // the file still has bytes — the fill is still there
	default:
		return false, fmt.Errorf("test ! -s %s@%s exited %d — unknown, not proof of truncation (255=unreachable, "+
			"-1=timed out); the obligation stays open and the host stays quarantined", path, host, code)
	}
}
