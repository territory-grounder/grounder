// Package opclasscat is the earned op-class catalog's OBSERVATION arm (spec/028 REQ-2801/2802/2811/2812,
// epic TG-227 plane 3, Stage 2): it turns recurring free-form proposals (spec/026's shadow plane) into
// op-class CANDIDATES an operator can later ratify from an evidence dossier.
//
// It creates NO EXECUTABLE CAPABILITY. Nothing here writes the registry, the graduation ladder, or any
// actuation surface; the only durable effects are a lifecycle row, an append-only evidence journal, and
// ledger entries. Rung 0 ("proposes only") remains registry ABSENCE — fail-closed by construction, zero new
// code (REQ-2805). The ratify lane, the overlay, and the widened ladder are Stage 4 and carry their own
// governed changes.
//
// Load-bearing properties:
//
//   - CONFIDENCE IS A BAR, NEVER A WEIGHT (REQ-2811). A key must clear a mean-confidence floor; a HIGHER
//     mean never buys candidacy faster or with fewer incidents. Nothing a model can inflate accelerates the
//     grant of a capability.
//   - EVIDENCE CREDIT IS EXACTLY-ONCE BY KEY, structurally, in the occurrence primary key — not by join
//     correctness downstream (the 4x-credit lesson: one fault raises four alert rules).
//   - HOST AND RULE FAMILY ARE EVIDENCE, NOT IDENTITY. Cross-host, cross-rule recurrence is precisely what
//     "this remedy generalizes" means; keying on them would fragment one remedy across alert families.
//   - EVERY STATUS CHANGE FLOWS THROUGH Transition — ledger BEFORE row, mandatory rationale, state-machine
//     guarded (the core/skillstore/transition.go chokepoint pattern).
package opclasscat

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/safety"
)

// Status is the candidate lifecycle vocabulary — the exact CHECK set of opclass_candidate.status.
type Status string

const (
	StatusObserving   Status = "observing"    // accruing evidence; below the candidacy bar
	StatusCandidate   Status = "candidate"    // cleared the mechanical recurrence bar
	StatusRatifyReady Status = "ratify_ready" // dossier complete; awaiting an operator (Stage 4)
	StatusRatified    Status = "ratified"     // operator granted (Stage 4 — never reached from here)
	StatusDismissed   Status = "dismissed"    // operator declined; 30-day read-path TTL
	StatusExpired     Status = "expired"      // 60 days of silence; the key is re-observable fresh
)

// Decision strings for the ONE org-global chain (REQ-2817).
const (
	DecisionCandidate   = "opclass:candidate"
	DecisionRatifyReady = "opclass:ratify-ready"
	DecisionExpire      = "opclass:expire"
	// The OPERATOR verbs (spec/028 T-028-7). Candidacy is mechanical and the cron drives it; these three
	// are reached only by a human acting through the ratify lane, and they are the reason the lane exists.
	DecisionRatify  = "opclass:ratify"
	DecisionDismiss = "opclass:dismiss"
	DecisionRevoke  = "opclass:revoke"
)

// Mechanical candidacy thresholds (REQ-2811). Exported so the oracles pin the SAME constants the cron
// reads — a threshold that lives in two places is a threshold that drifts.
const (
	// observing -> candidate
	MinDistinctRefs   = 3 // DISTINCT external_refs, never event count
	MinDistinctHosts  = 2 // ... OR the span below
	MinSpan           = 7 * 24 * time.Hour
	MinMeanConfidence = 0.6 // between StopThreshold 0.5 and EscalateThreshold 0.7
	EvidenceWindow    = 30 * 24 * time.Hour

	// candidate -> ratify_ready
	MinRefsForRatifyReady  = 5
	MinBlastRadiusCoverage = 0.8

	// lifecycle
	DismissTTL    = 30 * 24 * time.Hour
	SilenceExpiry = 60 * 24 * time.Hour
)

