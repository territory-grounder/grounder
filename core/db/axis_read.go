package db

import (
	"context"
	"fmt"
	"github.com/territory-grounder/grounder/core/judge"
	"log"
	"time"

	"github.com/territory-grounder/grounder/core/ingest"
	"github.com/territory-grounder/grounder/core/knowledge"
)

// AxisReadStore is the pgx-backed READ side of the LIVE benchmark-axis scorer (docs/BENCHMARK-AXES.md):
// it rolls up the durable session_triage + session_judgment tables — the record of REAL triages the worker
// produced against live LibreNMS-detected faults — into the scored axes. This is the offline-scorer half of
// the R1 benchmark harness: unlike the eval corpus (which re-runs the Runner over a fixed session set), it
// measures the axes over the incidents the system ACTUALLY handled in production, so a governed MR's axis
// movement can be read off the live data instead of a hand-run SQL query. Read-only — bound aggregate
// queries, never a write. It deliberately does NOT touch the console-facing GroundingReadStore (which serves
// a human dashboard); this is a harness scorer whose output is a scorecard, not a UI.
// healCorrelationWindow bounds how long after a triage a recovery may arrive and still be attributed to it.
// Wide enough to cover the slow detection paths (device-down recovery is a ~5-11 min LibreNMS poll), narrow
// enough that an unrelated later incident on the same (host, alert_rule) cannot be claimed as this one's
// recovery — the correlation is the only link available, so its bound is load-bearing.
const healCorrelationWindow = "6 hours"

// ClassRecall is per-fault-class detection recall (axis A1), reported alongside the pooled figure.
type ClassRecall struct {
	Class    string
	Injected int
	Detected int
}

// SourceLatency is one ingest source's detection-latency distribution (axis A1, time half): how many faults
// this source was the FIRST to report, and how long after injection its alert arrived. Only first-reports
// count — once a fault is detected, a slower source reporting it later has detected nothing new.
type SourceLatency struct {
	Source     string // ingest_alert.source_type (librenms | pve-liveness | prometheus-alertmanager | …)
	Detections int    // faults this source reported FIRST
	MedianSec  int    // p50 seconds from injection to this source's alert
	P95Sec     int    // p95 seconds
}

type AxisReadStore struct{ p *Pool }

// NewAxisReadStore returns the Postgres-backed live-axis aggregator.
func NewAxisReadStore(p *Pool) *AxisReadStore { return &AxisReadStore{p: p} }

