// Package ldap is the native-Go LDAP / FreeIPA credential-identity connector for the HUMAN plane
// (spec/016 T-016-10, REQ-1614, TG-102/TG-108). It is a read-only credential.CredentialSource whose
// Plane() is credential.PlaneHuman: Sync binds to a directory, SEARCHes users and groups, and maps each
// principal to an ApproverIdentity (a {user|group, name, groups} identity carrying NO secret material)
// that feeds the spec/015 approve_by resolution. It NEVER emits a host Bundle — the two-plane router
// (core/credential/plane.go) hard-fails a human-plane entry that carries one.
//
// Grounded against the LIVE estate FreeIPA (dc1freeipa01.sec.example.net, realm
// SEC.NUCLEARLIGHTERS.NET, base dc=sec,dc=example,dc=net) and the go-ldap/ldap/v3 API docs:
//   - users live under cn=users,cn=accounts,<base> (objectClass person/inetOrgPerson, uid attr, memberOf);
//   - groups live under cn=groups,cn=accounts,<base> (objectClass groupOfNames/ipausergroup, cn attr,
//     member/memberOf).
//
// The base DN, filters, and attribute names default to that layout but are fully configurable (the SEC
// realm is a default, not a hardcode).
//
// DISTROLESS-SAFE: go-ldap/ldap/v3 is a PURE-GO client (net + crypto/tls + asn1-ber, no cgo, no subprocess,
// no vendor CLI — INV-02). Transport is LDAPS:636 (TLS verified against a configured CA cert) or LDAP:389
// + StartTLS; a simple bind uses a service bind DN + password, BOTH supplied as core/config.SecretRef
// (env:/file:/store:), never literals (INV-13). READ-ONLY: only Bind + Search, never a write/modify.
//
// FAIL-CLOSED (REQ-1602/1614): an unreachable server, a TLS-verify failure, a bind failure, a search error,
// zero results, or an oversized result set all return an error — Sync fails and the prior converged state
// is retained; a partial-but-nil-error set is never returned. BOUNDED: the user/group searches carry an
// LDAP SizeLimit + TimeLimit and the combined result is capped, so a huge/hostile directory cannot OOM the
// worker. The bind password is NEVER logged, and an ApproverIdentity holds NO secret field (there are no
// secrets on the human plane — approvers are identities).
//
// Provenance: [O] INV-12/INV-13/INV-02/INV-05, spec/016 (REQ-1614), TG-102/TG-108.
package ldap

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"strings"
	"time"

	ldapv3 "github.com/go-ldap/ldap/v3"

	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/credential"
)

// SourceType is the vendor slug this connector serves.
const SourceType = "ldap"

// Generic, install-neutral defaults for a standard FreeIPA/389-DS layout. The site DN SUFFIX (dc=…) and the
// replica server URLs are ESTATE-specific and supplied at deploy time (TG_LDAP_USER_BASE / TG_LDAP_GROUP_BASE
// / TG_LDAP_URLS) — never compiled in (STONITH). An unconfigured base/URL fails closed, never falls back to a
// baked-in estate value.
const (
	defaultUserBase   = "cn=users,cn=accounts"
	defaultUserFilter = "(objectClass=person)"
	defaultUserIDAttr = "uid"
	defaultMemberOf   = "memberOf"

	defaultGroupBase   = "cn=groups,cn=accounts"
	defaultGroupFilter = "(objectClass=groupOfNames)"
	defaultGroupCNAttr = "cn"

	// bounds — belt-and-suspenders against a huge/hostile directory.
	defaultSizeLimit = 2000 // per-search LDAP SizeLimit (server aborts a bigger result → fail closed)
	defaultTimeLimit = 30   // per-search LDAP TimeLimit, seconds
	defaultMaxTotal  = 5000 // hard cap on combined users+groups
	dialTimeout      = 15 * time.Second
	opTimeout        = 20 * time.Second
)

