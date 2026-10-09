package faultinjector

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Runner executes an argv vector on a remote host. Argv-only, never a shell string: this component runs
// destructive commands against production, so an injected space or semicolon must be incapable of becoming a
// second command (the same reasoning as INV-02 on the product side).
type Runner interface {
	Run(ctx context.Context, host string, argv []string) (stdout string, exitCode int, err error)
}

// SSHRunner runs argv over ssh with a fixed identity. It never interpolates into a shell.
type SSHRunner struct {
	KeyPath string
	Timeout time.Duration
}

// Run executes argv on host. The argv elements are passed to ssh as separate arguments, so the remote shell
// receives them already tokenised.
func (r SSHRunner) Run(ctx context.Context, host string, argv []string) (string, int, error) {
	to := r.Timeout
	if to <= 0 {
		to = 25 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, to)
	defer cancel()

	args := []string{
		"-i", r.KeyPath,
		"-o", "StrictHostKeyChecking=no",
		"-o", "ConnectTimeout=8",
		"-o", "BatchMode=yes",
		"root@" + host,
	}
	args = append(args, argv...)

	cmd := exec.CommandContext(ctx, "ssh", args...)
	out, err := cmd.Output()
	if ee, ok := err.(*exec.ExitError); ok {
		return string(out), ee.ExitCode(), nil // a non-zero remote exit is DATA, not a transport failure
	}
	if err != nil {
		return string(out), -1, fmt.Errorf("ssh %s: %w", host, err)
	}
	return string(out), 0, nil
}

// AssertGuestName verifies that vmid on node really is the guest we think it is, immediately before acting on
// it. Proxmox vmids are reused and a stale pool entry is indistinguishable from a correct one until you have
// already stopped the wrong machine — which has happened here before. Any doubt (unreachable node, empty
// hostname, mismatch) is an error, never a proceed.
func AssertGuestName(ctx context.Context, run Runner, node, vmid, wantName string) error {
	out, code, err := run.Run(ctx, node, []string{"pct", "config", vmid})
	if err != nil {
		return fmt.Errorf("name-assert %s@%s: %w", vmid, node, err)
	}
	if code != 0 {
		return fmt.Errorf("name-assert %s@%s: pct config exited %d", vmid, node, code)
	}
	got := ""
	for line := range strings.SplitSeq(out, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "hostname:"); ok {
			got = strings.TrimSpace(rest)
			break
		}
	}
	if got == "" {
		return fmt.Errorf("name-assert %s@%s: pct config reported no hostname — refusing to act blind", vmid, node)
	}
	if got != wantName {
		return fmt.Errorf("SAFETY ABORT: vmid %s on %s is %q, not %q — the pool entry is stale", vmid, node, got, wantName)
	}
	return nil
}

// restoreUnitName builds a UNIQUE, self-collecting systemd unit name for a deferred restore.
//
// THE BUG THIS FIXES: the bash engine used a FIXED name per vmid for the disk arm
// (--unit=tg-diskclean-$vmid) with no --collect. A fixed name collides with a lingering unit from a previous
// injection and the arm SILENTLY FAILS; without --collect the unit lingers after firing and guarantees the
// next collision. The device arm had already been fixed this way; the disk arm had not, which is half of the
// docuseal01 stranding. Every class now gets the same treatment.
//
// The seconds-resolution timestamp is sufficient because the planner refuses to fault the same guest twice
// while a restore is outstanding, so two injections for one vmid cannot occur within the same second.
func restoreUnitName(class Class, vmid string, at time.Time) string {
	return fmt.Sprintf("tg-restore-%s-%s-%d", strings.ReplaceAll(string(class), "-", ""), vmid, at.Unix())
}

