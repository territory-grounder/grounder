package egress

// meter.go — the outbound destination/volume meter (TG-160 deliverable 3).
//
// It is an http.RoundTripper wrapper, and it is installed ONCE at worker boot over http.DefaultTransport
// (cmd/worker/main.go). That placement is the whole reason it is worth building: measured on this tree,
// 20+ modules construct their client as `http.DefaultClient` or `&http.Client{Timeout: …}` with no
// Transport of their own — matrix, netbox, pve, youtrack, jira, slack, teams, mattermost, servicenow,
// github-issues, twilio, librenms, awx, semaphore, vault, oidctoken, awxjob, awxplaybooks, cronicle, the
// seal transit client and the litellm model gateway all resolve to http.DefaultTransport at call time. So
// one wrap covers essentially the entire outbound HTTP surface of the process, and — critically — it
// covers modules that DO NOT EXIST YET, because a future connector written the same idiomatic way is
// metered on the day it is armed. A per-module hook would have to be remembered 20+ times and would be
// forgotten once, which is how "the control was built" and "the control ran" came apart everywhere else
// in this repo.
//
// WHAT IT DOES NOT COVER, stated here rather than discovered later:
//   - a client that sets its OWN Transport bypasses this. In-tree that is exactly one: the opt-in
//     insecure estate poller (cmd/worker/estateHTTPClient), which is wrapped explicitly at construction.
//   - non-HTTP egress: SSH actuation (crypto/ssh), the Temporal gRPC client, LDAP, and DNS. Those are
//     network-layer concerns and the compose/NetworkPolicy split is what covers them.
//   - the litellm CONTAINER's own calls to model providers. TG's process talks to litellm; litellm talks
//     to Anthropic/OpenAI/… That hop is metered by nothing here and is bounded only at the network layer.

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync"
)

// Mode selects what the meter does with an off-allowlist destination.
type Mode string

const (
	// ModeMeter counts and logs; it never blocks. THE DEFAULT, and deliberately so: a wrong allowlist
	// under enforcement takes production off the network, and this allowlist is derived from environment
	// configuration that no one has yet audited against reality. Meter first; block once the off-allowlist
	// series has been flat at zero for long enough to trust it.
	ModeMeter Mode = "meter"
	// ModeEnforce refuses an off-allowlist request with a typed error before it is dialled. Opt-in.
	ModeEnforce Mode = "enforce"
)

// destinationCap bounds the number of DISTINCT off-allowlist hosts held for exposition. Past the cap,
// hosts fold into the single bucket "other". Without a cap an attacker (or a bug) could mint unbounded
// metric label values by walking hostnames — turning an exfil meter into a memory-and-TSDB amplifier.
// core/metrics/families.go states this rule for the whole platform; this honours it.
const destinationCap = 32

// overflowHost is the fold-to bucket once destinationCap distinct off-allowlist hosts have been seen.
const overflowHost = "other"

// RefusalError is returned by RoundTrip under ModeEnforce. It names the destination and the fact that the
// destination was never declared — never a bare "connection refused", because the failure this produces
// must be diagnosable from the error text alone by whoever is paged at 03:00.
type RefusalError struct {
	Host string
}

func (e *RefusalError) Error() string {
	return fmt.Sprintf("egress refused: destination %q is not on the declared outbound allowlist "+
		"(TG-160 enforce mode). Declare it via the module's endpoint configuration or TG_EGRESS_ALLOW, "+
		"or set TG_EGRESS_MODE=meter to observe instead of block.", e.Host)
}

// destStat is one destination's running tally.
type destStat struct {
	Requests uint64
	BytesOut uint64
	BytesIn  uint64
}

// Meter wraps an http.RoundTripper and records where the bytes went.
type Meter struct {
	next  http.RoundTripper
	allow *Allowlist
	mode  Mode
	logf  func(string, ...any)

	mu       sync.Mutex
	total    map[string]*destStat // rule ("loopback", "netbox.example", "*.example.org") → tally
	off      map[string]*destStat // off-allowlist HOST → tally, capped at destinationCap
	refusals uint64
	// announced tracks which off-allowlist hosts have already produced a log line, so a beaconing
	// process logs each NEW destination once instead of drowning the journal (and the operator) in the
	// same host at poll frequency. The count keeps rising in the metric either way.
	announced map[string]bool
}

// Option configures a Meter.
type Option func(*Meter)

// WithMode selects meter (default) or enforce.
func WithMode(m Mode) Option {
	return func(x *Meter) {
		if m == ModeEnforce {
			x.mode = ModeEnforce
		}
	}
}

// WithLogger installs the log sink for first-sighting-of-a-new-destination lines. nil disables logging.
func WithLogger(f func(string, ...any)) Option { return func(x *Meter) { x.logf = f } }

