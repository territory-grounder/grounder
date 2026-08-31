package opclasscluster

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/opclasscat"
)

type memStore struct {
	occs map[string][]opclasscat.Occurrence
	live []opclasscat.Candidate
}

func newMem() *memStore { return &memStore{occs: map[string][]opclasscat.Occurrence{}} }

func (m *memStore) RecordOccurrence(_ context.Context, occ opclasscat.Occurrence) error {
	for _, e := range m.occs[occ.CandidateKey] {
		if e.ExternalRef == occ.ExternalRef {
			return nil
		}
	}
	m.occs[occ.CandidateKey] = append(m.occs[occ.CandidateKey], occ)
	return nil
}
func (m *memStore) UpsertObserving(context.Context, string, opclasscat.Occurrence) error { return nil }
func (m *memStore) LiveCandidates(context.Context) ([]opclasscat.Candidate, error) {
	return append([]opclasscat.Candidate(nil), m.live...), nil
}
func (m *memStore) Occurrences(_ context.Context, key string, since time.Time) ([]opclasscat.Occurrence, error) {
	var out []opclasscat.Occurrence
	for _, o := range m.occs[key] {
		if o.ObservedAt.After(since) {
			out = append(out, o)
		}
	}
	return out, nil
}
func (m *memStore) UpdateCandidate(_ context.Context, c opclasscat.Candidate) error {
	for i := range m.live {
		if m.live[i].CandidateKey == c.CandidateKey {
			m.live[i] = c
		}
	}
	return nil
}

type memLedger struct{ entries []audit.GovDecision }

func (l *memLedger) Append(d audit.GovDecision) (audit.LedgerEntry, error) {
	l.entries = append(l.entries, d)
	return audit.LedgerEntry{Seq: int64(len(l.entries))}, nil
}

func fixedNow() time.Time { return time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC) }

func healthyLiveness(_ context.Context, _ time.Duration) (Liveness, error) {
	return Liveness{NewestOccurrence: fixedNow().Add(-1 * time.Hour), SessionsSince: 12}, nil
}

// seed builds a store holding one observing candidate with the given occurrences.
func seed(occs ...opclasscat.Occurrence) (*memStore, *memLedger) {
	st, lg := newMem(), &memLedger{}
	for _, o := range occs {
		_ = st.RecordOccurrence(context.Background(), o)
	}
	st.live = []opclasscat.Candidate{{
		CandidateKey: "k", OpClass: "restart-service", Status: opclasscat.StatusObserving,
		AutoBarred: true, LastSeenAt: fixedNow().Add(-time.Hour),
	}}
	return st, lg
}

func occ(ref, host string, conf float64, at time.Time) opclasscat.Occurrence {
	return opclasscat.Occurrence{
		CandidateKey: "k", ExternalRef: ref, Host: host, OpClass: "restart-service",
		Confidence: conf, ObservedAt: at,
	}
}

// ---------------------------------------------------------------------------------------------------
// O-2801/O-2802 on the REAL pass: three distinct incidents across two hosts advance; minus-one does not.
// RED CONTROL (executed): lower MinDistinctRefs / drop the MeetsCandidacy guard in advance() -> the
// threshold-minus-one case advances and this test fails.
// ---------------------------------------------------------------------------------------------------

func TestThreeDistinctIncidentsAcrossTwoHostsAdvanceToCandidate(t *testing.T) {
	now := fixedNow()
	st, lg := seed(
		occ("r1", "hostA", 0.9, now.Add(-2*time.Hour)),
		occ("r2", "hostB", 0.9, now.Add(-3*time.Hour)),
		occ("r3", "hostA", 0.9, now.Add(-4*time.Hour)),
	)
	j := Job{Store: st, Ledger: lg, Liveness: healthyLiveness, Now: fixedNow}

	res, err := j.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.ToCandidate != 1 {
		t.Fatalf("want 1 advance to candidate, got %d (scanned %d, itemErrors %d)", res.ToCandidate, res.Scanned, res.ItemErrors)
	}
	if st.live[0].Status != opclasscat.StatusCandidate {
		t.Fatalf("row status: want candidate, got %s", st.live[0].Status)
	}
	// REQ-2817: the advance lands on the ONE chain with the candidacy decision string.
	if len(lg.entries) != 1 || lg.entries[0].Decision != opclasscat.DecisionCandidate {
		t.Fatalf("want one %q chain entry, got %+v", opclasscat.DecisionCandidate, lg.entries)
	}
	if lg.entries[0].ActionID != "k" {
		t.Fatalf("chain entry must bind the candidate_key, got %q", lg.entries[0].ActionID)
	}
}

