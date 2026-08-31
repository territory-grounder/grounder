// Package ldapident is the LDAP/FreeIPA identity/auth resolver (spec/023 REQ-2315..2319): the identity
// dimension of actor-attribution. It answers, for principals ALREADY named by admissible action-evidence,
// "is this an enabled, non-service member of a sanctioned admin group (CONFIRMED — promote), or a
// locked/disabled/expired credential (DISABLED — demote)?" It implements adapters/actorevidence.SanctionResolver
// — a NON-Reader seam: it never emits an Evidence record and never mints a taxonomy (REQ-2315 preserves
// REQ-2312), it only refines the classification of an already-named actor in the restrictive direction.
//
// It is READ-ONLY and ADVISORY / fail-open (REQ-2319): any error — unreachable, TLS-verify failure, bind
// failure, or an ambiguous/absent entry — yields EMPTY facts, so the enrichment fold leaves the
// classification byte-identical to the static Phase-1 ladder. A dead resolver can neither promote (grant a
// stand-down) nor demote (mint suspicious), because both buckets fill ONLY on a fresh POSITIVE directory fact.
//
// Transport mirrors modules/credsource/ldap (go-ldap/ldap/v3 — pure-Go, distroless-safe): CA-verified
// LDAPS:636 (TLS is NEVER skipped), ordered replica FAILOVER, a service bind DN + password supplied as
// core/config.SecretRef (never literals, INV-13), and per-search Size/Time limits so a hostile directory
// cannot OOM the worker. The one query the bulk credsource Sync lacks is added here: a bounded
// single-shot lookup of the session's actors reading nsAccountLock, krbPrincipalExpiration, memberOf, and
// objectClass. (A shared transport dialer with credsource is a deliberate future refactor; the
// CA-verification path is kept identical here and unit-tested.)
//
// Provenance: [F] owner epic (identity dimension) · [O] INV-11/INV-13/INV-17, spec/023 REQ-2315..2320.
package ldapident

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"strings"
	"time"

	ldapv3 "github.com/go-ldap/ldap/v3"

	"github.com/territory-grounder/grounder/adapters/actorevidence"
	"github.com/territory-grounder/grounder/core/config"
)

const (
	// Generic install-neutral default: the standard FreeIPA/389-DS accounts container. The site DN suffix
	// (dc=…) is supplied at deploy time via TG_LDAP_USER_BASE — the estate suffix is NEVER compiled in (STONITH).
	defaultUserBase  = "cn=users,cn=accounts"
	defaultSizeLimit = 500
	defaultTimeLimit = 15 // seconds, per-search LDAP TimeLimit
	dialTimeout      = 8 * time.Second
	opTimeout        = 12 * time.Second

	attrAccountLock = "nsAccountLock"
	attrKrbExpiry   = "krbPrincipalExpiration"
	attrMemberOf    = "memberOf"
	attrObjectClass = "objectClass"
	attrUID         = "uid"
)

// Conn is the minimal go-ldap surface the resolver uses (Bind + Search + Close), so a test injects a fake
// with no live directory. *ldapv3.Conn satisfies it.
type Conn interface {
	Bind(username, password string) error
	Search(*ldapv3.SearchRequest) (*ldapv3.SearchResult, error)
	Close() error
}

// DialFunc opens a bind-capable Conn to a specific server URL (the argument is what makes replica failover
// injectable). The production impl is realDial (CA-verified LDAPS); a test injects a fake.
type DialFunc func(ctx context.Context, url string) (Conn, error)

// Config configures the resolver. URLs is the ORDERED replica set (deterministic failover). BindDNRef and
// BindPasswordRef are REQUIRED SecretRefs. CACertRef verifies every server's LDAPS cert (TLS never skipped).
type Config struct {
	URLs            []string
	BindDNRef       config.SecretRef
	BindPasswordRef config.SecretRef
	// CACertRef verifies every server's LDAPS/StartTLS cert (TLS is never skipped). When EMPTY, verification
	// falls back to the system default CA pool — set it to the FreeIPA CA (file:/secrets/freeipa-ca.crt) so a
	// private CA is trusted; an empty ref against a private-CA directory fails the handshake (fail closed).
	CACertRef  config.SecretRef
	UserBaseDN string
	SizeLimit  int
	TimeLimit  int
	// Dial overrides the transport (a test injects a fake). Nil ⇒ the real CA-verified LDAPS dialer.
	Dial DialFunc
}

