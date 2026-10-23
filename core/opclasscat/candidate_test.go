package opclasscat

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/actuate/opschema"
	"github.com/territory-grounder/grounder/core/audit"
)

// ---------------------------------------------------------------------------------------------------
// Fakes — the whole lifecycle is provable without a database (the D5 in-memory-fake convention).
// ---------------------------------------------------------------------------------------------------

type fakeStore struct {
	occs    map[string][]Occurrence // key -> journal (dedup enforced, mirroring the PK)
	live    []Candidate
	updated []Candidate
	failOn  string
}

func newFakeStore() *fakeStore { return &fakeStore{occs: map[string][]Occurrence{}} }

func (f *fakeStore) RecordOccurrence(_ context.Context, occ Occurrence) error {
	if f.failOn == "record" {
		return errors.New("record failed")
	}
	// Structural exactly-once, mirroring PRIMARY KEY (candidate_key, external_ref).
	for _, e := range f.occs[occ.CandidateKey] {
		if e.ExternalRef == occ.ExternalRef {
			return nil
		}
	}
	f.occs[occ.CandidateKey] = append(f.occs[occ.CandidateKey], occ)
	return nil
}

func (f *fakeStore) UpsertObserving(_ context.Context, key string, occ Occurrence) error {
	for i := range f.live {
		if f.live[i].CandidateKey == key {
			f.live[i].LastSeenAt = occ.ObservedAt
			return nil
		}
	}
	f.live = append(f.live, Candidate{
		CandidateKey: key, OpClass: occ.OpClass, Op: occ.Op,
		Status: StatusObserving, AutoBarred: true,
		FirstSeenAt: occ.ObservedAt, LastSeenAt: occ.ObservedAt,
	})
	return nil
}

func (f *fakeStore) LiveCandidates(_ context.Context) ([]Candidate, error) {
	if f.failOn == "live" {
		return nil, errors.New("live failed")
	}
	return append([]Candidate(nil), f.live...), nil
}

func (f *fakeStore) Occurrences(_ context.Context, key string, since time.Time) ([]Occurrence, error) {
	var out []Occurrence
	for _, o := range f.occs[key] {
		if o.ObservedAt.After(since) {
			out = append(out, o)
		}
	}
	return out, nil
}

func (f *fakeStore) UpdateCandidate(_ context.Context, c Candidate) error {
	if f.failOn == "update" {
		return errors.New("update failed")
	}
	f.updated = append(f.updated, c)
	for i := range f.live {
		if f.live[i].CandidateKey == c.CandidateKey {
			f.live[i] = c
		}
	}
	return nil
}

type fakeLedger struct {
	entries []audit.GovDecision
	fail    bool
}

func (l *fakeLedger) Append(d audit.GovDecision) (audit.LedgerEntry, error) {
	if l.fail {
		return audit.LedgerEntry{}, errors.New("ledger down")
	}
	l.entries = append(l.entries, d)
	return audit.LedgerEntry{Seq: int64(len(l.entries)), Decision: d.Decision, ActionID: d.ActionID}, nil
}

func occAt(ref, host string, conf float64, at time.Time) Occurrence {
	return Occurrence{
		CandidateKey: "k", ExternalRef: ref, Host: host, Op: "restart nginx",
		OpClass: "restart-service", Confidence: conf, ObservedAt: at,
	}
}

// ---------------------------------------------------------------------------------------------------
// O-2801 — the dedup oracle: distinct REFS, never event count (the 4x-credit lesson).
// RED CONTROL (executed): make Summarize count events instead of distinct refs -> this test fails with
// "one incident re-proposed 5x must count ONCE: got 5 distinct refs".
// ---------------------------------------------------------------------------------------------------

