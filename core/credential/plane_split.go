// plane_split.go — the read/actuation credential-plane split (spec/022 REQ-2203, TG-157/TG-153 High#3).
//
// REQ-2203: the read-only triage plane SHALL NOT co-hold the actuation SSH identity or any mutation
// write-token; a compromise of the triage plane SHALL NOT yield an actuation-capable credential, so the read
// and mutate blast-radii are disjoint. The primary enforcement is at the substrate: the triage worker and the
// actuation worker run under DISTINCT OpenBao AppRoles whose policies do not overlap (design.md §3). This file
// is the in-process defense-in-depth beneath that: it classifies the credential REFERENCES the process holds
// into the two planes and asserts, at boot, that no reference appears in both — so a configuration mistake that
// pointed a triage source at the actuation key (or handed the triage token the actuation write scope) fails
// closed at startup rather than silently collapsing the split. References are safe to name (INV-13: they are
// not the secret), so a violation is reported by ref, never by value.
package credential

import (
	"fmt"

	"github.com/territory-grounder/grounder/core/config"
)

// CredentialClass is the plane a credential belongs to.
type CredentialClass int

const (
	// ClassReadTriage credentials answer read-only triage/investigation (estate reads, host-log reads, the
	// read-scoped substrate token). A compromise here can read what it triages and nothing more.
	ClassReadTriage CredentialClass = iota
	// ClassActuation credentials can MUTATE the estate (the actuation SSH identity, the proxmox/AWX/k8s
	// write-tokens). These must never be reachable from the triage plane.
	ClassActuation
)

func (c CredentialClass) String() string {
	if c == ClassActuation {
		return "actuation"
	}
	return "read-triage"
}

// PlaneSet declares the credential REFERENCES (never values) the process is configured to hold, partitioned by
// plane. Empty references are ignored (an unconfigured source contributes nothing).
type PlaneSet struct {
	ReadTriage []config.SecretRef // syslog/host-diag read keys, estate read tokens, the read-scoped substrate token
	Actuation  []config.SecretRef // the SSH mutate identity, plus the proxmox, AWX and k8s write tokens
}

// Validate enforces REQ-2203: no reference may serve BOTH planes. It returns the first crossing reference (safe
// to name) so the caller fails closed at boot. A reference of "" (unset) never crosses.
func (p PlaneSet) Validate() error {
	read := make(map[config.SecretRef]bool, len(p.ReadTriage))
	for _, r := range p.ReadTriage {
		if r != "" {
			read[r] = true
		}
	}
	for _, r := range p.Actuation {
		if r != "" && read[r] {
			return fmt.Errorf("credential plane split (REQ-2203): reference %q is configured for BOTH the read-triage and actuation planes — the triage plane must never co-hold an actuation credential; give each plane a distinct reference (and a distinct OpenBao role)", string(r))
		}
	}
	return nil
}

// Summary is a value-less description for the boot log: how many references each plane holds.
func (p PlaneSet) Summary() string {
	nz := func(rs []config.SecretRef) int {
		n := 0
		for _, r := range rs {
			if r != "" {
				n++
			}
		}
		return n
	}
	return fmt.Sprintf("plane split OK: %d read-triage ref(s) disjoint from %d actuation ref(s)", nz(p.ReadTriage), nz(p.Actuation))
}