var (
	ErrRationaleRequired = errors.New("opclasscat: rationale required")
	ErrBadTransition     = errors.New("opclasscat: illegal status transition")
)

// Occurrence is one screened shadow-proposal observation — the evidence unit. Model free text arrives
// ALREADY screened (spec/026 REQ-2606 screens at the trust boundary); this package never unscreens it and
// never parses it into control flow (INV-08).
type Occurrence struct {
	CandidateKey  string
	ExternalRef   string
	Host          string
	Target        string
	Op            string
	OpClass       string
	Rationale     string
	UndoSketch    string
	Confidence    float64
	EvidenceIDs   []string
	ActorEvidence []byte
	Band          string
	Outcome       string
	ObservedAt    time.Time
}

// Candidate is the lifecycle row.
type Candidate struct {
	ID              int64
	CandidateKey    string
	OpClass         string
	Op              string
	ParamNames      []string
	Status          Status
	AutoBarred      bool
	Family          string
	Tier            string
	DossierHash     string
	DismissedAt     *time.Time
	DismissUntil    *time.Time
	Rationale       string
	LedgerSeq       int64
	FirstSeenAt     time.Time
	LastSeenAt      time.Time
	StatusChangedAt time.Time
}

// Ledger is the slice of audit.Ledger this package needs (INV-19: one chain).
type Ledger interface {
	Append(d audit.GovDecision) (audit.LedgerEntry, error)
}

// Store is the persistence surface. The pgx implementation lives in core/db; an in-memory fake backs the
// CI oracles so the whole lifecycle is provable without a database.
type Store interface {
	// RecordOccurrence appends one observation. MUST be idempotent on (candidate_key, external_ref) —
	// re-proposals of the same incident are not new evidence.
	RecordOccurrence(ctx context.Context, occ Occurrence) error
	// UpsertObserving creates the live row for a key if none exists, and refreshes last_seen_at.
	UpsertObserving(ctx context.Context, key string, occ Occurrence) error
	// LiveCandidates returns every non-terminal row (observing/candidate/ratify_ready).
	LiveCandidates(ctx context.Context) ([]Candidate, error)
	// Occurrences returns the journal for a key within the window, newest first.
	Occurrences(ctx context.Context, key string, since time.Time) ([]Occurrence, error)
	// UpdateCandidate persists Status, AutoBarred, Family, Tier, DossierHash, dismissal, Rationale,
	// LedgerSeq, StatusChangedAt.
	UpdateCandidate(ctx context.Context, c Candidate) error
}

