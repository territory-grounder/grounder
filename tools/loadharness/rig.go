package loadharness

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math/big"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	ingestadapter "github.com/territory-grounder/grounder/adapters/ingest"
	"github.com/territory-grounder/grounder/core/auth"
	"github.com/territory-grounder/grounder/core/httpapi"
	"github.com/territory-grounder/grounder/core/trace"
	coreingest "github.com/territory-grounder/grounder/core/ingest"
	alertmanager "github.com/territory-grounder/grounder/modules/ingest/prometheus-alertmanager"
)

// Rig is the in-process stand-in the harness runs against in CI, where no Temporal/worker/Postgres stack
// exists to stand up (the verify jobs are a bare golang image; only the harness job carries Postgres).
// Everything an HTTP request traverses is REAL: the auth router (HMAC verify with replay protection, the
// per-source ingest bearer), the registered front-door handler, and the real Alertmanager normalizer with
// its full grammar. Only the seam BEHIND StartTriage is a fake — one that reproduces the production
// temporalTriage contract (idempotent by external_ref, reject-duplicate returns the existing workflow id
// as success) and then persists the session row asynchronously, like the Runner does.
//
// The rig's read store deliberately surfaces EVERY persisted row (production's sessions_read.go collapses
// per ref): a broken idempotency that double-mints must be VISIBLE to the harness's exactly-one check, or
// the check could never fail and would prove nothing.
//
// RigFaults are the falsifiability knobs: each one breaks exactly one invariant the harness claims to
// verify, so harness_test.go can red-prove every claim (break the rig → the harness must fail naming the
// ref → restore → green).
type Rig struct {
	// URL is the served base URL; ReadSource/ReadSecret and IngestToken are the freshly-minted per-rig
	// credentials a harness Config needs to talk to it.
	URL         string
	ReadSource  string
	ReadSecret  []byte
	IngestToken string

	srv      *httptest.Server
	sessions *rigSessionStore
	timers   struct {
		sync.Mutex
		list []*time.Timer
	}
}

// RigFaults selects which single defect the rig exhibits. The zero value is a healthy rig.
type RigFaults struct {
	// DropRef: the session for this external_ref is minted but NEVER persisted — a lost session.
	DropRef string
	// BreakIdempotency: a duplicate POST of an in-flight ref mints a SECOND workflow id and a SECOND row
	// instead of joining the existing session.
	BreakIdempotency bool
	// PoisonHostRef: this ref's session row persists a WRONG host — cross-contamination.
	PoisonHostRef string
	// Latency overrides the per-session classify latency; 0 = a small default jitter so percentiles are
	// non-degenerate.
	Latency time.Duration
	// StallNonTerminalRef classifies this ref (it appears on the sessions page) but NEVER reaches a
	// terminal status — the TG-80 P1-2 red-proof: a harness that stops at first visibility scores it
	// completed; one that polls to terminal must fail it naming the last status seen.
	StallNonTerminalRef string
}

// StartRig serves the rig and mints its credentials. Callers own Close.
func StartRig(f RigFaults) (*Rig, error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, fmt.Errorf("rig: secret: %w", err)
	}
	tokenRaw := make([]byte, 24)
	if _, err := rand.Read(tokenRaw); err != nil {
		return nil, fmt.Errorf("rig: token: %w", err)
	}
	r := &Rig{
		ReadSource:  "loadharness-rig-reader",
		ReadSecret:  secret,
		IngestToken: hex.EncodeToString(tokenRaw),
		sessions:    &rigSessionStore{},
	}
	sources := rigSources{
		// The push source: bearer-only, exactly like a provisioned Alertmanager (no HMAC secret, so the
		// verifier's empty-key guard keeps the HMAC path closed for it).
		"prometheus-alertmanager": auth.Source{SourceID: "prometheus-alertmanager", IngestToken: []byte(r.IngestToken)},
		// The read principal: HMAC-only machine caller for the sessions poll.
		r.ReadSource: auth.Source{SourceID: r.ReadSource, HMACSecret: secret},
	}
	v, err := auth.NewVerifier(sources, &rigNonces{seen: map[string]struct{}{}}, time.Minute)
	if err != nil {
		return nil, fmt.Errorf("rig: verifier: %w", err)
	}
	rt := auth.NewRouter(v)
	triage := &rigTriage{rig: r, faults: f, minted: map[string]string{}}
	httpapi.Register(rt, httpapi.Deps{
		Ingesters:    rigResolver{mod: alertmanager.New()},
		Triage:       triage,
		SessionsRead: r.sessions,
		// TG-80 P1-2: the detail surface carries STATUS; the harness polls it to terminal.
		SessionDetailRead: r.sessions,
	})
	r.srv = httptest.NewServer(rt.Mux())
	r.URL = r.srv.URL
	return r, nil
}

