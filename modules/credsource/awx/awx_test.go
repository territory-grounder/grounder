// ORACLE tests for the AWX connector (spec/016 T-016-8, REQ-1612). CI has no AWX, so a httptest.Server fakes
// GET /api/v2/ping/, /api/v2/hosts/ (paginated, with ansible connection vars), and /api/v2/groups/, and the
// tests drive the REAL client + CredentialSource through it. They prove: a paginated inventory pull →
// resolvable host+group entries; ansible_user/port/connection/key vars → Bundle fields (the key as a
// SecretRef, never a value); the fail-closed matrix (401/403/404/unreachable/malformed); that NO plaintext
// secret lands in any Bundle; and that the Bearer token is never emitted in an error/log. Run under -race.
//
// A BEST-EFFORT live check (TestLivePing) hits the real awx.example.net only when TG_AWX_LIVE_ADDR
// is set; it asserts only the unauthenticated /api/v2/ping/ endpoint (TG holds no AWX token here).
package awx_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/credential"
	"github.com/territory-grounder/grounder/modules/credsource/awx"
)

const goodToken = "oauth2-secret-token-value"

// fakeAWX is an in-memory AWX REST backend for the oracles. Hosts are served two-per-page to exercise
// pagination; the connection vars mix JSON and YAML .variables strings.
type fakeAWX struct {
	mu          sync.Mutex
	requireTok  bool   // when true, a request without the correct Bearer token gets 401
	forbid      bool   // force every authed read to 403
	notFound    bool   // force every list to 404
	offHostNext string // when set, page 1's .next is this (absolute) URL — exercises off-host rejection
	bombVars    bool   // when set, a host on page 1 carries a billion-laughs .variables blob
	credError   bool   // when set, /credentials/ returns 500 (JT-mode fail-closed) while hosts/groups succeed
	dupJT       bool   // when set, a SECOND JT binds inventory 1 to cred 100 (dedup fixture: identical entry)
	invError    bool   // when set, /inventories/ returns 500 (membership fail-closed)
	seenAuth    []string
}

func (f *fakeAWX) server(t *testing.T) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(s.Close)
	return s
}

func (f *fakeAWX) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if r.URL.Path == "/api/v2/ping/" { // unauthenticated
		writeJSON(w, 200, map[string]any{"ha": false, "version": "24.6.1", "active_node": "awx-1"})
		return
	}

	auth := r.Header.Get("Authorization")
	f.seenAuth = append(f.seenAuth, auth)
	if f.requireTok && auth != "Bearer "+goodToken {
		writeJSON(w, 401, map[string]any{"detail": "Authentication credentials were not provided."})
		return
	}
	if f.forbid {
		writeJSON(w, 403, map[string]any{"detail": "You do not have permission to perform this action."})
		return
	}
	if f.notFound {
		writeJSON(w, 404, map[string]any{"detail": "Not found."})
		return
	}

	switch r.URL.Path {
	case "/api/v2/hosts/":
		f.serveHosts(w, r)
	case "/api/v2/groups/":
		writeJSON(w, 200, map[string]any{"count": 2, "next": nil, "previous": nil, "results": []any{
			// group-level defaults: a group carrying a full connection identity (→ a group-selector entry).
			map[string]any{"id": 9, "name": "edge-switches", "inventory": 1,
				"variables": "ansible_user: netops\nansible_connection: network_cli\ntg_credential: netops-key\n"},
			// a purely organizational group with no connection info → skipped-with-record, not an entry.
			map[string]any{"id": 10, "name": "datacenter-a", "inventory": 1, "variables": ""},
		}})
	case "/api/v2/credentials/":
		f.serveCredentials(w, r)
	case "/api/v2/job_templates/":
		f.serveJobTemplates(w, r)
	case "/api/v2/inventories/":
		f.serveInventories(w, r)
	default:
		if strings.HasPrefix(r.URL.Path, "/api/v2/inventories/") && strings.HasSuffix(r.URL.Path, "/hosts/") {
			f.serveInventoryHosts(w, r)
			return
		}
		writeJSON(w, 404, map[string]any{"detail": "Not found."})
	}
}

// serveInventories fakes /api/v2/inventories/. The inventory NAMES match the JT-mode inventory names so a
// host's inventory membership maps to the same group a JT bundle is keyed by. NON-SECRET: id + name only.
func (f *fakeAWX) serveInventories(w http.ResponseWriter, _ *http.Request) {
	if f.invError {
		writeJSON(w, 500, map[string]any{"detail": "Internal server error."})
		return
	}
	inv := func(id int, name string) map[string]any { return map[string]any{"id": id, "name": name} }
	writeJSON(w, 200, map[string]any{"count": 3, "next": nil, "previous": nil, "results": []any{
		inv(1, "Cert Sync Targets"),
		inv(4, "HAProxy Targets"),
		inv(5, "Weird Targets"),
	}})
}

