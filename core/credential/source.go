package credential

import "context"

// ---------------------------------------------------------------------------------------------------------
// UNIFIED CREDENTIAL-SOURCE FRAMEWORK (spec/016 task T-016-7, REQ-1607/1608/1609/1610/1615).
//
// A CredentialSource pulls credential entries (a Selector over the shared estate object-model → a resolved
// Bundle) from some external system-of-record (AWX / Ansible / Semaphore / OpenBao / Vault on the machine
// plane; LDAP / OIDC on the human plane) into TG's native store, READ-ONLY. The concrete connectors are
// SEPARATE later slices (T-016-8/9/10, modules/credsource/*); this file defines only the INTERFACE and the
// pulled-entry type the orchestrator (sync.go) drives, so the framework is fully testable against an
// in-memory fake source with no third-party dependency.
//
// A synced entry carries only SecretRef references (never a plaintext credential, INV-13/REQ-1603) and its
// Bundle is the SAME Bundle the native rules use — so a synced entry feeds the SAME resolver (REQ-1605).
// ---------------------------------------------------------------------------------------------------------

// Plane is the single identity plane a source feeds (REQ-1611). PlaneMachine is the machine → host
// credential plane (how TG logs in to a target); PlaneHuman is the human → console approver plane (who may
// approve). Each source declares exactly one plane and the sync framework carries it onto every SyncRun.
//
// NOTE: this file defines the plane LABEL the framework needs to route and record. The two-plane ROUTER
// that ENFORCES "a machine-plane source may not populate an approver identity and an approver-plane source
// may not populate a host bundle" (REQ-1611) lives in the sibling file plane.go (T-016-6): its
// validateEntryPlane gates every synced entry, and its ResolveApprovers serves the human-plane resolution.
type Plane string

const (
	// PlaneMachine is the machine → host credential plane (AWX/Ansible/Semaphore/OpenBao/Vault + native).
	PlaneMachine Plane = "machine"
	// PlaneHuman is the human → console approver plane (LDAP/OIDC), feeding spec/015 approve_by.
	PlaneHuman Plane = "human"
)

func (p Plane) valid() bool { return p == PlaneMachine || p == PlaneHuman }

// SourceEntry is one entry pulled from an external source, keyed within its source by a stable NativeID. The
// (source_id, NativeID) pair is the idempotency key the orchestrator upserts on (REQ-1608).
//
// An entry carries the payload for its source's ONE plane (REQ-1611), and the two-plane router
// (validateEntryPlane, plane.go) refuses any entry that carries the other plane's payload:
//
//   - A machine-plane (PlaneMachine) entry carries a Selector over the shared estate object-model (REQ-1605)
//     bound to a resolved-identity Bundle, and leaves Approver zero. The Bundle carries SecretRefs only — a
//     source stores a REFERENCE (an AWX credential id, a Vault path), never a secret value (INV-13).
//   - A human-plane (PlaneHuman) entry carries an Approver identity (users / groups for spec/015 approve_by)
//     and leaves Bundle (and Selector) zero. An ApproverIdentity holds NO secret-bearing field.
//
// An empty NativeID, a machine entry with no valid Bundle, or a human entry with no valid Approver is
// rejected at sync time (fail closed): a source may never inject a blank identity, and it may never write
// across its plane (a machine entry carrying an approver, or a human entry carrying a bundle, is refused).
type SourceEntry struct {
	// NativeID is the entry's stable id in the source's system-of-record — the (source_id, native_object_id)
	// key half that makes a re-sync idempotent. Two entries with the same NativeID collapse to one (no
	// duplication); an entry present last sync but absent now is removed locally (no orphan).
	NativeID string
	// Selector matches the target this entry authenticates, over the shared object-model (machine plane).
	Selector Selector
	// Bundle is the resolved machine → host identity (SecretRefs only). Set on the machine plane; zero on the
	// human plane.
	Bundle Bundle
	// Approver is the resolved human → console approver identity (users / groups). Set on the human plane;
	// zero on the machine plane. It carries no secret material (see ApproverIdentity, plane.go).
	Approver ApproverIdentity
}

// CredentialSource is a read-only importer of credential/identity entries from an external platform into
// TG's native store (REQ-1607). Sync is the operation an operator schedule (a Temporal Schedule) invokes on
// a cadence AND that the console invokes on demand ("Sync now") — the framework treats both identically.
//
// The contract is READ-ONLY and re-read-by-id (INV-05): each Sync re-reads the CURRENT canonical entry set
// from the source's system-of-record by id rather than returning a cached mutable copy, and MUST NOT mutate
// the upstream platform. The orchestrator (SyncEngine, sync.go) diffs the returned set against the prior
// converged state to compute drift and converge idempotently, so a source implements only the pull.
type CredentialSource interface {
	// ID is the stable operator-declared source id — the (source_id, …) key half and the audit/provenance
	// label recorded as the winning/shadowed source on a resolution.
	ID() string
	// Plane is the single identity plane this source feeds (REQ-1611). It is fixed for the source's lifetime.
	Plane() Plane
	// Sync performs the READ-ONLY pull: it re-reads the source's CURRENT full credential-entry set from its
	// system-of-record by id and returns it. It returns an error (and the framework fails closed, leaving the
	// prior converged state intact) when the backend is unreachable, the read is denied, or an entry is
	// malformed. It never returns a partial-but-nil-error set that would orphan real entries.
	Sync(ctx context.Context) ([]SourceEntry, error)
}