func TestOneIncidentReProposedFiveTimesCountsOnce(t *testing.T) {
	now := time.Now().UTC()
	st := newFakeStore()
	// One incident (same external_ref), proposed five times — exactly what four LibreNMS rules plus a
	// vote-resume produces on a single stopped guest.
	for i := 0; i < 5; i++ {
		if err := st.RecordOccurrence(context.Background(), occAt("librenms-1", "hostA", 0.9, now)); err != nil {
			t.Fatalf("record: %v", err)
		}
	}
	occs, _ := st.Occurrences(context.Background(), "k", now.Add(-EvidenceWindow))
	e := Summarize(occs)
	if e.DistinctRefs != 1 {
		t.Fatalf("one incident re-proposed 5x must count ONCE: got %d distinct refs", e.DistinctRefs)
	}
	if MeetsCandidacy(e) {
		t.Fatal("one incident must NEVER reach candidacy — a capability grant manufactured by alert multiplicity")
	}
}

// Summarize must ALSO defend the property in memory, not only rely on the storage PK: a backfill or a
// future reader that hands it duplicates cannot be allowed to manufacture candidacy.
func TestSummarizeDefendsDedupInMemory(t *testing.T) {
	now := time.Now().UTC()
	dupes := []Occurrence{
		occAt("ref-1", "hostA", 0.9, now),
		occAt("ref-1", "hostB", 0.9, now), // same ref, different host — still ONE incident
		occAt("ref-1", "hostC", 0.9, now),
	}
	e := Summarize(dupes)
	if e.DistinctRefs != 1 {
		t.Fatalf("in-memory dedup: want 1 distinct ref, got %d", e.DistinctRefs)
	}
	if e.DistinctHosts != 1 {
		t.Fatalf("a duplicate ref must not inflate host breadth either: got %d hosts", e.DistinctHosts)
	}
}

// ---------------------------------------------------------------------------------------------------
// O-2802 — threshold-minus-one stays observing, on every axis independently.
// RED CONTROL (executed): lower any bar in MeetsCandidacy -> the matching sub-case fails.
// ---------------------------------------------------------------------------------------------------

func TestThresholdMinusOneStaysObserving(t *testing.T) {
	now := time.Now().UTC()
	cases := []struct {
		name string
		occs []Occurrence
		want bool
	}{
		{
			name: "three refs across two hosts clears the bar",
			occs: []Occurrence{
				occAt("r1", "hostA", 0.9, now),
				occAt("r2", "hostB", 0.9, now),
				occAt("r3", "hostA", 0.9, now),
			},
			want: true,
		},
		{
			name: "two refs (one short) stays observing",
			occs: []Occurrence{
				occAt("r1", "hostA", 0.9, now),
				occAt("r2", "hostB", 0.9, now),
			},
			want: false,
		},
		{
			name: "three refs on ONE host inside 7d stays observing",
			occs: []Occurrence{
				occAt("r1", "hostA", 0.9, now),
				occAt("r2", "hostA", 0.9, now.Add(-24*time.Hour)),
				occAt("r3", "hostA", 0.9, now.Add(-48*time.Hour)),
			},
			want: false,
		},
		{
			name: "three refs on ONE host spanning 7d clears via the span arm",
			occs: []Occurrence{
				occAt("r1", "hostA", 0.9, now),
				occAt("r2", "hostA", 0.9, now.Add(-4*24*time.Hour)),
				occAt("r3", "hostA", 0.9, now.Add(-8*24*time.Hour)),
			},
			want: true,
		},
		{
			name: "mean confidence below the bar stays observing",
			occs: []Occurrence{
				occAt("r1", "hostA", 0.5, now),
				occAt("r2", "hostB", 0.5, now),
				occAt("r3", "hostA", 0.5, now),
			},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := MeetsCandidacy(Summarize(tc.occs)); got != tc.want {
				t.Fatalf("MeetsCandidacy = %v, want %v", got, tc.want)
			}
		})
	}
}

// Confidence is a BAR, never a WEIGHT: a perfect-confidence pair must NOT substitute for the third
// incident. This is the property that stops anything a model emits from accelerating a capability grant.
func TestConfidenceIsABarNeverAWeight(t *testing.T) {
	now := time.Now().UTC()
	perfect := []Occurrence{
		occAt("r1", "hostA", 1.0, now),
		occAt("r2", "hostB", 1.0, now),
	}
	if MeetsCandidacy(Summarize(perfect)) {
		t.Fatal("confidence 1.0 must NOT buy candidacy with fewer incidents — it is a bar, not a weight")
	}
}