// Resolver implements actorevidence.SanctionResolver over an LDAP/FreeIPA directory.
type Resolver struct {
	urls      []string
	bindDNRef config.SecretRef
	bindPwRef config.SecretRef
	caCertRef config.SecretRef
	userBase  string
	sizeLimit int
	timeLimit int
	dial      DialFunc
}

// New builds a resolver, or returns an error (fail closed) when a required field is missing.
func New(cfg Config) (*Resolver, error) {
	urls := cfg.URLs
	if len(urls) == 0 {
		// Fail closed: no compiled-in estate server default (STONITH). The caller supplies the replica list
		// from deploy config (TG_LDAP_URLS); an empty list means the resolver is simply not configured.
		return nil, fmt.Errorf("ldapident: no LDAP server URLs configured (set TG_LDAP_URLS)")
	}
	for _, u := range urls {
		if !strings.HasPrefix(u, "ldap://") && !strings.HasPrefix(u, "ldaps://") {
			return nil, fmt.Errorf("ldapident: server URL %q must start with ldap:// or ldaps://", u)
		}
	}
	if strings.TrimSpace(string(cfg.BindDNRef)) == "" || strings.TrimSpace(string(cfg.BindPasswordRef)) == "" {
		return nil, fmt.Errorf("ldapident: a bind DN and password SecretRef are required (never literals)")
	}
	r := &Resolver{
		urls:      urls,
		bindDNRef: cfg.BindDNRef,
		bindPwRef: cfg.BindPasswordRef,
		caCertRef: cfg.CACertRef,
		userBase:  orDefault(cfg.UserBaseDN, defaultUserBase),
		sizeLimit: orDefaultInt(cfg.SizeLimit, defaultSizeLimit),
		timeLimit: orDefaultInt(cfg.TimeLimit, defaultTimeLimit),
		dial:      cfg.Dial,
	}
	if r.dial == nil {
		r.dial = r.realDial
	}
	return r, nil
}

func (r *Resolver) Dimension() string { return "ldap" }
func (r *Resolver) ReadOnly() bool    { return true }

var _ actorevidence.SanctionResolver = (*Resolver)(nil)

// Resolve looks up the DISTINCT actors of one domain in a single bind+search (with replica failover) and
// classifies each as Confirmed (enabled, non-service, member of a sanctioned admin group) or Disabled
// (locked/expired). An error is advisory (REQ-2319): the caller treats the result as absent. Only actors
// resolvable to a directory uid are queried; a principal that is not an LDAP uid (e.g. a PVE realm token)
// simply never appears in either bucket.
func (r *Resolver) Resolve(ctx context.Context, domain string, actors, sanctionedGroups []string) (actorevidence.SanctionFacts, error) {
	var facts actorevidence.SanctionFacts
	// Map each directory uid back to the ORIGINAL actor strings that produced it — the downstream classifier
	// matches Sanctioned by the exact actor string, so a verdict on uid "kp" must be applied to the actor
	// forms "kp" and/or "kp@SEC.REALM" exactly as they appear in the evidence.
	uidToActors := map[string][]string{}
	for _, a := range actors {
		if uid := actorToUID(a); uid != "" {
			uidToActors[uid] = append(uidToActors[uid], a)
		}
	}
	uids := make([]string, 0, len(uidToActors))
	for uid := range uidToActors {
		uids = append(uids, uid)
	}
	if len(uids) == 0 {
		return facts, nil // nothing an LDAP directory can speak to
	}
	bindDN, err := r.bindDNRef.Resolve()
	if err != nil {
		return facts, fmt.Errorf("ldapident: resolve bind DN (fail closed): %w", err)
	}
	bindPw, err := r.bindPwRef.Resolve()
	if err != nil {
		return facts, fmt.Errorf("ldapident: resolve bind password (fail closed): %w", err)
	}
	if bindDN == "" || bindPw == "" {
		return facts, fmt.Errorf("ldapident: bind DN or password empty (fail closed)")
	}

	var lastErr error
	for _, url := range r.urls {
		entries, err := r.searchOne(ctx, url, bindDN, bindPw, uids)
		if err != nil {
			lastErr = fmt.Errorf("%s: %w", url, err) // never carries the password
			continue
		}
		classify(entries, uidToActors, sanctionedGroups, &facts)
		return facts, nil // a successful bind+search on any replica satisfies the lookup
	}
	facts.Warnings = append(facts.Warnings, fmt.Sprintf("ldap: domain %q — all %d replica(s) unreachable (advisory; classification unchanged)", domain, len(r.urls)))
	return facts, fmt.Errorf("ldapident: all %d replica(s) failed (fail open, classification unchanged): %w", len(r.urls), lastErr)
}

