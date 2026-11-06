package main

import (
	"regexp"
	"sort"
	"strings"

	"github.com/territory-grounder/grounder/core/actuate/opschema"
	"github.com/territory-grounder/grounder/modules/actuation/ssh"
)

// PATTERN MODE (TG-626). List mode renders one exact line per (verb, subject) for the subjects a host permits,
// which cannot express the follow-estate scope ("any valid unit on this host, except the never-touch and
// stateful sets"). Pattern mode renders the SHAPES instead, from the same op-class registry list mode reads:
//
//	ALLOW <POSIX ERE matching the whole SSH_ORIGINAL_COMMAND for every registry verb, any valid subject>
//	DENY  <POSIX ERE, matched case-insensitively against the bare subject: ssh.GuardDenyERE()>
//
// The host guard (deploy/actuation-guard/tg-actuator-guard) consults it only when the exact list does not
// match, so a host with no pattern file is byte-identical to before.

// subjectERE is the POSIX twin of the leaf's token shapes (modules/actuation/ssh unitRe / containerRe). Neither
// admits a quote, a space, a glob or a shell metacharacter, which is what makes the guard's argv split exact.
var subjectERE = map[string]string{
	opschema.ParamUnit:      `[A-Za-z0-9][A-Za-z0-9@:._-]{0,127}`,
	opschema.ParamContainer: `[A-Za-z0-9][A-Za-z0-9_.-]{0,127}`,
}

// placeholder renders through spec.Argv for both subject kinds (it satisfies both token shapes).
const placeholder = "TGSUBJECTPLACEHOLDER0"

// RenderPattern returns the two lines of the pattern file.
func RenderPattern() []string {
	seen := map[string]bool{}
	add := func(argv []string, kind string) {
		if len(argv) == 0 {
			return
		}
		quoted := regexp.QuoteMeta(ssh.RemoteCommand(argv))
		seen[strings.ReplaceAll(quoted, placeholder, subjectERE[kind])] = true
	}
	for _, spec := range opschema.Specs() {
		for _, p := range spec.Params {
			if _, ok := subjectERE[p.Name]; !ok {
				continue
			}
			params := map[string]string{p.Name: placeholder}
			rendered := false
			if argv, err := spec.Argv(params); err == nil && len(argv) > 0 {
				add(argv, p.Name)
				rendered = true
			}
			if rb, err := spec.RollbackArgv(params); err == nil && len(rb) > 0 {
				add(rb, p.Name)
				rendered = true
			}
			if rendered && p.Name == opschema.ParamUnit {
				add(ssh.ServiceActiveProbeArgv(placeholder), p.Name) // the rollback-necessity read (TG-464)
			}
		}
	}
	alts := make([]string, 0, len(seen))
	for a := range seen {
		alts = append(alts, a)
	}
	sort.Strings(alts)
	return []string{
		"ALLOW ^(" + strings.Join(alts, "|") + ")$",
		"DENY " + ssh.GuardDenyERE(),
	}
}
