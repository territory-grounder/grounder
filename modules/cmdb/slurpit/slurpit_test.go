package slurpit

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/config"
)

// fullPage renders a JSON array of n minimal device records (each with a distinct hostname and a site), the
// bare-array shape Slurp'it's /api/devices returns.
func fullPage(n int) string {
	var b strings.Builder
	b.WriteByte('[')
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"hostname":"dev%d","site":"NL"}`, i)
	}
	b.WriteByte(']')
	return b.String()
}

// TestClientSendsBearerAuthAndReadsInventory drives the REAL http path against a plain-HTTP fake Slurp'it and
// proves the two facts the SDK grounding fixed: the credential is sent as `Bearer <token>` (not NetBox's
// `Token`, not LibreNMS's `X-Auth-Token`), resolved from its secret reference (INV-13), and the devices
// endpoint is /api/devices.
func TestClientSendsBearerAuthAndReadsInventory(t *testing.T) {
	t.Setenv("TG_TEST_SLURPIT_TOKEN", "sl_secret")
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		io.WriteString(w, `[{"hostname":"sw-core-01","site":"NL"},{"hostname":"sw-acc-01","site":"NL","parent":"sw-core-01"}]`)
	}))
	defer srv.Close()

	src := New(srv.URL, config.SecretRef("env:TG_TEST_SLURPIT_TOKEN"))
	devices, err := src.fetchDevices(context.Background())
	if err != nil {
		t.Fatalf("fetchDevices must succeed against a live Slurp'it: %v", err)
	}
	if gotAuth != "Bearer sl_secret" {
		t.Errorf("must authenticate with the resolved Bearer token, got %q", gotAuth)
	}
	if gotPath != devicesPath {
		t.Errorf("must read %s, got %q", devicesPath, gotPath)
	}
	if len(devices) != 2 || devices[0].Hostname != "sw-core-01" || devices[1].Parent != "sw-core-01" {
		t.Errorf("device inventory not parsed: %+v", devices)
	}
}

// TestSchemeLessURLDefaultsToPlainHTTPNeverTLS is the PLAIN-HTTP oracle. A base URL configured with no scheme
// (the natural way an operator types a bare host/IP) must resolve to http://, NEVER https:// — because
// Slurp'it is served over cleartext and assuming TLS would dial a cleartext port and fail misleadingly. The
// proof is behavioural: a scheme-less config reaches a REAL plain-HTTP server, which it could only do by
// speaking http and refusing the TLS assumption.
//
// KILLING MUTATION: make normalizeBaseURL prepend "https://" (or leave the scheme off). RED — the request
// fails against the plain-HTTP server, and the baseURL prefix assertion fails.
func TestSchemeLessURLDefaultsToPlainHTTPNeverTLS(t *testing.T) {
	t.Setenv("TG_TEST_SLURPIT_TOKEN", "t")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `[{"hostname":"sw-core-01","site":"NL"}]`)
	}))
	defer srv.Close()

	hostPort := strings.TrimPrefix(srv.URL, "http://") // e.g. "127.0.0.1:54321" — NO scheme, like a typed host
	src := New(hostPort, config.SecretRef("env:TG_TEST_SLURPIT_TOKEN"))

	if !strings.HasPrefix(src.baseURL, "http://") {
		t.Fatalf("a scheme-less Slurp'it URL must resolve to http://, got %q", src.baseURL)
	}
	if strings.HasPrefix(src.baseURL, "https://") {
		t.Fatalf("the client must NOT assume TLS for plain-HTTP Slurp'it, got %q", src.baseURL)
	}
	// And it must actually reach the plain-HTTP server — proof it spoke http rather than attempting a TLS
	// handshake against the cleartext port.
	if _, err := src.fetchDevices(context.Background()); err != nil {
		t.Fatalf("a scheme-less config must reach the plain-HTTP server over http: %v", err)
	}
}

// TestExplicitSchemesArePreserved: an operator who has fronted Slurp'it with a TLS proxy may set https://
// explicitly, and an explicit http:// is kept as-is. The http-default applies ONLY when no scheme was given.
func TestExplicitSchemesArePreserved(t *testing.T) {
	if got := normalizeBaseURL("https://slurpit.example/"); got != "https://slurpit.example" {
		t.Errorf("explicit https:// must be preserved (operator TLS-proxy override), got %q", got)
	}
	if got := normalizeBaseURL("http://slurpit.example"); got != "http://slurpit.example" {
		t.Errorf("explicit http:// must be preserved, got %q", got)
	}
	if got := normalizeBaseURL("192.0.2.57:80"); got != "http://192.0.2.57:80" {
		t.Errorf("a scheme-less host:port must default to http://, got %q", got)
	}
	if got := normalizeBaseURL(""); got != "" {
		t.Errorf("an empty URL must stay empty (the source is then unregistered), got %q", got)
	}
}

// TestPaginationFollowsOffsetAndStopsOnShortPage proves the reader walks offset pages and terminates on the
// first short page (fewer than pageLimit rows), rather than looping or stopping after one page.
func TestPaginationFollowsOffsetAndStopsOnShortPage(t *testing.T) {
	t.Setenv("TG_TEST_SLURPIT_TOKEN", "t")
	page0 := fullPage(pageLimit) // a full page ⇒ there is more
	var offsets []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		off, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		offsets = append(offsets, off)
		if off == 0 {
			io.WriteString(w, page0)
			return
		}
		io.WriteString(w, `[{"hostname":"tail-1"},{"hostname":"tail-2"}]`) // short page ⇒ the end
	}))
	defer srv.Close()

	src := New(srv.URL, config.SecretRef("env:TG_TEST_SLURPIT_TOKEN"))
	devices, err := src.fetchDevices(context.Background())
	if err != nil {
		t.Fatalf("fetchDevices: %v", err)
	}
	if len(devices) != pageLimit+2 {
		t.Errorf("expected %d devices across two pages, got %d", pageLimit+2, len(devices))
	}
	if len(offsets) != 2 || offsets[0] != 0 || offsets[1] != pageLimit {
		t.Errorf("pagination must follow offsets [0, %d], got %v", pageLimit, offsets)
	}
}

// TestPaginationCapBoundsAnUnboundedInstance is the INV-08 oracle. Against a server that ALWAYS returns a full
// page (an inventory that never terminates on its own — or a compromised one), the reader must stop after the
// hard page cap rather than pulling unbounded, so a single refresh materialises a bounded surface.
//
// KILLING MUTATION: remove the `page < maxPages` bound from fetchDevices. The server never returns a short
// page, so the loop would run forever — the test would hang instead of asserting the exact cap.
func TestPaginationCapBoundsAnUnboundedInstance(t *testing.T) {
	t.Setenv("TG_TEST_SLURPIT_TOKEN", "t")
	page := fullPage(pageLimit)
	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		io.WriteString(w, page) // always full — the reader must impose its own bound
	}))
	defer srv.Close()

	src := New(srv.URL, config.SecretRef("env:TG_TEST_SLURPIT_TOKEN"))
	devices, err := src.fetchDevices(context.Background())
	if err != nil {
		t.Fatalf("fetchDevices: %v", err)
	}
	if requests != maxPages {
		t.Errorf("INV-08: an unbounded instance must be capped at exactly %d requests, got %d", maxPages, requests)
	}
	if len(devices) != pageLimit*maxPages {
		t.Errorf("expected the bounded prefix of %d devices, got %d", pageLimit*maxPages, len(devices))
	}
}
