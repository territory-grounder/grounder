// Package nativedb is the operator-authored NATIVE per-target credential mapping as a first-class
// machine-plane credential.CredentialSource riding the EXISTING sync framework (spec/016 REQ-1607/
// REQ-1610, TG-109). Its rows live in the database (credential_native_rule, migration 0088) and are
// written ONLY through the ledgered single-writer worker lane (temporal/nativerule) — so an operator
// adds or removes a per-target rule from the console with NO worker restart and NO boot risk, which is
// what the env-spec native fallback (TG_CREDENTIAL_NATIVE_RULES) could never offer: an env edit is a
// redeploy, and a typo in it fails the next BOOT rather than the write that introduced it.
//
// INV-17 still holds: the adapter is COMPILED IN and REGISTERED AT STARTUP — what is late-bound is only
// the ROW LOADER (the DB pool exists after the source registry is built), behind the same TG-451 atomic
// handoff pattern as cmd/worker/estate_loaders.go: Bind Stores the loader, Sync Loads it across a memory
// barrier, and until bind lands a Sync fails closed with the same "pool not yet connected" error those
// loaders return (prior converged state retained — never an empty set that would orphan real entries).
//
// Each row is ONE packed ParseRules rule (kind:pattern|user|port|scheme[|refs…]). Sync re-parses every
// row (INV-05 re-read-by-id; the grammar is the ONE native grammar, never a second parser) and fails the
// WHOLE sync naming the offending row id when any row does not parse to exactly one rule — the
// CredentialSource contract (core/credential/source.go): an error retains the prior converged state, and
// a partial set is never returned. Every bundle carries SecretRef REFERENCES only (INV-13), enforced by
// NewBundle inside ParseRules.
//
// Provenance: [R] spec/016 REQ-1607 (sources ride one framework) / REQ-1610 (native store) · [O] INV-13,
// INV-17, INV-05 · mirrors modules/credsource/native (the env-spec hostdiag fallback) + the TG-451
// atomic-handoff loaders.
package nativedb

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/territory-grounder/grounder/core/credential"
)

// ID is the stable source id this adapter registers under — the (source_id, native_id) key half and the
// provenance label on every resolution it wins.
const ID = "native-db"

// RuleRow is one operator-authored rule row as this source needs it: the row id (the stable native id)
// and the packed entry. Defined LOCALLY so the module depends on core/credential only — the composition
// root adapts core/db.NativeRuleRow to this shape at Bind time, exactly like the estate loaders.
type RuleRow struct {
	ID    int64
	Entry string
}

// loadFunc is the late-bound row loader: the CURRENT full rule set, re-read from the system of record on
// every Sync (INV-05).
type loadFunc func(ctx context.Context) ([]RuleRow, error)

// Source is the DB-backed native machine-plane credential source. Construct with New at startup (INV-17);
// Bind the row loader once the pool connects. Sync fails closed until then.
type Source struct {
	load atomic.Pointer[loadFunc]
}

// New builds the source, unbound. Registration does not need the loader — only Sync does, and an unbound
// Sync fails closed.
func New() *Source { return &Source{} }

// Bind installs the row loader on the post-connect path (the TG-451 atomic handoff — safe to call
// concurrently with Sync).
func (s *Source) Bind(fn loadFunc) { s.load.Store(&fn) }

// ID implements credential.CredentialSource.
func (s *Source) ID() string { return ID }

// Plane implements credential.CredentialSource — operator-authored per-target rules feed the machine →
// host plane.
func (s *Source) Plane() credential.Plane { return credential.PlaneMachine }

// Sync implements credential.CredentialSource: re-read EVERY rule row from the database and map each to
// one machine-plane SourceEntry. Fail-closed on every edge, per the contract in core/credential/source.go:
//   - loader not yet bound (the pool has not connected) ⇒ error, prior converged state retained;
//   - the read fails ⇒ error, prior state retained;
//   - ANY row that errors under ParseRules or parses to anything but exactly ONE rule ⇒ the WHOLE sync
//     errors NAMING the row id — never a partial set that would silently orphan the good rows' neighbours.
//
// The one-row-one-rule shape is written in at the write lane (temporal/nativerule) — re-checked here
// because the sync must hold its own contract even against a hand-edited table.
func (s *Source) Sync(ctx context.Context) ([]credential.SourceEntry, error) {
	fn := s.load.Load()
	if fn == nil {
		return nil, errors.New("nativedb: database pool not yet connected — rule rows unreadable (prior state retained)")
	}
	rows, err := (*fn)(ctx)
	if err != nil {
		return nil, fmt.Errorf("nativedb: read rule rows: %w", err)
	}
	entries := make([]credential.SourceEntry, 0, len(rows))
	for _, row := range rows {
		rules, perr := credential.ParseRules(row.Entry)
		if perr != nil {
			return nil, fmt.Errorf("nativedb: rule row %d does not parse (sync refused, prior state retained): %w", row.ID, perr)
		}
		if len(rules) != 1 {
			return nil, fmt.Errorf("nativedb: rule row %d packs %d rules — one row, one rule (sync refused, prior state retained)", row.ID, len(rules))
		}
		entries = append(entries, credential.SourceEntry{
			// The row id is the stable native id, so a re-sync is idempotent and a resolution's provenance
			// names the exact row an operator can delete.
			NativeID: fmt.Sprintf("row-%d", row.ID),
			Selector: rules[0].Selector,
			Bundle:   rules[0].Bundle,
		})
	}
	return entries, nil
}

// compile-time proof Source satisfies the stable credential-source interface.
var _ credential.CredentialSource = (*Source)(nil)
