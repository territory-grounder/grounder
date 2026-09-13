package faultinjector

import (
	"fmt"
	"os"
	"strings"
)

// Config parsers for the injection pool, the TG actuation allowlist, and the class rotation. They were the
// faultinjector CLI's unexported helpers; TG-180 exports them (unchanged) so the worker's observation-probe
// loop reuses the SAME parsing — a guinea-pig pool the injector and the probe disagree on, or a class rotation
// only one of them validates, is exactly the silent divergence a single source of truth removes.

// LoadPool reads "<vmid> <name> <node> [container] [unit] [logpath] [healthprobe]" lines. A malformed line
// is fatal rather than skipped: silently dropping a pool entry changes the campaign's coverage without
// anyone noticing.
func LoadPool(path string) ([]PoolGuest, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("-pool-file is required")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []PoolGuest
	for i, line := range strings.Split(string(raw), "\n") { //nolint:gocritic // index needed for error messages
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		// The probe ABSORBS THE REST OF THE LINE, so it is the only field that may contain spaces — a real
		// probe is `curl -sf http://127.0.0.1:3000/api/chats`, which no single whitespace-delimited token
		// can hold. That removes the upper bound on field count: fields 4-6 are single tokens and "-" is the
		// explicit empty marker, so everything from the 7th onward is unambiguously the command.
		if len(f) < 3 {
			return nil, fmt.Errorf("line %d: want '<vmid> <name> <node> [container] [unit] [logpath] [healthprobe...]', got %q", i+1, line)
		}
		g := PoolGuest{VMID: f[0], Name: f[1], Node: f[2]}
		// OPTIONAL 4th field: the docker container a container-down fault may stop on this guest. Operator-
		// declared (config-not-code — no container name is compiled in), and absent means the guest is simply
		// not eligible for that class. Three fields remain valid, so existing pool files keep working.
		if len(f) >= 4 {
			g.Container = f[3]
		}
		// OPTIONAL 5th field: the systemd unit a service-down fault may stop on this guest. Same rule as the
		// 4th — operator-declared, never compiled in, absent means the guest is not eligible for that class.
		// Four- and three-field lines remain valid, so existing pool files keep working unchanged.
		if len(f) >= 5 {
			g.Unit = f[4]
		}
		// OPTIONAL 6th field: the application log a log-fill fault may grow (and its restore truncate). Same
		// operator-declared rule; absent ⇒ the guest is not eligible for log-fill. VALIDATED HERE, at the
		// only place a path enters the system, so an evidence store (journald, the actuator-guard trail, the
		// audit log) can never become a truncate target on any estate — see ValidLogPath.
		if len(f) >= 6 {
			g.LogPath = f[5]
		}
		// OPTIONAL 7th field: the command that proves this guest's primary service is serving through its
		// DATA PATH after a restore (TG-226). Same operator-declared rule; absent ⇒ a device-down restore is
		// verified at guest level only, which the engine says out loud rather than passing off as a full
		// check. VALIDATED HERE, at the only place a probe enters the system.
		//
		// The probe is a single command, not a shell line: a whitespace-separated declaration is all the
		// positional format can carry, and ValidHealthProbe refuses metacharacters so an operator who wants
		// a pipeline is told to put it in a script on the guest rather than silently getting literal args.
		if len(f) >= 7 {
			g.HealthProbe = strings.Join(f[6:], " ")
		}
		// The optional fields are POSITIONAL, so a guest with a unit but no container would be inexpressible —
		// and the natural workaround (put the unit in the 4th slot) silently declares it as a CONTAINER, which
		// means `docker stop nginx` on a host running no such container. "-" is the explicit empty marker so
		// that case can be written down instead of worked around.
		if g.Container == "-" {
			g.Container = ""
		}
		if g.Unit == "-" {
			g.Unit = ""
		}
		if g.LogPath == "-" {
			g.LogPath = ""
		}
		if g.HealthProbe == "-" {
			g.HealthProbe = ""
		}
		if g.LogPath != "" {
			if err := ValidLogPath(g.LogPath); err != nil {
				return nil, fmt.Errorf("line %d: %w", i+1, err)
			}
		}
		// FATAL, not "treated as absent". An absent probe is a legitimate declaration meaning "no app-level
		// check"; a MALFORMED one is an operator who believes they declared a check and did not, which is
		// precisely the false-assurance TG-226 exists to remove.
		if g.HealthProbe != "" {
			if err := ValidHealthProbe(g.HealthProbe); err != nil {
				return nil, fmt.Errorf("line %d: %w", i+1, err)
			}
		}
		out = append(out, g)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no guests in %s", path)
	}
	return out, nil
}

// ParseAllowlist turns a comma-separated guest list (TG_PROXMOX_ALLOWED_GUESTS) into a set. Blank entries are
// dropped so trailing commas and surrounding whitespace do not manufacture an empty allowlisted name.
func ParseAllowlist(csv string) map[string]bool {
	m := map[string]bool{}
	for s := range strings.SplitSeq(csv, ",") {
		if s = strings.TrimSpace(s); s != "" {
			m[s] = true
		}
	}
	return m
}

// ParseClasses validates the rotation against the known classes. An unknown class is fatal — a typo would
// otherwise silently produce a rotation that injects nothing.
func ParseClasses(csv string) ([]Class, error) {
	var out []Class
	for s := range strings.SplitSeq(csv, ",") {
		switch c := Class(strings.TrimSpace(s)); c {
		case ClassDeviceDown, ClassDiskFill, ClassContainerDown,
			ClassServiceDown, ClassLogFill:
			out = append(out, c)
		case ClassMemPressure:
			// Deliberately not wired: mem-pressure detection is measured at 1/14 on this estate, so every
			// injection would land in the A1 denominator as a miss that is an instrumentation gap, not a TG
			// failure. Roadmap P0-7 either fixes the LibreNMS rule scope or drops the class; until then it
			// must not pollute the benchmark.
			return nil, fmt.Errorf("mem-pressure is not wired: its detection rate is 1/14 on this estate, so injecting it manufactures A1 misses (roadmap P0-7)")
		case "":
			continue
		default:
			return nil, fmt.Errorf("unknown class %q", c)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("empty rotation")
	}
	return out, nil
}