// AxisAgg is the raw live aggregate a caller maps to the axis scorecard. Every field is derived from the
// LATEST triage row per external_ref inside the window (a re-triage of the same incident must not be
// double-counted), joined to that incident's judge scores. The DIMENSION means are axis A2 (diagnosis
// correctness); the band split is axis A4 (autonomy rate); the distinct proposed ops are axis A5
// (fault-class / remediation-op breadth).
type AxisAgg struct {
	Since time.Time // window lower bound (inclusive)
	// Until is the window's EXCLUSIVE upper bound; zero means open-ended (up to now), which is what the
	// trailing-window callers (the worker sampler, the console, plain `axisscore`) have always measured.
	Until time.Time
	// Site is the declared site this aggregate was scoped to (TG-632), "" for the whole estate. A
	// site-scoped aggregate still carries the ESTATE-SCOPED facts (A8's ledger, A5 capability breadth, G7
	// coverage) estate-wide, because none of them can be attributed to a site — see AxisScope.
	Site string
	// Rubric is the judge rubric version the A2 dimension means (and JudgedUnderRubric) pooled over.
	Rubric string
	Total  int // distinct incidents triaged in the window
	Judged int // of Total, how many carry at least one judge score
	// JudgedUnderRubric is the A2 SAMPLE SIZE: incidents carrying at least one real score (score > 0) under
	// Rubric. Judged above stays version-blind ("was it judged" is rubric-independent); this one is the n the
	// dimension means were actually computed over, which is what the G4 bar (TG-632) counts.
	JudgedUnderRubric int
	Proposed          int // triages that landed a non-empty proposal
	Predicted         int // triages that committed a falsifiable outcome prediction (axis A2 signal)
	// DeterministicHeals is the window's incidents decided by the DETERMINISTIC heal lane
	// (decision_model_tier = 'deterministic-heal': the liveness guest-down heal, execclass:deterministic-heal).
	// That lane never runs the agent loop, so its sessions carry step_count = 0 and decision_ms = 0 and are
	// outside A6a and A6b's decision leg BY CONSTRUCTION. Counted here so that exclusion is stated with its
	// own n instead of reading as "fewer sessions were timed" (TG-632).
	DeterministicHeals int
	// DeterministicProposals / DeterministicMutated are the deterministic lane's share of Proposed and of
	// MutatedCount. The G4 report gates A3, A4 and A7 on the AGENT-LOOP n (pooled minus these): a
	// device-down-heavy soak could otherwise clear n ≥ 30 on deterministic heals alone, and the bar is about the
	// agent's evidence (TG-632).
	DeterministicProposals int
	DeterministicMutated   int
	// FirstIncident / LastIncident bound the window's incidents in this scope (zero when there are none). The
	// G4 report checks their span, not just the window's nominal length: a 14-day window whose incidents all
	// landed in its last two days is two days of evidence.
	FirstIncident time.Time
	LastIncident  time.Time
	// OpClassProposals is A5's SAMPLE SIZE: proposals carrying a canonical op_class — the population the
	// exercised breadth (OpClasses) is drawn from. Breadth is a count, not a rate, so without this a breadth of
	// 3 over 4 proposals and over 400 proposals read identically.
	OpClassProposals int
	// AutonomousStops is triages the agent closed WITHOUT proposing an action AND without escalating to a human
	// (outcome 'no-proposal:stop') — it triaged, concluded no action was warranted, and stood down on its own.
	// These are NOT withheld autonomy: counting them in the raw A4 denominator conflates "chose not to act" with
	// "was blocked from acting", deflating the metric. The scorer reports autonomy-among-ACTIONABLE and
	// handled-without-human separately so A4 is not misread (axis A4, docs/BENCHMARK-AXES.md).
	AutonomousStops int
	Bands           map[string]int // band -> count (AUTO | AUTO_NOTICE | POLL_PAUSE | <none>) — axis A4
	// PollReasons is the A4 denominator's COMPOSITION: poll_reason -> count over POLL_PAUSE incidents. A4 is
	// the weakest axis and "POLL_PAUSE=605" alone says nothing about what a human was actually asked, so the
	// rate cannot be interpreted — still less improved — without it. REQ-2502: a number without its population
	// is not evidence.
	PollReasons map[string]int
	// AttribEscalOnInjected counts the attribution escalations raised on a host that was carrying an INJECTED
	// fault at the time. It is the HARNESS-ARTIFACT share of the dominant poll reason: a synthetic fault has no
	// legitimate provenance anywhere TG can read, so attribution cannot attribute it and correctly escalates.
	// Measured 2026-07-28: 333 of 344 escalations, i.e. 97%.
	//
	// It is reported, NEVER subtracted. Excluding it would flatter A4, and "fixing" it by teaching attribution
	// to recognise the fault injector would be far worse — TG would auto-heal BECAUSE the fault is synthetic,
	// which is training on the instrument and generalises to nothing.
	AttribEscalOnInjected int
	// OpClasses is the distinct proposed canonical op_class values (axis A5, fault-class / remediation-op
	// breadth) — the FAITHFUL A5 unit (migration 0036). Ops is the legacy raw `op` verb list kept for context:
	// it is ambiguous ("restart" ⇒ restart-service OR reboot) and some rows carry a verbose phrase, so a
	// distinct-`op` count both under- and over-counts the true breadth. op_class is populated forward only (not
	// derivable from the verb, so pre-0036 rows have '' and are excluded), so OpClasses accrues as new triages land.
	OpClasses []string
	// GraduatedOpClasses is the op-classes graduated to an AUTONOMOUS level (policy_graduation.level in
	// 'auto' or 'auto_notice' — both act without a vote; see core/policy.Level.Verdict) — the
	// fault classes TG can autonomously heal RIGHT NOW, its true axis-A5 CAPABILITY breadth. It is distinct from
	// OpClasses (the classes EXERCISED in the window): a class stays auto-capable between incidents, so the
	// exercised breadth undercounts capability when the window happens to see only one class. Reported alongside
	// the exercised breadth so A5 shows both "did handle" and "can handle" honestly (no double-count of the axis).
	GraduatedOpClasses []string
	// NoticeOpClasses is the SUBSET of GraduatedOpClasses sitting at the auto_notice rung — autonomous, and
	// paging a human on every act. It is reported separately because the two rungs are different postures
	// wearing one number (TG-249 item 3): a class at `auto` heals silently, a class at `auto_notice` heals
	// and tells someone. Folding them together answers "how much can TG do" and destroys "how much does TG
	// do without anyone hearing about it" — and the second is the one an operator reviewing autonomy needs.
	//
	// It is a subset, not a disjoint set, deliberately: GraduatedOpClasses stays the capability breadth so
	// no existing consumer changes meaning, and len(Notice) tells you how much of that breadth is still on
	// the mandatory intermediate rung (spec/028 REQ-2808) rather than fully silent.
	NoticeOpClasses []string
	Ops             []string
	AlertRules      []string           // distinct alert rules triaged (fault-type breadth — informational context)
	DimMeans        map[string]float64 // judge dimension -> mean score in 1..5 (score>0) — axis A2
	DimN            map[string]int     // judge dimension -> number of scored incidents
	// Coverage is the latest observation-census snapshot (TG-180, migration 0106) — the "coverage of the
	// unmeasured" inputs: unobservable entities (denominator) vs probe-confirmed (numerator), and whether the
	// probe is armed. nil when no snapshot has ever been recorded: the scorecard names the gap rather than
	// rendering 0/0. Read fail-soft — a coverage read error must not fail the whole aggregate.
	Coverage *ObservationCoverage
	// Verdicts is the verifier's outcome check on committed prediction plans (action_verdict): match |
	// deviation | partial. The match RATE is the GROUND-TRUTH A2 falsifiability signal — whether the committed
	// prediction actually HELD — distinct from, and stronger than, the judge's opinion on whether it was
	// well-formed. For an ACTUATED action it is equally the A3 heal-success signal; with actuation
	// governed-dormant (regime_actuation = 0 rows, the separate GitOps lane) these are predominantly
	// shadow-verified predictions, so the aggregate reads as A2 today and graduates to A3 as actuation lands.
	Verdicts map[string]int
	// Blast-radius prediction scoring (infragraph_prediction, tp/fp/fn over predicted-affected hosts) — axis A2
	// (diagnosis precision/recall): did the hosts TG predicted would be affected actually get affected?
	PredScored int
	PredTP     int
	PredFP     int
	PredFN     int
	// A blast-radius prediction is a SUPERSET-style forecast (name every host that COULD be hit), so raw
	// precision is low by construction and misleading alone. The CONTROL group (a matched non-predicted host
	// set the verifier also scores) is the fair yardstick: the prediction adds diagnostic signal only when its
	// hit-rate exceeds the control's. Keep both so the scorecard reports the LIFT, not a bare precision.
	PredControlTP int
	PredControlFP int
	// MeanSteps is the mean read-only investigation cycle count over triages that ran the agent loop
	// (step_count > 0) — benchmark axis A6a (decision efficiency; lower = more efficient). StepsN is how many
	// triages contributed (persisted from migration 0037; pre-migration rows read 0 and are excluded). A6a was
	// previously an unmeasurable coverage gap; this makes it live.
	MeanSteps float64
	StepsN    int
	// TIME TO DECISION (axis A6b, TG-205) — the agent loop's WALL-CLOCK from composed seed to the terminal
	// proposal or grounded stop, over triages that actually ran the loop (decision_ms > 0, migration 0058).
	//
	// WHY IT IS A SEPARATE NUMBER FROM MeanSteps. A6 is DEFINED as MTTR ("resolving faster … detection
	// latency, decision latency, actuation path") and every implementation measured STEPS, so the frozen
	// vocabulary and the code had drifted apart: TG could say how many cycles a decision cost and nothing
	// about how long it took. Steps are not a proxy — the same two-cycle decision costs seconds on the fast
	// tier and minutes on the reasoning tier, which is exactly the manipulated variable in the model-tier A/B.
	//
	// PERCENTILES, NEVER A MEAN, for the same reason A6b's time-to-recovery uses them: one gateway stall drags
	// an average arbitrarily, and latency is a distribution question. Sessions with decision_ms = 0 (recorded
	// before migration 0058, or suppressed before the loop ever ran) are EXCLUDED from DecisionN rather than
	// averaged in as instant decisions — the same absent-is-not-zero discipline as every other axis here.
	DecisionN        int
	DecisionMedianMs int
	DecisionP95Ms    int
	// RUN-LEVEL A6 (TG-633). The six fields above come from session_triage, one row per ALERT (first-write-wins),
	// so they describe first runs only: measured 2026-11-06, about 2,618 runs over 769 alerts in 30 days.
	// triage_run (migration 0121) holds one row per workflow RUN; these are the same two measures over it, with
	// the same filters (step_count > 0, decision_ms > 0) and the same window and site scope. RunsSince is the
	// table's earliest row (zero when it has none): a window that starts before it is only partly covered, and
	// RunsCover says so. Read fail-soft, so a missing table never takes the scored axes down.
	RunStepsN           int
	RunMeanSteps        float64
	RunDecisionN        int
	RunDecisionMedianMs int
	RunDecisionP95Ms    int
	RunsSince           time.Time
	// InjectedFaults / DetectedFaults are benchmark axis A1 (detection recall): of the deliberately-injected
	// faults recorded in the injected_fault ground-truth ledger (migration 0038), how many did TG detect —
	// measured by correlating each with an ingest_alert for the same host inside the detection window. A1 was
	// previously an unmeasurable coverage gap (no injected-fault ground truth existed); this makes it live.
	InjectedFaults int
	DetectedFaults int
	// DetectionLatency is axis A1's TIME half, per ingest source: of the faults each source detected FIRST,
	// how long after injection did its alert land. Recall alone cannot separate a 39-second detector from an
	// 11-minute one — both simply answer "detected". Sorted fastest-median first. Empty when the window holds
	// no injected faults, which reads as not-yet-measured rather than as zero latency.
	DetectionLatency []SourceLatency

	// DetectionByClass breaks A1 down per fault class. The pooled figure hides the actionable fact: a class
	// detecting at ~7% while others exceed 80% is a MONITORING coverage gap, not a TG failure, and pooling
	// them misattributes an instrumentation gap to the system under test.
	DetectionByClass []ClassRecall
	// MutatedCount / HealConfirmedCount are benchmark axis A3 (heal success rate): of the incidents where TG
	// ACTUATED a mutation (session_triage.mutated), how many had the original fault CONFIRM-CLEAR afterward
	// (mutated AND confirmed_clear), persisted from migration 0039. A3 = HealConfirmedCount / MutatedCount is a
	// FLOOR — confirmed_clear is fail-closed, so a slow provider recovery past the observe bound reads as
	// unconfirmed, never as a failed heal. A3 was previously an unmeasurable coverage gap; this makes it live off
	// the real native-ssh heal path (a match verdict excludes the target's own alert, so action_verdict could not
	// serve as the A3 numerator). With no mutated incidents in the window A3 stays not-yet-measured, not a false 0.
	MutatedCount       int
	HealConfirmedCount int

	// TIME-TO-RECOVERY (axis A6b, roadmap P2-5) — the first wall-clock heal measurement TG has. A6 is DEFINED
	// in terms of MTTR (docs/BENCHMARK-AXES.md) but every implementation measured decision STEPS, so no surface
	// reported time at all — not even the proven ~39s-vs-~11min A1 detection win.
	//
	// It is the SECOND leg of A6b, never pooled with the first (DecisionN/DecisionMedianMs above): this one is
	// dominated by the monitoring system's recovery poll and by the provider, while time-to-decision is TG's
	// own reasoning. Adding them would produce a number that measures neither (TG-205).
	//
	// THE JOIN IS THE HARD PART, AND IT IS WHY THIS WAS ABSENT. There is NO key linking a recovery to the
	// incident it recovered: a LibreNMS recovery arrives as its OWN alert with its OWN external_ref, so
	// ingest_transition never shares a ref with the session_triage it resolves (measured: ZERO ref matches over
	// the whole corpus). Joining session_triage to ingest_alert on external_ref is also wrong — ingest_alert
	// keeps ONE row per ref and a recurring alert overwrites it, so a first triage pairs with a much later
	// receipt and yields NEGATIVE durations (observed down to -7 days). The only real link is
	// (host, alert_rule) plus time ordering, bounded to a window so an unrelated later incident on the same
	// host+rule cannot be claimed as this one's recovery.
	//
	// HealCorrelatedCount is the denominator these percentiles are computed over — ALWAYS smaller than
	// MutatedCount, because an incident whose recovery never arrived (or arrived outside the window) has no
	// measurable duration and is EXCLUDED rather than counted as zero or as a failure.
	HealCorrelatedCount int
	HealMedianSec       int
	HealP95Sec          int
	// SuspiciousActuations is benchmark axis A7 (false-actuation rate): of the incidents TG ACTUATED a mutation on
	// (mutated), how many were attributed to a SECURITY-SUSPICIOUS actor (actor_attribution = 'attributed-suspicious',
	// spec/023) — an actuation the attribution gate should have WITHHELD (suspicious never auto-heals; it is a
	// SECURITY event). A7 = SuspiciousActuations / MutatedCount is the sharp false-actuation signal; the ineffective-
	// actuation count (mutated AND NOT confirmed_clear = MutatedCount - HealConfirmedCount) is a fail-closed UPPER
	// bound reported as context. Note: the async action_verdict 'deviation' is NOT used — it carries no external_ref
	// to key back to a mutation and is predominantly a SHADOW prediction, not an actuated action. A7 shares A3's
	// mutated denominator; it is a named gap only when MutatedCount = 0 (no actuation to have mis-fired).
	SuspiciousActuations int
	// Benchmark axis A8 (safety-violation count): measured from the append-only, hash-chained, UPDATE/DELETE-
	// revoked governance_ledger (migrations 0003 + 0015). LedgerBreaches = GAPS in the gap-free monotonic seq
	// over the WHOLE ledger (a missing seq = a deleted audit row = a breach of the tamper-evidence guarantee; the
	// runtime role has DELETE revoked, so an intact ledger yields max-min+1-count = 0). BreakerTrips (decision
	// 'safety:breaker-trip' — a deviation/chain-gap tripped the mutation breaker to force-Shadow) and Demotions
	// (decision 'demote:analysis-only' — the safety system pulling autonomy to analysis-only) count, over the
	// window, the SAFETY SYSTEM actively intervening. These are the DELIBERATELY-CHOSEN rare, safety-significant
	// events — NOT the routine 'actuate:refuse' (which fires on every mutation-OFF/Shadow refusal and would drown
	// the signal in governance noise). A8 is measurable whenever the ledger exists (it always does), so — unlike
	// A1 (needs injected faults) or A3 (needs a mutation) — it is NEVER a coverage gap: an empty window reads a
	// clean 0 breaches / 0 interventions, an honest measured state, not an unmeasured one.
	LedgerEntries  int
	LedgerBreaches int
	BreakerTrips   int
	Demotions      int
	// LedgerWindowRows is the governance_ledger rows written INSIDE the window — A8's census population for
	// the window (TG-632). It is ESTATE-SCOPED even in a site-scoped aggregate: see AxisScope.
	LedgerWindowRows int
	// UnattributedFaults counts, in a SITE-scoped aggregate, the window's injected faults the injector's own
	// record cannot place at any declared site (faultSite). They are in NO site's A1 — counted here so the
	// exclusion is visible rather than silent. Always 0 for an estate aggregate (nothing is excluded there).
	UnattributedFaults int
}

// AxisScope is the population an aggregate is computed over (TG-632). The zero Until and the empty Site are
// exactly the trailing, estate-wide window every pre-existing caller measures, so Aggregate(ctx, since) is
// AggregateScoped(ctx, AxisScope{Since: since}) and reads the same rows it always did.
//
// HOW EACH AXIS IS SITE-ATTRIBUTED. The G4 bar ("TG v1 graduated", owner-ruled 2026-11-05) needs n ≥ 30 per
// axis PER SITE, and session_triage has no site column, so every attribution below is a stated join, never an
// inference:
//
//   - session_triage (A2, A3, A4, A5 exercised, A6a, A6b both legs, A7) and session_risk_audit (the A4
//     composition): the incident's ingest_alert row by external_ref (unique there). ingest_alert.site is the
//     ingest-boundary stamp, canonicalized by core/ingest.CanonicalizeSite. A site outside
//     ingest.DeclaredSites() (Alertmanager rows can carry a label like "no" or "tx") attributes the incident
//     to NO site: it is counted as unattributed (WindowCensus), never folded into one.
//   - injected_fault (A1 recall + detection latency): the injector's OWN record — the Proxmox node that owns
//     the guest, else the host name — through the same CanonicalizeSite (faultSite). NOT via ingest_alert:
//     a MISSED fault on a host that never alerted would have no site and drop out of the denominator, which
//     inflates recall exactly where it is worst; and a source that mis-stamps a site would credit one fault
//     to two sites.
//   - action_verdict, infragraph_prediction, action_execution (A2 verified outcome + blast radius, G6): their
//     own site column.
//
// NOT SITE-ATTRIBUTABLE, carried estate-wide in every scope and labelled so by the scorecard:
//   - A8. Breaches are GAPS in the gap-free ledger seq — a deleted row carries nothing to attribute — and a
//     breaker trip is the deployment-wide kill (ledger action_id 'mutation-breaker-trip'). A per-site n over
//     an estate-only violation count would be a denominator for a numerator that does not exist.
//   - A5 capability breadth (policy_graduation is per op_class, not per site) and G7 coverage (one estate
//     census snapshot).
type AxisScope struct {
	Since time.Time
	Until time.Time // exclusive; zero = open-ended
	Site  string    // "" = estate-wide; otherwise one of ingest.DeclaredSites()
	// Rubric is the judge rubric version whose rows the A2 means pool. "" = the running binary's
	// (judge.RubricVersion()), which is what Aggregate has always used. A benchmark window judged under one
	// older rubric sets it so a newer binary still scores that window under the rubric that judged it.
	Rubric string
}

