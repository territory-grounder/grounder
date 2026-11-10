package main

import (
	"log"
	"strings"
	"time"

	"github.com/territory-grounder/grounder/core/guestgroup"
)

// exclusiveGroupsEnv declares the exclusive guest groups (TG-654, core/guestgroup): "default>partner|partner",
// groups separated by ';'. It is read by BOTH planes (the triage plane's liveness detector and the actuation
// plane's start-guest leaf), so it is on neither plane-withheld list: it is structured inventory, not a
// credential and not attacker-authored content. Guest names live only in the deployment's environment.
const exclusiveGroupsEnv = "TG_PVE_EXCLUSIVE_GROUPS"

// exclusiveGroupGrace is how long the detector waits, with a group's default stopped and no partner running,
// before raising it. An operator swap is stop-then-start (20 s measured on 2026-11-07), so the window only has
// to cover the gap between the two calls plus a liveness poll or two.
const exclusiveGroupGrace = 5 * time.Minute

// exclusiveGuestGroups reads and parses the declaration once per consumer and boot-logs what it armed. A value
// that does not parse is logged loudly and treated as NO declaration (today's behaviour), never a crash: a
// typo must not take the worker down, and an un-armed policy is no less safe than the code before it.
func exclusiveGuestGroups(get func(k, def string) string, who string) []guestgroup.Group {
	raw := strings.TrimSpace(get(exclusiveGroupsEnv, ""))
	if raw == "" {
		return nil
	}
	groups, err := guestgroup.Parse(raw)
	if err != nil {
		log.Printf("%s: exclusive guest groups DISABLED — %s does not parse (%v); behaving as if none were declared (TG-654)", who, exclusiveGroupsEnv, err)
		return nil
	}
	names := make([]string, 0, len(groups))
	for _, g := range groups {
		names = append(names, g.String())
	}
	log.Printf("%s: exclusive guest groups ARMED (TG-654, %s): %s — a member's stop is the declared swap, not a fault; a partner is never started, the default never while a partner runs",
		who, exclusiveGroupsEnv, strings.Join(names, "; "))
	return groups
}
