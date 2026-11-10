package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	ingestadapter "github.com/territory-grounder/grounder/adapters/ingest"
	"github.com/territory-grounder/grounder/core/auth"
	coreingest "github.com/territory-grounder/grounder/core/ingest"
	"github.com/territory-grounder/grounder/modules/ingest/crowdsec"
	alertmanager "github.com/territory-grounder/grounder/modules/ingest/prometheus-alertmanager"
)

type tg708Refusals struct{ by map[string]int }

func (r *tg708Refusals) record(_ string, reason string) { r.by[reason]++ }

type tg708Losses struct{ by map[string]int }

func (l *tg708Losses) add(_ string, reason string, n int) {
	if n > 0 {
		l.by[reason] += n
	}
}

type tg708Triage struct{ started []coreingest.IncidentEnvelope }

func (t *tg708Triage) StartTriage(_ context.Context, env coreingest.IncidentEnvelope) (string, error) {
	t.started = append(t.started, env)
	return "tg/" + env.ExternalRef, nil
}

type tg708Outcome struct {
	code     int
	resp     IngestBatchAccepted
	refusals map[string]int
	losses   map[string]int
	started  []coreingest.IncidentEnvelope
	appended []string
	logged   string
}

func tg708Deliver(t *testing.T, ing ingestadapter.Ingester, body []byte) tg708Outcome {
	t.Helper()
	var buf bytes.Buffer
	prev, flags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() { log.SetOutput(prev); log.SetFlags(flags) }()
	refusals := &tg708Refusals{by: map[string]int{}}
	losses := &tg708Losses{by: map[string]int{}}
	triage := &tg708Triage{}
	alerts := &countingAlerts{}
	deps := Deps{Ingesters: fakeResolver{byType: map[string]ingestadapter.Ingester{ing.SourceType(): ing}},
		Triage: triage, Alerts: alerts, IngestRefused: refusals.record, IngestAlertsLost: losses.add}
	r := httptest.NewRequest("POST", "/v1/ingest/"+ing.SourceType(), bytes.NewReader(body))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("source_type", ing.SourceType())
	r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	deps.ingestHandler(w, r, auth.Principal{SourceID: ing.SourceType()})
	out := tg708Outcome{code: w.Code, refusals: refusals.by, losses: losses.by, started: triage.started, appended: alerts.refs, logged: buf.String()}
	if w.Code == http.StatusAccepted {
		if err := json.Unmarshal(w.Body.Bytes(), &out.resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	return out
}

func TestABatchWhoseEveryAlertFailedTheGrammarIsRefusedCountedAndLogged(t *testing.T) {
	bi := fakeBatchIngester{src: "prometheus-alertmanager", rejected: []coreingest.Rejection{
		{Index: 0, Reason: "alertmanager: malformed startsAt \"x\""}, {Index: 1, Reason: "bad severity: \"sev2\""}}}
	o := tg708Deliver(t, bi, nil)
	if o.code != http.StatusBadRequest {
		t.Fatalf("a delivery where every alert failed the grammar must be a 400, got %d", o.code)
	}
	if o.losses["alert_rejected"] != 2 || len(o.losses) != 1 {
		t.Fatalf("each rejected alert must be counted once as lost, got %v", o.losses)
	}
	if o.refusals["payload_rejected"] != 1 || len(o.refusals) != 1 {
		t.Fatalf("the refused delivery is counted once as payload_rejected, got %v", o.refusals)
	}
	if len(o.started) != 0 || len(o.appended) != 0 {
		t.Fatalf("a refused delivery starts and records nothing, got %d started %v appended", len(o.started), o.appended)
	}
	for _, want := range []string{"alert 1 of 2 rejected: alertmanager: malformed startsAt", "alert 2 of 2 rejected: bad severity"} {
		if !strings.Contains(o.logged, want) {
			t.Fatalf("the log must carry %q, got %q", want, o.logged)
		}
	}
}

func TestAMixedBatchAdmitsTheGoodAlertAndReportsTheBadOne(t *testing.T) {
	bi := fakeBatchIngester{src: "prometheus-alertmanager",
		envs:     []coreingest.IncidentEnvelope{{ExternalRef: "am-good"}},
		rejected: []coreingest.Rejection{{Index: 1, Reason: "malformed startsAt"}}, skipped: 1}
	o := tg708Deliver(t, bi, nil)
	if o.code != http.StatusAccepted || o.resp.Count != 1 || o.resp.Rejected != 1 || o.resp.Skipped != 1 {
		t.Fatalf("202 with count 1, rejected 1, skipped 1, got %d %+v", o.code, o.resp)
	}
	if o.losses["alert_rejected"] != 1 || len(o.refusals) != 0 || len(o.started) != 1 || len(o.appended) != 1 {
		t.Fatalf("one lost alert, no refused delivery, one admitted alert, got %v %v started=%d appended=%v", o.losses, o.refusals, len(o.started), o.appended)
	}
	if !strings.Contains(o.logged, "alert 2 of 3 rejected: malformed startsAt") {
		t.Fatalf("the rejection must be logged with its position, got %q", o.logged)
	}
}

func TestATruncatedDeliveryIsCountedOnceAndReported(t *testing.T) {
	bi := fakeBatchIngester{src: "prometheus-alertmanager", envs: []coreingest.IncidentEnvelope{{ExternalRef: "am-a"}}, truncated: 1_000_000}
	o := tg708Deliver(t, bi, nil)
	if o.code != http.StatusAccepted || o.resp.Truncated != 1_000_000 || o.resp.Count != 1 {
		t.Fatalf("202 carrying the sender's truncation count, got %d %+v", o.code, o.resp)
	}
	if o.losses["sender_truncated"] != 1_000_000 || len(o.refusals) != 0 {
		t.Fatalf("the sender's truncated alerts are counted as lost, and the delivery is not refused, got %v %v", o.losses, o.refusals)
	}
	if !strings.Contains(o.logged, "truncated 1000000 alert(s)") {
		t.Fatalf("the log must carry the sender's count, got %q", o.logged)
	}
}

func TestRejectionLogLinesAreBoundedButEveryRejectionIsCounted(t *testing.T) {
	var rj []coreingest.Rejection
	for i := range 50 {
		rj = append(rj, coreingest.Rejection{Index: i, Reason: "bad"})
	}
	o := tg708Deliver(t, fakeBatchIngester{src: "prometheus-alertmanager", rejected: rj}, nil)
	if o.losses["alert_rejected"] != 50 {
		t.Fatalf("every rejection is counted, got %v", o.losses)
	}
	if n := strings.Count(o.logged, " rejected: bad"); n != maxRejectionLogLines {
		t.Fatalf("%d per-alert log lines, want %d", n, maxRejectionLogLines)
	}
	if !strings.Contains(o.logged, "30 more alert(s) rejected") {
		t.Fatalf("the rest must be summarised, got %q", o.logged)
	}
}

func TestAnAllNoiseBatchIsStillA202WithNothingCounted(t *testing.T) {
	o := tg708Deliver(t, fakeBatchIngester{src: "prometheus-alertmanager", skipped: 3}, nil)
	if o.code != http.StatusAccepted || o.resp.Count != 0 || o.resp.Skipped != 3 || o.resp.Rejected != 0 {
		t.Fatalf("an all-noise delivery is a 202 that says it skipped 3, got %d %+v", o.code, o.resp)
	}
	if len(o.refusals) != 0 || len(o.losses) != 0 || o.logged != "" {
		t.Fatalf("noise is not a loss: no count, no log, got %v %v %q", o.refusals, o.losses, o.logged)
	}
}

func tg708Payload(truncated int, alerts ...map[string]any) []byte {
	b, err := json.Marshal(map[string]any{"version": "4", "status": "firing", "receiver": "tg", "truncatedAlerts": truncated, "alerts": alerts})
	if err != nil {
		panic(err)
	}
	return b
}

func tg708L3Alert(site, startsAt string) map[string]any {
	labels := map[string]any{"alertname": fmt.Sprintf("TGLiveCheckDependencyDown%d", time.Now().UnixNano()), "instance": "app01:9100", "severity": "warning"}
	if site != "" {
		labels["site"] = site
	}
	return map[string]any{"status": "firing", "labels": labels,
		"annotations": map[string]any{"summary": "Application on app01 is down because its dependency is unavailable"}, "startsAt": startsAt}
}

func TestTheL3PayloadsThroughTheRealAlertmanagerModule(t *testing.T) {
	am := alertmanager.New()
	now := time.Now().UTC()
	valid := now.Add(-time.Minute).Format(time.RFC3339)

	o := tg708Deliver(t, am, tg708Payload(0, tg708L3Alert("", "%Y-%m-%dT%H:%M:%SZ")))
	if o.code != http.StatusBadRequest || o.losses["alert_rejected"] != 1 || o.refusals["payload_rejected"] != 1 || !strings.Contains(o.logged, "malformed startsAt") {
		t.Fatalf("the L3 run-1 payload must be refused, counted and logged with its field: %d %v %v %q", o.code, o.losses, o.refusals, o.logged)
	}

	o = tg708Deliver(t, am, tg708Payload(0, tg708L3Alert("", valid)))
	if o.code != http.StatusAccepted || o.resp.Count != 1 || len(o.started) != 1 || o.started[0].Site != "" || len(o.refusals) != 0 || len(o.losses) != 0 {
		t.Fatalf("a valid alert with no site label is admitted with an empty site: %d %+v started=%+v %v", o.code, o.resp, o.started, o.refusals)
	}

	o = tg708Deliver(t, am, tg708Payload(0, tg708L3Alert("dc1", valid)))
	if o.code != http.StatusAccepted || o.resp.Count != 1 || len(o.started) != 1 || o.started[0].Site != "dc1" {
		t.Fatalf("the L3 run-3 payload is admitted with its site: %d %+v started=%+v", o.code, o.resp, o.started)
	}

	o = tg708Deliver(t, am, tg708Payload(0, tg708L3Alert("dc1", now.Add(-31*24*time.Hour).Format(time.RFC3339))))
	if o.code != http.StatusBadRequest || !strings.Contains(o.logged, "older than") {
		t.Fatalf("an over-age startsAt is refused and logged: %d %q", o.code, o.logged)
	}

	o = tg708Deliver(t, am, tg708Payload(4, tg708L3Alert("dc1", valid), tg708L3Alert("dc1", "%Y-%m-%dT%H:%M:%SZ")))
	if o.code != http.StatusAccepted || o.resp.Count != 1 || o.resp.Rejected != 1 || o.resp.Truncated != 4 ||
		o.losses["alert_rejected"] != 1 || o.losses["sender_truncated"] != 4 || len(o.refusals) != 0 || len(o.started) != 1 {
		t.Fatalf("a mixed, truncated delivery admits one and reports the rest: %d %+v %v %v", o.code, o.resp, o.losses, o.refusals)
	}

	resolved := tg708L3Alert("dc1", "%Y-%m-%dT%H:%M:%SZ")
	resolved["status"] = "resolved"
	o = tg708Deliver(t, am, tg708Payload(0, resolved))
	if o.code != http.StatusBadRequest || o.losses["alert_rejected"] != 1 {
		t.Fatalf("a rejected RESOLVED transition is a lost clear and is counted: %d %v", o.code, o.losses)
	}

	o = tg708Deliver(t, am, tg708Payload(2, tg708L3Alert("", "%Y-%m-%dT%H:%M:%SZ"), tg708L3Alert("", "bad")))
	if o.code != http.StatusBadRequest || o.losses["alert_rejected"] != 2 || o.losses["sender_truncated"] != 2 {
		t.Fatalf("an all-rejected, truncated delivery is refused and both losses counted: %d %v", o.code, o.losses)
	}
}

func TestALongRejectionReasonIsCutInTheLog(t *testing.T) {
	long := "alertmanager: malformed startsAt \"" + strings.Repeat("9", 5000) + "\""
	o := tg708Deliver(t, fakeBatchIngester{src: "prometheus-alertmanager", rejected: []coreingest.Rejection{{Index: 0, Reason: long}}}, nil)
	line := strings.TrimSpace(o.logged)
	if len(line) > maxRejectionReasonBytes+120 || !strings.Contains(line, "bytes cut") || !strings.Contains(line, "malformed startsAt") {
		t.Fatalf("a sender-sized reason must be cut to a bounded line that still names the field, got %d bytes: %.160q", len(line), line)
	}
}

func TestTheRealCrowdSecModuleThroughTheFrontDoor(t *testing.T) {
	cs := crowdsec.New(crowdsec.WithClock(func() time.Time { return time.Date(2026, 7, 15, 12, 5, 0, 0, time.UTC) }))
	bad := `{"scenario":"","message":"probe","start_at":"2026-07-15T12:00:00Z","source":{"scope":"Ip","value":"5.6.7.8","ip":"5.6.7.8"},"decisions":[]}`
	good := `{"scenario":"crowdsecurity/ssh-bf","message":"Ip 1.2.3.4 performed ssh-bf","start_at":"2026-07-15T12:00:00Z","source":{"scope":"Ip","value":"1.2.3.4","ip":"1.2.3.4"},"decisions":[]}`
	o := tg708Deliver(t, cs, []byte("["+bad+","+bad+"]"))
	if o.code != http.StatusBadRequest || o.losses["alert_rejected"] != 2 || !strings.Contains(o.logged, "missing scenario") {
		t.Fatalf("an all-rejected CrowdSec push is refused, counted and logged: %d %v %q", o.code, o.losses, o.logged)
	}
	o = tg708Deliver(t, cs, []byte("["+good+","+bad+"]"))
	if o.code != http.StatusAccepted || o.resp.Count != 1 || o.resp.Rejected != 1 || o.losses["alert_rejected"] != 1 {
		t.Fatalf("a mixed CrowdSec push admits the good alert and counts the bad one: %d %+v %v", o.code, o.resp, o.losses)
	}
}
