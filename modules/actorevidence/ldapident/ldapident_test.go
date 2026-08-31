package ldapident

import (
	"context"
	"errors"
	"strings"
	"testing"

	ldapv3 "github.com/go-ldap/ldap/v3"

	"github.com/territory-grounder/grounder/core/config"
)

// fakeConn returns fixture entries for a Search (or an error at any stage), with no live directory. Entries
// use the REAL FreeIPA attribute shape captured from dc1freeipa01: nsAccountLock ("TRUE"/"FALSE"),
// memberOf DN-valued, objectClass person/ipaservice — so a fixture that diverged from the wire would surface.
type fakeConn struct {
	entries   []*ldapv3.Entry
	bindErr   error
	searchErr error
	bound     bool
}

func (f *fakeConn) Bind(_, _ string) error {
	if f.bindErr != nil {
		return f.bindErr
	}
	f.bound = true
	return nil
}

func (f *fakeConn) Search(_ *ldapv3.SearchRequest) (*ldapv3.SearchResult, error) {
	if f.searchErr != nil {
		return nil, f.searchErr
	}
	if !f.bound {
		return nil, errors.New("search before bind")
	}
	return &ldapv3.SearchResult{Entries: f.entries}, nil
}

func (f *fakeConn) Close() error { return nil }

func entry(uid, lock string, memberOf, objectClass []string) *ldapv3.Entry {
	attrs := map[string][]string{
		attrUID:         {uid},
		attrMemberOf:    memberOf,
		attrObjectClass: objectClass,
	}
	if lock != "" {
		attrs[attrAccountLock] = []string{lock}
	}
	return ldapv3.NewEntry("uid="+uid+",cn=users,cn=accounts,dc=sec,dc=example,dc=net", attrs)
}

const adminDN = "cn=admins,cn=groups,cn=accounts,dc=sec,dc=example,dc=net"

func newResolver(t *testing.T, entries []*ldapv3.Entry) *Resolver {
	t.Helper()
	t.Setenv("LDAP_BIND_DN", "uid=svc,cn=users,cn=accounts,dc=sec,dc=example,dc=net")
	t.Setenv("LDAP_BIND_PW", "secret")
	r, err := New(Config{
		URLs:            []string{"ldaps://freeipa.test:636"},
		BindDNRef:       config.SecretRef("env:LDAP_BIND_DN"),
		BindPasswordRef: config.SecretRef("env:LDAP_BIND_PW"),
		Dial: func(_ context.Context, _ string) (Conn, error) {
			return &fakeConn{entries: entries}, nil
		},
	})
	if err != nil {
		t.Fatalf("new resolver: %v", err)
	}
	return r
}