// serveInventoryHosts fakes /api/v2/inventories/<id>/hosts/ — the flattened host membership of one inventory.
// Inventory 1 ("Cert Sync Targets") intentionally includes bare01 and old01: hosts that Sync SKIPS as
// host-selector entries (no key ref / disabled) yet are still inventory MEMBERS — proving a host with no
// host rule still resolves its inventory's group bundle via membership. NON-SECRET: names only.
func (f *fakeAWX) serveInventoryHosts(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v2/inventories/"), "/hosts/")
	host := func(id int, name string) map[string]any { return map[string]any{"id": id, "name": name} }
	var results []any
	switch id {
	case "1":
		results = []any{host(1, "web01"), host(2, "db01"), host(3, "old01"), host(4, "bare01")}
	case "4":
		results = []any{host(20, "ha01")}
	case "5":
		results = []any{host(30, "weird01")}
	}
	writeJSON(w, 200, map[string]any{"count": len(results), "next": nil, "previous": nil, "results": results})
}

// serveCredentials fakes /api/v2/credentials/. Machine creds carry a plaintext inputs.username and their
// secret fields read back "$encrypted$" (as AWX does); one Machine cred carries a "$encrypted$" username to
// prove the defensive fallback, and one non-Machine (Vault) cred proves the Machine filter.
func (f *fakeAWX) serveCredentials(w http.ResponseWriter, _ *http.Request) {
	if f.credError {
		writeJSON(w, 500, map[string]any{"detail": "Internal server error."})
		return
	}
	machine := func(id int, name, user string) map[string]any {
		return map[string]any{"id": id, "name": name,
			"summary_fields": map[string]any{"credential_type": map[string]any{"name": "Machine"}},
			"inputs":         map[string]any{"username": user, "ssh_key_data": "$encrypted$", "password": "$encrypted$"}}
	}
	writeJSON(w, 200, map[string]any{"count": 5, "next": nil, "previous": nil, "results": []any{
		machine(100, "SSH ED25519 (one_key)", "root"),
		machine(101, "SSH Lab Common", "labadmin"),
		machine(104, "SSH Weird", "$encrypted$"), // username reads back encrypted → falls back to default user
		machine(103, "SSH Unmapped", ""),         // Machine but not in the operator's CredRefMap → skip
		// a non-Machine credential type → must be ignored by the Machine filter.
		map[string]any{"id": 102, "name": "Some Vault Cred",
			"summary_fields": map[string]any{"credential_type": map[string]any{"name": "Vault"}},
			"inputs":         map[string]any{"vault_password": "$encrypted$"}},
	}})
}

// serveJobTemplates fakes /api/v2/job_templates/ with summary_fields.inventory + summary_fields.credentials.
func (f *fakeAWX) serveJobTemplates(w http.ResponseWriter, _ *http.Request) {
	cred := func(id int, name string) map[string]any { return map[string]any{"id": id, "name": name} }
	jt := func(id int, name string, inv map[string]any, creds ...map[string]any) map[string]any {
		cs := make([]any, 0, len(creds))
		for _, c := range creds {
			cs = append(cs, c)
		}
		return map[string]any{"id": id, "name": name,
			"summary_fields": map[string]any{"inventory": inv, "credentials": cs}}
	}
	inv := func(id int, name string) map[string]any { return map[string]any{"id": id, "name": name} }
	results := []any{
		// mapped machine cred → one group(inventory) entry, user "root".
		jt(200, "Cert Sync - Proxmox", inv(1, "Cert Sync Targets"), cred(100, "SSH ED25519 (one_key)")),
		// Machine cred NOT in the map → skip-with-record (no entry, no blank identity).
		jt(201, "ChatOps Maintenance Mode", inv(2, "Localhost Only"), cred(103, "SSH Unmapped")),
		// no inventory (summary_fields.inventory null) → skip.
		jt(202, "Rogue Playbook", nil, cred(100, "SSH ED25519 (one_key)")),
		// has an inventory but only a non-Machine credential → skip.
		jt(203, "Vault Rotate", inv(3, "Vault Targets"), cred(102, "Some Vault Cred")),
		// TWO mapped machine creds on one inventory → two entries, same group selector (ambiguous at resolve).
		jt(204, "Cert Sync - HAProxy", inv(4, "HAProxy Targets"),
			cred(100, "SSH ED25519 (one_key)"), cred(101, "SSH Lab Common")),
		// mapped machine cred whose username reads "$encrypted$" → user falls back to the default.
		jt(205, "Weird Job", inv(5, "Weird Targets"), cred(104, "SSH Weird")),
	}
	if f.dupJT {
		// A SECOND JT binding the SAME inventory (1) to the SAME credential (100) → an IDENTICAL
		// (selector, bundle) — the connector must DEDUP it (no spurious equal-specificity ambiguity).
		results = append(results, jt(206, "Cert Sync - Proxmox (dup)", inv(1, "Cert Sync Targets"), cred(100, "SSH ED25519 (one_key)")))
	}
	writeJSON(w, 200, map[string]any{"count": len(results), "next": nil, "previous": nil, "results": results})
}