// searchOne binds one replica and runs the batched single-principal search, returning the matched entries.
func (r *Resolver) searchOne(ctx context.Context, url, bindDN, bindPw string, uids []string) ([]*ldapv3.Entry, error) {
	c, err := r.dial(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("connect/TLS: %w", err)
	}
	defer c.Close()
	if err := c.Bind(bindDN, bindPw); err != nil {
		return nil, fmt.Errorf("bind failed: %w", err) // go-ldap error carries no password
	}
	// One search over all the session's uids: (&(objectClass=person)(|(uid=a)(uid=b)…)). Each uid is
	// EscapeFilter'd — the actor came from a parsed log line and is untrusted input to the filter.
	var b strings.Builder
	b.WriteString("(&(objectClass=person)(|")
	for _, u := range uids {
		b.WriteString("(")
		b.WriteString(attrUID)
		b.WriteString("=")
		b.WriteString(ldapv3.EscapeFilter(u))
		b.WriteString(")")
	}
	b.WriteString("))")
	req := ldapv3.NewSearchRequest(
		r.userBase, ldapv3.ScopeWholeSubtree, ldapv3.NeverDerefAliases,
		r.sizeLimit, r.timeLimit, false,
		b.String(),
		[]string{attrUID, attrAccountLock, attrKrbExpiry, attrMemberOf, attrObjectClass},
		nil,
	)
	res, err := c.Search(req)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	return res.Entries, nil
}

// classify buckets each matched directory entry into facts.Confirmed / facts.Disabled per REQ-2317/2318.
// Precedence: DISABLED dominates — a locked/expired account is never also "confirmed" (a disabled admin
// credential in use is the compromise case). Confirmation requires enabled AND non-service AND membership in
// a configured sanctioned group. An entry that is neither stays in NEITHER bucket (the fail-open default).
func classify(entries []*ldapv3.Entry, uidToActors map[string][]string, sanctionedGroups []string, facts *actorevidence.SanctionFacts) {
	groupSet := lowerSet(sanctionedGroups)
	now := time.Now()
	for _, e := range entries {
		uid := strings.TrimSpace(e.GetAttributeValue(attrUID))
		orig := uidToActors[uid] // the original actor form(s) that produced this uid
		if uid == "" || len(orig) == 0 {
			continue
		}
		if accountDisabled(e, now) {
			facts.Disabled = append(facts.Disabled, orig...)
			continue
		}
		if isService(e) {
			continue // a service account is never live-promotable (REQ-2317)
		}
		if len(groupSet) > 0 && memberOfAny(e.GetAttributeValues(attrMemberOf), groupSet) {
			facts.Confirmed = append(facts.Confirmed, orig...)
		}
	}
}

// accountDisabled reports whether a fresh directory fact affirmatively shows the account locked, disabled, or
// expired (REQ-2318). nsAccountLock=TRUE is FreeIPA's disable flag; a krbPrincipalExpiration in the past is an
// expired principal. Absence of both is NOT "disabled" (fail open — absence is not a positive fact).
func accountDisabled(e *ldapv3.Entry, now time.Time) bool {
	if strings.EqualFold(strings.TrimSpace(e.GetAttributeValue(attrAccountLock)), "TRUE") {
		return true
	}
	if exp := strings.TrimSpace(e.GetAttributeValue(attrKrbExpiry)); exp != "" {
		if t, ok := parseKrbTime(exp); ok && t.Before(now) {
			return true
		}
	}
	return false
}