// ---------------------------------------------------------------------------------------------------
// Identity: the cluster key must never diverge from the lookup key it materializes under.
// RED CONTROL (executed): drop the normalizeSlug call in CandidateKey -> case variants produce different
// keys and this test fails with "case/whitespace variants must cluster together".
// ---------------------------------------------------------------------------------------------------

func TestCandidateKeyIsNormalizedAndOrderInsensitive(t *testing.T) {
	base := CandidateKey("restart-service", "restart nginx", []string{"unit", "host"})
	if got := CandidateKey("  Restart-Service ", "RESTART NGINX", []string{"host", "unit"}); got != base {
		t.Fatal("case/whitespace variants and param order must cluster together — one remedy, one key")
	}
	if got := CandidateKey("restart-service", "restart nginx", []string{"unit"}); got == base {
		t.Fatal("a DIFFERENT param contract must be a different key")
	}
}

// TestNormalizationMatchesOpschemaLookupTolerance is the EQUIVALENCE PIN standing in for a governed edit.
//
// REQ-2801 requires the cluster key use opschema's INV-08 normalization, but opschema's `normalize` is
// package-private and its file is lockstep-governed (spec/028 T-028-5, Law-Change trailer, Stage 4). This
// oracle proves the replica is equivalent through opschema's PUBLIC behaviour instead: every variant this
// package treats as ONE key must also resolve to ONE spec through opschema.Lookup. If opschema's tolerance
// ever changes, this fails loudly rather than letting a candidate materialize under a key nobody looks up.
func TestNormalizationMatchesOpschemaLookupTolerance(t *testing.T) {
	specs := opschema.Specs()
	if len(specs) == 0 {
		// NOT a skip. This oracle stands in for a governed edit to opschema's private normalize(): it is the
		// only thing proving the replica stays equivalent. With no embedded specs it pins nothing, and a pass
		// would assert the equivalence over an empty set — precisely the drift it exists to catch, reported
		// as success. opschema.Specs() reads the embedded registry, so empty is a regression, not an
		// environment.
		t.Fatal("opschema.Specs() is empty — the normalization equivalence pin has nothing to pin against, so a pass would be vacuous")
	}
	canonical := specs[0].OpClass
	variants := []string{canonical, strings.ToUpper(canonical), "  " + canonical + "  ", " " + strings.ToUpper(canonical)}

	wantKey := CandidateKey(canonical, "op", nil)
	for _, v := range variants {
		if _, ok := opschema.Lookup(v); !ok {
			t.Fatalf("opschema.Lookup rejected variant %q — our normalization is MORE tolerant than the reader it guards", v)
		}
		if got := CandidateKey(v, "op", nil); got != wantKey {
			t.Fatalf("variant %q clusters to a different key than opschema resolves it to — identity has diverged from lookup", v)
		}
	}
}

// ---------------------------------------------------------------------------------------------------
// The Transition chokepoint: ledger-before-row, mandatory rationale, state machine.
// RED CONTROL (executed): write the row before the ledger append -> the ledger-failure case below
// observes a persisted status change with no chain entry and fails.
// ---------------------------------------------------------------------------------------------------

