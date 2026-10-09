// Package faultinjector holds the DECISION LOGIC of the estate fault-injection engine: given the ledger's
// durable restore obligations, a live cluster snapshot and the operator's throttles, decide (a) which guest
// may be faulted next and with what class, and (b) which outstanding faults must be repaired now.
//
// Everything here is a PURE function of explicit inputs. That is deliberate: this component deliberately
// breaks a production estate, so the part that decides WHAT to break must be exhaustively testable without
// touching a machine. The SSH/Proxmox effects live in the command wrapper and are a thin translation of the
// decisions made here.
//
// PROVENANCE — this replaces an untracked bash engine that stranded two live guests at 97% root disk on
// 2026-07-26, ~80 minutes past their restore deadline. Both strandings had the same root cause: the restore
// obligation was held in volatile state (an in-process bash map, and a transient systemd timer armed INSIDE
// the guest being broken). Two invariants here exist specifically to make that class of failure
// unrepresentable, and each is pinned by a regression test named for the path it closes:
//
//	INVARIANT 1 (closes PATH B — "memory resets on restart"): busy-ness is derived ONLY from the durable
//	ledger, never from process memory. A freshly-started planner with no history must reach the same
//	conclusions as one that has been running for hours.
//
//	INVARIANT 2 (closes PATH A — "timer dies with its guest"): a guest that owes a restore is NEVER selected
//	for another fault, of any class. The original engine device-downed a guest that had a pending in-guest
//	disk-clean timer; stopping the guest destroyed the timer and the fill was never cleaned.
package faultinjector

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Class is a fault kind the engine can inject. The zero value is deliberately invalid so an unset class can
// never be mistaken for a real one.
type Class string

const (
	// ClassDeviceDown stops a guest. It owes a restore (`pct start` on the owning node) and it is the only
	// class that exercises the full detect→propose→heal loop, since start-guest is the graduated op-class.
	ClassDeviceDown Class = "device-down"
	// ClassDiskFill allocates a bounded file to push root usage into the alerting band. It owes a restore
	// (remove the file). This is the class that stranded two guests.
	ClassDiskFill Class = "disk-fill"
	// ClassMemPressure holds anonymous RSS with a built-in self-release timeout, so it owes NO restore — the
	// obligation is inside the injected process itself and dies with it. It still marks the host busy for the
	// hold window so the engine does not stack faults on one guest.
	ClassMemPressure Class = "mem-pressure"
	// ClassContainerDown stops ONE operator-declared docker container inside a guest, leaving the guest itself
	// up. It is the fault that exercises the `restart-container` op-class, which no other class reaches: a
	// device-down stops the whole guest (healed by start-guest) and a disk-fill never touches a service. It
	// owes a restore (`docker start <container>` on the GUEST, which stays up throughout — so unlike a
	// device-down there is no node-vs-guest placement hazard).
	ClassContainerDown Class = "container-down"
	// ClassServiceDown stops ONE operator-declared systemd unit inside a guest, leaving the guest itself up.
	// It is the ONLY class that reaches `restart-service` and `start-service` — the two op-classes that sat
	// frozen at 1/5 clean runs because nothing in the estate ever produced the condition they answer. Measured
	// when this shipped: 5 `service-down` rows in `injected_fault`, all hand-injected by a script, none by this
	// engine; device-down 264, disk-fill 144, container-down 49.
	//
	// It is container-down's systemd twin and shares its safety shape: the guest stays UP throughout, so the
	// undo is the plain inverse on the same host with no node-vs-guest placement hazard, and the unit is never
	// chosen here — an undeclared unit makes the guest ineligible rather than making the injector guess.
	ClassServiceDown Class = "service-down"
	// ClassLogFill grows an OPERATOR-DECLARED log file until root usage enters the alerting band — the
	// runaway-log shape a real estate actually produces, and the ONLY disk-pressure fault an honest reclaim
	// verb can remediate.
	//
	// WHY IT EXISTS SEPARATELY FROM disk-fill (read this before merging the two): disk-fill's artifact is a
	// `fallocate`d image at a path nothing but this harness owns. That is BENCHMARK INSTRUMENTATION, and TG
	// must never learn to delete it — teaching an agent to remove the injector's own file is a direct route
	// to deleting arbitrary operator files, which is why disk-fill is declared DETECTION-ONLY and provokes
	// nothing. log-fill inverts exactly that property: the target is a declared application log, the honest
	// remedy is to truncate or rotate it, and that remedy is a legitimate operator action rather than
	// tampering with the measurement.
	//
	// It owes a restore (truncate the file back to empty), which runs on the GUEST — the guest stays up
	// throughout, so this shares container-down's safety shape and not device-down's node-placement hazard.
	// The path is NEVER chosen here: an undeclared LogPath makes the guest ineligible, exactly as an
	// undeclared Unit does for service-down. The declaration is also the safety boundary — see PoolGuest.LogPath
	// for why journald paths are refused.
	ClassLogFill Class = "log-fill"
)

