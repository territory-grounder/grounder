package credential

import (
	"fmt"
	"sort"
	"strings"
)

// ---------------------------------------------------------------------------------------------------------
// TWO-PLANE ROUTER (spec/016 task T-016-6, REQ-1611).
//
// The credential engine serves TWO identity planes that must NEVER cross (design.md step 0, "Plane select"):
//
//   - PlaneMachine — the machine → host credential plane. A resolution answers "with what identity does TG
//     authenticate to this target?" and yields a Bundle (ssh key / user / port / become / api token — the
//     credential for REACHING a host). Fed by AWX / Ansible / Semaphore / OpenBao / Vault + the native store.
//
//   - PlaneHuman — the human → console approver plane. A resolution answers "which humans / groups may
//     APPROVE an action?" and yields an ApproverSet — a set of {users, groups}, NOT a host Bundle. Fed by
//     LDAP / OIDC (T-016-10) and consumed by the policy engine's spec/015 approve_by (REQ-1516, TG-102).
//
// This file formalizes the human-plane OUTPUT type (ApproverIdentity / ApproverSet) and the ROUTER that
// ENFORCES plane isolation (the core of REQ-1611): a machine-plane source's synced entries may ONLY populate
// host-credential resolution and NEVER an approver identity; a human-plane source's entries may ONLY
// populate approver identity and NEVER a host Bundle. Cross-plane leakage is a HARD failure at sync time
// (validateEntryPlane, called from sync.go), and the resolution paths are isolated by construction:
// SyncEngine.Resolve considers ONLY machine-plane slots, ResolveApprovers considers ONLY human-plane slots.
//
// The human plane is FAIL-CLOSED exactly like the machine plane (REQ-1602): no approver resolves → refuse,
// NEVER a default "anyone may approve". ApproverIdentity carries NO secret-bearing field — structurally it
// cannot be turned into a host credential, so the two planes cannot be conflated even by a coding mistake.
// ---------------------------------------------------------------------------------------------------------

// PrincipalKind is what an approver identity denotes: an individual user or a group / role. Fixed, closed
// vocabulary (an unknown kind is a construction error, never a silent default).
type PrincipalKind string

const (
	// PrincipalUser is an individual human principal who may approve.
	PrincipalUser PrincipalKind = "user"
	// PrincipalGroup is a group / role whose membership may approve (the spec/015 approve_by key shape).
	PrincipalGroup PrincipalKind = "group"
)

func (k PrincipalKind) valid() bool { return k == PrincipalUser || k == PrincipalGroup }

// ErrNoApprovers is the human-plane fail-closed sentinel (REQ-1611/1602): no approver identity resolves, so
// the action is not approvable. It WRAPS ErrUnresolved — every refusal is fail-closed, so IsRefused reports
// true for a no-approver outcome exactly as for an unresolved host — and there is NEVER a default "anyone may
// approve" fallback.
var ErrNoApprovers = fmt.Errorf("%w: no approver identity resolved — refuse (fail closed; never 'anyone may approve')", ErrUnresolved)

// ApproverIdentity is one HUMAN-plane principal that may approve — the human-plane analogue of Bundle and
// the OUTPUT of a human-plane sync entry (REQ-1611/1614). It is a user or a group, with the groups / roles it
// belongs to (membership), keyed within its source by the SourceEntry NativeID.
//
// It carries NO secret-bearing field — no SecretRef, no host, no ssh key, no port — so an ApproverIdentity
// can NEVER be authenticated against a host: the machine ↔ human separation is STRUCTURAL, not a runtime
// check. The fields are UNEXPORTED and the only constructor is NewApproverIdentity, so the ZERO value is
// invalid (Valid()=false) — "no blank approver" holds by construction, mirroring Bundle.
type ApproverIdentity struct {
	ok bool // false for the zero value — an invalid/unpopulated approver; set only by NewApproverIdentity

	kind   PrincipalKind
	name   string   // the canonical user or group name (non-secret)
	groups []string // normalized (trimmed, de-duplicated, sorted) group / role memberships
}

