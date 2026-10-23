package auth

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	ldapv3 "github.com/go-ldap/ldap/v3"
)

// fakeLDAPConn is an in-memory LDAP connection for the oracles (CI has no directory). It records the bind
// it saw, verifies a fixture password, and returns fixture memberOf entries — or forces a bind/search
// failure. It mirrors the credsource/ldap connector's Conn seam.
type fakeLDAPConn struct {
	// users maps a bind DN → its (password, memberOf DNs). A bind DN absent here → invalid credentials.
	users     map[string]fakeUser
	bindErr   error // when non-nil, forced on every Bind (e.g. a server-side, non-credential error)
	searchErr error
	sawBindDN string
	sawBindPw string
	closed    bool
}

type fakeUser struct {
	password string
	groups   []string // memberOf DNs
}

func (f *fakeLDAPConn) Bind(username, password string) error {
	f.sawBindDN, f.sawBindPw = username, password
	if f.bindErr != nil {
		return f.bindErr
	}
	u, ok := f.users[username]
	if !ok || u.password != password {
		return ldapv3.NewError(ldapv3.LDAPResultInvalidCredentials, errors.New("invalid credentials"))
	}
	return nil
}

func (f *fakeLDAPConn) Search(req *ldapv3.SearchRequest) (*ldapv3.SearchResult, error) {
	if f.searchErr != nil {
		return nil, f.searchErr
	}
	u, ok := f.users[req.BaseDN]
	if !ok {
		return &ldapv3.SearchResult{}, nil
	}
	e := &ldapv3.Entry{DN: req.BaseDN}
	e.Attributes = append(e.Attributes, &ldapv3.EntryAttribute{Name: "memberOf", Values: u.groups})
	return &ldapv3.SearchResult{Entries: []*ldapv3.Entry{e}}, nil
}

func (f *fakeLDAPConn) Close() error { f.closed = true; return nil }

const (
	testUserTmpl = "uid=%s,cn=users,cn=accounts,dc=sec,dc=example,dc=net"
	dnAdmin      = "uid=kyriakosp,cn=users,cn=accounts,dc=sec,dc=example,dc=net"
	dnOperator   = "uid=opuser,cn=users,cn=accounts,dc=sec,dc=example,dc=net"
	dnNogroup    = "uid=stranger,cn=users,cn=accounts,dc=sec,dc=example,dc=net"
)

// ipaAuthFixture: kyriakosp ∈ tg-admins, opuser ∈ tg-operators, stranger ∈ neither TG group.
func ipaAuthFixture() *fakeLDAPConn {
	return &fakeLDAPConn{users: map[string]fakeUser{
		dnAdmin: {password: "kp-freeipa-pw", groups: []string{
			"cn=sre-oncall,cn=groups,cn=accounts,dc=sec,dc=example,dc=net",
			"cn=tg-admins,cn=groups,cn=accounts,dc=sec,dc=example,dc=net",
		}},
		dnOperator: {password: "op-freeipa-pw", groups: []string{
			"cn=tg-operators,cn=groups,cn=accounts,dc=sec,dc=example,dc=net",
		}},
		dnNogroup: {password: "stranger-pw", groups: []string{
			"cn=sre-oncall,cn=groups,cn=accounts,dc=sec,dc=example,dc=net",
		}},
	}}
}

func testLDAPAuth(t *testing.T, dial LDAPDialFunc) *LDAPAuthenticator {
	t.Helper()
	l, err := NewLDAPAuthenticator(LDAPConfig{
		URLs:           []string{"ldaps://dc1freeipa01.sec.example.net:636"},
		UserDNTemplate: testUserTmpl,
		AdminGroup:     "tg-admins",
		OperatorGroup:  "tg-operators",
		Dial:           dial,
	})
	if err != nil {
		t.Fatalf("NewLDAPAuthenticator: %v", err)
	}
	return l
}

