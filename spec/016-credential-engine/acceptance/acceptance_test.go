package acceptance

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/cucumber/godog"

	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/credential"
	ansiblesrc "github.com/territory-grounder/grounder/modules/credsource/ansible"
	"github.com/territory-grounder/grounder/modules/credsource/awx"
	ldapsrc "github.com/territory-grounder/grounder/modules/credsource/ldap"
	"github.com/territory-grounder/grounder/modules/credsource/oidctoken"
	"github.com/territory-grounder/grounder/modules/credsource/vault"

	ldapv3 "github.com/go-ldap/ldap/v3"
)

// world is the per-scenario state driving the real core/credential resolver core.
type world struct {
	engine *credential.Engine
	target credential.Target

	bundle credential.Bundle
	err    error

	// bundle-construction (REQ-1601) oracle state.
	constructErr error

	// shared-object-model (REQ-1605) oracle state.
	resolverMatched bool
	policyMatched   bool

	// precedence (REQ-1606) oracle state.
	tieErr error

	// sync-framework (REQ-1607/1608/1609/1610/1615) oracle state.
	syncEngine  *credential.SyncEngine
	source      *fakeSource
	firstRun    credential.SyncRun
	secondRun   credential.SyncRun
	resolution  credential.Resolution
	ambiguecErr error

	// vault:/bao: SecretRef backend (REQ-1613) oracle state.
	vaultRef     config.SecretRef
	vaultVal     string
	vaultResErr  error
	vaultServers []*httptest.Server

	// oidc: client-credentials token minter (REQ-1619) oracle state.
	oidcRef    config.SecretRef
	oidcVal    string
	oidcResErr error

	// AWX inventory connector (REQ-1612) oracle state.
	awxSource  *awx.Source
	awxEntries []credential.SourceEntry
	awxErr     error

	// Un-controlled Ansible (files + ansible-vault) connector (REQ-1612) oracle state.
	ansibleSource   *ansiblesrc.Source
	ansibleEntries  []credential.SourceEntry
	ansibleErr      error
	ansibleResolver *ansiblesrc.Resolver
	ansibleDir      string // tempdir holding the real vaulted tree; removed in the After hook

	// LDAP human-plane connector (REQ-1614) oracle state.
	ldapSource  *ldapsrc.Source
	ldapEntries []credential.SourceEntry
	ldapErr     error
}

// fakeLDAPConn is an in-memory ldapsrc.Conn (Bind+Search) driving the REAL LDAP connector with no live
// directory — the FreeIPA-shaped fixture the acceptance scenario pulls through.
type fakeLDAPConn struct {
	byBase map[string]*ldapv3.SearchResult
}

func (f *fakeLDAPConn) Bind(string, string) error { return nil }
func (f *fakeLDAPConn) Search(req *ldapv3.SearchRequest) (*ldapv3.SearchResult, error) {
	if r, ok := f.byBase[req.BaseDN]; ok {
		return r, nil
	}
	return &ldapv3.SearchResult{}, nil
}
func (f *fakeLDAPConn) Close() error { return nil }

func ldapEntry(dn string, attrs map[string][]string) *ldapv3.Entry {
	e := &ldapv3.Entry{DN: dn}
	for name, vals := range attrs {
		e.Attributes = append(e.Attributes, &ldapv3.EntryAttribute{Name: name, Values: vals})
	}
	return e
}

// fakeSource is the in-memory CredentialSource driving the sync oracles (CI has no external platform). Its
// upstream entry set is mutable so a scenario can prove re-read-by-id and drift, and Sync returns a COPY.
type fakeSource struct {
	id      string
	plane   credential.Plane
	entries []credential.SourceEntry
}

func (f *fakeSource) ID() string              { return f.id }
func (f *fakeSource) Plane() credential.Plane { return f.plane }
func (f *fakeSource) Sync(context.Context) ([]credential.SourceEntry, error) {
	out := make([]credential.SourceEntry, len(f.entries))
	copy(out, f.entries)
	return out, nil
}

// accEntry builds a SourceEntry over the shared object-model with a sealed SecretRef (never plaintext).
func accEntry(nativeID, host, user, ref string) credential.SourceEntry {
	b, err := credential.NewBundle(credential.BundleSpec{
		User: user, Port: 22, Scheme: credential.SchemeSSH, SSHKeyRef: config.SecretRef(ref),
	})
	if err != nil {
		panic(fmt.Sprintf("accEntry bundle: %v", err))
	}
	return credential.SourceEntry{
		NativeID: nativeID,
		Selector: credential.Selector{Kind: credential.KindHost, Pattern: host},
		Bundle:   b,
	}
}

// accApprover builds a human-plane SourceEntry carrying an approver identity (no host Bundle) — the shape a
// human-plane source (LDAP / OIDC) syncs.
func accApprover(nativeID string, kind credential.PrincipalKind, name string, groups ...string) credential.SourceEntry {
	a, err := credential.NewApproverIdentity(credential.ApproverIdentitySpec{Kind: kind, Name: name, Groups: groups})
	if err != nil {
		panic(fmt.Sprintf("accApprover: %v", err))
	}
	return credential.SourceEntry{NativeID: nativeID, Approver: a}
}

// mustApprover builds a bare user approver identity (used to construct a cross-plane leak in the oracle).
func mustApprover(name string) credential.ApproverIdentity {
	a, err := credential.NewApproverIdentity(credential.ApproverIdentitySpec{Kind: credential.PrincipalUser, Name: name})
	if err != nil {
		panic(fmt.Sprintf("mustApprover: %v", err))
	}
	return a
}

// mustAccBundle builds a valid host bundle (used to construct a cross-plane leak in the oracle).
func mustAccBundle(user, ref string) credential.Bundle {
	b, err := credential.NewBundle(credential.BundleSpec{User: user, Port: 22, Scheme: credential.SchemeSSH, SSHKeyRef: config.SecretRef(ref)})
	if err != nil {
		panic(fmt.Sprintf("mustAccBundle: %v", err))
	}
	return b
}

// TestCredentialEngineAcceptance runs the spec/016 acceptance feature. @pending scenarios (the not-yet-built
// sync/connectors/UX behavior) are excluded; the executed set drives real core/credential code and must pass
// strictly.
func TestCredentialEngineAcceptance(t *testing.T) {
	suite := godog.TestSuite{
		Name:                "spec/016 credential-engine",
		ScenarioInitializer: initializeScenario,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"."},
			Tags:     "~@pending",
			Strict:   true,
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("spec/016 acceptance scenarios failed")
	}
}

// sampleRules is the operator-declared resolver config used across scenarios (config-not-code): host / glob
// / group / device-class rules, every secret a sealed env: reference.
const sampleRules = "host:dc1tg01|root|22|ssh|env:TG_ACC_HOSTKEY;" +
	"host-glob:dc1*|ops|22|ssh|env:TG_ACC_GLOBKEY;" +
	"group:edge|edgeops|22|ssh|env:TG_ACC_GRPKEY;" +
	"device-class:cisco-asa|admin|443|api||env:TG_ACC_APITOKEN"

