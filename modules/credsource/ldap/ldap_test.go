package ldap

// ORACLE tests for the LDAP / FreeIPA human-plane connector (spec/016 T-016-10, REQ-1614). CI has no LDAP
// directory, so the network bind+search is a thin INJECTABLE SEAM (the exported Conn/DialFunc via Config.Dial): a fakeConn
// yields fixture *ldap.Entry search results and the tests drive the REAL Source through it. The entry→
// ApproverIdentity MAPPING is also a pure function tested directly with fixture entries. Together they prove:
// users→user-approvers, groups→group-approvers, memberOf-DNs→group cn memberships, NO Bundle ever set (the
// two-plane router would reject one — we assert Approver-only), the fail-closed matrix (bind-fail /
// unreachable / TLS-verify-fail / empty / oversized / malformed), replica FAILOVER (first down → second used; all down → fail closed), and that the bind password never appears in
// any error/log. Run under -race.
//
// A BEST-EFFORT live check (TestLiveRootDSE) hits the real freeipa01 only when TG_LDAP_LIVE_ADDR is set; it
// does an ANONYMOUS root-DSE read (no creds) as the safe connectivity assertion.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	ldapv3 "github.com/go-ldap/ldap/v3"

	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/credential"
)

// ---- fake seam -------------------------------------------------------------------------------------

// fakeConn is an in-memory LDAP connection for the oracles. It records the bind it saw and returns fixture
// search results keyed by base DN, or forces a bind/search failure.
type fakeConn struct {
	bindErr    error
	searchErr  error
	byBase     map[string]*ldapv3.SearchResult
	sawBindDN  string
	sawBindPw  string
	bindCalled bool
	closed     bool
}

func (f *fakeConn) Bind(username, password string) error {
	f.bindCalled = true
	f.sawBindDN, f.sawBindPw = username, password
	return f.bindErr
}

func (f *fakeConn) Search(req *ldapv3.SearchRequest) (*ldapv3.SearchResult, error) {
	if f.searchErr != nil {
		return nil, f.searchErr
	}
	if r, ok := f.byBase[req.BaseDN]; ok {
		return r, nil
	}
	return &ldapv3.SearchResult{}, nil
}

func (f *fakeConn) Close() error { f.closed = true; return nil }

func entry(dn string, attrs map[string][]string) *ldapv3.Entry {
	e := &ldapv3.Entry{DN: dn}
	for name, vals := range attrs {
		e.Attributes = append(e.Attributes, &ldapv3.EntryAttribute{Name: name, Values: vals})
	}
	return e
}