// OwesRestore reports whether a class leaves an obligation the ledger must track. A class that owes nothing
// is still throttled, but a crash cannot strand it.
// AllClasses is the CLOSED enumeration of fault classes — the single source of truth for exhaustiveness.
// It exists because a class can be fully implemented (planner, effect, restore, verify, tests) and still be
// unschedulable if one accept-list somewhere does not know its name: container-down shipped exactly that way
// and crash-looped the injector on `unknown class`. A class added here and nowhere else must fail a test, not
// silently become unreachable.
func AllClasses() []Class {
	return []Class{ClassDeviceDown, ClassDiskFill, ClassMemPressure, ClassContainerDown, ClassServiceDown, ClassLogFill}
}

// Provokes declares which OP-CLASSES this fault class is meant to drive TG toward. It is the machine-readable
// form of what has only ever been prose in the comments above, and it exists so `specvalidate opcover --check`
// can prove the other direction: that no actuatable op-class is left with NO fault source.
//
// That direction is the one that silently rots. An op-class with no fault source is fully registered, fully
// tested, renders in the prompt catalog — and can never earn autonomy, because nothing in the estate will ever
// produce the condition it answers. Measured when this shipped: 3 of 6 op-classes (restart-service,
// start-service, reload-service) had no fault source at all, and reload-service had never been proposed once
// in the entire ledger. A5 breadth was therefore capped at 2 op-classes permanently, and nothing said so.
//
// A class provoking NOTHING is legitimate (mem-pressure is a detection/observability fault, not a remediation
// driver) — it simply contributes no coverage.
func (c Class) Provokes() []string {
	switch c {
	case ClassDeviceDown:
		return []string{"start-guest"}
	case ClassContainerDown:
		// BOTH, for the same reason as service-down: `start-container` is the literal inverse of a stopped
		// container and `restart-container` reaches the same end state, and TG has been observed choosing each.
		return []string{"restart-container", "start-container"}
	case ClassServiceDown:
		// BOTH are declared because both genuinely remediate a stopped unit and TG has been observed choosing
		// each: `start-service` is the literal inverse, and `restart-service` reaches the same end state from
		// either a stopped or a wedged unit. Declaring only one would leave the other reporting as an
		// uncovered op-class while a fault class in the rotation was in fact driving it.
		return []string{"start-service", "restart-service"}
	case ClassDiskFill:
		// NOTHING. disk-fill is a DETECTION-ONLY class for TG, and claiming otherwise was a false coverage
		// declaration — the exact failure opcover exists to prevent, made by opcover's own input.
		//
		// The fault is `fallocate` on FillPath (/var/tmp/tg-tier1-fill.img): a SYNTHETIC BENCHMARK ARTIFACT.
		// `disk-grow` does not remediate it — growing a filesystem to accommodate a rogue 7 GB file is the
		// wrong answer even where the disk CAN grow, and on this estate the pool roots are loopback-backed so
		// it cannot. The only correct remediation is removing the injector's own file, and TG must NEVER learn
		// that: it would teach the agent to delete the benchmark's instrumentation, and it is a direct route to
		// deleting arbitrary operator files.
		//
		// Measured 2026-07-28: 74 disk-fill faults, 12 drew a proposal, 1 was healed. TG declining is the ONLY
		// correct behaviour, so the pairing was crediting coverage that could never legitimately be exercised.
		// Same standing as mem-pressure above — a detection/observability fault, not a remediation driver.
		return nil
	case ClassLogFill:
		// NOTHING YET — deliberately, and this is the P3-before-P4 order the plan mandates rather than an
		// oversight. log-fill exists to be the fault a reclaim verb can honestly heal, but no such verb is
		// registered: the owner-authorized narrow reclaim capability is gated behind ParamSpec value bounds,
		// a verdict that can see TARGET-HOST effects (the sole verdict author excludes the target, and a
		// reclaim's only effect is on the target), and journal-evidence protection. Declaring a pairing to a
		// verb that does not exist is precisely the false coverage declaration disk-fill's entry above was
		// corrected for. When the truncate/rotate verb ships, IT claims this pairing and opcover proves the
		// link — until then log-fill contributes detection coverage only.
		return nil
	case ClassMemPressure:
		return nil // a pressure signal, not a remediation driver
	default:
		return nil
	}
}

