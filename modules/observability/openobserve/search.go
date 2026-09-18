// This file is the OpenObserve READ client — the query half of the connector, sibling to the exporter's
// write half (openobserve.go). It backs the agent's `correlate-logs` tool (correlate.go): the estate-wide,
// cross-host log correlation the per-host syslog-ng point tools (modules/observability/syslogng) cannot do,
// because a single firewall emits ~131 MB/day and TG must never become a raw-syslog destination — the logs
// are shipped to OpenObserve and this client queries the INDEX, never a raw file (TG-39).
//
// It reuses the exporter's transport contract exactly: the same Doer seam (so an oracle drives the real
// request-building path against a fake endpoint), the same base-URL convention (TG_OPENOBSERVE_URL already
// carries /api/<org>, so the search route is <endpoint>/_search just as ingest is <endpoint>/v1/logs), and
// the same HTTP Basic authentication with the base64 ingest credential presented verbatim — a bare Bearer
// 401s on OpenObserve (openobserve.go). The credential is a secret REFERENCE resolved per request (INV-13).
//
// EVERY read is bounded (INV-08): a per-query hit cap and byte cap, a hard-capped result size in the request
// body itself (so OpenObserve never streams an unbounded page across the wire), and a context deadline. A
// read that cannot be served returns an ERROR the tool renders as an honest "could not read", never a
// fabricated empty result — "no matching lines" and "the read failed" must never be the same answer to a
// triage agent that would otherwise conclude the fault is not in the logs.
//
// Provenance: [O] INV-08/INV-13, spec/008 REQ-818, TG-39.
package openobserve

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/territory-grounder/grounder/core/config"
)

const (
	// searchPath is the OpenObserve SQL search route, relative to the /api/<org> endpoint the module already
	// carries (the same prefix ingest posts under). Grounded in the OpenObserve Search API: POST
	// /api/{org}/_search with a JSON body {query:{sql,start_time,end_time,from,size}}.
	searchPath = "/_search"

	// DefaultLogStream is the OpenObserve stream the syslog-ng streams are shipped into when no stream is
	// configured. It is a DEFAULT, not a hardcode: WithStream overrides it from TG_OPENOBSERVE_LOG_STREAM so
	// an operator whose pipeline ships elsewhere is not silently querying an empty stream — the search
	// self-test (SelfTest) fails visibly on a stream that does not exist, which is the empty-vs-broken trap
	// this default would otherwise spring.
	DefaultLogStream = "syslog"

	// DefaultHostField is the stream field carrying the device host each log line came from — the column the
	// correlation filters on and attributes results by. Configurable via WithHostField
	// (TG_OPENOBSERVE_HOST_FIELD) because the field name is a property of the shipping pipeline, not of TG.
	DefaultHostField = "host"

	// timestampField is OpenObserve's built-in per-record ingest-time column (epoch microseconds). Results
	// are ordered newest-first by it, and it is rendered per line so a correlated cascade reads in time order.
	timestampField = "_timestamp"

	// searchMaxHits is the hard per-query hit cap: the largest `size` this client will ever ask OpenObserve
	// for, regardless of what a caller requests. It mirrors the syslog-ng search cap so the two log tiers
	// bound a single investigation the same way.
	searchMaxHits = 500

	// searchMaxBytes caps the response body READ off the wire, in case a proxy or a wrong host answers with an
	// unbounded stream — a second bound under the request-body size cap.
	searchMaxBytes = 1 << 20 // 1 MiB

	// searchTimeout bounds one _search when the caller's context carries no deadline of its own.
	searchTimeout = 20 * time.Second
)

// Reader is the read-only OpenObserve search client. Construct with NewReader. It holds NO write path — the
// only route it speaks is _search — and it is a distinct type from the exporter Module so a read can never
// be built from, or accidentally reach, an ingest route.
type Reader struct {
	endpoint  string
	tokenRef  config.SecretRef
	http      Doer
	stream    string
	hostField string
	timeout   time.Duration
	now       func() time.Time
}

