package httpapi

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"

	"github.com/territory-grounder/grounder/core/auth"
)

// The DEK rewrap surface (TG-163): POST /v1/seal/rewrap {rationale, after, limit}, admin-session-only.
//
// It re-wraps every stored DEK under the CURRENT master-key version so the previous OpenBao Transit key
// version can be retired. Before this existed, `bao transit key rotate` was a one-way door: old ciphertexts
// stayed readable under the old version and nothing ever moved them forward, so raising
// min_decryption_version would have made every sealed credential permanently unopenable.
//
// EXPLICIT, NEVER SCHEDULED. There is no cron behind this route and no "keys are old" banner in front of
// it. Rotation happens when an operator decides it should; this route is only the means.
//
// PATH CHOICE: /v1/seal/rewrap, NOT /v1/secrets/rewrap. The sealed-secret write route is
// /v1/secrets/{name}, and "rewrap" satisfies the secret-name pattern — a static sibling segment would win
// the chi match and quietly make a secret literally named "rewrap" unwritable forever. It also belongs
// here on the merits: this is a key-lifecycle operation, not a secret write. No secret value crosses it in
// either direction.

// SealRewrapRequest is the operator's rewrap order. There is no key material and no secret material in it.
type SealRewrapRequest struct {
	Rationale string `json:"rationale"`
	After     string `json:"after"` // resume just past this name (rows walk in name order)
	Limit     int    `json:"limit"` // 0 = the whole store
}

// SealRewrapOutcome is the value-LESS report: counts, the resume cursor, and the key-version census.
type SealRewrapOutcome struct {
	Scanned   int            `json:"scanned"`
	Rewrapped int            `json:"rewrapped"`
	Skipped   int            `json:"skipped"`
	LastName  string         `json:"last_name"`
	LedgerSeq int64          `json:"ledger_seq"`
	Partial   bool           `json:"partial"`
	Versions  map[string]int `json:"versions"`
	Note      string         `json:"note"`
}

// SealRewrapper runs the rewrap through the worker (the sealed store's single writer). nil = 503.
type SealRewrapper interface {
	RewrapSeals(ctx context.Context, rationale, after string, limit int, operator string) (SealRewrapOutcome, error)
}

// sealRewrapHandler serves POST /v1/seal/rewrap (AuthAdminSession).
func (d Deps) sealRewrapHandler(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if !adminWriteGuard(w, r, d.SealRewrap != nil, "DEK rewrap unavailable — no seal backend is configured", p) {
		return
	}
	var req SealRewrapRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&req); err != nil {
		http.Error(w, "malformed rewrap request", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Rationale) == "" {
		http.Error(w, "rationale required — re-keying the secret store states why", http.StatusBadRequest)
		return
	}
	if req.Limit < 0 {
		http.Error(w, "limit must be >= 0 (0 = the whole store)", http.StatusBadRequest)
		return
	}
	out, err := d.SealRewrap.RewrapSeals(r.Context(), strings.TrimSpace(req.Rationale),
		strings.TrimSpace(req.After), req.Limit, operatorOf(p))
	if err != nil {
		// LOG THE CAUSE, like the secret-write path next door (TG-276's lesson): a rewrap failure names the
		// row that could not be re-keyed, and that row is a pre-existing fault this run DISCOVERED. Losing
		// it to a generic 503 would throw away the most valuable thing the run produced.
		log.Printf("DEK rewrap REFUSED (by %s, after=%q limit=%d): %v", operatorOf(p), req.After, req.Limit, err)
		http.Error(w, "DEK rewrap failed — see the grounder log for the row that could not be re-keyed",
			http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}
