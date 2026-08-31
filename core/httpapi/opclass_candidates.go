package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/territory-grounder/grounder/core/actuate/opschema"
	"github.com/territory-grounder/grounder/core/auth"
	"github.com/territory-grounder/grounder/core/opclasscat"
)

// The earned op-class review surface (spec/028 REQ-2813, REQ-2817; ADR-0016).
//
// This is where a capability the system DISCOVERED is handed to a human to decide on. Everything unusual
// about the file follows from one rule in ADR-0016 decision 3: ratification is operator AUTHORSHIP, never
// model admission. The model's proposals are evidence about the world — "this keeps happening" — and an
// operator reading that evidence writes the actuation template themselves. The moment a model-authored
// string can become an argv template by being approved, the review is laundering rather than governing.
//
// So the read surface serves the model's words CLEARLY MARKED as exhibits, and the write surface accepts
// only what the operator typed. There is deliberately no endpoint that returns a draft spec, no field named
// "suggested_argv", and no verb that promotes an occurrence into a template. The console form is empty
// because there is nothing here to fill it with.

// OpClassCandidateView is one row of the review queue.
type OpClassCandidateView struct {
	CandidateKey string   `json:"candidate_key"`
	OpClass      string   `json:"op_class"`
	Op           string   `json:"op"`
	ParamNames   []string `json:"param_names"`
	Status       string   `json:"status"`
	Family       string   `json:"family"`
	Tier         string   `json:"tier"`
	Occurrences  int      `json:"occurrences"`
	Hosts        int      `json:"hosts"`
	FirstSeenAt  string   `json:"first_seen_at,omitempty"`
	LastSeenAt   string   `json:"last_seen_at,omitempty"`
	LedgerSeq    int64    `json:"ledger_seq"`
	// AutoBarred is carried into the queue rather than buried in the dossier: a class that can never reach a
	// silent rung is a different kind of decision, and an operator sorting a queue should see that first.
	AutoBarred bool `json:"auto_barred"`
	// CallerCanAct is server-derived. The console uses it to render, but it is not the enforcement — the
	// write routes are AuthSession and re-derive the operator themselves.
	CallerCanAct bool `json:"caller_can_act"`
	// ToCandidate is the distance still to travel, and is ABSENT (nil) once the shape has arrived. Rendering
	// it is what turns a queue of statuses into a queue an operator has a reason to come back to.
	ToCandidate *OpClassCandidacyGapView `json:"to_candidate,omitempty"`
}

// OpClassCandidatePage is one read of the review queue: the page itself, each shape's recurrence tally
// keyed by candidate_key, and the number of live candidates that exist — which is NOT len(Candidates)
// whenever the queue is longer than the page.
type OpClassCandidatePage struct {
	Candidates []opclasscat.Candidate
	Tallies    map[string]opclasscat.Tally
	Total      int
}

// OpClassCandidacyGapView is what a shape still needs before an operator may ratify it (TG-236 oracle 3).
//
// HostsNeeded and SpanHoursNeeded are ALTERNATIVE routes through one OR leg, never a conjunction: the
// console must render "one more host, or 4 more days" and never "one more host AND 4 more days", which
// would overstate the bar and suppress a ratification TG is already ready for.
type OpClassCandidacyGapView struct {
	RefsNeeded      int     `json:"refs_needed"`
	HostsNeeded     int     `json:"hosts_needed"`
	SpanHoursNeeded float64 `json:"span_hours_needed"`
	ConfidenceShort bool    `json:"confidence_short"`
}

// OpClassModelExhibit is the model's own text, and the field names say so.
//
// It is served for one reason: an operator cannot judge whether a proposal is trustworthy without reading
// what the model actually claimed. It is NEVER served in a shape that a form could consume — no field here
// corresponds to a writable field on the ratify request, and the console renders these screened and visually
// separated from the inputs. "Exhibit" is the accurate word: it is evidence shown to a decision-maker, not
// a value on its way into a template.
type OpClassModelExhibit struct {
	ModelVerb       string `json:"model_verb"`
	ModelRationale  string `json:"model_rationale"`
	ModelUndoSketch string `json:"model_undo_sketch"`
	Host            string `json:"host"`
	Target          string `json:"target"`
	Band            string `json:"band"`
	Outcome         string `json:"outcome"`
	ObservedAt      string `json:"observed_at,omitempty"`
	ExternalRef     string `json:"external_ref,omitempty"`
}

