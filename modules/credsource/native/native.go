// Package native is the NATIVE, standalone machine-plane credential source (spec/016, REQ-1600/1607/1610):
// it exposes the operator's read-only hostdiag SSH allowlist — the `site|hostglob|sshuser|keyref` rows in
// TG_HOSTDIAG_DEPLOYMENTS — as a credential.CredentialSource, so per-host SSH identity resolves THROUGH the
// credential engine instead of being read straight off the allowlist at the call site.
//
// It is the config-not-code native store the design's resolution procedure step 2 falls back to ("WHEN
// nothing is synced, fall back to the native store"): registered at the LOWEST precedence, any real synced
// system-of-record (OpenBao/Vault, AWX, Semaphore) shadows it, but with no external source configured it is
// the always-available baseline that resolves exactly what hostdiag reaches today.
//
// It reuses the SAME parse as hostdiag — hostdiag.ParseAccess — so there is ONE allowlist grammar, never a
// second. Each allowlist row becomes one machine-plane SourceEntry: selector = the host-glob, Bundle =
// {User: sshuser, Port: 22, Scheme: ssh, SSHKeyRef: <keyref>} carrying a SecretRef REFERENCE only (INV-13),
// never key material. A row that cannot build a valid, sealed-reference bundle is SKIPPED-with-record (never
// injected as a blank identity) — fail closed at the entry level.
//
// It is distroless-native (no subprocess, no network — a pure in-memory read of operator config) and its
// Sync is trivially idempotent: the same allowlist re-syncs to the same entry set with no drift.
//
// Provenance: [F] generalizes the hostdiag read-only SSH access allowlist into an engine source ·
// [R] paradigm-rule 4 (config-not-code) · [O] INV-13/INV-17, spec/016 REQ-1600/1607/1610.
package native

import (
	"context"
	"fmt"
	"strings"

	"github.com/territory-grounder/grounder/core/credential"
	"github.com/territory-grounder/grounder/modules/observability/hostdiag"
)

// DefaultID is the source id the native hostdiag-backed source registers under when none is declared.
const DefaultID = "native-hostdiag"

// sshPort is the fixed SSH port the hostdiag read-only path connects on (the syslog-ng native runner the
// hostdiag tools reuse dials port 22). The allowlist grammar carries no port, so every native bundle is
// port 22 — the exact port the current hostdiag read uses, so wiring changes no behavior.
const sshPort = 22

// SkipRecord is one allowlist row the most recent Sync could not turn into a valid bundle (coverage
// observability), with the non-secret reason. A skip is never a whole-Sync failure — a bad row simply grants
// no identity (fail closed) while the good rows still resolve.
type SkipRecord struct {
	HostGlob string
	Reason   string
}

// Source is the native machine-plane credential source over a parsed hostdiag allowlist. Construct with New
// or NewFromSpec. It holds only the parsed rows (operator config); it is immutable after construction and its
// Sync touches no network.
type Source struct {
	id      string
	access  []hostdiag.Access
	skipped []SkipRecord
}

// Config configures a Source. ID defaults to DefaultID. Access is the parsed hostdiag allowlist (share the
// hostdiag parse — pass hostdiag.ParseAccess output, or use NewFromSpec).
type Config struct {
	ID     string
	Access []hostdiag.Access
}

// New builds a Source from already-parsed hostdiag Access rows. It fails closed on no rows — an empty
// allowlist yields no source (the caller simply does not register it), never a source that refuses everything
// silently.
func New(cfg Config) (*Source, error) {
	if len(cfg.Access) == 0 {
		return nil, fmt.Errorf("native: source requires at least one hostdiag access rule")
	}
	id := strings.TrimSpace(cfg.ID)
	if id == "" {
		id = DefaultID
	}
	return &Source{id: id, access: cfg.Access}, nil
}

// NewFromSpec builds a Source directly from a raw TG_HOSTDIAG_DEPLOYMENTS spec, using hostdiag.ParseAccess so
// the native source and the hostdiag investigation tools share ONE allowlist grammar (no second parser). An
// empty/whitespace spec (or a spec with no well-formed rows) returns (nil, nil): there is nothing to serve,
// and the caller simply does not register a source.
func NewFromSpec(id, spec string) (*Source, error) {
	access := hostdiag.ParseAccess(spec)
	if len(access) == 0 {
		return nil, nil
	}
	return New(Config{ID: id, Access: access})
}

// ID implements credential.CredentialSource.
func (s *Source) ID() string { return s.id }

// Plane implements credential.CredentialSource — the native hostdiag store feeds the machine → host plane.
func (s *Source) Plane() credential.Plane { return credential.PlaneMachine }

// Skipped returns the rows the most recent Sync could not map to a valid bundle (a copy).
func (s *Source) Skipped() []SkipRecord {
	out := make([]SkipRecord, len(s.skipped))
	copy(out, s.skipped)
	return out
}

// Sync implements credential.CredentialSource: it maps each hostdiag allowlist row to one machine-plane
// SourceEntry with a SecretRef-only Bundle. It is a pure, read-only, no-network read of operator config, so
// it never returns an error (an unreachable/denied external backend is the failure mode of the connector
// sources, not of the native store). A row whose bundle cannot be built (e.g. a non-sealed key reference) is
// SKIPPED-with-record — never injected as a blank identity (fail closed) — while every good row still
// resolves. The (source_id, NativeID) key is stable per row so a re-sync is idempotent (no drift).
func (s *Source) Sync(_ context.Context) ([]credential.SourceEntry, error) {
	s.skipped = nil
	entries := make([]credential.SourceEntry, 0, len(s.access))
	for i, a := range s.access {
		b, err := credential.NewBundle(credential.BundleSpec{
			User:      a.SSHUser,
			Port:      sshPort,
			Scheme:    credential.SchemeSSH,
			SSHKeyRef: a.KeyRef, // a REFERENCE (env:/file:/store:), enforced sealed by NewBundle — never key material
		})
		if err != nil {
			// A row that cannot build a valid, sealed-reference bundle grants no identity (fail closed): record
			// the non-secret reason and skip it, never inject a blank/partial identity into the resolver.
			s.skipped = append(s.skipped, SkipRecord{HostGlob: a.HostGlob, Reason: err.Error()})
			continue
		}
		entries = append(entries, credential.SourceEntry{
			// A stable, unique key per row: two rows sharing a glob stay DISTINCT entries so the engine's
			// equal-specificity check fails closed on the genuine ambiguity rather than one silently collapsing
			// the other.
			NativeID: fmt.Sprintf("hostdiag:%d:%s", i, a.HostGlob),
			Selector: credential.Selector{Kind: credential.KindHostGlob, Pattern: a.HostGlob},
			Bundle:   b,
		})
	}
	return entries, nil
}

// compile-time proof Source satisfies the stable credential-source interface.
var _ credential.CredentialSource = (*Source)(nil)
