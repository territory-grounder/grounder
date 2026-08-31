package suppression

import (
	"path"
	"regexp"
	"strings"
)

// defaultRebootClassRE matches an alert rule that names a host reboot/restart/down condition — the class
// the scheduled-reboot lane applies to. Deliberately narrow so a NON-reboot alert on a host under a reboot
// schedule (a real incident during the window) is never swept up by that schedule.
var defaultRebootClassRE = regexp.MustCompile(`(?i)reboot|restart|host.?down|node.?down|unreachable|power.?cycle`)

// RebootRules is the reboot-class ALLOWLIST as data (config-not-code), porting the predecessor's operator-
// tunable REBOOT_RULE_PATTERNS. Estates name their reboot alerts differently ("Device rebooted",
// "sysUpTime reset"), and a rule the allowlist does not cover simply never reaches phase SR — so the list
// belongs in deployment config, not in a compiled regex an operator cannot reach.
//
// Empty Patterns ⇒ the compiled default set (unchanged behavior). Non-empty Patterns REPLACE the default,
// exactly like the predecessor's config key. Patterns are shell-style globs matched case-insensitively
// against the whole rule name (`*reboot*`, `*device rebooted*`). A MALFORMED pattern matches nothing: the
// failure direction is "not reboot-class" ⇒ no schedule suppression ⇒ investigate.
type RebootRules struct {
	Patterns []string
}

// IsReboot reports whether an alert rule names a reboot-class condition.
func (r RebootRules) IsReboot(alertRule string) bool {
	if alertRule == "" {
		return false
	}
	if len(r.Patterns) == 0 {
		return defaultRebootClassRE.MatchString(alertRule)
	}
	rule := strings.ToLower(alertRule)
	for _, p := range r.Patterns {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" {
			continue
		}
		if ok, err := path.Match(p, rule); err == nil && ok {
			return true
		}
	}
	return false
}
