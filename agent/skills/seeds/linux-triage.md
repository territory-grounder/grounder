## Linux guest triage (this estate's guests run Debian and Ubuntu — one systemd family; the few divergences that change a diagnosis are named below)

This skill sharpens WHICH fault and the SHAPE of the fix on a Linux guest's OS-plane incident. It does
NOT decide whether an action runs and it does NOT lower the risk band — the band is machine-enforced and
every proposal still goes to the gate at the band the classifier assigns. Nothing below is a licence to
auto-act; treat every proposal as poll-by-default and let the machinery decide.

The fault lives in ONE of three planes — NAME the plane before proposing anything:

UNIT plane (a named service is down/failed/flapping). Read the unit's STATE WORD before anything else —
each word is a different fault: `failed` = it ran and died (the exit code and the last unit log lines say
why); `inactive` but enabled = it stopped and nothing restarts it (no `Restart=` — a start is meaningful);
`masked` = an operator deliberately disabled it — proposing a start OVERRIDES a human decision, never do
it; `activating`/`auto-restart` loops, or systemd's restart rate-limit has tripped = it is already being restarted and LOSING —
another restart is a no-op that resets the counter and hides the evidence; diagnose the underlying cause
instead. Discriminate the LOG PLANES: the unit's own log (journal for that unit) explains a crash; the
kernel log explains a kill. The fix shape for a cleanly-dead unit is the smallest reversible verb on THAT
unit — start if stopped, restart if wedged — never a chain of units "to be safe".

PRESSURE plane (memory/disk/CPU threshold alerts). Attribute before proposing: WHICH process/cgroup holds
the resource — a box-wide number with no owner is not yet a diagnosis. Memory: an OOM kill names its victim
in the log — kernel OOM-killer lines ("Out of memory: Killed process") or systemd-oomd's own kill lines,
whichever the evidence shows; a service that reads UP because something keeps re-killing and restarting it
is a leak wearing a healthy status. Restarting a leaking unit is a band-aid that buys hours — say so in the
proposal and name the leaking unit. Disk: read the DEVICE beside the number — snap/squashfs mounts show as
100%-full loop devices and that is their NORMAL state, not disk pressure; and when df and the visible files
disagree, a process usually holds a DELETED-but-open file (commonly a rotated log) — the fix shape is
restarting the HOLDER, not deleting more files. CPU: sustained saturation with no runaway process is
capacity, not a fault — nothing here is restartable into health.

What the platform label cannot tell you: no tool here reliably names the distro — device status renders
whatever the monitoring system calls the OS, and the same guest can read as its hypervisor's platform, as
bare "linux", or as a distro name depending on how it was discovered. Treat that word as provenance, not
identity: the facts a verdict turns on — a loopback-backed rootfs, a unit's state word, an OOM line — live
in the observed mount/unit/log evidence, never in an OS label or a distro guess.

HOST/OS plane (the machinery of the OS itself). Update tooling (unattended-upgrades, apt timers) briefly
takes locks and spikes CPU/memory on BOTH distros — a threshold alert that coincides with the update
window and self-clears is that, not a service fault. Kernel messages, filesystem read-only remounts, OOM
storms killing MANY different processes: host-plane faults — no unit-level verb fixes them; diagnose,
say what the evidence shows, and hand off.

DEFAULT POSTURE is read-only diagnosis + human approval. The reversible lane on this platform is exactly
the unit verbs the operator allowlisted — start/restart/reload of ONE named unit whose down-state the
evidence establishes.

NEVER auto (human poll, always): rebooting the guest; killing processes by pid; any package operation
(install/upgrade/autoremove); anything touching a unit the operator did not allowlist; a second restart of
a unit that is already flapping or held by systemd's restart rate-limit.
