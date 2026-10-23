package runner

import (
	"context"
	"log"
	"time"

	"github.com/territory-grounder/grounder/core/correlate"
	"github.com/territory-grounder/grounder/core/execclass"
	"github.com/territory-grounder/grounder/core/ingest"
)

// CorrelateInput is the incident, reduced to what the correlation stage reads. It is deliberately NOT the
// whole envelope: this activity answers one question ("is anything else broken right now?") and carrying
// the summary/labels blob into it would put untrusted free text on a payload that only needs identifiers
// and a timestamp.
type CorrelateInput struct {
	ExternalRef string
	// SourceID is the authenticated ingest source slug (e.g. "pve-liveness"). Carried for TG-496 fix (c):
	// the deterministic guest-down fast-path fires ONLY for the TG-native, edge-triggered pve-liveness
	// detector (source + Device-Down rule), never a slow LibreNMS push alert under the same rule name. Absent
	// on a pre-TG-496 in-flight activity task ⇒ the fast-path predicate reads "" and fails closed to the
	// normal path (the same zero-value-safe contract as ClusterMemberContext).
	SourceID  string
	Host      string
	AlertRule string
	Severity  string
	// At is the incident's front-door arrival — the anchor the window is centred on. See
	// core/correlate.Observation on why arrival and not the provider's observed_at.
	At time.Time
}

// CorrelateResult is the routing decision the workflow adopts, plus the evidence summary that made it.
// Everything here is counts and controlled vocabulary — it is recorded in Temporal history and echoed
// onto the durable decision row, and neither is a place for untrusted text.
type CorrelateResult struct {
	// ExecClass is the topology (an execclass.Class value). The workflow validates it before adopting it,
	// so a future empty/garbage value can never route an incident into an unknown class.
	ExecClass string
	// Correlated is the signal the class was chosen from — the field TG-169 is about.
	Correlated bool
	// Reason is correlate's controlled vocabulary (isolated / multi-host-window / cross-source-window /
	// window-unavailable).
	Reason string
	// Degraded is true only when the correlation window could NOT be read and the stage fell back. It is
	// carried separately from Correlated so "TG could not look" never reads as "TG looked and saw one
	// system" — on the record or in a Temporal history.
	Degraded  bool
	Hosts     int
	Sources   int
	Members   int
	WindowSec int
	// HostNames / SourceNames / MemberRefs are the NON-SECRET identifier lists behind the counts above —
	// the []string names that Assess already computes and persists to exec_class_decision.evidence_json but
	// that used to STOP at the activity boundary: CorrelateResult exposed only the ints, so the workflow
	// (where dispatch happens) never saw WHO was in the cluster (TG-385 defect (b)). They return now so the
	// collapse can attach the cluster's members as evidence to the elected subject's record. MemberRefs is
	// the MaxMembers-capped audit list (bounded so a wide cascade cannot balloon the Temporal history).
	HostNames   []string
	SourceNames []string
	MemberRefs  []string
	// ClusterID is the DURABLE cluster identity this correlated session joined (alert_cluster, migration
	// 0085) — the id every member of one storm shares. 0 for an uncorrelated incident, or when no durable
	// cluster store is wired (then no collapse happens and the session investigates as before).
	ClusterID int64
	// ElectedRef / RunnerUpRef / ElectRule are the causal election's result: the member elected to
	// INVESTIGATE the cluster, the runner-up, and which tie-break decided (core/correlate.ElectRule*).
	// Empty for an uncorrelated incident.
	ElectedRef  string
	RunnerUpRef string
	ElectRule   string
	// Elected reports whether THIS incident should open an investigation session. True for an uncorrelated
	// incident (always its own subject), for the elected causal subject of a cluster, and whenever no
	// durable cluster identity could be formed (fail-safe: without a shared id, collapsing risks attaching
	// across storms, so every member investigates as before). False ONLY for a correlated member that joined
	// a durable cluster and lost the election — that member collapses (TG-376) and opens no session.
	Elected bool
	// Recorded is whether the durable audit row was written (migration 0058). False on a fail-open write
	// error or with no sink wired; the routing decision still stands.
	Recorded bool
	// FastHeal is TG-496 fix (c): true ONLY for a CONFIRMED-STOPPED, isolated, non-critical pve-liveness
	// guest-down — the one unambiguous reversible case the deterministic auto-heal fast-path serves. When
	// true the classifier signals KnownProcedure+Reversible were set (class DETERMINISTIC) and the workflow
	// routes the incident to DeterministicGuestHealActivity — a start-guest PROPOSAL synthesized WITHOUT the
	// agent loop, restoring auto-heal under any brain. It is set by confirmedGuestDownHeal, which fails
	// CLOSED: a flap, an unknown/stale/paused observation, a running guest, a criticality-tier target, a
	// correlated cascade, or an unwired reader all leave it false and keep the incident on the normal path.
	// Zero on a pre-TG-496 recorded result (an in-flight history replays with no fast-path, byte-identically).
	FastHeal bool
}