// ApproverIdentitySpec is the required-field input to NewApproverIdentity. A human-plane connector (LDAP /
// OIDC) builds one per synced principal.
type ApproverIdentitySpec struct {
	Kind   PrincipalKind
	Name   string
	Groups []string // group / role memberships (for a user) or nested groups (for a group); optional
}

// NewApproverIdentity builds an ApproverIdentity from a spec or returns an error. It enforces:
//   - Kind is a known PrincipalKind (user / group);
//   - Name is non-empty.
//
// Groups are normalized (trimmed, empties dropped, de-duplicated, sorted) so drift comparison is positional
// and deterministic. On any failure it returns the ZERO ApproverIdentity (Valid()=false) and a non-nil error
// — fail closed by construction.
func NewApproverIdentity(spec ApproverIdentitySpec) (ApproverIdentity, error) {
	if !spec.Kind.valid() {
		return ApproverIdentity{}, fmt.Errorf("credential: approver identity has unknown kind %q (use user/group)", spec.Kind)
	}
	name := strings.TrimSpace(spec.Name)
	if name == "" {
		return ApproverIdentity{}, fmt.Errorf("credential: approver identity requires a name")
	}
	return ApproverIdentity{ok: true, kind: spec.Kind, name: name, groups: normalizeGroups(spec.Groups)}, nil
}

// Valid reports whether a is a real populated approver. The zero ApproverIdentity is invalid — a caller must
// NEVER treat a zero/unresolved approver as an identity permitted to approve (REQ-1602).
func (a ApproverIdentity) Valid() bool { return a.ok }

// Non-secret accessors — an ApproverIdentity has ONLY non-secret fields, all safe to log / persist.
func (a ApproverIdentity) Kind() PrincipalKind { return a.kind }
func (a ApproverIdentity) Name() string        { return a.name }

// Groups returns a copy of the normalized memberships (never the backing slice).
func (a ApproverIdentity) Groups() []string { return append([]string(nil), a.groups...) }

// ApproverQuery is the key a human-plane resolution matches on: the group / role the policy engine's
// approve_by references (REQ-1516). ResolveApprovers answers "who may approve as this group / role?".
type ApproverQuery struct {
	Group string // the group / role name (e.g. "sre-oncall")
}

// ApproverSet is the OUTPUT of a human-plane resolution: the users and groups that may approve for the
// queried group / role, plus the human sources that contributed (non-secret provenance). It is a DISTINCT
// type from Bundle — a host-reachability resolution can never accidentally return one, and this can never
// carry host-credential material. An empty set is never returned with a nil error (fail closed).
type ApproverSet struct {
	Users   []string // approving user principals, sorted, de-duplicated
	Groups  []string // approving group / role principals, sorted, de-duplicated
	Sources []string // human-plane source ids that contributed, sorted
}

// Empty reports whether the set names no approver at all.
func (s ApproverSet) Empty() bool { return len(s.Users) == 0 && len(s.Groups) == 0 }