// Conn is the MINIMAL LDAP operation surface Sync needs — the injectable seam that keeps the connector
// unit-testable with no live directory. *ldapv3.Conn satisfies it; a test injects a fake via Config.Dial. It
// is exported so an out-of-package test (the spec/016 acceptance suite) can drive the real Source with a
// faked transport, exactly as the vault/awx connectors are driven through their exported HTTP Doer seam.
type Conn interface {
	Bind(username, password string) error
	Search(*ldapv3.SearchRequest) (*ldapv3.SearchResult, error)
	Close() error
}

// DialFunc opens a bind-capable Conn to a SPECIFIC server URL. The real implementation (realDial) does
// DialURL + LDAPS/StartTLS with CA verification; a test injects a fake returning fixture SearchResults via
// Config.Dial. The URL argument is what makes replica FAILOVER injectable — a fake can succeed for one URL
// and fail for another.
type DialFunc func(ctx context.Context, url string) (Conn, error)

// Config configures a Source. URLs is the ORDERED set of replica servers (ldaps://host:636 or
// ldap://host:389) tried in sequence with FAILOVER — the FreeIPA estate runs two equivalent replicas (NL +
// GR) serving the same directory, so a successful bind+search on ANY one satisfies the sync. When URLs is
// empty the single URL is used; when both are empty it defaults to both FreeIPA replicas (NL first, GR
// second) — deterministic, fixed order, no randomness. BindDNRef and BindPasswordRef are REQUIRED SecretRefs
// for the read-only service bind (never literals, INV-13). CACertRef, if set, resolves (typically a file:
// ref) to a PEM bundle used to VERIFY every server's LDAPS/StartTLS cert — TLS is NEVER skipped. StartTLS
// upgrades a plain ldap:// connection to TLS before binding. The base DNs, filters, and attribute names
// default to the FreeIPA layout above.
type Config struct {
	ID        string
	URLs      []string         // ordered replica list, tried with failover (preferred over URL)
	URL       string           // single-server convenience; used only when URLs is empty
	StartTLS  bool             // upgrade a plain ldap:// connection with StartTLS before binding
	CACertRef config.SecretRef // PEM to verify the server cert (file:/store:); empty = system roots

	BindDNRef       config.SecretRef // service bind DN (read-only)
	BindPasswordRef config.SecretRef // service bind password — NEVER logged

	UserBaseDN    string
	UserFilter    string
	UserIDAttr    string // the login/name attribute (default uid)
	UserGroupAttr string // the membership attribute on a user (default memberOf)

	GroupBaseDN    string
	GroupFilter    string
	GroupNameAttr  string // the group name attribute (default cn)
	GroupGroupAttr string // nested-group membership on a group (default memberOf)

	SizeLimit int // per-search LDAP SizeLimit (0 → default)
	TimeLimit int // per-search LDAP TimeLimit seconds (0 → default)
	MaxTotal  int // hard cap on combined users+groups (0 → default)

	// Dial overrides the transport (a test injects a fake Conn). Nil in production → the real LDAPS/StartTLS dialer.
	Dial DialFunc
}

// serverEndpoint is one replica the sync may bind against, with its per-URL transport facts.
type serverEndpoint struct {
	url        string
	isLDAPS    bool
	serverName string
}

// Source is a read-only human-plane credential.CredentialSource backed by an LDAP/FreeIPA directory (one or
// more equivalent replicas tried with failover).
type Source struct {
	id        string
	servers   []serverEndpoint
	startTLS  bool
	caCertRef config.SecretRef

	bindDNRef config.SecretRef
	bindPwRef config.SecretRef

	userBase, userFilter, userIDAttr, userGroupAttr       string
	groupBase, groupFilter, groupNameAttr, groupGroupAttr string

	sizeLimit, timeLimit, maxTotal int

	dial DialFunc
}