func TestTransitionRequiresRationaleAndGuardsTheStateMachine(t *testing.T) {
	st, lg := newFakeStore(), &fakeLedger{}
	c := Candidate{CandidateKey: "k1", Status: StatusObserving}

	if _, err := Transition(context.Background(), st, lg, c, StatusCandidate, "   "); !errors.Is(err, ErrRationaleRequired) {
		t.Fatalf("blank rationale must be refused, got %v", err)
	}
	// observing cannot jump straight to ratify_ready — the completeness gate is not skippable.
	if _, err := Transition(context.Background(), st, lg, c, StatusRatifyReady, "skip ahead"); !errors.Is(err, ErrBadTransition) {
		t.Fatalf("illegal transition must be refused, got %v", err)
	}
	// A grant is reachable ONLY from the completeness gate. Stage 4's ratify lane opened
	// ratify_ready -> ratified for the operator; it did not open a shortcut from anywhere else, so a
	// candidate that never assembled a full dossier still cannot be granted.
	for _, from := range []Status{StatusObserving, StatusCandidate} {
		c.Status = from
		if _, err := Transition(context.Background(), st, lg, c, StatusRatified, "grant"); !errors.Is(err, ErrBadTransition) {
			t.Fatalf("ratified must be unreachable from %s — only a COMPLETE dossier may be granted, got %v", from, err)
		}
	}
	// And nothing resurrects a decided key: a re-proposal after a dismissal is new evidence under a new
	// key, never a second attempt at a verdict an operator already gave.
	for _, from := range []Status{StatusDismissed, StatusExpired, StatusRatified} {
		c.Status = from
		if _, err := Transition(context.Background(), st, lg, c, StatusRatifyReady, "try again"); !errors.Is(err, ErrBadTransition) {
			t.Fatalf("a decided key (%s) must never re-enter the ladder, got %v", from, err)
		}
	}
}

// The property Stage 2 protected with an unreachable-edge assertion, restated so it survives Stage 4
// opening that edge: the CLUSTERING CRON can never grant, because its source never names the grant.
//
// This is a source-level oracle rather than a behavioural one on purpose. Once the operator lane exists,
// the state machine ALLOWS ratify_ready -> ratified, so no amount of driving the cron proves it will not
// take that edge tomorrow — only the absence of the target from its code does. A behavioural test here
// would pass while a one-line change to the cron quietly turned it into a granting authority.
func TestTheClusteringCronNeverNamesAGrantingTarget(t *testing.T) {
	src, err := os.ReadFile("../../temporal/opclasscluster/cron.go")
	if err != nil {
		t.Skipf("cron source unavailable: %v", err)
	}
	for _, forbidden := range []string{"StatusRatified", "StatusDismissed", "DecisionRatify", "DecisionRevoke"} {
		if strings.Contains(string(src), forbidden) {
			t.Fatalf("the clustering cron names %q — the mechanical pass must never be able to grant or "+
				"withdraw a capability; those verbs belong to the operator lane alone", forbidden)
		}
	}
}

func TestTransitionLedgersBeforeTheRowAndBindsTheCandidateKey(t *testing.T) {
	st, lg := newFakeStore(), &fakeLedger{}
	c := Candidate{CandidateKey: "abcdef0123456789", Status: StatusObserving}

	got, err := Transition(context.Background(), st, lg, c, StatusCandidate, "3 refs across 2 hosts")
	if err != nil {
		t.Fatalf("transition: %v", err)
	}
	if len(lg.entries) != 1 {
		t.Fatalf("want exactly one chain entry, got %d", len(lg.entries))
	}
	e := lg.entries[0]
	if e.Decision != DecisionCandidate {
		t.Fatalf("decision string: want %q, got %q", DecisionCandidate, e.Decision)
	}
	// REQ-2817: the action_id IS the candidate_key for candidacy-phase rows — the ledger writer rejects an
	// empty action_id, so every opclass:* row is joinable to the artifact it governs.
	if e.ActionID != c.CandidateKey {
		t.Fatalf("action_id must be the candidate_key, got %q", e.ActionID)
	}
	if e.Withheld {
		t.Fatal("advancing candidacy withholds nothing (it grants nothing either)")
	}
	if got.LedgerSeq != 1 || got.Status != StatusCandidate {
		t.Fatalf("row must carry the chain seq and new status, got seq=%d status=%s", got.LedgerSeq, got.Status)
	}
	if len(st.updated) != 1 {
		t.Fatalf("row must be persisted exactly once, got %d writes", len(st.updated))
	}
}

