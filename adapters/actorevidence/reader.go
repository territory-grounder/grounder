// Package actorevidence is the adapter seam for per-domain read-only actor-evidence readers (spec/023
// REQ-2306/REQ-2307). Before TG classifies a session, the runner asks "WHO is the actor behind the
// observed change?" — each domain (PVE task log, systemd journal, k8s audit log, NetBox changelog,
// GitOps/MR history, AWX job history, docker events) answers through ONE shared read-only interface, so
// the attributor (core/attribution) derives the taxonomy from typed records, never from model narrative.
//
// The seam's safety properties are structural: a reader is read-only by construction (a mutating reader
// is a compile-time contradiction of the seam), evidence collection is ADVISORY and fails OPEN (a dead
// reader degrades the session to the pre-feature ladder — REQ-2303/REQ-2307), and every reader
// authenticates with a least-privilege READ-ONLY credential resolved as a core/config.SecretRef
// (INV-13) — never the platform's own actuation write token.
package actorevidence

import (
	"context"
	"time"

	"github.com/territory-grounder/grounder/core/attribution"
)

// Reader is the one shared per-domain actor-evidence interface. Implementations live under
// modules/actorevidence/<domain>/ and are compiled in, config-gated, and explicitly registered at
// startup (INV-17/INV-18 — an unregistered or disabled domain has no execution path and yields
// `unattributable` for its targets).
type Reader interface {
	// Domain returns the domain slug this reader covers ("pve", "journal", "k8s-audit", "netbox",
	// "gitops-mr", "awx", "docker"). It keys the sanctioned-principal and self-identity config.
	Domain() string
	// ReadOnly is always true — the seam is read-only by construction; a mutating reader cannot satisfy it.
	ReadOnly() bool
	// Read returns the actor-evidence records for target within [since, until]. Errors are ADVISORY
	// (REQ-2307): the attributor logs them, records a warning in the session signals, and treats the
	// domain's evidence as absent — a reader failure never fails or blocks the session and never by
	// itself produces attributed-suspicious. Every Read runs under a bounded per-reader timeout
	// (config, with a compiled ceiling) so a hung audit endpoint cannot stall triage.
	Read(ctx context.Context, target string, since, until time.Time) ([]attribution.Evidence, error)
}
