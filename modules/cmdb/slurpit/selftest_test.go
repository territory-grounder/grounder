package slurpit

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/config"
)

func slurpitProbe(t *testing.T, handler http.HandlerFunc) *EstateSource {
	t.Helper()
	t.Setenv("TG_TEST_SLURPIT_TOKEN", "t")
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return New(srv.URL, config.SecretRef("env:TG_TEST_SLURPIT_TOKEN"))
}

// TestSelfTestGreenReportsSampleAndEdgeYield: a healthy read names the sampled devices (the evidence a human
// recognises) and reports how many carry a site/parent the estate can turn into an edge — the tally that
// separates "bound and authorised" from "actually contributing topology".
func TestSelfTestGreenReportsSampleAndEdgeYield(t *testing.T) {
	src := slurpitProbe(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.RawQuery, "offset=0&limit=5") {
			t.Errorf("selftest must read a bounded page, got query %q", r.URL.RawQuery)
		}
		io.WriteString(w, `[{"hostname":"sw-core-01","site":"NL"},{"hostname":"sw-acc-01","parent":"sw-core-01"},{"hostname":"sw-lonely-01"}]`)
	})
	res, err := src.SelfTest(context.Background(), "")
	if err != nil {
		t.Fatalf("a healthy read must not error: %v", err)
	}
	if !strings.Contains(res.Summary, "sw-core-01") || !strings.Contains(res.Summary, "2 of the 3") {
		t.Errorf("summary must name the sample and report the edge yield (2 of 3), got %q", res.Summary)
	}
}

// TestSelfTestPlainHTTPTLSGuidance is the inverted-TLS oracle. Because Slurp'it is served over PLAIN HTTP, a
// TLS/handshake failure means the URL was set to https:// against a cleartext port — the fix is to switch to
// http://, the OPPOSITE of the advice the HTTPS modules (NetBox/PVE) give. The classifier must say so.
//
// KILLING MUTATION: copy NetBox's TLS arm ("do not work around it by changing the URL to http"). RED — the
// guidance would tell the operator the exact wrong thing.
func TestSelfTestPlainHTTPTLSGuidance(t *testing.T) {
	msg := classifySelfTestFailure(fmt.Errorf(`slurpit: Get "https://slurpit.example/api/devices": tls: first record does not look like a TLS handshake`))
	low := strings.ToLower(msg)
	if !strings.Contains(low, "plain http") || !strings.Contains(low, "http://") {
		t.Errorf("a TLS error against plain-HTTP Slurp'it must advise using http://, got %q", msg)
	}
	if strings.Contains(low, "install") && strings.Contains(low, "ca") {
		t.Errorf("must NOT advise installing a CA — Slurp'it serves no certificate: %q", msg)
	}
}

// TestSelfTestNonSlurpitBodyIsNotAnEmptyInventory: a 2xx body that is not a device array (a proxy/login page,
// an object envelope) is a WRONG-URL fault the operator fixes here — never reported as an empty inventory,
// which is a permission diagnosis.
func TestSelfTestNonSlurpitBodyIsNotAnEmptyInventory(t *testing.T) {
	src := slurpitProbe(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"detail":"authentication required"}`) // an object, not a device array
	})
	res, err := src.SelfTest(context.Background(), "")
	if err == nil {
		t.Fatal("a non-Slurp'it body must be an error, not a silent pass")
	}
	if !strings.Contains(res.Summary, "not as Slurp'it") {
		t.Errorf("summary must name the wrong-endpoint fault, got %q", res.Summary)
	}
}

// TestSelfTestEmptyInventoryPassesWithWarning: an empty array is a PASS (credential + endpoint proven) but a
// LOUDLY QUALIFIED one — an empty inventory is exactly what a permission-filtered token also looks like.
func TestSelfTestEmptyInventoryPassesWithWarning(t *testing.T) {
	src := slurpitProbe(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `[]`)
	})
	res, err := src.SelfTest(context.Background(), "")
	if err != nil {
		t.Fatalf("an empty inventory is a pass, not a failure: %v", err)
	}
	if res.Detail == "" || !strings.Contains(res.Detail, "no devices") {
		t.Errorf("an empty inventory must warn about permissions, got detail %q", res.Detail)
	}
}