// Ledger-BEFORE-row, proven by the failure path: when the chain append fails, the row must NOT change.
// A crash may leave an over-recorded ledger; it must never leave an unrecorded state change.
func TestLedgerFailureLeavesTheRowUntouched(t *testing.T) {
	st, lg := newFakeStore(), &fakeLedger{fail: true}
	c := Candidate{CandidateKey: "k1", Status: StatusObserving}

	if _, err := Transition(context.Background(), st, lg, c, StatusCandidate, "should not land"); err == nil {
		t.Fatal("a ledger failure must fail the transition")
	}
	if len(st.updated) != 0 {
		t.Fatal("ledger-before-row violated: the status changed with no chain entry")
	}
}

// ---------------------------------------------------------------------------------------------------
// The ratify_ready completeness gate — fail-closed on absent evidence.
// RED CONTROL (executed): drop the coverage check in MeetsRatifyReady -> the nil-provider case passes and
// this test fails with "incomplete dossier must NEVER reach an operator".
// ---------------------------------------------------------------------------------------------------

func TestRatifyReadyIsFailClosedOnIncompleteEvidence(t *testing.T) {
	now := time.Now().UTC()
	var occs []Occurrence
	for i, h := range []string{"a", "b", "c", "d", "e"} {
		occs = append(occs, occAt("r"+h, "host"+h, 0.9, now.Add(-time.Duration(i)*time.Hour)))
	}
	e := Summarize(occs)
	full := ReadyInput{Family: "service", Tier: "low", AutoBarredStamped: true, BlastRadiusCoverage: 1.0}

	if !MeetsRatifyReady(e, full) {
		t.Fatal("a complete dossier must reach ratify_ready")
	}
	// Each absence independently holds the candidate back.
	for name, in := range map[string]ReadyInput{
		"no blast radius (Stage 3 unwired)": {Family: "service", Tier: "low", AutoBarredStamped: true, BlastRadiusCoverage: 0},
		"coverage below 80%":                {Family: "service", Tier: "low", AutoBarredStamped: true, BlastRadiusCoverage: 0.79},
		"screen never stamped":              {Family: "service", Tier: "low", AutoBarredStamped: false, BlastRadiusCoverage: 1.0},
		"family unassigned":                 {Tier: "low", AutoBarredStamped: true, BlastRadiusCoverage: 1.0},
		"tier unassigned":                   {Family: "service", AutoBarredStamped: true, BlastRadiusCoverage: 1.0},
		"dismiss TTL active":                {Family: "service", Tier: "low", AutoBarredStamped: true, BlastRadiusCoverage: 1.0, DismissActive: true},
	} {
		if MeetsRatifyReady(e, in) {
			t.Fatalf("%s: incomplete dossier must NEVER reach an operator as if it were complete", name)
		}
	}
	// Four refs is one short even with everything else perfect.
	if MeetsRatifyReady(Summarize(occs[:4]), full) {
		t.Fatal("threshold-minus-one must stay a candidate")
	}
}

// ---------------------------------------------------------------------------------------------------
// The server-derived safety screen: a model can never under-declare its own blast radius.
// RED CONTROL (executed): make ScreenAutoBarred consult a model-supplied tier instead of the observed op
// -> the destructive case returns false and this test fails.
// ---------------------------------------------------------------------------------------------------

func TestScreenIsServerDerivedAndFailsClosed(t *testing.T) {
	if !ScreenAutoBarred("restart-service", "rm -rf /var/lib/data", "prod-db") {
		t.Fatal("a destructive OBSERVED op must bar auto regardless of the class it claims to be")
	}
	if ScreenAutoBarred("restart-service", "systemctl restart nginx", "web01") {
		t.Fatal("a benign reversible op must not be barred (the screen must discriminate, not blanket-bar)")
	}
}

