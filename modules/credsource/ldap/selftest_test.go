package ldap

// ORACLE tests for the LDAP/FreeIPA source's console TEST probe (core/selftest.Tester). CI has no directory,
// so the probe is driven through the SAME injectable seam the sync oracles use (Config.Dial → a fakeConn
// returning fixture entries), plus one case against a genuinely closed TCP port for the transport class.
// They prove: the Summary reports what the fixture CONTAINED (which replica answered, both base DNs, both
// counts); a rejected bind, a denied search and a base DN that does not exist are classified as three
// DIFFERENT faults; a closed port is an error and not a pass; replica failover is reported rather than
// hidden; and — the killing oracle — a fully-configured source whose password the directory refuses FAILS.

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	ldapv3 "github.com/go-ldap/ldap/v3"

	"github.com/territory-grounder/grounder/core/selftest"
)

// probeSource builds the REAL source over the fake seam with every configured value present and non-empty —
// the precondition the killing oracle depends on. urls is the ordered replica list; dial decides which of
// them answers.
func probeSource(t *testing.T, urls []string, dial DialFunc) *Source {
	t.Helper()
	t.Setenv("TG_LDAP_BIND_DN", "uid=svc-tg,cn=users,cn=accounts,dc=sec,dc=example,dc=net")
	t.Setenv("TG_LDAP_BIND_PW", "s3cr3t-bind-pw-value")
	s, err := New(Config{
		ID:              "freeipa",
		URLs:            urls,
		BindDNRef:       "env:TG_LDAP_BIND_DN",
		BindPasswordRef: "env:TG_LDAP_BIND_PW",
		Dial:            dial,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func dialing(fc *fakeConn) DialFunc {
	return func(context.Context, string) (Conn, error) { return fc, nil }
}

func TestSelfTestReportsWhatTheBindCanSee(t *testing.T) {
	fc := ipaFixture()
	s := probeSource(t, []string{"ldaps://ipa01.example:636"}, dialing(fc))

	res, err := s.SelfTest(context.Background(), "alice")
	if err != nil {
		t.Fatalf("SelfTest: %v", err)
	}
	// The counts come from the FIXTURE (two users, one group), and the replica and both subtrees are named
	// so a human can see which directory answered — the descriptor calls the replica list an authority field
	// for exactly that reason.
	for _, want := range []string{"ldaps://ipa01.example:636", "2 users", "1 group", defaultUserBase, defaultGroupBase} {
		if !strings.Contains(res.Summary, want) {
			t.Fatalf("summary %q does not report %q", res.Summary, want)
		}
	}
	if res.Detail != "" {
		t.Fatalf("a healthy bind must not warn: %q", res.Detail)
	}
	// Rule 5, and here it has teeth: the bind DN is itself resolved from a SecretRef and the password is the
	// module's one live credential. Neither may appear in a string an operator pastes into a ticket.
	if strings.Contains(res.Summary+res.Detail, "s3cr3t-bind-pw-value") ||
		strings.Contains(res.Summary+res.Detail, "svc-tg") {
		t.Fatalf("the probe leaked the bind credential: %q / %q", res.Summary, res.Detail)
	}
	if !fc.bindCalled {
		t.Fatal("the probe did not BIND — a probe that only connects passes with a rotated password")
	}
	if !fc.closed {
		t.Fatal("the probe leaked the connection")
	}
}

func TestSelfTestFailureClassification(t *testing.T) {
	cases := []struct {
		name       string
		conn       *fakeConn
		wantDetail []string
	}{
		{
			// The password is refused. It is the one field in this dialog that takes effect with no restart,
			// so naming it precisely is what makes the button useful right after a save.
			name:       "rejected bind names the credential",
			conn:       &fakeConn{bindErr: ldapv3.NewError(ldapv3.LDAPResultInvalidCredentials, errors.New("invalid credentials"))},
			wantDetail: []string{"REJECTED THE BIND"},
		},
		{
			// The bind worked and the SEARCH was refused: an ACI, not a password.
			name:       "denied search names the ACI",
			conn:       &fakeConn{searchErr: ldapv3.NewError(ldapv3.LDAPResultInsufficientAccessRights, errors.New("insufficient access"))},
			wantDetail: []string{"may not read that subtree"},
		},
		{
			// The commonest LDAP misconfiguration in this codebase: the defaults carry no site suffix, so
			// they match nothing on a real directory.
			name:       "missing base DN names the site suffix",
			conn:       &fakeConn{searchErr: ldapv3.NewError(ldapv3.LDAPResultNoSuchObject, errors.New("no such object"))},
			wantDetail: []string{"DOES NOT EXIST", "site suffix"},
		},
		{
			// A bind that succeeds and sees NOTHING is the failure this probe exists to surface: Sync fails
			// over on exactly this condition, so a green result here would certify an approver set that never
			// populates.
			name:       "a bind that sees nothing is a failure, not a pass",
			conn:       &fakeConn{byBase: map[string]*ldapv3.SearchResult{}},
			wantDetail: []string{"BOTH base DNs returned nothing"},
		},
		{
			// Entries came back but carry no uid: entryToApprover fails the whole sync on the first one, so
			// this must be red rather than a cheerful count.
			name: "entries without the id attribute are a failure",
			conn: &fakeConn{byBase: map[string]*ldapv3.SearchResult{
				defaultUserBase: {Entries: []*ldapv3.Entry{
					entry("cn=container,cn=users,cn=accounts,dc=example,dc=net", map[string][]string{"cn": {"container"}}),
				}},
			}},
			wantDetail: []string{"WITHOUT the configured name attribute", "sAMAccountName"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := probeSource(t, []string{"ldaps://ipa01.example:636"}, dialing(tc.conn))

			res, err := s.SelfTest(context.Background(), "alice")
			if err == nil {
				t.Fatalf("expected an error, got summary=%q detail=%q", res.Summary, res.Detail)
			}
			if res.Detail == "" {
				t.Fatal("a failed probe must carry an actionable Detail, never a bare error")
			}
			for _, want := range tc.wantDetail {
				if !strings.Contains(res.Detail, want) {
					t.Fatalf("detail %q does not carry %q", res.Detail, want)
				}
			}
			if strings.Contains(res.Summary+res.Detail+err.Error(), "s3cr3t-bind-pw-value") {
				t.Fatal("the probe leaked the bind password into its result or error")
			}
		})
	}
}

func TestSelfTestOnAClosedPortIsAnErrorNotAPass(t *testing.T) {
	// No fake seam here: this drives the REAL dialer at a port nothing is listening on, which is the
	// third fault an operator presses TEST to rule out (a host that has been down for a week).
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close() // nothing listens here any more

	s := probeSource(t, []string{"ldap://" + addr}, nil) // nil Dial → the real LDAP dialer
	res, err := s.SelfTest(context.Background(), "alice")
	if err == nil {
		t.Fatalf("a closed port must fail: %q", res.Summary)
	}
	if !strings.Contains(res.Detail, "could not be reached") {
		t.Fatalf("detail %q does not name the transport fault", res.Detail)
	}
}

func TestSelfTestReportsFailoverRatherThanHidingIt(t *testing.T) {
	// A replica that is DOWN while a later one answers is invisible in a plain pass — and it is exactly the
	// state in which the next failure takes the approver directory with it.
	fc := ipaFixture()
	dial := func(_ context.Context, url string) (Conn, error) {
		if strings.Contains(url, "ipa01") {
			return nil, errors.New("dial tcp: connection refused")
		}
		return fc, nil
	}
	s := probeSource(t, []string{"ldaps://ipa01.example:636", "ldaps://ipa02.example:636"}, dial)

	res, err := s.SelfTest(context.Background(), "alice")
	if err != nil {
		t.Fatalf("a working second replica must pass: %v", err)
	}
	if !strings.Contains(res.Summary, "ipa02") {
		t.Fatalf("summary must name the replica that ANSWERED: %q", res.Summary)
	}
	if !strings.Contains(res.Detail, "ipa01") || !strings.Contains(res.Detail, "could not be reached") {
		t.Fatalf("detail must report the replica that did not answer: %q", res.Detail)
	}
}

func TestSelfTestStopsWhenTheContextIsDone(t *testing.T) {
	// go-ldap's Bind and Search take NO context, and the dialer's fallback timeout does not shrink once the
	// deadline has passed — so an expired console bound is only observable BETWEEN operations. Without a check
	// there, a two-replica estate answers a cancelled 30-second activity by opening a fresh 15-second dial to
	// the next replica and running two more capped operations against a directory nobody is waiting for.
	var dialled []string
	ctx, cancel := context.WithCancel(context.Background())
	dial := func(_ context.Context, url string) (Conn, error) {
		dialled = append(dialled, url)
		cancel() // the operator's bound expires while the first replica is being probed
		return nil, errors.New("dial tcp: connection refused")
	}
	s := probeSource(t, []string{"ldaps://ipa01.example:636", "ldaps://ipa02.example:636"}, dial)

	res, err := s.SelfTest(ctx, "alice")
	if err == nil {
		t.Fatalf("a probe that contacted nothing must fail: %q", res.Summary)
	}
	if len(dialled) != 1 {
		t.Fatalf("the probe kept dialling after the context was done: %v", dialled)
	}
	// And it must not report a whole-estate outage it never observed: ipa02 was never contacted.
	if strings.Contains(res.Summary, "all 2") {
		t.Fatalf("summary claims every replica failed when one was never tried: %q", res.Summary)
	}
	if !strings.Contains(res.Detail, "CUT SHORT") {
		t.Fatalf("a run cut short must be named as such, not diagnosed as unreachable: %q", res.Detail)
	}
}

func TestSelfTestSearchIsBounded(t *testing.T) {
	// The console holds an operator on a spinner with a 30-second bound and one attempt; the probe must not
	// pull a whole directory to answer "does this bind see anything".
	var sawSize, sawTime int
	fc := &recordingConn{inner: ipaFixture(), size: &sawSize, time: &sawTime}
	s := probeSource(t, []string{"ldaps://ipa01.example:636"}, func(context.Context, string) (Conn, error) { return fc, nil })

	if _, err := s.SelfTest(context.Background(), "alice"); err != nil {
		t.Fatalf("SelfTest: %v", err)
	}
	if sawSize != probeSizeLimit || sawSize == 0 {
		t.Fatalf("search SizeLimit = %d, want the probe bound %d", sawSize, probeSizeLimit)
	}
	if sawTime > probeTimeLimit || sawTime == 0 {
		t.Fatalf("search TimeLimit = %d, want at most %d", sawTime, probeTimeLimit)
	}
}

// recordingConn records the bounds of the last search request and otherwise delegates to a fakeConn.
type recordingConn struct {
	inner *fakeConn
	size  *int
	time  *int
}

func (r *recordingConn) Bind(u, p string) error { return r.inner.Bind(u, p) }
func (r *recordingConn) Close() error           { return r.inner.Close() }
func (r *recordingConn) Search(req *ldapv3.SearchRequest) (*ldapv3.SearchResult, error) {
	*r.size, *r.time = req.SizeLimit, req.TimeLimit
	return r.inner.Search(req)
}

// TestSelfTestFailsWithEveryValueConfigured is THE KILLING ORACLE.
//
// Every configured value is present and non-empty: a replica URL, a bind-DN reference and a bind-password
// reference that both RESOLVE to real values. Only the directory disagrees — it refuses the bind the way a
// rotated password, a locked service account, or a directory that never knew this account does. A SelfTest
// implemented as "the configured values are all set" passes this test; the real one must fail it. This is
// what makes the probe more than a mock.
func TestSelfTestFailsWithEveryValueConfigured(t *testing.T) {
	fc := ipaFixture()
	fc.bindErr = ldapv3.NewError(ldapv3.LDAPResultInvalidCredentials, errors.New("invalid credentials"))
	s := probeSource(t, []string{"ldaps://ipa01.example:636"}, dialing(fc))

	res, err := s.SelfTest(context.Background(), "alice")
	if err == nil {
		t.Fatalf("a fully-configured source whose bind is refused MUST fail: %q", res.Summary)
	}
	if res.Detail == "" {
		t.Fatal("a failed probe must carry an actionable Detail")
	}
}

// TestSourceImplementsTester pins the capability the console detects by assertion. Without it the dialog
// would report "no test is implemented" while promising a bind.
func TestSourceImplementsTester(t *testing.T) {
	if _, ok := selftest.Of(probeSource(t, []string{"ldaps://ipa01.example:636"}, dialing(ipaFixture()))); !ok {
		t.Fatal("the ldap credential source must be detected as a selftest.Tester")
	}
}
