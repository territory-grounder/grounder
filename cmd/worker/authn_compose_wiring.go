package main

// wireAuthnCompose arms the interceptor's 4d2 authn-compose gate (spec/016 REQ-1604, T-016-5) — OUT of
// main() per the TG-501 ratchet. SHIPS DARK: with TG_AUTHN_COMPOSE unset/empty the interceptor is
// returned untouched and the gate's own trail row states the control is unarmed. Armed ("1"/"true"),
// every policy-authorized target's identity must resolve through the audited resolver BEFORE anything
// executes — so arming with an incomplete credential rule table deliberately refuses the uncovered
// targets rather than pretending; that is the operator's arming decision, exactly like the other
// flip-gated controls. Either way the boot log says which posture this process runs.

import (
	"log"
	"strings"

	"github.com/territory-grounder/grounder/core/actuate"
	"github.com/territory-grounder/grounder/core/credential"
)

func wireAuthnCompose(ic *actuate.Interceptor, resolver *credential.AuditedResolver, getenv func(string, string) string) *actuate.Interceptor {
	if ic == nil {
		return nil
	}
	switch strings.ToLower(strings.TrimSpace(getenv("TG_AUTHN_COMPOSE", ""))) {
	case "1", "true":
	default:
		log.Print("authn compose: DARK (TG_AUTHN_COMPOSE unset) — gate 4d2 passes with its unarmed row; identity remains the effect leaf's static configuration")
		return ic
	}
	comp := credential.NewComposer(resolver)
	log.Print("authn compose: ARMED (spec/016 REQ-1604) — every policy-authorized target's identity resolves through the audited resolver before anything executes; a target with no declared identity now refuses at gate 4d2")
	return ic.WithComposer(comp.Compose)
}
