package falsify

// Discovery capture: the SOURCE side of the OpenAI-style three-set eval flywheel (regression / discovery /
// sealed holdout). The verify-time Scorer already surfaces every scored DEVIATION — a committed prediction
// that reality falsified (the model named the wrong hosts; the observed cascade carried surprise hosts the
// prediction never predicted). Those live-scored mispredictions are the richest possible source of real
// regression cases, so the Scorer CAPTURES each one into a rolling discovery corpus keyed by the deviation
// SIGNATURE. Capture is strictly ADDITIVE and SIDE-EFFECT-FREE on the eval gate: it writes to a separate
// holding area, never touches the immutable prediction row, the mechanical verdict, or the confusion matrix,
// and a capture blip is counted (never fatal) exactly like a verdict-write blip. Promotion of a captured
// deviation INTO the deterministic falsifiability regression suite is a deliberate, audited, deterministic
// step that lives in the eval package (never here) — this file only captures.

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/verify"
)

// DiscoveryRecord is one live-scored deviation captured for the discovery corpus. It is a faithful, promotable
// snapshot of a single verify-time misprediction: the committed prediction's identity, the typed deviation
// breakdown read straight off verify.VerdictDetail (SurpriseHosts / Mismatches — never re-derived here), the
// OBSERVED post-incident cascade (the outcome record a falsifiability scenario needs), and the confusion
// matrix the deterministic scorer already produced. NON-SECRET by construction: only host / rule / site slugs
// and hashes — no argv, credential, or token material can land here.
type DiscoveryRecord struct {
	ActionID       string
	PlanHash       string
	PredictionHash string
	TargetHost     string
	Site           string
	Verdict        safety.Verdict
	SurpriseHosts  []string               // the deviation triggers (off the typed VerdictDetail; sorted+deduped)
	Mismatches     []verify.RuleMismatch  // predicted-host / unpredicted-rule partials (off the typed VerdictDetail)
	Observed       []verify.ObservedAlert // the observed post-incident cascade — the promotable outcome record
	Score          Score                  // the verify-time confusion matrix (tp/fp/fn/control_tp/control_fp)
	CommittedAt    time.Time
	ObservedAt     time.Time
}

// DeviationKey is the misprediction SIGNATURE used to deduplicate captures and count reproductions: the same
// target mispredicting the same surprise-host set across DIFFERENT incidents is the SAME discovery case (a
// deviation that reproduces), even though each carries a distinct action_id. Keying on (target, site,
// sorted-surprise-hosts) — not action_id — is what makes "reproduces >= N" a real, promotion-gating signal.
func (r DiscoveryRecord) DeviationKey() string {
	hosts := append([]string(nil), r.SurpriseHosts...)
	sort.Strings(hosts)
	return strings.Join([]string{r.TargetHost, r.Site, strings.Join(hosts, ",")}, "|")
}

// DiscoveryWriter is the repository seam the Scorer captures scored deviations through. Capture reports whether
// the record was NEWLY captured (a first sighting of this signature) vs a reproduction of one already held —
// mirroring the idempotent-first-wins shape of the other verify-time seams. The in-memory MemDiscoveryCorpus
// is the oracle twin and the in-process rolling buffer; a durable backend (a JSON corpus file, or a future
// pgx store) satisfies the same seam. Optional on the Scorer: nil ⇒ deviations are still scored + logged, just
// not captured (honest zeros, never a panic).
type DiscoveryWriter interface {
	Capture(ctx context.Context, rec DiscoveryRecord) (bool, error)
}

// CapturedDeviation is a discovery-corpus entry as read back for promotion: the captured record plus how many
// times its signature has reproduced and the first/last time it was seen.
type CapturedDeviation struct {
	Record        DiscoveryRecord
	Reproductions int
	FirstSeen     time.Time
	LastSeen      time.Time
}

