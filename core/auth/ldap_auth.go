// LDAP-backed console login (spec/006 REQ-508 extension): a FOURTH way to authenticate a browser
// operator session — a FreeIPA user's own directory username + password — composed WITH, and never
// weakening, the static break-glass operator+token. It is bind-as-user by design: TG NEVER stores,
// hashes, or logs the password; the password is presented to FreeIPA in an LDAP simple BIND, and a
// successful bind IS the proof of identity (FreeIPA verified it, not TG). A bind failure is an auth
// failure, fail-closed. After the user binds, TG reads THAT USER'S OWN entry's group membership
// (memberOf) and maps it to a role: the configured admin group → admin-eligible (may step up to the
// admin write tier without the separate static admin credential), the operator group → read-only
// operator, and membership in NEITHER allowed group → DENY (a valid FreeIPA user with no TG group
// cannot log in). Replica FAILOVER mirrors the human-plane connector: try each URL in fixed order; a
// definitive invalid-credentials bind is returned immediately (same directory on every replica), while
// an unreachable/erroring replica moves to the next, and all-down fails closed.
//
// DISTROLESS-SAFE: go-ldap/ldap/v3 is a PURE-GO client (net + crypto/tls + asn1-ber, no cgo, no
// subprocess, no vendor CLI — INV-02). Transport is LDAPS:636 (TLS verified against a configured CA) or
// LDAP:389 + StartTLS; TLS is NEVER skipped (no InsecureSkipVerify), the same fail-closed TLS the
// credsource/ldap connector uses. The Conn/DialFunc is an INJECTABLE seam (LDAPConfig.Dial) so the
// oracles drive the real authenticator with a faked bind and no live directory — exactly as the
// connector is driven through its exported Conn seam. The CA arrives as a core/config.SecretRef, never a
// literal.
//
// Provenance: [O] INV-01 (mandatory auth, default-deny; the LDAP path never falls through to a weaker
// check), INV-02 (pure-Go, distroless), INV-13 (secrets by reference) · [R] spec/006 REQ-508 (browser
// operator session) + REQ-522 (admin tier — a tg-admins LDAP session is admin-eligible) · reuses the
// spec/016 credsource/ldap transport pattern.
package auth

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	ldapv3 "github.com/go-ldap/ldap/v3"

	"github.com/territory-grounder/grounder/core/config"
)

// LDAPRole is the outcome of an LDAP login: DENY (fail closed — no admissible TG group), a read-only
// operator, or an admin-eligible operator (a tg-admins member who may step up to the admin write tier).
type LDAPRole int

const (
	// LDAPDeny is the fail-closed zero value: not authenticated, or authenticated but in no allowed TG group.
	LDAPDeny LDAPRole = iota
	// LDAPOperator is a read-only operator session (operator-group member).
	LDAPOperator
	// LDAPAdmin is an admin-eligible session (admin-group member): read-only by default, may step up.
	LDAPAdmin
)

// Generic, install-neutral defaults. The user-DN TEMPLATE's site suffix (dc=…) is ESTATE-specific and
// supplied at deploy time (TG_LDAP_AUTH_USER_DN_TEMPLATE) — never compiled in (STONITH). The generic default
// carries only the standard FreeIPA/389-DS RDN path; a deployment against a real directory MUST set the full
// template (an unconfigured suffix binds under the bare path and fails auth clearly, never leaks an estate DN).
const (
	defaultUserDNTemplate = "uid=%s,cn=users,cn=accounts"
	defaultAdminGroup     = "tg-admins"
	defaultOperatorGroup  = "tg-operators"
	defaultMemberOfAttr   = "memberOf"

	ldapDialTimeout = 15 * time.Second
	ldapOpTimeout   = 20 * time.Second
)

// LDAPConn is the MINIMAL LDAP operation surface a login needs — the injectable seam that keeps the
// authenticator unit-testable with no live directory. *ldapv3.Conn satisfies it; a test injects a fake
// via LDAPConfig.Dial.
type LDAPConn interface {
	Bind(username, password string) error
	Search(*ldapv3.SearchRequest) (*ldapv3.SearchResult, error)
	Close() error
}

