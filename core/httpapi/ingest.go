package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	ingestadapter "github.com/territory-grounder/grounder/adapters/ingest"
	"github.com/territory-grounder/grounder/core/auth"
	coreingest "github.com/territory-grounder/grounder/core/ingest"
)

// maxIngestBody bounds a single ingested payload so a hostile or misconfigured source cannot exhaust memory.
const maxIngestBody = 1 << 20 // 1 MiB

// IngesterResolver resolves an authenticated source's vendor slug to its normalizing ingester, or fails
// closed if that source has no registered, enabled ingest capability (INV-17). The composition root backs it
// with the module registry (modules/resolve); the handler stays registry-agnostic and oracle-testable.
type IngesterResolver interface {
	ResolveIngester(sourceType string) (ingestadapter.Ingester, error)
}

// TriageStarter mints the Runner triage workflow for a normalized envelope, returning the workflow id. It is
// keyed by external_ref with reject-duplicate semantics, so a re-fire of the same incident joins the
// in-flight session rather than starting a second (idempotent). Optional: a deployment without a triage
// backend wired accepts and normalizes payloads without triggering (validate-only). The workflow it starts
// is the read-only Phase-0/1 Runner — it drives to a gated proposal and stops; it never mutates the estate.
type TriageStarter interface {
	StartTriage(ctx context.Context, env coreingest.IncidentEnvelope) (workflowID string, err error)
}

// TransitionRecord is a provider state transition captured at the front door — today a RECOVERY (a fault's
// alert went back UP), carrying the SAME external_ref as the fault that opened it. Non-secret identifiers
// only (INV-13). Mirrors the AlertRecord projection shape.
type TransitionRecord struct {
	ExternalRef string
	Kind        string // "recovery"
	// SourceID is the ingest source that delivered this recovery (TG-393). The envelope has always
	// carried it — RecordFromEnvelope reads env.SourceID for the alert log — and this projector dropped
	// it, which is why per-source freshness could only see raises.
	SourceID   string
	Host       string
	Site       string
	AlertRule  string
	ObservedAt time.Time
	ReceivedAt time.Time
}

// TransitionRecorder durably captures the provider recovery transitions the front door would otherwise
// discard (StartTriage dedup + the alert-log's ON CONFLICT DO NOTHING), so the Runner's confirmed-clear check
// (spec/012) can be driven by TG's OWN retained evidence of the recovery rather than a lagging re-pull.
// Best-effort by contract: Append returns no error — the ingest path must never block on it.
type TransitionRecorder interface {
	Append(ctx context.Context, rec TransitionRecord)
}

// isRecovery reports whether the envelope carries a provider recovery-transition label (a fault's alert went
// back UP). A recovery is NOT a new incident — it is clear-evidence for the waiting session.
func isRecovery(env coreingest.IncidentEnvelope) bool {
	return env.Labels[coreingest.LabelTransition] == coreingest.TransitionRecovery
}

// deliveryContext reports HOW this request reached TG (TG-372): the transport-level remote address and the
// Host header the caller addressed us by.
//
// BOTH ARMS OF THE FRONT DOOR STAMP IT. The single and the grouped-transport arm each Append, and an
// intake that records its origin on one shape and not the other answers the question only by luck.
//
// r.RemoteAddr is the PEER — behind a proxy that is the proxy, and that is exactly the useful reading: it
// says which hop handed TG the request. No X-Forwarded-For parsing, deliberately: that header is
// caller-controlled and trusting it would let a source choose what TG records about it. The Host header is
// caller-controlled too, but it is recorded AS a claim rather than resolved into one.
func deliveryContext(r *http.Request) (peer, host string) {
	return r.RemoteAddr, r.Host
}

// refuse records a REFUSED delivery and writes the HTTP error (TG-371). One call site per rejection so
// the counter and the status can never disagree — the failure this closes is a front door that publishes
// only what it ACCEPTED, leaving "the source has nothing to say" and "we are turning it away"
// indistinguishable from outside.
//
// The reason is a CLOSED vocabulary, not the error text: it is a metric label, and free text would make
// the series cardinality follow whatever a caller sent.
func (d Deps) refuse(w http.ResponseWriter, sourceType, reason, msg string, code int) {
	if d.IngestRefused != nil {
		d.IngestRefused(sourceType, reason)
	}
	http.Error(w, msg, code)
}

// transitionFromEnvelope projects an accepted recovery envelope onto the non-secret TransitionRecord.
func transitionFromEnvelope(env coreingest.IncidentEnvelope) TransitionRecord {
	return TransitionRecord{
		ExternalRef: env.ExternalRef,
		Kind:        coreingest.TransitionRecovery,
		SourceID:    env.SourceID,
		Host:        env.Host,
		Site:        env.Site,
		AlertRule:   env.AlertRule,
		ObservedAt:  env.ObservedAt,
		ReceivedAt:  env.ReceivedAt,
	}
}