func (c Class) OwesRestore() bool {
	return c == ClassDeviceDown || c == ClassDiskFill || c == ClassContainerDown || c == ClassServiceDown ||
		c == ClassLogFill
}

// PoolGuest is one member of the SAFE injection pool.
type PoolGuest struct {
	VMID string
	Name string
	Node string // the Proxmox node that owns it — a device-down restore MUST run here
	// Unit is the OPERATOR-DECLARED systemd unit a service-down fault may stop on this guest — the same
	// config-not-code rule as Container: no unit name is compiled in, empty means the guest is simply not
	// eligible for service-down, and `systemctl list-units` is never scraped to pick a victim.
	Unit string
	// Container is the OPERATOR-DECLARED docker container this guest's service runs in, and the only thing a
	// container-down fault may stop. It is config-not-code (the pool file's optional 4th field): no container
	// name is compiled in, because a literal estate identity in a shipped artifact is exactly the class of
	// defect the forbidden-pattern gate exists to catch. Empty ⇒ the guest is simply not eligible for
	// container-down; it is never guessed, and `docker ps` is never scraped to pick a victim.
	Container string
	// LogPath is the OPERATOR-DECLARED application log a log-fill fault may grow on this guest — same
	// config-not-code rule as Unit and Container: never compiled in, never discovered by scanning, and empty
	// simply means the guest is not eligible for log-fill.
	//
	// THE DECLARATION IS THE SAFETY BOUNDARY, and ValidLogPath enforces what may be declared. A log-fill's
	// restore truncates this file, and the future reclaim verb will too, so a path naming the SYSTEM JOURNAL
	// or the actuator guard's audit trail would make the harness — and later TG — destroy the evidence its
	// own safety controls depend on: the tg-actuator-guard ALLOW/DENY log is the last-line control's only
	// record, journald carries the sudo lines the actor-attribution engine reads, and on the control-plane
	// host every TG container logs through the journald driver. That is why the reclaim red-team rejected
	// `journalctl --vacuum-*` outright, and the same reasoning binds the fault that would justify it.
	LogPath string
	// HealthProbe is the OPERATOR-DECLARED command that proves this guest's PRIMARY SERVICE is serving
	// through its DATA PATH after a restore — the 7th positional field, same config-not-code rule as its
	// siblings: never compiled in, never discovered by scanning, "-" or absent means the guest simply has
	// no app-level restore check.
	//
	// TG-226. A device-down restore verified `pct status` said "running" and stopped there. That proves the
	// GUEST came back; it says nothing about whether the applications INSIDE re-established their downstream
	// connections. Found live 2026-07-31 on a Node app whose Mongoose pool came back wedged after a hard
	// stop: MongoDB was up, `mongo ping` answered, static endpoints returned 200, and every DB-backed
	// request buffered to a 10s timeout for ~5 hours. Every check the harness had — ICMP, device-status,
	// pct status — reported healthy for all five of those hours.
	//
	// The probe must therefore exercise the DATA PATH, not liveness: a request that reads or writes through
	// the app's persistent connection. It runs as fixed argv on the GUEST (strings.Fields, never a shell —
	// AGENTS.md forbids `sh -c`), so it cannot carry pipes, redirects or substitutions; exit 0 is healthy.
	HealthProbe string
}