// OpClassDossierPage answers the five questions the design puts to an operator, in that order: what keeps
// happening, how often and where, what the model said about it, what it would be allowed to do, and what
// the blast radius is if it is wrong.
type OpClassDossierPage struct {
	Candidate OpClassCandidateView  `json:"candidate"`
	Exhibits  []OpClassModelExhibit `json:"exhibits"`
	Hosts     []string              `json:"hosts"`
	// RatifyReady mirrors the engine's completeness gate. The console must not compute it: a second opinion
	// about whether a dossier is complete is a second gate that can disagree with the one that matters.
	RatifyReady bool `json:"ratify_ready"`
	// Embedded / already-granted state, so the form can explain a refusal before an operator writes a
	// rationale rather than after.
	AlreadyGranted bool `json:"already_granted"`
	Embedded       bool `json:"embedded"`
	CallerCanAct   bool `json:"caller_can_act"`
}

// OpClassPage is the review queue.
type OpClassPage struct {
	Candidates   []OpClassCandidateView `json:"candidates"`
	Total        int                    `json:"total"`
	CallerCanAct bool                   `json:"caller_can_act"`
}

// OpClassOutcome is what a verb returns: the new status and the ledger row that recorded it, so the console
// can show an operator the chain entry their click produced rather than a bare 200.
type OpClassOutcome struct {
	CandidateKey string `json:"candidate_key,omitempty"`
	OpClass      string `json:"op_class"`
	Status       string `json:"status,omitempty"`
	Level        string `json:"level,omitempty"`
	LedgerSeq    int64  `json:"ledger_seq"`
	EntryHash    string `json:"entry_hash,omitempty"`
	// Artifact carries the export-embed MR body. It is empty for every other verb.
	Artifact string `json:"artifact,omitempty"`
}

// OpClassCandidateReader is the read half.
type OpClassCandidateReader interface {
	// OpClassCandidates returns the live review queue, newest activity first, WITH each shape's recurrence
	// tally and the honest store total (TG-236 oracles 2 and 3).
	//
	// The tally and total are returned together with the page, not fetched separately, because they are
	// facts about the same read: a queue that reported counts from a second, later query could show a row
	// whose "seen 8x" disagreed with the journal it links to. The shipped defect this signature replaces
	// passed literal zeros into every row's occurrence and host count while the console faithfully rendered
	// "0x / 0 host(s)" for shapes with real evidence behind them — and Total was len(page), so the rail
	// badge under-reported the queue whenever it was longer than one page.
	OpClassCandidates(ctx context.Context, limit int) (OpClassCandidatePage, error)
	// OpClassDossier returns one candidate and its occurrence journal. The journal is the exhibit source.
	OpClassDossier(ctx context.Context, key string) (opclasscat.Candidate, []opclasscat.Occurrence, error)
}

// OpClassWriter routes every verb through the worker — the ledger's single writer — exactly as the spec/027
// manifest lane does.
//
// This surface therefore CANNOT become a second status writer or a second grant path. Lifecycle verbs land
// in opclasscat.Transition (which owns the state machine and the ledger-before-row ordering); the grant
// lands in the overlay's append-only writer. Both already exist; nothing here reimplements either.
type OpClassWriter interface {
	// Ratify grants the operator-AUTHORED spec. The spec argument is what the operator typed; no part of it
	// is derived from the candidate's model text, and the caller has already proven that.
	//
	// promoteThreshold is the operator's ladder bar. It may only RAISE the tier default (migration 0049
	// CHECKs >= 5 and the lane clamps upward only): a ratification that could LOWER the bar would let the
	// grant and the speed of its promotion be decided in the same click.
	Ratify(ctx context.Context, key string, spec opschema.OpClassSpec, promoteThreshold int, rationale, approver string) (OpClassOutcome, error)
	// Dismiss ends a candidacy without a grant.
	Dismiss(ctx context.Context, key, rationale, approver string) (OpClassOutcome, error)
	// Demote drops ANY rung to approve (REQ-2810). Distinct from the estate-wide breaker: one misbehaving
	// class must not silently mute the whole estate, and muting the estate must not read as a verdict on
	// one class.
	Demote(ctx context.Context, opClass, rationale, approver string) (OpClassOutcome, error)
	// Revoke withdraws a live grant by appending a revoked row — the class falls to rung 0, registry absence.
	Revoke(ctx context.Context, opClass, rationale, approver string) (OpClassOutcome, error)
	// ExportEmbed renders the embed-export MR body and ledgers the intent. It CHANGES NOTHING (see the verb
	// table below).
	ExportEmbed(ctx context.Context, opClass, rationale, approver string) (OpClassOutcome, error)
}

