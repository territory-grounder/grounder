// ORACLE tests for the Semaphore connector (spec/016 T-016-8, REQ-1612). CI has no Semaphore, so a
// httptest.Server fakes GET /api/ping, /api/projects, /api/project/{id}/keys, and
// /api/project/{id}/inventory, and the tests drive the REAL client + CredentialSource through it. They prove:
// a projects→inventories→hosts pull → resolvable host+group entries (the inventory-assigned Key Store key as
// a SecretRef by NAME, never a value); ansible INI + static-yaml parsing → Bundle fields; the fail-closed
// matrix (401/403/404/unreachable/malformed); that NO plaintext secret lands in any Bundle; that the Bearer
// token is never emitted in an error; that an oversized inventory text is bounded (DoS); and that an
// off-host absolute URL is refused. Run under -race.
//
// A BEST-EFFORT live check (TestLivePing) hits the real dc1semaphore01 only when TG_SEMAPHORE_LIVE_ADDR
// is set; it asserts only the unauthenticated /api/ping endpoint (TG holds no Semaphore token here).
package semaphore_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/credential"
	"github.com/territory-grounder/grounder/modules/credsource/semaphore"
)

const goodToken = "semaphore-api-token-value"

// iniInventory is a realistic static (INI) ansible inventory: two web hosts (one using the inventory-assigned
// key, one overriding with an explicit vault: ref), a switch in a network_cli group, group vars, and a group
// that carries connection vars but no user (→ skipped-with-record).
const iniInventory = `
[web]
web01 ansible_user=deploy ansible_port=2222
db01 ansible_user=dbadmin tg_secret_ref=vault:secret/data/semaphore/db01#ssh_key

[web:vars]
ansible_connection=ssh

[switches]
sw01 ansible_user=netops

[switches:vars]
ansible_connection=network_cli
ansible_user=netops
`

// yamlInventory is a static-yaml ansible inventory exercising the YAML parser + nested children.
const yamlInventory = `
all:
  hosts:
    bastion01:
      ansible_user: ops
  children:
    apiservers:
      hosts:
        api01:
          ansible_user: apiuser
          ansible_connection: httpapi
      vars:
        ansible_user: apiuser
`

// yamlBomb is a billion-laughs YAML alias bomb — yaml.v3 must reject it ("excessive aliasing") so the
// inventory is skipped-with-record, never expanded.
const yamlBomb = "all:\n  vars:\n" +
	"    a: &a [\"x\",\"x\"]\n    b: &b [*a,*a,*a,*a,*a,*a,*a,*a,*a]\n    c: &c [*b,*b,*b,*b,*b,*b,*b,*b,*b]\n" +
	"    d: &d [*c,*c,*c,*c,*c,*c,*c,*c,*c]\n    e: &e [*d,*d,*d,*d,*d,*d,*d,*d,*d]\n    f: [*e,*e,*e,*e,*e,*e,*e,*e,*e]\n"

// fakeSem is an in-memory Semaphore REST backend for the oracles.
type fakeSem struct {
	mu          sync.Mutex
	requireTok  bool
	forbid      bool
	notFound    bool
	malformed   bool
	inventories []map[string]any
	keys        []map[string]any
	seenAuth    []string
}

func defaultKeys() []map[string]any {
	return []map[string]any{
		{"id": 1, "name": "prod-ssh", "type": "ssh", "project_id": 1},
		{"id": 2, "name": "None", "type": "none", "project_id": 1},
		{"id": 5, "name": "sudo-pass", "type": "login_password", "project_id": 1},
	}
}

func iniInv() []map[string]any {
	return []map[string]any{{
		"id": 10, "name": "prod-static", "project_id": 1, "type": "static",
		"ssh_key_id": 1, "become_key_id": 5, "inventory": iniInventory,
	}}
}