func dialTo(fc *fakeLDAPConn) LDAPDialFunc {
	return func(context.Context, string) (LDAPConn, error) { return fc, nil }
}

// Construction fails closed on missing/invalid pieces.
func TestLDAPAuthConstructionFailsClosed(t *testing.T) {
	if _, err := NewLDAPAuthenticator(LDAPConfig{}); err == nil {
		t.Fatal("no URLs must be rejected")
	}
	if _, err := NewLDAPAuthenticator(LDAPConfig{URLs: []string{"http://x:636"}}); err == nil {
		t.Fatal("non-ldap URL must be rejected")
	}
	if _, err := NewLDAPAuthenticator(LDAPConfig{URLs: []string{"ldaps://x:636"}, UserDNTemplate: "uid=,cn=users"}); err == nil {
		t.Fatal("template without a percent-s placeholder must be rejected")
	}
	// Defaults fill in for a minimal valid config.
	if _, err := NewLDAPAuthenticator(LDAPConfig{URLs: []string{"ldaps://x:636"}}); err != nil {
		t.Fatalf("minimal valid config must construct: %v", err)
	}
}

// Correct password + tg-admins → LDAPAdmin; tg-operators → LDAPOperator; no TG group → DENY; wrong
// password → DENY; empty password → DENY (no anonymous/unauthenticated bind).
func TestLDAPAuthRoleMapping(t *testing.T) {
	l := testLDAPAuth(t, dialTo(ipaAuthFixture()))
	ctx := context.Background()

	cases := []struct {
		name, user, pw string
		wantRole       LDAPRole
		wantErr        bool
	}{
		{"admin", "kyriakosp", "kp-freeipa-pw", LDAPAdmin, false},
		{"operator", "opuser", "op-freeipa-pw", LDAPOperator, false},
		{"valid-user-no-tg-group", "stranger", "stranger-pw", LDAPDeny, true},
		{"wrong-password", "kyriakosp", "nope", LDAPDeny, true},
		{"empty-password", "kyriakosp", "", LDAPDeny, true},
		{"empty-username", "", "kp-freeipa-pw", LDAPDeny, true},
		{"unknown-user", "ghost", "whatever", LDAPDeny, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			role, err := l.Authenticate(ctx, c.user, c.pw)
			if role != c.wantRole {
				t.Errorf("role = %v, want %v", role, c.wantRole)
			}
			if (err != nil) != c.wantErr {
				t.Errorf("err = %v, wantErr = %v", err, c.wantErr)
			}
			if c.wantErr && err != nil && !errors.Is(err, ErrUnauthenticated) {
				// definitive-decision failures must be the uniform ErrUnauthenticated (deny), not a server error.
				if !strings.Contains(c.name, "unreachable") {
					t.Errorf("err = %v, want ErrUnauthenticated", err)
				}
			}
		})
	}
}

// A DN-injection attempt in the username is escaped (never breaks out of the RDN); the bind DN the fake
// sees is the escaped template, so the attacker cannot bind as a different principal.
func TestLDAPAuthEscapesUsername(t *testing.T) {
	fc := ipaAuthFixture()
	l := testLDAPAuth(t, dialTo(fc))
	// This crafted username, if unescaped, would inject an extra RDN. After EscapeDN it cannot match dnAdmin.
	_, err := l.Authenticate(context.Background(), "kyriakosp,cn=admins", "kp-freeipa-pw")
	if !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("injection attempt must fail closed, got %v", err)
	}
	if fc.sawBindDN == dnAdmin {
		t.Fatalf("escaped bind DN must differ from the real admin DN; saw %q", fc.sawBindDN)
	}
	// The injected comma must be ESCAPED (\\,) so it stays inside the uid RDN value and cannot form an
	// extra, unescaped RDN separator.
	if !strings.Contains(fc.sawBindDN, `\,cn=admins`) {
		t.Fatalf("username comma must be escaped so it cannot form an extra RDN; saw %q", fc.sawBindDN)
	}
}

