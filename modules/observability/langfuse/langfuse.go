// Package langfuse is the loadable Langfuse observability module (spec/008 REQ-819).
//
// It implements adapters/observability.Exporter (metric/sample export, freshness-stamped) and adds a
// per-session trace recorder: Record posts an LLM/agent trajectory KEYED BY the session id to the
// configured Langfuse ingestion endpoint, so a session's reasoning is reconstructable from the outside
// (INV-14) and an absent trace for a live session is observable rather than silently missing (INV-15).
// The HTTP transport is injectable (a Doer) so the oracle drives the real code path against a fake
// Langfuse. Langfuse authenticates ingestion with HTTP Basic auth (public key as username, secret key as
// password); both are secret references, resolved per request, never literals (INV-13).
//
// Provenance: [O] INV-13/INV-14/INV-15, spec/008 REQ-819.
package langfuse

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	observability "github.com/territory-grounder/grounder/adapters/observability"
	"github.com/territory-grounder/grounder/core/config"
)

// SourceType is the vendor slug this module serves.
const SourceType = "langfuse"

// ingestPath is the single Langfuse batch write route. Both metric samples and per-session traces are
// emitted as ingestion events here; there is no per-id write route on the public API.
const ingestPath = "/api/public/ingestion"

// Doer is the minimal HTTP contract; *http.Client satisfies it, and tests inject a fake Langfuse.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// Module is the Langfuse observability adapter. Construct with New.
type Module struct {
	endpoint  string
	publicRef config.SecretRef
	secretRef config.SecretRef
	http      Doer
}

// Option configures a Module.
type Option func(*Module)

// WithHTTPClient injects the HTTP transport (a fake in tests, *http.Client in production).
func WithHTTPClient(d Doer) Option { return func(m *Module) { m.http = d } }

// New builds a Langfuse module for an endpoint base URL and the Basic-auth key pair Langfuse ingestion
// requires: publicRef resolves to the public key (Basic-auth username, e.g. "pk-lf-…") and secretRef to
// the secret key (Basic-auth password, e.g. "sk-lf-…"). Both are secret references (e.g.
// "env:TG_LANGFUSE_PUBLIC" / "env:TG_LANGFUSE_SECRET"), resolved per request, never stored as literals
// (INV-13).
func New(endpoint string, publicRef, secretRef config.SecretRef, opts ...Option) *Module {
	m := &Module{
		endpoint:  strings.TrimRight(endpoint, "/"),
		publicRef: publicRef,
		secretRef: secretRef,
		http:      http.DefaultClient,
	}
	for _, o := range opts {
		o(m)
	}
	return m
}

// SourceType implements adapters/observability.Exporter.
func (m *Module) SourceType() string { return SourceType }

// compile-time proof the module satisfies the stable observability interface.
var _ observability.Exporter = (*Module)(nil)

// ingestEvent is one Langfuse ingestion envelope: a unique id (for idempotent dedup), a valid event type,
// the ISO-8601 time the batch was sent, and the type-specific body. A bare payload without this envelope
// is rejected by Langfuse, so every sample and trace step is wrapped here.
type ingestEvent struct {
	ID        string    `json:"id"`
	Type      string    `json:"type"`
	Timestamp time.Time `json:"timestamp"`
	Body      any       `json:"body"`
}

// ingestResponse is the Langfuse ingestion reply. A 207 (or even a 200) can carry a non-empty errors
// array: those events were REJECTED and dropped. Surfacing them is mandatory — a silently dropped sample
// makes a dead writer read as healthy (INV-15).
type ingestResponse struct {
	Successes []struct {
		ID     string `json:"id"`
		Status int    `json:"status"`
	} `json:"successes"`
	Errors []struct {
		ID      string `json:"id"`
		Status  int    `json:"status"`
		Message string `json:"message"`
	} `json:"errors"`
}

// eventID derives a deterministic, unique ingestion id from stable content (no rand, no send-time), so
// resending the same event dedupes and tests can assert exact ids.
func eventID(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(h[:16])
}