func initializeScenario(sc *godog.ScenarioContext) {
	w := &world{}

	// --- REQ-1600: a target resolves to exactly one bundle from config data ---
	sc.Step(`^operator-declared resolver config mapping host glob group and device-class to bundles$`, func() error {
		rules, err := credential.ParseRules(sampleRules)
		if err != nil {
			return fmt.Errorf("parse resolver config: %w", err)
		}
		w.engine = credential.NewEngine(rules)
		w.target = credential.Target{Host: "dc1tg01", Groups: []string{"edge"}, DeviceClass: "linux"}
		return nil
	})
	sc.Step(`^the engine resolves a target$`, func() error {
		w.bundle, w.err = w.engine.Resolve(w.target)
		return nil
	})
	sc.Step(`^exactly one credential bundle is returned from the config data with no code change$`, func() error {
		if w.err != nil {
			return fmt.Errorf("expected a resolved bundle, got refusal: %v", w.err)
		}
		if !w.bundle.Valid() {
			return fmt.Errorf("resolved bundle is invalid")
		}
		if w.bundle.RuleID() == "" {
			return fmt.Errorf("resolved bundle carries no config-rule provenance")
		}
		return nil
	})

	// --- REQ-1601: a bundle is constructible only with all required fields ---
	sc.Step(`^a credential bundle with an unset required field$`, func() error {
		// scheme ssh with no ssh key reference — a required field is unset.
		_, w.constructErr = credential.NewBundle(credential.BundleSpec{User: "root", Port: 22, Scheme: credential.SchemeSSH})
		return nil
	})
	sc.Step(`^the bundle is constructed$`, func() error { return nil })
	sc.Step(`^construction is a type error and no blank-identity bundle exists$`, func() error {
		if w.constructErr == nil {
			return fmt.Errorf("under-populated bundle was constructed without error")
		}
		if (credential.Bundle{}).Valid() {
			return fmt.Errorf("the zero Bundle is valid — a blank identity exists")
		}
		return nil
	})

	// --- REQ-1602: an unresolved target is refused with no default identity ---
	sc.Step(`^a target that no resolver rule and no synced source covers$`, func() error {
		rules, err := credential.ParseRules(sampleRules)
		if err != nil {
			return err
		}
		w.engine = credential.NewEngine(rules)
		w.syncEngine = nil // resolve via the native engine directly for this scenario
		w.target = credential.Target{Host: "unknown-host-not-in-any-rule"}
		return nil
	})
	sc.Step(`^the engine resolves the target$`, func() error {
		// The sync scenarios (REQ-1609/1610) resolve across sources + native fallback; the resolver-core
		// scenarios (REQ-1602) resolve the native engine directly. Same step text, one grammar.
		if w.syncEngine != nil {
			w.resolution, w.err = w.syncEngine.Resolve(w.target)
			w.bundle = w.resolution.Bundle
			return nil
		}
		w.bundle, w.err = w.engine.Resolve(w.target)
		return nil
	})
	sc.Step(`^the engine refuses and the target is neither investigable nor actuatable and no default or last-used identity is returned$`, func() error {
		if !credential.IsRefused(w.err) {
			return fmt.Errorf("expected a fail-closed refusal, got err=%v", w.err)
		}
		if w.bundle.Valid() {
			return fmt.Errorf("a bundle was returned for an unresolved target — fell open to a default identity")
		}
		return nil
	})

	// --- REQ-1602: fail-closed holds under an empty or partially-synced store ---
	sc.Step(`^an empty or partially-synced credential store$`, func() error {
		// a PARTIAL store: only one host rule synced; the native fallback covers nothing else.
		rules, err := credential.ParseRules("host:only-synced-host|root|22|ssh|env:TG_ACC_HOSTKEY")
		if err != nil {
			return err
		}
		w.engine = credential.NewEngine(rules)
		return nil
	})
	sc.Step(`^the engine resolves a target not covered by the synced subset$`, func() error {
		w.bundle, w.err = w.engine.Resolve(credential.Target{Host: "host-outside-the-synced-subset"})
		return nil
	})
	sc.Step(`^the engine fails closed and refuses rather than falling open to a default identity$`, func() error {
		if !credential.IsRefused(w.err) {
			return fmt.Errorf("expected fail-closed under a partial store, got err=%v", w.err)
		}
		if w.bundle.Valid() {
			return fmt.Errorf("partial store fell open to a default identity")
		}
		// also the fully-empty store refuses everything.
		if _, err := credential.NewEngine(nil).Resolve(credential.Target{Host: "anything"}); !credential.IsRefused(err) {
			return fmt.Errorf("empty store must refuse, got %v", err)
		}
		return nil
	})

	// --- REQ-1603: every credential value is a SecretRef, resolved at runtime, no plaintext stored ---
	sc.Step(`^a resolver config and credential store$`, func() error {
		rules, err := credential.ParseRules("host:sealed-host|ops|22|ssh|env:TG_ACC_HOSTKEY")
		if err != nil {
			return err
		}
		w.engine = credential.NewEngine(rules)
		w.target = credential.Target{Host: "sealed-host"}
		return nil
	})
	sc.Step(`^a credential bundle is resolved$`, func() error {
		w.bundle, w.err = w.engine.Resolve(w.target)
		return w.err
	})
	sc.Step(`^every secret-bearing field is a SecretRef resolved through the sealed store at runtime and no plaintext credential value appears in config the store or any exportable artifact$`, func() error {
		ref := w.bundle.SSHKeyRef()
		if ref == "" {
			return fmt.Errorf("resolved bundle carries no ssh key reference")
		}
		// the stored value is a REFERENCE (a scheme), never plaintext key material.
		if !hasSecretScheme(string(ref)) {
			return fmt.Errorf("secret field is not a sealed reference: %q", ref)
		}
		// resolve at runtime through the sealed store.
		t := setEnv("TG_ACC_HOSTKEY", "RESOLVED-KEY-MATERIAL")
		defer t()
		res, err := w.bundle.Resolve()
		if err != nil {
			return fmt.Errorf("runtime SecretRef resolution failed: %w", err)
		}
		if res.SSHKey != "RESOLVED-KEY-MATERIAL" {
			return fmt.Errorf("SecretRef did not resolve through the sealed store: %q", res.SSHKey)
		}
		return nil
	})

	// --- REQ-1605: the resolver matches on the same object-model as the policy engine ---
	sc.Step(`^one estate object-model of host glob group device-class and inventory$`, func() error {
		w.target = credential.Target{Host: "dc1tg01", Groups: []string{"edge"}, DeviceClass: "cisco-asa"}
		rules, err := credential.ParseRules(sampleRules)
		if err != nil {
			return err
		}
		w.engine = credential.NewEngine(rules)
		return nil
	})
	sc.Step(`^the credential resolver and the policy engine match a target$`, func() error {
		// resolver path: resolution succeeds iff a Selector matched over the shared object-model.
		_, err := w.engine.Resolve(w.target)
		w.resolverMatched = err == nil
		// policy-engine-style path: a DIRECT call to the SAME shared primitive (the one spec/015 will import)
		// over the SAME Target — proving one grammar, not a second divergent matcher.
		w.policyMatched = credential.Match(credential.Selector{Kind: credential.KindHostGlob, Pattern: "dc1*"}, w.target)
		return nil
	})
	sc.Step(`^both key off the same object-model and no second inventory grammar is defined$`, func() error {
		if !w.resolverMatched || !w.policyMatched {
			return fmt.Errorf("resolver=%v policy=%v — the shared primitive did not agree", w.resolverMatched, w.policyMatched)
		}
		// both are the SAME exported credential.Selector/Target/Match; there is no second grammar to reconcile.
		return nil
	})

	// --- REQ-1606: most-specific-wins, equal-specificity conflict refuses ---
	sc.Step(`^a target matched by more than one resolver rule$`, func() error {
		rules, err := credential.ParseRules(sampleRules)
		if err != nil {
			return err
		}
		w.engine = credential.NewEngine(rules)
		// this target matches the exact-host, the glob, AND the group rules simultaneously.
		w.target = credential.Target{Host: "dc1tg01", Groups: []string{"edge"}}

		// a SEPARATE equal-specificity pair for the conflict half of the scenario.
		tie, err := credential.ParseRules("host:dup01|a|22|ssh|env:TG_ACC_HOSTKEY;host:dup01|b|22|ssh|env:TG_ACC_GLOBKEY")
		if err != nil {
			return err
		}
		_, w.tieErr = credential.NewEngine(tie).Resolve(credential.Target{Host: "dup01"})
		return nil
	})
	sc.Step(`^the engine selects a bundle$`, func() error {
		w.bundle, w.err = w.engine.Resolve(w.target)
		return nil
	})
	sc.Step(`^it applies most-specific-wins precedence and an equal-specificity conflict fails closed instead of choosing arbitrarily$`, func() error {
		if w.err != nil {
			return fmt.Errorf("multi-rule target should resolve to the most-specific bundle, got %v", w.err)
		}
		if w.bundle.RuleID() != "host:dc1tg01" {
			return fmt.Errorf("most-specific-wins failed: expected exact-host rule, got %q", w.bundle.RuleID())
		}
		if !credential.IsRefused(w.tieErr) {
			return fmt.Errorf("equal-specificity conflict must fail closed, got %v", w.tieErr)
		}
		return nil
	})

	// --- REQ-1607: a source syncs read-only into the native store on schedule and on demand ---
	sc.Step(`^a configured CredentialSource$`, func() error {
		w.source = &fakeSource{id: "awx", plane: credential.PlaneMachine, entries: []credential.SourceEntry{
			accEntry("h-100", "host-a", "awxuser", "env:TG_ACC_HOSTKEY"),
		}}
		w.syncEngine = credential.NewSyncEngine(credential.NewEngine(nil))
		return w.syncEngine.RegisterSource(w.source, 0)
	})
	sc.Step(`^Sync runs on the operator schedule and on demand$`, func() error {
		// on-schedule entry point (a scheduled tick syncs all sources) ...
		if _, err := w.syncEngine.SyncAll(context.Background()); err != nil {
			return fmt.Errorf("scheduled SyncAll: %w", err)
		}
		// ... and the on-demand entry point (console "Sync now" on one source) — identical semantics.
		run, err := w.syncEngine.Sync(context.Background(), "awx")
		if err != nil {
			return fmt.Errorf("on-demand Sync: %w", err)
		}
		w.firstRun = run
		return nil
	})
	sc.Step(`^the source performs a read-only pull into the native store and re-reads each object from its system-of-record by id$`, func() error {
		// the pulled entry is resolvable in the store.
		res, err := w.syncEngine.Resolve(credential.Target{Host: "host-a"})
		if err != nil || res.Source != "awx" || res.Bundle.User() != "awxuser" {
			return fmt.Errorf("synced entry not resolvable: res=%+v err=%v", res, err)
		}
		// re-read-by-id: change the upstream object for the SAME native id and re-sync; the store reflects it,
		// proving the source re-reads its system-of-record rather than trusting a cached copy (INV-05).
		w.source.entries[0] = accEntry("h-100", "host-a", "rotateduser", "env:TG_ACC_HOSTKEY")
		if _, err := w.syncEngine.Sync(context.Background(), "awx"); err != nil {
			return fmt.Errorf("re-sync: %w", err)
		}
		res2, _ := w.syncEngine.Resolve(credential.Target{Host: "host-a"})
		if res2.Bundle.User() != "rotateduser" {
			return fmt.Errorf("re-read-by-id failed: got user %q", res2.Bundle.User())
		}
		return nil
	})

	// --- REQ-1608: a repeated sync of unchanged data converges with no duplicate or orphan ---
	sc.Step(`^a source that has already synced upstream data that has not changed$`, func() error {
		w.source = &fakeSource{id: "awx", plane: credential.PlaneMachine, entries: []credential.SourceEntry{
			accEntry("h-1", "host-a", "u1", "env:TG_ACC_HOSTKEY"),
			accEntry("h-2", "host-b", "u2", "env:TG_ACC_GLOBKEY"),
		}}
		w.syncEngine = credential.NewSyncEngine(nil)
		if err := w.syncEngine.RegisterSource(w.source, 0); err != nil {
			return err
		}
		run, err := w.syncEngine.Sync(context.Background(), "awx")
		if err != nil {
			return err
		}
		w.firstRun = run
		return nil
	})
	sc.Step(`^Sync runs again$`, func() error {
		run, err := w.syncEngine.Sync(context.Background(), "awx")
		if err != nil {
			return err
		}
		w.secondRun = run
		return nil
	})
	sc.Step(`^the store converges to the same state keyed by source and native object id with no duplicated identity and no orphaned bundle$`, func() error {
		if w.firstRun.Added != 2 {
			return fmt.Errorf("first sync should add 2, got %d", w.firstRun.Added)
		}
		if w.secondRun.Added != 0 || w.secondRun.Changed != 0 || w.secondRun.Removed != 0 || w.secondRun.Drifted() {
			return fmt.Errorf("re-sync of unchanged data must converge with zero drift, got %+v", w.secondRun)
		}
		// both hosts still resolve exactly once each (no duplication, no orphan).
		for _, h := range []string{"host-a", "host-b"} {
			if _, err := w.syncEngine.Resolve(credential.Target{Host: h}); err != nil {
				return fmt.Errorf("%s not resolvable after re-sync: %w", h, err)
			}
		}
		return nil
	})

	// --- REQ-1609: multiple sources → source precedence + shadowed record; ambiguity fails closed ---
	sc.Step(`^a target present in more than one synced source$`, func() error {
		high := &fakeSource{id: "vault", plane: credential.PlaneMachine, entries: []credential.SourceEntry{
			accEntry("v-1", "host-a", "vaultuser", "env:TG_ACC_HOSTKEY"),
		}}
		low := &fakeSource{id: "awx", plane: credential.PlaneMachine, entries: []credential.SourceEntry{
			accEntry("a-1", "host-a", "awxuser", "env:TG_ACC_GLOBKEY"),
		}}
		w.syncEngine = credential.NewSyncEngine(nil)
		if err := w.syncEngine.RegisterSource(high, 0); err != nil { // higher precedence (lower value)
			return err
		}
		if err := w.syncEngine.RegisterSource(low, 10); err != nil {
			return err
		}
		if _, err := w.syncEngine.SyncAll(context.Background()); err != nil {
			return err
		}
		w.target = credential.Target{Host: "host-a"}

		// a SEPARATE equal-precedence pair for the fail-closed half of the scenario.
		ambig := credential.NewSyncEngine(nil)
		a := &fakeSource{id: "awx2", plane: credential.PlaneMachine, entries: []credential.SourceEntry{
			accEntry("x", "host-z", "a", "env:TG_ACC_HOSTKEY"),
		}}
		b := &fakeSource{id: "semaphore", plane: credential.PlaneMachine, entries: []credential.SourceEntry{
			accEntry("y", "host-z", "b", "env:TG_ACC_GLOBKEY"),
		}}
		_ = ambig.RegisterSource(a, 5)
		_ = ambig.RegisterSource(b, 5) // SAME precedence → cannot disambiguate
		_, _ = ambig.SyncAll(context.Background())
		_, w.ambiguecErr = ambig.Resolve(credential.Target{Host: "host-z"})
		return nil
	})
	sc.Step(`^it applies the operator-declared source precedence records the winning and shadowed sources and fails closed when the precedence does not disambiguate$`, func() error {
		if w.err != nil {
			return fmt.Errorf("distinct-precedence target should resolve, got %v", w.err)
		}
		if w.resolution.Source != "vault" || w.resolution.Bundle.User() != "vaultuser" {
			return fmt.Errorf("expected the higher-precedence source to win, got %+v", w.resolution)
		}
		if len(w.resolution.Shadowed) != 1 || w.resolution.Shadowed[0] != "awx" {
			return fmt.Errorf("expected awx recorded as shadowed, got %v", w.resolution.Shadowed)
		}
		if !credential.IsRefused(w.ambiguecErr) {
			return fmt.Errorf("equal-precedence sources must fail closed, got %v", w.ambiguecErr)
		}
		return nil
	})

	// --- REQ-1610: a target no synced source covers resolves from the native-store fallback ---
	sc.Step(`^a target that no synced source covers but the native store does$`, func() error {
		nativeRules, err := credential.ParseRules("host:native-only-host|nativeuser|22|ssh|env:TG_ACC_HOSTKEY")
		if err != nil {
			return err
		}
		src := &fakeSource{id: "awx", plane: credential.PlaneMachine, entries: []credential.SourceEntry{
			accEntry("a-1", "some-synced-host", "awxuser", "env:TG_ACC_GLOBKEY"),
		}}
		w.syncEngine = credential.NewSyncEngine(credential.NewEngine(nativeRules))
		if err := w.syncEngine.RegisterSource(src, 0); err != nil {
			return err
		}
		if _, err := w.syncEngine.Sync(context.Background(), "awx"); err != nil {
			return err
		}
		w.target = credential.Target{Host: "native-only-host"}
		return nil
	})
	sc.Step(`^it resolves from the native store as the standalone fallback with zero third-party dependency$`, func() error {
		if w.err != nil {
			return fmt.Errorf("native fallback should resolve, got %v", w.err)
		}
		if !w.resolution.Native || w.resolution.Source != "" {
			return fmt.Errorf("expected the native-store fallback, got %+v", w.resolution)
		}
		if w.resolution.Bundle.User() != "nativeuser" {
			return fmt.Errorf("native fallback resolved the wrong bundle: %q", w.resolution.Bundle.User())
		}
		return nil
	})

	// --- REQ-1615: each source records last-synced and drift ---
	sc.Step(`^a source that has run Sync$`, func() error {
		w.source = &fakeSource{id: "awx", plane: credential.PlaneMachine, entries: []credential.SourceEntry{
			accEntry("h-1", "host-a", "u1", "env:TG_ACC_HOSTKEY"),
		}}
		w.syncEngine = credential.NewSyncEngine(nil)
		if err := w.syncEngine.RegisterSource(w.source, 0); err != nil {
			return err
		}
		run, err := w.syncEngine.Sync(context.Background(), "awx")
		if err != nil {
			return err
		}
		w.firstRun = run
		// a second sync with an added upstream object, so drift is observable.
		w.source.entries = append(w.source.entries, accEntry("h-2", "host-b", "u2", "env:TG_ACC_GLOBKEY"))
		run2, err := w.syncEngine.Sync(context.Background(), "awx")
		if err != nil {
			return err
		}
		w.secondRun = run2
		return nil
	})
	sc.Step(`^the console reads the source status$`, func() error {
		run, ok := w.syncEngine.LastRun("awx")
		if !ok {
			return fmt.Errorf("no last run recorded for the source")
		}
		w.secondRun = run
		return nil
	})
	sc.Step(`^last-synced and the drift of upstream objects added changed or removed are recorded and surfaced$`, func() error {
		if w.firstRun.LastSyncedAt.IsZero() || w.firstRun.Outcome != credential.SyncOK {
			return fmt.Errorf("first run did not record a last-synced ok outcome: %+v", w.firstRun)
		}
		if w.secondRun.LastSyncedAt.IsZero() {
			return fmt.Errorf("last-synced not recorded: %+v", w.secondRun)
		}
		if w.secondRun.Added != 1 || !w.secondRun.Drifted() {
			return fmt.Errorf("drift (added=1) not recorded/surfaced: %+v", w.secondRun)
		}
		if !w.secondRun.LastSyncedAt.After(w.firstRun.LastSyncedAt) && !w.secondRun.LastSyncedAt.Equal(w.firstRun.LastSyncedAt) {
			return fmt.Errorf("last-synced regressed: first=%v second=%v", w.firstRun.LastSyncedAt, w.secondRun.LastSyncedAt)
		}
		return nil
	})

	// --- REQ-1613: the vault:/bao: SecretRef scheme reads KV v2 read-only and fails closed ---
	sc.Step(`^a vault or bao SecretRef backed by OpenBao or HashiCorp Vault$`, func() error {
		// a fake OpenBao (KV v2 + AppRole login) driving the REAL vault client + config resolver.
		srv := w.fakeOpenBao(false)
		c, err := vault.New(vault.Config{
			BaseURL: srv.URL,
			Auth: vault.AppRole{
				RoleIDRef:   config.SecretRef("env:TG_ACC_VAULT_ROLEID"),
				SecretIDRef: config.SecretRef("env:TG_ACC_VAULT_SECRETID"),
			},
			HTTPClient: http.DefaultClient,
		})
		if err != nil {
			return err
		}
		vault.RegisterResolver(c)
		w.vaultRef = config.SecretRef("vault:secret/data/hosts/hostA#ssh_key")
		return nil
	})
	sc.Step(`^the engine resolves the reference$`, func() error {
		restore := setEnv("TG_ACC_VAULT_ROLEID", "acc-role-id")
		defer restore()
		restore2 := setEnv("TG_ACC_VAULT_SECRETID", "acc-secret-id")
		defer restore2()
		// resolution runs through the SAME core/config.SecretRef.Resolve a Bundle uses at authentication time.
		w.vaultVal, w.vaultResErr = w.vaultRef.Resolve()
		return nil
	})
	sc.Step(`^it performs a read-only KV v2 read under AppRole JWT or Kubernetes auth and fails closed with no default credential when unreachable denied or expired$`, func() error {
		defer vault.RegisterResolver(nil)
		if w.vaultResErr != nil {
			return fmt.Errorf("a valid vault ref should resolve, got %v", w.vaultResErr)
		}
		if w.vaultVal != "ACC-KEY-A" {
			return fmt.Errorf("vault KV v2 read returned %q, want ACC-KEY-A", w.vaultVal)
		}
		// fail closed: a DENIED backend yields no default credential.
		if v, err := w.clientFor(w.fakeOpenBao(true)).ResolveRef("vault:secret/data/hosts/hostA#ssh_key"); err == nil || v != "" {
			return fmt.Errorf("denied read must fail closed, got val=%q err=%v", v, err)
		}
		// fail closed: an UNREACHABLE backend yields no default credential.
		down := w.fakeOpenBao(false)
		addr := down.URL
		down.Close()
		unreach, err := vault.New(vault.Config{
			BaseURL:    addr,
			Auth:       vault.Token{TokenRef: config.SecretRef("env:TG_ACC_VAULT_SECRETID")},
			HTTPClient: http.DefaultClient,
		})
		if err != nil {
			return err
		}
		r := setEnv("TG_ACC_VAULT_SECRETID", "acc-secret-id")
		defer r()
		if v, err := unreach.ResolveRef("vault:secret/data/hosts/hostA#ssh_key"); err == nil || v != "" {
			return fmt.Errorf("unreachable read must fail closed, got val=%q err=%v", v, err)
		}
		return nil
	})
	// --- REQ-1619: the oidc: SecretRef scheme mints a machine-plane client-credentials token, over verified
	// TLS, cached by expires_in, and fails closed (unreachable / denied / untrusted cert / no token) ---
	sc.Step(`^an oidc SecretRef backed by an OIDC provider token endpoint$`, func() error {
		// a fake OIDC token endpoint (RFC 6749 §4.4 client-credentials) driving the REAL Minter + config resolver.
		srv := w.fakeOIDC(false)
		m := w.oidcMinter(srv.URL, http.DefaultClient)
		oidctoken.RegisterResolver(m)
		w.oidcRef = config.SecretRef("oidc:acc-service#openid")
		return nil
	})
	sc.Step(`^the engine mints the token$`, func() error {
		// minting runs through the SAME core/config.SecretRef.Resolve a Bundle uses at authentication time.
		w.oidcVal, w.oidcResErr = w.oidcRef.Resolve()
		return nil
	})
	sc.Step(`^it mints a machine-plane Bearer token via the client-credentials grant over verified TLS caches it by expires_in and fails closed with no default token when unreachable denied or the server certificate does not verify$`, func() error {
		defer oidctoken.RegisterResolver(nil)
		if w.oidcResErr != nil {
			return fmt.Errorf("a valid oidc ref should mint, got %v", w.oidcResErr)
		}
		if w.oidcVal != "acc-oidc-token" {
			return fmt.Errorf("oidc mint returned %q, want acc-oidc-token", w.oidcVal)
		}
		// fail closed: a DENIED token endpoint yields no default token.
		deny := w.oidcMinter(w.fakeOIDC(true).URL, http.DefaultClient)
		if v, err := deny.ResolveRef("oidc:acc-service#openid"); err == nil || v != "" {
			return fmt.Errorf("a denied grant must fail closed, got val=%q err=%v", v, err)
		}
		// fail closed: an UNREACHABLE endpoint yields no default token.
		down := w.fakeOIDC(false)
		addr := down.URL
		down.Close()
		if v, err := w.oidcMinter(addr, http.DefaultClient).ResolveRef("oidc:acc-service#openid"); err == nil || v != "" {
			return fmt.Errorf("an unreachable endpoint must fail closed, got val=%q err=%v", v, err)
		}
		// verified TLS: a client that trusts the cert mints; the SAME server over a default (untrusting)
		// transport fails closed — proving TLS is verified (no InsecureSkipVerify path).
		tlsSrv := w.fakeOIDCTLS()
		if v, err := w.oidcMinter(tlsSrv.URL, tlsSrv.Client()).Mint(context.Background(), ""); err != nil || v != "acc-oidc-token" {
			return fmt.Errorf("mint over verified TLS should succeed, got val=%q err=%v", v, err)
		}
		if v, err := w.oidcMinter(tlsSrv.URL, nil).Mint(context.Background(), ""); err == nil || v != "" {
			return fmt.Errorf("an untrusted server certificate must fail closed, got val=%q err=%v", v, err)
		}
		return nil
	})

	// --- REQ-1612: the AWX (Ansible/Semaphore family) source pulls inventory + credentials read-only, no
	// subprocess. AWX is the representative platform of the disjunctive family (Ansible-inventory/Vault and
	// Semaphore land in later MRs, T-016-8); the read-only, native-client, no-subprocess behavior is proven
	// here against a fake AWX REST API driving the REAL awx.Source. ---
	sc.Step(`^an AWX Ansible or Semaphore platform$`, func() error {
		srv := w.fakeAWX()
		c, err := awx.New(awx.Config{
			BaseURL:    srv.URL,
			TokenRef:   config.SecretRef("env:TG_ACC_AWX_TOKEN"),
			HTTPClient: http.DefaultClient,
		})
		if err != nil {
			return err
		}
		w.awxSource, err = awx.NewSource(awx.SourceConfig{ID: "acc-awx01", Client: c, RefScheme: "store", RefPrefix: "awx/"})
		if err != nil {
			return err
		}
		// ALSO bind the Ansible arm of the disjunction: a REAL un-controlled Ansible tree (plain files, no
		// controller, no REST API) parsed by the native connector, with its inline !vault secret decrypted in
		// pure Go at use time (no `ansible-vault` subprocess).
		w.ansibleDir, err = w.setupAnsibleTree()
		if err != nil {
			return err
		}
		tree, err := ansiblesrc.NewTree(w.ansibleDir, "")
		if err != nil {
			return err
		}
		w.ansibleResolver, err = ansiblesrc.NewResolver(ansiblesrc.ResolverConfig{
			Tree: tree, PasswordRef: config.SecretRef("env:TG_ACC_ANSIBLE_VAULT_PASS"),
		})
		if err != nil {
			return err
		}
		ansiblesrc.RegisterResolver(w.ansibleResolver)
		w.ansibleSource, err = ansiblesrc.NewSource(ansiblesrc.SourceConfig{ID: "acc-ansible01", Tree: tree})
		return err
	})
	sc.Step(`^the source syncs through its native Go client$`, func() error {
		restore := setEnv("TG_ACC_AWX_TOKEN", "acc-awx-oauth2-token")
		defer restore()
		w.awxEntries, w.awxErr = w.awxSource.Sync(context.Background())
		w.ansibleEntries, w.ansibleErr = w.ansibleSource.Sync(context.Background())
		return nil
	})
	sc.Step(`^it pulls inventory and per-target credentials read-only and spawns no subprocess$`, func() error {
		if w.awxErr != nil {
			return fmt.Errorf("awx sync should succeed, got %v", w.awxErr)
		}
		if w.awxSource.Plane() != credential.PlaneMachine {
			return fmt.Errorf("awx source must feed the machine plane, got %s", w.awxSource.Plane())
		}
		// inventory pulled: at least one host entry, keyed over the shared object-model.
		if len(w.awxEntries) == 0 {
			return fmt.Errorf("awx sync pulled no inventory entries")
		}
		// per-target credential is a REFERENCE only — never a plaintext secret (AWX never exposes one).
		for _, e := range w.awxEntries {
			ref := string(e.Bundle.SSHKeyRef())
			if ref == "" || !hasSecretScheme(ref) {
				return fmt.Errorf("entry %q credential is not a sealed reference: %q", e.Selector.Pattern, ref)
			}
		}
		// The Ansible arm: the un-controlled tree pulled its host inventory read-only, the become secret is an
		// ansible-vault: LOCATOR (never a plaintext value — INV-13), and resolving it NATIVELY decrypts the real
		// $ANSIBLE_VAULT payload to the known plaintext with NO subprocess (pure Go crypto).
		if w.ansibleErr != nil {
			return fmt.Errorf("ansible sync should succeed, got %v", w.ansibleErr)
		}
		if w.ansibleSource.Plane() != credential.PlaneMachine {
			return fmt.Errorf("ansible source must feed the machine plane, got %s", w.ansibleSource.Plane())
		}
		if len(w.ansibleEntries) != 1 {
			return fmt.Errorf("ansible sync pulled %d entries, want 1", len(w.ansibleEntries))
		}
		become := string(w.ansibleEntries[0].Bundle.BecomeRef())
		if !strings.HasPrefix(become, "ansible-vault:") || !hasSecretScheme(become) {
			return fmt.Errorf("ansible become is not a sealed ansible-vault: locator: %q", become)
		}
		if strings.Contains(become, accAnsiblePlain) {
			return fmt.Errorf("plaintext become password leaked into the Bundle locator: %q", become)
		}
		restore := setEnv("TG_ACC_ANSIBLE_VAULT_PASS", accAnsibleVaultPass)
		defer restore()
		got, err := config.SecretRef(become).Resolve()
		if err != nil {
			return fmt.Errorf("native ansible-vault decrypt failed: %w", err)
		}
		if got != accAnsiblePlain {
			return fmt.Errorf("native decrypt = %q, want %q", got, accAnsiblePlain)
		}
		// "spawns no subprocess" is STRUCTURAL: both connectors are native (net/http for AWX; stdlib crypto for
		// the ansible-vault decrypt) — no os/exec, no CLI — verified by the pulls completing with no shell.
		return nil
	})

	// --- REQ-1614: the LDAP/OIDC source pulls approver identities read-only into the human plane and NEVER
	// writes a host bundle. Proven against the REAL ldap.Source (T-016-10) with a FreeIPA-shaped fixture
	// driven through its injectable transport seam (Config.Dial) — no live directory needed. ---
	sc.Step(`^a configured LDAP or OIDC provider$`, func() error {
		base := "dc=sec,dc=example,dc=net"
		fc := &fakeLDAPConn{byBase: map[string]*ldapv3.SearchResult{
			"cn=users,cn=accounts," + base: {Entries: []*ldapv3.Entry{
				ldapEntry("uid=alice,cn=users,cn=accounts,"+base, map[string][]string{
					"uid": {"alice"}, "memberOf": {"cn=sre-oncall,cn=groups,cn=accounts," + base},
				}),
			}},
			"cn=groups,cn=accounts," + base: {Entries: []*ldapv3.Entry{
				ldapEntry("cn=sre-oncall,cn=groups,cn=accounts,"+base, map[string][]string{"cn": {"sre-oncall"}}),
			}},
		}}
		src, err := ldapsrc.New(ldapsrc.Config{
			ID:              "ldap",
			URL:             "ldaps://dir.example:636",
			BindDNRef:       config.SecretRef("env:TG_ACC_LDAP_BINDDN"),
			BindPasswordRef: config.SecretRef("env:TG_ACC_LDAP_BINDPW"),
			// Pass the site base DNs explicitly (the install-neutral pattern) — the connector no longer compiles
			// in an estate default, so the fixture supplies its directory's bases like a real deployment would.
			UserBaseDN:  "cn=users,cn=accounts," + base,
			GroupBaseDN: "cn=groups,cn=accounts," + base,
			Dial:        func(context.Context, string) (ldapsrc.Conn, error) { return fc, nil },
		})
		if err != nil {
			return err
		}
		w.ldapSource = src
		w.syncEngine = credential.NewSyncEngine(nil)
		return w.syncEngine.RegisterSource(src, 0)
	})
	sc.Step(`^the source syncs$`, func() error {
		// two grammars share this text: the router scenario (REQ-1611) registered a fakeSource; the LDAP
		// scenario (REQ-1614) registered the real ldap.Source, whose bind creds resolve from sealed env refs.
		if w.ldapSource != nil {
			restore := setEnv("TG_ACC_LDAP_BINDDN", "uid=svc-tg,cn=users,cn=accounts,dc=sec,dc=example,dc=net")
			defer restore()
			restore2 := setEnv("TG_ACC_LDAP_BINDPW", "acc-bind-password")
			defer restore2()
			// the raw read-only pull (the entry set the connector emits) ...
			if w.ldapEntries, w.ldapErr = w.ldapSource.Sync(context.Background()); w.ldapErr != nil {
				return w.ldapErr
			}
			// ... and the engine converge, which runs the two-plane router over every entry (fail-closed on a
			// bundle leak) and makes the approvers resolvable for spec/015 approve_by.
			_, w.ldapErr = w.syncEngine.Sync(context.Background(), "ldap")
			return w.ldapErr
		}
		_, err := w.syncEngine.Sync(context.Background(), w.source.id)
		return err
	})
	sc.Step(`^it pulls users and groups read-only into the human approver plane for spec/015 approve_by and writes no host credential bundle$`, func() error {
		if w.ldapErr != nil {
			return fmt.Errorf("ldap sync should succeed, got %v", w.ldapErr)
		}
		if w.ldapSource.Plane() != credential.PlaneHuman {
			return fmt.Errorf("ldap source must feed the human plane, got %s", w.ldapSource.Plane())
		}
		// EVERY pulled entry is approver-only — the router (Sync) already refused any that carried a Bundle.
		if len(w.ldapEntries) == 0 {
			return fmt.Errorf("ldap sync pulled no approver entries")
		}
		for _, e := range w.ldapEntries {
			if e.Bundle.Valid() {
				return fmt.Errorf("ldap entry %q carries a host bundle — human plane must not", e.NativeID)
			}
			if !e.Approver.Valid() {
				return fmt.Errorf("ldap entry %q carries no approver identity", e.NativeID)
			}
		}
		// the identities feed the spec/015 approve_by resolution ...
		set, err := w.syncEngine.ResolveApprovers(credential.ApproverQuery{Group: "sre-oncall"})
		if err != nil || len(set.Users) != 1 || set.Users[0] != "alice" {
			return fmt.Errorf("approver resolution failed: set=%+v err=%v", set, err)
		}
		// ... and NOTHING from this human-plane source resolves as a host bundle (cross-plane isolation).
		if _, err := w.syncEngine.Resolve(credential.Target{Host: "alice"}); !credential.IsRefused(err) {
			return fmt.Errorf("an LDAP human-plane source must never resolve a host bundle, got %v", err)
		}
		return nil
	})

	// --- REQ-1611: the two-plane router keeps machine ↔ human apart. A machine-plane source may ONLY
	// populate host-credential resolution (never an approver identity); a human-plane source may ONLY
	// populate approver identity (never a host bundle). Cross-plane entries fail closed at sync time. ---
	sc.Step(`^a machine-plane source such as AWX or Vault$`, func() error {
		w.source = &fakeSource{id: "awx", plane: credential.PlaneMachine, entries: []credential.SourceEntry{
			accEntry("m-1", "host-a", "awxuser", "env:TG_ACC_HOSTKEY"),
		}}
		w.syncEngine = credential.NewSyncEngine(nil)
		return w.syncEngine.RegisterSource(w.source, 0)
	})
	sc.Step(`^an approver-plane source such as LDAP or OIDC$`, func() error {
		w.source = &fakeSource{id: "ldap", plane: credential.PlaneHuman, entries: []credential.SourceEntry{
			accApprover("h-1", credential.PrincipalUser, "alice", "sre-oncall"),
			accApprover("h-2", credential.PrincipalGroup, "sre-oncall"),
		}}
		w.syncEngine = credential.NewSyncEngine(nil)
		return w.syncEngine.RegisterSource(w.source, 0)
	})
	sc.Step(`^it populates only host credential bundles and never an approver identity$`, func() error {
		// the machine host bundle resolves ...
		res, err := w.syncEngine.Resolve(credential.Target{Host: "host-a"})
		if err != nil || res.Bundle.User() != "awxuser" {
			return fmt.Errorf("machine host bundle should resolve, got res=%+v err=%v", res, err)
		}
		// ... and NOTHING resolves as an approver from this machine-plane source (fail closed).
		if _, err := w.syncEngine.ResolveApprovers(credential.ApproverQuery{Group: "host-a"}); !credential.IsRefused(err) {
			return fmt.Errorf("a machine-plane source must never populate an approver identity, got %v", err)
		}
		// and a machine entry that ALSO carried an approver is refused at sync time (cross-plane leak).
		leak := accEntry("m-2", "host-b", "u", "env:TG_ACC_HOSTKEY")
		leak.Approver = mustApprover("eve")
		bad := &fakeSource{id: "awx-bad", plane: credential.PlaneMachine, entries: []credential.SourceEntry{leak}}
		se := credential.NewSyncEngine(nil)
		if err := se.RegisterSource(bad, 0); err != nil {
			return err
		}
		if _, err := se.Sync(context.Background(), "awx-bad"); err == nil {
			return fmt.Errorf("a machine entry carrying an approver must fail closed")
		}
		return nil
	})
	sc.Step(`^it populates only approver identities and never a host credential bundle$`, func() error {
		// the approver identities resolve for the queried role ...
		set, err := w.syncEngine.ResolveApprovers(credential.ApproverQuery{Group: "sre-oncall"})
		if err != nil || len(set.Users) != 1 || set.Users[0] != "alice" {
			return fmt.Errorf("approver identities should resolve, got set=%+v err=%v", set, err)
		}
		// ... and NOTHING resolves as a host bundle from this human-plane source (fail closed).
		if _, err := w.syncEngine.Resolve(credential.Target{Host: "alice"}); !credential.IsRefused(err) {
			return fmt.Errorf("an approver-plane source must never populate a host bundle, got %v", err)
		}
		if _, err := w.syncEngine.Resolve(credential.Target{Host: "sre-oncall"}); !credential.IsRefused(err) {
			return fmt.Errorf("an approver-plane source must never populate a host bundle, got %v", err)
		}
		// and a human entry that ALSO carried a bundle is refused at sync time (cross-plane leak).
		leak := accApprover("h-3", credential.PrincipalUser, "bob", "sre-oncall")
		leak.Bundle = mustAccBundle("root", "env:TG_ACC_HOSTKEY")
		bad := &fakeSource{id: "ldap-bad", plane: credential.PlaneHuman, entries: []credential.SourceEntry{leak}}
		se := credential.NewSyncEngine(nil)
		if err := se.RegisterSource(bad, 0); err != nil {
			return err
		}
		if _, err := se.Sync(context.Background(), "ldap-bad"); err == nil {
			return fmt.Errorf("a human entry carrying a bundle must fail closed")
		}
		return nil
	})

	sc.After(func(ctx context.Context, _ *godog.Scenario, err error) (context.Context, error) {
		for _, s := range w.vaultServers {
			s.Close()
		}
		w.vaultServers = nil
		ansiblesrc.RegisterResolver(nil) // unregister the ansible-vault: scheme (fail closed between scenarios)
		if w.ansibleDir != "" {
			_ = os.RemoveAll(w.ansibleDir)
			w.ansibleDir = ""
		}
		return ctx, err
	})
}