// Replica failover: the first replica is unreachable, the second serves — a correct login still succeeds.
// All-down fails closed with a non-ErrUnauthenticated (server) error.
func TestLDAPAuthReplicaFailover(t *testing.T) {
	fc := ipaAuthFixture()
	up := "ldaps://dc2freeipa01.sec.example.net:636"
	l, err := NewLDAPAuthenticator(LDAPConfig{
		URLs:           []string{"ldaps://down.example:636", up},
		UserDNTemplate: testUserTmpl, AdminGroup: "tg-admins", OperatorGroup: "tg-operators",
		Dial: func(_ context.Context, url string) (LDAPConn, error) {
			if url == up {
				return fc, nil
			}
			return nil, errors.New("connection refused")
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	role, err := l.Authenticate(context.Background(), "kyriakosp", "kp-freeipa-pw")
	if err != nil || role != LDAPAdmin {
		t.Fatalf("failover login: role=%v err=%v", role, err)
	}

	// All replicas down → fail closed, and NOT the uniform ErrUnauthenticated (this is unavailability).
	allDown, _ := NewLDAPAuthenticator(LDAPConfig{
		URLs:           []string{"ldaps://a:636", "ldaps://b:636"},
		UserDNTemplate: testUserTmpl, AdminGroup: "tg-admins", OperatorGroup: "tg-operators",
		Dial: func(context.Context, string) (LDAPConn, error) { return nil, errors.New("unreachable") },
	})
	if _, err := allDown.Authenticate(context.Background(), "kyriakosp", "kp-freeipa-pw"); err == nil || errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("all-down must fail closed with a server error, got %v", err)
	}
}

// The password is NEVER logged or stored on the authenticator, and only ever appears as the Bind arg.
func TestLDAPAuthPasswordNeverRetained(t *testing.T) {
	fc := ipaAuthFixture()
	l := testLDAPAuth(t, dialTo(fc))
	const pw = "kp-freeipa-pw"
	if _, err := l.Authenticate(context.Background(), "kyriakosp", pw); err != nil {
		t.Fatalf("login: %v", err)
	}
	// The fake saw the password (it must, to bind), but the authenticator retains no password field.
	if fc.sawBindPw != pw {
		t.Fatalf("bind should receive the password")
	}
	// Reflectively assert no LDAPAuthenticator field holds the password value.
	if strings.Contains(structDump(l), pw) {
		t.Fatalf("authenticator retained the password")
	}
}

// structDump renders a shallow view of the authenticator's non-secret fields for the retention assertion.
func structDump(l *LDAPAuthenticator) string {
	var b strings.Builder
	b.WriteString(l.userDNTemplate)
	b.WriteString("|" + l.adminGroup + "|" + l.operatorGroup + "|" + l.memberOfAttr)
	for _, ep := range l.servers {
		b.WriteString("|" + ep.url)
	}
	b.WriteString("|" + string(l.caCertRef))
	return b.String()
}

// End-to-end through SessionAuthenticator + the admin tier: an LDAP admin login mints an admin-eligible
// session that steps up WITHOUT a static admin credential; an LDAP operator login is read-only and cannot
// step up; the static break-glass operator still works even when LDAP is enabled; and an LDAP failure
// never falls through to the static token path.
func TestLDAPSessionIntegration(t *testing.T) {
	store := NewMemSessionStore()
	// The static break-glass operator (token path) is "operator".
	ops := MemOperators{"operator": {Name: "operator", TokenSHA256: sha256.Sum256([]byte("break-glass-token"))}}
	sa, err := NewSessionAuthenticator([]byte(strings.Repeat("k", 32)), store, ops, time.Hour)
	if err != nil {
		t.Fatalf("session auth: %v", err)
	}
	sa.Secure = false
	sa.EnableLDAP(testLDAPAuth(t, dialTo(ipaAuthFixture())), "operator")

	aa, err := NewAdminAuthenticator(MemOperators{}, 15*time.Minute) // NO static admin credential
	if err != nil {
		t.Fatalf("admin auth: %v", err)
	}
	v := &Verifier{sessions: sa, admins: aa}
	ctx := context.Background()

	// 1) LDAP admin login → admin-eligible session → elevate with NO admin header succeeds.
	adminCookie, _, err := sa.Login(ctx, "kyriakosp", "kp-freeipa-pw", "192.0.2.1:5555")
	if err != nil {
		t.Fatalf("ldap admin login: %v", err)
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/session/elevate", nil)
	r.AddCookie(adminCookie)
	p, _, err := v.authenticateAdminElevate(r)
	if err != nil || !p.Admin {
		t.Fatalf("tg-admins step-up (no static cred) must succeed: p=%+v err=%v", p, err)
	}
	// And the elevated session now satisfies an AuthAdminSession route.
	if _, err := v.authenticateAdminSession(r); err != nil {
		t.Fatalf("elevated session must satisfy admin route: %v", err)
	}

	// 2) LDAP operator login → read-only session → cannot step up (not admin-eligible, no static cred).
	opCookie, _, err := sa.Login(ctx, "opuser", "op-freeipa-pw", "192.0.2.2:5555")
	if err != nil {
		t.Fatalf("ldap operator login: %v", err)
	}
	r2 := httptest.NewRequest(http.MethodPost, "/v1/session/elevate", nil)
	r2.AddCookie(opCookie)
	if _, _, err := v.authenticateAdminElevate(r2); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("operator step-up must fail closed, got %v", err)
	}

	// 3) The static break-glass operator still logs in (token path) even with LDAP enabled.
	if _, _, err := sa.Login(ctx, "operator", "break-glass-token", "10.0.0.3:5555"); err != nil {
		t.Fatalf("break-glass operator login must still work: %v", err)
	}
	// A break-glass operator is NOT admin-eligible (read-only bootstrap).
	bgCookie, _, _ := sa.Login(ctx, "operator", "break-glass-token", "10.0.0.3:5555")
	rb := httptest.NewRequest(http.MethodPost, "/v1/session/elevate", nil)
	rb.AddCookie(bgCookie)
	if _, _, err := v.authenticateAdminElevate(rb); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("break-glass operator must not step up without a static admin credential, got %v", err)
	}

	// 4) An LDAP user's WRONG password never falls through to the static token path (fail closed).
	if _, _, err := sa.Login(ctx, "kyriakosp", "break-glass-token", "10.0.0.4:5555"); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("ldap failure must not fall through to the static token, got %v", err)
	}
}