// LDAPDialFunc opens a bind-capable LDAPConn to a SPECIFIC server URL. The real implementation does
// DialURL + LDAPS/StartTLS with CA verification; a test injects a fake. The URL argument is what makes
// replica FAILOVER injectable — a fake can succeed for one URL and fail for another.
type LDAPDialFunc func(ctx context.Context, url string) (LDAPConn, error)

// LDAPConfig configures an LDAPAuthenticator. URLs is the ORDERED replica set (ldaps://host:636 or
// ldap://host:389) tried with failover. UserDNTemplate must contain exactly one %s (the escaped
// username) and produces the bind DN. AdminGroup/OperatorGroup are the group CNs whose membership grants
// admin-eligible / read-only operator roles. CACertRef, if set, resolves (typically a file: ref) to a
// PEM bundle used to VERIFY every server's LDAPS/StartTLS cert — TLS is NEVER skipped.
type LDAPConfig struct {
	URLs           []string
	StartTLS       bool
	CACertRef      config.SecretRef
	UserDNTemplate string
	AdminGroup     string
	OperatorGroup  string
	MemberOfAttr   string
	// Dial overrides the transport (a test injects a fake). Nil in production → the real LDAPS/StartTLS dialer.
	Dial LDAPDialFunc
}

// ldapEndpoint is one replica the login may bind against, with its per-URL transport facts.
type ldapEndpoint struct {
	url        string
	isLDAPS    bool
	serverName string
}

// LDAPAuthenticator binds a console login as the user against FreeIPA and maps the user's own group
// membership to a role. It holds NO password — the password flows through Authenticate and is discarded.
type LDAPAuthenticator struct {
	servers        []ldapEndpoint
	startTLS       bool
	caCertRef      config.SecretRef
	userDNTemplate string
	adminGroup     string
	operatorGroup  string
	memberOfAttr   string
	dial           LDAPDialFunc
}

// NewLDAPAuthenticator constructs the authenticator and fails closed at construction on any missing
// piece: no server URL, a malformed URL, a template without exactly one %s, or an empty group name is a
// boot-time error, never a silently weaker runtime.
func NewLDAPAuthenticator(cfg LDAPConfig) (*LDAPAuthenticator, error) {
	servers := make([]ldapEndpoint, 0, len(cfg.URLs))
	for _, raw := range cfg.URLs {
		u := strings.TrimSpace(raw)
		if u == "" {
			continue
		}
		if !strings.HasPrefix(u, "ldap://") && !strings.HasPrefix(u, "ldaps://") {
			return nil, fmt.Errorf("auth: LDAP server URL %q must start with ldap:// or ldaps://", u)
		}
		servers = append(servers, ldapEndpoint{url: u, isLDAPS: strings.HasPrefix(u, "ldaps://"), serverName: ldapHostFromURL(u)})
	}
	if len(servers) == 0 {
		return nil, errors.New("auth: LDAP login requires at least one server URL (ldaps://host:636 or ldap://host:389)")
	}
	tmpl := orLDAPDefault(cfg.UserDNTemplate, defaultUserDNTemplate)
	if strings.Count(tmpl, "%s") != 1 {
		return nil, fmt.Errorf("auth: LDAP user DN template %q must contain exactly one %%s", tmpl)
	}
	admin := orLDAPDefault(cfg.AdminGroup, defaultAdminGroup)
	operator := orLDAPDefault(cfg.OperatorGroup, defaultOperatorGroup)
	if strings.TrimSpace(admin) == "" || strings.TrimSpace(operator) == "" {
		return nil, errors.New("auth: LDAP login requires a non-empty admin group and operator group")
	}
	a := &LDAPAuthenticator{
		servers:        servers,
		startTLS:       cfg.StartTLS,
		caCertRef:      cfg.CACertRef,
		userDNTemplate: tmpl,
		adminGroup:     admin,
		operatorGroup:  operator,
		memberOfAttr:   orLDAPDefault(cfg.MemberOfAttr, defaultMemberOfAttr),
		dial:           cfg.Dial,
	}
	if a.dial == nil {
		a.dial = a.realDial
	}
	return a, nil
}