// ValidHealthProbe reports whether a declared app-health probe is acceptable, and why not when it is not.
//
// TG-226. The probe is the only field here that is a COMMAND rather than a name or a path, so it gets the
// strictest declaration-time gate. The injector runs fixed argv (strings.Fields) and never a shell, which
// already defeats pipes and redirects at execution — but a declaration containing them would still be a
// LIE about what runs: the operator writes `curl -sf localhost/api | grep ok` expecting a pipeline and
// silently gets curl handed the literal arguments "|" and "grep". Refusing here means the declaration and
// the behaviour cannot diverge.
//
// Refusing at declaration time also matches how the other fields fail: the planner treats an invalid
// declaration exactly like an absent one is NOT good enough for this field, because an absent probe means
// "no app-level check" — the very state TG-226 is about. LoadPool therefore makes it FATAL instead.
func ValidHealthProbe(cmd string) error {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return errors.New("empty health probe")
	}
	for _, meta := range []string{"|", ";", "&", ">", "<", "`", "$(", "\n"} {
		if strings.Contains(cmd, meta) {
			return fmt.Errorf("health probe %q contains %q: the injector runs fixed argv and never a shell, "+
				"so this would be passed as a literal argument rather than interpreted — declare a single "+
				"command, or wrap the pipeline in a script on the guest and declare that", cmd, meta)
		}
	}
	if len(strings.Fields(cmd)) == 0 {
		return fmt.Errorf("health probe %q has no command", cmd)
	}
	return nil
}

// ValidLogPath reports whether a declared log-fill target is acceptable, and why not when it is not.
//
// It is deliberately a CLOSED set of refusals rather than an allowlist of shapes: the operator owns their
// estate's log layout, but four properties are non-negotiable — absolute (a relative path resolves against an
// unknown cwd on the remote), no traversal, no shell metacharacters (the injector uses fixed argv, but a path
// that only LOOKS safe here would be pasted into an allowfile line later), and never inside the evidence
// stores named above. Refusing at DECLARATION time means an unsafe path can never reach an estate: the
// planner treats an invalid declaration exactly like an absent one — the guest is ineligible, never guessed at.
func ValidLogPath(p string) error {
	p = strings.TrimSpace(p)
	if p == "" {
		return errors.New("empty log path")
	}
	if !strings.HasPrefix(p, "/") {
		return fmt.Errorf("log path %q is not absolute — a relative path resolves against an unknown remote cwd", p)
	}
	if strings.Contains(p, "..") {
		return fmt.Errorf("log path %q contains a traversal segment", p)
	}
	if strings.ContainsAny(p, " \t\n\"'`$;&|<>*?()[]{}\\") {
		return fmt.Errorf("log path %q contains whitespace or shell metacharacters", p)
	}
	// The evidence stores. These are refused BY PREFIX because the harm is the directory, not one filename:
	// journald's archives, the actuator guard's trail, and the audit/lastlog family.
	for _, forbidden := range forbiddenLogPrefixes {
		if p == strings.TrimSuffix(forbidden, "/") || strings.HasPrefix(p, forbidden) {
			return fmt.Errorf("log path %q is inside %s — that is an evidence store (journald / the actuator-guard trail / the audit log), "+
				"and a fault whose restore truncates it would destroy the record TG's own safety controls depend on", p, forbidden)
		}
	}
	return nil
}

// forbiddenLogPrefixes is the closed set of evidence stores a log-fill target may never live in. Each entry
// exists because something READS it as proof: journald carries the sudo lines the actor-attribution engine
// parses AND (on the control-plane host) every TG container's own logs; the guard trail is the last-line
// actuation control's only record; wtmp/audit are the host's tamper-evidence.
var forbiddenLogPrefixes = []string{
	"/var/log/journal/",
	"/run/log/journal/",
	"/var/log/tg-actuator-guard",
	"/var/log/audit/",
	"/var/log/wtmp",
	"/var/log/btmp",
	"/var/log/lastlog",
}

// Outstanding is a ledger row whose restore is still owed (restore_state in 'pending','failed'). It is the
// ONLY source of busy-ness: see INVARIANT 1.
type Outstanding struct {
	ID           int64
	Host         string
	Class        Class
	Node         string
	FaultRef     string // the handle the undo needs (fill path, vmid, …)
	RestoreDueAt time.Time
	Failed       bool // a previous repair attempt failed; the host stays quarantined
}

// Limits are the operator-set throttles.
type Limits struct {
	MaxDown      int           // max guests concurrently stopped (a stopped guest is a real outage)
	MaxBusy      int           // max guests under ANY fault at once
	Target       int           // stop after this many injections (0 = unbounded)
	RestoreAfter time.Duration // how long a fault is held before its restore falls due
	// SettleWindow is how long a target must be observed RECOVERED before the same class may fault it again.
	// It exists because detection is a monitoring STATE TRANSITION, not a state: if the next fault lands before
	// the check has polled the recovered state, the alert never clears, never re-raises, and the fault is
	// invisible to TG. Measured live 2026-07-28 — two service-down faults 2 minutes apart on one host produced
	// exactly ONE alert, while injected_fault recorded two. Any detection rate computed as
	// detections/injections then scores the second as a MISS THAT WAS NEVER DETECTABLE, which is an instrument
	// artefact being read as a TG failure. Must exceed the monitoring poll interval. Zero disables the guard.
	SettleWindow time.Duration
}