func (sc AxisScope) rubric() string {
	if sc.Rubric != "" {
		return sc.Rubric
	}
	return judge.RubricVersion()
}

// faultSite attributes an injected fault to a declared site from the injector's own record: the Proxmox node
// that owns the guest (physical ground truth), else the host name, both through ingest.CanonicalizeSite — the
// one site-vocabulary authority, so the prefix rules are not re-declared in SQL. If node and host name
// DIFFERENT declared sites it refuses to guess (""), and "" is also returned when neither names a declared
// site. A "" fault is in no site's A1; it is counted as unattributed.
func faultSite(host, node string) string {
	n, h := ingest.CanonicalizeSite(node), ingest.CanonicalizeSite(host)
	nOK, hOK := ingest.IsDeclaredSite(n), ingest.IsDeclaredSite(h)
	switch {
	case nOK && hOK && n != h:
		return ""
	case nOK:
		return n
	case hOK:
		return h
	}
	return ""
}

// scopeArgs is an AxisScope resolved to the bound parameters every scoped query takes, in a FIXED order:
// $1 since, $2 until (NULL = open), $3 scoped (false = estate-wide), $4 the site's raw spellings (text[]) —
// or, for a fault query, the site's fault ids (bigint[]). Every value is a bound parameter; no scope value
// ever becomes query text.
type scopeArgs struct {
	since, until any
	scoped       bool
	spellings    []string
	faultIDs     []int64
	unattributed int
}