// ArmDeferredRestore schedules the undo ON THE HOST THAT SURVIVES the fault.
//
// Placement matters and is the other half of the docuseal01 stranding: a disk-fill's cleanup was armed INSIDE
// the guest, so stopping that guest destroyed the pending timer. A device-down restore is armed on the
// Proxmox NODE (which stays up); a disk-fill restore is armed inside the guest (which also stays up for a
// fill) — but neither is now the ONLY record, because the ledger holds the obligation and the reconciler
// repairs anything the timer misses. The timer is an optimisation; the ledger is the guarantee.
func ArmDeferredRestore(ctx context.Context, run Runner, armHost string, class Class, vmid string, after time.Duration, undo []string) error {
	unit := restoreUnitName(class, vmid, time.Now().UTC())
	// Clear any lingering failed unit from an older fixed-name arm before scheduling.
	_, _, _ = run.Run(ctx, armHost, []string{"systemctl", "reset-failed", unit + ".service"})

	argv := []string{
		"systemd-run", "--collect",
		"--on-active=" + strconv.Itoa(int(after.Minutes())) + "min",
		"--unit=" + unit,
	}
	argv = append(argv, undo...)

	_, code, err := run.Run(ctx, armHost, argv)
	if err != nil {
		return fmt.Errorf("arm restore %s on %s: %w", unit, armHost, err)
	}
	if code != 0 {
		return fmt.Errorf("arm restore %s on %s: systemd-run exited %d", unit, armHost, code)
	}
	return nil
}

// UndoArgv returns the idempotent repair for a class. Idempotence is load-bearing: the reconciler retries
// failed repairs, and a repair that errors on "already fine" would flap forever.
func UndoArgv(o Outstanding) ([]string, string, error) {
	switch o.Class {
	case ClassDeviceDown:
		// `pct start` on an already-running guest exits non-zero but is harmless; the caller verifies by
		// reading status rather than trusting the exit code.
		return []string{"pct", "start", o.FaultRef}, o.Node, nil
	case ClassDiskFill:
		if o.FaultRef == "" {
			return nil, "", fmt.Errorf("disk-fill %d has no fault_ref — cannot know what to remove", o.ID)
		}
		// rm -f is idempotent on an absent file.
		return []string{"rm", "-f", "--", o.FaultRef}, o.Host, nil
	case ClassLogFill:
		if o.FaultRef == "" {
			return nil, "", fmt.Errorf("log-fill %d has no fault_ref — cannot know what to truncate", o.ID)
		}
		// TRUNCATE, never rm: the file is the application's real log, so the undo must restore its SIZE
		// without destroying the inode the running service holds open (an unlinked log leaves the writer
		// appending to a file nobody can read, which would be a worse estate than the fault). `truncate -s 0`
		// is idempotent on an already-empty file and a no-op if the service has since rotated it.
		//
		// The path is re-validated even here, at the undo. FaultRef is read back from the LEDGER, so a row
		// written by an older binary — or edited — must not be able to make the repair path truncate an
		// evidence store. A repair that cannot be proven safe is refused, which quarantines the host rather
		// than silently doing damage in the name of cleanup.
		if err := ValidLogPath(o.FaultRef); err != nil {
			return nil, "", fmt.Errorf("log-fill %d has an unsafe fault_ref %q: %w", o.ID, o.FaultRef, err)
		}
		return []string{"truncate", "-s", "0", "--", o.FaultRef}, o.Host, nil
	case ClassContainerDown:
		if o.FaultRef == "" {
			return nil, "", fmt.Errorf("container-down %d has no fault_ref — cannot know what to start", o.ID)
		}
		// `docker start` on an already-running container exits 0 and is a no-op; the caller verifies by reading
		// status rather than trusting the exit code. Runs on the GUEST, which stayed up throughout.
		return []string{"docker", "start", o.FaultRef}, o.Host, nil
	case ClassServiceDown:
		if o.FaultRef == "" {
			return nil, "", fmt.Errorf("service-down %d has no fault_ref — cannot know what to start", o.ID)
		}
		// `systemctl start` on an already-active unit exits 0 and is a no-op; the caller verifies by reading
		// is-active rather than trusting the exit code. Runs on the GUEST, which stayed up throughout.
		return []string{"systemctl", "start", o.FaultRef}, o.Host, nil
	case ClassMemPressure:
		return nil, "", fmt.Errorf("mem-pressure owes no restore (self-releasing)")
	default:
		return nil, "", fmt.Errorf("unknown class %q", o.Class)
	}
}