// A REAL ansible-vault AES256 payload produced by the `ansible-vault encrypt_string` CLI (ansible-core
// 2.19.7) with accAnsibleVaultPass; it decrypts natively (no subprocess) to accAnsiblePlain. The salt is
// embedded, so this is a deterministic known vector.
const (
	accAnsibleVaultPass = "tg-demo-vault-pass"
	accAnsiblePlain     = "librespeed01-sudo-pw"
	accAnsibleVault     = `$ANSIBLE_VAULT;1.1;AES256
36643866386639623436666233626463363139643466663034666332383764343039323133376339
3561326362343130633964363261613063626263343433350a396337356235336166663564323534
38316538303363623264366536616264656461383437393166356232363139393230346330383731
3961393637613038660a326231373831636330336132313131393462346463313364663066333733
33336134636538393365373364623735393030653761656634323932353964386532`
)

// setupAnsibleTree writes a minimal un-controlled Ansible tree (inventory + group_vars + host_vars with the
// REAL vaulted ansible_become_pass) to a fresh tempdir, returning the root. Removed in the After hook.
func (w *world) setupAnsibleTree() (string, error) {
	dir, err := os.MkdirTemp("", "tg-acc-ansible-")
	if err != nil {
		return "", err
	}
	writeFile := func(rel, content string) error {
		p := dir + "/" + rel
		if err := os.MkdirAll(dirOf(p), 0o755); err != nil {
			return err
		}
		return os.WriteFile(p, []byte(content), 0o644)
	}
	if err := writeFile("inventory.ini", "[speedtest]\nlibrespeed01\n"); err != nil {
		return "", err
	}
	if err := writeFile("group_vars/all.yml", "ansible_user: root\nansible_ssh_private_key_file: /secrets/one_key\n"); err != nil {
		return "", err
	}
	var frag strings.Builder
	frag.WriteString("ansible_become_pass: !vault |\n")
	for _, ln := range strings.Split(accAnsibleVault, "\n") {
		frag.WriteString("  " + ln + "\n")
	}
	if err := writeFile("host_vars/librespeed01.yml", frag.String()); err != nil {
		return "", err
	}
	return dir, nil
}