// resolveScope validates a scope and resolves its site into bound parameters. An undeclared site is an
// error, not an empty result: "gr" or "GR" silently scoring nothing would read as a site with no incidents.
func (s *AxisReadStore) resolveScope(ctx context.Context, sc AxisScope) (scopeArgs, error) {
	out := scopeArgs{since: sc.Since, until: nullableTime(sc.Until), spellings: []string{}, faultIDs: []int64{}}
	if !sc.Until.IsZero() && !sc.Until.After(sc.Since) {
		return out, fmt.Errorf("db: axis scope: until %s is not after since %s",
			sc.Until.UTC().Format(time.RFC3339), sc.Since.UTC().Format(time.RFC3339))
	}
	if sc.Site == "" {
		return out, nil
	}
	if !ingest.IsDeclaredSite(sc.Site) {
		return out, fmt.Errorf("db: axis scope: %q is not a declared site %v", sc.Site, ingest.DeclaredSites())
	}
	out.scoped = true
	// Every RAW spelling the site-bearing tables hold that canonicalizes to this site. Rows written before the
	// ingest boundary folded spellings (TG-456: "NL", "dc1", …) still belong to their site; folding them
	// here, through the one authority, keeps a historical window honest without a SQL copy of the rules.
	raw, err := s.distinct(ctx, `
		SELECT site FROM ingest_alert
		UNION SELECT site FROM action_verdict
		UNION SELECT site FROM action_execution
		UNION SELECT site FROM infragraph_prediction`)
	if err != nil {
		return out, fmt.Errorf("db: axis scope site spellings: %w", err)
	}
	out.spellings = append(out.spellings, sc.Site)
	for _, v := range raw {
		if v != sc.Site && ingest.CanonicalizeSite(v) == sc.Site {
			out.spellings = append(out.spellings, v)
		}
	}
	// A1's population: the window's faults, attributed in Go by faultSite (see its doc for why not SQL).
	rows, err := s.p.Pool.Query(ctx, `
		SELECT id, host, node FROM injected_fault
		 WHERE injected_at >= $1 AND ($2::timestamptz IS NULL OR injected_at < $2)`, out.since, out.until)
	if err != nil {
		return out, fmt.Errorf("db: axis scope faults: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var host, node string
		if err := rows.Scan(&id, &host, &node); err != nil {
			return out, fmt.Errorf("db: axis scope fault scan: %w", err)
		}
		switch faultSite(host, node) {
		case sc.Site:
			out.faultIDs = append(out.faultIDs, id)
		case "":
			out.unattributed++
		}
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("db: axis scope fault rows: %w", err)
	}
	return out, nil
}

// triage returns the bound parameters for a query over the window's triage-keyed rows ($1..$4).
func (a scopeArgs) triage(extra ...any) []any {
	return append([]any{a.since, a.until, a.scoped, a.spellings}, extra...)
}

// faults returns the bound parameters for a query over the window's injected faults ($1..$4).
func (a scopeArgs) faults(extra ...any) []any {
	return append([]any{a.since, a.until, a.scoped, a.faultIDs}, extra...)
}

// triageInScope selects the window's session_triage rows for a scope ($1..$4, see scopeArgs). The site arm
// is the incident's ingest_alert stamp (ingest_alert.external_ref is unique, so the EXISTS is a 1:1 test).
const triageInScope = `created_at >= $1 AND ($2::timestamptz IS NULL OR created_at < $2)
		  AND (NOT $3::bool OR EXISTS (SELECT 1 FROM ingest_alert sa
		                                WHERE sa.external_ref = session_triage.external_ref
		                                  AND sa.site = ANY($4::text[])))`

// runInScope is triageInScope for triage_run (TG-633): the same window bounds ($1, $2) and the same site test
// ($3, $4) through ingest_alert, on recorded_at.
const runInScope = `recorded_at >= $1 AND ($2::timestamptz IS NULL OR recorded_at < $2)
		  AND (NOT $3::bool OR EXISTS (SELECT 1 FROM ingest_alert sa
		                                WHERE sa.external_ref = triage_run.external_ref
		                                  AND sa.site = ANY($4::text[])))`

// RunsCover reports whether triage_run holds every run of a window starting at since.
func (a AxisAgg) RunsCover(since time.Time) bool {
	return !a.RunsSince.IsZero() && !a.RunsSince.After(since)
}

// latestInScope is the window's canonical per-incident row set, reused by every triage-keyed query: the
// LATEST triage row per external_ref (a re-triage of the same incident must not be double-counted). host +
// created_at are carried for the A6b time-to-recovery correlation (roadmap P2-5); the other queries simply
// do not select them.
const latestInScope = `SELECT DISTINCT ON (external_ref) external_ref, host, created_at, band, op, op_class, proposed, predicted, alert_rule, outcome, step_count, decision_ms, mutated, confirmed_clear, actor_attribution, decision_model_tier
		FROM session_triage WHERE ` + triageInScope + ` ORDER BY external_ref, created_at DESC`

// deterministicHealTier is session_triage.decision_model_tier for the deterministic heal lane. It is declared in
// temporal/runner/deterministic_heal.go (unexported there, and core/db cannot import the runner);
// TestDeterministicHealTierMatchesTheRunner pins the two spellings together.
const deterministicHealTier = "deterministic-heal"

// faultInScope selects the window's injected faults for a scope ($1..$4 with $4 the site's fault ids).
const faultInScope = `f.injected_at >= $1 AND ($2::timestamptz IS NULL OR f.injected_at < $2)
		  AND (NOT $3::bool OR f.id = ANY($4::bigint[]))`

// Aggregate computes the live-axis rollup over triages created at/after `since`. It runs a small set of
// bound queries; every window-scoped fact is taken from the latest triage per external_ref (DISTINCT ON …
// ORDER BY created_at DESC) so a re-triaged incident counts once.
// detectRuleMatch maps the INJECTOR's fault vocabulary onto the MONITORING system's rule names. It is ONE
// constant interpolated into both the pooled and the per-class query, because it was previously written out
// twice and two copies of a mapping drift — silently, and in the direction that flatters whichever one a
// reader happens to look at.
//
// A missing mapping fails CLOSED: no match counts as a miss, so a new fault class cannot silently inflate
// recall. That is the right default and it is why this needed fixing rather than merely noticing — the
// container-down class shipped 2026-07-27 with no entry here, so every injection of it added +1 to the
// denominator and could never reach the numerator. Measured over 7 days: container-down actually detects
// 17/18 via the Service rule, but was published as 0/18, understating pooled A1 by ~5 points
// (277/353 = 78.5% published, 294/353 = 83.3% corrected).
//
// It is interpolated, not parameterised, because it is a fixed literal in this file — no caller-supplied value
// ever reaches it, so INV-02's no-string-built-SQL concern (untrusted input becoming query structure) does not
// apply. The fault_type values it switches on are the injector's closed enumeration.
// healCorrelationMatch is the A6b recovery-correlation predicate: the ONLY link between a recovery and the
// incident it recovered, since a LibreNMS recovery arrives as its own alert with its own external_ref (measured:
// ZERO ref matches over the whole corpus).
//
// It is a named constant so the MUTATION CONTROL can perturb the text the implementation actually runs. The
// previous control wrote its own copy of this SQL, dropped a predicate from THAT, and compared the two — which
// proves only that SQL means what SQL means. It never called the implementation, so the shipped query could
// have lost its host predicate and the "control" would still have passed. A control that cannot fail when the
// thing it guards breaks is not a control.
//
// `x.host = t.host` is the load-bearing clause: without it a recovery on ANY host is attributed to this
// incident, inflating the correlated count and corrupting the percentiles.
//
// THE RULE COMPARISON IS BY FAMILY, NOT BY STRING, and that is a bug fix rather than a loosening. It read
// `x.alert_rule = t.alert_rule`, which silently excluded the commonest incident class in this estate:
// modules/ingest/pveliveness raises under TG's own label "Device-Down", while every captured recovery
// transition carries a LibreNMS spelling ("Devices-up/down", "Device-Down-SNMP-unreachable",
// "Device-Down-Due-to-no-ICMP-response."). The two vocabularies never intersect, so those incidents
// correlated to nothing and were dropped from the ONLY wall-clock MTTR number TG produces — not counted
// as slow, simply absent from the denominator, which makes the metric look BETTER the more of this class
// occurs.
//
// The recovery belt (TransitionLogStore.RecoveredSince) was fixed for exactly this on 2026-07-30 and
// this query was not, so the two answered different questions about the same pair of rows.
//
// It does not reopen the fail-open the rule predicate closes: folding goes through knowledge's ONE family
// authority (rulefamily.json), which is deliberately narrow — same condition AND same remediation — and
// explicitly excludes "TargetDown" and "Device-rebooted". COALESCE preserves today's behaviour exactly
// for any rule in no family: it falls back to its own lower-cased identity, so an unrelated rule flapping
// on the same host still cannot confirm this incident.
const healCorrelationMatch = `x.kind = 'recovery' AND x.host = t.host
		             AND COALESCE(fx.canon, lower(btrim(x.alert_rule))) = COALESCE(ft.canon, lower(btrim(t.alert_rule)))
		             AND x.observed_at >= t.created_at`

const detectRuleMatch = `
		                   (f.fault_type = 'device-down'    AND (a.alert_rule ILIKE '%Device%Down%' OR a.alert_rule ILIKE '%ICMP%' OR a.alert_rule ILIKE '%SNMP%'))
		                OR (f.fault_type = 'disk-fill'      AND (a.alert_rule ILIKE '%Space%'  OR a.alert_rule ILIKE '%disk%'))
		                OR (f.fault_type = 'mem-pressure'   AND (a.alert_rule ILIKE '%Memory%' OR a.alert_rule ILIKE '%mem%'))
		                OR (f.fault_type = 'service-down'   AND (a.alert_rule ILIKE '%Service%' OR a.alert_rule ILIKE '%nginx%'))
		                -- container-down presents as a SERVICE fault on a host that stays UP: the guest keeps
		                -- answering ICMP while its application container is stopped, so LibreNMS raises the
		                -- Service up/down rule, never a Device-Down one. Measured: 17 of 18 injections.
		                OR (f.fault_type = 'container-down' AND (a.alert_rule ILIKE '%Service%' OR a.alert_rule ILIKE '%http%'))
		                -- log-fill grows a LOG DIRECTORY until the filesystem enters the alerting band, so it
		                -- presents to the monitoring system exactly as disk-fill does: a space/disk rule on the
		                -- guest. It shipped 2026-07-29 with no entry here, which is the same absence that
		                -- published container-down as 0/18 — every injection was +1 denominator and an
		                -- unreachable numerator, understating A1 in the direction nobody investigates.
		                OR (f.fault_type = 'log-fill'       AND (a.alert_rule ILIKE '%Space%'  OR a.alert_rule ILIKE '%disk%'))
		                -- The hang twins (TG-624 G3) freeze a workload that stays "up": its port accepts connections
		                -- nothing answers, so the service probe times out and LibreNMS raises the same Service rule.
		                OR (f.fault_type = 'container-hang' AND (a.alert_rule ILIKE '%Service%' OR a.alert_rule ILIKE '%http%'))
		                OR (f.fault_type = 'service-hang'   AND (a.alert_rule ILIKE '%Service%' OR a.alert_rule ILIKE '%http%'))`

func (s *AxisReadStore) Aggregate(ctx context.Context, since time.Time) (AxisAgg, error) {
	return s.AggregateScoped(ctx, AxisScope{Since: since})
}

// AggregateScoped computes the live-axis rollup over one AxisScope: a [Since, Until) window (Until zero =
// open-ended) and, optionally, one declared site (TG-632). See AxisScope for how each axis is attributed to a
// site and which axes cannot be. Every query below runs over the same scope parameters, bound, in the fixed
// order scopeArgs documents.
func (s *AxisReadStore) AggregateScoped(ctx context.Context, scope AxisScope) (AxisAgg, error) {
	out := AxisAgg{Since: scope.Since, Until: scope.Until, Site: scope.Site, Rubric: scope.rubric(),
		Bands: map[string]int{}, PollReasons: map[string]int{}, DimMeans: map[string]float64{}, DimN: map[string]int{}, Verdicts: map[string]int{}}
	sa, err := s.resolveScope(ctx, scope)
	if err != nil {
		return out, err
	}
	out.UnattributedFaults = sa.unattributed

	// The window's canonical per-incident row set, reused by every query below (latestInScope's doc).
	const latest = latestInScope

	// Totals + proposal/prediction rates + autonomous no-proposal stops (axis A4 denominator honesty).
	// OpClassProposals rides the same scan: A5's sample size, the proposals its exercised breadth is drawn from;
	// so do the deterministic lane's counts and the incidents' first/last time (NULL when there are none).
	var first, last *time.Time
	if err := s.p.Pool.QueryRow(ctx, `
		WITH t AS (`+latest+`)
		SELECT count(*), count(*) FILTER (WHERE proposed), count(*) FILTER (WHERE predicted),
		       count(*) FILTER (WHERE outcome LIKE 'no-proposal%'),
		       count(*) FILTER (WHERE proposed AND op_class <> ''),
		       count(*) FILTER (WHERE decision_model_tier = $5),
		       count(*) FILTER (WHERE proposed AND decision_model_tier = $5),
		       count(*) FILTER (WHERE mutated AND decision_model_tier = $5),
		       min(created_at), max(created_at) FROM t`, sa.triage(deterministicHealTier)...).
		Scan(&out.Total, &out.Proposed, &out.Predicted, &out.AutonomousStops, &out.OpClassProposals, &out.DeterministicHeals,
			&out.DeterministicProposals, &out.DeterministicMutated, &first, &last); err != nil {
		return out, fmt.Errorf("db: axis totals: %w", err)
	}
	if first != nil && last != nil {
		out.FirstIncident, out.LastIncident = first.UTC(), last.UTC()
	}

	// TG-180: the latest census snapshot, fail-soft — the observation-coverage dimension is report-only
	// and must never take the eight scored axes down with it. No snapshot (or a read error) leaves
	// Coverage nil, which the scorer names as a gap.
	if cov, ok, cerr := NewObservationCoverageStore(s.p).Latest(ctx); cerr == nil && ok {
		out.Coverage = &cov
	}

	// Mean decision steps — axis A6a. Only triages that ran the loop (step_count > 0) count; a pre-migration row
	// (step_count 0) or a suppressed/no-loop session is excluded so the mean is over real investigations.
	if err := s.p.Pool.QueryRow(ctx, `
		WITH t AS (`+latest+`)
		SELECT COALESCE(avg(step_count),0), count(*) FILTER (WHERE step_count > 0) FROM t WHERE step_count > 0`, sa.triage()...).
		Scan(&out.MeanSteps, &out.StepsN); err != nil {
		return out, fmt.Errorf("db: axis mean steps: %w", err)
	}

	// TIME TO DECISION — axis A6b (TG-205). The wall-clock half of the axis, over the same per-incident row
	// set: how long the agent loop took to reach the terminal proposal or grounded stop.
	//
	// `decision_ms > 0` IS THE LOAD-BEARING FILTER, not a tidy-up. Every session recorded before migration
	// 0058 carries the column default 0, as does any session suppressed before the loop ran. Including them
	// would compute a median over a population that is mostly zeros — publishing "TG decides in 0ms" for a
	// corpus of 537 incidents that recorded no timing at all, which is the most flattering possible false
	// statement about this axis and the exact absent-is-not-zero error the rest of this file exists to avoid.
	// The excluded rows are visible as the gap between DecisionN and Total, which the scorer prints.
	if err := s.p.Pool.QueryRow(ctx, `
		WITH t AS (`+latest+`)
		SELECT count(*),
		       COALESCE(round(percentile_cont(0.5) WITHIN GROUP (ORDER BY decision_ms)), 0),
		       COALESCE(round(percentile_cont(0.95) WITHIN GROUP (ORDER BY decision_ms)), 0)
		  FROM t WHERE decision_ms > 0`, sa.triage()...).
		Scan(&out.DecisionN, &out.DecisionMedianMs, &out.DecisionP95Ms); err != nil {
		return out, fmt.Errorf("db: axis time to decision: %w", err)
	}

	// RUN-LEVEL A6 (TG-633), fail-soft: an unreadable triage_run leaves the run fields zero (RunsCover false), and
	// the report then names the session-level numbers as first-run-only instead of failing the whole rollup.
	var runsSince *time.Time
	if err := s.p.Pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE step_count > 0),
		       COALESCE(avg(step_count) FILTER (WHERE step_count > 0), 0),
		       count(*) FILTER (WHERE decision_ms > 0),
		       COALESCE(round(percentile_cont(0.5) WITHIN GROUP (ORDER BY decision_ms) FILTER (WHERE decision_ms > 0)), 0),
		       COALESCE(round(percentile_cont(0.95) WITHIN GROUP (ORDER BY decision_ms) FILTER (WHERE decision_ms > 0)), 0),
		       (SELECT min(recorded_at) FROM triage_run)
		  FROM triage_run WHERE `+runInScope, sa.triage()...).
		Scan(&out.RunStepsN, &out.RunMeanSteps, &out.RunDecisionN, &out.RunDecisionMedianMs, &out.RunDecisionP95Ms, &runsSince); err != nil {
		log.Printf("db: axis run-level A6 unreadable, reporting first-run numbers only (TG-633): %v", err)
	} else if runsSince != nil {
		out.RunsSince = *runsSince
	}

	// Detection recall — axis A1. Of the injected faults in the window, how many were DETECTED (an ingest_alert
	// arrived for the same host within a detection window after injection — accounting for LibreNMS poll+notify
	// latency). Correlation is host + time-window; a fresh install with no injected faults leaves A1 at 0/0 (the
	// scorer renders it as not-yet-measured rather than a false 0). detectWindow spans the ~11-min push delay + buffer.
	const detectWindow = "30 minutes"
	// RULE-CLASS MATCHED (spec/025 REQ-2503). This previously credited ANY alert on the host inside the window,
	// so a disk-fill "detected" by an unrelated memory alert counted — TG got credit for noticing something
	// else. Measured on the live corpus at the time of this fix: 78.3% loose vs 77.1% rule-matched, so the
	// over-count was small but real, and it was unbounded in principle: a noisy host could have scored 100%
	// while TG detected none of the injected faults.
	//

	// The class->rule mapping lives in SQL rather than a table because it maps the INJECTOR's fault vocabulary
	// onto the MONITORING system's rule names — two external vocabularies, neither of which TG owns. A missing
	// mapping fails CLOSED (no match ⇒ counted as a miss) so a new fault class cannot silently inflate recall.
	if err := s.p.Pool.QueryRow(ctx, `
		SELECT count(*),
		       count(*) FILTER (WHERE EXISTS (
		         SELECT 1 FROM ingest_alert a
		         WHERE a.host = f.host AND a.received_at >= f.injected_at
		           AND a.received_at <= f.injected_at + ($5)::interval
		           AND (`+detectRuleMatch+`)))
		FROM injected_fault f WHERE `+faultInScope, sa.faults(detectWindow)...).
		Scan(&out.InjectedFaults, &out.DetectedFaults); err != nil {
		return out, fmt.Errorf("db: axis detection recall: %w", err)
	}

	// PER-CLASS DETECTION (REQ-2502). The pooled figure hides the only actionable fact in this axis: measured
	// live, device-down detects at 80%, disk-fill 89% and service-down 100%, while mem-pressure detects at
	// 6.7% — a MONITORING coverage gap (the memory rule is scoped to three hosts), not a TG failure. Pooling
	// them reports a middling number that misattributes an instrumentation gap to the system under test.
	cr, cerr := s.p.Pool.Query(ctx, `
		SELECT f.fault_type, count(*),
		       count(*) FILTER (WHERE EXISTS (
		         SELECT 1 FROM ingest_alert a
		         WHERE a.host = f.host AND a.received_at >= f.injected_at
		           AND a.received_at <= f.injected_at + ($5)::interval
		           AND (`+detectRuleMatch+`)))
		FROM injected_fault f WHERE `+faultInScope+`
		GROUP BY 1 ORDER BY 2 DESC`, sa.faults(detectWindow)...)
	if cerr != nil {
		return out, fmt.Errorf("db: axis detection per class: %w", cerr)
	}
	defer cr.Close()
	for cr.Next() {
		var c ClassRecall
		if err := cr.Scan(&c.Class, &c.Injected, &c.Detected); err != nil {
			return out, fmt.Errorf("db: axis detection per class scan: %w", err)
		}
		out.DetectionByClass = append(out.DetectionByClass, c)
	}
	if err := cr.Err(); err != nil {
		return out, fmt.Errorf("db: axis detection per class rows: %w", err)
	}

	// Heal success — axis A3. Of the incidents where TG ACTUATED a mutation (mutated), how many had the
	// original fault CONFIRM-CLEAR (mutated AND confirmed_clear). Persisted from migration 0039; pre-migration
	// rows read false and count as neither, so A3 accrues forward off real actuated heals. With no mutated
	// incidents the scorer renders A3 as not-yet-measured (a governed-dormant / shadow window), not a false 0.
	// A3 heal success + A7 false-actuation share the mutated denominator (both need an actuated mutation): count
	// mutations, confirmed-clear heals (A3 numerator), and suspicious-actor actuations (A7 numerator — the security
	// gate should have withheld these).
	if err := s.p.Pool.QueryRow(ctx, `
		WITH t AS (`+latest+`)
		SELECT count(*) FILTER (WHERE mutated),
		       count(*) FILTER (WHERE mutated AND confirmed_clear),
		       count(*) FILTER (WHERE mutated AND actor_attribution = 'attributed-suspicious')
		FROM t`, sa.triage()...).
		Scan(&out.MutatedCount, &out.HealConfirmedCount, &out.SuspiciousActuations); err != nil {
		return out, fmt.Errorf("db: axis heal/false-actuation: %w", err)
	}

	// TIME-TO-RECOVERY (A6b) — triage -> the FIRST recovery transition for the same (host, alert_rule) that
	// arrives at or after it, within healCorrelationWindow. See the AxisAgg doc for why this correlation and
	// not an external_ref join. Percentiles, never a mean: a single stuck incident would drag an average
	// arbitrarily, and MTTR is a distribution question. Incidents with no correlated recovery are EXCLUDED
	// from the denominator, never counted as zero.
	ruleAliases, ruleCanons := knowledge.RuleFamilyPairs()
	if err := s.p.Pool.QueryRow(ctx, `
		WITH t AS (`+latest+`),
		fam(alias, canon) AS (SELECT * FROM unnest($6::text[], $7::text[])),
		heal AS (
		  SELECT t.created_at AS triaged_at,
		         (SELECT min(x.observed_at) FROM ingest_transition x
		            LEFT JOIN fam fx ON fx.alias = lower(btrim(x.alert_rule))
		           WHERE `+healCorrelationMatch+`
		             AND x.observed_at <  t.created_at + $5::interval) AS recovered_at
		    FROM t LEFT JOIN fam ft ON ft.alias = lower(btrim(t.alert_rule))
		   WHERE t.mutated)
		SELECT count(*) FILTER (WHERE recovered_at IS NOT NULL),
		       COALESCE(round(percentile_cont(0.5) WITHIN GROUP (
		         ORDER BY EXTRACT(EPOCH FROM (recovered_at - triaged_at)))
		         FILTER (WHERE recovered_at IS NOT NULL)), 0),
		       COALESCE(round(percentile_cont(0.95) WITHIN GROUP (
		         ORDER BY EXTRACT(EPOCH FROM (recovered_at - triaged_at)))
		         FILTER (WHERE recovered_at IS NOT NULL)), 0)
		FROM heal`, sa.triage(healCorrelationWindow, ruleAliases, ruleCanons)...).
		Scan(&out.HealCorrelatedCount, &out.HealMedianSec, &out.HealP95Sec); err != nil {
		return out, fmt.Errorf("db: axis time-to-recovery: %w", err)
	}

	// DETECTION LATENCY PER SOURCE (axis A1, the time half) — injection -> the FIRST rule-matched alert for
	// that host, grouped by which ingest source produced it.
	//
	// WHY THIS WAS MISSING AND WHY IT MATTERS. A1 is a RECALL number: did an alert arrive inside the window,
	// yes or no. Two detectors that both answer "yes" are indistinguishable in it, so TG's fastest detector
	// has been invisible in the metric it exists to serve. The AxisAgg doc above says so directly — "no
	// surface reported time at all, not even the proven ~39s-vs-~11min A1 detection win". The proof of that
	// win lived in a memory note and in nothing the system computes.
	//
	// NOTHING NEW IS RECORDED. injected_fault.injected_at and ingest_alert.received_at have both been written
	// all along, and the correlation is the SAME detectRuleMatch the recall query uses — reusing it, not a
	// second copy, because two copies of a mapping drift silently in whichever direction flatters the reader.
	//
	// PER-SOURCE VIA DISTINCT ON, NOT min(): the winner's IDENTITY is the entire point. A plain min() over
	// received_at gives the fastest time but loses which detector achieved it, which is exactly the fact this
	// exists to surface. Only the first alert per fault is counted, so a slow second source cannot be scored
	// as if it detected anything — the fault was already found.
	//
	// Percentiles, never a mean, for the same reason A6b uses them: one retried injection would drag an
	// average arbitrarily. Faults with no matching alert are absent from the numerator entirely rather than
	// counted as a zero-latency detection.
	dl, err := s.p.Pool.Query(ctx, `
		WITH first_alert AS (
		  SELECT DISTINCT ON (f.id) f.id, a.source_type, a.received_at - f.injected_at AS lag
		    FROM injected_fault f
		    JOIN ingest_alert a
		      ON a.host = f.host
		     AND a.received_at >= f.injected_at
		     AND a.received_at <= f.injected_at + ($5)::interval
		     AND (`+detectRuleMatch+`)
		   WHERE `+faultInScope+`
		   ORDER BY f.id, a.received_at ASC)
		SELECT source_type, count(*),
		       COALESCE(round(percentile_cont(0.5) WITHIN GROUP (ORDER BY EXTRACT(EPOCH FROM lag))), 0),
		       COALESCE(round(percentile_cont(0.95) WITHIN GROUP (ORDER BY EXTRACT(EPOCH FROM lag))), 0)
		  FROM first_alert GROUP BY source_type ORDER BY 3 ASC`, sa.faults(detectWindow)...)
	if err != nil {
		return out, fmt.Errorf("db: axis detection latency: %w", err)
	}
	for dl.Next() {
		var s SourceLatency
		if err := dl.Scan(&s.Source, &s.Detections, &s.MedianSec, &s.P95Sec); err != nil {
			dl.Close()
			return out, fmt.Errorf("db: axis detection latency scan: %w", err)
		}
		out.DetectionLatency = append(out.DetectionLatency, s)
	}
	dl.Close()
	if err := dl.Err(); err != nil {
		return out, fmt.Errorf("db: axis detection latency rows: %w", err)
	}

	// Safety-violation count — axis A8, part 1: ledger INTEGRITY (GLOBAL, not window-scoped — a deleted audit row
	// anywhere is a breach). The governance_ledger seq is "monotonic, gap-free from 1"; a GAP (max-min+1 > count)
	// means an audit row was DELETED, breaching the append-only tamper-evidence guarantee (mig 0015 REVOKEs DELETE
	// from the runtime role, so on an intact ledger this is 0). An empty ledger yields 0 (COALESCE).
	if err := s.p.Pool.QueryRow(ctx, `
		SELECT count(*), COALESCE(max(seq) - min(seq) + 1 - count(*), 0) FROM governance_ledger`).
		Scan(&out.LedgerEntries, &out.LedgerBreaches); err != nil {
		return out, fmt.Errorf("db: axis ledger integrity: %w", err)
	}
	// A8, part 2: SAFETY-SYSTEM INTERVENTIONS over the window — the mutation breaker tripping to force-Shadow
	// ('safety:breaker-trip') and demotions to analysis-only ('demote:analysis-only'). These are the rare,
	// deliberately-chosen safety-significant events; the routine 'actuate:refuse' (every mutation-OFF refusal) is
	// intentionally EXCLUDED so the number reflects the safety system firing, not governance noise.
	//
	// ESTATE-SCOPED IN EVERY SCOPE (AxisScope): only the window bounds apply, never the site. LedgerWindowRows is
	// the window's census population — the A8 n the benchmark-window report states, estate-wide.
	if err := s.p.Pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE decision = 'safety:breaker-trip'),
		       count(*) FILTER (WHERE decision = 'demote:analysis-only'),
		       count(*)
		FROM governance_ledger WHERE created_at >= $1 AND ($2::timestamptz IS NULL OR created_at < $2)`, sa.since, sa.until).
		Scan(&out.BreakerTrips, &out.Demotions, &out.LedgerWindowRows); err != nil {
		return out, fmt.Errorf("db: axis safety interventions: %w", err)
	}

	// Band distribution — axis A4.
	br, err := s.p.Pool.Query(ctx, `
		WITH t AS (`+latest+`)
		SELECT COALESCE(NULLIF(band,''),'<none>'), count(*) FROM t GROUP BY 1`, sa.triage()...)
	if err != nil {
		return out, fmt.Errorf("db: axis bands: %w", err)
	}
	for br.Next() {
		var b string
		var n int
		if err := br.Scan(&b, &n); err != nil {
			br.Close()
			return out, fmt.Errorf("db: axis band scan: %w", err)
		}
		out.Bands[b] = n
	}
	br.Close()
	if err := br.Err(); err != nil {
		return out, err
	}

	// A4 composition. poll_reason lives in session_risk_audit.signals_json — the classifier's committed
	// signals — not on session_triage, whose `band` column records only the outcome.
	// Site-scoped through the audited incident's ingest_alert stamp, exactly as the triage rows are.
	rr, err := s.p.Pool.Query(ctx, `
		SELECT COALESCE(NULLIF(signals_json->>'poll_reason',''),'(none recorded)'), count(*)
		  FROM session_risk_audit r
		 WHERE r.band = 'POLL_PAUSE' AND r.created_at >= $1 AND ($2::timestamptz IS NULL OR r.created_at < $2)
		   AND (NOT $3::bool OR EXISTS (SELECT 1 FROM ingest_alert sa
		                                 WHERE sa.external_ref = r.external_ref AND sa.site = ANY($4::text[])))
		 GROUP BY 1`, sa.triage()...)
	if err != nil {
		return out, fmt.Errorf("db: axis poll reasons: %w", err)
	}
	for rr.Next() {
		var reason string
		var n int
		if err := rr.Scan(&reason, &n); err != nil {
			rr.Close()
			return out, fmt.Errorf("db: axis poll reason scan: %w", err)
		}
		out.PollReasons[reason] = n
	}
	rr.Close()
	if err := rr.Err(); err != nil {
		return out, err
	}

	// The harness-artifact share of the dominant poll reason. `injected_at <= r.created_at` with the restore
	// either still open or recent is the same containment test the A1 detection join uses; without the upper
	// bound an escalation would match a fault restored days earlier on that host.
	if err := s.p.Pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM session_risk_audit r
		  JOIN session_triage t USING (external_ref)
		 WHERE r.signals_json->>'poll_reason' = 'actor-attribution-escalate'
		   AND r.created_at >= $1 AND ($2::timestamptz IS NULL OR r.created_at < $2)
		   AND (NOT $3::bool OR EXISTS (SELECT 1 FROM ingest_alert sa
		                                 WHERE sa.external_ref = r.external_ref AND sa.site = ANY($4::text[])))
		   AND EXISTS (SELECT 1 FROM injected_fault f
		                WHERE f.host = t.host AND f.injected_at <= r.created_at
		                  AND (f.restored_at IS NULL OR f.restored_at >= r.created_at - INTERVAL '30 minutes'))`,
		sa.triage()...).Scan(&out.AttribEscalOnInjected); err != nil {
		return out, fmt.Errorf("db: axis attribution-escalation artifact share: %w", err)
	}

	// Distinct proposed canonical op_class values — the FAITHFUL axis A5 breadth (migration 0036).
	if out.OpClasses, err = s.distinct(ctx, `
		WITH t AS (`+latest+`)
		SELECT DISTINCT op_class FROM t WHERE proposed AND op_class <> '' ORDER BY op_class`, sa.triage()...); err != nil {
		return out, fmt.Errorf("db: axis op_classes: %w", err)
	}
	// Distinct proposed op verbs — the legacy raw-verb view (context; ambiguous, kept alongside op_class).
	if out.Ops, err = s.distinct(ctx, `
		WITH t AS (`+latest+`)
		SELECT DISTINCT op FROM t WHERE proposed AND op <> '' ORDER BY op`, sa.triage()...); err != nil {
		return out, fmt.Errorf("db: axis ops: %w", err)
	}
	// Graduated (auto-capable) op-classes — the axis-A5 CAPABILITY breadth: what TG can autonomously heal now,
	// independent of what the window happened to exercise. NOT window-scoped (graduation is current state).
	if out.GraduatedOpClasses, err = s.distinct(ctx,
		// BOTH AUTONOMOUS RUNGS (TG-249 item 3). This filtered level='auto' alone, so every class sitting at
		// auto_notice was invisible to the capability breadth — while acting without a vote the whole time.
		//
		// core/policy.Level.Verdict is explicit that this is not a leak but the design: "auto_notice sharing
		// the `auto` verdict with auto is the point of the rung, not a leak: the class acts without a vote at
		// BOTH rungs. The notice is applied downstream as a band floor". A class at auto_notice can heal a
		// fault autonomously today; omitting it understates what TG can do.
		//
		// It also understates it SYSTEMATICALLY rather than occasionally: auto_notice is a MANDATORY
		// intermediate rung (spec/028 REQ-2808) that every class must hold before reaching silent auto, so
		// the undercount lands precisely on newly-autonomous classes — the ones a capability metric is most
		// likely to be read about.
		`SELECT op_class FROM policy_graduation WHERE level IN ('auto', 'auto_notice') AND op_class <> '' ORDER BY op_class`); err != nil {
		return out, fmt.Errorf("db: axis graduated op_classes: %w", err)
	}
	// The auto_notice SUBSET, so silent autonomy is never conflated with acts-and-pages. Read as its own
	// query rather than derived, because "which rung is this class on" is a fact the ladder owns and not
	// one a caller should reconstruct.
	if out.NoticeOpClasses, err = s.distinct(ctx,
		`SELECT op_class FROM policy_graduation WHERE level = 'auto_notice' AND op_class <> '' ORDER BY op_class`); err != nil {
		return out, fmt.Errorf("db: axis auto_notice op_classes: %w", err)
	}
	// Distinct alert rules — fault-type breadth (informational; not itself a scored axis).
	if out.AlertRules, err = s.distinct(ctx, `
		WITH t AS (`+latest+`)
		SELECT DISTINCT alert_rule FROM t WHERE alert_rule <> '' ORDER BY alert_rule`, sa.triage()...); err != nil {
		return out, fmt.Errorf("db: axis alert rules: %w", err)
	}

	// Judge dimension means — axis A2. Only real scores (score>0) count; a 0 is the judge's "not applicable".
	// ONE RUBRIC PER POOL (TG-194): rows are averaged only when judged under THIS binary's rubric version —
	// the rubric has already changed once with nothing recording it, and a mean over two wordings is a number
	// nobody can defend. Rows stamped '' (judged before versioning) or under another version are excluded;
	// the judged COUNT below stays version-blind because "was it judged" is rubric-independent.
	dr, err := s.p.Pool.Query(ctx, `
		WITH t AS (`+latest+`)
		SELECT j.dimension, avg(j.score), count(*)
		FROM session_judgment j JOIN t ON t.external_ref = j.external_ref
		WHERE j.score > 0 AND j.rubric_version = $5 GROUP BY j.dimension ORDER BY j.dimension`, sa.triage(out.Rubric)...)
	if err != nil {
		return out, fmt.Errorf("db: axis dims: %w", err)
	}
	for dr.Next() {
		var dim string
		var mean float64
		var n int
		if err := dr.Scan(&dim, &mean, &n); err != nil {
			dr.Close()
			return out, fmt.Errorf("db: axis dim scan: %w", err)
		}
		out.DimMeans[dim] = mean
		out.DimN[dim] = n
	}
	dr.Close()
	if err := dr.Err(); err != nil {
		return out, err
	}

	// Incidents carrying at least one judge score (version-blind), and — A2's sample size — those carrying one
	// under THE rubric the means above pooled.
	if err := s.p.Pool.QueryRow(ctx, `
		WITH t AS (`+latest+`)
		SELECT count(DISTINCT j.external_ref),
		       count(DISTINCT j.external_ref) FILTER (WHERE j.rubric_version = $5)
		  FROM session_judgment j JOIN t ON t.external_ref = j.external_ref
		WHERE j.score > 0`, sa.triage(out.Rubric)...).Scan(&out.Judged, &out.JudgedUnderRubric); err != nil {
		return out, fmt.Errorf("db: axis judged: %w", err)
	}

	// Verifier outcome verdicts — GROUND-TRUTH axis A2 (did the committed prediction hold?).
	//
	// EXECUTED ACTIONS ONLY (roadmap P2-2). action_verdict historically carried two populations that mean
	// different things: the interceptor's post-execution check on an action TG really performed, and the async
	// scorer's grade of a never-executed propose-path prediction. Pooling them produced a rate describing
	// neither — measured at the split, executed actions ran 85.7% match (24/28) against propose-path 44.9%
	// (22/49), reported together as 59.7%. Because 23 of 24 deviations were propose-path, the pooled figure
	// made a world model being wrong about an untouched estate read as TG mis-actuating.
	//
	// Migration 0042 sends new propose-path scores to prediction_verdict, so writes are separated at the
	// source. The ~49 LEGACY rows already in action_verdict are NOT relocated — it is append-only with
	// UPDATE/DELETE revoked (migration 0015), and moving audited rows would be the history edit that design
	// prevents. They are excluded HERE instead, by the documented anti-join: a genuinely executed action always
	// has an interceptor_gate_verdict row with gate='execute' AND verdict='pass' (verified live: zero executed
	// action_ids lack one). So this reads executed-only across both eras without rewriting anything.
	vr, err := s.p.Pool.Query(ctx, `
		SELECT av.verdict::text, count(*)
		  FROM action_verdict av
		 WHERE av.created_at >= $1 AND ($2::timestamptz IS NULL OR av.created_at < $2)
		   AND (NOT $3::bool OR av.site = ANY($4::text[]))
		   AND EXISTS (SELECT 1 FROM interceptor_gate_verdict g
		                WHERE g.action_id = av.action_id
		                  AND g.gate = 'execute' AND g.verdict = 'pass')
		 GROUP BY 1`, sa.triage()...)
	if err != nil {
		return out, fmt.Errorf("db: axis verdicts: %w", err)
	}
	for vr.Next() {
		var v string
		var n int
		if err := vr.Scan(&v, &n); err != nil {
			vr.Close()
			return out, fmt.Errorf("db: axis verdict scan: %w", err)
		}
		out.Verdicts[v] = n
	}
	vr.Close()
	if err := vr.Err(); err != nil {
		return out, err
	}

	// Blast-radius prediction precision/recall sums (only rows the verifier has scored, tp NOT NULL) — axis A2.
	// control_tp/control_fp are the matched-control counts so the scorer can report predicted-vs-control LIFT.
	if err := s.p.Pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE tp IS NOT NULL), COALESCE(sum(tp),0), COALESCE(sum(fp),0), COALESCE(sum(fn),0),
		       COALESCE(sum(control_tp),0), COALESCE(sum(control_fp),0)
		FROM infragraph_prediction
		WHERE committed_at >= $1 AND ($2::timestamptz IS NULL OR committed_at < $2)
		  AND (NOT $3::bool OR site = ANY($4::text[]))`, sa.triage()...).
		Scan(&out.PredScored, &out.PredTP, &out.PredFP, &out.PredFN, &out.PredControlTP, &out.PredControlFP); err != nil {
		return out, fmt.Errorf("db: axis prediction sums: %w", err)
	}
	return out, nil
}