// isService reports whether the entry is a service principal (objectClass ipaservice). FreeIPA "system user"
// person entries are NOT flagged here (they are regular person objects) — the conservative Phase-2 scope; a
// naming-convention service demotion is a deferred refinement.
func isService(e *ldapv3.Entry) bool {
	for _, oc := range e.GetAttributeValues(attrObjectClass) {
		if strings.EqualFold(strings.TrimSpace(oc), "ipaservice") {
			return true
		}
	}
	return false
}

// memberOfAny reports whether any memberOf DN's leading cn is in the sanctioned-group set. memberOf values are
// DNs like "cn=admins,cn=groups,cn=accounts,…"; the leading cn is the group name.
func memberOfAny(memberOf []string, groupSet map[string]bool) bool {
	for _, dn := range memberOf {
		if cn := leadingCN(dn); cn != "" && groupSet[cn] {
			return true
		}
	}
	return false
}

// leadingCN extracts the lowercased value of the first cn= RDN of a DN ("cn=admins,cn=groups,…" → "admins"),
// using the go-ldap DN parser so escaping is handled correctly.
func leadingCN(dn string) string {
	parsed, err := ldapv3.ParseDN(dn)
	if err != nil {
		return ""
	}
	for _, rdn := range parsed.RDNs {
		for _, ava := range rdn.Attributes {
			if strings.EqualFold(ava.Type, "cn") {
				return strings.ToLower(strings.TrimSpace(ava.Value))
			}
		}
	}
	return ""
}

// parseKrbTime parses a Kerberos generalized time ("20260101000000Z") or an RFC3339 time.
func parseKrbTime(s string) (time.Time, bool) {
	for _, layout := range []string{"20060102150405Z", time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// realDial dials one server with mandatory CA-verified TLS (mirrors modules/credsource/ldap.realDial). LDAPS
// verifies at dial; a plain ldap:// is upgraded with StartTLS. TLS is NEVER skipped — a verify failure fails
// closed and triggers failover to the next replica.
func (r *Resolver) realDial(ctx context.Context, url string) (Conn, error) {
	tc, err := r.tlsConfig(hostFromURL(url))
	if err != nil {
		return nil, err
	}
	d := &net.Dialer{Timeout: dialTimeout}
	var opts []ldapv3.DialOpt
	if strings.HasPrefix(url, "ldaps://") {
		opts = append(opts, ldapv3.DialWithTLSDialer(tc, d))
	} else {
		opts = append(opts, ldapv3.DialWithDialer(d))
	}
	l, err := ldapv3.DialURL(url, opts...)
	if err != nil {
		return nil, err // unreachable or TLS-verify failure → fail closed → failover
	}
	if strings.HasPrefix(url, "ldap://") {
		if err := l.StartTLS(tc); err != nil {
			l.Close()
			return nil, fmt.Errorf("starttls: %w", err)
		}
	}
	l.SetTimeout(opTimeout)
	return l, nil
}

// tlsConfig builds a TLS config verifying the server against the CA PEM (RootCAs); TLS is never skipped.
func (r *Resolver) tlsConfig(serverName string) (*tls.Config, error) {
	tc := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: serverName}
	if strings.TrimSpace(string(r.caCertRef)) != "" {
		pem, err := r.caCertRef.Resolve()
		if err != nil {
			return nil, fmt.Errorf("ldapident: resolve CA cert ref: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(pem)) {
			return nil, fmt.Errorf("ldapident: CA cert ref contains no valid certificate")
		}
		tc.RootCAs = pool
	}
	return tc, nil
}

// actorToUID reduces a domain-native actor to a directory uid: "kp@SEC.REALM" → "kp"; "kp" → "kp". A
// numeric "uid:N" fallback form (from a journal record that named no invoker) resolves to no directory uid.
func actorToUID(actor string) string {
	a := strings.TrimSpace(actor)
	if a == "" || strings.HasPrefix(a, "uid:") {
		return ""
	}
	if i := strings.IndexByte(a, '@'); i > 0 {
		a = a[:i]
	}
	return a
}

func hostFromURL(u string) string {
	u = strings.TrimPrefix(strings.TrimPrefix(u, "ldaps://"), "ldap://")
	if i := strings.IndexByte(u, ':'); i >= 0 {
		u = u[:i]
	}
	return u
}

func lowerSet(xs []string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		if x = strings.ToLower(strings.TrimSpace(x)); x != "" {
			m[x] = true
		}
	}
	return m
}

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
