package acceptance

// TG-109 (T-016-13) — the console credential surface's WRITE-half bindings, driving the REAL registered
// routes through the real auth stack (httpapi.Register + session login), never a re-typed copy. Two of the
// six console scenarios stay @pending honestly: the object-groups editor has no model to edit (carved to
// TG-481), and the coverage view's packet-tracer PAIRING is a console-DOM property beyond this API layer
// (its API half — resolved-vs-not — is asserted by the map binding below; the pairing renders fixture-free
// in the served module and is exercised by deploy/console/v2/e2e/credentials-write.mjs locally).

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/auth"
	"github.com/territory-grounder/grounder/core/httpapi"
	"github.com/territory-grounder/grounder/temporal/nativerule"
)

// consoleRig is the real served surface: httpapi.Register over Mem backends, a real session login.
type consoleRig struct {
	srv     *httptest.Server
	cookie  *http.Cookie
	reader  *httpapi.MemCredentialsReader
	syncer  *httpapi.MemCredentialSyncer
	body    string
	status  int
	mapBody string
}

func (w *world) buildConsoleRig() error {
	ops := auth.MemOperators{"op": {Name: "op", TokenSHA256: sha256.Sum256([]byte("t0ken"))}}
	sa, err := auth.NewSessionAuthenticator([]byte(strings.Repeat("k", 32)), auth.NewMemSessionStore(), ops, time.Hour)
	if err != nil {
		return err
	}
	sa.Secure = false
	v := &auth.Verifier{}
	v.EnableBrowserSessions(sa)
	// The write half sits at AuthAdminSession: the rig carries the REAL admin authenticator and performs
	// the REAL step-up below, so the sync-on-demand scenario's admitted branch actually executes (the
	// first binding left it structurally dead — the QA review's finding; an oracle must be able to fail
	// on the branch its Then describes).
	aa, err := auth.NewAdminAuthenticator(auth.MemOperators{"root-admin": {Name: "root-admin", TokenSHA256: sha256.Sum256([]byte("admin-token-test"))}}, 15*time.Minute)
	if err != nil {
		return err
	}
	v.EnableAdminSessions(aa)
	rt := auth.NewRouter(v)

	w.consoleReader = &httpapi.MemCredentialsReader{
		Sources: []httpapi.CredentialSource{
			{SourceID: "openbao", Plane: "machine", LastSyncedAt: "2026-08-14T06:00:00Z", Added: 2, Drifted: true, CoveredTargets: 7, Outcome: "ok", Precedence: 10},
			{SourceID: "native-db", Plane: "machine", LastSyncedAt: "2026-08-14T06:00:00Z", CoveredTargets: 1, Outcome: "ok", Precedence: 90},
		},
		Resolutions: []httpapi.CredentialResolution{
			{Target: "web01", Plane: "machine", Outcome: "resolved", Source: "openbao", KeyRefScheme: "bao"},
			{Target: "spare01", Plane: "machine", Outcome: "resolved", Native: true, KeyRefScheme: "env"},
		},
	}
	w.consoleSyncer = &httpapi.MemCredentialSyncer{Outcome: httpapi.CredentialSyncOutcome{
		SourceID: "openbao", OK: true, Summary: "synced OK — 7 entries (+2 ~0 -0)", Added: 2, Entries: 7,
	}}
	httpapi.Register(rt, httpapi.Deps{Sessions: sa, AdminSessions: aa, Credentials: w.consoleReader, CredentialSync: w.consoleSyncer})
	srv := httptest.NewServer(rt.Mux())
	cookie, _, err := sa.Login(context.Background(), "op", "t0ken", "192.0.2.1:1")
	if err != nil {
		srv.Close()
		return err
	}
	// The REAL step-up: the plain session elevates through the registered endpoint, exactly as the
	// console does. After this the cookie satisfies AuthAdminSession for its TTL.
	elevReq, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/session/elevate", nil)
	if err != nil {
		srv.Close()
		return err
	}
	elevReq.AddCookie(cookie)
	elevReq.Header.Set(auth.AdminHeaderName, "root-admin")
	elevReq.Header.Set("Authorization", "Bearer admin-token-test")
	elevResp, err := http.DefaultClient.Do(elevReq)
	if err != nil {
		srv.Close()
		return err
	}
	elevResp.Body.Close()
	if elevResp.StatusCode != http.StatusOK {
		srv.Close()
		return fmt.Errorf("elevate: %d", elevResp.StatusCode)
	}
	w.consoleRig = &consoleRig{srv: srv, cookie: cookie, reader: w.consoleReader, syncer: w.consoleSyncer}
	return nil
}