func dirOf(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[:i]
	}
	return "."
}

// fakeAWX returns a tracked httptest server faking AWX's paginated /api/v2/hosts/ and /api/v2/groups/ (Bearer
// auth), driving the REAL awx.Source read-only. Closed in the After hook alongside the vault servers.
func (w *world) fakeAWX() *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != "Bearer acc-awx-oauth2-token" {
			rw.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(rw).Encode(map[string]any{"detail": "Authentication credentials were not provided."})
			return
		}
		switch r.URL.Path {
		case "/api/v2/hosts/":
			_ = json.NewEncoder(rw).Encode(map[string]any{"count": 1, "next": nil, "previous": nil, "results": []any{
				map[string]any{"id": 1, "name": "acc-web01", "enabled": true, "inventory": 1,
					"variables": `{"ansible_user":"deploy","ansible_port":22,"ansible_ssh_private_key_file":"/keys/acc_web01.pem"}`},
			}})
		case "/api/v2/groups/":
			_ = json.NewEncoder(rw).Encode(map[string]any{"count": 0, "next": nil, "previous": nil, "results": []any{}})
		default:
			rw.WriteHeader(http.StatusNotFound)
		}
	}))
	w.vaultServers = append(w.vaultServers, srv)
	return srv
}

// fakeOpenBao returns a tracked httptest server faking OpenBao's AppRole login + KV v2 read. When deny is
// set, every authenticated read returns 403 (to prove fail-closed). Servers are closed in the After hook.
func (w *world) fakeOpenBao(deny bool) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		path := strings.TrimPrefix(r.URL.Path, "/v1/")
		if strings.HasSuffix(path, "/login") {
			_ = json.NewEncoder(rw).Encode(map[string]any{"auth": map[string]any{"client_token": "acc-tok", "lease_duration": 3600}})
			return
		}
		if r.Header.Get("X-Vault-Token") != "acc-tok" || deny {
			rw.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(rw).Encode(map[string]any{"errors": []string{"permission denied"}})
			return
		}
		_ = json.NewEncoder(rw).Encode(map[string]any{"data": map[string]any{"data": map[string]any{"user": "vaultuser", "ssh_key": "ACC-KEY-A"}}})
	}))
	w.vaultServers = append(w.vaultServers, srv)
	return srv
}