// correlateInputFor projects the envelope onto the correlation query. PURE and deterministic, so it is
// safe in workflow code.
//
// ReceivedAt IS THE ANCHOR, with ObservedAt only as a fallback for an envelope that carries no arrival
// time. The window's CONTENTS are filtered on the front door's own received_at clock (see
// core/db.CorrelationStore), and anchoring on a different clock than the one the filter uses would centre
// the window on a provider timestamp that can sit minutes away from what TG actually saw — an off-centre
// window is the one failure here that nothing downstream can detect.
func correlateInputFor(env ingest.IncidentEnvelope) CorrelateInput {
	at := env.ReceivedAt
	if at.IsZero() {
		at = env.ObservedAt
	}
	return CorrelateInput{
		ExternalRef: env.ExternalRef,
		SourceID:    env.SourceID, // TG-496 fix (c): the fast-path is scoped to the pve-liveness source
		Host:        env.Host,
		AlertRule:   env.AlertRule,
		Severity:    env.Severity.String(),
		At:          at,
	}
}

// ClusterMemberContext is the elected-cluster evidence the workflow threads into InvestigateActivity
// (TG-465 part 2). TG-385 returned the member NAMES to the workflow and persisted them on the decision row
// and the collapsed:cluster-member records — but the elected subject's PROMPT never received them, so the
// one investigation standing in for a 157-alert storm (pve03, 2026-08-06) did not know it represented a
// cluster. This struct carries exactly what the seed needs and nothing more:
//
//   - ElectedSubject + HostNames + Members ARE threaded: the prompt's contract sentence ("you are the
//     elected causal subject of N incidents across these hosts") is built from them.
//   - MemberRefs are NOT: they are the MaxMembers-capped AUDIT identifiers, already persisted on
//     exec_class_decision.evidence_json and the collapsed rows. In the prompt they would spend block budget
//     naming alert ids the agent's read-only tools cannot look up, where the HOST names are what the estate
//     tools take.
//
// The ZERO VALUE composes today's seed byte-for-byte (no block) — the same fail-safe contract as the
// execClass parameter: an in-flight activity task scheduled by pre-TG-465p2 workflow code deserializes the
// absent argument as this zero value and behaves exactly as before.
type ClusterMemberContext struct {
	// ElectedSubject is true only when THIS incident is the elected causal subject of a CORRELATED window
	// (cor.Correlated && cor.Elected). An uncorrelated incident is trivially "elected" on its own behalf
	// (CorrelateResult.Elected defaults true) and must NOT read as a cluster subject, so the projection
	// gates on both.
	ElectedSubject bool
	// Members is the FULL correlated member count in the window (alerts, pre-cap — Verdict.MemberCount).
	Members int
	// HostNames is the DISTINCT, sorted member host list (Verdict.Hosts — uncapped at source; the seed
	// renderer applies its own display cap with a truncation notice).
	HostNames []string
}