// opClassVerbs is the CLOSED verb table (REQ-2813). Route text is never a status, and the set is exactly
// what the spec declares — a sixth verb is a design change, not a routing change.
//
// Two routes, not one, and the split is REQ-2817's rather than an accident of URL design: during candidacy
// the governed artifact is the candidate_key, and after ratification it is the granted class. A verb aimed
// at the wrong noun is refused by the router instead of discovering mid-handler that it has no id to ledger
// against.
var opClassCandidateVerbs = map[string]bool{"ratify": true, "dismiss": true}

// opClassClassVerbs act on a granted class.
//
// export-embed sits in the WRITE table while changing no state, which is worth stating plainly rather than
// letting a reader assume it is a mutation. It is here because it is an operator-only act requiring an
// authenticated session and a same-origin POST, and because REQ-2820 makes it a ledgered decision: an
// operator declaring intent to promote a class into the strongest tamper domain is governance-relevant even
// though the artifact it produces is only text a human must then review and merge.
var opClassClassVerbs = map[string]bool{"demote": true, "revoke": true, "export-embed": true}

// opClassPageLimit bounds one read of the review queue.
const opClassPageLimit = 200

// opClassLimits is this lane's OWN per-operator write limiter (the manifest/vote precedent). Sharing a
// window with another lane would let ratifications and adoptions starve each other.
var opClassLimits voteLimiter

// ErrOpClassCandidateNotFound is the writer's signal that a key names no candidate.
var ErrOpClassCandidateNotFound = errors.New("httpapi: unknown op-class candidate")

// ErrOpClassNotGranted is the writer's signal that a class has no live grant to act on.
var ErrOpClassNotGranted = errors.New("httpapi: op-class has no live grant")

// opClassCandidatesHandler serves GET /v1/opclass/candidates — the review queue.
func (d Deps) opClassCandidatesHandler(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if d.OpClass == nil {
		http.Error(w, "op-class candidates surface unavailable", http.StatusServiceUnavailable)
		return
	}
	limit := opClassPageLimit
	if v := strings.TrimSpace(r.URL.Query().Get("limit")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n < opClassPageLimit {
			limit = n
		}
	}
	page, err := d.OpClass.OpClassCandidates(r.Context(), limit)
	if err != nil {
		http.Error(w, "op-class candidates surface unavailable", http.StatusServiceUnavailable)
		return
	}
	canAct := strings.HasPrefix(p.SourceID, "operator:")
	views := make([]OpClassCandidateView, 0, len(page.Candidates))
	for _, c := range page.Candidates {
		t := page.Tallies[c.CandidateKey]
		v := candidateView(c, t.Occurrences, t.Hosts, canAct)
		// The journey, stated per shape. Only for shapes that have NOT yet arrived: a countdown rendered
		// against a candidate already awaiting ratification would invent a queue behind an open door.
		if g := opclasscat.CandidacyGap(t.Evidence()); !g.Met {
			v.ToCandidate = &OpClassCandidacyGapView{
				RefsNeeded:      g.RefsNeeded,
				HostsNeeded:     g.HostsNeeded,
				SpanHoursNeeded: g.SpanNeeded.Hours(),
				ConfidenceShort: g.ConfidenceShort,
			}
		}
		views = append(views, v)
	}
	w.Header().Set("Content-Type", "application/json")
	// Total is the STORE count, never len(views): the rail badge is a claim about the estate, and a badge
	// that silently equals the page size stops being one the moment the queue outgrows a page.
	_ = json.NewEncoder(w).Encode(OpClassPage{Candidates: views, Total: page.Total, CallerCanAct: canAct})
}