// ReaderOption configures a Reader without breaking the two-argument constructor.
type ReaderOption func(*Reader)

// WithReaderHTTPClient injects the HTTP transport (a fake endpoint in tests, *http.Client in production).
func WithReaderHTTPClient(d Doer) ReaderOption { return func(r *Reader) { r.http = d } }

// WithStream sets the OpenObserve stream the correlation queries. A blank value leaves DefaultLogStream in
// place — an operator who blanks the key gets the sane default back, never an empty stream name that would
// build `FROM ""` and error every query.
func WithStream(name string) ReaderOption {
	return func(r *Reader) {
		if s := sanitizeIdent(name); s != "" {
			r.stream = s
		}
	}
}

// WithHostField sets the stream field the correlation filters and attributes by. Blank ⇒ DefaultHostField.
func WithHostField(field string) ReaderOption {
	return func(r *Reader) {
		if f := sanitizeIdent(field); f != "" {
			r.hostField = f
		}
	}
}

// NewReader builds the read client for an OpenObserve /api/<org> base URL and an ingest-token secret
// reference. An empty endpoint yields NIL — the composition root only reaches this with a configured
// TG_OPENOBSERVE_URL, and a nil Reader makes the correlate tool structurally absent (registration-gated,
// exactly like the exporter and the syslog-ng tools) rather than a live tool that errors on every call.
func NewReader(endpoint string, tokenRef config.SecretRef, opts ...ReaderOption) *Reader {
	if strings.TrimSpace(endpoint) == "" {
		return nil
	}
	r := &Reader{
		endpoint:  strings.TrimRight(strings.TrimSpace(endpoint), "/"),
		tokenRef:  tokenRef,
		http:      http.DefaultClient,
		stream:    DefaultLogStream,
		hostField: DefaultHostField,
		timeout:   searchTimeout,
		now:       func() time.Time { return time.Now().UTC() },
	}
	for _, o := range opts {
		o(r)
	}
	return r
}

// Stream and HostField expose the resolved configuration so the tool can name them in its operator- and
// agent-facing text (a correlation that found nothing must be able to say WHICH stream it searched).
func (r *Reader) Stream() string    { return r.stream }
func (r *Reader) HostField() string { return r.hostField }

// CorrelationQuery is one bounded cross-host search: a host set, a time window (epoch microseconds), and
// optional free-text refinements. The Reader builds the SQL from it with injection-safe quoting — a caller
// never hands raw SQL — so an alert-derived Pattern cannot escape the string literal it lands in.
type CorrelationQuery struct {
	Hosts       []string // device host names to correlate across (validated by the caller; escaped here too)
	StartMicros int64    // window start, epoch microseconds
	EndMicros   int64    // window end, epoch microseconds
	Pattern     string   // optional full-text refinement (an error code, an interface, a peer) — may be ""
	Severity    string   // optional severity/level refinement — may be ""
	Size        int      // requested hit count; hard-capped at searchMaxHits
}

// LogHit is one decoded, host-attributed log record. Fields carries the raw record so the tool can render
// whatever the stream holds; Host and TimestampMicros are lifted from it for attribution and ordering.
type LogHit struct {
	Host            string
	TimestampMicros int64
	Fields          map[string]any
}

// SearchResult is the bounded outcome of one _search: the decoded hits, the vendor-reported total (which may
// exceed len(Hits) when the size cap truncated), and whether this client capped the returned set.
type SearchResult struct {
	Hits      []LogHit
	Total     int
	Truncated bool
}

// searchQuery/searchRequest/searchResponse mirror the OpenObserve Search API wire shape exactly, so the
// oracle decodes the real bytes and pins the timestamp UNIT (microseconds) and the route, not a paraphrase.
type searchQuery struct {
	SQL       string `json:"sql"`
	StartTime int64  `json:"start_time"` // epoch MICROSECONDS
	EndTime   int64  `json:"end_time"`   // epoch MICROSECONDS
	From      int    `json:"from"`
	Size      int    `json:"size"`
}