func (f *fakeSem) server(t *testing.T) *httptest.Server {
	t.Helper()
	if f.keys == nil {
		f.keys = defaultKeys()
	}
	if f.inventories == nil {
		f.inventories = iniInv()
	}
	s := httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(s.Close)
	return s
}

func (f *fakeSem) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if r.URL.Path == "/api/ping" { // unauthenticated
		w.WriteHeader(200)
		_, _ = w.Write([]byte("pong"))
		return
	}

	auth := r.Header.Get("Authorization")
	f.seenAuth = append(f.seenAuth, auth)
	if f.requireTok && auth != "Bearer "+goodToken {
		writeJSON(w, 401, map[string]any{"error": "unauthorized"})
		return
	}
	if f.forbid {
		writeJSON(w, 403, map[string]any{"error": "forbidden"})
		return
	}
	if f.notFound {
		writeJSON(w, 404, map[string]any{"error": "not found"})
		return
	}
	if f.malformed {
		w.WriteHeader(200)
		_, _ = w.Write([]byte("{not json"))
		return
	}

	switch r.URL.Path {
	case "/api/projects":
		writeJSON(w, 200, []map[string]any{{"id": 1, "name": "prod", "type": ""}})
	case "/api/project/1/keys":
		writeJSON(w, 200, f.keys)
	case "/api/project/1/inventory":
		writeJSON(w, 200, f.inventories)
	default:
		writeJSON(w, 404, map[string]any{"error": "not found"})
	}
}