// New builds a Source. It fails closed if the id, any server URL, the bind DN ref, or the bind password ref
// is missing/malformed. The server list is URLs (ordered), else the single URL, else both FreeIPA replicas.
func New(cfg Config) (*Source, error) {
	if strings.TrimSpace(cfg.ID) == "" {
		return nil, fmt.Errorf("ldap: source requires an id")
	}
	urls := cfg.URLs
	if len(urls) == 0 && strings.TrimSpace(cfg.URL) != "" {
		urls = []string{cfg.URL}
	}
	if len(urls) == 0 {
		// Fail closed: no compiled-in estate replica default (STONITH). The replica list is deploy config
		// (TG_LDAP_URLS); an empty list is a configuration error, never a baked-in estate fallback.
		return nil, fmt.Errorf("ldap: source %q requires at least one server URL (set TG_LDAP_URLS)", cfg.ID)
	}
	servers := make([]serverEndpoint, 0, len(urls))
	for _, raw := range urls {
		u := strings.TrimSpace(raw)
		if u == "" {
			continue
		}
		if !strings.HasPrefix(u, "ldap://") && !strings.HasPrefix(u, "ldaps://") {
			return nil, fmt.Errorf("ldap: source %q server URL %q must start with ldap:// or ldaps://", cfg.ID, u)
		}
		servers = append(servers, serverEndpoint{url: u, isLDAPS: strings.HasPrefix(u, "ldaps://"), serverName: hostFromURL(u)})
	}
	if len(servers) == 0 {
		return nil, fmt.Errorf("ldap: source %q requires at least one server URL (ldaps://host:636 or ldap://host:389)", cfg.ID)
	}
	if strings.TrimSpace(string(cfg.BindDNRef)) == "" {
		return nil, fmt.Errorf("ldap: source %q requires a bind DN SecretRef (never a literal)", cfg.ID)
	}
	if strings.TrimSpace(string(cfg.BindPasswordRef)) == "" {
		return nil, fmt.Errorf("ldap: source %q requires a bind password SecretRef (never a literal)", cfg.ID)
	}
	s := &Source{
		id:             cfg.ID,
		servers:        servers,
		startTLS:       cfg.StartTLS,
		caCertRef:      cfg.CACertRef,
		bindDNRef:      cfg.BindDNRef,
		bindPwRef:      cfg.BindPasswordRef,
		userBase:       orDefault(cfg.UserBaseDN, defaultUserBase),
		userFilter:     orDefault(cfg.UserFilter, defaultUserFilter),
		userIDAttr:     orDefault(cfg.UserIDAttr, defaultUserIDAttr),
		userGroupAttr:  orDefault(cfg.UserGroupAttr, defaultMemberOf),
		groupBase:      orDefault(cfg.GroupBaseDN, defaultGroupBase),
		groupFilter:    orDefault(cfg.GroupFilter, defaultGroupFilter),
		groupNameAttr:  orDefault(cfg.GroupNameAttr, defaultGroupCNAttr),
		groupGroupAttr: orDefault(cfg.GroupGroupAttr, defaultMemberOf),
		sizeLimit:      orDefaultInt(cfg.SizeLimit, defaultSizeLimit),
		timeLimit:      orDefaultInt(cfg.TimeLimit, defaultTimeLimit),
		maxTotal:       orDefaultInt(cfg.MaxTotal, defaultMaxTotal),
		dial:           cfg.Dial,
	}
	if s.dial == nil {
		s.dial = s.realDial
	}
	return s, nil
}

// ID implements credential.CredentialSource.
func (s *Source) ID() string { return s.id }

// Plane implements credential.CredentialSource — an LDAP source feeds the human → console approver plane.
func (s *Source) Plane() credential.Plane { return credential.PlaneHuman }

// Sync performs the READ-ONLY pull (REQ-1607/1614) with replica FAILOVER: it tries each configured server in
// FIXED order (deterministic, no randomness) and returns the first server's full result. A per-server failure
// — unreachable / TLS-verify failure (dial), bind failure, search error, ZERO results, or an OVERSIZED result
// — moves to the next replica; only when ALL replicas are exhausted does Sync FAIL CLOSED (leaving the prior
// converged state intact). The bind password is resolved from a SecretRef once and never logged.
func (s *Source) Sync(ctx context.Context) ([]credential.SourceEntry, error) {
	bindDN, err := s.bindDNRef.Resolve()
	if err != nil {
		return nil, fmt.Errorf("ldap: source %q resolve bind DN: %w", s.id, err)
	}
	bindPw, err := s.bindPwRef.Resolve()
	if err != nil {
		return nil, fmt.Errorf("ldap: source %q resolve bind password: %w", s.id, err)
	}
	if bindDN == "" || bindPw == "" {
		return nil, fmt.Errorf("ldap: source %q bind DN or password is empty (fail closed)", s.id)
	}

	var lastErr error
	for _, ep := range s.servers {
		entries, err := s.syncOne(ctx, ep, bindDN, bindPw)
		if err == nil {
			return entries, nil // a successful bind+search on any replica satisfies the sync
		}
		lastErr = fmt.Errorf("%s: %w", ep.url, err) // never carries the password
	}
	return nil, fmt.Errorf("ldap: source %q all %d replica(s) failed (fail closed, prior state retained): %w", s.id, len(s.servers), lastErr)
}

