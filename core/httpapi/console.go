package httpapi

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/auth"
)

// This file adds the read-only data endpoint the operator console (spec/010) consumes beyond /v1/stats:
// the governance ledger. It is a pure read over the immutable, hash-chained audit spine (INV-19) — no
// mutation, no side effects — authority-checked against the caller (INV-12). It stays behind an interface
// (like StatsReader/SnapshotStore) so the handler is oracle-testable with an in-memory fake; the durable
// pgx-backed adapter lives at the composition root (cmd/grounder).

// ledgerPageLimit bounds a single ledger read so the console pages the tail of the chain rather than
// pulling an unbounded history in one response.
const ledgerPageLimit = 200

// LedgerReader returns the most recent governance-ledger entries the principal is authorized to see. It is
// the read side of the append-only, hash-chained spine (INV-19); it never mutates the chain. Authority is
// resolved inside the implementation against the principal, never inferred from a request field (INV-12).
type LedgerReader interface {
	Recent(ctx context.Context, p auth.Principal, limit int) ([]audit.LedgerEntry, error)
}

// LedgerPage is the read-only ledger view the console renders. Entries are ordered oldest→newest as the
// chain was written; the console re-verifies prev_hash/hash linkage client-side against this same order.
type LedgerPage struct {
	Entries []audit.LedgerEntry `json:"entries"`
}

// WhoAmI is the authenticated caller's identity and the control-plane posture it sees. mutation_enabled is
// the WORKER's published live posture (read through StatsReader), never computed here; posture_stale +
// posture_source carry whether that posture is fresh, so the console can flag "unknown" instead of a
// confident OFF the read-only grounder cannot vouch for.
type WhoAmI struct {
	Source          string `json:"source"`
	MutationEnabled bool   `json:"mutation_enabled"`
	PostureStale    bool   `json:"posture_stale"`
	PostureSource   string `json:"posture_source"`
}

// whoamiHandler echoes the authenticated principal's source id and the live mutation posture. It was
// previously registered inline in cmd/grounder — served but ABSENT from the generated contract, so the
// served surface drifted from the contract that gencontracts derives from httpapi.Register (INV-15). Homing
// it here closes that drift: the route is now generated into the contract like every other. A nil StatsReader
// reports mutation OFF flagged posture-unknown (the fail-closed default), never panics.
func (d Deps) whoamiHandler(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	who := WhoAmI{Source: p.SourceID, PostureStale: true, PostureSource: "unknown"}
	if d.Stats != nil {
		if s, err := d.Stats.Stats(r.Context(), p); err == nil {
			who.MutationEnabled = s.MutationEnabled
			who.PostureStale = s.PostureStale
			who.PostureSource = s.PostureSource
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(who)
}

// ledgerHandler serves the recent governance ledger. It runs only after core/auth has authenticated the
// caller. A nil reader (no durable ledger wired) fails closed to 503 rather than nil-dereferencing — the
// console then holds its restrictive guarded state instead of rendering fabricated rows.
func (d Deps) ledgerHandler(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if d.Ledger == nil {
		http.Error(w, "ledger unavailable", http.StatusServiceUnavailable)
		return
	}
	entries, err := d.Ledger.Recent(r.Context(), p, ledgerPageLimit)
	if err != nil {
		http.Error(w, "ledger unavailable", http.StatusServiceUnavailable)
		return
	}
	if entries == nil {
		entries = []audit.LedgerEntry{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(LedgerPage{Entries: entries})
}