// memberContextFor projects the correlation verdict onto the investigate payload. PURE and deterministic,
// so it is safe in workflow code (the same contract as correlateInputFor). Anything short of a correlated
// window with THIS incident as its elected subject projects the zero value — the no-block, byte-identical
// seed path.
func memberContextFor(cor CorrelateResult) ClusterMemberContext {
	if !cor.Correlated || !cor.Elected {
		return ClusterMemberContext{}
	}
	return ClusterMemberContext{
		ElectedSubject: true,
		Members:        cor.Members,
		HostNames:      cor.HostNames,
	}
}

// severityCorrelated is the PRE-TG-169 rule, kept in one named place so the thing that was wrong is
// visible rather than scattered: `Correlated` was `env.Severity == critical`.
//
// It survives for exactly one job — the fallback when the correlation stage cannot run at all (no reader
// wired, i.e. a worker with no durable pool, or a read error). That is the "safe default for existing
// deployments" rule: a deployment that has not got the evidence keeps the behaviour it had, rather than
// silently routing every incident to the cheaper class the day this ships. It is NOT a second opinion the
// real correlator can be overruled by — when the window IS readable, this function has no say.
func severityCorrelated(severity string) bool {
	return severity == ingest.SeverityCritical.String()
}

// legacyExecClassFor is the pre-TG-169 envelope-only topology decision, retained as the fallback for a
// session whose correlation stage did not run: an in-flight workflow history from before this shipped, a
// harness dispatching InvestigateActivity directly, or a worker with no correlation reader. Pure and
// deterministic (INV-08) and fail-safe.
func legacyExecClassFor(env ingest.IncidentEnvelope) execclass.Class {
	return execclass.Classify(execclass.Input{Correlated: severityCorrelated(env.Severity.String())})
}

// classFor resolves the execution class an ACTIVITY must honour: the class the workflow's correlation
// stage decided and threaded down, or — when nothing was threaded — the legacy envelope-only rule.
//
// THIS EXISTS BECAUSE TWO ACTIVITY-SIDE CALLERS USED TO RE-DERIVE THE CLASS FROM THE ENVELOPE
// (composeGuidance's skill selection and investigateTierFor's model floor). The moment the class stops
// being a pure function of the envelope, a re-derivation is a SECOND, DIFFERENT decision wearing the
// first one's name: the workflow would record DEEP_INVESTIGATION for a warning cascade while the
// investigation it launched read on the cheap tier, and nothing would disagree with anything. So the
// decided class is threaded, and this is the one place that falls back.
func classFor(env ingest.IncidentEnvelope, decided string) execclass.Class {
	if c := execclass.Class(decided); execclass.Valid(c) {
		return c
	}
	return legacyExecClassFor(env)
}