func TestThresholdMinusOneStaysObservingOnTheRealPass(t *testing.T) {
	now := fixedNow()
	st, lg := seed(
		occ("r1", "hostA", 0.9, now.Add(-2*time.Hour)),
		occ("r2", "hostB", 0.9, now.Add(-3*time.Hour)),
	)
	j := Job{Store: st, Ledger: lg, Liveness: healthyLiveness, Now: fixedNow}

	res, err := j.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.ToCandidate != 0 {
		t.Fatalf("two incidents must NOT advance, got %d advances", res.ToCandidate)
	}
	if st.live[0].Status != opclasscat.StatusObserving {
		t.Fatalf("row must stay observing, got %s", st.live[0].Status)
	}
	if len(lg.entries) != 0 {
		t.Fatalf("a non-advance must write NOTHING to the chain, got %d entries", len(lg.entries))
	}
}

// The same incident re-proposed five times is ONE incident on the real pass too (the 4x-credit lesson
// end-to-end, not only in Summarize).
func TestOneIncidentReProposedNeverAdvancesOnTheRealPass(t *testing.T) {
	now := fixedNow()
	st, lg := seed(
		occ("r1", "hostA", 0.99, now.Add(-1*time.Hour)),
		occ("r1", "hostA", 0.99, now.Add(-2*time.Hour)),
		occ("r1", "hostA", 0.99, now.Add(-3*time.Hour)),
		occ("r1", "hostA", 0.99, now.Add(-4*time.Hour)),
		occ("r1", "hostA", 0.99, now.Add(-5*time.Hour)),
	)
	j := Job{Store: st, Ledger: lg, Liveness: healthyLiveness, Now: fixedNow}

	res, _ := j.Run(context.Background())
	if res.ToCandidate != 0 {
		t.Fatal("one incident re-proposed five times must never manufacture candidacy")
	}
}

// ---------------------------------------------------------------------------------------------------
// O-2812 — the dead-man. RED CONTROL (executed): compute liveness from the cron's OWN writes (i.e. drop
// the SessionsSince disagreement check) -> the stale case runs the pass and this test fails.
// ---------------------------------------------------------------------------------------------------

func TestCronRefusesItsPassWhenIntakeIsStaleWhileSessionsFlow(t *testing.T) {
	now := fixedNow()
	st, lg := seed(
		occ("r1", "hostA", 0.9, now.Add(-2*time.Hour)),
		occ("r2", "hostB", 0.9, now.Add(-3*time.Hour)),
		occ("r3", "hostA", 0.9, now.Add(-4*time.Hour)),
	)
	stale := func(_ context.Context, _ time.Duration) (Liveness, error) {
		// Sessions are flowing, but the occurrence seam has written nothing for three days.
		return Liveness{NewestOccurrence: now.Add(-72 * time.Hour), SessionsSince: 40}, nil
	}
	j := Job{Store: st, Ledger: lg, Liveness: stale, Now: fixedNow}

	_, err := j.Run(context.Background())
	if !errors.Is(err, ErrIntakeStale) {
		t.Fatalf("stale intake with sessions flowing MUST refuse the pass loudly, got %v", err)
	}
	// And it must refuse BEFORE doing any work — a truncated journal must not advance anything.
	if st.live[0].Status != opclasscat.StatusObserving || len(lg.entries) != 0 {
		t.Fatal("the pass ran despite stale intake — candidacy computed over a silently truncated journal")
	}
}

// A genuinely quiet estate is NOT stale: silence with no work is honest silence, and must not page.
func TestAQuietEstateIsNotStale(t *testing.T) {
	now := fixedNow()
	st, lg := seed(occ("r1", "hostA", 0.9, now.Add(-2*time.Hour)))
	quiet := func(_ context.Context, _ time.Duration) (Liveness, error) {
		return Liveness{NewestOccurrence: now.Add(-96 * time.Hour), SessionsSince: 0}, nil
	}
	j := Job{Store: st, Ledger: lg, Liveness: quiet, Now: fixedNow}

	if _, err := j.Run(context.Background()); err != nil {
		t.Fatalf("a quiet estate must not trip the dead-man: %v", err)
	}
}

// A missing probe is itself fail-closed: a cron that cannot prove its intake is alive must not compute.
func TestNoLivenessProbeIsFailClosed(t *testing.T) {
	st, lg := seed()
	j := Job{Store: st, Ledger: lg, Now: fixedNow}
	if _, err := j.Run(context.Background()); !errors.Is(err, ErrIntakeStale) {
		t.Fatalf("a nil liveness probe must refuse the pass, got %v", err)
	}
}

// ---------------------------------------------------------------------------------------------------
// The ratify_ready gate is fail-closed without Stage 3's estate walk.
// RED CONTROL (executed): default a nil Ready resolver to full coverage -> this test fails.
// ---------------------------------------------------------------------------------------------------