// syncOne binds against ONE replica and pulls its full user+group set, or returns an error (which triggers
// failover to the next replica). It never returns a partial-but-nil-error set.
func (s *Source) syncOne(ctx context.Context, ep serverEndpoint, bindDN, bindPw string) ([]credential.SourceEntry, error) {
	c, err := s.dial(ctx, ep.url) // unreachable / TLS-verify failure surfaces here
	if err != nil {
		return nil, fmt.Errorf("connect/TLS: %w", err)
	}
	defer c.Close()

	if err := c.Bind(bindDN, bindPw); err != nil {
		// The go-ldap error carries only the LDAP result code, never the password; do not add it.
		return nil, fmt.Errorf("bind failed: %w", err)
	}

	users, err := s.searchPrincipals(c, s.userBase, s.userFilter,
		[]string{s.userIDAttr, s.userGroupAttr}, s.userIDAttr, s.userGroupAttr, credential.PrincipalUser)
	if err != nil {
		return nil, fmt.Errorf("user search: %w", err)
	}
	groups, err := s.searchPrincipals(c, s.groupBase, s.groupFilter,
		[]string{s.groupNameAttr, s.groupGroupAttr}, s.groupNameAttr, s.groupGroupAttr, credential.PrincipalGroup)
	if err != nil {
		return nil, fmt.Errorf("group search: %w", err)
	}

	if len(users)+len(groups) == 0 {
		return nil, fmt.Errorf("returned zero users and zero groups")
	}
	if total := len(users) + len(groups); total > s.maxTotal {
		return nil, fmt.Errorf("returned %d principals over the %d cap", total, s.maxTotal)
	}
	return append(users, groups...), nil
}

// searchPrincipals runs one bounded (SizeLimit+TimeLimit) subtree search and maps every entry to an
// approver-identity SourceEntry via the pure mapper. A search error (including the server aborting on
// SizeLimit) propagates → fail closed.
func (s *Source) searchPrincipals(c Conn, baseDN, filter string, attrs []string, nameAttr, groupAttr string, kind credential.PrincipalKind) ([]credential.SourceEntry, error) {
	req := ldapv3.NewSearchRequest(
		baseDN, ldapv3.ScopeWholeSubtree, ldapv3.NeverDerefAliases,
		s.sizeLimit, s.timeLimit, false, filter, attrs, nil,
	)
	res, err := c.Search(req)
	if err != nil {
		return nil, err
	}
	out := make([]credential.SourceEntry, 0, len(res.Entries))
	for _, e := range res.Entries {
		entry, err := entryToApprover(e, nameAttr, groupAttr, kind)
		if err != nil {
			return nil, err // a malformed principal fails the whole sync closed (never inject a blank identity)
		}
		out = append(out, entry)
	}
	return out, nil
}

// entryToApprover is the PURE mapping from one LDAP entry to a human-plane SourceEntry — the core testable
// unit. It reads nameAttr as the principal name and groupAttr (a multi-valued list of group DNs, e.g.
// memberOf) as the group memberships (each DN reduced to its cn). It NEVER sets a Bundle (human plane): the
// returned entry carries an Approver only. NativeID is the entry DN (its stable directory id).
func entryToApprover(e *ldapv3.Entry, nameAttr, groupAttr string, kind credential.PrincipalKind) (credential.SourceEntry, error) {
	name := strings.TrimSpace(e.GetAttributeValue(nameAttr))
	if name == "" {
		return credential.SourceEntry{}, fmt.Errorf("ldap: entry %q has no %s value (fail closed, no blank identity)", e.DN, nameAttr)
	}
	groups := groupCNs(e.GetAttributeValues(groupAttr))
	a, err := credential.NewApproverIdentity(credential.ApproverIdentitySpec{Kind: kind, Name: name, Groups: groups})
	if err != nil {
		return credential.SourceEntry{}, fmt.Errorf("ldap: entry %q approver: %w", e.DN, err)
	}
	// Human plane: Approver only, Bundle deliberately left zero (the router rejects a bundle here).
	return credential.SourceEntry{NativeID: firstNonEmpty(e.DN, name), Approver: a}, nil
}