func (f *fakeAWX) serveHosts(w http.ResponseWriter, r *http.Request) {
	page := r.URL.Query().Get("page")
	// A billion-laughs .variables blob — must be refused by parseVars (skipped-with-record), never expanded.
	const bomb = "a: &a [\"x\",\"x\"]\nb: &b [*a,*a,*a,*a,*a,*a,*a,*a,*a]\nc: &c [*b,*b,*b,*b,*b,*b,*b,*b,*b]\n" +
		"d: &d [*c,*c,*c,*c,*c,*c,*c,*c,*c]\ne: &e [*d,*d,*d,*d,*d,*d,*d,*d,*d]\nf: [*e,*e,*e,*e,*e,*e,*e,*e,*e]\n"
	switch page {
	case "", "1":
		next := any("/api/v2/hosts/?page=2")
		if f.offHostNext != "" {
			next = f.offHostNext
		}
		if f.bombVars {
			// A single host whose vars are a parse bomb — Sync must skip it fail-closed and not hang/crash.
			writeJSON(w, 200, map[string]any{"count": 1, "previous": nil, "next": nil, "results": []any{
				map[string]any{"id": 7, "name": "bomb01", "enabled": true, "inventory": 1, "variables": bomb},
			}})
			return
		}
		writeJSON(w, 200, map[string]any{
			"count": 4, "previous": nil, "next": next, "results": []any{
				// JSON .variables with an explicit private key file → key REFERENCE by name.
				map[string]any{"id": 1, "name": "web01", "enabled": true, "inventory": 1,
					"variables": `{"ansible_user":"deploy","ansible_port":2222,"ansible_ssh_private_key_file":"/keys/web01_ed25519.pem"}`},
				// YAML .variables using tg_secret_ref → a verbatim sealed vault: reference.
				map[string]any{"id": 2, "name": "db01", "enabled": true, "inventory": 1,
					"variables": "ansible_user: dbadmin\ntg_secret_ref: vault:secret/data/awx/db01#ssh_key\n"},
			}})
	case "2":
		writeJSON(w, 200, map[string]any{
			"count": 4, "previous": "/api/v2/hosts/?page=1", "next": nil, "results": []any{
				// disabled host → skipped-with-record.
				map[string]any{"id": 3, "name": "old01", "enabled": false, "inventory": 1,
					"variables": `{"ansible_user":"x","ansible_ssh_private_key_file":"/k/x"}`},
				// enabled but NO derivable key reference → skipped-with-record (never a blank identity).
				map[string]any{"id": 4, "name": "bare01", "enabled": true, "inventory": 1,
					"variables": `{"ansible_user":"nobody"}`},
			}})
	default:
		writeJSON(w, 200, map[string]any{"count": 4, "next": nil, "previous": nil, "results": []any{}})
	}
}

