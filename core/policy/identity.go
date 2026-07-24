package policy

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/territory-grounder/grounder/core/credential"
)

// ---------------------------------------------------------------------------------------------------------
// PRINCIPAL RESOLUTION for approve_by enforcement (spec/015 task T-015-9, REQ-1516).
//
// A policy rule that resolves to the `approve` verdict names, in its `approve_by` list, the principals /
// groups permitted to cast the approving vote on /v1/vote (rule.go: Rule.ApproveBy, grammar
// `{user:* | group:*}`, design.md). This file resolves a voting actor to a Principal (id + group
// memberships) so approve.go can enforce, fail-closed, that a vote is admitted ONLY WHEN the voter is a
// member of the pending decision's approve_by set (REQ-1516).
//
// SHARED IDENTITY — the human plane, NOT a second store. spec/016 already builds the human → console
// approver identity model (core/credential/plane.go: ApproverIdentity / ApproverSet / ResolveApprovers),
// the LDAP/OIDC-synced approvers that REQ-1516 says feed the policy engine's approve_by. So the LOCAL
// resolver here reads a TG-local operator/group registry AND composes with that ONE shared human plane
// (credential.SyncEngine.ResolveApprovers) — it never forks a parallel approver store. A `group:` entry
// resolves to its members via the human plane exactly as spec/016 intends.
//
// The FEDERATED (LDAP/OIDC) resolver is a DIFFERENT leaf (T-015-10, core/policy/federated.go) that will
// implement this SAME PrincipalResolver interface. This leaf ships the interface + the local impl only.
// ---------------------------------------------------------------------------------------------------------

// Principal is a resolved voting actor: a non-secret id plus the group / role memberships it holds. It is
// pure, loggable data — the audit-safe projection of an actor's identity used to test membership against a
// rule's approve_by set. Groups are normalized (trimmed, case-deduplicated, sorted), consistent with the
// credential human plane's ApproverIdentity.
type Principal struct {
	ID     string   // canonical, non-secret principal id (an operator / user login)
	Groups []string // the principal's group / role memberships (normalized)
}

// PrincipalResolver maps a voting actor id to its Principal (id + groups), or fails. The LOCAL impl
// (LocalResolver) reads the TG-local registry + the credential human plane; the FEDERATED impl (T-015-10)
// resolves through an LDAP / OIDC provider behind this SAME interface. A resolver error MUST be treated as a
// denial by the caller (fail closed) — an identity backend that cannot answer never admits a vote.
type PrincipalResolver interface {
	Resolve(ctx context.Context, actorID string) (Principal, error)
}

// groupMemberResolver is an OPTIONAL capability a PrincipalResolver may implement: report whether an actor is
// a member of a named group via a source the flat Principal.Groups list does not carry. The credential human
// plane is group-keyed (an inverse index: group → members via ResolveApprovers), so membership there is
// tested per approve_by group rather than enumerated onto the Principal. MayApprove consults this for a
// `group:` entry the local registry did not already satisfy. Unexported: only in-package resolvers
// (LocalResolver now, federated later) implement it.
type groupMemberResolver interface {
	groupMember(ctx context.Context, actorID, group string) (bool, error)
}

// LocalPrincipal is one entry in the TG-local operator/group registry: an operator id and the groups it
// belongs to. It is the local-first half of approve_by resolution (the federated half is the human plane).
type LocalPrincipal struct {
	ID     string
	Groups []string
}

// LocalResolver resolves a voting actor against BOTH the TG-local operator/group registry AND the credential
// human plane (the ONE shared approver store). It never forks a second identity store: local group
// membership comes from the registry, and any additional membership comes from credential.SyncEngine via
// ResolveApprovers. The zero LocalResolver resolves every actor to an empty-group Principal (no local
// registry, no human plane) — safe, since an actor that is a member of nothing can approve nothing.
type LocalResolver struct {
	local map[string][]string    // normalized (lower) actor id -> normalized group memberships
	human *credential.SyncEngine // the SHARED spec/016 human plane; nil = local-only (no federated approvers)
}

// NewLocalResolver builds a LocalResolver over the credential human plane (may be nil for local-only) and the
// given TG-local principals. Registry ids and groups are normalized so lookup and comparison are
// case-insensitive and deterministic, matching the human plane.
func NewLocalResolver(human *credential.SyncEngine, principals ...LocalPrincipal) *LocalResolver {
	local := make(map[string][]string, len(principals))
	for _, p := range principals {
		id := strings.ToLower(strings.TrimSpace(p.ID))
		if id == "" {
			continue
		}
		local[id] = normalizeGroupNames(p.Groups)
	}
	return &LocalResolver{local: local, human: human}
}

// Resolve returns the actor's Principal: its id plus its LOCAL group memberships. It fails ONLY on an empty
// actor id (fail closed — an unnamed actor cannot approve). An actor absent from the local registry is NOT an
// error: it resolves to an empty-group Principal, and it may still approve iff it is named directly in
// approve_by OR the human plane places it in an approve_by group (checked in groupMember) — so a human-plane
// approver never known to the local registry still resolves. An actor that is a member of nothing everywhere
// simply matches nothing (cannot approve).
func (r *LocalResolver) Resolve(_ context.Context, actorID string) (Principal, error) {
	id := strings.TrimSpace(actorID)
	if id == "" {
		return Principal{}, fmt.Errorf("policy: cannot resolve an empty actor id (fail closed)")
	}
	var groups []string
	if r != nil && r.local != nil {
		if g, ok := r.local[strings.ToLower(id)]; ok {
			groups = append([]string(nil), g...)
		}
	}
	return Principal{ID: id, Groups: groups}, nil
}

// groupMember reports whether the actor is a member of the named group via the SHARED credential human plane.
// It reuses credential.ResolveApprovers (the spec/016 group → members resolution) — no second grammar, no
// second store. A group the human plane names no member for (or no human plane at all) is a fail-closed
// non-match (false, nil), NOT an error; only an unexpected backend failure returns an error (→ deny).
func (r *LocalResolver) groupMember(_ context.Context, actorID, group string) (bool, error) {
	if r == nil || r.human == nil {
		return false, nil
	}
	set, err := r.human.ResolveApprovers(credential.ApproverQuery{Group: group})
	if err != nil {
		// "no approver resolves for this group" is the human plane's fail-closed sentinel, not a hard failure:
		// this group simply names no member — a non-match, keep checking other approve_by entries.
		if credential.IsRefused(err) {
			return false, nil
		}
		return false, err
	}
	for _, u := range set.Users {
		if strings.EqualFold(strings.TrimSpace(u), strings.TrimSpace(actorID)) {
			return true, nil
		}
	}
	return false, nil
}

// normalizeGroupNames trims, drops empties, case-deduplicates (keeping the first spelling), and sorts group
// names — the SAME normalization behavior the credential human plane applies to an ApproverIdentity's groups
// (plane.go: normalizeGroups), so the two identity layers share one group grammar.
func normalizeGroupNames(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
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