// Authenticate binds as the user and resolves their role, with replica FAILOVER. It returns:
//   - (role, nil) with role in {LDAPOperator, LDAPAdmin} on a successful bind whose user is in an allowed group;
//   - (LDAPDeny, ErrUnauthenticated) on a definitive auth failure (bad password) OR a successful bind whose
//     user is in NO allowed TG group (fail closed — a valid FreeIPA user with no TG group cannot log in);
//   - (LDAPDeny, non-nil error) when ALL replicas are unreachable/erroring (fail closed, never a weaker fallback).
//
// The password is NEVER logged, stored, or hashed — it is passed to Bind and then out of scope.
func (a *LDAPAuthenticator) Authenticate(ctx context.Context, username, password string) (LDAPRole, error) {
	// An empty password with a valid DN would be an LDAP "unauthenticated bind" that many servers accept
	// as success without verifying anything — reject it explicitly (fail closed). An empty/hostile
	// username is rejected before it is ever interpolated into a DN.
	if strings.TrimSpace(username) == "" || password == "" {
		return LDAPDeny, ErrUnauthenticated
	}
	if strings.ContainsAny(username, "\x00\n\r") {
		return LDAPDeny, ErrUnauthenticated
	}
	userDN := fmt.Sprintf(a.userDNTemplate, ldapv3.EscapeDN(username))

	var lastErr error
	for _, ep := range a.servers {
		c, err := a.dial(ctx, ep.url)
		if err != nil {
			lastErr = fmt.Errorf("%s: connect/TLS: %w", ep.url, err) // never carries the password
			continue
		}
		bindErr := c.Bind(userDN, password)
		if bindErr != nil {
			c.Close()
			// A definitive "invalid credentials" is the same verdict on every replica — return it now,
			// do NOT fail over (that would just re-ask the same directory the same question).
			if ldapv3.IsErrorWithCode(bindErr, ldapv3.LDAPResultInvalidCredentials) {
				return LDAPDeny, ErrUnauthenticated
			}
			lastErr = fmt.Errorf("%s: bind: %w", ep.url, bindErr) // server-side issue → fail over
			continue
		}
		groups, err := a.readOwnGroups(c, userDN)
		c.Close()
		if err != nil {
			lastErr = fmt.Errorf("%s: group read: %w", ep.url, err)
			continue
		}
		// Bind succeeded → the user IS authenticated. The role decision is now definitive (same groups on
		// every replica): return it without failover, DENY included.
		role := a.roleFor(groups)
		return role, roleErr(role)
	}
	return LDAPDeny, fmt.Errorf("auth: LDAP login: all %d replica(s) failed (fail closed): %w", len(a.servers), lastErr)
}

// readOwnGroups reads the just-bound user's OWN entry (base-scoped search on their DN) and returns their
// group CNs. The user can always read their own memberOf, so no service bind is needed.
func (a *LDAPAuthenticator) readOwnGroups(c LDAPConn, userDN string) ([]string, error) {
	req := ldapv3.NewSearchRequest(
		userDN, ldapv3.ScopeBaseObject, ldapv3.NeverDerefAliases,
		0, int(ldapOpTimeout.Seconds()), false,
		"(objectClass=*)", []string{a.memberOfAttr}, nil,
	)
	res, err := c.Search(req)
	if err != nil {
		return nil, err
	}
	if len(res.Entries) == 0 {
		return nil, errors.New("bound user entry not readable")
	}
	return ldapGroupCNs(res.Entries[0].GetAttributeValues(a.memberOfAttr)), nil
}

// roleFor maps a user's group CNs to a role: admin group wins over operator group; neither → DENY.
func (a *LDAPAuthenticator) roleFor(groups []string) LDAPRole {
	for _, g := range groups {
		if strings.EqualFold(g, a.adminGroup) {
			return LDAPAdmin
		}
	}
	for _, g := range groups {
		if strings.EqualFold(g, a.operatorGroup) {
			return LDAPOperator
		}
	}
	return LDAPDeny
}