func newClient(t *testing.T, base string) *awx.Client {
	t.Helper()
	t.Setenv("TG_TEST_AWX_TOKEN", goodToken)
	c, err := awx.New(awx.Config{
		BaseURL:    base,
		TokenRef:   config.SecretRef("env:TG_TEST_AWX_TOKEN"),
		HTTPClient: http.DefaultClient,
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return c
}

func newSource(t *testing.T, c *awx.Client) *awx.Source {
	t.Helper()
	src, err := awx.NewSource(awx.SourceConfig{
		ID: "awx01", Client: c, RefScheme: "store", RefPrefix: "awx/",
	})
	if err != nil {
		t.Fatalf("new source: %v", err)
	}
	return src
}

// jtCredRefMap is the operator's AWX-cred-name → sealed SecretRef map for the JT-mode tests. "SSH Unmapped"
// is deliberately absent to exercise skip-with-record.
func jtCredRefMap() map[string]string {
	return map[string]string{
		"SSH ED25519 (one_key)": "file:/secrets/one_key",
		"SSH Lab Common":        "file:/secrets/lab-common",
		"SSH Weird":             "file:/secrets/weird",
	}
}

func newJTSource(t *testing.T, c *awx.Client) *awx.Source {
	t.Helper()
	src, err := awx.NewSource(awx.SourceConfig{
		ID: "awx01", Client: c, RefScheme: "store", RefPrefix: "awx/",
		CredRefMap: jtCredRefMap(), DefaultUser: "svc-default",
	})
	if err != nil {
		t.Fatalf("new JT source: %v", err)
	}
	return src
}

// TestSyncJobTemplateGroupBundles proves the second mapping mode: a JT with an inventory + a MAPPED Machine
// credential emits a GROUP(inventory-name) entry whose SSHKeyRef is the operator-mapped SecretRef and whose
// user is the AWX credential's username (else the configured default); unmapped names, no-inventory, and
// no-Machine JTs are skipped-with-record (never a blank identity); and a multi-mapped-cred JT emits one entry
// per cred sharing the same group selector.
func TestSyncJobTemplateGroupBundles(t *testing.T) {
	f := &fakeAWX{requireTok: true}
	c := newClient(t, f.server(t).URL)
	src := newJTSource(t, c)

	entries, err := src.Sync(context.Background())
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	byNative := map[string]credential.SourceEntry{}
	for _, e := range entries {
		byNative[e.NativeID] = e
	}

	// JT 200: "Cert Sync Targets" ← one_key, user root.
	e := byNative["jt:200:cred:100"]
	if e.Selector.Kind != credential.KindGroup || e.Selector.Pattern != "Cert Sync Targets" {
		t.Fatalf("jt200 selector wrong: kind=%s pattern=%q", e.Selector.Kind, e.Selector.Pattern)
	}
	if e.Bundle.User() != "root" || e.Bundle.Port() != 22 || e.Bundle.Scheme() != credential.SchemeSSH {
		t.Fatalf("jt200 bundle wrong: user=%s port=%d scheme=%s", e.Bundle.User(), e.Bundle.Port(), e.Bundle.Scheme())
	}
	if got := string(e.Bundle.SSHKeyRef()); got != "file:/secrets/one_key" {
		t.Fatalf("jt200 ssh key ref = %q, want file:/secrets/one_key", got)
	}

	// JT 204: two mapped machine creds → two entries, SAME group selector (equal-specificity → ambiguous at
	// resolve; the connector never silently picks one).
	haproxy := []string{"jt:204:cred:100", "jt:204:cred:101"}
	for _, id := range haproxy {
		if _, ok := byNative[id]; !ok {
			t.Fatalf("expected multi-cred JT entry %s", id)
		}
		if byNative[id].Selector.Pattern != "HAProxy Targets" {
			t.Fatalf("%s selector = %q, want HAProxy Targets", id, byNative[id].Selector.Pattern)
		}
	}
	if u := byNative["jt:204:cred:101"].Bundle.User(); u != "labadmin" {
		t.Fatalf("jt204 lab cred user = %q, want labadmin", u)
	}

	// JT 205: "$encrypted$" username → default user "svc-default".
	if u := byNative["jt:205:cred:104"].Bundle.User(); u != "svc-default" {
		t.Fatalf("jt205 user = %q, want svc-default (encrypted-username fallback)", u)
	}

	// Unmapped (201), no-inventory (202), no-Machine (203) JTs produced NO entry.
	for _, absent := range []string{"jt:201:cred:103", "jt:202:cred:100", "jt:203:cred:102"} {
		if _, ok := byNative[absent]; ok {
			t.Fatalf("entry %s should have been skipped-with-record, not emitted", absent)
		}
	}

	// The JT skips are recorded with non-secret reasons.
	var haveUnmapped, haveNoInv, haveNoMachine bool
	for _, s := range src.Skipped() {
		switch {
		case strings.Contains(s.Reason, "no TG SecretRef mapped for AWX credential"):
			haveUnmapped = true
		case strings.Contains(s.Reason, "no inventory"):
			haveNoInv = true
		case strings.Contains(s.Reason, "no Machine credential"):
			haveNoMachine = true
		}
	}
	if !haveUnmapped || !haveNoInv || !haveNoMachine {
		t.Fatalf("missing a JT skip record: unmapped=%v noInv=%v noMachine=%v", haveUnmapped, haveNoInv, haveNoMachine)
	}
}

// TestJTModeDisabledByDefault proves that with NO CredRefMap the JT walk does not run — the connector behaves
// exactly as the host-only mode (zero behaviour change, pure fail-closed default).
func TestJTModeDisabledByDefault(t *testing.T) {
	f := &fakeAWX{requireTok: true}
	c := newClient(t, f.server(t).URL)
	entries, err := newSource(t, c).Sync(context.Background()) // newSource sets no CredRefMap
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.NativeID, "jt:") {
			t.Fatalf("JT entry %s emitted with no CredRefMap configured", e.NativeID)
		}
	}
}

