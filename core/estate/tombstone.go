package estate

import "time"

// TombstoneTTL bounds how long an unconfirmed hypervisor containment edge is retained after its hypervisor
// stops answering, before it is treated as decommissioned and allowed to drop. It must comfortably outlast a
// real hypervisor outage — the 2026-08-06 pve03 NVMe failure ran ~11h — so correlation keeps the guest→host
// parents that fold N guest-down alerts into ONE hypervisor incident for the whole window.
const TombstoneTTL = 7 * 24 * time.Hour

// TombstoneConfidence is the reduced confidence a tombstoned (last-known-but-unconfirmed) hypervisor
// containment edge carries: below the ~0.95 of a live hypervisor read, so a consumer can tell "we can no
// longer reach this parent" from "we just confirmed it", while the edge still participates in the
// blast-radius walk.
const TombstoneConfidence = 0.5

// placementIndices is the positive evidence the tombstone reasons over, captured ONCE from a freshly-rebuilt
// graph (indexPlacements) BEFORE any tombstone is written. Sharing one snapshot across every source's pass is
// load-bearing (TG-521): the Holder runs carryForwardUnreachable once per live-hypervisor source on the SAME
// graph, and if each pass re-scanned `next` it would read the PRIOR pass's just-written tombstones as fresh
// positive evidence — an order-dependent cross-source pollution. Built once, the passes are order-independent.
type placementIndices struct {
	// guestPresent: a guest placed by ANY authoritative source. Cross-source ON PURPOSE — it catches a guest
	// that migrated pve↔vsphere (its new-host edge, whatever the source, supersedes the stale parent).
	guestPresent map[string]bool
	// nodeHasGuest: a host still hosting ≥1 guest. Keyed per host name; a live host means a vanished guest was
	// DESTROYED, not silenced.
	nodeHasGuest map[string]bool
	// sourceHasEdges: whether THIS source emitted any authoritative runs_on edge this cycle — the honest-empty
	// discriminator, PER SOURCE. A source that produced zero rows without erroring is genuinely empty; one that
	// produced some is a partial picture whose missing hosts are silent. This must be per-source: a global
	// "did ANY source produce edges" check (the pre-generalization guard's effective behaviour via nodeHasGuest)
	// silently defeats a genuine per-source decommission the moment a second live hypervisor exists.
	sourceHasEdges map[Source]bool
}

// indexPlacements captures the positive-evidence indices from a fresh build. Call it ONCE per refresh, before
// any carryForwardUnreachable pass, and share the result across every source's pass (see placementIndices).
func indexPlacements(next *Graph) placementIndices {
	p := placementIndices{
		guestPresent:   map[string]bool{},
		nodeHasGuest:   map[string]bool{},
		sourceHasEdges: map[Source]bool{},
	}
	if next == nil {
		return p
	}
	for _, e := range next.edges {
		if e.Rel != RelRunsOn || e.Source == SourceIncident {
			continue // only authoritative containment is positive evidence of placement
		}
		p.guestPresent[canonName(e.From.Name)] = true
		p.nodeHasGuest[canonName(e.To.Name)] = true
		p.sourceHasEdges[e.Source] = true
	}
	return p
}