func candidateView(c opclasscat.Candidate, occurrences, hosts int, canAct bool) OpClassCandidateView {
	v := OpClassCandidateView{
		CandidateKey: c.CandidateKey,
		OpClass:      c.OpClass,
		Op:           c.Op,
		ParamNames:   c.ParamNames,
		Status:       string(c.Status),
		Family:       c.Family,
		Tier:         c.Tier,
		Occurrences:  occurrences,
		Hosts:        hosts,
		LedgerSeq:    c.LedgerSeq,
		AutoBarred:   c.AutoBarred,
		CallerCanAct: canAct,
	}
	if v.ParamNames == nil {
		v.ParamNames = []string{}
	}
	if !c.FirstSeenAt.IsZero() {
		v.FirstSeenAt = c.FirstSeenAt.UTC().Format(time.RFC3339)
	}
	if !c.LastSeenAt.IsZero() {
		v.LastSeenAt = c.LastSeenAt.UTC().Format(time.RFC3339)
	}
	return v
}

// opClassDossierHandler serves GET /v1/opclass/candidates/{key} — the five-question dossier.
func (d Deps) opClassDossierHandler(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if d.OpClass == nil {
		http.Error(w, "op-class candidates surface unavailable", http.StatusServiceUnavailable)
		return
	}
	key := strings.TrimSpace(chi.URLParam(r, "key"))
	if key == "" {
		http.Error(w, "candidate key required", http.StatusBadRequest)
		return
	}
	c, occs, err := d.OpClass.OpClassDossier(r.Context(), key)
	if errors.Is(err, ErrOpClassCandidateNotFound) {
		http.Error(w, "unknown op-class candidate", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "op-class candidates surface unavailable", http.StatusServiceUnavailable)
		return
	}
	canAct := strings.HasPrefix(p.SourceID, "operator:")
	hostSet := map[string]bool{}
	exhibits := make([]OpClassModelExhibit, 0, len(occs))
	for _, o := range occs {
		if o.Host != "" {
			hostSet[o.Host] = true
		}
		e := OpClassModelExhibit{
			ModelVerb:       o.Op,
			ModelRationale:  o.Rationale,
			ModelUndoSketch: o.UndoSketch,
			Host:            o.Host,
			Target:          o.Target,
			Band:            o.Band,
			Outcome:         o.Outcome,
			ExternalRef:     o.ExternalRef,
		}
		if !o.ObservedAt.IsZero() {
			e.ObservedAt = o.ObservedAt.UTC().Format(time.RFC3339)
		}
		exhibits = append(exhibits, e)
	}
	hosts := make([]string, 0, len(hostSet))
	for h := range hostSet {
		hosts = append(hosts, h)
	}
	sortStrings(hosts)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(OpClassDossierPage{
		Candidate:      candidateView(c, len(occs), len(hosts), canAct),
		Exhibits:       exhibits,
		Hosts:          hosts,
		RatifyReady:    c.Status == opclasscat.StatusRatifyReady,
		AlreadyGranted: alreadyGranted(c.OpClass),
		Embedded:       opschema.IsEmbedded(c.OpClass),
		CallerCanAct:   canAct,
	})
}

// alreadyGranted reports whether the class already resolves — embedded or overlay. The console uses it to
// explain a refusal BEFORE an operator writes a rationale rather than after they have done the work.
func alreadyGranted(opClass string) bool {
	_, ok := opschema.Lookup(opClass)
	return ok
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func (d Deps) opClassWriteGuard(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return false
	}
	if d.OpClassWrite == nil {
		http.Error(w, "op-class write path unavailable", http.StatusServiceUnavailable)
		return false
	}
	if !sameOrigin(r) {
		http.Error(w, "cross-origin write rejected", http.StatusForbidden)
		return false
	}
	return true
}

// opClassWriteErr keeps the retryable and the refused distinguishable — a 409 means this decision is not
// available from this state; a 503 means the lane is unwell and the caller should try again. Collapsing them
// teaches an operator to retry a permanent refusal, or to give up on a transient one.
func opClassWriteErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, opclasscat.ErrRationaleRequired):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, opclasscat.ErrBadTransition):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, ErrOpClassNotGranted):
		http.Error(w, "op-class has no live grant", http.StatusConflict)
	case errors.Is(err, ErrOpClassCandidateNotFound):
		http.Error(w, "unknown op-class candidate", http.StatusNotFound)
	default:
		http.Error(w, "op-class write failed — retry", http.StatusServiceUnavailable)
	}
}