// IngestAccepted is the response for a normalized payload: the correlation key the rest of the pipeline joins
// on, echoed so the source can correlate its submission, plus whether triage was triggered and its id.
type IngestAccepted struct {
	Accepted    bool   `json:"accepted"`
	SourceType  string `json:"source_type"`
	ExternalRef string `json:"external_ref"`
	Triggered   bool   `json:"triggered"`
	WorkflowID  string `json:"workflow_id,omitempty"`
}

// ingestHandler is the alert front door. An authenticated source POSTs its raw payload to
// /v1/ingest/{source_type}; the handler resolves that source's ingester from the module registry — an
// unregistered or DISABLED source has no execution path and is a 404 (INV-17) — and normalizes the payload
// against the source's explicit grammar (INV-04): a payload that does not satisfy the grammar is rejected as
// a 400, never coerced. This is the registry-backed resolution seam: an alert can only enter through a
// declared, enabled ingest capability. Phase 0/1 is read-only triage — the handler validates and normalizes;
// publishing the triage.requested event that mints the Runner workflow is the next step. A nil resolver fails
// closed to 503.
func (d Deps) ingestHandler(w http.ResponseWriter, r *http.Request, _ auth.Principal) {
	// Resolved FIRST so every refusal below can name the source it turned away (TG-371). A refusal
	// counter without the source is only marginally better than no counter: "something was rejected"
	// does not tell an operator which feed went dark.
	sourceType := chi.URLParam(r, "source_type")
	if d.Ingesters == nil {
		d.refuse(w, sourceType, "no_ingester_registry", "ingest unavailable", http.StatusServiceUnavailable)
		return
	}
	ing, err := d.Ingesters.ResolveIngester(sourceType)
	if err != nil {
		// Unregistered, disabled, or wrong-typed — no execution path (INV-17). Reveal nothing more.
		d.refuse(w, sourceType, "unknown_source", "no ingest capability for source", http.StatusNotFound)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxIngestBody))
	if err != nil {
		d.refuse(w, sourceType, "unreadable_body", "unreadable body", http.StatusBadRequest)
		return
	}
	if bi, ok := ing.(ingestadapter.BatchIngester); ok {
		// Grouped-transport source (Alertmanager batches alerts per webhook): fan each normalized envelope
		// out to its own idempotent triage session. An empty result (every alert skipped as noise) is a 202
		// with count 0 — the webhook was well-formed; there was simply nothing to triage.
		d.ingestBatch(w, r, sourceType, bi, raw)
		return
	}
	env, err := ing.Normalize(r.Context(), raw)
	if err != nil {
		// Grammar violation — rejected, never coerced (INV-04). LOG the field-level reason so an operator
		// wiring a real source transport sees WHICH field failed (missing site, non-IP host, bad timestamp,
		// …) instead of a blind 400. The error text is field/grammar-level, not payload-value-level, so no
		// host detail is logged; the response stays a terse "payload rejected" (no reason leaked to the caller).
		log.Printf("ingest: %s payload rejected: %v", sourceType, err)
		d.refuse(w, sourceType, "payload_rejected", "payload rejected", http.StatusBadRequest)
		return
	}
	resp := IngestAccepted{Accepted: true, SourceType: sourceType, ExternalRef: env.ExternalRef}
	if isRecovery(env) {
		// A provider RECOVERY transition is NOT a new incident — minting a triage session would only dedup
		// into the finished fault workflow, and the alert-log append is an ON CONFLICT no-op. Capture it as
		// durable clear-evidence for the waiting Runner (spec/012) and return 202 without triggering triage.
		if d.IngestPredrop != nil {
			d.IngestPredrop("recovery_transition") // TG-380: accepted, but mints no triage — a pre-admission drop
		}
		if d.Transitions != nil {
			d.Transitions.Append(r.Context(), transitionFromEnvelope(env))
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(resp)
		return
	}
	if d.Triage != nil {
		// Mint the read-only Runner triage session (idempotent by external_ref). A backend failure is a 502
		// so the source can retry rather than have its alert silently dropped un-triaged.
		wfID, terr := d.Triage.StartTriage(r.Context(), env)
		if terr != nil {
			d.refuse(w, sourceType, "triage_unavailable", "triage unavailable", http.StatusBadGateway)
			return
		}
		resp.Triggered = true
		resp.WorkflowID = wfID
	}
	if d.Alerts != nil {
		// Record the ACCEPTED envelope in the alert log (REQ-510) — the ingest tier's own record of
		// what the front door admitted. Never blocks or fails the ingest path.
		d.Alerts.Append(r.Context(), RecordFromEnvelope(sourceType, env, resp.WorkflowID).
			WithDelivery(deliveryContext(r)))
	}
	if d.Suppression != nil {
		// SHADOW ONLY — records whether incident-scoped suppression WOULD have dropped this alert, and
		// drops nothing. 73.3% of organic alerts are repeats, but suppression that is wrong in the other
		// direction is an incident nobody sees, so the judgement earns its way in on live evidence before
		// it is allowed to act. Deliberately takes no context and returns no error: the ingest path can
		// neither be delayed nor failed by an observation.
		d.Suppression.ObserveAccepted(env.Host, env.AlertRule, env.ReceivedAt)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(resp)
}

// IngestBatchAccepted is the response for a grouped-transport payload: one entry per normalized alert, each
// with its correlation key and triage workflow id.
type IngestBatchAccepted struct {
	Accepted   bool             `json:"accepted"`
	SourceType string           `json:"source_type"`
	Count      int              `json:"count"`
	Incidents  []IngestAccepted `json:"incidents,omitempty"`
}

// ingestBatch is the grouped-transport arm of the front door: normalize the whole webhook (per-alert grammar
// discipline lives in the module), then mint one idempotent triage session per envelope. A triage-backend
// failure is a 502 so the source retries the webhook; re-delivery is safe because sessions are keyed by
// external_ref with reject-duplicate semantics.
func (d Deps) ingestBatch(w http.ResponseWriter, r *http.Request, sourceType string, bi ingestadapter.BatchIngester, raw []byte) {
	envs, err := bi.NormalizeBatch(r.Context(), raw)
	if err != nil {
		// Grammar violation at the webhook level — rejected, never coerced (INV-04). Log the field-level
		// reason (see the single-alert path) so a misconfigured transport is diagnosable, not a blind 400.
		log.Printf("ingest: %s batch payload rejected: %v", sourceType, err)
		d.refuse(w, sourceType, "payload_rejected", "payload rejected", http.StatusBadRequest)
		return
	}
	resp := IngestBatchAccepted{Accepted: true, SourceType: sourceType, Count: len(envs)}
	// Phase 1: mint EVERY triage session before any other side effect. StartTriage is idempotent
	// (external_ref, reject-duplicate) but the alert-log append is NOT — interleaving them meant a
	// mid-batch failure 502'd AFTER earlier envelopes were appended, and the source's whole-webhook
	// retry double-appended those records. Failing here leaves ZERO side effects beyond idempotent
	// session starts, so re-delivery is safe end-to-end.
	wfIDs := make([]string, len(envs))
	if d.Triage != nil {
		for i, env := range envs {
			if isRecovery(env) {
				continue // a recovery transition mints no triage session (captured as clear-evidence in phase 2)
			}
			wfID, terr := d.Triage.StartTriage(r.Context(), env)
			if terr != nil {
				d.refuse(w, sourceType, "triage_unavailable", "triage unavailable", http.StatusBadGateway)
				return
			}
			wfIDs[i] = wfID
		}
	}
	// Phase 2: record + respond — reached only when every session started.
	for i, env := range envs {
		item := IngestAccepted{Accepted: true, SourceType: sourceType, ExternalRef: env.ExternalRef}
		if isRecovery(env) {
			// Clear-evidence, not an incident: capture it durably (spec/012) and report it un-triggered.
			if d.Transitions != nil {
				d.Transitions.Append(r.Context(), transitionFromEnvelope(env))
			}
			resp.Incidents = append(resp.Incidents, item)
			continue
		}
		if d.Triage != nil {
			item.Triggered = true
			item.WorkflowID = wfIDs[i]
		}
		if d.Alerts != nil {
			d.Alerts.Append(r.Context(), RecordFromEnvelope(sourceType, env, item.WorkflowID).
				WithDelivery(deliveryContext(r)))
		}
		if d.Suppression != nil {
			// THE SAME OBSERVATION THE SINGLE-ENVELOPE ARM MAKES. Its absence here was not a policy choice:
			// every alert from a grouped-transport source — Alertmanager, and CrowdSec, which is a SECURITY
			// source — was excluded from the shadow measurement that the enable decision rests on, and
			// nothing said so. A percentage computed over some intakes and not others is not conservative;
			// its error moves with whichever source happens to be busy.
			d.Suppression.ObserveAccepted(env.Host, env.AlertRule, env.ReceivedAt)
		}
		resp.Incidents = append(resp.Incidents, item)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(resp)
}

// SuppressionObserver records, for an accepted alert, whether incident-scoped suppression WOULD have
// dropped it as a repeat of a still-open incident (core/ingest.DecideSuppress).
//
// It is SHADOW by construction. The method takes no context and returns no error precisely so that the
// ingest path cannot be delayed or failed by an observation — an implementation that needs to read durable
// history must do so on its own goroutine with its own deadline. Measuring what a suppressor would do,
// before letting it do it, is the same discipline TG applies to its own actuations: commit the prediction,
// then score it.
type SuppressionObserver interface {
	ObserveAccepted(host, alertRule string, at time.Time)
}
