// Package httpfailover is a RoundTripper that fails a request over between the members of ONE logical API
// served identically by several hosts — a Proxmox VE cluster, whose /api2/json answers from any quorate
// member (TG-572).
//
// WHY IT EXISTS. Every Proxmox reader TG ran (pve-liveness, the estate refresh, the actor-attribution reader,
// confighash) and the actuation leaf were configured with ONE node's API URL. When dc1pve01 died on
// 2026-09-30 every one of them went blind for nine days — guest_liveness wrote nothing after 10-04 and an
// autonomous heal was refused — while four other members of the same cluster answered normally. The cluster
// was never down; TG's view of it was a single point of failure.
//
// SEMANTICS, deliberately narrow:
//   - Only requests whose scheme+host is a MEMBER of the group are touched; everything else passes through
//     byte-identical.
//   - Only a TRANSPORT failure fails over (the request never produced an HTTP response: dial refused, no route,
//     TLS failure, timeout before headers). Any HTTP response — 401, 500, 595 — is the cluster's answer and is
//     returned as-is; failing over on a status code would turn an authorization refusal into a retry storm.
//   - A request with a body is retried only when the body can be replayed (GetBody). Proxmox lifecycle POSTs
//     carry no body; a non-replayable body is never re-sent.
//   - STICKY: the member that last answered is tried first, so a dead primary costs one failed dial per
//     process, not one per request.
//   - The request is re-addressed to the member (URL.Host); path, query, method and headers (including the
//     cluster-wide API token) are unchanged — a PVE API token is valid on every member.
package httpfailover

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// Transport fails a group member's request over to the other members on a transport error.
type Transport struct {
	base    http.RoundTripper
	members []string        // scheme://host:port, in preference order
	inGroup map[string]bool // same keys
	logf    func(string, ...any)

	mu        sync.Mutex
	preferred int // index into members: the last member that answered
}

// New wraps base for the given member base URLs (e.g. "https://dc1pve04.example.net:8006"). Members are
// normalized to scheme://host:port; duplicates and unparsable entries are dropped. Fewer than two members ⇒
// the returned Transport is a pure pass-through.
func New(base http.RoundTripper, memberURLs []string, logf func(string, ...any)) *Transport {
	if base == nil {
		base = http.DefaultTransport
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	t := &Transport{base: base, inGroup: map[string]bool{}, logf: logf}
	for _, raw := range memberURLs {
		if k, ok := key(raw); ok && !t.inGroup[k] {
			t.inGroup[k] = true
			t.members = append(t.members, k)
		}
	}
	return t
}

// Members returns the normalized group, in preference order at construction.
func (t *Transport) Members() []string { return append([]string(nil), t.members...) }

func key(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", false
	}
	host := u.Host
	if u.Port() == "" {
		switch strings.ToLower(u.Scheme) {
		case "https":
			host += ":443"
		case "http":
			host += ":80"
		}
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(host), true
}

// RoundTrip implements http.RoundTripper.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	k, ok := key(req.URL.Scheme + "://" + req.URL.Host)
	if !ok || !t.inGroup[k] || len(t.members) < 2 {
		return t.base.RoundTrip(req)
	}
	replayable := req.Body == nil || req.Body == http.NoBody || req.GetBody != nil

	t.mu.Lock()
	start := t.preferred
	t.mu.Unlock()

	var firstErr error
	for i := 0; i < len(t.members); i++ {
		idx := (start + i) % len(t.members)
		m := t.members[idx]
		r, err := readdress(req, m, i > 0)
		if err != nil {
			return nil, err
		}
		resp, rerr := t.base.RoundTrip(r)
		if rerr == nil {
			t.mu.Lock()
			if t.preferred != idx {
				t.logf("pve failover (TG-572): %s answered after %s failed — now preferred for %s (%v)",
					m, t.members[t.preferred], k, firstErr)
				t.preferred = idx
			}
			t.mu.Unlock()
			return resp, nil
		}
		if firstErr == nil {
			firstErr = rerr
		}
		if req.Context().Err() != nil || !replayable {
			return nil, rerr // the caller gave up, or the body cannot be re-sent: never retry
		}
	}
	return nil, fmt.Errorf("httpfailover: every member of %v failed (first error: %w)", t.members, firstErr)
}

// readdress clones req onto member m. The original request is never mutated (RoundTripper contract).
func readdress(req *http.Request, m string, replay bool) (*http.Request, error) {
	u, _ := url.Parse(m) // m came from key(), always parsable
	r := req.Clone(req.Context())
	r.URL.Scheme, r.URL.Host = u.Scheme, u.Host
	r.Host = "" // derive the Host header (and TLS SNI) from the member
	if replay && req.GetBody != nil {
		b, err := req.GetBody()
		if err != nil {
			return nil, err
		}
		r.Body = b
	}
	return r, nil
}