// Close stops the server and any classify timers still pending (a stopped timer's session simply never
// lands, which is fine — the rig is gone).
func (r *Rig) Close() {
	r.timers.Lock()
	for _, t := range r.timers.list {
		t.Stop()
	}
	r.timers.Unlock()
	r.srv.Close()
}

// HarnessConfig returns a ready harness Config aimed at this rig, with CI-scale timing.
func (r *Rig) HarnessConfig() Config {
	return Config{
		BaseURL:          r.URL,
		IngestToken:      r.IngestToken,
		HMACSource:       r.ReadSource,
		HMACSecret:       r.ReadSecret,
		PollInterval:     20 * time.Millisecond,
		SessionTimeout:   5 * time.Second,
		RunTimeout:       30 * time.Second,
		ExpectQuietSpine: true,
	}
}

// rigSources resolves the two provisioned sources — the auth.SourceResolver seam production backs with
// the DB source registry.
type rigSources map[string]auth.Source

func (s rigSources) LookupSource(_ context.Context, id string) (auth.Source, error) {
	src, ok := s[id]
	if !ok {
		return auth.Source{}, fmt.Errorf("rig: unknown source %q", id)
	}
	return src, nil
}

// rigNonces is the replay store: each (source, nonce) admits once, like core/db's PgNonceStore.
type rigNonces struct {
	mu   sync.Mutex
	seen map[string]struct{}
}

func (n *rigNonces) SeenBefore(_ context.Context, src, nonce string, _ time.Time) (bool, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	key := src + "\n" + nonce
	if _, ok := n.seen[key]; ok {
		return true, nil
	}
	n.seen[key] = struct{}{}
	return false, nil
}

// rigResolver serves the ONE registered ingest capability — the real Alertmanager module. Every other
// slug has no execution path (the same INV-17 refusal the registry-backed production resolver gives).
type rigResolver struct{ mod *alertmanager.Module }

func (rr rigResolver) ResolveIngester(sourceType string) (ingestadapter.Ingester, error) {
	if sourceType != rr.mod.SourceType() {
		return nil, fmt.Errorf("rig: no ingest capability for %q", sourceType)
	}
	return rr.mod, nil
}

// rigTriage reproduces the production temporalTriage contract at the StartTriage seam: workflow id
// deterministic from the ref, reject-duplicate treated as success returning the EXISTING id, and the
// session row persisted asynchronously after a classify latency (the Runner's triage write, compressed).
type rigTriage struct {
	rig    *Rig
	faults RigFaults

	mu     sync.Mutex
	minted map[string]string // ref -> workflow id
	seq    int
}

func (t *rigTriage) StartTriage(_ context.Context, env coreingest.IncidentEnvelope) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if id, ok := t.minted[env.ExternalRef]; ok {
		if !t.faults.BreakIdempotency {
			return id, nil // reject-duplicate: the in-flight session is joined, never re-minted
		}
		// FAULT: mint a second identity AND a second row — the defect the duplicate probe must catch.
		t.seq++
		dupID := fmt.Sprintf("%s-dup%d", id, t.seq)
		t.schedule(env, dupID)
		return dupID, nil
	}
	t.seq++
	id := fmt.Sprintf("tg-rig/%s", env.ExternalRef)
	t.minted[env.ExternalRef] = id
	t.schedule(env, id)
	return id, nil
}