func TestNilReadyResolverNeverReachesRatifyReady(t *testing.T) {
	now := fixedNow()
	var occs []opclasscat.Occurrence
	for i, h := range []string{"a", "b", "c", "d", "e"} {
		occs = append(occs, occ("r"+h, "host"+h, 0.9, now.Add(-time.Duration(i+1)*time.Hour)))
	}
	st, lg := seed(occs...)
	st.live[0].Status = opclasscat.StatusCandidate

	j := Job{Store: st, Ledger: lg, Liveness: healthyLiveness, Now: fixedNow} // Ready nil
	res, err := j.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.ToRatifyReady != 0 || st.live[0].Status != opclasscat.StatusCandidate {
		t.Fatal("without the estate blast-radius walk an incomplete dossier must NEVER reach an operator")
	}

	// With a resolver reporting full completeness, the same candidate advances — proving the gate is the
	// evidence, not a hardcoded refusal.
	j.Ready = func(context.Context, opclasscat.Candidate, []opclasscat.Occurrence) (opclasscat.ReadyInput, error) {
		return opclasscat.ReadyInput{Family: "service", Tier: "low", AutoBarredStamped: true, BlastRadiusCoverage: 1.0}, nil
	}
	res, err = j.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.ToRatifyReady != 1 || st.live[0].Status != opclasscat.StatusRatifyReady {
		t.Fatalf("a complete dossier must advance, got %d (status %s)", res.ToRatifyReady, st.live[0].Status)
	}
	if lg.entries[len(lg.entries)-1].Decision != opclasscat.DecisionRatifyReady {
		t.Fatalf("want a %q chain entry", opclasscat.DecisionRatifyReady)
	}
}

// Expiry retires the ROW after 60 days of silence, never the possibility — and it is ledgered.
func TestSilentCandidateExpiresAndIsLedgered(t *testing.T) {
	now := fixedNow()
	st, lg := seed()
	st.live[0].LastSeenAt = now.Add(-61 * 24 * time.Hour)

	j := Job{Store: st, Ledger: lg, Liveness: healthyLiveness, Now: fixedNow}
	res, err := j.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Expired != 1 || st.live[0].Status != opclasscat.StatusExpired {
		t.Fatalf("a 60-day-silent candidate must expire, got %d (status %s)", res.Expired, st.live[0].Status)
	}
	last := lg.entries[len(lg.entries)-1]
	if last.Decision != opclasscat.DecisionExpire || !last.Withheld {
		t.Fatalf("expiry must be ledgered as a withheld %q, got %+v", opclasscat.DecisionExpire, last)
	}
}

// The pass creates no executable capability: whatever it does, it never reaches ratified.
func TestThePassCanNeverReachRatified(t *testing.T) {
	now := fixedNow()
	var occs []opclasscat.Occurrence
	for i, h := range []string{"a", "b", "c", "d", "e", "f"} {
		occs = append(occs, occ("r"+h, "host"+h, 1.0, now.Add(-time.Duration(i+1)*time.Hour)))
	}
	st, lg := seed(occs...)
	j := Job{Store: st, Ledger: lg, Liveness: healthyLiveness, Now: fixedNow,
		Ready: func(context.Context, opclasscat.Candidate, []opclasscat.Occurrence) (opclasscat.ReadyInput, error) {
			return opclasscat.ReadyInput{Family: "service", Tier: "low", AutoBarredStamped: true, BlastRadiusCoverage: 1.0}, nil
		}}

	// Run repeatedly: even with perfect evidence, the ceiling is ratify_ready. The grant is an operator act.
	for i := 0; i < 5; i++ {
		if _, err := j.Run(context.Background()); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
	if st.live[0].Status == opclasscat.StatusRatified {
		t.Fatal("the observation arm reached RATIFIED — it manufactured a capability grant")
	}
	for _, e := range lg.entries {
		if e.Decision == "opclass:ratify" {
			t.Fatal("the observation arm wrote a ratify decision to the chain")
		}
	}
}

// Per-item isolation: one failing candidate must not abort the pass (the finalizer contract).
type oneBadStore struct{ *memStore }

func (s oneBadStore) Occurrences(ctx context.Context, key string, since time.Time) ([]opclasscat.Occurrence, error) {
	if key == "bad" {
		return nil, errors.New("boom")
	}
	return s.memStore.Occurrences(ctx, key, since)
}

func TestOneBadCandidateDoesNotAbortThePass(t *testing.T) {
	now := fixedNow()
	base, lg := seed(
		occ("r1", "hostA", 0.9, now.Add(-2*time.Hour)),
		occ("r2", "hostB", 0.9, now.Add(-3*time.Hour)),
		occ("r3", "hostA", 0.9, now.Add(-4*time.Hour)),
	)
	base.live = append([]opclasscat.Candidate{{
		CandidateKey: "bad", Status: opclasscat.StatusObserving, LastSeenAt: now.Add(-time.Hour),
	}}, base.live...)

	j := Job{Store: oneBadStore{base}, Ledger: lg, Liveness: healthyLiveness, Now: fixedNow}
	res, err := j.Run(context.Background())
	if err != nil {
		t.Fatalf("a per-item error must not fail the pass: %v", err)
	}
	if res.ItemErrors != 1 || res.ToCandidate != 1 {
		t.Fatalf("want 1 item error and 1 healthy advance, got %+v", res)
	}
}