// freeIPAFake wires a fakeConn with a FreeIPA-shaped fixture (two users, one group) and returns a Source that
// dials it. Env sets the bind SecretRefs so Sync can resolve them.
func freeIPAFake(t *testing.T, fc *fakeConn) *Source {
	t.Helper()
	t.Setenv("TG_LDAP_BIND_DN", "uid=svc-tg,cn=users,cn=accounts,dc=sec,dc=example,dc=net")
	t.Setenv("TG_LDAP_BIND_PW", "s3cr3t-bind-pw-value")
	s, err := New(Config{
		ID:              "freeipa",
		URL:             "ldaps://dc1freeipa01.sec.example.net:636",
		BindDNRef:       "env:TG_LDAP_BIND_DN",
		BindPasswordRef: "env:TG_LDAP_BIND_PW",
		Dial:            func(context.Context, string) (Conn, error) { return fc, nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func ipaFixture() *fakeConn {
	return &fakeConn{
		byBase: map[string]*ldapv3.SearchResult{
			defaultUserBase: {Entries: []*ldapv3.Entry{
				entry("uid=alice,cn=users,cn=accounts,dc=sec,dc=example,dc=net", map[string][]string{
					"uid":      {"alice"},
					"memberOf": {"cn=sre-oncall,cn=groups,cn=accounts,dc=sec,dc=example,dc=net", "cn=admins,cn=groups,cn=accounts,dc=sec,dc=example,dc=net"},
				}),
				entry("uid=bob,cn=users,cn=accounts,dc=sec,dc=example,dc=net", map[string][]string{
					"uid":      {"bob"},
					"memberOf": {"cn=sre-oncall,cn=groups,cn=accounts,dc=sec,dc=example,dc=net"},
				}),
			}},
			defaultGroupBase: {Entries: []*ldapv3.Entry{
				entry("cn=sre-oncall,cn=groups,cn=accounts,dc=sec,dc=example,dc=net", map[string][]string{
					"cn": {"sre-oncall"},
				}),
			}},
		},
	}
}

// ---- pure-mapping oracles --------------------------------------------------------------------------

func TestEntryToApprover_User(t *testing.T) {
	t.Parallel()
	e := entry("uid=alice,cn=users,cn=accounts,dc=sec,dc=example,dc=net", map[string][]string{
		"uid":      {"alice"},
		"memberOf": {"cn=sre-oncall,cn=groups,cn=accounts,dc=sec,dc=example,dc=net", "cn=admins,cn=groups,cn=accounts,dc=sec,dc=example,dc=net"},
	})
	se, err := entryToApprover(e, "uid", "memberOf", credential.PrincipalUser)
	if err != nil {
		t.Fatalf("entryToApprover: %v", err)
	}
	if se.Bundle.Valid() {
		t.Fatal("human-plane entry carries a Bundle — the router would reject it")
	}
	a := se.Approver
	if !a.Valid() || a.Kind() != credential.PrincipalUser || a.Name() != "alice" {
		t.Fatalf("bad approver: valid=%v kind=%v name=%q", a.Valid(), a.Kind(), a.Name())
	}
	// memberOf DNs reduced to their cn, normalized+sorted by NewApproverIdentity.
	if got := strings.Join(a.Groups(), ","); got != "admins,sre-oncall" {
		t.Fatalf("groups = %q, want admins,sre-oncall", got)
	}
	if se.NativeID != e.DN {
		t.Fatalf("NativeID = %q, want the entry DN", se.NativeID)
	}
}

func TestEntryToApprover_Group(t *testing.T) {
	t.Parallel()
	e := entry("cn=sre-oncall,cn=groups,cn=accounts,dc=sec,dc=example,dc=net", map[string][]string{
		"cn":       {"sre-oncall"},
		"memberOf": {"cn=all-oncall,cn=groups,cn=accounts,dc=sec,dc=example,dc=net"}, // nested parent group
	})
	se, err := entryToApprover(e, "cn", "memberOf", credential.PrincipalGroup)
	if err != nil {
		t.Fatalf("entryToApprover: %v", err)
	}
	if se.Bundle.Valid() {
		t.Fatal("group entry carries a Bundle")
	}
	a := se.Approver
	if a.Kind() != credential.PrincipalGroup || a.Name() != "sre-oncall" {
		t.Fatalf("bad group approver: kind=%v name=%q", a.Kind(), a.Name())
	}
	if got := strings.Join(a.Groups(), ","); got != "all-oncall" {
		t.Fatalf("nested groups = %q, want all-oncall", got)
	}
}

func TestEntryToApprover_NoNameFailsClosed(t *testing.T) {
	t.Parallel()
	e := entry("uid=,cn=users,dc=x", map[string][]string{"memberOf": {"cn=g,dc=x"}})
	if _, err := entryToApprover(e, "uid", "memberOf", credential.PrincipalUser); err == nil {
		t.Fatal("an entry with no uid must fail closed, not inject a blank identity")
	}
}

func TestGroupCNs_DNAndBareName(t *testing.T) {
	t.Parallel()
	got := groupCNs([]string{
		"cn=sre-oncall,cn=groups,cn=accounts,dc=sec,dc=example,dc=net",
		"plain-group-name", // a directory that returns a bare name
		"  ",               // dropped
	})
	if strings.Join(got, "|") != "sre-oncall|plain-group-name" {
		t.Fatalf("groupCNs = %v", got)
	}
}

// ---- Sync oracles (real Source + fake seam) --------------------------------------------------------

func TestSync_UsersAndGroupsToApprovers(t *testing.T) {
	fc := ipaFixture()
	s := freeIPAFake(t, fc)

	entries, err := s.Sync(context.Background())
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(entries) != 3 { // alice, bob, sre-oncall
		t.Fatalf("got %d entries, want 3", len(entries))
	}
	var users, groups int
	for _, e := range entries {
		if e.Bundle.Valid() {
			t.Fatalf("entry %q carries a host Bundle — human plane must not", e.NativeID)
		}
		if !e.Approver.Valid() {
			t.Fatalf("entry %q carries no valid approver", e.NativeID)
		}
		switch e.Approver.Kind() {
		case credential.PrincipalUser:
			users++
		case credential.PrincipalGroup:
			groups++
		}
	}
	if users != 2 || groups != 1 {
		t.Fatalf("users=%d groups=%d, want 2/1", users, groups)
	}

	// The synced entries feed the human-plane resolver end-to-end (REQ-1614 → spec/015 approve_by).
	se := credential.NewSyncEngine(nil)
	if err := se.RegisterSource(s, 0); err != nil {
		t.Fatalf("RegisterSource: %v", err)
	}
	if _, err := se.Sync(context.Background(), "freeipa"); err != nil {
		t.Fatalf("engine Sync: %v", err)
	}
	set, err := se.ResolveApprovers(credential.ApproverQuery{Group: "sre-oncall"})
	if err != nil {
		t.Fatalf("ResolveApprovers: %v", err)
	}
	if strings.Join(set.Users, ",") != "alice,bob" {
		t.Fatalf("approver users = %v, want alice,bob", set.Users)
	}
	if len(set.Groups) != 1 || set.Groups[0] != "sre-oncall" {
		t.Fatalf("approver groups = %v, want [sre-oncall]", set.Groups)
	}
	// And a human-plane source NEVER resolves as a host bundle (cross-plane isolation).
	if _, err := se.Resolve(credential.Target{Host: "alice"}); !credential.IsRefused(err) {
		t.Fatalf("human-plane source resolved a host bundle: %v", err)
	}
}

func TestSync_RouterRejectsAnyBundleLeak(t *testing.T) {
	// Every entry the connector emits must be Approver-only; assert the router accepts the whole set (it
	// hard-fails a human entry carrying a Bundle). This proves the connector never sets Bundle.
	fc := ipaFixture()
	s := freeIPAFake(t, fc)
	se := credential.NewSyncEngine(nil)
	if err := se.RegisterSource(s, 0); err != nil {
		t.Fatalf("RegisterSource: %v", err)
	}
	if _, err := se.Sync(context.Background(), "freeipa"); err != nil {
		t.Fatalf("router rejected the connector's entries (a Bundle leaked?): %v", err)
	}
}

func TestSync_FailClosedMatrix(t *testing.T) {
	dialErr := errors.New("dial tcp: connection refused") // unreachable / TLS-verify failure surface here
	cases := []struct {
		name string
		fc   *fakeConn
		dial DialFunc
	}{
		{"bind-fail", &fakeConn{bindErr: ldapv3.NewError(ldapv3.LDAPResultInvalidCredentials, errors.New("invalid credentials"))}, nil},
		{"search-fail", &fakeConn{searchErr: ldapv3.NewError(ldapv3.LDAPResultSizeLimitExceeded, errors.New("size limit exceeded"))}, nil},
		{"empty", &fakeConn{byBase: map[string]*ldapv3.SearchResult{}}, nil},
		{"unreachable", nil, func(context.Context, string) (Conn, error) { return nil, dialErr }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TG_LDAP_BIND_DN", "uid=svc-tg,dc=sec")
			t.Setenv("TG_LDAP_BIND_PW", "pw")
			dial := tc.dial
			if dial == nil {
				fc := tc.fc
				dial = func(context.Context, string) (Conn, error) { return fc, nil }
			}
			s, err := New(Config{ID: "f", URL: "ldaps://h:636", BindDNRef: "env:TG_LDAP_BIND_DN", BindPasswordRef: "env:TG_LDAP_BIND_PW", Dial: dial})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if _, err := s.Sync(context.Background()); err == nil {
				t.Fatalf("%s: Sync must fail closed", tc.name)
			}
		})
	}
}

func TestSync_ReplicaFailover(t *testing.T) {
	// Two equivalent replicas (NL first, GR second). The FIRST is unreachable; the connector must
	// transparently fail over to the SECOND and still return the full result. Deterministic, fixed order.
	const nl, gr = "ldaps://nl-freeipa:636", "ldaps://gr-freeipa:636"
	t.Setenv("TG_LDAP_BIND_DN", "uid=svc,dc=x")
	t.Setenv("TG_LDAP_BIND_PW", "pw")

	var dialed []string
	good := ipaFixture()
	dial := func(_ context.Context, url string) (Conn, error) {
		dialed = append(dialed, url)
		if url == nl {
			return nil, errors.New("dial tcp: connection refused") // first replica down
		}
		return good, nil // second replica up
	}
	s, err := New(Config{ID: "f", URLs: []string{nl, gr}, BindDNRef: "env:TG_LDAP_BIND_DN", BindPasswordRef: "env:TG_LDAP_BIND_PW", Dial: dial})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	entries, err := s.Sync(context.Background())
	if err != nil {
		t.Fatalf("failover Sync should succeed via the second replica, got %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("got %d entries via failover, want 3", len(entries))
	}
	if len(dialed) != 2 || dialed[0] != nl || dialed[1] != gr {
		t.Fatalf("failover order = %v, want [%s %s] (fixed, deterministic)", dialed, nl, gr)
	}

	// ALL replicas down → fail closed after exhausting the ordered set.
	allDown := func(context.Context, string) (Conn, error) { return nil, errors.New("dial tcp: connection refused") }
	s2, err := New(Config{ID: "f", URLs: []string{nl, gr}, BindDNRef: "env:TG_LDAP_BIND_DN", BindPasswordRef: "env:TG_LDAP_BIND_PW", Dial: allDown})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := s2.Sync(context.Background()); err == nil || !strings.Contains(err.Error(), "all 2 replica") {
		t.Fatalf("all replicas down must fail closed, got %v", err)
	}
}

// STONITH: with no URLs configured, New fails closed — there is NO compiled-in estate replica default. The
// replica list is deploy config (TG_LDAP_URLS); an unconfigured source is a configuration error, never a
// baked-in estate fallback.
func TestNew_FailsClosedWithoutURLs(t *testing.T) {
	t.Parallel()
	if _, err := New(Config{ID: "f", BindDNRef: "env:X", BindPasswordRef: "env:Y"}); err == nil {
		t.Fatal("New with no URLs must fail closed (no compiled estate default), got nil error")
	}
}

// The site base DNs come from Config (deploy-time), and the generic default carries only the standard RDN
// path — no estate dc=… suffix compiled in.
func TestNew_UsesConfiguredBaseDNsNotEstateDefault(t *testing.T) {
	t.Parallel()
	s, err := New(Config{ID: "f", URLs: []string{"ldaps://dir.example:636"}, BindDNRef: "env:X", BindPasswordRef: "env:Y",
		UserBaseDN: "cn=users,cn=accounts,dc=example,dc=test", GroupBaseDN: "cn=groups,cn=accounts,dc=example,dc=test"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if s.userBase != "cn=users,cn=accounts,dc=example,dc=test" || s.groupBase != "cn=groups,cn=accounts,dc=example,dc=test" {
		t.Fatalf("base DNs must come from Config, got user=%q group=%q", s.userBase, s.groupBase)
	}
	// The generic default (empty Config base) carries no estate suffix.
	g, _ := New(Config{ID: "g", URLs: []string{"ldaps://dir.example:636"}, BindDNRef: "env:X", BindPasswordRef: "env:Y"})
	if strings.Contains(g.userBase, "example") {
		t.Fatalf("generic default base must not carry an estate suffix, got %q", g.userBase)
	}
}

func TestSync_OversizedFailsClosed(t *testing.T) {
	// A directory just over the cap must fail closed (bounded-result guard) rather than OOM.
	big := &ldapv3.SearchResult{}
	for i := 0; i < 11; i++ {
		big.Entries = append(big.Entries, entry(fmt.Sprintf("uid=u%d,dc=x", i), map[string][]string{"uid": {fmt.Sprintf("u%d", i)}}))
	}
	fc := &fakeConn{byBase: map[string]*ldapv3.SearchResult{defaultUserBase: big}}
	t.Setenv("TG_LDAP_BIND_DN", "uid=svc,dc=x")
	t.Setenv("TG_LDAP_BIND_PW", "pw")
	s, err := New(Config{ID: "f", URL: "ldaps://h:636", BindDNRef: "env:TG_LDAP_BIND_DN", BindPasswordRef: "env:TG_LDAP_BIND_PW", MaxTotal: 10, Dial: func(context.Context, string) (Conn, error) { return fc, nil }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := s.Sync(context.Background()); err == nil || !strings.Contains(err.Error(), "cap") {
		t.Fatalf("oversized result must fail closed on the cap, got %v", err)
	}
}

func TestSync_PasswordNeverLogged(t *testing.T) {
	const pw = "UNIQUE-BIND-PASSWORD-do-not-leak-9f3a"
	t.Setenv("TG_LDAP_BIND_DN", "uid=svc,dc=x")
	t.Setenv("TG_LDAP_BIND_PW", pw)
	// Force a bind failure so the error path is exercised, then assert the password is absent from it.
	fc := &fakeConn{bindErr: ldapv3.NewError(ldapv3.LDAPResultInvalidCredentials, errors.New("invalid credentials"))}
	s, err := New(Config{ID: "f", URL: "ldaps://h:636", BindDNRef: "env:TG_LDAP_BIND_DN", BindPasswordRef: "env:TG_LDAP_BIND_PW", Dial: func(context.Context, string) (Conn, error) { return fc, nil }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = s.Sync(context.Background())
	if err == nil {
		t.Fatal("expected a bind failure")
	}
	if strings.Contains(err.Error(), pw) {
		t.Fatalf("bind password leaked into the error: %v", err)
	}
	// The fake DID receive the correct password (proving it was passed to Bind, not that it is safe to log).
	if fc.sawBindPw != pw {
		t.Fatalf("connector did not pass the resolved password to Bind")
	}
}

func TestSync_MissingBindRefFailsClosed(t *testing.T) {
	// A bind DN/password ref pointing at an unset env var must fail closed (never bind anonymously).
	fc := ipaFixture()
	s, err := New(Config{ID: "f", URL: "ldaps://h:636", BindDNRef: "env:TG_LDAP_ABSENT_DN", BindPasswordRef: "env:TG_LDAP_ABSENT_PW", Dial: func(context.Context, string) (Conn, error) { return fc, nil }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := s.Sync(context.Background()); err == nil {
		t.Fatal("unresolvable bind ref must fail closed")
	}
	if fc.bindCalled {
		t.Fatal("connector attempted a bind despite an unresolvable credential ref")
	}
}

func TestNew_Validation(t *testing.T) {
	t.Parallel()
	bad := []Config{
		{URL: "ldaps://h", BindDNRef: "env:X", BindPasswordRef: "env:Y"},                   // no id
		{ID: "f", URL: "https://h", BindDNRef: "env:X", BindPasswordRef: "env:Y"},          // wrong scheme
		{ID: "f", URLs: []string{"ftp://h"}, BindDNRef: "env:X", BindPasswordRef: "env:Y"}, // wrong scheme in list
		{ID: "f", URL: "ldaps://h", BindPasswordRef: "env:Y"},                              // no bind DN ref
		{ID: "f", URL: "ldaps://h", BindDNRef: "env:X"},                                    // no bind pw ref
	}
	for i, c := range bad {
		if _, err := New(c); err == nil {
			t.Fatalf("case %d: New should reject %+v", i, c)
		}
	}
}

func TestPlaneIsHuman(t *testing.T) {
	t.Parallel()
	s, err := New(Config{ID: "f", URL: "ldaps://h:636", BindDNRef: "env:X", BindPasswordRef: "env:Y"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if s.Plane() != credential.PlaneHuman {
		t.Fatalf("Plane = %v, want human", s.Plane())
	}
}

// keep config imported (SecretRef literals above are typed via the Config fields).
var _ = config.SecretRef("")

// ---- BEST-EFFORT live connectivity check -----------------------------------------------------------

// TestLiveRootDSE hits the real FreeIPA only when TG_LDAP_LIVE_ADDR is set (e.g.
// "ldaps://dc1freeipa01.sec.example.net:636" or "ldap://…:389"). It does an ANONYMOUS root-DSE
// read (base scope, base "", namingContexts) — no bind credentials needed — as the safe connectivity
// assertion. The authenticated user/group search is NOT exercised here (TG has no FreeIPA bind creds); set
// TG_LDAP_CACERT to a PEM path for LDAPS verification.
func TestLiveRootDSE(t *testing.T) {
	addr := os.Getenv("TG_LDAP_LIVE_ADDR")
	if addr == "" {
		t.Skip("set TG_LDAP_LIVE_ADDR to run the live FreeIPA anonymous root-DSE connectivity check")
	}
	s, err := New(Config{
		ID: "live", URL: addr, StartTLS: os.Getenv("TG_LDAP_STARTTLS") != "",
		CACertRef: config.SecretRef(caRefFromEnv()),
		// bind refs are required by New but not used by the anonymous probe below.
		BindDNRef: "env:TG_LDAP_LIVE_ADDR", BindPasswordRef: "env:TG_LDAP_LIVE_ADDR",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := s.realDial(ctx, addr)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	defer c.Close()
	// Anonymous root-DSE read: no bind, base scope over the empty base DN.
	req := ldapv3.NewSearchRequest("", ldapv3.ScopeBaseObject, ldapv3.NeverDerefAliases, 0, 10, false,
		"(objectClass=*)", []string{"namingContexts"}, nil)
	res, err := c.Search(req)
	if err != nil {
		t.Fatalf("anonymous root-DSE search: %v", err)
	}
	if len(res.Entries) == 0 {
		t.Fatal("root-DSE returned no entry")
	}
	t.Logf("live FreeIPA reachable: namingContexts=%v", res.Entries[0].GetAttributeValues("namingContexts"))
}

func caRefFromEnv() string {
	if p := os.Getenv("TG_LDAP_CACERT"); p != "" {
		return "file:" + p
	}
	return ""
}