// NewMeter wraps next. A nil next means http.DefaultTransport, and a nil allowlist means an EMPTY
// allowlist — which permits nothing and therefore flags everything, the visible direction.
func NewMeter(next http.RoundTripper, allow *Allowlist, opts ...Option) *Meter {
	if next == nil {
		next = http.DefaultTransport
	}
	if allow == nil {
		allow = NewAllowlist(nil)
	}
	m := &Meter{
		next: next, allow: allow, mode: ModeMeter,
		total: map[string]*destStat{}, off: map[string]*destStat{}, announced: map[string]bool{},
	}
	for _, o := range opts {
		o(m)
	}
	return m
}

// Mode reports the meter's posture (published as tg_egress_enforcing).
func (m *Meter) Mode() Mode { return m.mode }

// Allowlist exposes the compiled declaration (for the rules-count gauge and for boot logging).
func (m *Meter) Allowlist() *Allowlist { return m.allow }

// RoundTrip meters one outbound request. It NEVER changes the response, and under ModeMeter it never
// changes whether the request happens — so installing it cannot alter behaviour, which is the property
// that makes it safe to turn on everywhere at once.
func (m *Meter) RoundTrip(req *http.Request) (*http.Response, error) {
	return m.roundTripVia(m.next, req)
}

// Wrap returns a RoundTripper that meters into THIS meter but dials through next. It exists for the one
// in-tree client that installs its OWN Transport — the opt-in insecure estate poller
// (cmd/worker/estateHTTPClient) — because the single client most likely to be pointed somewhere
// unexpected must not be the one client the meter cannot see. Tallies are shared, so a snapshot covers
// both paths.
func (m *Meter) Wrap(next http.RoundTripper) http.RoundTripper {
	if next == nil {
		next = http.DefaultTransport
	}
	return &wrapped{m: m, next: next}
}

type wrapped struct {
	m    *Meter
	next http.RoundTripper
}

func (w *wrapped) RoundTrip(r *http.Request) (*http.Response, error) {
	return w.m.roundTripVia(w.next, r)
}

func (m *Meter) roundTripVia(next http.RoundTripper, req *http.Request) (*http.Response, error) {
	host := ""
	if req.URL != nil {
		host = normaliseHost(req.URL.Hostname())
	}
	rule, permitted := m.allow.Permits(host)

	if !permitted && m.mode == ModeEnforce {
		m.recordOff(host, 0, 0)
		m.mu.Lock()
		m.refusals++
		m.mu.Unlock()
		return nil, &RefusalError{Host: host}
	}

	// Bytes OUT. ContentLength is authoritative when the caller set it (every JSON post in this tree
	// does, via bytes.Reader); when it is unknown (-1, a streamed body) the body is wrapped and counted
	// as it is consumed by the transport. Header bytes are not counted: the signal wanted here is payload
	// volume, and a fixed per-request header overhead only adds noise to it.
	var out uint64
	if req.ContentLength > 0 {
		out = uint64(req.ContentLength)
	}
	var streamed *countingReadCloser
	if req.ContentLength < 0 && req.Body != nil {
		streamed = &countingReadCloser{rc: req.Body}
		req = req.Clone(req.Context())
		req.Body = streamed
	}

	resp, err := next.RoundTrip(req)

	if streamed != nil {
		out += streamed.count()
	}
	// THE REQUEST IS RECORDED NOW, not when the body closes. Bytes IN are added later, on Close.
	//
	// The obvious shape — record everything in one go from the body's Close — silently loses any call
	// whose body the caller never closes, and it loses it from the REQUEST count as well as the byte
	// count. That is the wrong direction for this control: a leaked body would make an outbound call
	// disappear from tg_egress_offallowlist_requests_total entirely, so the exfil signal would be
	// suppressed by the same sloppiness that is common in error paths. Counting the request eagerly means
	// the worst a leaked body can cost is an undercount of RESPONSE bytes, which is not the exfil
	// direction (bytes OUT is), and the destination still appears and is still named in the log.
	m.record(host, rule, permitted, out, 0)
	if err != nil || resp == nil {
		return resp, err
	}
	// Bytes IN are counted as the caller drains the body, so a large response is attributed even though
	// RoundTrip returns before it is read. The wrapper is transparent: same Close, same errors.
	counted := &countingReadCloser{rc: resp.Body, onClose: func(n uint64) { m.addBytesIn(host, rule, permitted, n) }}
	resp.Body = counted
	return resp, nil
}

func (m *Meter) record(host, rule string, permitted bool, out, in uint64) {
	if permitted {
		m.mu.Lock()
		bump(m.total, rule, out, in)
		m.mu.Unlock()
		return
	}
	m.recordOff(host, out, in)
}

// addBytesIn attributes response bytes to an ALREADY-COUNTED request. It never increments Requests — the
// request was counted when it was made — so a response body cannot inflate the destination count.
func (m *Meter) addBytesIn(host, rule string, permitted bool, in uint64) {
	if in == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if permitted {
		if s := m.total[rule]; s != nil {
			s.BytesIn += in
		}
		return
	}
	if s := m.total[string(offAllowlistRule)]; s != nil {
		s.BytesIn += in
	}
	if s := m.off[m.offKeyLocked(host)]; s != nil {
		s.BytesIn += in
	}
}