type searchRequest struct {
	Query      searchQuery `json:"query"`
	SearchType string      `json:"search_type,omitempty"`
}

type searchResponse struct {
	Took  int              `json:"took"`
	Hits  []map[string]any `json:"hits"`
	Total int              `json:"total"`
}

// Correlate runs one bounded cross-host search and returns the host-attributed hits. It FAILS CLOSED: a
// transport failure, a non-2xx status, or an undecodable body is an error — never an empty SearchResult that
// a caller could misread as "no logs". An empty result is reserved for the one case it truly means: a 2xx
// answer whose hit list is empty.
func (r *Reader) Correlate(ctx context.Context, q CorrelationQuery) (SearchResult, error) {
	if r == nil {
		return SearchResult{}, fmt.Errorf("openobserve: nil search reader")
	}
	if len(q.Hosts) == 0 {
		// Never send `IN ()` — DataFusion rejects it, and an unbounded `SELECT *` with no host filter would
		// pull the whole stream. A caller with no hosts has nothing to correlate; that is not a read failure.
		return SearchResult{}, fmt.Errorf("openobserve: correlation query has no hosts to search")
	}

	size := q.Size
	if size < 1 || size > searchMaxHits {
		size = searchMaxHits
	}
	// Ask for one MORE than the cap so the vendor `total` and our own truncation flag agree even when the
	// stream holds exactly `size` matches: if the server returns more than `size`, we know we truncated
	// without trusting `total` (which OpenObserve computes differently across versions).
	reqSize := size
	sql := buildCorrelationSQL(r.stream, r.hostField, q.Hosts, q.Pattern, q.Severity)

	var resp searchResponse
	if err := r.post(ctx, searchRequest{
		Query: searchQuery{
			SQL:       sql,
			StartTime: q.StartMicros,
			EndTime:   q.EndMicros,
			From:      0,
			Size:      reqSize,
		},
		SearchType: "ui",
	}, &resp); err != nil {
		return SearchResult{}, err
	}

	out := SearchResult{Total: resp.Total}
	for _, raw := range resp.Hits {
		if len(out.Hits) >= size {
			out.Truncated = true
			break
		}
		out.Hits = append(out.Hits, LogHit{
			Host:            hostOfHit(raw, r.hostField),
			TimestampMicros: microsOfHit(raw),
			Fields:          raw,
		})
	}
	return out, nil
}