// opClassRatifyRequest is EXACTLY what an operator may author. Every field is theirs.
//
// Note what is absent: there is no candidate-supplied spec, no "accept the model's suggestion" flag, and no
// way to reference the occurrence text by id. Adding any of those would create the prefill path ADR-0016
// decision 3 forbids — and it would do so in a way that looked like a convenience feature.
type opClassRatifyRequest struct {
	Rationale        string               `json:"rationale"`
	Op               string               `json:"op"`
	Family           string               `json:"family"`
	SafetyTier       string               `json:"safety_tier"`
	Params           []opschema.ParamSpec `json:"params"`
	ArgvTemplate     []string             `json:"argv_template"`
	RollbackTemplate []string             `json:"rollback_template"`
	PromoteThreshold int                  `json:"promote_threshold"`
}

// opClassCandidateVerbHandler serves POST /v1/opclass/candidates/{key}/{verb}.
func (d Deps) opClassCandidateVerbHandler(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if !d.opClassWriteGuard(w, r) {
		return
	}
	// Server-derived approver. A body-supplied one would make the grant's authorship a client claim — and
	// authorship is the entire content of a ratification.
	approver := operatorOf(p)
	if !opClassLimits.allow(approver, time.Now()) {
		http.Error(w, "op-class write rate limit exceeded", http.StatusTooManyRequests)
		return
	}
	key := strings.TrimSpace(chi.URLParam(r, "key"))
	if key == "" {
		http.Error(w, "candidate key required", http.StatusBadRequest)
		return
	}
	verb := chi.URLParam(r, "verb")
	if !opClassCandidateVerbs[verb] {
		http.Error(w, "unknown transition", http.StatusNotFound)
		return
	}
	var req opClassRatifyRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384)).Decode(&req); err != nil {
		http.Error(w, "malformed request", http.StatusBadRequest)
		return
	}
	rationale := strings.TrimSpace(req.Rationale)
	if rationale == "" {
		http.Error(w, "rationale required — every grant and dismissal states why", http.StatusBadRequest)
		return
	}
	if verb == "dismiss" {
		out, err := d.OpClassWrite.Dismiss(r.Context(), key, rationale, approver)
		if err != nil {
			opClassWriteErr(w, err)
			return
		}
		writeJSON(w, out)
		return
	}

	// RATIFY. The dossier is re-read server-side to obtain the model text, because the tripwire's whole
	// premise is that the comparison operand cannot come from the caller. A client-supplied "here is what
	// the model said" would let an attacker pass the check by submitting text that matches nothing.
	c, occs, err := d.OpClass.OpClassDossier(r.Context(), key)
	if err != nil {
		opClassWriteErr(w, err)
		return
	}
	spec := opschema.OpClassSpec{
		OpClass:          c.OpClass,
		Op:               strings.TrimSpace(req.Op),
		Family:           strings.TrimSpace(req.Family),
		SafetyTier:       strings.TrimSpace(req.SafetyTier),
		Params:           req.Params,
		ArgvTemplate:     req.ArgvTemplate,
		RollbackTemplate: req.RollbackTemplate,
	}
	// THE LAUNDERING TRIPWIRE (ADR-0016 decision 3). A template element that byte-matches the model's own
	// occurrence text is refused, because that is what model admission looks like from the outside: an
	// operator who pastes the proposal has authored nothing, and the system would be granting a capability
	// on the model's say-so while recording a human's name against it.
	//
	// This is a genuinely imperfect gate and saying so matters more than overselling it: byte equality
	// catches the paste, not a paraphrase. What it buys is that the EASY path — copy the suggestion into
	// the box — is closed, so authorship requires the operator to actually think about the command. A
	// determined operator can still retype the model's idea in their own words, and that is a person making
	// a judgment, which is precisely what ratification is supposed to be.
	//
	// 422, not 400: the request is well-formed and the operator is authenticated. What is inadmissible is
	// the CONTENT. The gate's own message is passed through verbatim rather than flattened to "invalid" —
	// an operator who is told WHICH element matched can rewrite that element, and one who is told "invalid"
	// will try the same paste again.
	validated, err := opschema.ValidateRatification(spec, modelTextOf(occs))
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	out, err := d.OpClassWrite.Ratify(r.Context(), key, validated, req.PromoteThreshold, rationale, approver)
	if err != nil {
		opClassWriteErr(w, err)
		return
	}
	writeJSON(w, out)
}

