package openbao_test

// ORACLE test for the OpenBao wrapper: proves the bao: SecretRef scheme resolves KV v2 read-only and fails
// closed, and that it is the same wire client as the vault package (OpenBao is API-compatible).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/credential"
	"github.com/territory-grounder/grounder/modules/credsource/openbao"
)

func fakeServer(t *testing.T, deny bool) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/v1/")
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(path, "/login") {
			_ = json.NewEncoder(w).Encode(map[string]any{"auth": map[string]any{"client_token": "tok-1", "lease_duration": 3600}})
			return
		}
		if r.Header.Get("X-Vault-Token") != "tok-1" || deny {
			w.WriteHeader(403)
			_ = json.NewEncoder(w).Encode(map[string]any{"errors": []string{"permission denied"}})
			return
		}
		if path == "secret/metadata/hosts" {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"keys": []string{"hostA"}}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"data": map[string]any{"user": "baouser", "ssh_key": "KEY-A"}}})
	}))
	t.Cleanup(s.Close)
	return s
}

func newClient(t *testing.T, url string) *openbao.Client {
	t.Helper()
	t.Setenv("TG_TEST_TOKEN", "tok-1")
	c, err := openbao.New(openbao.Config{BaseURL: url, Auth: openbao.Token{TokenRef: config.SecretRef("env:TG_TEST_TOKEN")}, HTTPClient: http.DefaultClient})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	return c
}

func TestBaoSchemeResolvesReadOnly(t *testing.T) {
	c := newClient(t, fakeServer(t, false).URL)
	openbao.RegisterResolver(c)
	t.Cleanup(func() { openbao.RegisterResolver(nil) })

	got, err := config.SecretRef("bao:secret/data/hosts/hostA#ssh_key").Resolve()
	if err != nil {
		t.Fatalf("resolve bao ref: %v", err)
	}
	if got != "KEY-A" {
		t.Fatalf("got %q want KEY-A", got)
	}
}

func TestBaoSyncEmitsBaoRefs(t *testing.T) {
	c := newClient(t, fakeServer(t, false).URL)
	src, err := openbao.NewSource(openbao.SourceConfig{ID: "bao01", Client: c, Mount: "secret", Prefix: "hosts"})
	if err != nil {
		t.Fatalf("new source: %v", err)
	}
	entries, err := src.Sync(context.Background())
	if err != nil || len(entries) != 1 {
		t.Fatalf("sync: entries=%d err=%v", len(entries), err)
	}
	if ref := string(entries[0].Bundle.SSHKeyRef()); !strings.HasPrefix(ref, "bao:") {
		t.Fatalf("expected a bao: ref, got %q", ref)
	}
	if src.Plane() != credential.PlaneMachine {
		t.Fatalf("openbao source must feed the machine plane")
	}
}

func TestBaoFailsClosed(t *testing.T) {
	c := newClient(t, fakeServer(t, true).URL)
	if _, err := c.ResolveRef("bao:secret/data/hosts/hostA#ssh_key"); err == nil {
		t.Fatal("denied bao read must fail closed")
	}
}
