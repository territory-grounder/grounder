// This file is the READ client's answer to the console's TEST button (core/selftest.Tester) for the
// correlate-logs capability.
//
// WHY A SEARCH AND NOT THE EXPORTER'S STREAM-LIST PROBE. The exporter's SelfTest (selftest.go) lists the
// org's streams — the right proof for the WRITE half, because ingest and a stream list share a credential.
// The correlate tool reads through a DIFFERENT route (_search) over a SPECIFIC stream, and two things the
// stream list cannot catch fail exactly there: a credential granted list-but-not-search (OpenObserve grants
// read scopes separately), and a TG_OPENOBSERVE_LOG_STREAM that names a stream the pipeline never ships to —
// which would otherwise surface only as `correlate-logs` returning "no matching lines" during an incident,
// the empty-vs-broken trap. So this probe runs the ACTUAL read path the tool uses: one bounded, read-only
// _search over the configured stream. It is the probe wired to the openobserve TEST button (the exporter's
// stream-list SelfTest was never offered to the probe registry), so its verb is what the descriptor now
// promises (descriptor.go).
//
// It writes NOTHING — _search is a read — and it is bounded: size 0 (no rows returned, only the count), a
// narrow recent window, a capped body read, and the console's context deadline.
package openobserve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/territory-grounder/grounder/core/selftest"
)

// probeWindow is how far back the self-test's bounded search looks. Short because the probe proves the
// stream is QUERYABLE, not that it is non-empty: an empty window is still a pass (with a stated caveat).
const probeWindow = 15 * time.Minute

// compile-time proof the read client can honour the TEST button its descriptor advertises. A rename in
// core/selftest turns this into a build failure rather than a silently unreachable method — the exact
// defect class this repo's self-test surface exists to close.
var _ selftest.Tester = (*Reader)(nil)