// State is the complete input to a planning decision.
type State struct {
	Now         time.Time
	Pool        []PoolGuest
	Allowlist   map[string]bool   // TG_PROXMOX_ALLOWED_GUESTS — the guests TG itself may actuate
	Status      map[string]string // vmid -> "running"|"stopped", from the live cluster snapshot
	Outstanding []Outstanding
	// Settling is the most recent RESTORE time per (host, class) for faults already discharged. A target whose
	// previous fault of the same class was restored only moments ago must not be re-faulted yet: the monitoring
	// check has not had time to observe the RECOVERED state, so the next fault raises no new alert and is
	// undetectable. See SettleWindow.
	Settling    map[string]time.Time
	BreakerOpen bool
	KillSwitch  bool
	// Injected counts faults that ACTUALLY LANDED. It answers one question — has the campaign reached its
	// target — and it must never answer any other, because it does not advance on a cycle that fails.
	Injected int
	// Cycle counts TICKS, landed or not, and is what drives the class rotation and the pool sweep.
	//
	// ★ THESE TWO WERE THE SAME COUNTER, AND THAT KILLED THE CAMPAIGN FOR 7.5 HOURS (measured live
	// 2026-07-29 02:19Z-09:34Z). `injected` advances only when InjectOnce returns true, so a class that
	// CANNOT act — every candidate ineligible, or the effect leaf refusing before touching anything — left
	// the cursor where it was and the next tick chose the same class again. The rotation froze on log-fill,
	// the only class no pool guest could satisfy, and every other class was starved out: 6 classes
	// configured, 1 selected, 148 consecutive aborted ticks, ingest fell from ~25 alerts/hour to ~1.
	//
	// The failure is silent by construction and it is NOT specific to log-fill: the harness kept running,
	// kept logging per-tick, kept honestly reporting "provably nothing was broken" — and produced no faults
	// at all, so every A1/A3/A4/A5 figure measured in that window was computed over near-zero volume while
	// looking exactly like a healthy campaign.
	//
	// Fixing the log-fill eligibility alone would have made it WORSE: the planner would have found no
	// eligible guest, returned Act=false, left the cursor frozen for the same reason, and gone completely
	// quiet instead of loudly aborting.
	Cycle  int
	Limits Limits
}

// Decision is the planner's output. Act=false always carries a Reason — a silent no-op is indistinguishable
// from a stuck engine, and this thing runs unattended for days.
type Decision struct {
	Act    bool
	Guest  PoolGuest
	Class  Class
	Reason string
}

// busyHosts returns the set of hosts that owe a restore. Derived ONLY from the ledger (INVARIANT 1).
func busyHosts(out []Outstanding) map[string]bool {
	busy := make(map[string]bool, len(out))
	for _, o := range out {
		busy[o.Host] = true
	}
	return busy
}