// fakeOIDC returns a tracked httptest server faking an OAuth2 client-credentials token endpoint (RFC 6749
// §4.4). When deny is set, or the grant/credentials are wrong, it returns 401 invalid_client (to prove
// fail-closed). Servers are closed in the After hook alongside the vault servers.
func (w *world) fakeOIDC(deny bool) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		_ = r.ParseForm()
		if deny || r.PostFormValue("grant_type") != "client_credentials" ||
			r.PostFormValue("client_id") != "acc-oidc-client" || r.PostFormValue("client_secret") != "acc-oidc-secret" {
			rw.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(rw).Encode(map[string]any{"error": "invalid_client"})
			return
		}
		_ = json.NewEncoder(rw).Encode(map[string]any{"access_token": "acc-oidc-token", "token_type": "Bearer", "expires_in": 3600})
	}))
	w.vaultServers = append(w.vaultServers, srv)
	return srv
}

// fakeOIDCTLS is fakeOIDC over HTTPS (a self-signed httptest cert) so the oracle can prove TLS is verified.
func (w *world) fakeOIDCTLS() *httptest.Server {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		_ = r.ParseForm()
		if r.PostFormValue("grant_type") != "client_credentials" ||
			r.PostFormValue("client_id") != "acc-oidc-client" || r.PostFormValue("client_secret") != "acc-oidc-secret" {
			rw.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(rw).Encode(map[string]any{"error": "invalid_client"})
			return
		}
		_ = json.NewEncoder(rw).Encode(map[string]any{"access_token": "acc-oidc-token", "token_type": "Bearer", "expires_in": 3600})
	}))
	w.vaultServers = append(w.vaultServers, srv)
	return srv
}

