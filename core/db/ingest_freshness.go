package db

import (
	"context"
	"sort"
	"time"
)

// IngestFreshness is the per-source liveness of TG's OWN INPUT.
//
// WHY THIS EXISTS. On 2026-08-05 the alert intake was found to have collapsed 99% five days earlier:
// ingest_alert went from 564 rows on 2026-07-30 to 6 on 2026-08-01 and 0 on 2026-08-05, and triage
// followed it to zero. Nothing alerted. Every dashboard was green, the workers were healthy, the pipelines
// passed, and a platform whose entire purpose is triaging estate alerts had been processing ~1% of its
// normal load for five days.
//
// TG-250's yield register instruments the INTERNAL seams — notify, discovery, wiki, suppression, votes —
// and the front door was never instrumented, so the one signal that mattered did not exist. The reasoning
// was already written down elsewhere in this codebase, on the confidence calibrator: "ABSENT is not ZERO
// ... that silence is indistinguishable from health, which is the failure mode this whole family of alerts
// exists to prevent" (REQ-2022). It was simply never applied to the input.
//
// The pair below is deliberately a PAIR, for the same reason the yield register publishes offered beside
// produced: an age with no baseline cannot be judged. A source that has never delivered is not broken, and
// a source silent for six hours is only interesting if it used to speak.
type IngestFreshness struct {
	SourceID string
	// LastSeen is the newest received_at for this source. Zero when the source has never delivered.
	LastSeen time.Time
	// RecentTotal is how many alerts this source delivered in the baseline window. It is the DENOMINATOR:
	// silence from a source with RecentTotal 0 is unremarkable, silence from one with RecentTotal 500 is
	// the estate going deaf.
	RecentTotal int64
	// RecentUnattributed is how many of RecentTotal named NO MACHINE — neither a host nor a subject IP
	// (TG-373). It is a NUMERATOR over the denominator above, and the pair is the point: 0 unattributed out
	// of 0 delivered says nothing, 48 out of 165 is a source whose incidents cannot be blast-radius
	// reasoned, deduped against the estate, or matched to their own ticket.
	//
	// A workload-only alert (a Kubernetes deployment with no node label) legitimately names no machine, so
	// a non-zero value is not by itself a defect — it is the fraction that matters, and it was previously
	// discoverable only by querying the database by hand.
	RecentUnattributed int64
}

// DeclaredButSilent is a source the deployment CONFIGURED that has never delivered a single alert.
//
// This is the gap the freshness store above cannot see, stated as a type rather than a comment because it
// is a different failure with a different remedy. Sources there are discovered FROM THE DATA, so a source
// with no rows has no row to go stale — it is invisible, not quiet.
//
// CrowdSec is the standing instance (TG-291): the boot log advertises it as a capability and the
// all-time distinct source list in ingest_alert is librenms-dc1, pve-liveness, prometheus-alertmanager,
// librenms-dc2. Four entries, never a fifth. Not suppressed, not filtered — never arrived.
type DeclaredButSilent struct {
	SourceID string
}