// PlanNext decides whether to inject, into which guest, and with what class.
//
// It is FAIL-CLOSED at every step: any doubt (kill-switch set, breaker open, snapshot missing or implausibly
// short, host not allowlisted, host owing a restore) yields Act=false with a reason. Refusing to inject is
// always safe; injecting wrongly breaks a production guest.
//
// rotation is the class cycle; the caller supplies it so weighting is configuration, not code.
func PlanNext(st State, rotation []Class) Decision {
	switch {
	case st.KillSwitch:
		return Decision{Reason: "kill-switch engaged"}
	case st.BreakerOpen:
		return Decision{Reason: "TG mutation breaker is OPEN — the estate is already unhappy; not adding load"}
	case st.Limits.Target > 0 && st.Injected >= st.Limits.Target:
		return Decision{Reason: fmt.Sprintf("target reached (%d)", st.Limits.Target)}
	case len(rotation) == 0:
		return Decision{Reason: "no class rotation configured"}
	case len(st.Pool) == 0:
		return Decision{Reason: "empty pool"}
	}

	// Never fault blind. A short/absent snapshot means we cannot tell running from stopped, and stopping an
	// already-stopped guest (or one an operator is mid-maintenance on) is exactly the surprise this engine
	// must never cause. The threshold is relative to the pool: a snapshot that cannot even describe the pool
	// is not a snapshot.
	if len(st.Status) < len(st.Pool) {
		return Decision{Reason: fmt.Sprintf("cluster snapshot too short (%d entries for a %d-guest pool) — refusing to fault blind", len(st.Status), len(st.Pool))}
	}

	busy := busyHosts(st.Outstanding)
	if len(busy) >= st.Limits.MaxBusy {
		return Decision{Reason: fmt.Sprintf("throttle: %d/%d guests already faulted", len(busy), st.Limits.MaxBusy)}
	}

	// Count guests currently stopped, from the LIVE snapshot rather than our own records: a guest an operator
	// stopped by hand still counts against the outage budget.
	down := 0
	for _, g := range st.Pool {
		if st.Status[g.VMID] == "stopped" {
			down++
		}
	}

	// Rotate the starting offset so the pool is swept evenly instead of always hammering the first free guest.
	order := make([]PoolGuest, len(st.Pool))
	copy(order, st.Pool)
	sort.SliceStable(order, func(i, j int) bool { return order[i].VMID < order[j].VMID })
	start := 0
	if n := len(order); n > 0 {
		start = st.Cycle % n
	}

	class := rotation[st.Cycle%len(rotation)]
	// If the outage budget is spent, switch this cycle to a class that does not stop a guest, so disk data
	// keeps accruing instead of the engine idling.
	//
	// The substitute MUST come from the CONFIGURED rotation. An earlier version picked from a hardcoded pair
	// and could emit mem-pressure even when the operator had deliberately excluded it — observed live on
	// 2026-07-26 (id=68), where the operator had excluded mem-pressure precisely because its detection rate is
	// 1/14 on this estate. The fail-closed injector caught it ("no injector wired") so nothing was broken, but
	// the cycle was wasted and, had the class been wired, the engine would have injected a fault its operator
	// had switched off. A planner must never widen its own mandate.
	if class == ClassDeviceDown && down >= st.Limits.MaxDown {
		sub, ok := substituteClass(rotation, st.Cycle)
		if !ok {
			return Decision{Reason: fmt.Sprintf("outage budget spent (%d/%d down) and the rotation declares no non-stopping class", down, st.Limits.MaxDown)}
		}
		class = sub
	}

	for i := range order {
		g := order[(start+i)%len(order)]
		switch {
		case !st.Allowlist[g.Name]:
			// A guest TG is not permitted to actuate on can never be healed by TG, so faulting it produces a
			// guaranteed detection/heal MISS that silently pollutes the A1/A3 denominators. Skip it and let
			// the boot-time pool assertion surface the mismatch to an operator.
			continue
		case busy[g.Name]:
			// INVARIANT 2 — the host owes a restore. Faulting it again is how a pending in-guest cleanup gets
			// destroyed (PATH A). Never stack faults.
			continue
		case st.Status[g.VMID] != "running":
			continue
		case settling(st, g.Name, class):
			// The previous fault of THIS class on THIS host was restored too recently for the monitoring check
			// to have observed the recovery. Faulting again now produces no state transition, so no alert, so
			// no detection opportunity — and the injection would still be counted in the denominator.
			continue
		case class == ClassServiceDown && g.Unit == "":
			// No unit declared for this guest, so this class has nothing it may stop here. Skip rather than
			// guess: scraping the unit list would let the injector stop sshd, the log shipper or the database.
			continue
		case class == ClassContainerDown && g.Container == "":
			// The operator did not declare a container for this guest, so there is nothing this class may stop
			// here. Skip rather than guess: picking a victim by scraping `docker ps` would let the injector stop
			// a database or the log shipper, and would make the fault non-reproducible run to run.
			continue
		case class == ClassLogFill && ValidLogPath(g.LogPath) != nil:
			// No usable log declared for this guest. This arm was MISSING when log-fill shipped, and the live
			// campaign found it within the hour: the planner picked dc1wallos01 (no LogPath), the injector
			// refused at the effect leaf, and the run was recorded as
			//   "injection aborted BEFORE any effect ... empty log path — closing the obligation
			//    (provably nothing was broken)".
			// The SAFETY property held — nothing was touched and the obligation closed honestly — but the
			// planner had already spent that cycle's injection slot, so the class would waste roughly one turn
			// in four on this estate (3 of 12 guests declare a path) and its aborted rows would sit in the
			// benchmark population looking like faults.
			//
			// It validates rather than merely checking for "", so a guest whose declared path is malformed or
			// points into an evidence store is skipped HERE, before it can consume a slot — the same answer
			// the leaf gives, one step earlier.
			continue
		}
		return Decision{Act: true, Guest: g, Class: class}
	}
	return Decision{Reason: "no eligible guest (all allowlisted guests are busy, stopped, or absent from the snapshot)"}
}

