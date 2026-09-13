package credential

import (
	"context"
	"sort"
	"strings"
	"sync"
)

// ---------------------------------------------------------------------------------------------------------
// ESTATE HOST↔GROUP MEMBERSHIP RECONCILIATION (spec/016 T-052, REQ-1605).
//
// A group-selector rule/entry — e.g. an AWX job-template bundle keyed by its inventory NAME
// (Selector{Kind: KindGroup, Pattern: "Proxmox Dynamic Inventory"}) — can only match a Target whose Groups
// carry that group (see Match, KindGroup). But the read-only investigation path (and the future actuation
// path) build a Target from a HOST name alone (Target{Host: host}) and know nothing of which estate groups
// that host belongs to. So a host that lives in an AWX inventory could never be matched to that inventory's
// bundle — the machine-plane group bundles emitted by the AWX source never RESOLVED.
//
// This closes that gap: a CredentialSource that ALSO knows host↔group membership (AWX inventory→host today,
// extensible to Semaphore/OpenBao/native later) contributes a host→[groups] map through MembershipSource;
// the SyncEngine indexes it (per-source, so a re-sync replaces one source's membership without orphaning
// others) and consults it at resolve time to populate Target.Groups BEFORE matching. The membership is
// NON-SECRET by construction — host names and group names only, never a token, key, or secret value.
//
// FAIL-CLOSED, ADDITIVE: no membership for a host ⇒ no extra groups ⇒ resolution is EXACTLY as before
// (host / host-glob / native rules still win by most-specific-wins). This only ADDS the ability to match a
// group selector for a host known to be in that group; it never removes or overrides a more-specific match.
// ---------------------------------------------------------------------------------------------------------

// MembershipSource is the OPTIONAL capability a CredentialSource may also implement to contribute estate
// host↔group membership (REQ-1605). Membership returns a map from host name to the group names that host
// belongs to (e.g. the AWX inventories a host is a member of). It is READ-ONLY and NON-SECRET: the map holds
// host names and group names ONLY — never a credential, token, or secret value. It fails closed on a
// transport/read error (returns an error and no partial map), leaving the prior indexed membership intact.
type MembershipSource interface {
	Membership(ctx context.Context) (map[string][]string, error)
}

// MembershipIndex resolves a host to the estate groups it belongs to, so a group-selector rule/entry
// resolves for a host in that group. The SyncEngine's built-in index is populated from every registered
// MembershipSource; the interface is exported so an alternative (durable) index can be substituted later.
type MembershipIndex interface {
	GroupsFor(host string) []string
}

// membershipStore is the in-memory MembershipIndex. It keeps a SEPARATE host→groups map per source id so a
// source's re-sync replaces only that source's membership (no orphan, no cross-source duplication), and a
// GroupsFor is the UNION across sources. Host keys are lowercased for case-insensitive lookup (mirroring
// Match's case-insensitive names); group names are stored verbatim. Safe for concurrent use.
type membershipStore struct {
	mu       sync.RWMutex
	bySource map[string]map[string][]string // sourceID → lower(host) → group names
}

func newMembershipStore() *membershipStore {
	return &membershipStore{bySource: map[string]map[string][]string{}}
}

// replace installs (or replaces) one source's host→groups membership. An empty/nil map clears that source's
// membership entirely (a source that now knows no memberships stops contributing any). Host names are
// normalised (trimmed, lowercased) and their group lists deduped; a blank host or group is dropped.
func (m *membershipStore) replace(sourceID string, hostGroups map[string][]string) {
	norm := make(map[string][]string, len(hostGroups))
	for host, groups := range hostGroups {
		h := strings.ToLower(strings.TrimSpace(host))
		if h == "" {
			continue
		}
		norm[h] = dedupeGroups(append(norm[h], groups...)) // union if the same host appears under >1 key form
	}
	m.mu.Lock()
	if len(norm) == 0 {
		delete(m.bySource, sourceID)
	} else {
		m.bySource[sourceID] = norm
	}
	m.mu.Unlock()
}

// groupsFor returns the union of the groups the host belongs to across all sources — sorted, deduped, and
// non-nil only when at least one source knows the host. An unknown host yields nil (⇒ Target.Groups stays
// exactly as the caller built it ⇒ resolution unchanged, fail-closed).
func (m *membershipStore) groupsFor(host string) []string {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "" {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []string
	for _, hg := range m.bySource {
		out = append(out, hg[h]...)
	}
	return dedupeGroups(out)
}

// GroupsFor implements MembershipIndex.
func (m *membershipStore) GroupsFor(host string) []string { return m.groupsFor(host) }

// mergeGroups unions the caller-supplied groups with membership-supplied groups, deduped case-insensitively.
// It NEVER drops a caller-supplied group (a caller that already knows a target's groups keeps them); it only
// ADDS the membership-derived ones. Returns the caller's slice unchanged when there is nothing to add.
func mergeGroups(have, add []string) []string {
	if len(add) == 0 {
		return have
	}
	return dedupeGroups(append(append(make([]string, 0, len(have)+len(add)), have...), add...))
}

// dedupeGroups drops blank and case-insensitively-duplicate group names and returns a sorted, stable slice
// (nil for an empty input). Sorting keeps equal-specificity tie detection deterministic downstream.
func dedupeGroups(groups []string) []string {
	if len(groups) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(groups))
	out := make([]string, 0, len(groups))
	for _, g := range groups {
		g = strings.TrimSpace(g)
		if g == "" {
			continue
		}
		k := strings.ToLower(g)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, g)
	}
	if len(out) == 0 {
		return nil
	}
	sort.Strings(out)
	return out
}

// compile-time proof the in-memory store satisfies the exported index contract.
var _ MembershipIndex = (*membershipStore)(nil)
