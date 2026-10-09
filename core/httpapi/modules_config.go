package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/territory-grounder/grounder/core/auth"
)

// THE MODULE CONFIGURATION SURFACE — what the console's per-module dialog reads and writes.
//
// Three routes, three trust levels, and the split is the point:
//
//	GET  /v1/modules/schema          read-only  — the generated form's SHAPE, no values
//	POST /v1/modules/{s}/{t}/test    admin      — exercise the real module
//	POST /v1/modules/{s}/{t}/secret  admin      — store a credential, write-only
//
// The schema route returns no values at all, not even non-secret ones: the console already reads current
// values with their provenance from GET /v1/config, and duplicating them here would create a second
// answer to "what is configured" that can disagree with the first. This surface answers "what CAN be
// configured".

// ModuleFieldDTO is one field of a generated dialog. It describes the input; it never carries a value.
type ModuleFieldDTO struct {
	Name      string `json:"name"`
	EnvKey    string `json:"env_key,omitempty"`
	ConfigKey string `json:"config_key,omitempty"`
	Label     string `json:"label"`
	Help      string `json:"help,omitempty"`
	Type      string `json:"type"`
	Security  string `json:"security"`
	// Effect tells the operator whether a save takes effect now or at the next restart. A dialog that
	// cannot say this truthfully produces a Save button that silently does nothing until a redeploy.
	Effect   string `json:"effect"`
	Required bool   `json:"required"`
	Pattern  string `json:"pattern,omitempty"`
	MaxItems int    `json:"max_items,omitempty"`
	MaxLen   int    `json:"max_len,omitempty"`
}

// ModuleSchemaDTO is one module's dialog.
type ModuleSchemaDTO struct {
	Surface    string           `json:"surface"`
	SourceType string           `json:"source_type"`
	Title      string           `json:"title"`
	Summary    string           `json:"summary,omitempty"`
	Fields     []ModuleFieldDTO `json:"fields"`
	// HasSecret marks a module with a credential lane, so the dialog renders a write-only field.
	HasSecret bool `json:"has_secret"`
	// TestVerb is what pressing Test will actually do, in the operator's words. Shown BEFORE they press
	// it: "post a test message to the approvals room" is consent; "Test" alone is not.
	TestVerb string `json:"test_verb,omitempty"`
	// Enabled reports whether this module is currently configured and registered — and is meaningful ONLY
	// when EnabledKnown is true.
	//
	// THIS PROCESS CANNOT SEE MOST MODULES. The API process registers ingest receivers and declares the
	// model/actuation/observability surfaces; the notifiers, trackers, cmdb, credsource, discovery and
	// knowledge connectors live in the WORKER, which has no channel to publish its registry here. A plain
	// bool therefore reported `false` for every one of them — indistinguishable from "the operator turned
	// it off", and demonstrably wrong for a Matrix notifier that was delivering governance polls at the
	// time. With one descriptor that was a single bad row; across the connector fleet it is a page
	// asserting that almost nothing works.
	//
	// So absence of knowledge is now carried explicitly rather than collapsed into "off". See TG-251 for
	// the real fix: a worker-published capability projection the API can read.
	Enabled bool `json:"enabled"`
	// EnabledKnown reports whether this process can observe the module's state at all. False means the
	// console must say "not reported by this process", never "disabled".
	EnabledKnown bool `json:"enabled_known"`
}

// ModuleUndescribedDTO is one module package with no dialog, AND WHY.
//
// The reason is not decoration. This surface first carried a bare []string, and a bare name cannot
// distinguish "nobody has written this dialog yet" from "this module has nothing to configure" — so the
// console rendered a finished set of connectors as permanently unfinished work, with no way for an
// operator to tell which entries would ever move.
type ModuleUndescribedDTO struct {
	Package string `json:"package"`
	Reason  string `json:"reason,omitempty"`
}

// ModuleSchemaPage is the schema surface's payload.
type ModuleSchemaPage struct {
	Modules []ModuleSchemaDTO `json:"modules"`
	// Undescribed names module packages that publish NO schema and therefore get no dialog. Reported
	// rather than omitted: a console that silently lists only the described modules cannot distinguish
	// "this is everything" from "this is what we happen to have written a form for", and that distinction
	// is the whole reason the list is explicit.
	Undescribed []ModuleUndescribedDTO `json:"undescribed,omitempty"`
}