// ResolveApprovers resolves a group / role to the set of human principals that may approve, across every
// HUMAN-plane synced source, or fails closed (REQ-1611/1614/1602). It is the human-plane counterpart of
// Resolve (which serves the machine plane): it scans ONLY PlaneHuman slots — a machine-plane source can
// NEVER contribute an approver — and unions the matching principals (approver identity is additive: a
// principal that is the queried group, or is a member of it, may approve).
//
// It NEVER returns a default or "anyone may approve" set: an empty union is ErrUnresolved (fail closed), and
// a nil / zero-source engine or an empty query refuses. The returned set carries only non-secret identity.
func (se *SyncEngine) ResolveApprovers(q ApproverQuery) (ApproverSet, error) {
	if se == nil {
		return ApproverSet{}, ErrNoApprovers
	}
	group := strings.TrimSpace(q.Group)
	if group == "" {
		return ApproverSet{}, fmt.Errorf("%w: empty approver query", ErrNoApprovers)
	}

	se.mu.RLock()
	defer se.mu.RUnlock()

	users := map[string]struct{}{}
	groups := map[string]struct{}{}
	srcs := map[string]struct{}{}
	for _, slot := range se.slots {
		// ISOLATION: a machine-plane slot NEVER contributes an approver identity (REQ-1611).
		if slot.src.Plane() != PlaneHuman || !slot.synced {
			continue
		}
		for _, e := range slot.entries {
			a := e.Approver
			if !a.Valid() || !approverMatches(a, group) {
				continue
			}
			switch a.kind {
			case PrincipalUser:
				users[a.name] = struct{}{}
			case PrincipalGroup:
				groups[a.name] = struct{}{}
			}
			srcs[slot.src.ID()] = struct{}{}
		}
	}

	if len(users) == 0 && len(groups) == 0 {
		return ApproverSet{}, fmt.Errorf("%w: no human-plane approver resolves for group %q", ErrUnresolved, group)
	}
	return ApproverSet{Users: sortedKeys(users), Groups: sortedKeys(groups), Sources: sortedKeys(srcs)}, nil
}

// approverMatches reports whether the approver a may approve for the queried group / role — either a IS that
// group (a group principal named group) or a is a MEMBER of it (group ∈ a.groups). Case-insensitive.
func approverMatches(a ApproverIdentity, group string) bool {
	if a.kind == PrincipalGroup && strings.EqualFold(a.name, group) {
		return true
	}
	for _, g := range a.groups {
		if strings.EqualFold(g, group) {
			return true
		}
	}
	return false
}

// validateEntryPlane is the CORE of the two-plane router (REQ-1611): it validates one synced entry against
// its source's declared plane, called from the sync convergence (sync.go) for EVERY pulled entry. A
// machine-plane entry MUST carry a valid host Bundle and MUST NOT carry an approver identity; a human-plane
// entry MUST carry a valid approver identity and MUST NOT carry a host Bundle. Any cross-plane entry is a
// hard error — the sync fails closed and the prior converged state is retained — so a credential-plane sync
// can NEVER populate approve_by and an approver-plane sync can NEVER populate a host bundle.
func validateEntryPlane(plane Plane, e SourceEntry) error {
	switch plane {
	case PlaneMachine:
		if e.Approver.Valid() {
			return fmt.Errorf("machine-plane entry %q carries an approver identity (cross-plane leak refused)", e.NativeID)
		}
		if !e.Bundle.Valid() {
			return fmt.Errorf("machine-plane entry %q carries no valid host bundle", e.NativeID)
		}
	case PlaneHuman:
		if e.Bundle.Valid() {
			return fmt.Errorf("human-plane entry %q carries a host credential bundle (cross-plane leak refused)", e.NativeID)
		}
		if !e.Approver.Valid() {
			return fmt.Errorf("human-plane entry %q carries no valid approver identity", e.NativeID)
		}
	default:
		return fmt.Errorf("entry %q declares unknown plane %q", e.NativeID, plane)
	}
	return nil
}

// sameApprover reports whether two approver identities are equal (used by the drift diff for human entries).
// Groups are normalized+sorted at construction, so a positional compare is exact.
func sameApprover(a, b ApproverIdentity) bool {
	if a.ok != b.ok || a.kind != b.kind || a.name != b.name || len(a.groups) != len(b.groups) {
		return false
	}
	for i := range a.groups {
		if a.groups[i] != b.groups[i] {
			return false
		}
	}
	return true
}

// normalizeGroups trims, drops empties, de-duplicates (case-insensitively, keeping the first spelling), and
// sorts the memberships for a deterministic, comparable ApproverIdentity.
func normalizeGroups(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(in))
	for _, g := range in {
		g = strings.TrimSpace(g)
		if g == "" {
			continue
		}
		key := strings.ToLower(g)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, g)
	}
	if len(out) == 0 {
		return nil
	}
	sort.Strings(out)
	return out
}

// sortedKeys returns the map's keys sorted — the deterministic ordering for an ApproverSet's fields.
func sortedKeys(m map[string]struct{}) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
