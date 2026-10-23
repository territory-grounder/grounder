package main

import (
	"net/url"
	"path/filepath"
	"sort"
	"strings"

	"github.com/territory-grounder/grounder/core/estate"
	"github.com/territory-grounder/grounder/core/metrics"
	"github.com/territory-grounder/grounder/modules/actorevidence/journal"
)

// TG-394 — the self-dependency PLACEMENT concentration signal, published from the live estate graph.
//
// TG holds no inventory of where its own dependency hosts run, and nothing reported the concentration when 7
// of 26 of them sat on the single hypervisor it was diagnosing, silently degrading retrieval to
// lexical-only for 11h12m. This publishes the risk that is KNOWABLE AT BOOT rather than at the outage: how
// many of TG's own dependency hosts share one hypervisor, per capability.
//
// SLICE 1 covers the JOURNAL-EVIDENCE capability — the SSH hosts TG reads journals from for actor
// attribution — because 5 of the 7 concentrated hosts in the incident were exactly these, and the set is
// already declared in TG's own config (TG_JOURNAL_DEPLOYMENTS) as host globs. Other capabilities (secrets,
// model, tracker, notifier) are additive under the same {capability} label; the metric shape does not change.
//
// NO ESTATE IDENTIFIERS ARE COMPILED IN: the host set is derived from TG's runtime config (the globs) and
// resolved against the live graph. Hostnames only ever appear as the {parent} label VALUE at runtime.
const selfDepCapabilityJournal = "journal-evidence"

// SLICE 2 (TG-394) — TG's OTHER dependency capabilities, each declared in TG's own config as an ENDPOINT URL
// (not a host glob). The URL is resolved to its host and used as an exact glob; a capability whose endpoint is
// a compose-internal service (e.g. http://litellm:4000) or an unset var resolves to no estate guest — honestly
// visible as hosts_resolved < globs_declared, never a guessed placement. Values are the closed capability
// vocabulary, never an estate identifier.
const (
	selfDepCapabilitySecrets  = "secrets"  // TG_OPENBAO_ADDR — the credential substrate
	selfDepCapabilityModel    = "model"    // TG_LITELLM_URL — the reasoning gateway
	selfDepCapabilityTracker  = "tracker"  // TG_YOUTRACK_URL — the incident tracker
	selfDepCapabilityNotifier = "notifier" // TG_MATRIX_HOMESERVER + TG_MATTERMOST_URL — the governance channels
)

// depHostType reports whether an estate entity is a kind of host a dependency glob should match — a guest or
// a physical host, never a site / service / network device (those are not the SSH hosts a journal glob names,
// and matching a same-named non-host would resolve a phantom dependency). TypePVENode is DELIBERATELY excluded:
// a dependency host is a guest/host that RUNS ON a hypervisor, not the hypervisor itself — a glob naming a
// bare PVE node has no runs_on parent of its own to concentrate on, so it is out of scope by construction (all
// declared journal-deployment hosts to date are guests/hosts, never a bare node).
func depHostType(t estate.EntityType) bool {
	switch t {
	case estate.TypeVM, estate.TypeLXC, estate.TypePhysicalHost, estate.TypeHost:
		return true
	}
	return false
}

// journalDepGlobs extracts the host globs TG declares for journal-evidence access, de-duplicated and sorted.
// Empty globs are dropped (journal.ParseAccess already skips rows with no hostglob).
func journalDepGlobs(access []journal.Access) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, a := range access {
		if a.HostGlob == "" || seen[a.HostGlob] {
			continue
		}
		seen[a.HostGlob] = true
		out = append(out, a.HostGlob)
	}
	sort.Strings(out)
	return out
}

