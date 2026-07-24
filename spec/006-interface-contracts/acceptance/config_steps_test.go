package acceptance

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/cucumber/godog"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/auth"
	"github.com/territory-grounder/grounder/core/cpconfig"
	"github.com/territory-grounder/grounder/core/httpapi"
	"github.com/territory-grounder/grounder/core/seal"
	"github.com/territory-grounder/grounder/temporal/configwrite"
)

// Task #27 oracles (REQ-520/522/523/524): the LAW-clamped resolver, the admin step-up tier, the
// ledger-before-commit config write, and the sealed-secret write-only surface. Every Then inspects
// a real resolver result, a real HTTP response off the REAL router, a real ledger, or a real
// AEAD open — no step fabricates an outcome (INV-22). Credentials are generated per run.

const (
	cfgOperatorToken = "acceptance-operator-token-1234567890"
	cfgAdminToken    = "acceptance-admin-step-up-token-0987654321"
)

type cfgWorld struct {
	// resolver oracle (REQ-520)
	resolved []cpconfig.Value

	// HTTP surface oracle (REQ-522/523/524)
	srv          *httptest.Server
	cookie       *http.Cookie
	adminWired   bool
	configWriter *cfgFakeWriter
	secretWriter *cfgFakeSecretWriter
	lastStatus   int
	lastBody     string

	// worker activity oracle (REQ-523)
	ledger   *audit.Ledger
	store    *cfgMemStore
	applyRes configwrite.ConfigResult
	applyErr error

	// seal oracle (REQ-524)
	master []byte
	sealed seal.Sealed
	opened []byte
}

type cfgFakeWriter struct{ calls int }

func (f *cfgFakeWriter) WriteConfig(_ context.Context, key, value, _, _ string) (httpapi.ConfigWriteOutcome, error) {
	f.calls++
	return httpapi.ConfigWriteOutcome{Key: key, Value: value, Source: "console", LedgerSeq: 7}, nil
}

type cfgFakeSecretWriter struct{ lastName, lastValue string }

func (f *cfgFakeSecretWriter) PutSecret(_ context.Context, name, value, _, _, _ string) (httpapi.SecretPutOutcome, error) {
	f.lastName, f.lastValue = name, value
	return httpapi.SecretPutOutcome{Name: name, Ref: "store:" + name, LedgerSeq: 9}, nil
}

type cfgSealedList struct{ name string }

func (l cfgSealedList) SealedSecrets(_ context.Context) ([]httpapi.SealedSecretInfo, error) {
	if l.name == "" {
		return nil, nil
	}
	return []httpapi.SealedSecretInfo{{Name: l.name, Ref: "store:" + l.name, Purpose: "acceptance", CreatedAt: time.Now(), UpdatedAt: time.Now()}}, nil
}

type cfgRefsList struct{}

func (cfgRefsList) SecretRefs(_ context.Context, _ auth.Principal) ([]httpapi.SecretRefStatus, error) {
	return []httpapi.SecretRefStatus{{Ref: "env:TG_SEAL_KEY", Purpose: "seal master key", Resolved: true}}, nil
}

type cfgMemStore struct {
	rows map[string]string
	seqs map[string]int64
	fail bool
}

func (m *cfgMemStore) Upsert(_ context.Context, key, value, _, _ string, ledgerSeq int64, sv int) error {
	if m.fail {
		return errors.New("store down")
	}
	if sv <= 0 {
		return errors.New("unstamped row")
	}
	if m.rows == nil {
		m.rows, m.seqs = map[string]string{}, map[string]int64{}
	}
	m.rows[key] = value
	m.seqs[key] = ledgerSeq
	return nil
}

// build stands up the REAL router: sessions always; the admin tier only when admin=true.
func (w *cfgWorld) build(admin bool) error {
	w.close()
	w.adminWired = admin
	ops := auth.MemOperators{"kyriakos": {Name: "kyriakos", TokenSHA256: sha256.Sum256([]byte(cfgOperatorToken))}}
	sa, err := auth.NewSessionAuthenticator([]byte(strings.Repeat("c", 32)), auth.NewMemSessionStore(), ops, time.Hour)
	if err != nil {
		return err
	}
	sa.Secure = false
	verifier, err := auth.NewVerifier(fakeSources{}, &fakeNonces{}, time.Hour)
	if err != nil {
		return err
	}
	verifier.EnableBrowserSessions(sa)
	w.configWriter = &cfgFakeWriter{}
	w.secretWriter = &cfgFakeSecretWriter{}
	d := httpapi.Deps{
		Sessions:     sa,
		ConfigWrite:  w.configWriter,
		SecretsWrite: w.secretWriter,
		SecretsRead:  cfgRefsList{},
		SealedRead:   cfgSealedList{name: "librenms.token"},
	}
	if admin {
		admins := auth.MemOperators{"root-admin": {Name: "root-admin", TokenSHA256: sha256.Sum256([]byte(cfgAdminToken))}}
		aa, aerr := auth.NewAdminAuthenticator(admins, 10*time.Minute)
		if aerr != nil {
			return aerr
		}
		verifier.EnableAdminSessions(aa)
		d.AdminSessions = aa
	}
	rt := auth.NewRouter(verifier)
	httpapi.Register(rt, d)
	w.srv = httptest.NewServer(rt.Mux())
	return nil
}