// SourcesNeverSeen returns the declared source TYPES that have never appeared in ingest_alert.
//
// The caller supplies the declared set, because the module registry — not the database — is the authority
// on what this deployment believes it ingests. Passing it in also keeps this store read-only over one
// table and free of a dependency on the registry.
//
// An empty declared set returns nothing, which is correct and is why the caller must publish its own
// vacuity floor: "no declared sources" and "every declared source is delivering" are different facts and
// must not render identically.
func (s *IngestFreshnessStore) SourcesNeverSeen(ctx context.Context, declared []string) ([]DeclaredButSilent, error) {
	if len(declared) == 0 {
		return nil, nil
	}
	// SOURCE_TYPE, NOT SOURCE_ID. The module registry declares a source TYPE ("librenms"); ingest rows
	// carry a per-site source ID ("librenms-dc1", "librenms-dc2"). Comparing the declared type
	// against observed IDs marks every multi-site source as never-delivered.
	//
	// That is not hypothetical — it is what the first version of this did. Deployed, it immediately
	// reported tg_ingest_source_never_delivered{source_id="librenms"} = 1 for the source responsible for
	// 2,692 of the estate's alerts. A false positive on the busiest source is worse than no gauge: it is
	// the fastest way to teach an operator that this metric lies.
	rows, err := s.p.Query(ctx, `SELECT DISTINCT source_type FROM ingest_alert`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		seen[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []DeclaredButSilent
	for _, d := range declared {
		if !seen[d] {
			out = append(out, DeclaredButSilent{SourceID: d})
		}
	}
	return out, nil
}

// IngestFreshnessStore reads per-source intake liveness. Read-only by construction: one bound SELECT, no
// mutation. It reports what ARRIVED, never what was acted on.
type IngestFreshnessStore struct{ p *Pool }

// NewIngestFreshnessStore returns the Postgres-backed intake freshness reader.
func NewIngestFreshnessStore(p *Pool) *IngestFreshnessStore { return &IngestFreshnessStore{p: p} }

// Sources returns one row per source_id seen in the baseline window, newest-delivery first.
//
// The window is a parameter rather than a constant because the right baseline is deployment-shaped: an
// estate that alerts hourly and one that alerts weekly disagree about what "gone quiet" means.
//
// IMPORTANT: sources are discovered from the DATA, not from a configured list. A source that stops
// existing in config but is still delivering must remain visible, and — the case that actually happened —
// a source that is configured but has NEVER delivered cannot be discovered here at all. That second gap is
// real and is why this is only half the answer; the module registry knows the configured set, and
// reconciling the two is follow-up work recorded in TG-336.
func (s *IngestFreshnessStore) Sources(ctx context.Context, window time.Duration) ([]IngestFreshness, error) {
	rows, err := s.p.Query(ctx, `
		SELECT source_id,
		       MAX(received_at)                                        AS last_seen,
		       COUNT(*) FILTER (WHERE received_at > now() - $1::interval) AS recent_total,
		       -- Unattributed: no hostname AND no subject address. Scoped to the SAME window as
		       -- recent_total so the two are a numerator and denominator of one population; counting
		       -- unattributed over all time against a windowed total would report a ratio of nothing.
		       COUNT(*) FILTER (
		         WHERE received_at > now() - $1::interval
		           AND btrim(coalesce(host,'')) = ''
		           AND subject_ip IS NULL
		       )                                                       AS recent_unattributed
		FROM ingest_alert
		GROUP BY source_id

		UNION ALL

		-- CLEARS COUNT AS TRAFFIC (TG-393). Raises land in ingest_alert; RECOVERIES land here, and this
		-- read used to ignore them — so a source whose current traffic is entirely recovery notifications
		-- read as SILENT. That is not an edge case, it is the recovery phase of every incident.
		--
		-- Measured on production 2026-08-07: librenms-dc2's last raise was 04:26 and its last recovery
		-- 13:56 — NINE AND A HALF HOURS later — while AlertSourceWentSilent fired against it. A false
		-- positive on a healthy feed trains an operator to ignore the alert and buries the one genuine
		-- silence there is (pve-liveness, quiet since 2026-07-31, TG-350).
		--
		-- source_id IS NOT NULL excludes rows written before migration 0068, which carry no source and are
		-- deliberately not backfilled. Their absence can only make a source look STALER than the truth,
		-- never fresher — the safe direction, degrading to the old behaviour rather than inventing recency.
		--
		-- A recovery is never counted as UNATTRIBUTED: that ratio is about alerts arriving without a
		-- subject, and a clear names the incident it closes. Feeding zeros keeps the union's two arms
		-- shape-compatible without polluting a numerator that means something else.
		SELECT source_id,
		       MAX(received_at)                                        AS last_seen,
		       COUNT(*) FILTER (WHERE received_at > now() - $1::interval) AS recent_total,
		       0                                                       AS recent_unattributed
		FROM ingest_transition
		WHERE source_id IS NOT NULL
		GROUP BY source_id
		ORDER BY 2 DESC`, window.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// COLLAPSE THE UNION'S TWO ARMS PER SOURCE. UNION ALL yields up to two rows per source (raises and
	// clears); a source must appear ONCE, with the NEWEST delivery of either kind and the SUM of both
	// recent counts. Emitting both rows would double-count the source in every consumer and let the older
	// arm's timestamp win an ordering.
	byID := map[string]*IngestFreshness{}
	order := []string{}
	for rows.Next() {
		var sid string
		var last *time.Time
		var recent, unattributed int64
		if err := rows.Scan(&sid, &last, &recent, &unattributed); err != nil {
			return nil, err
		}
		f, seen := byID[sid]
		if !seen {
			f = &IngestFreshness{SourceID: sid}
			byID[sid] = f
			order = append(order, sid)
		}
		if last != nil && last.After(f.LastSeen) {
			f.LastSeen = *last
		}
		f.RecentTotal += recent
		f.RecentUnattributed += unattributed
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]IngestFreshness, 0, len(order))
	for _, id := range order {
		out = append(out, *byID[id])
	}
	// Newest-delivery first, as the single-arm query promised.
	sort.SliceStable(out, func(i, j int) bool { return out[i].LastSeen.After(out[j].LastSeen) })
	return out, nil
}
