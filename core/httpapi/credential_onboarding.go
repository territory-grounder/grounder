package httpapi

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/territory-grounder/grounder/core/auth"
)

// CredentialBindingDTO is one row of the credential-onboarding screen (TG-274): an inventory source's
// named credential, what it governs, and whether TG can use it.
//
// REFERENCES ONLY, NEVER MATERIAL (INV-13). Ref is a SecretRef string like "bao:secret/data/tg/onekey#key";
// the key itself never crosses this boundary, which is the same contract the module dialogs hold.
type CredentialBindingDTO struct {
	Source string `json:"source"` // the inventory source that reported it (e.g. "awx")
	Name   string `json:"name"`   // the credential's name AS THE SOURCE SPELLS IT — the exact
	// string the operator must map, so it can be copied not guessed
	Scope  string `json:"scope"`         // the inventory/group it governs
	Via    string `json:"via,omitempty"` // what binds them (an AWX job template) — where to change it
	Hosts  int    `json:"hosts"`         // blast radius of supplying this key, and cost of not
	Mapped bool   `json:"mapped"`        // TG holds a SecretRef for this name
	Usable bool   `json:"usable"`        // mapped AND the ref is non-blank
	Ref    string `json:"ref,omitempty"` // the SecretRef, never the secret
}

// CredentialOnboarding is what the first screen of credential setup renders.
//
// ★ THE UNMAPPED COUNT IS THE POINT. An onboarding surface that listed only credentials TG can already use
// would answer "everything I can see works" while blind to the rest of the fleet. Measured on this estate
// 2026-08-04: AWX holds 11 Machine credentials and TG maps ONE — and nothing anywhere said so, because the
// connector computed exactly this and discarded it (awx.Source.Skipped() was never called by any non-test
// code). Unmapped is not an error state; it is the work list.
type CredentialOnboarding struct {
	Bindings []CredentialBindingDTO `json:"bindings"`
	Total    int                    `json:"total"`
	Unmapped int                    `json:"unmapped"`
	// Sources names the inventory sources that reported bindings. Empty with Total 0 means NOTHING reported
	// — which is a different fact from "everything is mapped" and must not render as success.
	Sources []string `json:"sources"`
}

// CredentialOnboardingReader reports the credential→scope bindings the inventory sources discovered.
type CredentialOnboardingReader interface {
	CredentialOnboarding(ctx context.Context, p auth.Principal) (CredentialOnboarding, error)
}

// credentialOnboardingHandler serves GET /v1/credentials/onboarding.
//
// AuthTraceRead (TG-294), the ELEVATED read tier — raised from AuthReadOnly, where it shipped. The original
// argument for read-only was that the response carries no secret value, only names, scopes, counts and
// SecretRef strings, exactly like /v1/credentials/sources. That argument answered the wrong question: what
// makes this surface sensitive is not what it leaks but what it RANKS. Unmapped ∪ hosts-descending — the
// pairing migration 0054 built an index for — is a target list ordered by blast radius, which the sibling
// history reads are not. See the tier rationale on the route in router.go; supplying the material is still a
// separate, admin-gated write (POST /v1/modules/{surface}/{source}/secret).
func (d Deps) credentialOnboardingHandler(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if d.CredentialOnboardingRead == nil {
		// A deployment with no inventory source wired says so, rather than returning an empty list that
		// reads as "nothing to configure".
		http.Error(w, "credential onboarding unavailable (no inventory source is wired)", http.StatusServiceUnavailable)
		return
	}
	out, err := d.CredentialOnboardingRead.CredentialOnboarding(r.Context(), p)
	if err != nil {
		http.Error(w, "credential onboarding unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}