// post ships a search body to <endpoint>/_search under HTTP Basic auth (the exporter's exact construction)
// and decodes a 2xx JSON body into out. A non-2xx is an error carrying the status — never silently dropped.
func (r *Reader) post(ctx context.Context, body searchRequest, out *searchResponse) error {
	token, err := r.tokenRef.Resolve()
	if err != nil {
		return fmt.Errorf("openobserve: resolve search token: %w", err)
	}
	if strings.TrimSpace(token) == "" {
		return fmt.Errorf("openobserve: search token reference resolved empty (fail closed)")
	}
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	timeout := r.timeout
	if timeout <= 0 {
		timeout = searchTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(cctx, http.MethodPost, r.endpoint+searchPath, bytes.NewReader(b))
	if err != nil {
		return err
	}
	// HTTP Basic with the already-base64 ingest credential presented verbatim — NOT a Bearer (a bare Bearer
	// 401s on OpenObserve). The same construction the exporter's post() uses (openobserve.go).
	req.Header.Set("Authorization", "Basic "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := r.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, searchMaxBytes))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("openobserve: search POST %s: status %d: %s", searchPath, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if err := json.Unmarshal(raw, out); err != nil {
		// A 2xx whose body is not an OpenObserve search reply is a wrong-host / proxy answer — an error, not
		// an empty result, for exactly the reason a fabricated empty is dangerous here.
		return fmt.Errorf("openobserve: search reply was not decodable JSON (endpoint may not be OpenObserve): %w", err)
	}
	return nil
}

// ---- injection-safe SQL construction ----

// buildCorrelationSQL renders the correlation query. EVERY value interpolated into the SQL is either a
// double-quoted identifier (stream, host field, timestamp column — all sanitised to an identifier charset at
// construction) or a single-quoted string literal with its single quotes doubled (the host names and the
// alert-derived Pattern/Severity). DataFusion — OpenObserve's SQL engine — follows standard SQL string
// literals, where the ONLY in-literal metacharacter is the single quote and backslash is not an escape, so
// doubling the quote is the complete and correct escaping. This is the last line against a log-search query
// built from an alert field being turned into query injection (TG-39): a Pattern of `x') OR ('1'='1` becomes
// the harmless literal `'x”) OR (”1”=”1'`.
func buildCorrelationSQL(stream, hostField string, hosts []string, pattern, severity string) string {
	var b strings.Builder
	b.WriteString("SELECT * FROM ")
	b.WriteString(sqlIdent(stream))
	b.WriteString(" WHERE ")
	b.WriteString(sqlIdent(hostField))
	b.WriteString(" IN (")
	for i, h := range hosts {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(sqlStringLiteral(h))
	}
	b.WriteByte(')')
	if strings.TrimSpace(pattern) != "" {
		b.WriteString(" AND match_all(")
		b.WriteString(sqlStringLiteral(pattern))
		b.WriteByte(')')
	}
	if strings.TrimSpace(severity) != "" {
		b.WriteString(" AND match_all(")
		b.WriteString(sqlStringLiteral(severity))
		b.WriteByte(')')
	}
	b.WriteString(" ORDER BY ")
	b.WriteString(sqlIdent(timestampField))
	b.WriteString(" DESC")
	return b.String()
}

// sqlStringLiteral renders s as a safe single-quoted SQL string literal (single quotes doubled).
func sqlStringLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// sqlIdent renders s as a safe double-quoted SQL identifier (double quotes doubled). Identifiers reach here
// already sanitised to a conservative charset (sanitizeIdent), so this is belt-and-braces.
func sqlIdent(s string) string {
	return "\"" + strings.ReplaceAll(s, "\"", "\"\"") + "\""
}

// sanitizeIdent reduces a configured stream/field name to a conservative SQL-identifier charset: letters,
// digits, underscore, dash and dot (OpenObserve stream and field names live in this set). Anything else is
// dropped rather than passed through — a stream name is operator config, not model text, but a control
// character or a quote in it is a misconfiguration, never a legitimate name, and must not reach the query.
// An empty result signals "use the default" to the WithStream/WithHostField options.
func sanitizeIdent(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-', r == '.':
			b.WriteRune(r)
		}
	}
	return b.String()
}

// hostOfHit lifts the host attribution from a decoded record. A record missing the host field is attributed
// to a sentinel rather than silently dropped — a line TG cannot attribute is still evidence the window held
// activity, and hiding it would understate the cascade.
func hostOfHit(rec map[string]any, hostField string) string {
	if v, ok := rec[hostField]; ok {
		if s := stringifyScalar(v); s != "" {
			return s
		}
	}
	return "(unattributed)"
}

// microsOfHit lifts the ingest timestamp (epoch microseconds) from a decoded record, 0 when absent/odd.
func microsOfHit(rec map[string]any) int64 {
	v, ok := rec[timestampField]
	if !ok {
		return 0
	}
	switch n := v.(type) {
	case float64:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	case int64:
		return n
	}
	return 0
}

// stringifyScalar renders a scalar JSON value as a compact string; a non-scalar yields "".
func stringifyScalar(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		// OpenObserve host fields are strings; a numeric one is rendered without a trailing ".0".
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%g", t)
	case bool:
		return fmt.Sprintf("%t", t)
	case json.Number:
		return t.String()
	}
	return ""
}