// ModuleSchemaReader supplies the descriptor set. nil ⇒ 503.
type ModuleSchemaReader interface {
	Schema(ctx context.Context) (ModuleSchemaPage, error)
}

// ModuleTester runs a real probe against a configured module.
type ModuleTester interface {
	TestModule(ctx context.Context, surface, sourceType, operator string) (ModuleTestOutcome, error)
}

// ModuleTestOutcome is the Test button's answer.
type ModuleTestOutcome struct {
	Surface    string `json:"surface"`
	SourceType string `json:"source_type"`
	OK         bool   `json:"ok"`
	Summary    string `json:"summary"`
	Detail     string `json:"detail,omitempty"`
	ElapsedMS  int64  `json:"elapsed_ms"`
}

// ModuleSecretWriter stores a module credential. It returns WHERE the secret landed and never the value.
type ModuleSecretWriter interface {
	WriteModuleSecret(ctx context.Context, surface, sourceType, value, operator string) (ModuleSecretOutcome, error)
}

// ModuleSecretOutcome names the destination, never the material.
type ModuleSecretOutcome struct {
	Surface    string `json:"surface"`
	SourceType string `json:"source_type"`
	KVPath     string `json:"kv_path"`
	Field      string `json:"field"`
}

// moduleSchemaHandler serves GET /v1/modules/schema (AuthReadOnly — it describes forms, not values).
func (d Deps) moduleSchemaHandler(w http.ResponseWriter, r *http.Request, _ auth.Principal) {
	if d.ModuleSchema == nil {
		http.Error(w, "module schema unavailable", http.StatusServiceUnavailable)
		return
	}
	page, err := d.ModuleSchema.Schema(r.Context())
	if err != nil {
		http.Error(w, "module schema unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(page)
}

// moduleTestHandler serves POST /v1/modules/{surface}/{source}/test (AuthAdminSession).
//
// Admin-only despite being read-only in TG's own terms, because it causes a visible side effect in a
// third-party system that other people watch. Anyone who can make TG post into an operations room can
// make noise there.
func (d Deps) moduleTestHandler(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if !adminWriteGuard(w, r, d.ModuleTest != nil, "module test path unavailable", p) {
		return
	}
	surface, source, ok := moduleParams(w, r)
	if !ok {
		return
	}
	out, err := d.ModuleTest.TestModule(r.Context(), surface, source, operatorOf(p))
	if err != nil {
		// A transport failure REACHING the test is distinct from the test failing, and conflating them
		// would tell an operator their module is broken when TG's own worker is.
		http.Error(w, "the test could not be run: "+err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// ModuleSecretRequest carries the submitted credential.
type ModuleSecretRequest struct {
	Value string `json:"value"`
}

// moduleSecretHandler serves POST /v1/modules/{surface}/{source}/secret (AuthAdminSession).
func (d Deps) moduleSecretHandler(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if !adminWriteGuard(w, r, d.ModuleSecret != nil, "module secret path unavailable", p) {
		return
	}
	surface, source, ok := moduleParams(w, r)
	if !ok {
		return
	}
	var req ModuleSecretRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384)).Decode(&req); err != nil {
		// The body carried a credential; the error must not echo any of it back.
		http.Error(w, "malformed secret submission", http.StatusBadRequest)
		return
	}
	out, err := d.ModuleSecret.WriteModuleSecret(r.Context(), surface, source, req.Value, operatorOf(p))
	// req.Value is deliberately not referenced again past this point.
	if err != nil {
		// The writer's errors are built to be safe to display (they name the module and the bound, never
		// the material) — see core/secretwrite.
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// moduleParams extracts and bounds the module identity from the path.
//
// Both segments are bounded and charset-checked because they are used to look up a SECRET LANE. They can
// never name a path directly — the descriptor decides that — but an unbounded identifier still reaches a
// map lookup and a log line, and neither should accept arbitrary bytes.
func moduleParams(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	surface := strings.ToLower(strings.TrimSpace(chi.URLParam(r, "surface")))
	source := strings.ToLower(strings.TrimSpace(chi.URLParam(r, "source")))
	if !moduleIdentOK(surface) || !moduleIdentOK(source) {
		http.Error(w, "surface and source_type required", http.StatusBadRequest)
		return "", "", false
	}
	return surface, source, true
}

func moduleIdentOK(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' && r != '_' {
			return false
		}
	}
	return true
}