// offKeyLocked resolves the (capped) exposition key for an off-allowlist host. Caller holds mu.
func (m *Meter) offKeyLocked(host string) string {
	if host == "" {
		host = "unknown"
	}
	if _, seen := m.off[host]; !seen && len(m.off) >= destinationCap {
		return overflowHost
	}
	return host
}

func (m *Meter) recordOff(host string, out, in uint64) {
	if host == "" {
		host = "unknown"
	}
	m.mu.Lock()
	bump(m.total, string(offAllowlistRule), out, in)
	bump(m.off, m.offKeyLocked(host), out, in)
	first := !m.announced[host]
	if first {
		m.announced[host] = true
	}
	logf := m.logf
	m.mu.Unlock()
	if first && logf != nil {
		// The one line an operator greps for. It names the destination because a count without a name
		// cannot be acted on, and TG-160's whole complaint is that an off-allowlist connection was not
		// VISIBLE. No payload, no header and no credential is emitted — only the host.
		logf("EGRESS OFF-ALLOWLIST (TG-160): outbound to %q, which is not among the %d declared "+
			"destinations. Metering only — nothing was blocked. If this is legitimate, declare it "+
			"(module endpoint config or TG_EGRESS_ALLOW); if it is not, this is the exfil signal.",
			host, m.allow.Size())
	}
}

// offAllowlistRule is the rule label used for every off-allowlist request in the aggregate family. The
// per-host detail lives in the (capped) destinations family; this one stays a bounded enum.
const offAllowlistRule = "off_allowlist"

func bump(m map[string]*destStat, key string, out, in uint64) {
	s := m[key]
	if s == nil {
		s = &destStat{}
		m[key] = s
	}
	s.Requests++
	s.BytesOut += out
	s.BytesIn += in
}

// Destination is one row of the meter's snapshot.
type Destination struct {
	// Rule is the allowlist rule that admitted the traffic, or "off_allowlist" in the aggregate family,
	// or the off-allowlist HOST in the per-destination family (capped, overflow folded to "other").
	Rule     string
	Requests uint64
	BytesOut uint64
	BytesIn  uint64
}

// Snapshot is the meter's readable state. Everything on it is a count; no payload is retained anywhere in
// this package, so no snapshot can ever leak content.
type Snapshot struct {
	// ByRule is the aggregate family, keyed by allowlist rule plus the single "off_allowlist" bucket.
	ByRule []Destination
	// OffAllowlist is the per-host detail for undeclared destinations, sorted, capped at destinationCap.
	OffAllowlist []Destination
	// Requests / BytesOut / BytesIn are the totals across every destination.
	Requests, BytesOut, BytesIn uint64
	// OffRequests / OffBytesOut are the exfil-shaped totals: how much left for somewhere undeclared.
	OffRequests, OffBytesOut uint64
	// Refusals counts requests blocked under ModeEnforce (always 0 under ModeMeter).
	Refusals uint64
	// AllowlistRules is len(allowlist). A ZERO here with non-zero Requests means the meter is comparing
	// traffic against an empty declaration — the vacuity condition, readable live.
	AllowlistRules int
	// Enforcing reports the posture.
	Enforcing bool
}

// Snapshot reads the meter. Safe to call concurrently with traffic.
func (m *Meter) Snapshot() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := Snapshot{AllowlistRules: m.allow.Size(), Enforcing: m.mode == ModeEnforce, Refusals: m.refusals}
	for rule, st := range m.total {
		s.ByRule = append(s.ByRule, Destination{Rule: rule, Requests: st.Requests, BytesOut: st.BytesOut, BytesIn: st.BytesIn})
		s.Requests += st.Requests
		s.BytesOut += st.BytesOut
		s.BytesIn += st.BytesIn
		if rule == string(offAllowlistRule) {
			s.OffRequests = st.Requests
			s.OffBytesOut = st.BytesOut
		}
	}
	for host, st := range m.off {
		s.OffAllowlist = append(s.OffAllowlist, Destination{Rule: host, Requests: st.Requests, BytesOut: st.BytesOut, BytesIn: st.BytesIn})
	}
	sort.Slice(s.ByRule, func(i, j int) bool { return s.ByRule[i].Rule < s.ByRule[j].Rule })
	sort.Slice(s.OffAllowlist, func(i, j int) bool { return s.OffAllowlist[i].Rule < s.OffAllowlist[j].Rule })
	return s
}

// countingReadCloser counts bytes as they flow and reports the total exactly once, on Close.
type countingReadCloser struct {
	rc      io.ReadCloser
	onClose func(uint64)
	mu      sync.Mutex
	n       uint64
	closed  bool
}

func (c *countingReadCloser) Read(p []byte) (int, error) {
	n, err := c.rc.Read(p)
	if n > 0 {
		c.mu.Lock()
		c.n += uint64(n)
		c.mu.Unlock()
	}
	return n, err
}

func (c *countingReadCloser) Close() error {
	c.mu.Lock()
	already, n := c.closed, c.n
	c.closed = true
	c.mu.Unlock()
	err := c.rc.Close()
	if !already && c.onClose != nil {
		c.onClose(n)
	}
	return err
}

func (c *countingReadCloser) count() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}
