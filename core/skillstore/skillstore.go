// Package skillstore is the versioned skill store: the rows the console edits, the flywheel graduates,
// and seed composition reads (spec/014). It ports the predecessor's prompt-patch-trial machinery onto
// skills while designing out its verified production failures — every status is CHECK-constrained and
// every transition passes through the single Transition state machine (its trial table accumulated
// out-of-enum rows via raw SQL), rationale is mandatory at creation and at every transition (its
// rationale lived in a sidecar file that split-brained), and one-production / one-active-trial are
// structural partial unique indexes (its supersede logic drifted in application code).
//
// The store changes agent COMPETENCE only. Enforcement — bands, the never-auto floor, the prediction
// gate, the actuation interceptor — is machine-checked outside the seed (spec/001/002/013) and is not
// reachable from here; a pinned skill's compiled body additionally can never be overridden (REQ-1305).
// INV-08 holds throughout: selection predicates are declarative, validated, and evaluated in Go; no
// model token becomes control flow.
package skillstore

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"
)

// Status is a skill version's lifecycle state (REQ-1301). The vocabulary is mirrored by the database
// CHECK constraint, so an out-of-band writer cannot mint a state the code does not know.
type Status string

const (
	StatusDraft      Status = "draft"
	StatusTrial      Status = "trial"
	StatusProduction Status = "production"
	StatusRetired    Status = "retired"
	StatusRejected   Status = "rejected"
)

// AppliesWhen is the declarative selection predicate stored on a version row: phase and execution-class
// membership, validated at write time (REQ-1303). Empty slices mean "always" — mirroring the compiled
// registry's `always` selector. It is deliberately NOT a program: the vocabulary is closed, evaluation
// is a pure membership test in Go, and no model token participates (INV-08).
type AppliesWhen struct {
	Phases      []string `json:"phases,omitempty"`
	ExecClasses []string `json:"exec_classes,omitempty"`
}

// Version is one row of skill_version.
type Version struct {
	ID              int64
	SkillName       string
	Version         string
	Status          Status
	Body            string
	AppliesWhen     AppliesWhen
	ContentHash     string
	Author          string
	Source          string
	Rationale       string // the append-only transition log
	OfflineEval     []byte // the offline admission run (REQ-1307), stored pass or fail
	ParentVersionID int64
	LedgerSeq       int64
	CreatedAt       time.Time
	StatusChangedAt time.Time
}

// Skill is one row of skill (the identity the versions hang off).
type Skill struct {
	Name     string
	Kind     string
	Pinned   bool
	Position int
}

// ProductionRow is one production version joined with its skill identity — the composer's snapshot unit
// (REQ-1303): everything seed composition needs in one read.
type ProductionRow struct {
	// VersionID is the skill_version row id (0 when the row does not come from the store, e.g. a
	// compiled body). It rides into the per-session skill_load provenance so the judge spine can bind
	// a judged session to the exact graduated version the regression watch tracks (REQ-1310).
	VersionID   int64
	SkillName   string
	Version     string
	Body        string
	AppliesWhen AppliesWhen
	ContentHash string
	Pinned      bool
	Position    int
}

// ContentHash is the stale-audit anchor: sha256 over the body and the predicate, so any change to
// either is structurally visible (no git archaeology).
func ContentHash(body string, aw AppliesWhen) string {
	h := sha256.New()
	h.Write([]byte(body))
	h.Write([]byte{0})
	for _, p := range aw.Phases {
		h.Write([]byte(p))
		h.Write([]byte{1})
	}
	for _, c := range aw.ExecClasses {
		h.Write([]byte(c))
		h.Write([]byte{2})
	}
	return hex.EncodeToString(h.Sum(nil))
}

var (
	// ErrRationaleRequired fails a creation or transition that states no reason (REQ-1301).
	ErrRationaleRequired = errors.New("skillstore: a rationale is required")
	// ErrPinnedSkill refuses a store row targeting a pinned skill (REQ-1305).
	ErrPinnedSkill = errors.New("skillstore: skill is pinned — the compiled body cannot be overridden")
	// ErrBadTransition refuses a status move the state machine does not allow (REQ-1301).
	ErrBadTransition = errors.New("skillstore: status transition not allowed")
	// ErrBadPredicate refuses an applies-when outside the closed vocabulary (REQ-1303).
	ErrBadPredicate = errors.New("skillstore: applies_when references an unknown phase or execution class")
	// ErrBodyBounds refuses an empty or oversized body (REQ-1301; the 8 KiB cap is the schema's too).
	ErrBodyBounds = errors.New("skillstore: body must be 1..8192 bytes")
	// ErrProductionExists surfaces the one-production partial index refusing a concurrent double
	// graduation (REQ-1302) — the loser of the race gets the invariant by name, not a raw SQL error.
	ErrProductionExists = errors.New("skillstore: skill already has a production version")
)