// TestCandidacyGapNeverDisagreesWithMeetsCandidacy is the anti-drift oracle for TG-236 oracle 3.
//
// The console will render "2 more and this becomes a candidate" from CandidacyGap while the cron promotes
// from MeetsCandidacy. If those two ever disagree, the surface promises an operator a finish line the
// machinery does not honour — the exact class of defect (a second copy of a threshold) the constants block
// in candidate.go was written to prevent. So this asserts the invariant directly across a grid that
// straddles every leg, rather than spot-checking a few hand-picked cases.
func TestCandidacyGapNeverDisagreesWithMeetsCandidacy(t *testing.T) {
	for _, refs := range []int{0, 1, 2, 3, 4, 6} {
		for _, hosts := range []int{0, 1, 2, 3} {
			for _, span := range []time.Duration{0, 3 * 24 * time.Hour, MinSpan, 30 * 24 * time.Hour} {
				for _, conf := range []float64{0.0, 0.59, MinMeanConfidence, 0.95} {
					e := Evidence{DistinctRefs: refs, DistinctHosts: hosts, Span: span, MeanConfidence: conf}
					g := CandidacyGap(e)
					if g.Met != MeetsCandidacy(e) {
						t.Fatalf("gap.Met=%v but MeetsCandidacy=%v for %+v", g.Met, MeetsCandidacy(e), e)
					}
					if g.Met {
						if g.RefsNeeded != 0 || g.HostsNeeded != 0 || g.SpanNeeded != 0 || g.ConfidenceShort {
							t.Fatalf("a met candidacy must report no remaining distance, got %+v for %+v", g, e)
						}
						continue
					}
					// Closing every reported remainder must actually open the gate. This is the property
					// that makes the countdown TRUE rather than decorative.
					closed := e
					closed.DistinctRefs += g.RefsNeeded
					if g.HostsNeeded > 0 {
						closed.DistinctHosts += g.HostsNeeded
					}
					if g.ConfidenceShort {
						closed.MeanConfidence = MinMeanConfidence
					}
					if !MeetsCandidacy(closed) {
						t.Fatalf("closing the reported gap %+v did not reach candidacy: %+v -> %+v", g, e, closed)
					}
				}
			}
		}
	}
}

// TestCandidacyGapReportsTheOrLegAsTwoRoutesNotAConjunction pins the one place a naive remainder would lie.
//
// The second leg is ">=2 distinct hosts OR >=7d span". A shape one host short but already past the span is
// NOT waiting for a host at all, and telling an operator it needs one would overstate the bar and suppress a
// ratification TG is in fact ready for.
func TestCandidacyGapReportsTheOrLegAsTwoRoutesNotAConjunction(t *testing.T) {
	// Span leg already satisfied, hosts short, and candidacy NOT yet met (refs still short) — so the gap is
	// actually computed rather than short-circuited by Met. An earlier version of this case used
	// DistinctRefs=MinDistinctRefs, which MEETS candidacy outright: CandidacyGap returned the zero Gap and
	// the assertion held no matter what the OR leg did. A mutation that turned the leg into a conjunction
	// survived it. The refs value below is what makes this oracle able to fail.
	spanned := Evidence{DistinctRefs: MinDistinctRefs - 1, DistinctHosts: 1, Span: MinSpan, MeanConfidence: 0.9}
	if MeetsCandidacy(spanned) {
		t.Fatal("fixture must NOT meet candidacy, or the assertion below is vacuous")
	}
	if g := CandidacyGap(spanned); g.HostsNeeded != 0 || g.SpanNeeded != 0 {
		t.Fatalf("span leg satisfied — no second-leg remainder may be reported, got %+v", g)
	}
	// Neither route open: BOTH must be offered, so the operator can see either one will do.
	neither := Evidence{DistinctRefs: 1, DistinctHosts: 1, Span: 0, MeanConfidence: 0.9}
	g := CandidacyGap(neither)
	if g.HostsNeeded != MinDistinctHosts-1 || g.SpanNeeded != MinSpan {
		t.Fatalf("both routes must be reported when neither is open, got %+v", g)
	}
	if g.RefsNeeded != MinDistinctRefs-1 {
		t.Fatalf("refs remainder wrong: got %d want %d", g.RefsNeeded, MinDistinctRefs-1)
	}
}