// TestJTModeNoEncryptedOrTokenLeak proves no "$encrypted$", token, or key material ever reaches a Bundle
// field, user, or SkipRecord in JT mode.
func TestJTModeNoEncryptedOrTokenLeak(t *testing.T) {
	f := &fakeAWX{requireTok: true}
	c := newClient(t, f.server(t).URL)
	src := newJTSource(t, c)
	entries, err := src.Sync(context.Background())
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	banned := []string{"$encrypted$", goodToken}
	for _, e := range entries {
		fields := []string{e.Bundle.User(), string(e.Bundle.SSHKeyRef()), string(e.Bundle.APITokenRef()), string(e.Bundle.BecomeRef())}
		for _, fld := range fields {
			for _, b := range banned {
				if strings.Contains(fld, b) {
					t.Fatalf("entry %s field %q leaked %q", e.NativeID, fld, b)
				}
			}
		}
	}
	for _, s := range src.Skipped() {
		for _, b := range banned {
			if strings.Contains(s.Reason, b) {
				t.Fatalf("skip record %q leaked %q", s.Reason, b)
			}
		}
	}
}

// TestJTModeFailsClosed proves the JT walk fails the whole Sync closed when /credentials/ is unreadable (a
// partial/failed outcome, never a fail-open bundle), even though hosts/groups succeeded.
func TestJTModeFailsClosed(t *testing.T) {
	f := &fakeAWX{requireTok: true, credError: true}
	c := newClient(t, f.server(t).URL)
	src := newJTSource(t, c)
	if _, err := src.Sync(context.Background()); err == nil {
		t.Fatal("a 500 on /credentials/ must fail the whole Sync closed")
	}
}

func TestPingUnauthenticated(t *testing.T) {
	c := newClient(t, (&fakeAWX{requireTok: true}).server(t).URL)
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("ping should not require a token: %v", err)
	}
}

func TestSyncPaginatedInventoryResolvable(t *testing.T) {
	f := &fakeAWX{requireTok: true}
	c := newClient(t, f.server(t).URL)
	src := newSource(t, c)

	if src.Plane() != credential.PlaneMachine || src.ID() != "awx01" {
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
	// web01 + db01 (hosts) + edge-switches (group). old01 disabled and bare01 keyless are skipped.
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries (web01, db01, edge-switches), got %d: %v", len(entries), keys(byName))
	}

	// web01: ansible vars → user/port, key file → a store: REFERENCE keyed by the key name (never the key).
	web := byName["web01"]
	if web.Selector.Kind != credential.KindHost {
		t.Fatalf("web01 selector kind = %s, want host", web.Selector.Kind)
	}
	if web.Bundle.User() != "deploy" || web.Bundle.Port() != 2222 {
		t.Fatalf("web01 metadata wrong: user=%s port=%d", web.Bundle.User(), web.Bundle.Port())
	}
	ref := string(web.Bundle.SSHKeyRef())
	if ref != "store:awx/web01_ed25519" {
		t.Fatalf("web01 ssh key ref = %q, want store:awx/web01_ed25519", ref)
	}

	// db01: explicit tg_secret_ref used verbatim (a vault: reference).
	if got := string(byName["db01"].Bundle.SSHKeyRef()); got != "vault:secret/data/awx/db01#ssh_key" {
		t.Fatalf("db01 ssh key ref = %q, want the verbatim vault: ref", got)
	}

	// edge-switches: a GROUP-selector entry, network_cli → netconf scheme, tg_credential → store ref.
	grp := byName["edge-switches"]
	if grp.Selector.Kind != credential.KindGroup || grp.Bundle.Scheme() != credential.SchemeNetconf {
		t.Fatalf("edge-switches wrong: kind=%s scheme=%s", grp.Selector.Kind, grp.Bundle.Scheme())
	}
	if got := string(grp.Bundle.SSHKeyRef()); got != "store:awx/netops-key" {
		t.Fatalf("edge-switches ref = %q, want store:awx/netops-key", got)
	}

	// NativeID uniqueness across kinds (idempotency key half).
	if web.NativeID != "host:1" || grp.NativeID != "group:9" {
		t.Fatalf("native ids wrong: web=%s grp=%s", web.NativeID, grp.NativeID)
	}

	// skipped-with-record: old01 (disabled), bare01 (no key ref), datacenter-a (no connection identity)
	// are recorded, never injected as a blank identity.
	skipped := src.Skipped()
	if len(skipped) != 3 {
		t.Fatalf("expected 3 skip records, got %d: %+v", len(skipped), skipped)
	}
}