// CorrelateActivity is the incident CORRELATION stage (TG-169): it reads the alerts TG admitted around
// this incident, decides — cross-source, cross-host, time-windowed — whether the incident is one system in
// trouble or several, classifies the execution topology from that, and durably records the decision with
// the inputs that produced it.
//
// WHY IT IS AN ACTIVITY AND THE CLASSIFY CALL LIVES HERE. The correlation evidence is a database read, and
// a workflow may not perform one. The classification itself is pure, and keeping it beside the read means
// the ROUTING DECISION and the EVIDENCE FOR IT are computed and persisted in one place, in one round trip
// — the alternative (return the verdict, classify in the workflow, persist from a third activity) splits a
// single decision across three sites where two of them can disagree. Determinism is unaffected: the result
// is recorded in Temporal history, so a replay adopts the same class forever, and the classifier itself is
// still the pure core/execclass function no model token touches (INV-08).
//
// FAIL-OPEN, IN THE DIRECTION THAT PRESERVES TODAY'S BEHAVIOUR. No reader wired (a worker with no durable
// pool) or a read error ⇒ the verdict is `window-unavailable`, marked degraded, carrying the PRE-TG-169
// severity answer, so such a deployment routes exactly as it did before this shipped. A record-write
// failure never fails the session: the audit row feeds review, never authorization (INV-08).
func (a *Activities) CorrelateActivity(ctx context.Context, in CorrelateInput) (CorrelateResult, error) {
	subject := correlate.Observation{
		ExternalRef: in.ExternalRef,
		Host:        in.Host,
		AlertRule:   in.AlertRule,
		Severity:    in.Severity,
		At:          in.At,
	}

	// The two fallback paths are written out separately because they are two different facts about a
	// deployment — "this worker has no durable pool" and "the read failed" — and a correlation stage that
	// cannot tell them apart cannot be debugged from its own logs.
	var v correlate.Verdict
	var w correlate.Window
	windowReadable := false // the window was actually READ (not the nil/error degraded fallbacks) — the
	// precondition for forming a durable cluster identity + electing a causal subject below.
	if a.D.CorrelationWindow == nil {
		v = correlate.Unavailable(severityCorrelated(in.Severity))
	} else if got, err := a.D.CorrelationWindow(ctx, in.At); err != nil {
		// The read failed. Say so on the record — a degraded verdict is NOT an isolated one, and a
		// correlation stage that quietly reports "nothing else is broken" whenever its database is
		// unreachable is worse than no stage at all.
		log.Printf("correlate: window read for %s failed, falling back to the pre-TG-169 severity rule (non-blocking): %v", in.ExternalRef, err)
		v = correlate.Unavailable(severityCorrelated(in.Severity))
	} else {
		w = got
		windowReadable = true
		v = correlate.Assess(subject, w)
	}

	inputs := execclass.Input{Correlated: v.Correlated}
	// TG-496 fix (c) — the deterministic guest-down auto-heal fast-path (wiring TG-42's classifier signals
	// for the one unambiguous reversible case they left open). A pve-liveness Device-Down whose guest is
	// CONFIRMED observed-stopped is a registered, bounded, VERIFIED, reversible remediation (start-guest —
	// exactly the KnownProcedure doc at core/execclass). Setting KnownProcedure+Reversible routes it to the
	// DETERMINISTIC class → a deterministic-heal emission that does NOT depend on the agent loop grounding a
	// diagnosis (the propensity that collapsed under the 2026-08-08 Mistral swap; TG-496). LOAD-BEARING:
	// confirmedGuestDownHeal fails CLOSED — a flapping/unknown/stale/running/criticality-tier/correlated
	// incident never sets the signals, so it stays STANDARD_AGENT. The signals are recorded on the
	// exec_class_decision row below (Inputs: inputs), so the audit is honest about why the class is what it is.
	fastHeal := a.confirmedGuestDownHeal(ctx, in, v.Correlated)
	if fastHeal {
		inputs.KnownProcedure = true
		inputs.Reversible = true
	}
	class := execclass.Classify(inputs)

	// TG-380 correlate stage triple (offered / eligible / acted). offered = every CorrelateActivity. eligible
	// = the correlation window was READABLE — NOT the nil-window (:140) or read-error (:142) degraded arms, so
	// a genuine multi-system question was actually asked (v.Degraded is set on exactly those two fallbacks).
	// acted = a real correlation was found (v.Correlated). This makes a zero read cleanly: eligible=0 means
	// "no durable window anywhere, every session fell back to the pre-TG-169 severity rule", distinct from
	// eligible>0/acted=0 meaning "the stage ran and correlated nothing" (the healthy-quiet case). nil tally is
	// a no-op. NOTE: the "classify" stage is deliberately NOT recorded here — execclass.Classify is driven
	// only by v.Correlated at this call site, so its verdict is a pure function of this stage (see
	// metrics.PendingDecisionStages' slice-3 note); a separate classify triple would duplicate this one.
	a.D.Stages.Record("correlate", !v.Degraded, v.Correlated)

	// TG-385 / TG-376: give a genuinely correlated cascade a DURABLE cluster identity and elect ONE causal
	// subject to investigate it, so the storm collapses to one session instead of one per member. This runs
	// only when the window was actually READ and the verdict is correlated — a degraded or isolated verdict
	// is a singleton that always investigates, and forming a cluster off a window that could not be read
	// would invent a cascade the way the severity heuristic used to. Both seams are nil-safe.
	var clusterID int64
	var el correlate.Election
	if windowReadable && v.Correlated {
		anchorRef, anchorAt := correlate.ClusterAnchor(subject, w)
		if a.D.ClusterJoin != nil && anchorRef != "" {
			if id, err := a.D.ClusterJoin(ctx, correlate.ClusterBucket(anchorAt), anchorRef, anchorAt, v.Span); err != nil {
				// FAIL-OPEN: no durable identity ⇒ no collapse ⇒ this session investigates as before. A cascade
				// costing N sessions is exactly the pre-TG-385 behaviour — strictly better than collapsing onto a
				// cluster we cannot reliably share, which could attach a member to the wrong storm.
				log.Printf("correlate: cluster join for %s failed (non-blocking, no collapse): %v", in.ExternalRef, err)
			} else {
				clusterID = id
			}
		}
		// The election reads the FULL member set (never the MaxMembers-truncated audit list) and is nil-topo
		// safe. Computed even when the join failed, so the decision row still records the causal ranking.
		el = correlate.Elect(subject, w, a.D.ClusterTopology)
	}

	res := CorrelateResult{
		ExecClass: string(class), Correlated: v.Correlated, Reason: v.Reason, Degraded: v.Degraded,
		Hosts: len(v.Hosts), Sources: len(v.Sources), Members: v.MemberCount,
		WindowSec:   int(v.Span / time.Second),
		HostNames:   v.Hosts, // TG-385 (b): the NAMES the workflow needs, not just the counts
		SourceNames: v.Sources,
		MemberRefs:  v.Members, // the MaxMembers-capped audit list (bounded)
		ClusterID:   clusterID,
		ElectedRef:  el.Elected,
		RunnerUpRef: el.RunnerUp,
		ElectRule:   el.Rule,
		FastHeal:    fastHeal, // TG-496 fix (c): the confirmed-guest-down deterministic-heal routing signal
	}
	// Whether THIS incident opens an investigation session (TG-376). Default: yes. It collapses to evidence
	// ONLY when it is a correlated member that joined a DURABLE cluster (clusterID>0), LOST the election, AND
	// the election was decided by CAUSAL evidence (in-degree / runs_on parent-fanout).
	//
	// THE CAUSAL-RULE GUARD IS A SILENCING SAFEGUARD. Collapsing DROPS the other members' investigations, so a
	// wrong collapse SILENCES a real incident. A cluster elected by earliest-ref alone (topology unseeded — no
	// in-degree winner, no parent-fanout winner) is a TIME COINCIDENCE: three hosts that merely alerted
	// together, the exact shape of a TG-169 false positive. Silencing two genuine incidents demands causal
	// evidence of who the parent is; time-coincidence is not enough. So a non-causal election never collapses —
	// every member investigates (the safe status quo) — and only a causally-anchored cascade (guests that
	// run_on a hypervisor) collapses to one session. The elect_rule + runner_up are recorded on the decision
	// row regardless (see correlate.IsCausalRule), so the audit is unchanged.
	res.Elected = true
	if v.Correlated && clusterID > 0 && el.Elected != "" && el.Elected != in.ExternalRef && correlate.IsCausalRule(el.Rule) {
		res.Elected = false
	}

	// The audit trail the topology decision never had. Best-effort: nil sink (no DB) ⇒ no row, and a write
	// error is logged and swallowed — a routing decision that was made must not be undone by a failure to
	// write it down.
	if a.D.ExecClassRecord != nil {
		d := correlate.Decision{
			ExternalRef: in.ExternalRef, ExecClass: class, Inputs: inputs, Verdict: v,
			ClusterID: clusterID, Election: el, // TG-385/TG-376: the cluster + who was elected + why
			DecidedAt: time.Now().UTC(), // when the STAGE ran; in.At is when the alert arrived (both are on the row's joins)
		}
		if err := a.D.ExecClassRecord(ctx, d); err != nil {
			log.Printf("correlate: exec_class_decision write for %s failed (non-blocking): %v", in.ExternalRef, err)
		} else {
			res.Recorded = true
		}
	}
	return res, nil
}