// normalizeSlug mirrors opschema's INV-08 normalization (lowercase + trim) so a cluster key can never
// diverge from the LOOKUP key the class would later materialize under.
//
// It is a deliberate REPLICA rather than a call: opschema's `normalize` is package-private, and exporting it
// means editing core/actuate/opschema — a lockstep-governed file owned by spec/028 T-028-5, which carries a
// Law-Change trailer and belongs to Stage 4. Replicating two lines under a pinning oracle
// (TestNormalizationMatchesOpschemaLookupTolerance, which proves equivalence through opschema's PUBLIC
// behaviour) buys the same guarantee without a governed edit — and the oracle fails loudly if the two ever
// diverge. The validator-tolerance == reader-tolerance pattern, applied to identity.
func normalizeSlug(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// CandidateKey computes the cluster identity (REQ-2801):
//
//	SHA-256("v1|" + norm(op_class) + "|" + norm(op) + "|" + sorted-param-names)
//
// Param NAMES only — never values: "restart nginx on host A" and "restart nginx on host B" are the SAME
// desire with different arguments, and a value-sensitive key would make every incident its own singleton
// and candidacy unreachable. The "v1|" prefix versions the scheme so a future change is a new key space
// rather than a silent re-clustering of history.
func CandidateKey(opClass, op string, paramNames []string) string {
	names := append([]string(nil), paramNames...)
	for i := range names {
		names[i] = normalizeSlug(names[i])
	}
	sort.Strings(names)
	h := sha256.Sum256([]byte("v1|" + normalizeSlug(opClass) + "|" + normalizeSlug(op) + "|" + strings.Join(names, ",")))
	return hex.EncodeToString(h[:])
}

// ScreenAutoBarred stamps the SERVER-DERIVED never-auto screen from the OBSERVED operation — never from
// anything the model declared about itself. Fail-closed: the column defaults TRUE and only this screen can
// clear it, so a row that never ran the screen can never be mistaken for one that passed it.
//
// A barred candidate stays RATIFIABLE (a recurring destructive desire must be VISIBLE to an operator) but
// is ceiling-capped at "asks first" forever — visible, never climbable.
func ScreenAutoBarred(opClass, op string, targets ...string) bool {
	if safety.IsNeverAuto(opClass) {
		return true
	}
	parts := append([]string{opClass, op}, targets...)
	return safety.IsDestructiveOp(parts...)
}

// Evidence is the mechanical summary of a key's journal within the rolling window — the ONLY input to the
// candidacy decision. Every field is a count or a span; none is a model-supplied score that could be
// inflated into a faster grant.
type Evidence struct {
	DistinctRefs   int
	DistinctHosts  int
	Span           time.Duration
	MeanConfidence float64
	Newest         time.Time
	Oldest         time.Time
}

// Summarize reduces occurrences to Evidence. Occurrences are already exactly-once by (key, ref) at the
// storage layer; this defends the same property in memory so a fake store, a backfill, or a future reader
// can never re-introduce the 4x-credit defect.
func Summarize(occs []Occurrence) Evidence {
	var e Evidence
	refs := map[string]bool{}
	hosts := map[string]bool{}
	var confSum float64
	for _, o := range occs {
		if refs[o.ExternalRef] {
			continue // exactly-once by ref, defended in memory too
		}
		refs[o.ExternalRef] = true
		if h := strings.TrimSpace(o.Host); h != "" {
			hosts[h] = true
		}
		confSum += o.Confidence
		if e.Newest.IsZero() || o.ObservedAt.After(e.Newest) {
			e.Newest = o.ObservedAt
		}
		if e.Oldest.IsZero() || o.ObservedAt.Before(e.Oldest) {
			e.Oldest = o.ObservedAt
		}
	}
	e.DistinctRefs = len(refs)
	e.DistinctHosts = len(hosts)
	if e.DistinctRefs > 0 {
		e.MeanConfidence = confSum / float64(e.DistinctRefs)
		e.Span = e.Newest.Sub(e.Oldest)
	}
	return e
}

// MeetsCandidacy reports whether evidence clears the observing -> candidate bar (REQ-2811):
// >=3 DISTINCT refs AND (>=2 distinct hosts OR >=7d span) AND mean confidence >= the bar.
//
// Confidence enters as a THRESHOLD only. There is deliberately no arithmetic in which a higher mean
// substitutes for fewer incidents or fewer hosts.
func MeetsCandidacy(e Evidence) bool {
	if e.DistinctRefs < MinDistinctRefs {
		return false
	}
	if e.DistinctHosts < MinDistinctHosts && e.Span < MinSpan {
		return false
	}
	return e.MeanConfidence >= MinMeanConfidence
}

// Gap is what a shape still needs before it crosses observing -> candidate, expressed as the REMAINDER
// against the same constants MeetsCandidacy reads (TG-236 oracle 3, "the journey made visible").
//
// It exists so the console can say "seen 8x on 3 hosts — 2 more and this becomes a candidate you can
// ratify" without a second copy of the thresholds. Evidence accrual is currently invisible to an
// operator, which makes the motivation to engage invisible too; a surface that shows the finish line is
// what turns a log into a decision surface.
type Gap struct {
	// Met is CandidacyGap's answer to the same question MeetsCandidacy answers, and is derived from it
	// directly rather than re-deduced — the two can never disagree.
	Met bool
	// RefsNeeded is how many further DISTINCT incidents are required. Distinct refs, never event count:
	// one stopped guest raises four alert rules, and the PK (candidate_key, external_ref) is what stops
	// that multiplicity from manufacturing candidacy.
	RefsNeeded int
	// HostsNeeded and SpanNeeded are the TWO ROUTES through the second leg, which is an OR. Both are
	// reported whenever the leg is unmet, and a reader must never present them as a conjunction: either
	// one more host OR more elapsed time opens the gate, and telling an operator they need both would
	// overstate what TG is waiting for.
	HostsNeeded int
	SpanNeeded  time.Duration
	// ConfidenceShort is a threshold fact, not a quantity to close. There is deliberately no arithmetic
	// anywhere in which a higher mean substitutes for fewer incidents or fewer hosts, so this is reported
	// as a blocking flag rather than as a distance the operator could be tempted to trade against.
	ConfidenceShort bool
}

// CandidacyGap reports the remaining distance to the observing -> candidate bar.
//
// It mirrors MeetsCandidacy leg for leg and reads the same exported constants. The zero Gap with Met=true
// means "this shape is already a candidate"; every other field is meaningless in that case and is left
// zero deliberately, so a caller that ignores Met cannot render a countdown for something already arrived.
func CandidacyGap(e Evidence) Gap {
	g := Gap{Met: MeetsCandidacy(e)}
	if g.Met {
		return g
	}
	if n := MinDistinctRefs - e.DistinctRefs; n > 0 {
		g.RefsNeeded = n
	}
	// The OR leg: report a remainder only when NEITHER route is already open.
	if e.DistinctHosts < MinDistinctHosts && e.Span < MinSpan {
		g.HostsNeeded = MinDistinctHosts - e.DistinctHosts
		g.SpanNeeded = MinSpan - e.Span
	}
	g.ConfidenceShort = e.MeanConfidence < MinMeanConfidence
	return g
}

// Tally is the per-shape recurrence count a QUEUE row needs: how many distinct incidents this remedy has
// answered, and across how many hosts.
//
// It is deliberately not Evidence: the queue renders counts for many shapes at once and must not pay for
// each one's full occurrence journal, while the dossier (one shape, under an operator's eye) still reads
// the FULL journal because a windowed read would quietly shrink the evidence a grant rests on.
type Tally struct {
	Occurrences int
	Hosts       int
	// Span and MeanConfidence carry the two remaining facts CandidacyGap needs, so a queue row can render
	// its own distance-to-candidacy from one aggregate read.
	Span           time.Duration
	MeanConfidence float64
}

// Evidence renders a Tally into the shape CandidacyGap consumes. Same constants, same predicate, one
// conversion — so the queue's countdown and the cron's promotion decision cannot drift apart.
func (t Tally) Evidence() Evidence {
	return Evidence{
		DistinctRefs:   t.Occurrences,
		DistinctHosts:  t.Hosts,
		Span:           t.Span,
		MeanConfidence: t.MeanConfidence,
	}
}

// ReadyInput carries the completeness facts for the candidate -> ratify_ready gate that do NOT come from
// the journal: the mechanically assigned family/tier and the estate blast-radius coverage.
type ReadyInput struct {
	Family string
	Tier   string
	// AutoBarredStamped records that the never-auto screen RAN; AutoBarred is its VERDICT. Distinct on
	// purpose: a row that never ran the screen must never be mistaken for one that passed it, and a
	// barred row must carry the bar itself so the cron can stamp it onto the candidate (TG-227 blocker 4,
	// the stamp half — the enforcement half lives at the ratify writer and the graduation tier floor).
	AutoBarredStamped   bool
	AutoBarred          bool
	BlastRadiusCoverage float64 // fraction of occurrence targets with a computed blast radius
	DismissActive       bool
}

// MeetsRatifyReady reports whether a candidate's dossier is COMPLETE enough to put in front of an operator
// (REQ-2811): >=5 distinct refs AND family/tier assigned AND the auto_barred screen stamped AND blast
// radius computed for >=80% of occurrence targets AND no active dismiss TTL.
//
// Fail-closed on absence: with no blast-radius provider wired (Stage 3 supplies the estate walk), coverage
// is 0 and a candidate simply STAYS a candidate. Incomplete evidence never reaches an operator as if it
// were complete.
// RatifyReadyGaps names EVERY leg of the ratify-ready gate a candidate currently fails, in the order the
// gate evaluates them. An empty result means the dossier is complete.
//
// WHY THIS EXISTS. MeetsRatifyReady answers one bit, and "not ready" is five different situations:
// measured 2026-08-06, all 8 candidates on the deployed estate sat at `observing` with nothing telling
// them apart. A candidate one distinct-ref short of the bar and a candidate whose blast-radius provider is
// not wired at all are the same row in the same status — and only the second is a deployment defect.
//
// The gate is correctly fail-closed and its own comment says so: "with no blast-radius provider wired
// (Stage 3 supplies the estate walk), coverage is 0 and a candidate simply STAYS a candidate." That is the
// right behaviour and it is exactly what makes the state unreadable: a gate that is plausibly satisfiable
// and practically unsatisfiable looks identical to one that is merely waiting for more evidence.
//
// It returns REASONS, not a score. A single "readiness percentage" would let four satisfied legs mask the
// one that is structurally unreachable, which is the failure this is built to end.
func RatifyReadyGaps(e Evidence, in ReadyInput) []string {
	var gaps []string
	if e.DistinctRefs < MinRefsForRatifyReady {
		gaps = append(gaps, fmt.Sprintf("distinct_refs %d < %d", e.DistinctRefs, MinRefsForRatifyReady))
	}
	if strings.TrimSpace(in.Family) == "" {
		gaps = append(gaps, "family unassigned")
	}
	if strings.TrimSpace(in.Tier) == "" {
		gaps = append(gaps, "tier unassigned")
	}
	if !in.AutoBarredStamped {
		gaps = append(gaps, "auto_barred screen not stamped")
	}
	if in.BlastRadiusCoverage < MinBlastRadiusCoverage {
		// Zero coverage is called out separately: it is the signature of an UNWIRED blast-radius provider,
		// not of a candidate that is partway there. Those need different responses — one is a deployment
		// gap, the other is patience.
		if in.BlastRadiusCoverage == 0 {
			gaps = append(gaps, "blast_radius coverage 0% (no provider wired, or no target resolved)")
		} else {
			gaps = append(gaps, fmt.Sprintf("blast_radius coverage %.0f%% < %.0f%%",
				in.BlastRadiusCoverage*100, MinBlastRadiusCoverage*100))
		}
	}
	if in.DismissActive {
		gaps = append(gaps, "an operator dismissal is still in its TTL")
	}
	return gaps
}

func MeetsRatifyReady(e Evidence, in ReadyInput) bool {
	if e.DistinctRefs < MinRefsForRatifyReady {
		return false
	}
	if strings.TrimSpace(in.Family) == "" || strings.TrimSpace(in.Tier) == "" {
		return false
	}
	if !in.AutoBarredStamped {
		return false
	}
	if in.BlastRadiusCoverage < MinBlastRadiusCoverage {
		return false
	}
	return !in.DismissActive
}

// allowed is the candidacy-phase state machine. Stage 4 adds ratify/dismiss from ratify_ready; this table
// contains only what Stage 2 can legally drive, so an observation-arm bug cannot invent a grant.
// The OPERATOR edges (StatusRatified / StatusDismissed) were deliberately absent through Stage 2-3: the
// clustering cron drives every MECHANICAL edge, and leaving ratify unreachable made "the cron can never
// grant anything" structural rather than conventional. Stage 4's ratify lane opens them — and only for a
// human, because nothing else calls Transition with these targets: the cron's own code names
// StatusCandidate, StatusRatifyReady and StatusExpired and nothing else, and it cannot name a target that
// does not appear in its source.
//
// Note what is STILL absent. There is no edge INTO StatusObserving (candidacy is earned from evidence, not
// conferred), none out of StatusRatified except via the overlay's revoke row (the grant's lifecycle belongs
// to the append-only overlay, not to this lifecycle table), and no resurrection from dismissed or expired —
// a re-proposal after a dismissal is a NEW key's evidence, not a second chance at an old verdict.
var allowed = map[Status]map[Status]bool{
	StatusObserving: {StatusCandidate: true, StatusExpired: true},
	StatusCandidate: {StatusRatifyReady: true, StatusExpired: true, StatusDismissed: true},
	// ratify_ready→expired closes a latent trap: the cron's silence-expiry check runs for EVERY live
	// status, and without this edge a 60-day-silent ratify_ready row would fail the transition on every
	// pass, forever, wedging the pass with ErrBadTransition instead of retiring the stale dossier. An
	// expired dossier is re-observable — expiry loses nothing but staleness.
	StatusRatifyReady: {StatusRatified: true, StatusDismissed: true, StatusExpired: true},
}

// TransitionAllowed reports whether the candidacy state machine permits from→to. Exported so oracles can
// pin edges (the ratify_ready→expired edge closed a wedge; its absence must be a red test, not a rediscovery).
func TransitionAllowed(from, to Status) bool { return allowed[from][to] }

func decisionFor(to Status) string {
	switch to {
	case StatusCandidate:
		return DecisionCandidate
	case StatusRatifyReady:
		return DecisionRatifyReady
	case StatusExpired:
		return DecisionExpire
	case StatusRatified:
		return DecisionRatify
	case StatusDismissed:
		return DecisionDismiss
	default:
		return "opclass:" + string(to)
	}
}

// Transition is the ONLY way a candidate's status changes. It guards the state machine, appends the
// mandatory rationale to the row's append-only log, and ledger-records the decision BEFORE writing the row
// — so a crash leaves an over-recorded ledger, never an unrecorded state change (the skillstore chokepoint
// contract).
//
// ActionID is the candidate_key (REQ-2817): the ledger writer rejects an empty action_id, so every
// opclass:* row is joinable to the exact artifact it governs.
func Transition(ctx context.Context, st Store, lg Ledger, c Candidate, to Status, rationale string) (Candidate, error) {
	rationale = strings.TrimSpace(rationale)
	if rationale == "" {
		return Candidate{}, ErrRationaleRequired
	}
	if !allowed[c.Status][to] {
		return Candidate{}, fmt.Errorf("%w: %s -> %s (key %s)", ErrBadTransition, c.Status, to, shortKey(c.CandidateKey))
	}

	entry, err := lg.Append(audit.GovDecision{
		Decision: decisionFor(to),
		Reason:   rationale,
		ActionID: c.CandidateKey,
		// Expiry withholds the candidacy; advancing it does not. Neither GRANTS anything — the grant is
		// Stage 4's operator act.
		Withheld: to == StatusExpired,
	})
	if err != nil {
		return Candidate{}, fmt.Errorf("ledger append: %w", err)
	}

	c.Status = to
	c.Rationale = clipLog(c.Rationale + "\n[" + string(to) + "] " + rationale)
	c.LedgerSeq = entry.Seq
	c.StatusChangedAt = time.Now().UTC()
	if err := st.UpdateCandidate(ctx, c); err != nil {
		return Candidate{}, err
	}
	return c, nil
}

func shortKey(k string) string {
	if len(k) > 12 {
		return k[:12]
	}
	return k
}

// clipLog bounds the rationale log so a long-lived candidate cannot grow a row without limit.
func clipLog(s string) string {
	const max = 8000
	if len(s) <= max {
		return strings.TrimSpace(s)
	}
	return strings.TrimSpace(s[len(s)-max:])
}