func (w *cfgWorld) close() {
	if w.srv != nil {
		w.srv.Close()
		w.srv = nil
	}
}

func (w *cfgWorld) login() error {
	req, _ := http.NewRequest(http.MethodPost, w.srv.URL+"/v1/session", nil)
	req.Header.Set("X-TG-Operator", "kyriakos")
	req.Header.Set("Authorization", "Bearer "+cfgOperatorToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("login status %d", resp.StatusCode)
	}
	for _, c := range resp.Cookies() {
		if c.Name == auth.SessionCookieName {
			w.cookie = c
			return nil
		}
	}
	return errors.New("no session cookie issued")
}

func (w *cfgWorld) elevate() (int, error) {
	req, _ := http.NewRequest(http.MethodPost, w.srv.URL+"/v1/session/elevate", nil)
	if w.cookie != nil {
		req.AddCookie(w.cookie)
	}
	req.Header.Set(auth.AdminHeaderName, "root-admin")
	req.Header.Set("Authorization", "Bearer "+cfgAdminToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}

func (w *cfgWorld) post(path, body string) error {
	req, _ := http.NewRequest(http.MethodPost, w.srv.URL+path, bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	if w.cookie != nil {
		req.AddCookie(w.cookie)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	w.lastStatus, w.lastBody = resp.StatusCode, string(b)
	return nil
}

func registerConfigSteps(sc *godog.ScenarioContext) {
	w := &cfgWorld{}
	sc.After(func(ctx context.Context, _ *godog.Scenario, _ error) (context.Context, error) {
		w.close()
		return ctx, nil
	})

	// --- REQ-520: the LAW-clamped layered resolver -------------------------------------------------
	sc.Step(`^a control-plane config resolver with law, env, and console layers$`, func() error {
		w.resolved = nil
		return nil
	})
	sc.Step(`^the configuration is resolved$`, func() error {
		r := cpconfig.Resolver{
			Law: map[string]string{"safety.mutation_enabled": "off"},
			Env: map[string]string{"safety.mutation_enabled": "on", "session.ttl": "12h", "operator.name": "kyriakos"},
			Console: cpconfig.ConsoleStore(consoleMap{
				"safety.mutation_enabled": "on", // an illegal override of a LAW key — must be ignored
				"session.ttl":             "8h", // legal: console-writable non-LAW
				"operator.name":           "x",  // illegal: not console-writable
			}),
		}
		vals, err := r.Resolve(context.Background())
		w.resolved = vals
		return err
	})
	sc.Step(`^a LAW key resolves to its compiled value whatever env or console hold$`, func() error {
		for _, v := range w.resolved {
			if v.Name == "safety.mutation_enabled" {
				if v.Value != "off" || v.Source != cpconfig.SourceLaw {
					return fmt.Errorf("LAW key resolved %q/%s — the clamp failed", v.Value, v.Source)
				}
				return nil
			}
		}
		return errors.New("LAW key absent from the resolved set")
	})
	sc.Step(`^a console override is honored only for a console-writable non-LAW key$`, func() error {
		var ttl, opname *cpconfig.Value
		for i := range w.resolved {
			switch w.resolved[i].Name {
			case "session.ttl":
				ttl = &w.resolved[i]
			case "operator.name":
				opname = &w.resolved[i]
			}
		}
		if ttl == nil || ttl.Value != "8h" || ttl.Source != cpconfig.SourceConsole {
			return fmt.Errorf("writable key did not honor the console override: %+v", ttl)
		}
		if opname == nil || opname.Source == cpconfig.SourceConsole {
			return fmt.Errorf("non-writable key honored a console override: %+v", opname)
		}
		return nil
	})

	// --- REQ-522: the admin step-up tier ----------------------------------------------------------
	sc.Step(`^a session-enabled interface surface with an admin credential configured$`, func() error {
		return w.build(true)
	})
	sc.Step(`^a logged-in operator session that is not elevated$`, func() error {
		if err := w.login(); err != nil {
			return err
		}
		// Prove non-elevation: an admin write route refuses the plain session.
		if err := w.post("/v1/config/session.ttl", `{"value":"8h","rationale":"r"}`); err != nil {
			return err
		}
		if w.lastStatus != http.StatusUnauthorized {
			return fmt.Errorf("plain session admitted to an admin route: %d", w.lastStatus)
		}
		return nil
	})
	sc.Step(`^the operator presents the admin credential to the elevation route$`, func() error {
		code, err := w.elevate()
		if err != nil {
			return err
		}
		if code != http.StatusOK {
			return fmt.Errorf("elevation status %d", code)
		}
		return nil
	})
	sc.Step(`^the same session satisfies an admin-session write route with the admin capability$`, func() error {
		if err := w.post("/v1/config/session.ttl", `{"value":"8h","rationale":"shorter sessions"}`); err != nil {
			return err
		}
		if w.lastStatus != http.StatusOK {
			return fmt.Errorf("elevated write status %d (%s)", w.lastStatus, w.lastBody)
		}
		if w.configWriter.calls != 1 {
			return fmt.Errorf("backend calls = %d", w.configWriter.calls)
		}
		return nil
	})

	sc.Step(`^a session-enabled interface surface without an admin authenticator$`, func() error {
		if err := w.build(false); err != nil {
			return err
		}
		return w.login()
	})
	sc.Step(`^any caller posts to the elevation route or an admin write route$`, func() error {
		return nil // the Then probes both routes itself
	})
	sc.Step(`^the routes are not registered and every caller is refused$`, func() error {
		for _, p := range []string{"/v1/session/elevate", "/v1/config/session.ttl", "/v1/secrets/name"} {
			if err := w.post(p, `{"value":"v","rationale":"r"}`); err != nil {
				return err
			}
			if w.lastStatus != http.StatusNotFound && w.lastStatus != http.StatusMethodNotAllowed {
				return fmt.Errorf("%s answered %d without an admin authenticator", p, w.lastStatus)
			}
		}
		return nil
	})

	// --- REQ-523: the config write clamp + ledger-before-commit -----------------------------------
	sc.Step(`^an admin-elevated operator session$`, func() error {
		if err := w.build(true); err != nil {
			return err
		}
		if err := w.login(); err != nil {
			return err
		}
		code, err := w.elevate()
		if err != nil {
			return err
		}
		if code != http.StatusOK {
			return fmt.Errorf("elevation status %d", code)
		}
		return nil
	})
	sc.Step(`^the operator posts an override for a LAW key$`, func() error {
		return w.post("/v1/config/safety.mutation_enabled", `{"value":"on","rationale":"try the clamp"}`)
	})
	sc.Step(`^the write is refused with 422 and never reaches the write backend$`, func() error {
		if w.lastStatus != http.StatusUnprocessableEntity {
			return fmt.Errorf("LAW write status %d, want 422", w.lastStatus)
		}
		if w.configWriter.calls != 0 {
			return fmt.Errorf("the LAW write reached the backend (%d calls)", w.configWriter.calls)
		}
		return nil
	})

	sc.Step(`^the worker's config-write activity with a governance ledger and an override store$`, func() error {
		w.ledger = audit.NewLedger()
		w.store = &cfgMemStore{}
		return nil
	})
	sc.Step(`^a legal override is applied$`, func() error {
		acts := &configwrite.Activities{D: configwrite.Deps{Ledger: w.ledger, Config: w.store}}
		w.applyRes, w.applyErr = acts.ApplyConfigActivity(context.Background(),
			configwrite.ConfigRequest{Key: "session.ttl", Value: "8h", Rationale: "acceptance", Operator: "kyriakos"})
		return w.applyErr
	})
	sc.Step(`^the ledger holds the decision and the row carries its sequence$`, func() error {
		entries := w.ledger.Entries()
		if len(entries) != 1 || entries[0].Decision != "config:set" {
			return fmt.Errorf("ledger entries: %+v", entries)
		}
		if w.store.seqs["session.ttl"] != entries[0].Seq || w.applyRes.LedgerSeq != entries[0].Seq {
			return fmt.Errorf("row/result seq mismatch: row=%d res=%d ledger=%d",
				w.store.seqs["session.ttl"], w.applyRes.LedgerSeq, entries[0].Seq)
		}
		return nil
	})
	sc.Step(`^a store failure after the append leaves an over-recorded ledger, never an unrecorded change$`, func() error {
		lg := audit.NewLedger()
		st := &cfgMemStore{fail: true}
		acts := &configwrite.Activities{D: configwrite.Deps{Ledger: lg, Config: st}}
		if _, err := acts.ApplyConfigActivity(context.Background(),
			configwrite.ConfigRequest{Key: "session.ttl", Value: "8h", Rationale: "acceptance", Operator: "kyriakos"}); err == nil {
			return errors.New("a store failure must surface")
		}
		if len(lg.Entries()) != 1 {
			return fmt.Errorf("ledger must hold the appended decision, has %d", len(lg.Entries()))
		}
		return nil
	})

	// --- REQ-524: the sealed-secret envelope + the write-only surface ------------------------------
	sc.Step(`^a master key generated for the test$`, func() error {
		w.master = make([]byte, seal.KeySize)
		_, err := rand.Read(w.master)
		return err
	})
	sc.Step(`^a value is sealed under a name and opened again$`, func() error {
		var err error
		w.sealed, err = seal.Seal(w.master, "librenms.token", []byte("acceptance-material"))
		if err != nil {
			return err
		}
		w.opened, err = seal.Open(w.master, "librenms.token", w.sealed)
		return err
	})
	sc.Step(`^the value round-trips, and a wrong key, wrong name, or tampered blob refuses to open$`, func() error {
		if string(w.opened) != "acceptance-material" {
			return fmt.Errorf("round trip mismatch: %q", w.opened)
		}
		other := make([]byte, seal.KeySize)
		if _, err := rand.Read(other); err != nil {
			return err
		}
		if _, err := seal.Open(other, "librenms.token", w.sealed); !errors.Is(err, seal.ErrOpenFailed) {
			return fmt.Errorf("wrong key: %v", err)
		}
		if _, err := seal.Open(w.master, "other.name", w.sealed); !errors.Is(err, seal.ErrOpenFailed) {
			return fmt.Errorf("wrong name: %v", err)
		}
		tam := w.sealed
		tam.Ciphertext = append([]byte{}, w.sealed.Ciphertext...)
		tam.Ciphertext[0] ^= 1
		if _, err := seal.Open(w.master, "librenms.token", tam); !errors.Is(err, seal.ErrOpenFailed) {
			return fmt.Errorf("tampered: %v", err)
		}
		return nil
	})

	sc.Step(`^an admin-elevated operator session and a sealed-secret backend$`, func() error {
		if err := w.build(true); err != nil {
			return err
		}
		if err := w.login(); err != nil {
			return err
		}
		code, err := w.elevate()
		if err != nil || code != http.StatusOK {
			return fmt.Errorf("elevation: %d %v", code, err)
		}
		return nil
	})
	sc.Step(`^the operator seals a secret value$`, func() error {
		return w.post("/v1/secrets/librenms.token",
			`{"value":"acceptance-material","purpose":"poller","rationale":"provisioning"}`)
	})
	sc.Step(`^the response carries the store reference and never echoes the value$`, func() error {
		if w.lastStatus != http.StatusCreated {
			return fmt.Errorf("put status %d (%s)", w.lastStatus, w.lastBody)
		}
		if !strings.Contains(w.lastBody, `"store:librenms.token"`) {
			return fmt.Errorf("no store reference in %s", w.lastBody)
		}
		if strings.Contains(w.lastBody, "acceptance-material") {
			return fmt.Errorf("the response echoed the material: %s", w.lastBody)
		}
		return nil
	})
	sc.Step(`^the secrets read surface lists the sealed name with no value field$`, func() error {
		req, _ := http.NewRequest(http.MethodGet, w.srv.URL+"/v1/secrets", nil)
		if w.cookie != nil {
			req.AddCookie(w.cookie)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("secrets read status %d", resp.StatusCode)
		}
		if !strings.Contains(string(b), `"librenms.token"`) {
			return fmt.Errorf("sealed name absent: %s", b)
		}
		if strings.Contains(string(b), `"value"`) || strings.Contains(string(b), "acceptance-material") {
			return fmt.Errorf("a value appeared on the read surface: %s", b)
		}
		return nil
	})
}

// consoleMap adapts a plain map to the resolver's ConsoleStore seam.
type consoleMap map[string]string

func (m consoleMap) Overrides(_ context.Context) (map[string]string, error) { return m, nil }