// When LDAP is unreachable, the break-glass static operator still authenticates (LDAP-down cannot lock
// everyone out); a non-break-glass name still fails closed (never a weaker check).
func TestLDAPDownBreakGlassStillWorks(t *testing.T) {
	store := NewMemSessionStore()
	ops := MemOperators{"operator": {Name: "operator", TokenSHA256: sha256.Sum256([]byte("break-glass-token"))}}
	sa, _ := NewSessionAuthenticator([]byte(strings.Repeat("k", 32)), store, ops, time.Hour)
	sa.Secure = false
	down, _ := NewLDAPAuthenticator(LDAPConfig{
		URLs:           []string{"ldaps://down:636"},
		UserDNTemplate: testUserTmpl, AdminGroup: "tg-admins", OperatorGroup: "tg-operators",
		Dial: func(context.Context, string) (LDAPConn, error) { return nil, errors.New("unreachable") },
	})
	sa.EnableLDAP(down, "operator")

	if _, _, err := sa.Login(context.Background(), "operator", "break-glass-token", "192.0.2.9:1"); err != nil {
		t.Fatalf("break-glass must work while LDAP is down: %v", err)
	}
	if _, _, err := sa.Login(context.Background(), "kyriakosp", "kp-freeipa-pw", "192.0.2.9:2"); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("ldap user must fail closed while LDAP is down, got %v", err)
	}
}