// schedule persists the session row after the classify latency — unless this ref is the configured
// drop (a LOST session: minted, never lands).
func (t *rigTriage) schedule(env coreingest.IncidentEnvelope, workflowID string) {
	if t.faults.DropRef != "" && env.ExternalRef == t.faults.DropRef {
		return
	}
	host := env.Host
	if t.faults.PoisonHostRef != "" && env.ExternalRef == t.faults.PoisonHostRef {
		host = "poisoned-" + host
	}
	row := httpapi.SessionSummary{
		ExternalRef:  env.ExternalRef,
		Host:         host,
		Band:         "POLL_PAUSE",
		RiskLevel:    "low",
		ActionID:     workflowID, // a stand-in identity; the harness never reads it
		ClassifiedAt: time.Now().UTC(),
	}
	// Classification lands after the classify latency; the TERMINAL record lands one more latency later
	// (TG-80 P1-2) — unless this ref is the configured stall, which classifies and then never terminates.
	stalled := t.faults.StallNonTerminalRef != "" && env.ExternalRef == t.faults.StallNonTerminalRef
	ref := env.ExternalRef
	timer := time.AfterFunc(t.classifyLatency(), func() {
		t.rig.sessions.append(row)
		if stalled {
			return
		}
		t.rig.timers.Lock()
		t.rig.timers.list = append(t.rig.timers.list, time.AfterFunc(t.classifyLatency(), func() { t.rig.sessions.markTerminal(ref) }))
		t.rig.timers.Unlock()
	})
	t.rig.timers.Lock()
	t.rig.timers.list = append(t.rig.timers.list, timer)
	t.rig.timers.Unlock()
}

// classifyLatency jitters 10–40ms by default so a level's percentiles are a real distribution, not one
// repeated constant.
func (t *rigTriage) classifyLatency() time.Duration {
	if t.faults.Latency > 0 {
		return t.faults.Latency
	}
	n, err := rand.Int(rand.Reader, big.NewInt(30))
	if err != nil {
		return 25 * time.Millisecond
	}
	return time.Duration(10+n.Int64()) * time.Millisecond
}

// rigSessionStore is the spine read-back: newest first, EVERY row surfaced (see the Rig doc for why it
// does not collapse per ref), count = distinct refs (production Count's UNION-distinct semantics).
type rigSessionStore struct {
	mu   sync.Mutex
	rows []httpapi.SessionSummary
	// terminal marks refs whose "workflow" reached its terminal record (TG-80 P1-2); a classified row
	// whose ref is absent here reports status classified on the detail surface.
	terminal map[string]bool
}

// markTerminal flips a ref to its terminal status — the second timer the rig fires after classification.
func (s *rigSessionStore) markTerminal(ref string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.terminal == nil {
		s.terminal = map[string]bool{}
	}
	s.terminal[ref] = true
}

// SessionDetail serves the detail surface the harness polls to terminal: status "classified" until the rig
// marks the ref terminal, then "stopped" (the rig proposes nothing). An unknown ref is an error — the real
// endpoint 404s, and the harness must treat "not found" as not-yet, never as terminal.
func (s *rigSessionStore) SessionDetail(_ context.Context, _ auth.Principal, ref string) (trace.SessionTrace, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.rows) - 1; i >= 0; i-- {
		if s.rows[i].ExternalRef != ref {
			continue
		}
		st := trace.StatusClassified
		if s.terminal[ref] {
			st = trace.StatusStopped
		}
		return trace.SessionTrace{ExternalRef: ref, Host: s.rows[i].Host, Band: s.rows[i].Band, Status: st}, nil
	}
	return trace.SessionTrace{}, fmt.Errorf("rig: no session %q", ref)
}

func (s *rigSessionStore) append(row httpapi.SessionSummary) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows = append(s.rows, row)
}

func (s *rigSessionStore) RecentSessions(_ context.Context, _ auth.Principal, limit int) ([]httpapi.SessionSummary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]httpapi.SessionSummary, 0, min(limit, len(s.rows)))
	for i := len(s.rows) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, s.rows[i])
	}
	return out, nil
}

func (s *rigSessionStore) SessionCount(_ context.Context, _ auth.Principal) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	distinct := map[string]struct{}{}
	for _, r := range s.rows {
		distinct[r.ExternalRef] = struct{}{}
	}
	return len(distinct), nil
}

// compile-time proof the rig satisfies the httpapi seams it stands in for.
var (
	_ httpapi.IngesterResolver = rigResolver{}
	_ httpapi.TriageStarter    = (*rigTriage)(nil)
	_ httpapi.SessionsReader   = (*rigSessionStore)(nil)
	_ auth.SourceResolver      = rigSources{}
	_ auth.NonceStore          = (*rigNonces)(nil)
)

// guard against the fixture namespace ever naming anything outside the reserved TLD — the constant is
// load-bearing for "can never collide with real estate hosts".
var _ = func() struct{} {
	if !strings.HasSuffix(fixtureDomain, ".invalid") {
		panic("loadharness: fixtureDomain must stay under the RFC-2606 reserved .invalid TLD")
	}
	return struct{}{}
}()