func TestNoPlaintextSecretInAnyBundle(t *testing.T) {
	f := &fakeAWX{requireTok: true}
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
		f := &fakeAWX{requireTok: true}
		c, _ := awx.New(awx.Config{BaseURL: f.server(t).URL, TokenRef: config.SecretRef("env:TG_MISSING_TOKEN"), HTTPClient: http.DefaultClient})
		// token env unset → resolve fails closed before any request.
		if _, err := newSourceFrom(c).Sync(context.Background()); err == nil {
			t.Fatal("missing token must fail closed")
		}
	})
	t.Run("forbidden-403", func(t *testing.T) {
		f := &fakeAWX{requireTok: true, forbid: true}
		c := newClient(t, f.server(t).URL)
		if _, err := newSource(t, c).Sync(context.Background()); err == nil {
			t.Fatal("403 must fail closed")
		}
	})
	t.Run("not-found-404", func(t *testing.T) {
		f := &fakeAWX{requireTok: true, notFound: true}
		c := newClient(t, f.server(t).URL)
		if _, err := newSource(t, c).Sync(context.Background()); err == nil {
			t.Fatal("404 must fail closed")
		}
	})
	t.Run("unreachable", func(t *testing.T) {
		srv := (&fakeAWX{}).server(t)
		addr := srv.URL
		srv.Close()
		c := newClient(t, addr)
		if _, err := newSource(t, c).Sync(context.Background()); err == nil {
			t.Fatal("unreachable AWX must fail closed")
		}
	})
	t.Run("malformed-json", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(200)
			_, _ = w.Write([]byte("{not json"))
		}))
		t.Cleanup(srv.Close)
		c := newClient(t, srv.URL)
		if _, err := newSource(t, c).Sync(context.Background()); err == nil {
			t.Fatal("malformed page must fail closed")
		}
	})
}

// TestTokenNeverLogged proves the Bearer token never appears in an error message even when the request path
// carries it (defensive) and that a real 401 error is secret-free.
func TestTokenNeverLogged(t *testing.T) {
	f := &fakeAWX{requireTok: true}
	c, _ := awx.New(awx.Config{
		BaseURL:    f.server(t).URL,
		TokenRef:   config.SecretRef("env:TG_TEST_AWX_TOKEN"),
		HTTPClient: http.DefaultClient,
	})
	t.Setenv("TG_TEST_AWX_TOKEN", "super-secret-should-not-appear")
	// Force a 403 to generate an error we can inspect.
	f.forbid = true
	_, err := newSourceFrom(c).Sync(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "super-secret-should-not-appear") {
		t.Fatalf("token leaked into error: %v", err)
	}
}

func TestConstructorFailClosed(t *testing.T) {
	if _, err := awx.New(awx.Config{TokenRef: "env:X"}); err == nil {
		t.Fatal("missing base URL must error")
	}
	if _, err := awx.NewSource(awx.SourceConfig{ID: "x"}); err == nil {
		t.Fatal("missing client must error")
	}
	if _, err := awx.NewSource(awx.SourceConfig{Client: &awx.Client{}}); err == nil {
		t.Fatal("missing id must error")
	}
}

// TestLivePing is a BEST-EFFORT env-gated live check against the real awx.example.net. It is skipped
// unless TG_AWX_LIVE_ADDR is set (e.g. "https://awx.example.net"). It asserts only the
// unauthenticated /api/v2/ping/ endpoint (TG holds no AWX token in this environment).
func TestLivePing(t *testing.T) {
	addr := os.Getenv("TG_AWX_LIVE_ADDR")
	if addr == "" {
		t.Skip("set TG_AWX_LIVE_ADDR to run the live awx.example.net ping check")
	}
	c, err := awx.New(awx.Config{BaseURL: addr, CACertPath: os.Getenv("TG_AWX_CACERT")})
	if err != nil {
		t.Fatalf("live client: %v", err)
	}
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("live awx ping: %v", err)
	}
}

// ---- helpers ----