// An enabled member of the sanctioned admin group is CONFIRMED (promotion); a locked account is DISABLED
// (demotion); an enabled non-admin is neither; a service account is never confirmed. The @REALM actor form
// maps to the uid and the verdict comes back on the ORIGINAL actor string.
func TestResolveConfirmedAndDisabled(t *testing.T) {
	entries := []*ldapv3.Entry{
		entry("kp", "FALSE", []string{adminDN, "cn=ipausers,cn=groups,cn=accounts,dc=sec,dc=example,dc=net"}, []string{"inetorgperson", "person"}),
		entry("mallory", "TRUE", []string{adminDN}, []string{"inetorgperson", "person"}),                                          // locked admin credential in use
		entry("bob", "FALSE", []string{"cn=ipausers,cn=groups,cn=accounts,dc=sec,dc=example,dc=net"}, []string{"person"}), // enabled non-admin
		entry("svc", "FALSE", []string{adminDN}, []string{"ipaservice"}),                                                          // service account, never promoted
	}
	r := newResolver(t, entries)
	facts, err := r.Resolve(context.Background(), "journal",
		[]string{"kp@SEC.NUCLEARLIGHTERS.NET", "mallory", "bob", "svc"}, []string{"admins"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(facts.Confirmed) != 1 || facts.Confirmed[0] != "kp@SEC.NUCLEARLIGHTERS.NET" {
		t.Fatalf("confirmed must be the ORIGINAL @REALM actor form for the enabled admin, got %+v", facts.Confirmed)
	}
	if len(facts.Disabled) != 1 || facts.Disabled[0] != "mallory" {
		t.Fatalf("disabled must be the locked admin, got %+v", facts.Disabled)
	}
}

// A disabled account that is ALSO an admin-group member must be DISABLED, never confirmed (disabled
// dominates — a locked admin credential in use is the compromise case, REQ-2318).
func TestDisabledDominatesConfirmed(t *testing.T) {
	r := newResolver(t, []*ldapv3.Entry{entry("kp", "TRUE", []string{adminDN}, []string{"person"})})
	facts, err := r.Resolve(context.Background(), "journal", []string{"kp"}, []string{"admins"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(facts.Confirmed) != 0 {
		t.Fatalf("a disabled admin must never be confirmed, got %+v", facts.Confirmed)
	}
	if len(facts.Disabled) != 1 || facts.Disabled[0] != "kp" {
		t.Fatalf("a disabled admin must be demoted, got %+v", facts.Disabled)
	}
}

// A resolver error (all replicas fail) is advisory: it returns EMPTY facts + a non-nil error, so the fold
// leaves the static list (REQ-2319). A dead resolver never confirms nor disables.
func TestResolveFailsOpenOnError(t *testing.T) {
	t.Setenv("LDAP_BIND_DN", "uid=svc")
	t.Setenv("LDAP_BIND_PW", "secret")
	r, err := New(Config{
		URLs: []string{"ldaps://a.test:636", "ldaps://b.test:636"}, BindDNRef: config.SecretRef("env:LDAP_BIND_DN"), BindPasswordRef: config.SecretRef("env:LDAP_BIND_PW"),
		Dial: func(_ context.Context, _ string) (Conn, error) { return nil, errors.New("unreachable") },
	})
	if err != nil {
		t.Fatal(err)
	}
	facts, rerr := r.Resolve(context.Background(), "journal", []string{"kp"}, []string{"admins"})
	if rerr == nil {
		t.Fatal("all-replicas-fail must return a non-nil advisory error")
	}
	if len(facts.Confirmed) != 0 || len(facts.Disabled) != 0 {
		t.Fatalf("a failed resolve must yield empty facts (fail open), got %+v", facts)
	}
}

// No admin group configured ⇒ no promotion (an enabled member is not confirmed without a sanctioned group).
func TestNoSanctionedGroupNoPromotion(t *testing.T) {
	r := newResolver(t, []*ldapv3.Entry{entry("kp", "FALSE", []string{adminDN}, []string{"person"})})
	facts, err := r.Resolve(context.Background(), "journal", []string{"kp"}, nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(facts.Confirmed) != 0 {
		t.Fatalf("no sanctioned group ⇒ no promotion, got %+v", facts.Confirmed)
	}
}

// A numeric "uid:N" fallback actor (a journal record that named no invoker) names no directory uid and is
// never queried.
func TestUIDFallbackActorIsNotQueried(t *testing.T) {
	r := newResolver(t, nil)
	facts, err := r.Resolve(context.Background(), "journal", []string{"uid:0"}, []string{"admins"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(facts.Confirmed) != 0 || len(facts.Disabled) != 0 {
		t.Fatalf("a uid:N fallback actor must produce no facts, got %+v", facts)
	}
}

func TestActorToUID(t *testing.T) {
	cases := map[string]string{
		"kp":                         "kp",
		"kp@SEC.NUCLEARLIGHTERS.NET": "kp",
		"uid:1001":                   "",
		"":                           "",
		"alice@a@b":                  "alice",
	}
	for in, want := range cases {
		if got := actorToUID(in); got != want {
			t.Fatalf("actorToUID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLeadingCN(t *testing.T) {
	if got := leadingCN(adminDN); got != "admins" {
		t.Fatalf("leadingCN = %q, want admins", got)
	}
	if got := leadingCN("not-a-dn"); got != "" {
		t.Fatalf("leadingCN of a non-DN must be empty, got %q", got)
	}
}

// REQ-2320: a resolver warning (the only identity data that leaves the seam) carries the provider and domain
// only — never the principal or its group memberships. SanctionFacts itself has no membership field.
func TestWarningsCarryNoPrincipal(t *testing.T) {
	t.Setenv("LDAP_BIND_DN", "uid=svc")
	t.Setenv("LDAP_BIND_PW", "secret")
	r, err := New(Config{
		URLs: []string{"ldaps://a.test:636"}, BindDNRef: config.SecretRef("env:LDAP_BIND_DN"), BindPasswordRef: config.SecretRef("env:LDAP_BIND_PW"),
		Dial: func(_ context.Context, _ string) (Conn, error) { return nil, errors.New("unreachable") },
	})
	if err != nil {
		t.Fatal(err)
	}
	facts, _ := r.Resolve(context.Background(), "journal", []string{"topsecret-admin"}, []string{"admins"})
	for _, w := range facts.Warnings {
		if strings.Contains(w, "topsecret-admin") || strings.Contains(w, "admins") {
			t.Fatalf("a resolver warning must not carry the principal or its group memberships, got %q", w)
		}
	}
}