// resolveDepHosts expands host globs against the CURRENT estate's host entities to concrete names. A glob
// that matches nothing contributes no host (visible as hosts_resolved < globs_declared), never a guess.
func resolveDepHosts(g *estate.Graph, globs []string) []string {
	if g == nil || len(globs) == 0 {
		return nil
	}
	set := map[string]bool{}
	for _, n := range g.Export().Nodes {
		if !depHostType(n.Type) {
			continue
		}
		for _, glob := range globs {
			if ok, err := filepath.Match(glob, n.Name); err == nil && ok {
				set[n.Name] = true
				break
			}
		}
	}
	out := make([]string, 0, len(set))
	for h := range set {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

// selfDepConcentrationSamples publishes the placement concentration for one capability's dependency hosts.
//
// ALWAYS EMITS THE COVERAGE PAIR WHEN THE GRAPH IS PRESENT (globs_declared + hosts_resolved), so a scrape can
// tell "checked, no concentration" from "exporter gone" and from "declared hosts but none resolved in the
// estate" — the same absent-is-not-zero discipline as tg_estate_edges. The concentration series appears only
// for a parent carrying 2+ of those hosts (the actual single-point-of-failure); its ABSENCE beside a present
// hosts_resolved reads as "no concentration", never as "unmeasured".
func selfDepConcentrationSamples(g *estate.Graph, capability string, globs []string) []metrics.Sample {
	if g == nil {
		return nil
	}
	capLbl := map[string]string{"capability": capability}
	// hosts_resolved is derived from the SAME grouping the concentration uses, not from mere node existence:
	// InfraParentGroups counts only hosts with a FRESH runs_on placement, so a dependency host that exists in
	// the graph but whose placement edge has EXPIRED (an ingest source going quiet — the very failure class
	// TG-394 exists to catch) is not counted as resolved. Counting glob-matched node existence instead would
	// hold hosts_resolved high while the concentration silently lost members — a false coverage claim exactly
	// when it matters. The gap between globs_declared and hosts_resolved is the honest signal that some
	// declared dependency has no live placement (glob matched nothing, or its placement went stale).
	candidates := resolveDepHosts(g, globs)
	groups := g.InfraParentGroups(candidates)
	placed := 0
	for _, grp := range groups {
		placed += len(grp.Hosts)
	}
	out := []metrics.Sample{
		{
			Name: "tg_self_dependency_globs_declared", Kind: metrics.Gauge,
			Help: "Host globs TG declares as its OWN dependencies for this capability (TG-394). The coverage " +
				"numerator: hosts_resolved is how many of them resolve to a LIVE placement in the estate.",
			Value: float64(len(globs)), Labels: capLbl,
		},
		{
			Name: "tg_self_dependency_hosts_resolved", Kind: metrics.Gauge,
			Help: "TG's own dependency hosts for this capability that resolve to a LIVE runs_on placement in " +
				"the estate (TG-394) — a host that exists but whose placement edge has expired is NOT counted, " +
				"so this tracks the same set the concentration is computed over. ALWAYS emitted when the graph " +
				"is present, so an absent series is the exporter being gone rather than 'no concentration'.",
			Value: float64(placed), Labels: capLbl,
		},
	}
	for _, grp := range groups {
		if len(grp.Hosts) < 2 {
			continue
		}
		out = append(out, metrics.Sample{
			Name: "tg_self_dependency_concentration", Kind: metrics.Gauge,
			Help: "TG's own dependency hosts (this capability) that share ONE hypervisor — the single-point-" +
				"of-failure count knowable at boot (TG-394). >= 2 is the standing risk: one silent parent " +
				"failure takes them all at once, as the shared node did in the cascade this closes.",
			Value:  float64(len(grp.Hosts)),
			Labels: map[string]string{"capability": capability, "parent": grp.Parent.Name},
		})
	}
	return out
}

// selfDepHostOfURL extracts the HOST from a configured endpoint URL (or a bare host[:port]) — the dependency
// host to resolve against the estate (TG-394 slice 2). A URL to a compose-internal service (http://litellm:4000)
// yields that service name, which resolves to no estate guest — honestly visible as hosts_resolved <
// globs_declared, never a guessed placement. Empty/unparseable → "".
func selfDepHostOfURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	// A bare host or host:port carries no scheme; url.Parse would read it as a path, so give it one first.
	if !strings.Contains(raw, "://") {
		raw = "tcp://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// selfDepCapability is one capability whose dependency host globs the concentration metric covers.
type selfDepCapability struct {
	Name  string
	Globs []string
}

// selfDepCapabilities builds the concentration metric's capability set from TG's OWN config (TG-394 slice 2):
// the journal SSH hosts (slice 1, passed in already resolved to globs) plus the URL-declared endpoints of TG's
// other dependencies. Each endpoint URL is resolved to its host and used as an exact glob; a capability whose
// endpoint is unset contributes nothing (no phantom coverage). getenv is the runtime env reader.
func selfDepCapabilities(getenv func(string, string) string, journalGlobs []string) []selfDepCapability {
	caps := []selfDepCapability{{Name: selfDepCapabilityJournal, Globs: journalGlobs}}
	for _, uc := range []struct {
		name string
		urls []string
	}{
		{selfDepCapabilitySecrets, []string{getenv("TG_OPENBAO_ADDR", "")}},
		// The model gateway is ALWAYS dialed — main.go's real reader defaults TG_LITELLM_URL to the compose
		// gateway, so the agent has a model dependency even when the var is unset. Mirror that default here or the
		// metric under-counts the model dep on any deploy that leaves the var implicit (the others are opt-in:
		// substrate/tracker/notifier stay "" because their real readers require the var to be set).
		{selfDepCapabilityModel, []string{getenv("TG_LITELLM_URL", "http://litellm:4000")}},
		{selfDepCapabilityTracker, []string{getenv("TG_YOUTRACK_URL", "")}},
		{selfDepCapabilityNotifier, []string{getenv("TG_MATRIX_HOMESERVER", ""), getenv("TG_MATTERMOST_URL", "")}},
	} {
		seen := map[string]bool{}
		var globs []string
		for _, u := range uc.urls {
			if h := selfDepHostOfURL(u); h != "" && !seen[h] {
				seen[h] = true
				globs = append(globs, h)
			}
		}
		if len(globs) > 0 {
			sort.Strings(globs)
			caps = append(caps, selfDepCapability{Name: uc.name, Globs: globs})
		}
	}
	return caps
}

// startSelfDepConcentrationMultiJob returns a scrape-time reader that emits the concentration samples for EVERY
// configured capability (TG-394 slice 2), each under its own {capability} label. Like the single-capability job
// it reads the live graph from the atomic holder — no database, no ticker — so a scrape cannot see a stale copy.
// A nil holder or empty capability set yields a reader that emits nothing.
func startSelfDepConcentrationMultiJob(holder *estate.Holder, caps []selfDepCapability) func() []metrics.Sample {
	if holder == nil || len(caps) == 0 {
		return func() []metrics.Sample { return nil }
	}
	return func() []metrics.Sample {
		g := holder.Graph()
		var out []metrics.Sample
		for _, c := range caps {
			out = append(out, selfDepConcentrationSamples(g, c.Name, c.Globs)...)
		}
		return out
	}
}

// TG-394 SLICE 3 — the REACHABILITY / DEGRADED signal (fix-direction parts 2 & 4).
//
// Slices 1-2 publish the standing PLACEMENT concentration — the single-point-of-failure risk knowable at
// BOOT. This publishes the thing the pve03 cascade actually needed and TG did not have: a LIVE signal that
// one of TG's OWN dependency capabilities is degraded. During the incident TG's only embedding backend went
// unreachable and retrieval silently ran lexical-only for 11h12m with NOTHING reporting a reduced capability.
//
// THE SIGNAL IS THE GRAPH TG ALREADY HOLDS, NOT A NEW PROBER. A declared dependency host is "reachable" iff it
// has a LIVE-CONFIRMED placement in the estate graph — a fresh runs_on edge whose confidence is ABOVE the
// tombstone level (hostReachable, below). Freshness ALONE is not enough, and getting that wrong would have made
// this feature miss its own motivating incident: when a hypervisor goes silent the estate does not drop its
// guests' placements, it TOMBSTONES them (carryForwardUnreachable, core/estate/tombstone.go) — carrying each
// runs_on edge forward FRESH, at TombstoneConfidence, for up to TombstoneTTL (7 days) so blast-radius still
// folds the guest-down alerts into one hypervisor incident. A fresh-only check would read every guest on the
// dead hypervisor as reachable for those 7 days. The confidence floor is what discriminates a live placement
// from that blast-radius-continuity tombstone. TG does NOT open a socket to the estate for this: an active
// network prober would add a probe surface and a credential this metric must not own, and the graph already
// expresses the fact.
//
// CAPABILITIES (this family): embed / journal-evidence / secrets / tracker / notify — TG's dependency
// capabilities whose backing hosts its OWN config names. This is a DISTINCT set from slice-2's concentration
// vocabulary (model/notifier): the question here is reachability of the retrieval/notify CAPABILITY, and the
// session degraded-set stamp (part 4) is keyed on THESE names. `embed` is the retrieval backend the fused
// retriever dials — through the LiteLLM gateway (model.Embedder.Gateway, built from TG_LITELLM_URL); TG holds
// no separate ollama address (modules/catalog: "the server address is LiteLLM's config, not TG's"), so on a
// compose deploy embed's host is the internal gateway name, which resolves to no estate guest and is reported
// as hosts_checked=0 — honestly UNMEASURED, never a false "degraded". The estate-resolvable capabilities
// (journal-evidence and any URL pointed at a named estate host) are the armed ones: 5 of the 7 hosts the
// incident concentrated on were journal-evidence hosts, and their placement aging out is exactly what
// tg_capability_degraded{capability="journal-evidence"} would have caught. NO ESTATE IDENTIFIERS ARE COMPILED
// IN — the {host} label is a runtime value resolved from the graph.
const (
	selfDepCapabilityEmbed  = "embed"  // the retrieval embedding backend (dialed via TG_LITELLM_URL)
	selfDepCapabilityNotify = "notify" // the governance channels (TG_MATRIX_HOMESERVER + TG_MATTERMOST_URL)
)

// reachCapFromURLs resolves endpoint URLs to their hosts — de-duplicated, sorted — as an exact-match glob
// set. An unset/compose-internal URL contributes a host that simply resolves to nothing in the estate (honest
// hosts_checked < declared), never a guessed placement. Same host-of-URL resolution as slice 2.
func reachCapFromURLs(name string, urls []string) selfDepCapability {
	seen := map[string]bool{}
	var globs []string
	for _, u := range urls {
		if h := selfDepHostOfURL(u); h != "" && !seen[h] {
			seen[h] = true
			globs = append(globs, h)
		}
	}
	sort.Strings(globs)
	return selfDepCapability{Name: name, Globs: globs}
}

// selfDepReachCapabilities builds the reachability family's capability set from TG's OWN config (TG-394 slice
// 3): embed / journal-evidence / secrets / tracker / notify. getenv is the runtime env reader; journalGlobs
// is slice-1's already-resolved TG_JOURNAL_DEPLOYMENTS host-glob set. An endpoint that is unset contributes
// nothing (no phantom capability), EXCEPT embed's gateway, which — like slice-2's model — is ALWAYS dialed
// because main.go defaults TG_LITELLM_URL to the compose gateway, so the agent has an embed dependency even
// when the var is implicit and the signal must not under-count it.
func selfDepReachCapabilities(getenv func(string, string) string, journalGlobs []string) []selfDepCapability {
	caps := []selfDepCapability{
		reachCapFromURLs(selfDepCapabilityEmbed, []string{getenv("TG_LITELLM_URL", "http://litellm:4000")}),
	}
	if len(journalGlobs) > 0 {
		caps = append(caps, selfDepCapability{Name: selfDepCapabilityJournal, Globs: journalGlobs})
	}
	for _, c := range []selfDepCapability{
		reachCapFromURLs(selfDepCapabilitySecrets, []string{getenv("TG_OPENBAO_ADDR", "")}),
		reachCapFromURLs(selfDepCapabilityTracker, []string{getenv("TG_YOUTRACK_URL", "")}),
		reachCapFromURLs(selfDepCapabilityNotify, []string{getenv("TG_MATRIX_HOMESERVER", ""), getenv("TG_MATTERMOST_URL", "")}),
	} {
		if len(c.Globs) > 0 {
			caps = append(caps, c)
		}
	}
	return caps
}

// reachState is the THREE-valued classification of a resolved dependency host for the reachability / degraded
// signal (TG-394 slice 3, refined by TG-460). "Not a live placement" splits into two very different facts:
//
//   - reachLive     — a live-confirmed placement/adjacency (reachable).
//   - reachDegraded — resolved and backed by OBSERVED evidence, but that evidence is NOT a live confirmation:
//     a silent-hypervisor tombstone, or an authoritative source decayed/aged out. THIS is the
//     degradation the pve03 cascade needed surfaced.
//   - reachExcluded — the host's ONLY estate evidence is a thin LEARNED co-occurrence edge. Its sub-tombstone
//     confidence reflects LEARNING STRENGTH, not an outage, so it is neither reachable nor
//     degraded: it is dropped from the determination entirely — out of the degraded rollup AND
//     out of the hosts_checked denominator, keeping numerator and denominator over the SAME
//     host set (TG-460; the TG-449 numerator/denominator-parity discipline).
type reachState int

const (
	reachLive reachState = iota
	reachDegraded
	reachExcluded
)

// authoritativeSource reports whether an estate edge from this source is an OBSERVED placement/adjacency —
// evidence a real source actually saw this host in the estate — as opposed to the LEARNED co-occurrence tier
// (estate.SourceIncident), whose confidence encodes how strongly two things co-alarmed rather than how
// reliably the world was observed. The reachability signal reads a sub-tombstone confidence as an OUTAGE
// (hostReachState, below); it may only do so for sources whose confidence tracks OBSERVATION reliability, or a
// thin learned edge (estate.LearnedConfidence(1)=0.45, below TombstoneConfidence) would masquerade as a silent
// hypervisor. Anything NOT known to be an observed source (SourceIncident today, plus any future non-observed
// tier) is non-authoritative, so a host backed only by such an edge reads UNMEASURED (honest) rather than a
// false "degraded" — the absent-is-not-zero discipline this whole signal rests on. The observed set is the
// seeded/ground-truth sources (the ones carrying a fixed estate.SourceConfidence): tunnel/pve/netbox/librenms/
// declared, and chaos (an injected-and-observed fault whose root is ground truth).
func authoritativeSource(s estate.Source) bool {
	switch s {
	case estate.SourceTunnel, estate.SourcePVE, estate.SourceNetbox,
		estate.SourceLibreNMS, estate.SourceDeclared, estate.SourceChaos:
		return true
	default: // estate.SourceIncident (learned co-occurrence) and any future non-observed tier
		return false
	}
}

// hostReachState classifies a dependency host's placement in the estate graph — the graph-based degradation
// proxy (TG-394 slice 3), with the confirmed-live check that freshness ALONE misses AND the TG-460 refinement
// that a thin LEARNED edge is not an outage signal.
//
// THE DEFECT FRESHNESS ALONE HAD (guest path — steps 1-2, UNCHANGED). When a hypervisor goes silent — the
// pve03 NVMe-failure shape this feature exists to catch — the estate does NOT drop its guests' placement edges.
// carryForwardUnreachable (core/estate/tombstone.go) TOMBSTONES each guest's `runs_on` edge: it re-inserts
// it with ValidUntil = now + TombstoneTTL (7 days, in the FUTURE) at Confidence decayed to TombstoneConfidence
// (0.5), so blast-radius still folds the N guest-down alerts into ONE hypervisor incident. A tombstoned edge is
// therefore STILL FRESH by g.fresh(); a bare len(Parents())>0 would have read every guest on the dead
// hypervisor as reachable for up to 7 days. So a `runs_on` ABOVE TombstoneConfidence is reachLive; a guest
// whose every `runs_on` sits AT or BELOW it is the blast-radius-continuity tombstone → reachDegraded. This is
// DISTINCT from g.fresh(): the tombstone passes freshness and fails the confidence floor.
//
// THE NON-GUEST FALLBACK (step 3 — refined by TG-460). A host with no `runs_on` containment (a bare/physical
// dependency host monitored by netbox/librenms) is confirmed live by an AUTHORITATIVE-source edge ABOVE the
// tombstone floor — a real source still observing it. The confidence floor ALONE is not enough here: a thin
// learned co-occurrence edge (estate.SourceIncident, estate.LearnedConfidence(count)=0.4+0.05·count, so
// count<=2 ⇒ <=0.5) sits BELOW the floor for reasons that have nothing to do with an outage — the low number
// is about how little the pair co-alarmed, not about a silent hypervisor. Reading THAT as degraded is a false
// positive (TG-460). So for a non-guest:
//   - an authoritative edge above the floor                                → reachLive;
//   - authoritative evidence present but all at/below the floor, OR no
//     fresh parents at all (source decayed/aged out — a real degradation)  → reachDegraded (safe direction);
//   - ONLY learned edges back the host (no authoritative edge, no runs_on) → reachExcluded — dropped from the
//     determination, because a learned edge's confidence is not a statement about reachability at all.
//
// Ground-truth topology (pve 0.95, netbox/librenms 0.90, declared 0.85) clears the floor; the 0.5 tombstone
// (or a decayed remnant) does not; a learned-only host is EXCLUDED, not degraded. THIS is the check the killing
// mutation flips: force reachLive and tg_capability_degraded can never leave 0.
func hostReachState(g *estate.Graph, host string) reachState {
	parents := g.Parents(estate.Entity{Name: host})
	sawRunsOn := false
	for _, p := range parents {
		if p.Rel == estate.RelRunsOn {
			sawRunsOn = true
			if p.Confidence > estate.TombstoneConfidence {
				return reachLive // a live-confirmed placement
			}
		}
	}
	if sawRunsOn {
		// Every runs_on placement is a tombstone/decay (<= TombstoneConfidence): the hypervisor is silent and the
		// edge is carried forward only for blast-radius continuity — not a live confirmation of this host.
		return reachDegraded
	}
	// No containment edge at all (a non-guest host). Reachability requires an AUTHORITATIVE observation above the
	// tombstone floor; a bare/physical dependency host monitored by netbox/librenms is not a permanent false-
	// degraded, but a host whose only evidence is a thin LEARNED edge is EXCLUDED, not degraded (TG-460).
	hasAuthoritative := false
	for _, p := range parents {
		if !authoritativeSource(p.Source) {
			continue
		}
		hasAuthoritative = true
		if p.Confidence > estate.TombstoneConfidence {
			return reachLive
		}
	}
	if !hasAuthoritative && len(parents) > 0 {
		// The host resolves, but its only fresh edges are thin LEARNED co-occurrence guesses — no observed source
		// has a placement for it. That is not an outage; it is the absence of an authoritative signal. Exclude it
		// from the degraded rollup AND the coverage denominator rather than mint a false degraded.
		return reachExcluded
	}
	// Authoritative evidence exists but none is a live confirmation (decayed at/below the floor), OR the host has
	// no fresh parents at all — either way its placement is not confirmed live: degraded (the safe direction).
	return reachDegraded
}

// hostReachable is the boolean live-placement predicate — true iff a host is reachLive. It collapses BOTH
// reachDegraded and reachExcluded to false, and is a convenience for the guest-path regression tests, which
// never produce reachExcluded. Production reachability/degraded accounting reads hostReachState directly, so a
// learned-only host is EXCLUDED (TG-460) rather than counted as degraded.
func hostReachable(g *estate.Graph, host string) bool {
	return hostReachState(g, host) == reachLive
}

// selfDepReachabilitySamples publishes, per capability: one tg_self_dependency_reachable{capability,host}
// (1/0) for each RESOLVED backing host, plus — ALWAYS, absent-is-not-zero — the tg_capability_degraded
// rollup and the tg_self_dependency_hosts_checked coverage denominator. A capability whose declared hosts
// resolve to nothing (a compose-internal endpoint) emits degraded=0 WITH hosts_checked=0, so a scrape reads
// "unmeasured", never a false "healthy" (the coverage-denominator-shares-the-numerator's-freshness discipline,
// TG-449). A nil graph emits nothing.
func selfDepReachabilitySamples(g *estate.Graph, caps []selfDepCapability) []metrics.Sample {
	if g == nil {
		return nil
	}
	var out []metrics.Sample
	for _, c := range caps {
		hosts := resolveDepHosts(g, c.Globs)
		degraded := 0.0
		checked := 0
		for _, h := range hosts {
			st := hostReachState(g, h)
			if st == reachExcluded {
				// A learned-only host: its sub-tombstone confidence is about co-occurrence strength, not an outage.
				// Drop it from BOTH the per-host series and the coverage denominator so tg_self_dependency_hosts_
				// checked stays the EXACT host set tg_capability_degraded is computed over (TG-460 / TG-449 parity).
				continue
			}
			checked++
			v := 1.0
			if st != reachLive {
				v = 0.0
				degraded = 1.0
			}
			out = append(out, metrics.Sample{
				Name: "tg_self_dependency_reachable", Kind: metrics.Gauge,
				Help: "1 iff this backing host of a TG dependency capability has a LIVE-CONFIRMED placement in the " +
					"estate graph — a fresh runs_on edge ABOVE the tombstone confidence (TG-394 slice 3). 0 = its " +
					"placement is gone OR only carried forward as a silent-hypervisor tombstone (fresh but decayed " +
					"to TombstoneConfidence for blast-radius continuity) — the degradation proxy, from the graph TG " +
					"already holds, no active probe. {host} is a runtime label resolved from the estate; no estate " +
					"identifier is compiled in.",
				Value: v, Labels: map[string]string{"capability": c.Name, "host": h},
			})
		}
		out = append(out,
			metrics.Sample{
				Name: "tg_capability_degraded", Kind: metrics.Gauge,
				Help: "1 iff ANY host backing this TG dependency capability is unreachable in the estate graph " +
					"(TG-394 slice 3) — the LIVE signal that a capability has silently degraded, the thing the " +
					"pve03 cascade needed and TG lacked when retrieval ran lexical-only for 11h12m. ALWAYS " +
					"emitted per declared capability; read beside tg_self_dependency_hosts_checked, which is 0 " +
					"when the capability's endpoint resolves to no estate host (degraded=0 there is UNMEASURED, " +
					"not healthy).",
				Value: degraded, Labels: map[string]string{"capability": c.Name},
			},
			metrics.Sample{
				Name: "tg_self_dependency_hosts_checked", Kind: metrics.Gauge,
				Help: "backing hosts of this capability whose reachability was evaluated (resolved against the " +
					"estate) — the coverage denominator for tg_capability_degraded (TG-394 slice 3). 0 means the " +
					"declared endpoint resolves to no estate host (a compose-internal gateway), so the degraded " +
					"rollup beside it is unmeasured rather than a healthy zero. A host whose ONLY estate evidence " +
					"is a thin learned co-occurrence edge is excluded here as well as from the rollup (TG-460), so " +
					"numerator and denominator stay over the same host set. Always emitted per capability.",
				Value: float64(checked), Labels: map[string]string{"capability": c.Name},
			},
		)
	}
	return out
}

// degradedCapabilitySet returns the sorted names of the capabilities with at least one DEGRADED backing host
// — the same computation tg_capability_degraded publishes, as a set (TG-394 slice 3 part 4). It is the value
// stamped on each session record so a lexical-only investigation is legible afterwards: the runner's
// composition root wraps this over the live estate holder and injects it as runner.Deps.DegradedCapabilities.
// Routed through hostReachState so the killing mutation reddens both the gauge and the stamp, and — matching
// the gauge exactly — a host whose only evidence is a thin learned co-occurrence edge is EXCLUDED, not counted
// as degraded (TG-460): only reachDegraded (an observed placement that is a tombstone or has aged out) marks a
// capability. A nil graph or empty capability set yields no degraded capabilities.
func degradedCapabilitySet(g *estate.Graph, caps []selfDepCapability) []string {
	if g == nil {
		return nil
	}
	var out []string
	for _, c := range caps {
		for _, h := range resolveDepHosts(g, c.Globs) {
			if hostReachState(g, h) == reachDegraded {
				out = append(out, c.Name)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// startSelfDepReachabilityJob returns the scrape-time reader for the reachability/degraded family. Like the
// concentration job it reads the live graph from the atomic holder — no database, no ticker — so a scrape
// cannot see a stale copy. A nil holder or empty capability set yields a reader that emits nothing.
func startSelfDepReachabilityJob(holder *estate.Holder, caps []selfDepCapability) func() []metrics.Sample {
	if holder == nil || len(caps) == 0 {
		return func() []metrics.Sample { return nil }
	}
	return func() []metrics.Sample {
		return selfDepReachabilitySamples(holder.Graph(), caps)
	}
}