func newSourceFrom(c *awx.Client) *awx.Source {
	src, _ := awx.NewSource(awx.SourceConfig{ID: "awx01", Client: c, RefScheme: "store", RefPrefix: "awx/"})
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

// TestSyncRefusesOffHostPagination proves the .next-URL fix: a compromised/MITM'd AWX that returns an
// absolute .next pointing at ANOTHER host must be refused — the connector must not fetch (with its Bearer
// token) from the off-host URL. The "evil" server must receive ZERO requests.
func TestSyncRefusesOffHostPagination(t *testing.T) {
	var evilHits int32
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&evilHits, 1)
		writeJSON(w, 200, map[string]any{"count": 0, "next": nil, "previous": nil, "results": []any{}})
	}))
	defer evil.Close()

	f := &fakeAWX{requireTok: true, offHostNext: evil.URL + "/api/v2/hosts/?page=2"}
	c := newClient(t, f.server(t).URL)
	src := newSource(t, c)

	_, err := src.Sync(context.Background())
	if err == nil {
		t.Fatal("sync must FAIL closed on an off-host .next url, got nil error")
	}
	if !strings.Contains(err.Error(), "off-host") {
		t.Fatalf("expected an off-host refusal error, got: %v", err)
	}
	if n := atomic.LoadInt32(&evilHits); n != 0 {
		t.Fatalf("the off-host server was contacted %d times — the Bearer token may have leaked off-host", n)
	}
}

