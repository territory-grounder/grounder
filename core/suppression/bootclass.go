package suppression

import "regexp"

// Boot-reason classification for the scheduled-reboot lane (REQ-406, port-fidelity P1-12). A suppressed
// scheduled reboot is only CONFIRMED when the host came back on a genuinely CLEAN boot; a REACTIVE boot
// (an OOM kill, kernel panic, watchdog, hung task, emergency/self-heal, thermal event) is a symptom, never
// a schedule — it must reopen the incident, and it must never be learned as a scheduled reboot. An UNKNOWN
// boot reason is treated as not-clean (fail-safe: do not confirm a suppression on an unclear boot). Ported
// from the predecessor's classify-reboot-alert.py CLEAN_RE / REACTIVE_RE.
var (
	cleanBootRE    = regexp.MustCompile(`(?i)reached target reboot\.target|systemd-reboot|systemd-shutdown|syncing filesystems`)
	reactiveBootRE = regexp.MustCompile(`(?i)oom.?kill|out of memory|invoked oom|kernel panic|watchdog|hung_task|emergency|self.?heal|nvml|thermal`)
)

// IsReactiveBoot reports whether a boot reason indicates a REACTIVE (crash/kill/watchdog) reboot — a symptom
// that must never be suppressed or learned as a schedule.
func IsReactiveBoot(reason string) bool { return reactiveBootRE.MatchString(reason) }

// IsCleanBoot reports whether a boot reason is a genuinely clean, orderly reboot — the only kind that
// confirms a suppressed scheduled reboot. A reactive reason is never clean even if it also matches a clean
// marker, and an unknown reason is not clean.
func IsCleanBoot(reason string) bool {
	return !IsReactiveBoot(reason) && cleanBootRE.MatchString(reason)
}
