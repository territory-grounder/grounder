package main

// The pack catalog's boot attestation (TG-85 item 6), in its own wire-file per the TG-501 god-file
// ratchet: every compiled platform pack resolved against the LIVE tool registry (core/pack.Resolve —
// the lazy not-installed guard), so the deployed binary itself states which packs govern which domains
// and whether their declared capabilities exist. A declared-but-unregistered tool is NAMED (degraded,
// never silently dropped); a vendor lane with no transport resolver wired fails closed inside Resolve
// and says so. An empty catalog logs nothing — the pre-pack boot log stays byte-identical.

import (
	"fmt"
	"log"
	"strings"

	"github.com/territory-grounder/grounder/agent"
	"github.com/territory-grounder/grounder/core/pack"
)

func logPackBootAttestation(tools *agent.ToolSet) {
	compiled, err := pack.All()
	if err != nil {
		// An invalid compiled catalog is a programming bug the pack tests catch pre-merge; at boot the
		// honest statement is what pack.For will DO with it: swallow the error and govern nothing.
		log.Printf("pack catalog INVALID: %v — no pack will govern any session", err)
	}
	for _, p := range compiled {
		av := pack.Resolve(p, func(n string) bool { _, ok := tools.Get(n); return ok }, nil)
		status := "all declared capabilities resolve"
		if len(av.ToolsMissing) > 0 || !av.TransportOK {
			status = fmt.Sprintf("DEGRADED (tools missing: %s; transport: %s)", strings.Join(av.ToolsMissing, ","), av.Reason)
		}
		log.Printf("pack %s: domains=%s tier-hint=%q band-floor=%v — %s", p.LedgerToken(), strings.Join(p.Domains, ","), p.TierHint, p.Band.Applies, status)
	}
}