// TestSyncSkipsParseBombVariables proves the .variables parse-bomb fix: a host carrying a billion-laughs
// YAML blob is refused by parseVars (skipped-with-record), so Sync neither hangs nor crashes and yields no
// entry for it — fail-closed, never a blank/expanded identity.
func TestSyncSkipsParseBombVariables(t *testing.T) {
	f := &fakeAWX{requireTok: true, bombVars: true}
	c := newClient(t, f.server(t).URL)
	src := newSource(t, c)

	done := make(chan struct{})
	var entries []credential.SourceEntry
	var err error
	go func() { entries, err = src.Sync(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("sync hung on a parse-bomb .variables blob — DoS not bounded")
	}
	if err != nil {
		t.Fatalf("sync should skip the bomb host, not error the whole run: %v", err)
	}
	for _, e := range entries {
		if e.Selector.Pattern == "bomb01" {
			t.Fatalf("the parse-bomb host must be skipped fail-closed, but it produced an entry: %+v", e)
		}
	}
}

// ---- ESTATE HOST↔GROUP MEMBERSHIP RECONCILIATION (spec/016 T-052, REQ-1605) ------------------------------

// TestMembershipMapsHostsToInventories proves the AWX connector's MembershipSource: it walks the inventories
// and each inventory's hosts and returns a host→[inventory-name] map — the estate reconciliation that lets a
// JT group(inventory) bundle resolve for a host in that inventory. It also asserts the map is NON-SECRET: no
// Bearer token, no "$encrypted$", no key material anywhere in it.
func TestMembershipMapsHostsToInventories(t *testing.T) {
	f := &fakeAWX{requireTok: true}
	c := newClient(t, f.server(t).URL)
	src := newJTSource(t, c)

	m, err := src.Membership(context.Background())
	if err != nil {
		t.Fatalf("membership: %v", err)
	}
	want := map[string][]string{
		"web01":   {"Cert Sync Targets"},
		"db01":    {"Cert Sync Targets"},
		"old01":   {"Cert Sync Targets"},
		"bare01":  {"Cert Sync Targets"},
		"ha01":    {"HAProxy Targets"},
		"weird01": {"Weird Targets"},
	}
	for host, wg := range want {
		got := m[host]
		if len(got) != len(wg) || (len(got) > 0 && got[0] != wg[0]) {
			t.Fatalf("membership[%q] = %v, want %v", host, got, wg)
		}
	}
	// NON-SECRET: nothing in the whole map may carry a token / encrypted marker / key material.
	for host, groups := range m {
		for _, banned := range []string{goodToken, "$encrypted$", "/secrets/", "file:"} {
			if strings.Contains(host, banned) {
				t.Fatalf("membership host key %q leaked %q", host, banned)
			}
			for _, g := range groups {
				if strings.Contains(g, banned) {
					t.Fatalf("membership group %q (host %q) leaked %q", g, host, banned)
				}
			}
		}
	}
}

// TestMembershipFailsClosed proves a membership fetch fails CLOSED (returns an error, no partial map) when
// /inventories/ is unreadable — so the engine retains its prior membership rather than dropping it silently.
func TestMembershipFailsClosed(t *testing.T) {
	f := &fakeAWX{requireTok: true, invError: true}
	c := newClient(t, f.server(t).URL)
	src := newJTSource(t, c)

	if _, err := src.Membership(context.Background()); err == nil {
		t.Fatal("membership must FAIL closed when /inventories/ errors, got nil")
	}
}

// TestResolveAWXBundleViaMembership is the end-to-end proof of the gap this task closes: a host that lives in
// an AWX inventory (bare01, which Sync SKIPS as a host entry — it has no key ref — yet is an inventory MEMBER)
// now RESOLVES that inventory's JT group bundle, because the SyncEngine populates Target.Groups from the AWX
// membership. It ALSO proves: (a) NO regression — a host WITH its own host-selector rule (web01) still wins by
// most-specific-wins, and a host in NO inventory (stranger01) resolves via the native fallback exactly as
// before; and (b) the DEDUP — two JTs binding the same inventory to the same cred do not make the inventory
// resolve ambiguously.
func TestResolveAWXBundleViaMembership(t *testing.T) {
	f := &fakeAWX{requireTok: true, dupJT: true} // dupJT: identical duplicate entry that must be deduped
	c := newClient(t, f.server(t).URL)
	src := newJTSource(t, c)

	// A native fallback with a rule for a host that is NOT in any AWX inventory — proves no-regression.
	nativeRules, err := credential.ParseRules("host:stranger01|nativeuser|22|ssh|file:/secrets/native_key")
	if err != nil {
		t.Fatalf("native rules: %v", err)
	}
	se := credential.NewSyncEngine(credential.NewEngine(nativeRules))
	if err := se.RegisterSource(src, 20); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := se.Sync(context.Background(), "awx01"); err != nil {
		t.Fatalf("sync: %v", err)
	}

	// GroupsFor is populated from the membership walk.
	if g := se.GroupsFor("bare01"); len(g) != 1 || g[0] != "Cert Sync Targets" {
		t.Fatalf("GroupsFor(bare01) = %v, want [Cert Sync Targets]", g)
	}

	// (1) bare01: no host-selector rule (skipped by Sync), but its inventory's group bundle now RESOLVES.
	res, err := se.Resolve(credential.Target{Host: "bare01"})
	if err != nil {
		t.Fatalf("resolve bare01 via membership must succeed, got: %v (dedup broken → spurious ambiguity?)", err)
	}
	if string(res.Bundle.SSHKeyRef()) != "file:/secrets/one_key" || res.Bundle.User() != "root" {
		t.Fatalf("bare01 resolved to user=%q keyref=%q, want root / file:/secrets/one_key (the Cert Sync Targets JT bundle)",
			res.Bundle.User(), res.Bundle.SSHKeyRef())
	}
	if res.Source != "awx01" {
		t.Fatalf("bare01 winning source = %q, want awx01", res.Source)
	}

	// (2) NO regression — web01 HAS a host-selector rule (deploy@...); most-specific-wins keeps the host rule
	// even though web01 is ALSO in the Cert Sync Targets inventory.
	res, err = se.Resolve(credential.Target{Host: "web01"})
	if err != nil {
		t.Fatalf("resolve web01: %v", err)
	}
	if res.Bundle.User() != "deploy" {
		t.Fatalf("web01 resolved to user=%q, want deploy (its host rule must beat the group bundle)", res.Bundle.User())
	}

	// (3) NO regression — stranger01 is in no AWX inventory; it resolves via the native fallback exactly as
	// before (membership adds nothing).
	res, err = se.Resolve(credential.Target{Host: "stranger01"})
	if err != nil {
		t.Fatalf("resolve stranger01 (native fallback): %v", err)
	}
	if !res.Native || res.Bundle.User() != "nativeuser" {
		t.Fatalf("stranger01 native=%v user=%q, want native=true user=nativeuser", res.Native, res.Bundle.User())
	}

	// (4) NO regression — a host in no inventory and no rule stays fail-closed unresolved.
	if _, err := se.Resolve(credential.Target{Host: "ghost01"}); err == nil {
		t.Fatal("ghost01 (no rule, no inventory) must stay unresolved fail-closed")
	}
}

// TestJTDedupCollapsesIdenticalEntries proves the connector-level dedup directly: with two JTs binding the
// same inventory to the same credential, Sync emits ONE group entry for that inventory, not two identical
// ones (which would otherwise be equal-specificity and fail the inventory closed as spuriously ambiguous).
func TestJTDedupCollapsesIdenticalEntries(t *testing.T) {
	f := &fakeAWX{requireTok: true, dupJT: true}
	c := newClient(t, f.server(t).URL)
	src := newJTSource(t, c)

	entries, err := src.Sync(context.Background())
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	n := 0
	for _, e := range entries {
		if e.Selector.Kind == credential.KindGroup && e.Selector.Pattern == "Cert Sync Targets" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("Cert Sync Targets emitted %d entries, want 1 (identical duplicate must be deduped)", n)
	}
}