// modelTextOf collects the model's authored PROSE about this candidate — its rationale and its undo
// sketch. Anything an operator's template byte-matches in here is a paste, not an authorship.
//
// SPEC DEVIATION, FLAGGED FOR REVIEW (spec/028 REQ-2814 prose says "the verbs, rationales and undo
// sketches"). The bare op VERB is deliberately excluded. opschema's tripwire compares whole elements for
// exact equality, and the verb of a service-lifecycle class is "reload" — which is also, necessarily, an
// element of any argv template that reloads anything. Including it does not close a laundering path; it
// makes the entire service-lifecycle family unratifiable, and a gate that refuses every legitimate grant
// is indistinguishable from a broken lane. The verb is not model prose in any case: it is the observed
// operation identity, already fixed in the candidate's op_class slug before an operator sees the dossier.
//
// What the tripwire still catches is what it was written for — an operator pasting the model's suggested
// COMMAND ("systemctl reload haproxy") or its reasoning into the box.
func modelTextOf(occs []opclasscat.Occurrence) []string {
	out := make([]string, 0, len(occs)*2)
	for _, o := range occs {
		out = append(out, o.Rationale, o.UndoSketch)
	}
	return out
}

// opClassClassVerbHandler serves POST /v1/opclass/classes/{class}/{verb}.
func (d Deps) opClassClassVerbHandler(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if !d.opClassWriteGuard(w, r) {
		return
	}
	approver := operatorOf(p)
	if !opClassLimits.allow(approver, time.Now()) {
		http.Error(w, "op-class write rate limit exceeded", http.StatusTooManyRequests)
		return
	}
	class := strings.TrimSpace(chi.URLParam(r, "class"))
	if class == "" {
		http.Error(w, "op-class required", http.StatusBadRequest)
		return
	}
	verb := chi.URLParam(r, "verb")
	if !opClassClassVerbs[verb] {
		http.Error(w, "unknown transition", http.StatusNotFound)
		return
	}
	var req struct {
		Rationale string `json:"rationale"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, "malformed request", http.StatusBadRequest)
		return
	}
	// Rationale is mandatory for export-embed too, even though it changes nothing. The artifact it produces
	// is a request to move a class into the tamper domain that permits silent action; "why now" belongs in
	// the chain beside it.
	rationale := strings.TrimSpace(req.Rationale)
	if rationale == "" {
		http.Error(w, "rationale required — every grant, withdrawal and promotion request states why", http.StatusBadRequest)
		return
	}
	var (
		out OpClassOutcome
		err error
	)
	switch verb {
	case "demote":
		out, err = d.OpClassWrite.Demote(r.Context(), class, rationale, approver)
	case "revoke":
		out, err = d.OpClassWrite.Revoke(r.Context(), class, rationale, approver)
	case "export-embed":
		out, err = d.OpClassWrite.ExportEmbed(r.Context(), class, rationale, approver)
	}
	if err != nil {
		opClassWriteErr(w, err)
		return
	}
	writeJSON(w, out)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// The *ForAcceptance wrappers expose the unexported handlers to the spec/028 acceptance oracle, which drives
// the REAL handlers from an external package. They add no behavior — an oracle that exercised a copy would
// prove something about the copy.
func (d Deps) OpClassCandidatesHandlerForAcceptance(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	d.opClassCandidatesHandler(w, r, p)
}

// OpClassDossierHandlerForAcceptance — see OpClassCandidatesHandlerForAcceptance.
func (d Deps) OpClassDossierHandlerForAcceptance(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	d.opClassDossierHandler(w, r, p)
}

// OpClassCandidateVerbHandlerForAcceptance — see OpClassCandidatesHandlerForAcceptance.
func (d Deps) OpClassCandidateVerbHandlerForAcceptance(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	d.opClassCandidateVerbHandler(w, r, p)
}

// OpClassClassVerbHandlerForAcceptance — see OpClassCandidatesHandlerForAcceptance.
func (d Deps) OpClassClassVerbHandlerForAcceptance(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	d.opClassClassVerbHandler(w, r, p)
}