// ingest POSTs a batch of ingestion events to Langfuse's batch route under HTTP Basic auth (public key :
// secret key), both resolved from their secret references at call time (INV-13). A non-2xx response is an
// error; a 2xx response whose errors array is non-empty is ALSO an error naming the dropped event ids —
// never a silent drop (INV-15).
func (m *Module) ingest(ctx context.Context, events []ingestEvent) error {
	public, err := m.publicRef.Resolve()
	if err != nil {
		return fmt.Errorf("langfuse: resolve public key: %w", err)
	}
	secret, err := m.secretRef.Resolve()
	if err != nil {
		return fmt.Errorf("langfuse: resolve secret key: %w", err)
	}
	b, err := json.Marshal(map[string]any{"batch": events})
	if err != nil {
		return fmt.Errorf("langfuse: marshal batch: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.endpoint+ingestPath, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.SetBasicAuth(public, secret) // Langfuse ingestion is HTTP Basic (public:secret), NOT a Bearer token.
	req.Header.Set("Content-Type", "application/json")

	resp, err := m.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("langfuse: POST %s: status %d: %s", ingestPath, resp.StatusCode, strings.TrimSpace(string(out)))
	}
	// A 207 Multi-Status (or a 200) can still report per-event rejections in its errors array; those events
	// were dropped and must surface rather than pass as success.
	if len(bytes.TrimSpace(out)) > 0 {
		var ir ingestResponse
		if err := json.Unmarshal(out, &ir); err != nil {
			return fmt.Errorf("langfuse: decode ingestion response (status %d): %w", resp.StatusCode, err)
		}
		if len(ir.Errors) > 0 {
			parts := make([]string, 0, len(ir.Errors))
			for _, e := range ir.Errors {
				if e.Message != "" {
					parts = append(parts, fmt.Sprintf("%s (%s)", e.ID, e.Message))
				} else {
					parts = append(parts, e.ID)
				}
			}
			return fmt.Errorf("langfuse: ingestion rejected %d event(s): %s", len(ir.Errors), strings.Join(parts, "; "))
		}
	}
	return nil
}

// Export ships a batch of freshness-stamped samples to the configured Langfuse endpoint. Each sample is
// wrapped in a proper ingestion envelope; the envelope timestamp is the send time (ISO-8601) while the
// body carries the sample's own Stamped freshness time (INV-15). A sample that arrives unstamped is
// stamped at export time so no series ever leaves without a freshness timestamp. An empty batch is a
// no-op.
func (m *Module) Export(ctx context.Context, samples []observability.Sample) error {
	if len(samples) == 0 {
		return nil
	}
	sent := time.Now().UTC()
	events := make([]ingestEvent, 0, len(samples))
	for i, s := range samples {
		stamped := s.Stamped
		if stamped.IsZero() {
			stamped = sent
		}
		events = append(events, ingestEvent{
			ID:        eventID("sample", s.Name, stamped.Format(time.RFC3339Nano), strconv.Itoa(i)),
			Type:      "event-create",
			Timestamp: sent,
			Body: map[string]any{
				"name":      s.Name,
				"value":     s.Value,
				"labels":    s.Labels,
				"startTime": stamped, // the sample's freshness stamp (INV-15).
			},
		})
	}
	return m.ingest(ctx, events)
}

// Record posts a per-session LLM/agent trace keyed by sessionID to the configured Langfuse endpoint. The
// trace is emitted as a trace-create event plus one observation-create event per step, all linked to a
// trace id derived from the session id, so a session's trajectory is unambiguously attributable to it
// (INV-14). An empty session id is rejected — an unkeyed trace is worse than none.
func (m *Module) Record(ctx context.Context, sessionID string, events []string) error {
	if sessionID == "" {
		return fmt.Errorf("langfuse: empty session id")
	}
	sent := time.Now().UTC()
	traceID := "trace-" + sessionID
	batch := make([]ingestEvent, 0, len(events)+1)
	batch = append(batch, ingestEvent{
		ID:        eventID("trace", sessionID),
		Type:      "trace-create",
		Timestamp: sent,
		Body: map[string]any{
			"id":        traceID,
			"sessionId": sessionID,
			"name":      "agent-loop",
			"timestamp": sent,
		},
	})
	for i, e := range events {
		batch = append(batch, ingestEvent{
			ID:        eventID("obs", sessionID, strconv.Itoa(i), e),
			Type:      "observation-create",
			Timestamp: sent,
			Body: map[string]any{
				"id":        eventID("obs-body", sessionID, strconv.Itoa(i)),
				"traceId":   traceID,
				"type":      "SPAN",
				"name":      e,
				"startTime": sent,
			},
		})
	}
	return m.ingest(ctx, batch)
}