// MemDiscoveryCorpus is the in-memory ROLLING discovery corpus: a bounded, deduplicated holding area for
// live-scored deviations, keyed by DeviationKey. It is the oracle twin of any durable backend and doubles as
// the worker's in-process capture buffer. Dedup is first-wins per signature (a reproduction increments a
// count, it does not re-insert). The size bound is honest — when a NEW signature arrives at capacity the
// OLDEST is evicted (rolling = the most-recent N distinct deviations) and its key is recorded in Dropped so
// the caller can LOG it; there is never a silent cap. Guarded by a mutex — capture may run concurrently with
// a snapshot read.
type MemDiscoveryCorpus struct {
	mu      sync.Mutex
	cap     int
	byKey   map[string]*discEntry
	order   []string // keys in insertion order — the FIFO eviction order at capacity
	dropped []string // keys evicted by the rolling cap, oldest-eviction first (never silently discarded)
}

type discEntry struct {
	rec           DiscoveryRecord
	reproductions int
	firstSeen     time.Time
	lastSeen      time.Time
}

// DefaultDiscoveryCap bounds the in-memory rolling corpus. It is generous — the discovery corpus is periodically
// drained into the durable eval corpus — but finite, so a long-running worker never grows it unbounded.
const DefaultDiscoveryCap = 1024

// NewMemDiscoveryCorpus returns an empty rolling discovery corpus. A capacity <= 0 uses DefaultDiscoveryCap.
func NewMemDiscoveryCorpus(capacity int) *MemDiscoveryCorpus {
	if capacity <= 0 {
		capacity = DefaultDiscoveryCap
	}
	return &MemDiscoveryCorpus{cap: capacity, byKey: map[string]*discEntry{}}
}

// compile-time proof the in-memory corpus satisfies the seam the Scorer captures through.
var _ DiscoveryWriter = (*MemDiscoveryCorpus)(nil)

// Capture records a scored deviation. A first sighting of the signature inserts (evicting the oldest entry if
// at capacity, and recording the eviction in Dropped); a reproduction increments the count and advances
// last-seen. Returns whether the record was newly captured (true) vs a reproduction (false).
func (c *MemDiscoveryCorpus) Capture(_ context.Context, rec DiscoveryRecord) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := rec.DeviationKey()
	if e, ok := c.byKey[key]; ok {
		e.reproductions++
		if rec.ObservedAt.After(e.lastSeen) {
			e.lastSeen = rec.ObservedAt
		}
		return false, nil
	}
	if c.cap > 0 && len(c.byKey) >= c.cap {
		oldest := c.order[0]
		c.order = c.order[1:]
		delete(c.byKey, oldest)
		c.dropped = append(c.dropped, oldest) // rolling cap — never silent; the caller logs Dropped()
	}
	c.byKey[key] = &discEntry{rec: rec, reproductions: 1, firstSeen: rec.ObservedAt, lastSeen: rec.ObservedAt}
	c.order = append(c.order, key)
	return true, nil
}

// Snapshot returns the captured deviations sorted by signature (deterministic), each with its reproduction
// count — the shape the eval-side ingest drains into the durable rolling corpus.
func (c *MemDiscoveryCorpus) Snapshot() []CapturedDeviation {
	c.mu.Lock()
	defer c.mu.Unlock()
	keys := make([]string, 0, len(c.byKey))
	for k := range c.byKey {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]CapturedDeviation, 0, len(keys))
	for _, k := range keys {
		e := c.byKey[k]
		out = append(out, CapturedDeviation{Record: e.rec, Reproductions: e.reproductions, FirstSeen: e.firstSeen, LastSeen: e.lastSeen})
	}
	return out
}

// Dropped returns the keys the rolling cap evicted (a copy), so the worker can log exactly what was shed —
// there is never a silent cap.
func (c *MemDiscoveryCorpus) Dropped() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.dropped...)
}

// Len is the number of distinct deviation signatures currently held.
func (c *MemDiscoveryCorpus) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.byKey)
}
