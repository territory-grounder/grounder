package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	ingestadapter "github.com/territory-grounder/grounder/adapters/ingest"
	"github.com/territory-grounder/grounder/core/auth"
	coreingest "github.com/territory-grounder/grounder/core/ingest"
)

// fakeBatchIngester implements adapters/ingest.BatchIngester with a fixed envelope set.
type fakeBatchIngester struct {
	src  string
	envs []coreingest.IncidentEnvelope
	err  error
}

func (f fakeBatchIngester) SourceType() string { return f.src }
func (f fakeBatchIngester) Normalize(context.Context, []byte) (coreingest.IncidentEnvelope, error) {
	if len(f.envs) != 1 {
		return coreingest.IncidentEnvelope{}, errors.New("not exactly one")
	}
	return f.envs[0], f.err
}
func (f fakeBatchIngester) NormalizeBatch(context.Context, []byte) ([]coreingest.IncidentEnvelope, error) {
	return f.envs, f.err
}

// scriptedTriage fails once at failAtCall (1-based), then succeeds on every call — simulating a transient
// Temporal outage mid-batch followed by a clean retry.
type scriptedTriage struct {
	calls      int
	failAtCall int
	failed     bool
}

func (s *scriptedTriage) StartTriage(_ context.Context, env coreingest.IncidentEnvelope) (string, error) {
	s.calls++
	if !s.failed && s.calls == s.failAtCall {
		s.failed = true
		return "", errors.New("temporal transiently down")
	}
	return "tg/" + env.ExternalRef, nil
}

// countingAlerts records every appended external_ref, so the test can prove no double-append.
type countingAlerts struct{ refs []string }

func (c *countingAlerts) Append(_ context.Context, rec AlertRecord) {
	c.refs = append(c.refs, rec.ExternalRef)
}
func (c *countingAlerts) Recent(context.Context, auth.Principal, int) ([]AlertRecord, error) {
	return nil, nil
}

// TestIngestBatchRetryDoesNotDoubleAppend reproduces the review's confirmed major: a grouped webhook whose
// triage backend fails mid-batch is 502'd and RETRIED WHOLE by the source. The alert log must not carry
// duplicate records — the first (failed) delivery must append NOTHING, and the successful retry appends
// exactly one record per incident.
func TestIngestBatchRetryDoesNotDoubleAppend(t *testing.T) {
	envs := []coreingest.IncidentEnvelope{{ExternalRef: "am-a"}, {ExternalRef: "am-b"}, {ExternalRef: "am-c"}}
	bi := fakeBatchIngester{src: "prometheus-alertmanager", envs: envs}
	resolver := fakeResolver{byType: map[string]ingestadapter.Ingester{"prometheus-alertmanager": bi}}
	triage := &scriptedTriage{failAtCall: 2} // fails on env #2 of the FIRST delivery only
	alerts := &countingAlerts{}
	deps := Deps{Ingesters: resolver, Triage: triage, Alerts: alerts}

	// delivery 1: mid-batch triage failure → 502, and NO alert-log side effects.
	w := httptest.NewRecorder()
	deps.ingestHandler(w, ingestReq("prometheus-alertmanager"), auth.Principal{SourceID: "prometheus-alertmanager"})
	if w.Code != http.StatusBadGateway {
		t.Fatalf("mid-batch triage failure must 502, got %d", w.Code)
	}
	if len(alerts.refs) != 0 {
		t.Fatalf("a failed batch delivery must append NOTHING to the alert log, got %v", alerts.refs)
	}

	// delivery 2 (the source's whole-webhook retry): succeeds, appends exactly one record per incident.
	w = httptest.NewRecorder()
	deps.ingestHandler(w, ingestReq("prometheus-alertmanager"), auth.Principal{SourceID: "prometheus-alertmanager"})
	if w.Code != http.StatusAccepted {
		t.Fatalf("retry must succeed 202, got %d", w.Code)
	}
	if len(alerts.refs) != 3 {
		t.Fatalf("the alert log must hold exactly one record per incident after the retry, got %v", alerts.refs)
	}
	var resp IngestBatchAccepted
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Count != 3 || len(resp.Incidents) != 3 || !resp.Incidents[0].Triggered {
		t.Fatalf("batch response must report all three incidents triggered: %+v", resp)
	}
}

// TestIngestBatchEmptyResultAccepted: a well-formed webhook whose alerts were all skipped as noise is a 202
// with count 0 — never a 400 (there was nothing to triage, but the webhook was not malformed).
func TestIngestBatchEmptyResultAccepted(t *testing.T) {
	bi := fakeBatchIngester{src: "prometheus-alertmanager", envs: nil}
	resolver := fakeResolver{byType: map[string]ingestadapter.Ingester{"prometheus-alertmanager": bi}}
	alerts := &countingAlerts{}
	w := httptest.NewRecorder()
	Deps{Ingesters: resolver, Triage: &scriptedTriage{}, Alerts: alerts}.ingestHandler(w, ingestReq("prometheus-alertmanager"), auth.Principal{SourceID: "prometheus-alertmanager"})
	if w.Code != http.StatusAccepted {
		t.Fatalf("an all-noise batch must 202, got %d", w.Code)
	}
	if len(alerts.refs) != 0 {
		t.Fatalf("nothing should be appended for an empty batch, got %v", alerts.refs)
	}
}