// groupCNs reduces a list of group references to their common-name (cn) values. FreeIPA populates memberOf
// with full DNs ("cn=sre-oncall,cn=groups,cn=accounts,..."); a directory that already returns a bare name is
// passed through. The cn is what spec/015 approve_by matches on.
func groupCNs(vals []string) []string {
	if len(vals) == 0 {
		return nil
	}
	out := make([]string, 0, len(vals))
	for _, v := range vals {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if cn := cnOfDN(v); cn != "" {
			out = append(out, cn)
			continue
		}
		out = append(out, v)
	}
	return out
}

// cnOfDN returns the value of the first cn= RDN in a DN, or "" if v is not a DN / has no cn RDN. It uses the
// go-ldap DN parser so escaping is handled correctly.
func cnOfDN(v string) string {
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

// realDial is the production transport for ONE replica URL: DialURL over LDAPS (implicit TLS) or plain LDAP +
// optional StartTLS, verifying the server cert against the configured CA (never InsecureSkipVerify).
// Read/bind operations carry a timeout so a hung server fails closed (and Sync then fails over to the next
// replica). It looks up the endpoint's per-URL transport facts (LDAPS?, ServerName) from the configured set.
func (s *Source) realDial(ctx context.Context, url string) (Conn, error) {
	ep := s.endpointFor(url)
	tc, err := s.tlsConfig(ep.serverName)
	if err != nil {
		return nil, err
	}
	d := &net.Dialer{Timeout: timeoutFrom(ctx, dialTimeout)}
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
	if !ep.isLDAPS && s.startTLS {
		if err := l.StartTLS(tc); err != nil {
			l.Close()
			return nil, fmt.Errorf("starttls: %w", err) // TLS-verify failure → fail closed → failover
		}
	}
	l.SetTimeout(opTimeout)
	return l, nil
}

// endpointFor returns the configured endpoint for a URL (its LDAPS/ServerName facts), or a freshly-derived
// one if the URL is not in the set (e.g. a live-test probe URL).
func (s *Source) endpointFor(url string) serverEndpoint {
	for _, ep := range s.servers {
		if ep.url == url {
			return ep
		}
	}
	return serverEndpoint{url: url, isLDAPS: strings.HasPrefix(url, "ldaps://"), serverName: hostFromURL(url)}
}

// tlsConfig builds the verified TLS config: TLS 1.2+, the given ServerName, and (if a CA ref is set) the
// resolved PEM as the sole root pool. It NEVER sets InsecureSkipVerify.
func (s *Source) tlsConfig(serverName string) (*tls.Config, error) {
	tc := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: serverName}
	if strings.TrimSpace(string(s.caCertRef)) != "" {
		pem, err := s.caCertRef.Resolve()
		if err != nil {
			return nil, fmt.Errorf("resolve CA cert ref: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(pem)) {
			return nil, fmt.Errorf("CA cert ref contains no valid certificate")
		}
		tc.RootCAs = pool
	}
	return tc, nil
}

// compile-time proof the source satisfies the stable credential-source interface.
var _ credential.CredentialSource = (*Source)(nil)

// ---- small helpers ----------------------------------------------------------------------------------

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

func orDefaultInt(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

// hostFromURL extracts the host (without scheme or port) for TLS ServerName verification.
func hostFromURL(u string) string {
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

// timeoutFrom returns the time until the context deadline, capped at def, or def if there is no deadline.
func timeoutFrom(ctx context.Context, def time.Duration) time.Duration {
	if dl, ok := ctx.Deadline(); ok {
		if d := time.Until(dl); d > 0 && d < def {
			return d
		}
	}
	return def
}
