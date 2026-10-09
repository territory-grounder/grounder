package netbox

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/config"
)

type fakeDoer struct {
	auth string
}

func (f *fakeDoer) Do(req *http.Request) (*http.Response, error) {
	f.auth = req.Header.Get("Authorization")
	body := "{}"
	switch {
	case strings.Contains(req.URL.Path, "/api/dcim/devices/42/"):
		body = `{"id":42,"name":"sw-core-01","display":"sw-core-01","status":{"value":"active"},"site":{"name":"NL"}}`
	case strings.Contains(req.URL.Path, "/api/extras/object-changes/"):
		body = `{"results":[{"id":9,"action":"updated","time":"2026-07-15T12:00:00Z","user_name":"ops","object_repr":"sw-core-01"}]}`
	default:
		return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader("not found")), Header: make(http.Header)}, nil
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
}

func newFixture(t *testing.T) (*Module, *fakeDoer) {
	t.Setenv("TG_TEST_NETBOX_TOKEN", "nb_secret")
	f := &fakeDoer{}
	return New("https://netbox.example/", config.SecretRef("env:TG_TEST_NETBOX_TOKEN"), WithHTTPClient(f)), f
}

func TestResolveReReadsDeviceByID(t *testing.T) {
	m, f := newFixture(t)
	e, err := m.Resolve(context.Background(), "device", "42")
	if err != nil {
		t.Fatalf("Resolve must succeed: %v", err)
	}
	if e.ID != "42" || e.Kind != "device" || e.Name != "sw-core-01" {
		t.Errorf("entity mismatch: %+v", e)
	}
	if e.Attributes["site"] != "NL" || e.Attributes["status"] != "active" {
		t.Errorf("attributes not resolved: %+v", e.Attributes)
	}
	if f.auth != "Token nb_secret" {
		t.Errorf("must authenticate with the resolved NetBox token, got %q", f.auth)
	}
}

func TestChangelogExposesHistory(t *testing.T) {
	m, _ := newFixture(t)
	changes, err := m.Changelog(context.Background(), "42")
	if err != nil {
		t.Fatalf("Changelog must succeed: %v", err)
	}
	if len(changes) != 1 || changes[0].Action != "updated" {
		t.Errorf("changelog not exposed: %+v", changes)
	}
}

func TestUnsupportedKindAndEmptyIDRejected(t *testing.T) {
	m, _ := newFixture(t)
	if _, err := m.Resolve(context.Background(), "planet", "1"); err == nil {
		t.Fatal("an unsupported entity kind must be rejected")
	}
	if _, err := m.Resolve(context.Background(), "device", ""); err == nil {
		t.Fatal("an empty id must be rejected")
	}
}