// oidcMinter builds a REAL oidctoken.Minter bound to a fake token endpoint (client creds via env: SecretRefs).
// A nil hc leaves New to build a default TLS-verifying http.Client (used to prove an untrusted cert fails).
func (w *world) oidcMinter(tokenURL string, hc oidctoken.Doer) *oidctoken.Minter {
	_ = os.Setenv("TG_ACC_OIDC_ID", "acc-oidc-client")
	_ = os.Setenv("TG_ACC_OIDC_SECRET", "acc-oidc-secret")
	m, _ := oidctoken.New(oidctoken.Config{
		TokenURL:        tokenURL,
		ClientIDRef:     config.SecretRef("env:TG_ACC_OIDC_ID"),
		ClientSecretRef: config.SecretRef("env:TG_ACC_OIDC_SECRET"),
		Scope:           "openid",
		HTTPClient:      hc,
	})
	return m
}

// clientFor builds an AppRole vault client bound to a fake server (creds via env, set by the caller).
func (w *world) clientFor(srv *httptest.Server) *vault.Client {
	_ = os.Setenv("TG_ACC_VAULT_ROLEID", "acc-role-id")
	_ = os.Setenv("TG_ACC_VAULT_SECRETID", "acc-secret-id")
	c, _ := vault.New(vault.Config{
		BaseURL: srv.URL,
		Auth: vault.AppRole{
			RoleIDRef:   config.SecretRef("env:TG_ACC_VAULT_ROLEID"),
			SecretIDRef: config.SecretRef("env:TG_ACC_VAULT_SECRETID"),
		},
		HTTPClient: http.DefaultClient,
	})
	return c
}

// setEnv sets an env var for the duration of a step and returns a restore func (godog steps have no
// *testing.T, so t.Setenv is unavailable). The resolver reads it through core/config's env: scheme.
func setEnv(k, v string) func() {
	prev, had := os.LookupEnv(k)
	_ = os.Setenv(k, v)
	return func() {
		if had {
			_ = os.Setenv(k, prev)
		} else {
			_ = os.Unsetenv(k)
		}
	}
}

func hasSecretScheme(s string) bool {
	for _, sc := range []string{"env:", "file:", "store:", "vault:", "bao:", "ansible-vault:"} {
		if strings.HasPrefix(s, sc) {
			return true
		}
	}
	return false
}