func (w *world) rigDo(method, path, body string) error {
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, w.consoleRig.srv.URL+path, rdr)
	if err != nil {
		return err
	}
	req.AddCookie(w.consoleRig.cookie)
	req.Header.Set("Content-Type", "application/json")
	// Same-origin, as the console is: the admin write guard refuses cross-origin.
	req.Header.Set("Origin", w.consoleRig.srv.URL)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	w.consoleRig.body, w.consoleRig.status = string(b), resp.StatusCode
	return nil
}

// --- Scenario: The console renders per-source last-synced and drift (REQ-1615) ---

func (w *world) givenConsoleCredentialsSurface() error { return w.buildConsoleRig() }

func (w *world) whenOperatorViewsSources() error {
	return w.rigDo(http.MethodGet, "/v1/credentials/sources", "")
}

func (w *world) thenSourcesRenderLastSyncedAndDrift() error {
	defer w.consoleRig.srv.Close()
	if w.consoleRig.status != http.StatusOK {
		return fmt.Errorf("sources read: %d (%s)", w.consoleRig.status, w.consoleRig.body)
	}
	for _, want := range []string{`"last_synced_at":"2026-08-14T06:00:00Z"`, `"drifted":true`, `"precedence":10`} {
		if !strings.Contains(w.consoleRig.body, want) {
			return fmt.Errorf("REQ-1615: sources payload missing %s in %s", want, w.consoleRig.body)
		}
	}
	return nil
}

// --- Scenario: The console configures tests schedules and syncs each source on demand (REQ-1618) ---
// Config/test/schedule ride the module-config lane's own bound oracles (POST /v1/config/{key} write suite
// and the moduletest workflow suite); THIS binding proves the on-demand SYNC half end-to-end through the
// real admin-gated route, and that the surface re-reads last-synced/drift afterwards.

func (w *world) givenFirstClassConsoleSurface() error { return w.buildConsoleRig() }

func (w *world) whenOperatorTriggersSyncNow() error {
	// Config/test/schedule ride the module-config lane's own bound oracles; THIS drives the on-demand
	// sync end-to-end through the real admin-gated route with the rig's genuinely elevated session.
	return w.rigDo(http.MethodPost, "/v1/credentials/sources/openbao/sync", `{}`)
}

func (w *world) thenSourceSyncedOnDemand() error {
	defer w.consoleRig.srv.Close()
	if w.consoleRig.status != http.StatusOK || !strings.Contains(w.consoleRig.body, `"ok":true`) {
		return fmt.Errorf("sync-now must be ADMITTED for the elevated session and succeed: %d (%s)", w.consoleRig.status, w.consoleRig.body)
	}
	if w.consoleSyncer.LastSourceID != "openbao" {
		return fmt.Errorf("the sync lane was not driven: %q", w.consoleSyncer.LastSourceID)
	}
	if w.consoleSyncer.LastOperator == "" {
		return fmt.Errorf("the operator identity must be principal-derived, got empty")
	}
	// ...and its last-synced and drift are shown: the surface re-reads the sources after a sync.
	if err := w.rigDo(http.MethodGet, "/v1/credentials/sources", ""); err != nil {
		return err
	}
	if !strings.Contains(w.consoleRig.body, `"last_synced_at"`) || !strings.Contains(w.consoleRig.body, `"drifted"`) {
		return fmt.Errorf("post-sync sources read missing last-synced/drift: %s", w.consoleRig.body)
	}
	return nil
}

// --- Scenario: The console renders the per-target credential map with source and precedence (REQ-1618) ---

func (w *world) givenResolvedBundlesMultiSource() error { return w.buildConsoleRig() }

func (w *world) whenOperatorViewsMap() error {
	if err := w.rigDo(http.MethodGet, "/v1/credentials/resolutions", ""); err != nil {
		return err
	}
	w.consoleRig.mapBody = w.consoleRig.body
	return w.rigDo(http.MethodGet, "/v1/credentials/sources", "")
}