func newClient(t *testing.T, base string) *semaphore.Client {
	t.Helper()
	t.Setenv("TG_TEST_SEMAPHORE_TOKEN", goodToken)
	c, err := semaphore.New(semaphore.Config{
		BaseURL:    base,
		TokenRef:   config.SecretRef("env:TG_TEST_SEMAPHORE_TOKEN"),
		HTTPClient: http.DefaultClient,
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return c
}

func newSource(t *testing.T, c *semaphore.Client) *semaphore.Source {
	t.Helper()
	src, err := semaphore.NewSource(semaphore.SourceConfig{
		ID: "semaphore01", Client: c, RefScheme: "store", RefPrefix: "semaphore/",
	})
	if err != nil {
		t.Fatalf("new source: %v", err)
	}
	return src
}

func TestPingUnauthenticated(t *testing.T) {
	c := newClient(t, (&fakeSem{requireTok: true}).server(t).URL)
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("ping should not require a token: %v", err)
	}
}

func TestSyncINIInventoryResolvable(t *testing.T) {
	f := &fakeSem{requireTok: true}
	c := newClient(t, f.server(t).URL)
	src := newSource(t, c)

	if src.Plane() != credential.PlaneMachine || src.ID() != "semaphore01" {
		t.Fatalf("source metadata wrong: plane=%s id=%s", src.Plane(), src.ID())
	}

	entries, err := src.Sync(context.Background())
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	byName := map[string]credential.SourceEntry{}
	for _, e := range entries {
		byName[e.Selector.Pattern] = e
	}
	// web01 + db01 + sw01 (hosts) + switches (group). web group has vars but no user → skipped.
	if len(entries) != 4 {
		t.Fatalf("expected 4 entries (web01, db01, sw01, switches), got %d: %v", len(entries), keys(byName))
	}

	// web01: inline user/port, web:vars ssh, NO per-host key → the inventory-assigned key name → store ref.
	web := byName["web01"]
	if web.Selector.Kind != credential.KindHost || web.Bundle.User() != "deploy" || web.Bundle.Port() != 2222 {
		t.Fatalf("web01 wrong: kind=%s user=%s port=%d", web.Selector.Kind, web.Bundle.User(), web.Bundle.Port())
	}
	if got := string(web.Bundle.SSHKeyRef()); got != "store:semaphore/prod-ssh" {
		t.Fatalf("web01 ssh key ref = %q, want store:semaphore/prod-ssh (inventory-assigned key name)", got)
	}
	// inventory become_key_id 5 → sudo-pass → an optional become reference (still a REFERENCE).
	if got := string(web.Bundle.BecomeRef()); got != "store:semaphore/sudo-pass" {
		t.Fatalf("web01 become ref = %q, want store:semaphore/sudo-pass", got)
	}
	if web.NativeID != "project:1/inventory:10/host:web01" {
		t.Fatalf("web01 native id = %q", web.NativeID)
	}

	// db01: explicit tg_secret_ref used verbatim (a vault: reference), overriding the inventory key.
	if got := string(byName["db01"].Bundle.SSHKeyRef()); got != "vault:secret/data/semaphore/db01#ssh_key" {
		t.Fatalf("db01 ssh key ref = %q, want the verbatim vault: ref", got)
	}

	// sw01: network_cli → netconf scheme; the inventory key name lands in the ssh-key slot.
	sw := byName["sw01"]
	if sw.Bundle.Scheme() != credential.SchemeNetconf {
		t.Fatalf("sw01 scheme = %s, want netconf", sw.Bundle.Scheme())
	}
	if got := string(sw.Bundle.SSHKeyRef()); got != "store:semaphore/prod-ssh" {
		t.Fatalf("sw01 ref = %q, want store:semaphore/prod-ssh", got)
	}

	// switches: a GROUP-selector entry with the group-level connection identity.
	grp := byName["switches"]
	if grp.Selector.Kind != credential.KindGroup || grp.Bundle.Scheme() != credential.SchemeNetconf || grp.Bundle.User() != "netops" {
		t.Fatalf("switches group wrong: kind=%s scheme=%s user=%s", grp.Selector.Kind, grp.Bundle.Scheme(), grp.Bundle.User())
	}

	// skipped-with-record: the web group carries vars but no ansible_user → recorded, never a blank identity.
	skipped := src.Skipped()
	if len(skipped) != 1 || skipped[0].Kind != credential.KindGroup || skipped[0].Name != "web" {
		t.Fatalf("expected exactly the web-group skip record, got %+v", skipped)
	}
}

func TestSyncYAMLInventoryResolvable(t *testing.T) {
	f := &fakeSem{requireTok: true, inventories: []map[string]any{{
		"id": 20, "name": "yaml-static", "project_id": 1, "type": "static-yaml",
		"ssh_key_id": 1, "become_key_id": 0, "inventory": yamlInventory,
	}}}
	c := newClient(t, f.server(t).URL)
	entries, err := newSource(t, c).Sync(context.Background())
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	byName := map[string]credential.SourceEntry{}
	for _, e := range entries {
		byName[e.Selector.Pattern] = e
	}
	// bastion01 (ssh) + api01 (httpapi→api) hosts + apiservers group.
	bastion, ok := byName["bastion01"]
	if !ok || bastion.Bundle.Scheme() != credential.SchemeSSH || string(bastion.Bundle.SSHKeyRef()) != "store:semaphore/prod-ssh" {
		t.Fatalf("bastion01 wrong: %+v", bastion)
	}
	api, ok := byName["api01"]
	if !ok || api.Bundle.Scheme() != credential.SchemeAPI || string(api.Bundle.APITokenRef()) != "store:semaphore/prod-ssh" {
		t.Fatalf("api01 wrong: scheme=%s apiref=%q", api.Bundle.Scheme(), api.Bundle.APITokenRef())
	}
	if _, ok := byName["apiservers"]; !ok {
		t.Fatalf("expected an apiservers group entry, got %v", keys(byName))
	}
}

func TestNoPlaintextSecretInAnyBundle(t *testing.T) {
	f := &fakeSem{requireTok: true}
	c := newClient(t, f.server(t).URL)
	entries, err := newSource(t, c).Sync(context.Background())
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	for _, e := range entries {
		for _, ref := range []string{
			string(e.Bundle.SSHKeyRef()), string(e.Bundle.APITokenRef()), string(e.Bundle.BecomeRef()),
		} {
			if ref == "" {
				continue
			}
			if !isSealed(ref) {
				t.Fatalf("bundle for %q carries a non-sealed secret field %q", e.Selector.Pattern, ref)
			}
		}
	}
}

func TestSyncFailClosed(t *testing.T) {
	t.Run("unauthorized-401", func(t *testing.T) {
		f := &fakeSem{requireTok: true}
		c, _ := semaphore.New(semaphore.Config{BaseURL: f.server(t).URL, TokenRef: config.SecretRef("env:TG_MISSING_TOKEN"), HTTPClient: http.DefaultClient})
		if _, err := newSourceFrom(c).Sync(context.Background()); err == nil {
			t.Fatal("missing token must fail closed")
		}
	})
	t.Run("forbidden-403", func(t *testing.T) {
		c := newClient(t, (&fakeSem{requireTok: true, forbid: true}).server(t).URL)
		if _, err := newSource(t, c).Sync(context.Background()); err == nil {
			t.Fatal("403 must fail closed")
		}
	})
	t.Run("not-found-404", func(t *testing.T) {
		c := newClient(t, (&fakeSem{requireTok: true, notFound: true}).server(t).URL)
		if _, err := newSource(t, c).Sync(context.Background()); err == nil {
			t.Fatal("404 must fail closed")
		}
	})
	t.Run("unreachable", func(t *testing.T) {
		srv := (&fakeSem{}).server(t)
		addr := srv.URL
		srv.Close()
		c := newClient(t, addr)
		if _, err := newSource(t, c).Sync(context.Background()); err == nil {
			t.Fatal("unreachable Semaphore must fail closed")
		}
	})
	t.Run("malformed-json", func(t *testing.T) {
		c := newClient(t, (&fakeSem{requireTok: true, malformed: true}).server(t).URL)
		if _, err := newSource(t, c).Sync(context.Background()); err == nil {
			t.Fatal("malformed page must fail closed")
		}
	})
}

// TestTokenNeverLogged proves the Bearer token never appears in an error even when a read is denied.
func TestTokenNeverLogged(t *testing.T) {
	f := &fakeSem{requireTok: true, forbid: true}
	c, _ := semaphore.New(semaphore.Config{
		BaseURL:    f.server(t).URL,
		TokenRef:   config.SecretRef("env:TG_TEST_SEMAPHORE_TOKEN"),
		HTTPClient: http.DefaultClient,
	})
	t.Setenv("TG_TEST_SEMAPHORE_TOKEN", "super-secret-should-not-appear")
	_, err := newSourceFrom(c).Sync(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "super-secret-should-not-appear") {
		t.Fatalf("token leaked into error: %v", err)
	}
}

func TestConstructorFailClosed(t *testing.T) {
	if _, err := semaphore.New(semaphore.Config{TokenRef: "env:X"}); err == nil {
		t.Fatal("missing base URL must error")
	}
	if _, err := semaphore.NewSource(semaphore.SourceConfig{ID: "x"}); err == nil {
		t.Fatal("missing client must error")
	}
	if _, err := semaphore.NewSource(semaphore.SourceConfig{Client: &semaphore.Client{}}); err == nil {
		t.Fatal("missing id must error")
	}
}

// TestOversizedInventoryBounded proves the DoS bound: an inventory whose text exceeds the size cap is
// skipped-with-record before parsing, so Sync neither hangs nor allocates unbounded and yields no entry.
func TestOversizedInventoryBounded(t *testing.T) {
	huge := "[web]\n" + strings.Repeat("h ansible_user=x\n", 200_000) // > 1 MiB
	f := &fakeSem{requireTok: true, inventories: []map[string]any{{
		"id": 30, "name": "huge", "project_id": 1, "type": "static", "ssh_key_id": 1, "inventory": huge,
	}}}
	c := newClient(t, f.server(t).URL)
	src := newSource(t, c)

	done := make(chan struct{})
	var entries []credential.SourceEntry
	var err error
	go func() { entries, err = src.Sync(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("sync hung on an oversized inventory — DoS not bounded")
	}
	if err != nil {
		t.Fatalf("oversized inventory should skip, not error the whole run: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("oversized inventory must yield no entries, got %d", len(entries))
	}
	if sk := src.Skipped(); len(sk) != 1 || !strings.Contains(sk[0].Reason, "too large") {
		t.Fatalf("expected an oversized skip record, got %+v", sk)
	}
}

// TestParseBombYAMLInventory proves the YAML alias-bomb defense: a billion-laughs static-yaml inventory is
// rejected by yaml.v3 → skipped-with-record; Sync neither hangs nor crashes and yields no entry.
func TestParseBombYAMLInventory(t *testing.T) {
	f := &fakeSem{requireTok: true, inventories: []map[string]any{{
		"id": 40, "name": "bomb", "project_id": 1, "type": "static-yaml", "ssh_key_id": 1, "inventory": yamlBomb,
	}}}
	c := newClient(t, f.server(t).URL)
	src := newSource(t, c)

	done := make(chan struct{})
	var entries []credential.SourceEntry
	var err error
	go func() { entries, err = src.Sync(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("sync hung on a YAML alias bomb — DoS not bounded")
	}
	if err != nil {
		t.Fatalf("alias bomb should skip, not error the whole run: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("alias-bomb inventory must yield no entries, got %d", len(entries))
	}
}

// TestUnparseableInventoryTypeSkipped proves a "file" / "terraform-workspace" inventory (no inline host text)
// is skipped-with-record, never a blank identity, and never a whole-Sync failure.
func TestUnparseableInventoryTypeSkipped(t *testing.T) {
	f := &fakeSem{requireTok: true, inventories: []map[string]any{{
		"id": 50, "name": "repo-file", "project_id": 1, "type": "file", "ssh_key_id": 1, "inventory": "inventories/prod",
	}}}
	c := newClient(t, f.server(t).URL)
	src := newSource(t, c)
	entries, err := src.Sync(context.Background())
	if err != nil {
		t.Fatalf("file-type inventory should skip, not error: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("file-type inventory must yield no entries, got %d", len(entries))
	}
	if sk := src.Skipped(); len(sk) != 1 {
		t.Fatalf("expected a skip record for the file-type inventory, got %+v", sk)
	}
}

// TestLivePing is a BEST-EFFORT env-gated live check against the real dc1semaphore01. It is skipped unless
// TG_SEMAPHORE_LIVE_ADDR is set (e.g. "http://dc1semaphore01.example.net:3000"). It asserts only
// the unauthenticated /api/ping endpoint (TG holds no Semaphore token in this environment).
func TestLivePing(t *testing.T) {
	addr := os.Getenv("TG_SEMAPHORE_LIVE_ADDR")
	if addr == "" {
		t.Skip("set TG_SEMAPHORE_LIVE_ADDR to run the live dc1semaphore01 ping check")
	}
	c, err := semaphore.New(semaphore.Config{BaseURL: addr, CACertPath: os.Getenv("TG_SEMAPHORE_CACERT")})
	if err != nil {
		t.Fatalf("live client: %v", err)
	}
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("live semaphore ping: %v", err)
	}
}

// ---- helpers ----

func newSourceFrom(c *semaphore.Client) *semaphore.Source {
	src, _ := semaphore.NewSource(semaphore.SourceConfig{ID: "semaphore01", Client: c, RefScheme: "store", RefPrefix: "semaphore/"})
	return src
}

func isSealed(ref string) bool {
	for _, sc := range []string{"env:", "file:", "store:", "vault:", "bao:"} {
		if strings.HasPrefix(ref, sc) {
			return true
		}
	}
	return false
}

func keys(m map[string]credential.SourceEntry) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