// carryForwardUnreachable preserves the causal topology of a hypervisor that has gone SILENT instead of
// deleting it (TG-375; generalized across hypervisor sources in TG-521). It is parameterized by `source` — the
// authoritative hypervisor source whose silence is being reasoned about (SourcePVE for Proxmox, SourceVsphere
// for vCenter; any future live-hypervisor source slots in the same way). The estate refresh is a full rebuild
// from current source visibility, so when the hypervisor's API stops listing a down host's guests, that host's
// authoritative `runs_on` parent edges simply vanish from the freshly built graph — absence of evidence
// written as evidence of absence, into the ONE structure correlation uses to fold N guest-down alerts into a
// single hypervisor incident. Measured on the pve03 cascade: runs_on→pve03 went 52→0 three minutes after the
// NVMe failed, and blast-radius then predicted an EMPTY set for the outage it should have explained. vCenter
// has the identical failure shape (TG-521): a silently-dark vCenter's VM→host edges vanish and read as
// reachLive-until-TTL without this.
//
// It distinguishes a host that went SILENT from a guest that genuinely LEFT, using only the two graphs (no
// extra API call), over the SHARED placement snapshot `idx` (from indexPlacements(next), taken before any pass
// wrote tombstones). For each prior authoritative `runs_on` edge G→N (from `source`) the new build did NOT
// re-emit:
//   - if G reappears on ANY host in the new graph → G MIGRATED; its new-host edge supersedes this one → drop;
//   - else if N still hosts ≥1 guest in the new graph → N is up and G was destroyed → drop;
//   - else N hosts NO guest in the new graph → N is unreachable → TOMBSTONE: carry G→N forward with a reduced
//     confidence and a bounded ValidUntil, so correlation still reads "these guests share a parent we can no
//     longer reach" — the diagnosis — until the host returns (a live read ratchets it back) or the TTL lapses.
//
// Deletion therefore requires POSITIVE evidence the relationship ended (a migration, or a still-up host),
// never the mere silence of the API. prior is the last good graph (held by Holder); next is the freshly built
// graph about to be swapped in; tombstones are written into next.
//
// sourceUnreadable is true when THIS `source` itself ERRORED this cycle (its whole API is unreachable, not
// just one host). The HONEST-EMPTY GUARD (TG-521 fix) is per-source: this source's prior edges are preserved
// ONLY when it errored, or it still emitted some placement (idx.sourceHasEdges[source] — a partial picture
// whose missing hosts are silent). A source that produced ZERO edges without erroring is a genuine
// decommission-to-zero, and its prior edges must NOT be resurrected — EVEN when the OTHER hypervisor is alive.
// (The pre-generalization guard tested cross-source placement, which silently defeated this once a second live
// hypervisor existed: a retired vCenter would keep phantom VM→host tombstones for the whole TTL while pve ran.)
//
// KNOWN LIMITATION (fails toward the safe direction; documented rather than guarded because the more precise
// signal is a larger change than the defect warrants):
//   - EMPTY-BUT-UP host. Reachability is inferred from "the host still hosts ≥1 guest", because these sources
//     do not report per-host liveness. If ops drains the LAST guest off an otherwise-healthy host, that host
//     reads as silent and its departed guest is tombstoned (decayed 0.5, ≤ TTL) rather than deleted — a mildly
//     over-connected blast radius at reduced confidence for up to the TTL, strictly better than deleting live
//     topology, self-healing when the host next hosts a guest or the TTL lapses.
//   - GUEST IDENTITY. The migration test keys on the guest's canonical name; two distinct guests sharing a
//     canonical name would be conflated — inheriting the graph's (Type, Name) identity assumption, which the
//     estate's site-prefixed guest names keep unique.
func carryForwardUnreachable(prior, next *Graph, source Source, sourceUnreadable bool, idx placementIndices) int {
	if prior == nil || next == nil {
		return 0
	}
	now := next.now()
	// Honest-empty guard, SCOPED to THIS source (TG-521). See the doc above.
	if !sourceUnreadable && !idx.sourceHasEdges[source] {
		return 0
	}
	var kept int
	for k, e := range prior.edges {
		if e.Source != source || e.Rel != RelRunsOn {
			continue // scope: only THIS source's authoritative containment is tombstoned
		}
		if _, stillEmitted := next.edges[k]; stillEmitted {
			continue // the source re-emitted it this cycle — a live read, nothing to preserve
		}
		if idx.guestPresent[canonName(e.From.Name)] {
			continue // guest migrated (present under ANY source): its new edge supersedes this one → genuine delete
		}
		if idx.nodeHasGuest[canonName(e.To.Name)] {
			continue // host is up and still hosts guests, but this guest is gone → genuine delete
		}
		// The guest's host hosts nothing in the new graph → it went silent, not empty. Preserve the parent.
		conf := e.Confidence
		if conf > TombstoneConfidence {
			conf = TombstoneConfidence // decay a live read down to the unconfirmed level (once)
		}
		validUntil := e.ValidUntil
		if validUntil.IsZero() {
			validUntil = now.Add(TombstoneTTL) // first disappearance: start the decommission clock, anchored here
		}
		if !validUntil.After(now) {
			continue // silent past the TTL → treated as decommissioned → allow the drop
		}
		next.Upsert(Edge{
			From: e.From, To: e.To, Rel: e.Rel,
			Confidence: conf, Source: e.Source,
			ValidUntil: validUntil, ExpectedAlerts: e.ExpectedAlerts, DelaySeconds: e.DelaySeconds,
		})
		kept++
	}
	return kept
}

// sourceFailed reports whether the named source itself errored this refresh (its whole API is unreachable, not
// just one host) — the signal that authorises preserving every prior parent from that source even when no host
// answered.
func sourceFailed(errs []SourceError, source Source) bool {
	for _, se := range errs {
		if se.Source == source {
			return true
		}
	}
	return false
}