// SelfTest runs one bounded, read-only _search over the configured stream and reports whether the correlate
// path is reachable, authenticated and queryable. It ingests nothing and leaves nothing to attribute, so the
// operator argument is ignored.
//
// It returns an error when the token cannot be resolved, the endpoint cannot be reached, OpenObserve refuses
// the credential or the read, or the answer is not an OpenObserve search reply — the last because a 200 from
// something that is not OpenObserve is the "pointed at the wrong thing" case a TEST button exists to catch.
func (r *Reader) SelfTest(ctx context.Context, _ string) (selftest.Result, error) {
	if r == nil || strings.TrimSpace(r.endpoint) == "" {
		return selftest.Result{Summary: "no OpenObserve base URL is configured, so the correlate-logs tool was not registered"},
			errors.New("openobserve: correlate self-test needs a base URL — TG_OPENOBSERVE_URL is empty, so the read client was never built")
	}
	if r.http == nil {
		return selftest.Result{Summary: "this OpenObserve read client has no HTTP transport, so nothing can be queried"},
			errors.New("openobserve: correlate self-test found no HTTP transport on the reader — it was not built by NewReader")
	}

	// ── 1. THE CREDENTIAL, resolved exactly as Correlate resolves it on every read ────────────────────────
	token, err := r.tokenRef.Resolve()
	if err != nil {
		return selftest.Result{
			Summary: "the OpenObserve token could not be read from " + safeRef(r.tokenRef) + " — nothing was queried",
			Detail: "the token never resolved, so every correlation fails the same way before a request leaves the " +
				"worker. This is a TG-side secret problem, not an OpenObserve one. Underlying fault: " + err.Error(),
		}, fmt.Errorf("openobserve: correlate self-test could not resolve the token reference %s: %w", safeRef(r.tokenRef), err)
	}
	if strings.TrimSpace(token) == "" {
		return selftest.Result{
			Summary: "the reference " + safeRef(r.tokenRef) + " resolved, but the stored token is EMPTY — nothing was queried",
			Detail:  "an empty credential is refused on every search, so correlate-logs can never read. Save the OpenObserve credential — base64(user:password) — into this module's secret lane.",
		}, errors.New("openobserve: correlate self-test found the token reference resolves to an empty value")
	}

	// ── 2. THE BOUNDED, READ-ONLY SEARCH ──────────────────────────────────────────────────────────────────
	end := r.now()
	start := end.Add(-probeWindow)
	body, _ := json.Marshal(searchRequest{
		Query: searchQuery{
			// SELECT * over the configured stream, size 0: the query PLANS and the credential is checked, but
			// no row crosses the wire. This is the same route and the same stream Correlate reads.
			SQL:       "SELECT * FROM " + sqlIdent(r.stream),
			StartTime: start.UnixMicro(),
			EndTime:   end.UnixMicro(),
			From:      0,
			Size:      0,
		},
		SearchType: "ui",
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.endpoint+searchPath, strings.NewReader(string(body)))
	if err != nil {
		return selftest.Result{
			Summary: "the configured OpenObserve base URL is not a usable URL — nothing was queried",
			Detail:  "TG_OPENOBSERVE_URL could not be turned into a request: " + err.Error(),
		}, fmt.Errorf("openobserve: correlate self-test could not build the search request: %w", err)
	}
	// The SAME auth construction Correlate uses: HTTP Basic with the base64 credential verbatim, never Bearer.
	req.Header.Set("Authorization", "Basic "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := r.http.Do(req)
	if err != nil {
		return selftest.Result{
			Summary: "OpenObserve at " + r.endpoint + " could not be reached — nothing was queried",
			Detail:  classifyProbeTransport(err),
		}, fmt.Errorf("openobserve: correlate self-test could not reach %s: %w", r.endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, searchMaxBytes))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return selftest.Result{
			Summary: fmt.Sprintf("OpenObserve at %s answered %d to a bounded search of stream %q — correlate-logs cannot read", r.endpoint, resp.StatusCode, r.stream),
			Detail:  classifyProbeSearchStatus(resp.StatusCode, r.stream),
		}, fmt.Errorf("openobserve: correlate self-test got status %d from the search route", resp.StatusCode)
	}

	// ── 3. THE OBSERVATION ────────────────────────────────────────────────────────────────────────────────
	var sr searchResponse
	if err := json.Unmarshal(raw, &sr); err != nil {
		return selftest.Result{
			Summary: fmt.Sprintf("%s answered 200 but not with an OpenObserve search reply — correlate-logs cannot trust it", r.endpoint),
			Detail: "the response could not be parsed as OpenObserve's search payload, so this host is probably not " +
				"the OpenObserve API: check TG_OPENOBSERVE_URL for a portal or proxy answering in front of it. Parse fault: " + err.Error(),
		}, fmt.Errorf("openobserve: correlate self-test could not decode the search reply: %w", err)
	}

	notes := []string{
		"this proves the endpoint answers, the credential is accepted for SEARCH, and the configured stream is " +
			"queryable — the exact read path correlate-logs uses. It does not prove ingest is permitted (OpenObserve " +
			"grants read and write separately), and it is bounded to the last " + probeWindow.String(),
		"correlate-logs also needs the estate graph populated, so it can expand a host to its blast-radius " +
			"neighbours; an empty graph leaves the tool able to search only the named host",
	}
	return selftest.Result{
		Summary: fmt.Sprintf("reached OpenObserve at %s and ran a bounded read of stream %q (%d record(s) in the last %s) — nothing was ingested",
			r.endpoint, r.stream, sr.Total, probeWindow.String()),
		Detail: joinProbeNotes(notes),
	}, nil
}

// classifyProbeSearchStatus turns OpenObserve's answer to the bounded search into an operator-actionable
// diagnosis. It keys on the STATUS CODE, never the body — vendor wording and proxy error pages drift — and
// names the SEARCH-specific failure modes the stream-list probe cannot: a missing/mistyped stream (404) and
// a credential granted list-but-not-search (403).
func classifyProbeSearchStatus(status int, stream string) string {
	switch status {
	case http.StatusUnauthorized:
		return "OpenObserve rejected the credential (401) — it is wrong, expired, or revoked. The common shape is a " +
			"RAW API key in the token field: OpenObserve expects the base64(user:password) credential presented as " +
			"HTTP Basic, and a raw key 401s exactly like this"
	case http.StatusForbidden:
		return "the credential authenticated but OpenObserve refused the SEARCH (403) — the account may list streams " +
			"yet lack read on this data. correlate-logs needs search, so grant the account read on stream " +
			strconv.Quote(stream) + " (a scope the exporter's stream-list test would not have caught)"
	case http.StatusNotFound:
		return "OpenObserve has no such stream or route (404) — either TG_OPENOBSERVE_LOG_STREAM names a stream (" +
			strconv.Quote(stream) + ") the shipping pipeline never created, or the base URL is missing its /api/<org> " +
			"prefix. Both surface only as correlate-logs finding nothing, which is why this test exists"
	case http.StatusTooManyRequests:
		return "OpenObserve is rate-limiting this credential (429) — the credential and endpoint are fine; retry shortly"
	}
	if status >= 500 {
		return fmt.Sprintf("OpenObserve answered %d — TG reached it and presented the credential, but OpenObserve "+
			"itself is failing. A vendor-side fault rather than a TG configuration one", status)
	}
	return fmt.Sprintf("OpenObserve answered %d to a bounded search of stream %s", status, strconv.Quote(stream))
}
