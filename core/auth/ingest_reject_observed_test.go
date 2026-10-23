package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TG-371 item 4 — the AUTH layer counts an ingest push it turns away, the half the handler counter cannot
// see (auth fails BEFORE the handler runs). These oracles pin: it fires on an auth-refusal status for an
// AuthIngestPush route, names the source, does NOT fire on success (which would double-count against the
// handler) or for non-ingest routes, and fires even when the reject-brake is unwired.

// captureRejects records observer calls.
type captureRejects struct{ calls [][2]string }

func (c *captureRejects) obs(sourceType, reason string) {
	c.calls = append(c.calls, [2]string{sourceType, reason})
}

// driveBrakeWrap runs one request through brakeWrap over an AuthIngestPush inner handler that writes the given
// status, and returns the observer's captured calls. brake is left as the Router's default (armed).
func driveBrakeWrap(t *testing.T, method AuthMethod, path string, status int) [][2]string {
	t.Helper()
	rt := &Router{brake: newRejectBrake(nil)}
	cap := &captureRejects{}
	rt.SetIngestRejectObserver(cap.obs)
	h := rt.brakeWrap(method, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) })
	h(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, path, nil))
	return cap.calls
}

func TestAuthLayerCountsAnIngestPushItRefuses(t *testing.T) {
	calls := driveBrakeWrap(t, AuthIngestPush, "/v1/ingest/prometheus-alertmanager", http.StatusUnauthorized)
	if len(calls) != 1 {
		t.Fatalf("observer fired %d time(s) on a 401 ingest push, want 1 — an auth refusal is invisible, so a "+
			"rotated bearer and a quiet source are the same observable (TG-371 item 4)", len(calls))
	}
	if calls[0] != [2]string{"prometheus-alertmanager", "auth"} {
		t.Errorf("observer got %v, want [prometheus-alertmanager auth] — the refusal must name the source and "+
			"the reason, or the operator cannot tell WHICH feed is being turned away", calls[0])
	}
}

func TestAuthRefusalIsAlsoCountedForForbidden(t *testing.T) {
	calls := driveBrakeWrap(t, AuthIngestPush, "/v1/ingest/crowdsec", http.StatusForbidden)
	if len(calls) != 1 || calls[0][1] != "auth" {
		t.Fatalf("a 403 on an ingest push was not counted as an auth refusal: %v", calls)
	}
}

func TestAuthPassIsNotCountedAsARefusal(t *testing.T) {
	// A 200/202 means auth ADMITTED the push and the handler ran — the handler's own counter records any
	// handler-level refusal, so counting here too would double-charge.
	for _, ok := range []int{http.StatusOK, http.StatusAccepted} {
		if calls := driveBrakeWrap(t, AuthIngestPush, "/v1/ingest/authlog", ok); len(calls) != 0 {
			t.Errorf("observer fired on a %d (auth PASSED): %v — auth successes must never count as refusals, "+
				"or every accepted delivery inflates the refusal series", ok, calls)
		}
	}
}

func TestNonIngestAuthRefusalIsNotCounted(t *testing.T) {
	// A 401 on a console/API route is not an ingest refusal — the series is per ingest source_type.
	if calls := driveBrakeWrap(t, AuthHMAC, "/v1/proposals", http.StatusUnauthorized); len(calls) != 0 {
		t.Errorf("observer fired on a non-ingest 401: %v — tg_ingest_refused_total must not absorb every "+
			"auth failure in the router", calls)
	}
}

// TestObserverFiresEvenWhenBrakeUnwired — the ticket's specific caveat: the observation must NOT hang off the
// brake, because brakeWrap's brake path is skipped when brake == nil. A refusal counter that goes dark exactly
// when the brake is unconfigured is the failure this closes.
func TestObserverFiresEvenWhenBrakeUnwired(t *testing.T) {
	rt := &Router{brake: nil}
	cap := &captureRejects{}
	rt.SetIngestRejectObserver(cap.obs)
	h := rt.brakeWrap(AuthIngestPush, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) })
	h(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/ingest/prometheus-alertmanager", nil))
	if len(cap.calls) != 1 {
		t.Fatalf("observer fired %d time(s) with brake==nil, want 1 — the observation is hung off the brake "+
			"and inherits its nil-skip, going dark exactly when the brake is unwired (TG-371 item 4)", len(cap.calls))
	}
}

func TestIngestSourceFromPath(t *testing.T) {
	cases := map[string]string{
		"/v1/ingest/prometheus-alertmanager": "prometheus-alertmanager",
		"/v1/ingest/crowdsec/extra":          "crowdsec",
		"/v1/ingest/":                        "unknown",
		"/v1/proposals":                      "unknown",
		"":                                   "unknown",
	}
	for in, want := range cases {
		if got := ingestSourceFromPath(in); got != want {
			t.Errorf("ingestSourceFromPath(%q) = %q, want %q", in, got, want)
		}
	}
}