func (w *world) thenMapShowsSourcePrecedenceNative() error {
	defer w.consoleRig.srv.Close()
	for _, want := range []string{`"source":"openbao"`, `"native":true`, `"key_ref_scheme":"bao"`} {
		if !strings.Contains(w.consoleRig.mapBody, want) {
			return fmt.Errorf("REQ-1618 map: missing %s in %s", want, w.consoleRig.mapBody)
		}
	}
	if !strings.Contains(w.consoleRig.body, `"precedence":10`) || !strings.Contains(w.consoleRig.body, `"precedence":90`) {
		return fmt.Errorf("REQ-1618 map: precedence pair missing in %s", w.consoleRig.body)
	}
	return nil
}

// --- Scenario: A secret value is write-only in the console and never echoed (REQ-1618) ---
// Two REAL halves: (1) the credential DTO layer is a CLOSED field enumeration with no value-bearing field —
// reflection reddens on any new field, so a secret CANNOT be serialized by construction (the INV-13 design,
// made falsifiable); (2) a native-rule write carrying a SecretRef reaches the ledger with the reference
// ABSENT from the audit text, driven through the real nativerule activity + a real audit ledger.

func (w *world) givenConsoleCredentialSurfaceForSecrets() error { return nil }

func (w *world) whenOperatorEntersSecretAndViews() error {
	led := audit.NewLedger()
	acts := &nativerule.Activities{D: nativerule.Deps{Store: &memNativeStore{}, Ledger: led}}
	res, err := acts.ApplyNativeRuleWriteActivity(context.Background(),
		nativerule.Request{Verb: "add", Entry: "host:web01|deploy|22|ssh|env:TG_SECRET_WRITEONLY_PROBE", Rationale: "binding", Operator: "op", AdminAuthorized: true})
	if err != nil {
		return err
	}
	w.secretLedger = led
	w.secretRuleID = res.ID
	return nil
}

func (w *world) thenSecretWriteOnlyNeverEchoed() error {
	// (1) closed DTO enumeration.
	wantFields := map[string][]string{
		"CredentialSource":      {"SourceID", "Plane", "LastSyncedAt", "Added", "Changed", "Removed", "Drifted", "CoveredTargets", "Outcome", "Err", "Precedence"},
		"CredentialSyncOutcome": {"SourceID", "OK", "Summary", "Detail", "Added", "Changed", "Removed", "Entries", "Starved", "ElapsedMS"},
	}
	for name, want := range wantFields {
		var t reflect.Type
		switch name {
		case "CredentialSource":
			t = reflect.TypeOf(httpapi.CredentialSource{})
		case "CredentialSyncOutcome":
			t = reflect.TypeOf(httpapi.CredentialSyncOutcome{})
		}
		var got []string
		for i := 0; i < t.NumField(); i++ {
			got = append(got, t.Field(i).Name)
		}
		sort.Strings(got)
		sorted := append([]string(nil), want...)
		sort.Strings(sorted)
		if strings.Join(got, ",") != strings.Join(sorted, ",") {
			return fmt.Errorf("REQ-1618: %s field set changed (%v) — a new field on a credential DTO must prove it cannot carry a secret and update this closed enumeration", name, got)
		}
	}
	// (2) the ledger records the edit without the reference.
	entries := w.secretLedger.Entries()
	if len(entries) == 0 {
		return fmt.Errorf("REQ-1618: the credential edit appended no ledger entry")
	}
	for _, e := range entries {
		if strings.Contains(e.Reason, "TG_SECRET_WRITEONLY_PROBE") || strings.Contains(e.Reason, "env:") {
			return fmt.Errorf("REQ-1618/INV-13: the SecretRef reached the audit text: %q", e.Reason)
		}
	}
	if w.secretRuleID == 0 {
		return fmt.Errorf("REQ-1618: the write did not persist")
	}
	return nil
}

// memNativeStore is the minimal nativerule.Store for the binding.
type memNativeStore struct{ n int64 }

func (m *memNativeStore) Insert(_ context.Context, _, _, _ string) (int64, error) {
	m.n++
	return m.n, nil
}
func (m *memNativeStore) Delete(_ context.Context, id int64) (bool, error) { return id <= m.n, nil }