// RestoreAction is one repair the reconciler wants performed.
type RestoreAction struct {
	Fault   Outstanding
	Overdue time.Duration // how late the repair is; >0 means the estate has been degraded longer than intended
}

// Reconcile returns every outstanding fault whose restore is due or overdue, soonest-due first.
//
// This is what makes a crash survivable (INVARIANT 1): a brand-new process reads the ledger and repairs
// whatever the dead one left behind. It is called on boot AND on every loop tick, so a restore missed
// because a host was briefly unreachable is retried rather than forgotten.
//
// A 'failed' row is returned again — repair is idempotent by construction (removing an absent file, starting
// a running guest) so retrying is safe, and giving up silently is how a guest stays at 97% disk.
func Reconcile(now time.Time, out []Outstanding) []RestoreAction {
	var due []RestoreAction
	for _, o := range out {
		if !o.Class.OwesRestore() {
			continue
		}
		if now.Before(o.RestoreDueAt) {
			continue
		}
		due = append(due, RestoreAction{Fault: o, Overdue: now.Sub(o.RestoreDueAt)})
	}
	sort.SliceStable(due, func(i, j int) bool { return due[i].Fault.RestoreDueAt.Before(due[j].Fault.RestoreDueAt) })
	return due
}

// PoolMismatch reports guests that are in the injection pool but NOT in TG's actuation allowlist, and vice
// versa. Asserted at boot.
//
// A pool guest missing from the allowlist is the more damaging direction: TG structurally cannot heal it, so
// every fault injected there is an automatic A1/A3 miss that looks like a TG failure but is an instrumentation
// artifact. This was live on 2026-07-26 — searxng01 sat in the pool and not in the allowlist, and three
// injections were spent on it.
func PoolMismatch(pool []PoolGuest, allowlist map[string]bool) (notAllowlisted, notDrilled []string) {
	inPool := make(map[string]bool, len(pool))
	for _, g := range pool {
		inPool[g.Name] = true
		if !allowlist[g.Name] {
			notAllowlisted = append(notAllowlisted, g.Name)
		}
	}
	for name := range allowlist {
		if !inPool[name] {
			notDrilled = append(notDrilled, name)
		}
	}
	sort.Strings(notAllowlisted)
	sort.Strings(notDrilled)
	return notAllowlisted, notDrilled
}

// substituteClass picks a non-stopping class from the CONFIGURED rotation, used when the outage budget is
// spent. It never invents a class the operator did not ask for: excluding a class is a deliberate act (a
// class whose detection is broken must not pollute the benchmark), and the planner has no standing to
// override it. Returns ok=false when the rotation is entirely device-down, so the caller refuses with a
// reason rather than silently widening its mandate.
func substituteClass(rotation []Class, injected int) (Class, bool) {
	var candidates []Class
	for _, c := range rotation {
		if c != ClassDeviceDown {
			candidates = append(candidates, c)
		}
	}
	if len(candidates) == 0 {
		return "", false
	}
	return candidates[injected%len(candidates)], true
}

// settleKey identifies the (host, class) pair whose recovery must be observed before re-faulting.
func settleKey(host string, c Class) string { return host + "\x00" + string(c) }

// settling reports whether this (host, class) was restored too recently to be faulted again — i.e. the
// monitoring check has not yet had a chance to see the target RECOVERED, so a new fault would raise no alert.
//
// This is the same idea as INVARIANT 2 (never stack faults) carried past the restore: INVARIANT 2 protects the
// ESTATE from overlapping faults, and this protects the MEASUREMENT from faults nothing can detect.
func settling(st State, host string, c Class) bool {
	if st.Limits.SettleWindow <= 0 {
		return false // guard disabled
	}
	at, ok := st.Settling[settleKey(host, c)]
	if !ok || at.IsZero() {
		return false // never faulted with this class, or no restore recorded — nothing to wait for
	}
	return st.Now.Sub(at) < st.Limits.SettleWindow
}