// roleErr turns a DENY role into the fail-closed auth error, so a valid FreeIPA user in no TG group is
// rejected exactly like a bad password.
func roleErr(r LDAPRole) error {
	if r == LDAPDeny {
		return ErrUnauthenticated
	}
	return nil
}

// realDial is the production transport for ONE replica URL: DialURL over LDAPS (implicit TLS) or plain
// LDAP + optional StartTLS, verifying the server cert against the configured CA (never InsecureSkipVerify).
func (a *LDAPAuthenticator) realDial(ctx context.Context, url string) (LDAPConn, error) {
	ep := a.endpointFor(url)
	tc, err := a.tlsConfig(ep.serverName)
	if err != nil {
		return nil, err
	}
	d := &net.Dialer{Timeout: ldapDialTimeout}
	var opts []ldapv3.DialOpt
	if ep.isLDAPS {
		opts = append(opts, ldapv3.DialWithTLSDialer(tc, d))
	} else {
		opts = append(opts, ldapv3.DialWithDialer(d))
	}
	l, err := ldapv3.DialURL(url, opts...)
	if err != nil {
		return nil, err // unreachable or (for ldaps) TLS-verify failure → fail closed → failover
	}
	if !ep.isLDAPS && a.startTLS {
		if err := l.StartTLS(tc); err != nil {
			l.Close()
			return nil, fmt.Errorf("starttls: %w", err)
		}
	}
	l.SetTimeout(ldapOpTimeout)
	return l, nil
}

// endpointFor returns the configured endpoint facts for a URL (LDAPS?, ServerName).
func (a *LDAPAuthenticator) endpointFor(url string) ldapEndpoint {
	for _, ep := range a.servers {
		if ep.url == url {
			return ep
		}
	}
	return ldapEndpoint{url: url, isLDAPS: strings.HasPrefix(url, "ldaps://"), serverName: ldapHostFromURL(url)}
}

// tlsConfig builds the verified TLS config: TLS 1.2+, the given ServerName, and (if a CA ref is set) the
// resolved PEM as the sole root pool. It NEVER sets InsecureSkipVerify.
func (a *LDAPAuthenticator) tlsConfig(serverName string) (*tls.Config, error) {
	tc := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: serverName}
	if strings.TrimSpace(string(a.caCertRef)) != "" {
		pem, err := a.caCertRef.Resolve()
		if err != nil {
			return nil, fmt.Errorf("resolve CA cert ref: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(pem)) {
			return nil, errors.New("CA cert ref contains no valid certificate")
		}
		tc.RootCAs = pool
	}
	return tc, nil
}

// ---- small helpers (kept local so core/auth does not depend on modules/) ----

func orLDAPDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

// ldapGroupCNs reduces a list of group references to their common-name (cn) values. FreeIPA populates
// memberOf with full DNs ("cn=tg-admins,cn=groups,cn=accounts,..."); a bare name is passed through.
func ldapGroupCNs(vals []string) []string {
	out := make([]string, 0, len(vals))
	for _, v := range vals {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if cn := ldapCNOfDN(v); cn != "" {
			out = append(out, cn)
			continue
		}
		out = append(out, v)
	}
	return out
}

// ldapCNOfDN returns the value of the first cn= RDN in a DN, or "" if v is not a DN / has no cn RDN.
func ldapCNOfDN(v string) string {
	if !strings.Contains(v, "=") {
		return ""
	}
	dn, err := ldapv3.ParseDN(v)
	if err != nil {
		return ""
	}
	for _, rdn := range dn.RDNs {
		for _, atv := range rdn.Attributes {
			if strings.EqualFold(atv.Type, "cn") {
				return atv.Value
			}
		}
	}
	return ""
}

// ldapHostFromURL extracts the host (without scheme or port) for TLS ServerName verification.
func ldapHostFromURL(u string) string {
	h := u
	if i := strings.Index(h, "://"); i >= 0 {
		h = h[i+3:]
	}
	if i := strings.IndexByte(h, '/'); i >= 0 {
		h = h[:i]
	}
	if host, _, err := net.SplitHostPort(h); err == nil {
		return host
	}
	return h
}