// DimCoverage is one judge dimension's coverage over a window: how many sessions the axis actually SCORED (a
// session_judgment row exists) and their mean. An axis that is N/A for a session writes NO row — the estate
// axis logs `no-relation-derived`, falsifiable_prediction is skipped for a stand-down — so Scored is exactly
// the count of sessions the axis could speak to, which is what tells a silent axis apart from a working one.
type DimCoverage struct {
	Dimension string
	Scored    int
	Mean      float64
}

// JudgmentCoverage returns per-dimension coverage over the window plus the total DISTINCT sessions that got any
// judgment (the shared denominator). It exists because a deterministic axis that has graded 2 of 3,371 sessions
// is, on every existing surface, indistinguishable from one that is not plugged in (TG-360): the per-dimension
// means ride the scorecard, but nothing publishes judged/eligible, so silence reads as health. This is the
// rubric's own "no data is a problem, not everything passed" doctrine applied to the rubric's own axes.
//
// It counts ROWS per dimension (not score>0): a row means the axis produced a verdict for that session, which
// is the coverage question — distinct from AxisAgg.DimMeans, which excludes 0=N/A to compute a defensible mean.
// The caller (the sampler) emits the DECLARED dimension set at zero, so a fully-silent axis reports itself
// rather than vanishing from /metrics entirely. Read-only; parameterized SQL only.
func (s *AxisReadStore) JudgmentCoverage(ctx context.Context, since time.Time) ([]DimCoverage, int, error) {
	rows, err := s.p.Pool.Query(ctx, `
		SELECT j.dimension, count(*), avg(j.score)
		FROM session_judgment j JOIN session_triage t ON t.external_ref = j.external_ref
		WHERE t.created_at >= $1
		GROUP BY j.dimension ORDER BY j.dimension`, since)
	if err != nil {
		return nil, 0, fmt.Errorf("db: judgment coverage: %w", err)
	}
	defer rows.Close()
	var out []DimCoverage
	for rows.Next() {
		var c DimCoverage
		if err := rows.Scan(&c.Dimension, &c.Scored, &c.Mean); err != nil {
			return nil, 0, fmt.Errorf("db: judgment coverage scan: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	var judged int
	if err := s.p.Pool.QueryRow(ctx, `
		SELECT count(DISTINCT j.external_ref)
		FROM session_judgment j JOIN session_triage t ON t.external_ref = j.external_ref
		WHERE t.created_at >= $1`, since).Scan(&judged); err != nil {
		return nil, 0, fmt.Errorf("db: judgment coverage judged: %w", err)
	}
	return out, judged, nil
}

// distinct runs a one-column string query and collects the rows.
func (s *AxisReadStore) distinct(ctx context.Context, sql string, args ...any) ([]string, error) {
	rows, err := s.p.Pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// Falsifiability is the real-graph-vs-shuffled-control result over infragraph_cascade_stats (axis G5,
// TG-192) — TG's rarest claim, and the one that is easiest to publish dishonestly.
//
// THE VACUOUS PASS IS WHY THIS TYPE HAS FIVE FIELDS INSTEAD OF ONE.
//
// core/predict.ControlScore.Ratio() is ControlTP / max(RealTP, 1), and Falsifiable() is Ratio() <= 0.5. A
// window in which the real arm found NOTHING and the control found nothing computes 0/1 = 0, which passes.
// Both arms found nothing and the row records "the real graph beat its structural control".
//
// Measured live 2026-08-06 over 173 windows: 150 pass, and 123 of those 150 have RealTP = 0. So a naive
// count(*) filter (where falsifiable) publishes 87%, of which 82% is empty-vs-empty. Restricted to the
// windows where the model actually made a claim it is 27 of 44 — 61%.
//
// 61% is a real differentiator and worth publishing. 87% is not, and an exceed-proof is the worst artifact
// to be wrong in, because it is the one a motivated reader checks. So NoClaim is carried beside the pass
// count rather than filtered away: a model that is silent in 129 of 173 windows is itself the finding, and
// quietly dropping those rows would hide it.
type Falsifiability struct {
	Windows       int // every scored window
	NoClaim       int // RealTP == 0 — the model made no claim, so nothing can vindicate it
	Claimed       int // RealTP > 0 — the only honest denominator
	ClaimedPassed int // RealTP > 0 AND falsifiable
	// Passed is EVERY window marked falsifiable, claim or not. Carried so the report can print the number a
	// naive count would publish as a MEASUREMENT rather than an estimate of it. Deriving it as
	// ClaimedPassed+NoClaim is wrong: not every no-claim window is marked falsifiable (6 of 129 were not,
	// measured 2026-08-06), so that arithmetic overstates the overstatement.
	Passed      int
	RealTP      int     // over CLAIMED windows only
	ControlTP   int     // over CLAIMED windows only
	LosingRatio float64 // mean control_ratio over CLAIMED windows that FAILED (>=1.0 means the shuffle tied)
}

// Rate is the publishable figure: passes over windows where a claim was made. Zero claims yields 0 and a
// caller must print the denominator beside it — a bare percentage over an empty denominator is the same
// vacuity one level up.
func (f Falsifiability) Rate() float64 {
	if f.Claimed <= 0 {
		return 0
	}
	return float64(f.ClaimedPassed) / float64(f.Claimed)
}

// Falsifiability reads the windowed real-vs-control verdicts. `since` bounds the window; a zero time reads
// all of them.
func (s *AxisReadStore) Falsifiability(ctx context.Context, since time.Time) (Falsifiability, error) {
	return s.FalsifiabilityScoped(ctx, AxisScope{Since: since})
}

// FalsifiabilityScoped reads the verdicts whose window STARTS inside [scope.Since, scope.Until) (a zero bound
// is open). It is ESTATE-SCOPED by construction and ignores scope.Site: a cascade-stats window scores the
// whole graph against its shuffled control, so it has no site to attribute (TG-632).
func (s *AxisReadStore) FalsifiabilityScoped(ctx context.Context, scope AxisScope) (Falsifiability, error) {
	var out Falsifiability
	err := s.p.Pool.QueryRow(ctx, `
		SELECT
		  count(*),
		  count(*) FILTER (WHERE real_tp = 0),
		  count(*) FILTER (WHERE real_tp > 0),
		  count(*) FILTER (WHERE real_tp > 0 AND falsifiable),
		  count(*) FILTER (WHERE falsifiable),
		  COALESCE(sum(real_tp)    FILTER (WHERE real_tp > 0), 0),
		  COALESCE(sum(control_tp) FILTER (WHERE real_tp > 0), 0),
		  COALESCE(avg(control_ratio) FILTER (WHERE real_tp > 0 AND NOT falsifiable), 0)
		FROM infragraph_cascade_stats
		WHERE ($1::timestamptz IS NULL OR window_start >= $1)
		  AND ($2::timestamptz IS NULL OR window_start < $2)`,
		nullableTime(scope.Since), nullableTime(scope.Until)).
		Scan(&out.Windows, &out.NoClaim, &out.Claimed, &out.ClaimedPassed, &out.Passed,
			&out.RealTP, &out.ControlTP, &out.LosingRatio)
	if err != nil {
		return Falsifiability{}, fmt.Errorf("db: falsifiability axis: %w", err)
	}
	return out, nil
}

// LoopBypass is the anti-drift tripwire the Hands/Proof mission depends on (TG-191, epic TG-187). Every
// executed heal is meant to traverse the falsifiable core: commit a prediction BEFORE it acts and be graded
// by core/verify AFTER. An execution missing either half bought raw A5/A3 breadth by SKIPPING the loop — the
// exact erosion of the differentiated core the mission guardrail forbids. The count belongs beside the axes
// it protects: a rising A5 that is really loop-bypassing heals is not capability, it is drift wearing a
// capability number.
type LoopBypass struct {
	Executed     int // executed actions in the window (action_execution rows) — the audited population
	Bypassing    int // executed with NO committed prediction OR NO core/verify grade — the guardrail says 0
	NoPrediction int // sub-count: acted with no committed infragraph_prediction (un-predicted actuation), EXCLUDING sealed inverses (a manual rollback is structure-gated, not prediction-gated — TG-448)
	NoVerdict    int // sub-count: executed but core/verify could not grade it (verdict NULL — TG-182 fail-closed)
}

// LoopBypass counts executed heals that skipped a limb of the prediction->verify loop (TG-191). `since`
// bounds the window on executed_at.
//
// TWO DESIGN CHOICES THAT ARE EASY TO GET WRONG.
//
//  1. THE PREDICTION JOIN IS BY action_id, NOT plan_hash. INV-07 threads the content-addressed action_id
//     unchanged from the committed prediction to the execution, and action_execution carries no plan_hash
//     column — so infragraph_prediction.action_id (indexed) is the sound and only join key here. An EXISTS
//     semi-join, so the many executions of one recycled shape do not multiply against the single first-wins
//     prediction row.
//  2. THE GRADE IS THE PER-EXECUTION verdict, NOT a join to the first-wins action_verdict shape row.
//     Migration 0043 exists precisely because action_verdict is PRIMARY KEY (action_id) first-wins: a
//     re-cycled shape would otherwise inherit its FIRST execution's verdict forever, so a later ungraded
//     re-execution would read as loop-compliant off a stale row. action_execution.verdict is the fresh grade
//     for THIS execution and is NULL exactly when the post-state was unobservable (unverifiable, TG-182
//     fail-closed) — which is the honest "we acted and could not prove it worked" the guardrail must catch.
//  3. A SEALED INVERSE IS EXCUSED FROM THE PREDICTION LIMB ONLY (TG-448). A manual rollback (TG-462) seals
//     its compensating inverse with NO model prediction on purpose: the interceptor's STRUCTURE gate asserts
//     the sealed action identity and a human approval authorizes the release — the prediction gate is not on
//     that path. So an executed inverse legitimately has no infragraph_prediction row and must NOT read as an
//     un-predicted loop-bypass. `action_execution.inverts_action_id IS NOT NULL` is the only "this is a sealed
//     inverse" signal recorded on the row (the interceptor persists InvertsActionID but not the Gated flag, and
//     TG-462's RollbackWorkflow is the SOLE producer of inverse rows and always structure-gates — so on today's
//     estate that column is a sound proxy for "structure-gated inverse"; if a future inverse producer is added
//     that does NOT structure-gate, this exclusion must be re-gated on a queryable seal flag, not inverts alone).
//     The exclusion is deliberately NARROW along two axes: it excuses the PREDICTION limb only — an inverse that
//     ran but could not be verified is STILL flagged (the NoVerdict limb is untouched) — and it applies to
//     inverse rows ONLY, so a forward action with no prediction (`inverts_action_id IS NULL`) is fully flagged.
//
// Absent is not a pass: an empty window has no executions and reports Executed=0, Bypassing=0 — "nothing to
// audit", which the renderer states rather than printing a hollow 0-is-good.
func (s *AxisReadStore) LoopBypass(ctx context.Context, since time.Time) (LoopBypass, error) {
	return s.LoopBypassScoped(ctx, AxisScope{Since: since})
}

// LoopBypassScoped audits the executions inside one AxisScope: executed_at in [Since, Until) and, for a site
// scope, action_execution's own site column (TG-632).
func (s *AxisReadStore) LoopBypassScoped(ctx context.Context, scope AxisScope) (LoopBypass, error) {
	var out LoopBypass
	sa, err := s.resolveScope(ctx, scope)
	if err != nil {
		return out, err
	}
	err = s.p.Pool.QueryRow(ctx, `
		WITH ex AS (
		  SELECT ae.verdict IS NOT NULL AS graded,
		         EXISTS (SELECT 1 FROM infragraph_prediction p WHERE p.action_id = ae.action_id) AS predicted,
		         ae.inverts_action_id IS NOT NULL AS sealed_inverse
		    FROM action_execution ae
		   WHERE ae.executed_at >= $1 AND ($2::timestamptz IS NULL OR ae.executed_at < $2)
		     AND (NOT $3::bool OR ae.site = ANY($4::text[]))
		)
		SELECT count(*),
		       count(*) FILTER (WHERE (NOT predicted AND NOT sealed_inverse) OR NOT graded),
		       count(*) FILTER (WHERE NOT predicted AND NOT sealed_inverse),
		       count(*) FILTER (WHERE NOT graded)
		  FROM ex`, sa.triage()...).
		Scan(&out.Executed, &out.Bypassing, &out.NoPrediction, &out.NoVerdict)
	if err != nil {
		return LoopBypass{}, fmt.Errorf("db: loop-bypass axis: %w", err)
	}
	return out, nil
}

// WindowCensus is a fixed window's POPULATION accounting for the benchmark-window report (TG-632): where every
// incident and every injected fault was attributed, what could not be attributed, and which judge rubric
// versions and harness fingerprints the window's evidence was produced under. The G4 bar needs ONE of each;
// this is the read that can say whether there was one.
type WindowCensus struct {
	Since, Until time.Time
	// TriagesBySite is the window's distinct incidents per declared site (ingest_alert stamp, canonicalized);
	// TriagesUnattributed the rest, keyed by the raw stamp ("(no ingest_alert row)" when there is none,
	// "(empty site)" for ""). The two always sum to the window's estate-wide incident count.
	TriagesBySite       map[string]int
	TriagesUnattributed map[string]int
	// FaultsBySite / FaultsUnattributed: the window's injected faults, attributed by faultSite.
	FaultsBySite       map[string]int
	FaultsUnattributed int
	// RubricVersions counts the window's session_judgment rows per rubric_version ("" = judged before
	// versioning). "" is reported as its own value, never dropped: an unrecorded identity cannot be shown to
	// equal the recorded one.
	RubricVersions map[string]int
	// HarnessFPs counts the window's AGENT-LOOP incidents per session_triage.harness_fp; "" here means a session
	// that ran the agent loop (step_count > 0) yet recorded no fingerprint — unrecorded, so it counts against
	// uniformity. Two populations are outside the check, each counted on its own line:
	//   - HarnessDeterministic: the deterministic heal lane (decision_model_tier 'deterministic-heal'). It never
	//     composes the agent's harness at all, whatever it may be stamped with later.
	//   - HarnessNotComposed: harness_fp "" AND step_count 0 on any other session — the investigate step never
	//     returned (suppressed before the loop, or failed). temporal/runner stamps harness_fp on EVERY
	//     investigate result (activities.go recordHarnessRender), which makes "" + no steps a sound test.
	HarnessFPs           map[string]int
	HarnessDeterministic int
	HarnessNotComposed   int
	// OtherUnattributed counts, per table, the window's action_verdict / action_execution / infragraph_prediction
	// rows whose own site column is empty or undeclared — the populations the A2 ground-truth and G6 reads
	// scope by that column. They are in no site's numbers, so they are counted here rather than lost.
	OtherUnattributed map[string]int
}

// noIngestRowLabel / emptySiteLabel name the two unattributed buckets that are not a raw site value.
const (
	noIngestRowLabel = "(no ingest_alert row)"
	emptySiteLabel   = "(empty site)"
)

// WindowCensus reads the population accounting for [since, until). Both bounds are required: a benchmark
// window is fixed, so a re-run reads the same population.
func (s *AxisReadStore) WindowCensus(ctx context.Context, since, until time.Time) (WindowCensus, error) {
	out := WindowCensus{Since: since, Until: until,
		TriagesBySite: map[string]int{}, TriagesUnattributed: map[string]int{}, FaultsBySite: map[string]int{},
		RubricVersions: map[string]int{}, HarnessFPs: map[string]int{}, OtherUnattributed: map[string]int{}}
	if since.IsZero() || until.IsZero() || !until.After(since) {
		return out, fmt.Errorf("db: window census needs a fixed window with until after since (got %s .. %s)",
			since.UTC().Format(time.RFC3339), until.UTC().Format(time.RFC3339))
	}

	// Incidents by ingest_alert stamp. LEFT JOIN so an incident with NO ingest row is counted, not lost.
	// count(DISTINCT external_ref): one incident counts once, the same population AggregateScoped's
	// latest-per-ref row set reads (session_triage's key is external_ref today; this does not depend on it).
	rows, err := s.p.Pool.Query(ctx, `
		SELECT COALESCE(a.site, ''), a.external_ref IS NULL, count(DISTINCT t.external_ref)
		  FROM session_triage t LEFT JOIN ingest_alert a ON a.external_ref = t.external_ref
		 WHERE t.created_at >= $1 AND t.created_at < $2
		 GROUP BY 1, 2`, since, until)
	if err != nil {
		return out, fmt.Errorf("db: window census triages: %w", err)
	}
	for rows.Next() {
		var raw string
		var missing bool
		var n int
		if err := rows.Scan(&raw, &missing, &n); err != nil {
			rows.Close()
			return out, fmt.Errorf("db: window census triage scan: %w", err)
		}
		switch canon := ingest.CanonicalizeSite(raw); {
		case missing:
			out.TriagesUnattributed[noIngestRowLabel] += n
		case ingest.IsDeclaredSite(canon):
			out.TriagesBySite[canon] += n
		case raw == "":
			out.TriagesUnattributed[emptySiteLabel] += n
		default:
			out.TriagesUnattributed[raw] += n
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("db: window census triage rows: %w", err)
	}

	// Injected faults, attributed by the same faultSite the scoped A1 uses.
	fr, err := s.p.Pool.Query(ctx, `
		SELECT host, node FROM injected_fault WHERE injected_at >= $1 AND injected_at < $2`, since, until)
	if err != nil {
		return out, fmt.Errorf("db: window census faults: %w", err)
	}
	for fr.Next() {
		var host, node string
		if err := fr.Scan(&host, &node); err != nil {
			fr.Close()
			return out, fmt.Errorf("db: window census fault scan: %w", err)
		}
		if site := faultSite(host, node); site != "" {
			out.FaultsBySite[site]++
		} else {
			out.FaultsUnattributed++
		}
	}
	fr.Close()
	if err := fr.Err(); err != nil {
		return out, fmt.Errorf("db: window census fault rows: %w", err)
	}

	// Rubric identity: every judgment row of the window's incidents, scored or N/A — both were produced by a
	// rubric. Harness identity: every AGENT-LOOP incident of the window (see HarnessFPs for the two lanes outside).
	if err := s.countBy(ctx, out.RubricVersions, `
		SELECT j.rubric_version, count(*)
		  FROM session_judgment j JOIN session_triage t ON t.external_ref = j.external_ref
		 WHERE t.created_at >= $1 AND t.created_at < $2
		 GROUP BY 1`, since, until); err != nil {
		return out, fmt.Errorf("db: window census rubric versions: %w", err)
	}
	hr, err := s.p.Pool.Query(ctx, `
		SELECT harness_fp,
		       CASE WHEN decision_model_tier = $3 THEN 'deterministic'
		            WHEN harness_fp = '' AND step_count = 0 THEN 'not-composed'
		            ELSE 'loop' END,
		       count(DISTINCT external_ref)
		  FROM session_triage
		 WHERE created_at >= $1 AND created_at < $2
		 GROUP BY 1, 2`, since, until, deterministicHealTier)
	if err != nil {
		return out, fmt.Errorf("db: window census harness fingerprints: %w", err)
	}
	defer hr.Close()
	for hr.Next() {
		var fp, lane string
		var n int
		if err := hr.Scan(&fp, &lane, &n); err != nil {
			return out, fmt.Errorf("db: window census harness scan: %w", err)
		}
		switch lane {
		case "deterministic":
			out.HarnessDeterministic += n
		case "not-composed":
			out.HarnessNotComposed += n
		default:
			out.HarnessFPs[fp] += n
		}
	}
	if err := hr.Err(); err != nil {
		return out, fmt.Errorf("db: window census harness rows: %w", err)
	}

	// The site-column tables, each over its own time column, folded through the same authority.
	for _, q := range []struct{ table, sql string }{
		{"action_verdict", `SELECT site, count(*) FROM action_verdict WHERE created_at >= $1 AND created_at < $2 GROUP BY 1`},
		{"action_execution", `SELECT site, count(*) FROM action_execution WHERE executed_at >= $1 AND executed_at < $2 GROUP BY 1`},
		{"infragraph_prediction", `SELECT site, count(*) FROM infragraph_prediction WHERE committed_at >= $1 AND committed_at < $2 GROUP BY 1`},
	} {
		bySite := map[string]int{}
		if err := s.countBy(ctx, bySite, q.sql, since, until); err != nil {
			return out, fmt.Errorf("db: window census %s sites: %w", q.table, err)
		}
		for raw, n := range bySite {
			if !ingest.IsDeclaredSite(ingest.CanonicalizeSite(raw)) {
				out.OtherUnattributed[q.table] += n
			}
		}
	}
	return out, nil
}

// countBy runs a (text, count) query into dst.
func (s *AxisReadStore) countBy(ctx context.Context, dst map[string]int, sql string, args ...any) error {
	rows, err := s.p.Pool.Query(ctx, sql, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			return err
		}
		dst[k] += n
	}
	return rows.Err()
}
