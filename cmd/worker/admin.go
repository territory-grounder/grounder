package main

// The worker's minimal internal admin surface (Phase-2 readiness review §4.B.2/§2): the worker OWNS the
// process-global actuation chokepoint (the mode-driven successor to the retired mutation gate), so the runtime
// kill-switch must reach THIS process. Today the worker has no HTTP server, so the only ON→OFF path was a
// restart. This adds a tiny listener serving exactly two routes: a HALT-ONLY kill-switch (POST /halt →
// chokepoint.ForceShadow, dropping the mode to read-only Shadow, REQ-1520) and a read-only /metrics exposition.
// It is SAFETY-ADDITIVE by construction: there is NO enable route here — /halt can only ever make the posture
// MORE restrictive, and /metrics is read-only. Bind it to the internal network (a distinct port, default :8444).

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/territory-grounder/grounder/adapters/observability"
	"github.com/territory-grounder/grounder/core/actuate"
	"github.com/territory-grounder/grounder/core/attribution"
	"github.com/territory-grounder/grounder/core/attribution/readertally"
	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/breaker"
	"github.com/territory-grounder/grounder/core/cost"
	"github.com/territory-grounder/grounder/core/egress"
	"github.com/territory-grounder/grounder/core/metrics"
	"github.com/territory-grounder/grounder/core/observe"
	"github.com/territory-grounder/grounder/core/policy"
	"github.com/territory-grounder/grounder/core/preflight"
	"github.com/territory-grounder/grounder/core/safety"
)

// workerAdmin is the worker's read-only-plus-halt admin surface. It holds only a DIGEST of the halt bearer
// token (never the token), so the process retains no reusable secret material.
type workerAdmin struct {
	gate    *safety.Chokepoint
	breaker *safety.MutationBreaker
	cost    *cost.Accountant // the spend guard; nil ⇒ cost tracking disabled (no accumulator to read)
	// costCfg is the spend guard's CONFIGURATION, held separately from the accountant because the interesting
	// reading is the one where there is no accountant. See the gauges in metricsSamples.
	costCfg    cost.Config
	ledger     *audit.Ledger
	haltDigest [32]byte // sha256 of the admin bearer token
	haltAuthed bool     // false ⇒ /halt is NOT registered (fail closed) — only /metrics is served
	halts      atomic.Int64
	// The boot credential-preflight (TG-113) result, surfaced on /metrics as tg_ssh_credential_ready so the
	// console shows a DEGRADED signal when a configured SSH key is missing/unreadable/unparseable by the
	// worker's real runtime user — instead of a false healthy. sshCredConfigured==0 ⇒ no SSH in use, no gauge.
	sshCredConfigured int
	sshCredReady      bool
	// Per-domain actor-evidence accounting (core/attribution/readertally). nil ⇒ no readers were armed, so
	// nothing is emitted — an unarmed estate has no reader to report on, and a silent series is honest
	// there. When it IS set, a domain read at least once emits rows=0 EXPLICITLY: an absent series and a
	// zero series read identically in a graph and differently in an alert, and the zero is the reading.
	actorTally *readertally.Tally
	// THE ACTUATION-FREQUENCY GOVERNOR (TG-286/TG-166). nil ⇒ nothing emitted.
	//
	// The limiter was wired on the real path — interceptor.go calls Admit before every effect — and
	// published nothing. For a rate governor that is a specific kind of blind: it is SUPPOSED to be quiet,
	// so "has never needed to refuse" and "is admitting everything because its window is misconfigured"
	// produce identical evidence, which is none. A leaked lease (Admit without Release) wedges the lane and
	// is equally invisible.
	actuationLimiter func() actuate.LimiterStats
	policyRateGov    func() (policy.RateGovernorStats, bool)
	// TG'S OWN INPUT LIVENESS (TG-336). nil ⇒ nothing emitted.
	//
	// The alert intake collapsed 99% on 2026-07-31 and ran at ~1% for five days with triage at zero, and
	// no signal existed to notice. TG-250 instrumented the internal seams and left the front door
	// uninstrumented, so the platform could go deaf while every dashboard stayed green.
	ingestFreshness func() []metrics.Sample
	evidenceShape   func() []metrics.Sample
	ledgerShape     func() []metrics.Sample
	loopClosure     func() []metrics.Sample
	categoryCover   func() []metrics.Sample
	predictionWidth func() []metrics.Sample
	// THE HUMAN GATE'S DEPTH (TG-173). nil ⇒ nothing emitted.
	//
	// The rate governor answers load by clamping auto -> APPROVE, routing MORE decisions to the operator
	// exactly when the operator is least able to keep up, and nothing measured the result: 47 published
	// metric families and not one of them the depth of the approval queue. CountOpen was rendered on
	// /v1/stats, which is a number for someone already looking at the console.
	pollQueue     func() []metrics.Sample
	syntheticLeak func() []metrics.Sample
	// WHAT THE UPSTREAM HAD (TG-344). nil ⇒ nothing emitted.
	//
	// Every other ingest gauge counts what ARRIVED. Without this one, "upstream 0, ingested 0" (a quiet
	// estate) and "upstream 50, ingested 0" (a broken connector) are the same reading.
	upstreamProbe      func() []metrics.Sample
	knownHostsCoverage func() []metrics.Sample
	// THE MUTATION GATE'S INPUT (TG-343). nil ⇒ nothing emitted.
	//
	// The interceptor's host-match and blast-radius checks are evaluated against the estate graph, and
	// "a gate reasoning over an empty graph is a gate that cannot refuse". Nothing published that graph's
	// size, so an empty graph and a healthy one were indistinguishable from monitoring — and a credential
	// that resolves but returns nothing produces no error to notice.
	estateSize            func() []metrics.Sample
	pveLivenessYield      func() []metrics.Sample
	authlogYield          func() []metrics.Sample
	suppressionDecisions  func() []metrics.Sample
	stageDecisions        func() []metrics.Sample // TG-380 decision-stage triple; nil ⇒ nothing emitted
	selfDepConcentration  func() []metrics.Sample // TG-394 self-dependency placement concentration; nil ⇒ nothing
	posturePolicyWarnings func() []metrics.Sample // TG-506 operator-posture permissive-condition warnings; nil ⇒ nothing
	selfDepReachable      func() []metrics.Sample // TG-394 slice 3 per-capability reachability + degraded rollup; nil ⇒ nothing
	estateDocCoverage     func() []metrics.Sample // TG-86 estate-doc grounding coverage; nil ⇒ nothing
	observationCensus     func() []metrics.Sample // TG-180 estate-observation census; nil ⇒ nothing
	observationProbe      func() []metrics.Sample // TG-180 part 2 probe coverage-of-the-unmeasured; nil ⇒ nothing
	// plane is which credential plane this process is (TG-112, TG-153). Empty is legitimate for a worker
	// built before this was known, and renders as "unknown" rather than being guessed.
	//
	// It exists because BOTH worker processes published component="worker" and nothing else distinguished
	// them: measured 2026-08-06, mutation_enabled{component="worker"} returned two series — one for the
	// triage plane and one for the plane that can actually mutate the estate — and only job/instance told
	// them apart. A label whose whole job is naming the component named the wrong thing on the one process
	// where it matters most.
	plane string
	// THE OWNER-SET AUTONOMY MODE, as a gauge. nil ⇒ nothing emitted.
	//
	// CLAUDE.md calls the mode chokepoint the control every actuation traverses, and until now it
	// published NOTHING: you could not tell from monitoring which of the four modes the estate was in.
	// mutation_enabled is downstream of it (MayActuate = mode-permits && preflight-green), so the one
	// series that existed conflated "the owner chose an actuating mode" with "the gate is open".
	//
	// That gap made the then-alert UnexpectedMutationEnabled unfalsifiable: its rule was a bare posture
	// binary == 1 with the comment "under Phase 0/1 this must NEVER fire" — an assumption, not a reading.
	// The owner set Semi-auto on 2026-07-30, so it had been firing critical every 5 minutes for six days
	// over correct behaviour, with no way for the rule to know. Its successor
	// (ActuationPermittedWhileModeForbidsIt) COMPARES tg_may_actuate against this gauge instead (TG-112).
	policyMode func() string
	// THE WIRING REGISTERS' GAUGES (TG-250). nil ⇒ nothing emitted.
	//
	// These used to reach Prometheus ONLY through the observability EXPORTER loop in main.go, which is
	// gated on TG_OBSERVABILITY_EXPORT_INTERVAL (off by default) AND on at least one enabled exporter
	// module resolving. Measured on dc1tg01 2026-08-05: TG_OBSERVABILITY_EXPORT_INTERVAL was empty, so
	// not a single tg_wiring_seam_* series existed anywhere — the detector built to find seams that are
	// bound, running and producing nothing was itself bound, running and producing nothing. Its only
	// working output was a log line, and it was ALREADY REPORTING A STARVED SEAM nobody had seen.
	//
	// A safety detector must not be behind an optional exporter. /metrics is unconditional and already
	// scraped on both workers, so that is where these belong. The exporter path stays as-is for estates
	// that ship samples elsewhere; this is an additional, always-on surface, not a replacement.
	wiringSamples func() []observability.Sample
	// The attribution carve-outs, for the expiry gauge. A carve-out suspends the security path for its
	// actors on its hosts, and its bound is mandatory — so the estate needs to be able to ALERT on the
	// approach of a lapse, not just read a boot log nobody re-reads. Empty ⇒ no carve-outs declared ⇒ no
	// series, which is honest: there is no suspension to report the end of.
	carveOuts []attribution.CarveOut
	// The periodically-sampled benchmark axes (A1 recall + per-source detection latency). nil ⇒ unarmed, so
	// nothing is emitted: an install with no injected faults has nothing to measure, and a zero recall would
	// read as "TG detects nothing" — the most alarming false statement available about a healthy estate.
	axes *axisSampler
	// The read-lane recon meter (TG-165). Two jobs here: /halt now stops RECON as well as mutation — before
	// this, a halted worker went on enumerating the estate at full rate, because ForceShadow only ever
	// touched the mutation chokepoint — and /metrics carries the read-lane posture so a recon burst is
	// alertable rather than merely logged. nil ⇒ no governor wired ⇒ no series and nothing extra to halt.
	recon *safety.ReconGovernor
	// The shared breaker store, for the NAMED-breaker exposition (TG-221). CONSTITUTION.md:130 promises
	// breakers that are "named, observable, with persisted state": the mutation breaker was exposed from its
	// own armed instance above, but every OTHER named breaker in the shared row set — above all the
	// per-model-tier production gateway breakers — had no series at all, so a tripped model plane was
	// invisible. Listing the store is the predecessor's exporter shape (`circuit_breaker.py --export` walks
	// the whole table). nil ⇒ no store wired ⇒ no extra series.
	breakerStore breaker.Store
	// The OUTBOUND destination/volume meter (TG-160). Before it, the platform could not answer "who did
	// this process talk to and how many bytes left" for ANY destination — there was no egress control and
	// no egress observation of any kind. nil ⇒ no meter wired ⇒ no series, which is honest: a zero would
	// claim "no outbound happened", which is never true of a running worker.
	egress *egress.Meter
}

// withRecon binds the read-lane recon meter so POST /halt stops RECON as well as mutation, and so the
// read-lane counters reach /metrics. Chainable. See the recon field for why the halt needed a second half.
func (a *workerAdmin) withRecon(g *safety.ReconGovernor) *workerAdmin { a.recon = g; return a }

// withActuationLimiter publishes the governor's admitted/refused tallies, its live in-flight count and the
// budget those were measured against. Chainable.
func (a *workerAdmin) withActuationLimiter(f func() actuate.LimiterStats) *workerAdmin {
	a.actuationLimiter = f
	return a
}

// withPolicyRateGovernor publishes the POLICY-layer rate governor's counters (TG-339). Distinct from the
// actuation limiter above: that one refuses effects at the chokepoint, this one tightens auto→approve at
// policy-decide time, five gates earlier. Both were wired-and-silent; only the actuation one had been fixed.
func (a *workerAdmin) withPolicyRateGovernor(f func() (policy.RateGovernorStats, bool)) *workerAdmin {
	a.policyRateGov = f
	return a
}

// withEstateSize publishes the size of the estate graph the mutation gate reasons over. nil is legitimate
// (an in-memory worker with no graph holder) and emits nothing.
func (a *workerAdmin) withEstateSize(f func() []metrics.Sample) *workerAdmin {
	a.estateSize = f
	return a
}

// withPollQueue publishes the depth, near-duplicate collapse and oldest wait of the human approval queue.
// nil is legitimate (no database ⇒ no projection to read) and emits nothing.
// withPlane records which credential plane this worker is, so its posture series say which process they
// describe. See workerAdmin.plane.
func (a *workerAdmin) withPlane(p string) *workerAdmin {
	a.plane = p
	return a
}

// withKnownHostsCoverage publishes how many alerted hosts TG can actually diagnose (TG-271). The
// host-diagnostic lane failed on 100% of calls for weeks because the known_hosts file covered 16 of 38
// alerted hosts, and nothing said so — every read returned a valid-looking "(host was unreachable)"
// sentinel.
func (a *workerAdmin) withKnownHostsCoverage(f func() []metrics.Sample) *workerAdmin {
	a.knownHostsCoverage = f
	return a
}

// withUpstreamProbe publishes what each ingest source's upstream currently HAS, beside what arrived.
func (a *workerAdmin) withUpstreamProbe(f func() []metrics.Sample) *workerAdmin {
	a.upstreamProbe = f
	return a
}

// withSuppressionDecisions publishes the tier-1 gate's decision counts (TG-380). nil is legitimate (the
// gate is not armed yet) and emits nothing.
func (a *workerAdmin) withSuppressionDecisions(f func() []metrics.Sample) *workerAdmin {
	a.suppressionDecisions = f
	return a
}

// withStageDecisions publishes the TG-380 decision-stage triple (tg_stage_{offered,eligible,acted}_total
// per stage). nil / an empty tally emits nothing — an idle or unwired instrument is silent, not a row of
// asserted zeros.
func (a *workerAdmin) withStageDecisions(f func() []metrics.Sample) *workerAdmin {
	a.stageDecisions = f
	return a
}

// withPolicyPostureWarnings publishes the operator-posture warnings (TG-506, tg_policy_posture_warning{code}).
// nil emits nothing; the provider itself always emits the closed code set (legible zeros) once wired.
func (a *workerAdmin) withPolicyPostureWarnings(f func() []metrics.Sample) *workerAdmin {
	a.posturePolicyWarnings = f
	return a
}

// withPVELivenessYield publishes TG's fastest detector's own yield (TG-350 follow-through). nil is
// legitimate — the detector is config-gated — and emits nothing.
func (a *workerAdmin) withPVELivenessYield(f func() []metrics.Sample) *workerAdmin {
	a.pveLivenessYield = f
	return a
}

// withAuthlogYield publishes the authlog collector's offered-vs-produced register (TG-315). Chained
// unconditionally: the collector ships DARK, and a dark lane that publishes nothing is indistinguishable
// from a dead one — which is the defect this source exists to stop being an example of.
func (a *workerAdmin) withAuthlogYield(f func() []metrics.Sample) *workerAdmin {
	a.authlogYield = f
	return a
}

// withSelfDepConcentration publishes how many of TG's OWN dependency hosts share a single hypervisor (TG-394)
// — the standing single-point-of-failure risk that was knowable at boot but reported NOWHERE when 7 of 26 of
// them sat on the node TG was diagnosing and retrieval went silently lexical-only for 11h12m. nil is
// legitimate (no estate holder) and emits nothing.
func (a *workerAdmin) withSelfDepConcentration(f func() []metrics.Sample) *workerAdmin {
	a.selfDepConcentration = f
	return a
}

// withSelfDepReachable publishes, per TG dependency capability, whether every backing host is still reachable
// in the estate graph and the tg_capability_degraded rollup (TG-394 slice 3) — the LIVE degradation signal
// that was missing when TG's embedding backend went unreachable and retrieval ran lexical-only for 11h12m
// with nothing reporting a reduced capability. nil is legitimate (no estate holder / no capabilities) and
// emits nothing.
func (a *workerAdmin) withSelfDepReachable(f func() []metrics.Sample) *workerAdmin {
	a.selfDepReachable = f
	return a
}

// withEstateDocCoverage publishes how much of TG's OWN estate documentation has been ingested into the
// grounding corpus (TG-86 slice 1b): files, scrubbed chunks, redactions. nil is legitimate (no docs root
// configured) and emits nothing, so an ungrounded deployment reads as absent rather than a silent zero.
func (a *workerAdmin) withEstateDocCoverage(f func() []metrics.Sample) *workerAdmin {
	a.estateDocCoverage = f
	return a
}

// withObservationCensus publishes how much of the estate TG can actually SEE (TG-180): live hosts split into
// observed / healthy_quiet / unobservable, so structural blindness stops reading as health. nil is legitimate
// (no estate holder or DB) and emits nothing.
func (a *workerAdmin) withObservationCensus(f func() []metrics.Sample) *workerAdmin {
	a.observationCensus = f
	return a
}

// withObservationProbe publishes the coverage-of-the-unmeasured dimension (TG-180 part 2): of the census-
// unobservable entities, how many the fault-injection probe has confirmed a verdict on, plus the probe's
// arming posture. Default-OFF — the numerator is 0 until an owner arms the probe. nil emits nothing.
func (a *workerAdmin) withObservationProbe(f func() []metrics.Sample) *workerAdmin {
	a.observationProbe = f
	return a
}

func (a *workerAdmin) withPollQueue(f func() []metrics.Sample) *workerAdmin {
	a.pollQueue = f
	return a
}

// withSyntheticLeak publishes the live-DB-leak tripwire (TG-190a, CONSTITUTION 4.9). It is ALWAYS chained,
// even with no store: the register's job is to make a zero readable, and a register that goes silent when
// unwired publishes the same nothing as a clean database. Chainable.
func (a *workerAdmin) withSyntheticLeak(f func() []metrics.Sample) *workerAdmin {
	a.syntheticLeak = f
	return a
}

// withIngestFreshness publishes per-source intake liveness — how long since each alert source last
// delivered, beside how much it delivered in the baseline window. Chainable.
func (a *workerAdmin) withIngestFreshness(f func() []metrics.Sample) *workerAdmin {
	a.ingestFreshness = f
	return a
}

// withEvidenceShape publishes the agent_step_evidence corpus size beside the count of rows matching a
// credential shape — the premise TG-302's decision not to seal that table at rest rests on (TG-345).
// Chainable.
func (a *workerAdmin) withEvidenceShape(f func() []metrics.Sample) *workerAdmin {
	a.evidenceShape = f
	return a
}

// withLedgerShape publishes the governance ledger's credential-shape hygiene gauges (TG-57 item 1).
// Separate from withEvidenceShape because the operator action differs: a hit on the evidence corpus
// re-opens TG-302's sealing decision, a hit here means the ledger write path needs a screen it never had.
func (a *workerAdmin) withLedgerShape(f func() []metrics.Sample) *workerAdmin {
	a.ledgerShape = f
	return a
}

// withLoopClosure publishes whether each built loop has ever completed (TG-348).
func (a *workerAdmin) withLoopClosure(f func() []metrics.Sample) *workerAdmin {
	a.loopClosure = f
	return a
}

// withCategoryCoverage publishes whether the high-risk poll-forcing driver is reachable at all (TG-405).
func (a *workerAdmin) withCategoryCoverage(f func() []metrics.Sample) *workerAdmin {
	a.categoryCover = f
	return a
}

// withPredictionWidth publishes the blast-radius predictor's width distribution (TG-352).
func (a *workerAdmin) withPredictionWidth(f func() []metrics.Sample) *workerAdmin {
	a.predictionWidth = f
	return a
}

// withPolicyMode publishes the owner-set autonomy mode as tg_policy_mode{mode="..."}. Chainable.
func (a *workerAdmin) withPolicyMode(f func() string) *workerAdmin { a.policyMode = f; return a }

// withWiringRegisters puts the TG-250 dark-seam and seam-YIELD gauges on /metrics. Chainable; a nil
// source emits nothing.
//
// Before this, those gauges reached Prometheus only through the observability EXPORTER loop, gated on
// TG_OBSERVABILITY_EXPORT_INTERVAL (off by default). On dc1tg01 that variable was empty, so no
// tg_wiring_seam_* series existed at all: the detector for seams that are bound, running and producing
// nothing was in exactly that state itself, and its one working output was a log line nobody reads. It
// was already reporting vote.inbound STARVED — 10 events offered, 0 votes delivered — and nothing could
// alert on it.
func (a *workerAdmin) withWiringRegisters(f func() []observability.Sample) *workerAdmin {
	a.wiringSamples = f
	return a
}

// withBreakerStore exposes EVERY named breaker in the shared store on /metrics, matching the predecessor's
// whole-table exporter. Chainable.
func (a *workerAdmin) withBreakerStore(s breaker.Store) *workerAdmin { a.breakerStore = s; return a }

// withEgressMeter binds the outbound destination/volume meter so an off-allowlist connection is ALERTABLE
// and not merely logged (TG-160). Chainable. The counters it exposes are the only place the platform can
// answer "did bytes leave for somewhere we never declared", which is the covert-channel question.
func (a *workerAdmin) withEgressMeter(m *egress.Meter) *workerAdmin { a.egress = m; return a }

// withActorTally records the actor-evidence reader tally so /metrics can answer "which reader is actually
// contributing" — a per-reader fact this system asserted for weeks and could not produce. Chainable.
func (a *workerAdmin) withActorTally(t *readertally.Tally) *workerAdmin { a.actorTally = t; return a }

// withCarveOuts records the attribution carve-outs so /metrics can expose how long each one has left.
// Chainable. See the carveOuts field for why the expiry needs to be alertable and not just logged.
func (a *workerAdmin) withCarveOuts(cos []attribution.CarveOut) *workerAdmin {
	a.carveOuts = cos
	return a
}

// withAxisSampler records the periodic axis sample so /metrics carries the benchmark axes — above all the
// per-source DETECTION LATENCY, which had no operational surface at all and is the only axis a faster
// detector moves. Chainable.
func (a *workerAdmin) withAxisSampler(s *axisSampler) *workerAdmin { a.axes = s; return a }

// withSSHCredential records the boot credential-preflight result so /metrics can surface a DEGRADED-credential
// health signal (TG-113). Chainable. A zero configured count emits no gauge (native SSH is not in use).
// withCostConfig hands the admin surface the spend guard's CONFIGURATION. It is separate from the
// accountant on purpose: the reading that matters most — "no budget is armed" — is exactly the one where
// there is no accountant to ask. Chainable.
func (a *workerAdmin) withCostConfig(c cost.Config) *workerAdmin {
	a.costCfg = c
	return a
}

func (a *workerAdmin) withSSHCredential(rep preflight.Report) *workerAdmin {
	a.sshCredConfigured = rep.Configured()
	a.sshCredReady = rep.Ready()
	return a
}

// boolGauge renders a boolean posture as the 1/0 a Prometheus gauge carries. Named rather than inlined
// because these gauges exist to be READ AT ZERO, and a bare `if x { v = 1 }` at each call site is where a
// posture silently stops being published.
func boolGauge(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// newWorkerAdmin builds the admin surface. A non-empty bearerToken (resolved from TG_ADMIN_TOKEN_REF) arms
// the /halt route behind a constant-time bearer check; an empty token leaves /halt UNREGISTERED (fail
// closed) so only the read-only /metrics exists. A nil costAcct simply omits the cost gauges.
func newWorkerAdmin(gate *safety.Chokepoint, mb *safety.MutationBreaker, costAcct *cost.Accountant, ledger *audit.Ledger, bearerToken string) *workerAdmin {
	a := &workerAdmin{gate: gate, breaker: mb, cost: costAcct, ledger: ledger}
	if bearerToken != "" {
		a.haltDigest = sha256.Sum256([]byte(bearerToken))
		a.haltAuthed = true
	}
	return a
}

// mux builds the admin router: always /metrics (read-only), and /halt ONLY when a bearer token is
// configured. There is deliberately no /enable route — this surface can never turn mutation on.
func (a *workerAdmin) mux() *http.ServeMux {
	m := http.NewServeMux()
	m.Handle("/metrics", metrics.Handler(a.samples))
	if a.haltAuthed {
		m.HandleFunc("/halt", a.haltHandler)
	}
	return m
}

// authorized verifies the Authorization: Bearer <token> against the configured digest in constant time. A
// missing/malformed header, or an unconfigured halt token, is unauthorized (fail closed).
func (a *workerAdmin) authorized(r *http.Request) bool {
	if !a.haltAuthed {
		return false
	}
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" {
		return false
	}
	got := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(got[:], a.haltDigest[:]) == 1
}

// haltHandler serves POST /halt: an authenticated operator disables the process-global mutation gate. It
// is idempotent and always safe (Disable can only turn mutation more off), records the halt to the
// tamper-evident governance ledger bound to a synthetic action_id, and returns 200. It NEVER enables.
func (a *workerAdmin) haltHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !a.authorized(r) {
		http.Error(w, "unauthenticated", http.StatusUnauthorized) // one indistinguishable 401
		return
	}
	// The kill: force the mode to Shadow (safe, idempotent) BEFORE recording, so the halt takes effect even if
	// the ledger write fails. This is the absorbed gate.Disable() — it drops the mode chokepoint to read-only
	// (REQ-1520). A synthetic action_id binds the halt in the audit chain (the ledger rejects an empty id).
	a.gate.ForceShadow("worker kill-switch: operator POST /halt")
	// AND STOP THE READ LANE (TG-165). ForceShadow drops the MUTATION posture; until this line, recon ran
	// straight through a halt — so the operator's kill switch stopped the half that Shadow had already
	// stopped and left estate enumeration, the pre-actuation half of the attack chain, running at full rate.
	// Halt holds the same contract as ForceShadow: safe, idempotent, never refused, never re-enabling.
	a.recon.Halt("worker kill-switch: operator POST /halt")
	n := a.halts.Add(1)
	actionID := fmt.Sprintf("kill-switch-halt-%d", time.Now().UTC().UnixNano())
	if a.ledger != nil {
		if _, err := a.ledger.Append(audit.GovDecision{
			Decision: "safety:halt",
			Reason:   "worker kill-switch: operator POST /halt forced mode to Shadow (chokepoint.ForceShadow)",
			ActionID: actionID,
			Withheld: true, // autonomy withheld — mutation turned off
		}); err != nil {
			log.Printf("worker kill-switch: halt applied but ledger append failed: %v", err)
		}
	}
	log.Printf("worker kill-switch: HALT — may_actuate=%v (action_id=%s)", a.gate.MayActuate(), actionID)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"halted":      true,
		"may_actuate": a.gate.MayActuate(),
		// What the operator actually needs to know back: whether the READ lane stopped too. false here means
		// no governor is wired in this process, i.e. the halt covered mutation only — an honest answer that
		// an operator can act on, rather than a "halted: true" that overstates what stopped (TG-165).
		"recon_halted": a.recon.Halted(),
		"action_id":    actionID,
		"halts":        n,
	})
}

// postureLabels is the label set every posture gauge carries.
//
// component stays "worker" so existing rules and dashboards that join on it keep working; the PLANE is
// added BESIDE it rather than replacing it, because a rename silently orphans every rule already matching
// on component while looking like a tidy-up.
func (a *workerAdmin) postureLabels() map[string]string {
	plane := a.plane
	if plane == "" {
		plane = "unknown" // never guessed: an unlabelled process is reported as unlabelled
	}
	return map[string]string{"component": "worker", "plane": plane}
}

// samples collects the worker's read-only metrics: the gate posture, the mutation breaker's three-state
// gauge + deviation count, and the halt total. All read-only; no secret is ever emitted.
func (a *workerAdmin) samples() []metrics.Sample {
	ctx := context.Background()
	enabled := 0.0
	if a.gate.MayActuate() {
		enabled = 1
	}
	out := []metrics.Sample{
		// THE PRECISE SIGNAL (TG-112). may_actuate is what the code actually computes: mode in
		// {Semi-auto, Full-auto} AND boot preflight green AND the mutation breaker not tripped. The 4-mode
		// chokepoint is the single source of truth and this is its derived "can it act right now".
		{Name: "tg_may_actuate", Kind: metrics.Gauge, Help: "1 when this process may actuate RIGHT NOW — " +
			"mode in {Semi-auto, Full-auto} AND boot preflight green AND the mutation breaker not tripped. " +
			"Read beside tg_policy_mode, which is the owner-set mode this is derived FROM. Labelled by " +
			"plane: only the actuation plane can mutate the estate, so a 1 on the triage plane is a " +
			"finding about the credential split, not about the mode.",
			Value: enabled, Labels: a.postureLabels()},
		// The deprecated `mutation_enabled` alias that rode beside tg_may_actuate is RETIRED (TG-112):
		// alert.rules.yml, safety.json, the console and shadowbench all join on tg_may_actuate /
		// tg_policy_mode now, so the alias had no consumer left — and a second name for the same read
		// only invites the two to drift.
		{Name: "tg_worker_halt_total", Kind: metrics.Counter, Help: "count of kill-switch halts applied via POST /halt", Value: float64(a.halts.Load())},
	}
	if a.breaker != nil {
		mstate := a.breaker.StateValue(ctx)
		out = append(out,
			metrics.Sample{Name: "circuit_breaker_state", Kind: metrics.Gauge, Help: "mutation breaker: 0 closed / 1 half-open / 2 open", Value: mstate, Labels: map[string]string{"name": "mutation"}},
			// TG-452: the tg_-prefixed FORWARD NAME for the same 3-state gauge, dual-emitted so dashboards/alerts
			// can migrate off the unprefixed legacy series without a breaking rename. Identical value + labels;
			// the legacy circuit_breaker_state stays until every consumer has cut over (alert rules key on it).
			metrics.Sample{Name: "tg_breaker_state", Kind: metrics.Gauge, Help: "mutation breaker: 0 closed / 1 half-open / 2 open (tg_-prefixed forward name for circuit_breaker_state, TG-452)", Value: mstate, Labels: map[string]string{"name": "mutation"}},
			metrics.Sample{Name: "deviation_count", Kind: metrics.Counter, Help: "trip-worthy post-execution deviations/chain-gaps observed by the mutation breaker", Value: float64(a.breaker.Deviations())},
		)
	}
	// THE READ LANE (TG-165). Only dollar spend was ever metered on the read side, and dollars are not
	// scope: a thousand cheap enumeration probes cost less than one reasoning cycle. These are the series
	// that make recon volume alertable — above all tg_recon_reads_total, which is also the answer to "is
	// this bound wired at all?": a worker running investigations with a flat zero there is a governor that
	// is counting nothing. Emitted only when a governor is wired; an absent series says "not metered",
	// which is honest, where a zero would say "no reads happened".
	if a.recon != nil {
		rs := a.recon.Snapshot()
		halted := 0.0
		if rs.Halted {
			halted = 1
		}
		out = append(out,
			metrics.Sample{Name: "tg_recon_reads_total", Kind: metrics.Counter, Help: "estate READS dispatched by the agent loop and metered by the recon budget (TG-165). A flat zero while sessions run means the bound is not counting.", Value: float64(rs.Reads)},
			metrics.Sample{Name: "tg_recon_refused_total", Kind: metrics.Counter, Help: "estate reads REFUSED by a recon bound (per-session, per-hour, burst, or a halt). Non-zero means investigations are being truncated — check the bound before assuming the estate went quiet.", Value: float64(rs.Refusals)},
			metrics.Sample{Name: "tg_recon_burst_total", Kind: metrics.Counter, Help: "recon BURST episodes: read rate over the burst bound. Each one forced the mode to Shadow — the read-lane anomaly feeding the kill switch.", Value: float64(rs.Bursts)},
			metrics.Sample{Name: "tg_recon_reads_hour", Kind: metrics.Gauge, Help: "estate reads in the rolling hour, across ALL sessions — the cross-session volume nothing measured before TG-165.", Value: float64(rs.ReadsHour)},
			metrics.Sample{Name: "tg_recon_reads_burst_window", Kind: metrics.Gauge, Help: "estate reads inside the burst window (the rate the burst alarm watches).", Value: float64(rs.ReadsBurst)},
			metrics.Sample{Name: "tg_recon_targets_hour", Kind: metrics.Gauge, Help: "DISTINCT estate objects read in the rolling hour — the fan-out signal: 500 reads of one host is a poll, 500 reads of 500 hosts is a sweep. Reported, never gated on.", Value: float64(rs.TargetsHour)},
			metrics.Sample{Name: "tg_recon_fanout_flagged_total", Kind: metrics.Counter, Help: "OBSERVE-ONLY per-session fan-out flags (TG-325): a session touched >= the distinct-target ceiling — a methodical sweep whose composition is anomalous even when its read count is under every volume bound. Reported, never refused. Non-zero warrants a look at which session swept.", Value: float64(rs.FanoutFlags)},
			metrics.Sample{Name: "tg_recon_fanout_ceiling", Kind: metrics.Gauge, Help: "the per-session distinct-target ceiling the fan-out flag fires at (0 = disabled) — so an alert reads against the ceiling in force, not a hard-coded copy.", Value: float64(rs.FanoutObserve)},
			metrics.Sample{Name: "tg_recon_reads_hour_limit", Kind: metrics.Gauge, Help: "the per-hour recon bound in force, so an alert can be written against the ratio rather than a hard-coded copy of the constant.", Value: float64(rs.PerHourLimit)},
			metrics.Sample{Name: "tg_recon_halted", Kind: metrics.Gauge, Help: "1 = the READ lane is halted (operator POST /halt): every estate read is refused with an explicit reason. 0 = reads are being served.", Value: halted},
		)
	}
	// THE OUTBOUND LANE (TG-160). Nothing in this stack measured egress at all: no allowlist, no
	// destination counter, no byte counter, on any channel. These series make an undeclared destination
	// ALERTABLE. The two that carry the signal are tg_egress_offallowlist_requests_total (did we talk to
	// somewhere we never declared) and tg_egress_offallowlist_bytes_out_total (how much left) — a count
	// alone cannot separate a health probe from a corpus upload. tg_egress_allowlist_rules is the vacuity
	// floor made live: a flat 0 there means the meter is comparing traffic against an empty declaration,
	// so the off-allowlist series is measuring nothing and must not be read as an intrusion.
	if a.egress != nil {
		es := a.egress.Snapshot()
		enforcing := 0.0
		if es.Enforcing {
			enforcing = 1
		}
		out = append(out,
			metrics.Sample{Name: "tg_egress_requests_total", Kind: metrics.Counter, Help: "outbound HTTP requests metered by the TG-160 egress meter, all destinations.", Value: float64(es.Requests)},
			metrics.Sample{Name: "tg_egress_bytes_out_total", Kind: metrics.Counter, Help: "outbound request-body bytes metered by the TG-160 egress meter. VOLUME is the exfil dimension a destination count cannot see.", Value: float64(es.BytesOut)},
			metrics.Sample{Name: "tg_egress_bytes_in_total", Kind: metrics.Counter, Help: "inbound response-body bytes read back from outbound calls.", Value: float64(es.BytesIn)},
			metrics.Sample{Name: "tg_egress_offallowlist_requests_total", Kind: metrics.Counter, Help: "outbound requests to a destination this deployment never DECLARED. Non-zero is the covert-channel signal — read it against tg_egress_allowlist_rules before treating it as an intrusion.", Value: float64(es.OffRequests)},
			metrics.Sample{Name: "tg_egress_offallowlist_bytes_out_total", Kind: metrics.Counter, Help: "request-body bytes sent to UNDECLARED destinations. This is the exfil volume.", Value: float64(es.OffBytesOut)},
			metrics.Sample{Name: "tg_egress_offallowlist_destinations", Kind: metrics.Gauge, Help: "distinct undeclared destination hosts seen (bounded; overflow folds into the 'other' bucket).", Value: float64(len(es.OffAllowlist))},
			metrics.Sample{Name: "tg_egress_refused_total", Kind: metrics.Counter, Help: "outbound requests REFUSED for being off-allowlist. Always 0 unless TG_EGRESS_MODE=enforce.", Value: float64(es.Refusals)},
			metrics.Sample{Name: "tg_egress_allowlist_rules", Kind: metrics.Gauge, Help: "declared outbound destinations the meter compares traffic against. A flat 0 means the meter is measuring against nothing (the vacuity condition), NOT that egress is clean.", Value: float64(es.AllowlistRules)},
			metrics.Sample{Name: "tg_egress_enforcing", Kind: metrics.Gauge, Help: "1 = off-allowlist destinations are BLOCKED; 0 = metered only (the default posture).", Value: enforcing},
		)
	}
	// EVERY OTHER NAMED BREAKER in the shared store (TG-221): one circuit_breaker_state + failure_count series
	// per row, so a tripped PRODUCTION MODEL-PATH breaker (model-<tier>) is alertable exactly like the mutation
	// breaker. "mutation" is skipped here because it is already emitted above from its ARMED instance, whose
	// read fails CLOSED (an unreadable safety breaker reports OPEN) — a second series from the raw row would
	// both duplicate the label set (an invalid exposition) and report the softer of the two readings. A store
	// read error emits nothing rather than a fabricated zero: absent is honest, "0 = closed" would not be.
	if a.breakerStore != nil {
		if recs, err := a.breakerStore.List(ctx); err == nil {
			for _, rec := range recs {
				if rec.Name == "mutation" {
					continue
				}
				nstate := breaker.StateValue(rec.State)
				out = append(out,
					metrics.Sample{Name: "circuit_breaker_state", Kind: metrics.Gauge, Help: "named circuit breaker: 0 closed / 1 half-open / 2 open", Value: nstate, Labels: map[string]string{"name": rec.Name}},
					// TG-452: tg_-prefixed forward name, dual-emitted for the same value + labels (see the mutation breaker above).
					metrics.Sample{Name: "tg_breaker_state", Kind: metrics.Gauge, Help: "named circuit breaker: 0 closed / 1 half-open / 2 open (tg_-prefixed forward name for circuit_breaker_state, TG-452)", Value: nstate, Labels: map[string]string{"name": rec.Name}},
					metrics.Sample{Name: "circuit_breaker_failure_count", Kind: metrics.Gauge, Help: "consecutive-failure counter of a named circuit breaker", Value: float64(rec.FailureCount), Labels: map[string]string{"name": rec.Name}},
				)
				// SINCE WHEN (TG-347). The state gauge says a breaker is OPEN and nothing says for how long,
				// so a LATCHED trip is indistinguishable from one that fired a minute ago. Measured
				// 2026-08-06: judge-death read OPEN on both planes for days on a demonstrably live judge,
				// blocking every skill graduation, and the only way to learn WHEN it tripped was to read the
				// worker log. rec.OpenedAt is already carried on the record — it was simply never published.
				//
				// Emitted ONLY while open, and that is deliberate: OpenedAt is documented as "zero unless
				// State == StateOpen", so publishing it for a closed breaker would export a 1970 timestamp
				// that every dashboard would render as an ancient trip. Absent means closed; a present series
				// means open, and its value is when.
				if !rec.OpenedAt.IsZero() {
					out = append(out,
						metrics.Sample{
							Name: "circuit_breaker_opened_at_seconds", Kind: metrics.Gauge,
							Help: "unix time this named breaker OPENED; absent while it is closed. " +
								"time() - this is how long the trip has been latched — a breaker that has been " +
								"open for days on a healthy dependency is a false trip nobody re-armed",
							Value:  float64(rec.OpenedAt.Unix()),
							Labels: map[string]string{"name": rec.Name},
						},
					)
				}
			}
		}
	}
	// The COST/BUDGET spend guard gauges (spec/013 REQ-1211/1212): the durable UTC-day accrued spend and the
	// $-ceiling breaker's state. Read-only, fail-open (a store read error reports $0 / closed, logged in the
	// Accountant — a metrics read never halts). Emitted only when cost tracking is configured (a.cost != nil).
	//
	// THE DENOMINATOR IS PUBLISHED FIRST, AND UNCONDITIONALLY. Until now every cost gauge was inside
	// `if a.cost != nil`, so a deployment with no budget configured emitted NOTHING — measured on
	// dc1tg01 2026-08-06: `tg_cost_breaker_state`, `tg_cost_usd_today` and `tg_cost_spend_usd_total`
	// all returned ZERO SERIES from Prometheus while 3.18M model tokens had been spent. An operator's
	// dashboard showed no cost panel, which is indistinguishable from a healthy one.
	//
	// Two bits, because there are three postures and `tg_cost_breaker_state = 0` collapses all of them:
	//   metering=0 enforcing=0  nothing configured — the gateway is not even wrapped
	//   metering=1 enforcing=0  rates set, no ceiling — the breaker CANNOT trip; state 0 means nothing
	//   metering=1 enforcing=1  a ceiling is armed; state 0 now genuinely means "within budget"
	// Without these, the third case and the first two render identically. `absent()` on tg_cost_metering is
	// the vacuity floor for the whole family.
	out = append(out,
		metrics.Sample{Name: "tg_cost_metering", Kind: metrics.Gauge, Help: "1 = a rate or budget is configured, so spend is being accrued; 0 = no TG_COST_* setting at all and the model gateway is left un-wrapped. ALWAYS emitted — its absence is a dead exporter, not a cheap deployment.", Value: boolGauge(a.costCfg.Enabled())},
		metrics.Sample{Name: "tg_cost_enforcing", Kind: metrics.Gauge, Help: "1 = a positive daily budget or session ceiling is armed, so the cost breaker CAN trip; 0 = meter-only, and tg_cost_breaker_state=0 therefore means 'nothing can open it' rather than 'within budget'. ALWAYS emitted.", Value: boolGauge(a.costCfg.Enforces())},
	)
	if a.cost != nil {
		out = append(out,
			metrics.Sample{Name: "tg_cost_usd_today", Kind: metrics.Gauge, Help: "approximate USD spend accrued so far this UTC day (the cost breaker's daily accumulator)", Value: a.cost.TodayUSD(ctx)},
			metrics.Sample{Name: "tg_cost_breaker_state", Kind: metrics.Gauge, Help: "cost/budget breaker: 0 closed / 2 open (over budget). Read it BESIDE tg_cost_enforcing: with enforcing=0 nothing can ever open this.", Value: a.cost.StateValue(ctx), Labels: map[string]string{"name": "cost"}},
		)
	}
	// The CREDENTIAL health gauge (TG-113): the boot preflight proved (or failed to prove) that the worker's
	// REAL runtime user can resolve+read+parse every configured SSH private key. 0 = at least one is missing/
	// unreadable/unparseable ⇒ native SSH investigation + actuation is DEGRADED (the silent-kill made loud);
	// 1 = all configured refs are usable. Emitted only when at least one SSH key ref is configured — a
	// worker with no SSH surface has nothing to report and stays silent (no false-negative alert).
	if a.sshCredConfigured > 0 {
		ready := 0.0
		if a.sshCredReady {
			ready = 1
		}
		out = append(out, metrics.Sample{Name: "tg_ssh_credential_ready", Kind: metrics.Gauge, Help: "SSH credential preflight: 1 = every configured SSH key ref resolves+parses for the worker's runtime user; 0 = at least one is missing/unreadable/unparseable (native SSH investigation + actuation DEGRADED)", Value: ready, Labels: map[string]string{"component": "worker"}})
	}
	// The OBSERVE-ONLY agent-loop/verify/governance metrics recorded by the Runner's activities (spec/012),
	// collected from the process-global emitter the composition root installed. nil (no default set — e.g. a
	// unit test that never boots the worker) contributes nothing. Read-only; emits counts + bounded enum
	// labels only, never a secret.
	out = append(out, observe.Collect()...)
	// Per-domain actor-evidence counts. Emitted only when readers were armed.
	out = append(out, a.actorTally.Collect()...)
	// The benchmark axes, sampled on a tick from the SAME db.Aggregate that axisscore prints — one derivation,
	// so the dashboard and the CLI cannot disagree.
	out = append(out, a.axes.Collect()...)
	// Carve-out remaining life, in SECONDS and signed: positive = still in force, negative = lapsed. A
	// duration is emitted rather than the absolute deadline because the alert an operator wants is "less
	// than a week left", which is a threshold on this value directly and needs no clock arithmetic in the
	// query. Both series are emitted for every carve-out so `expired` is never inferred from absence.
	if len(a.carveOuts) > 0 {
		now := time.Now().UTC()
		for _, e := range attribution.CarveOutExpiries(attribution.Config{CarveOuts: a.carveOuts}, now) {
			lbl := map[string]string{"carve_out": e.ID, "domain": e.Domain}
			expired := 0.0
			if e.Expired {
				expired = 1
			}
			out = append(out,
				metrics.Sample{Name: "tg_attribution_carveout_remaining_seconds", Kind: metrics.Gauge,
					Help:  "Seconds until this attribution carve-out lapses (negative = already lapsed). A carve-out suspends the security path for its actors on its hosts; past its bound they revert toward stand-down, which withholds actuation, so a lapse silently stops auto-heal on those hosts.",
					Value: e.Remaining.Seconds(), Labels: lbl},
				metrics.Sample{Name: "tg_attribution_carveout_expired", Kind: metrics.Gauge,
					Help:  "1 = this attribution carve-out is expired or has no bound at all, so it matches nothing and its hosts no longer resolve to authorized-test; 0 = in force.",
					Value: expired, Labels: lbl})
		}
	}

	// The wiring registers (TG-250): dark-seam and seam-YIELD gauges, unconditionally on /metrics.
	//
	// KIND IS GAUGE FOR ALL OF THEM, including the *_total pair. offered/produced are re-reported
	// snapshots of a running total, not a monotonic counter this handler owns; declaring them Counter
	// would invite rate() over a series that resets when the worker restarts.
	if a.wiringSamples != nil {
		for _, ws := range a.wiringSamples() {
			help := "TG-250 wiring register."
			switch ws.Name {
			case "tg_wiring_seam_offered_total":
				help = "units of work OFFERED to a seam. Read beside _produced_total: a gap is the filter doing its job, and a gap that becomes total is starvation."
			case "tg_wiring_seam_produced_total":
				help = "units the seam actually PRODUCED. Zero against a non-zero _offered_total is a seam that is wired, running, and emitting nothing."
			case "tg_wiring_seam_starved":
				help = "1 when work was offered and NOTHING was produced. This is the alertable one."
			case "tg_wiring_seam_yield_unobserved":
				help = "1 when nothing reports this seam's yield at all — the register's own vacuity floor. Uncovered, never healthy."
			case "tg_wiring_seam_dark":
				help = "1 when the seam was never BOUND at boot. Distinct from starved: dark is unbound, starved is bound and yielding nothing."
			}
			out = append(out, metrics.Sample{
				Name: ws.Name, Kind: metrics.Gauge, Help: help, Value: ws.Value, Labels: ws.Labels,
			})
		}
	}

	// THE MODE, AS AN ENUM GAUGE. All four modes are emitted every scrape, exactly one at 1.
	//
	// Emitting only the ACTIVE mode would leave a stale series behind on every transition, and — worse —
	// a rule written against an absent series cannot distinguish "not in Shadow" from "the worker stopped
	// reporting". Both readings must be present for a comparison to mean anything.
	if a.policyMode != nil {
		active := a.policyMode()
		for _, m := range []string{"Shadow", "HITL", "Semi-auto", "Full-auto"} {
			v := 0.0
			if m == active {
				v = 1
			}
			out = append(out, metrics.Sample{
				Name: "tg_policy_mode", Kind: metrics.Gauge,
				Help: "the owner-set autonomy mode; exactly one label is 1. Shadow and HITL never actuate, " +
					"Semi-auto and Full-auto may. tg_may_actuate is downstream of this AND the boot preflight.",
				Value: v, Labels: map[string]string{"mode": m},
			})
		}
	}

	// The estate graph the mutation gate reasons over (TG-343). Read live from the atomic holder — no
	// database, so this stays a pure read on the scrape path.
	if a.estateSize != nil {
		out = append(out, a.estateSize()...)
	}
	// TG's OWN dependency-placement concentration (TG-394) — from the same live graph, same scrape-path read.
	if a.selfDepConcentration != nil {
		out = append(out, a.selfDepConcentration()...)
	}
	// TG's OWN dependency-capability reachability + degraded rollup (TG-394 slice 3) — same live graph.
	if a.selfDepReachable != nil {
		out = append(out, a.selfDepReachable()...)
	}
	// TG's estate-doc grounding coverage (TG-86 slice 1b) — the ingest reported where Prometheus scrapes.
	if a.estateDocCoverage != nil {
		out = append(out, a.estateDocCoverage()...)
	}
	// TG's estate-observation census (TG-180) — how much of the estate it can actually see.
	if a.observationCensus != nil {
		out = append(out, a.observationCensus()...)
	}
	// TG's coverage-of-the-unmeasured (TG-180 part 2) — how much of that blindness the probe has TESTED.
	if a.observationProbe != nil {
		out = append(out, a.observationProbe()...)
	}
	// TG's fastest detector's yield (TG-350 follow-through).
	if a.pveLivenessYield != nil {
		out = append(out, a.pveLivenessYield()...)
	}
	if a.authlogYield != nil {
		out = append(out, a.authlogYield()...)
	}
	// THE DECISION PLANE (TG-380). Until now `tg_suppression_decisions` existed only behind the
	// observability export loop, which is unconfigured in production — so zero series were emitted.
	if a.stageDecisions != nil {
		out = append(out, a.stageDecisions()...)
	}
	if a.posturePolicyWarnings != nil {
		out = append(out, a.posturePolicyWarnings()...)
	}
	if a.suppressionDecisions != nil {
		out = append(out, a.suppressionDecisions()...)
	}

	// The human approval queue (TG-173). Emitted as computed by the job — see pollQueueSamples — so this
	// handler stays a pure reader and a scrape can never trigger a database query.
	if a.syntheticLeak != nil {
		out = append(out, a.syntheticLeak()...)
	}
	if a.pollQueue != nil {
		out = append(out, a.pollQueue()...)
	}

	// What the upstream HAD (TG-344), beside what arrived. Emitted as computed by the job so this handler
	// stays a pure reader and a scrape never triggers a network call to the estate.
	if a.upstreamProbe != nil {
		out = append(out, a.upstreamProbe()...)
	}

	// Host-diagnostic reach (TG-271). Emitted as computed by the job so this handler stays a pure reader and
	// a scrape never triggers a database query or a DNS lookup.
	if a.knownHostsCoverage != nil {
		out = append(out, a.knownHostsCoverage()...)
	}

	// Intake liveness (TG-336). Emitted as computed by the job — see ingestFreshnessSamples — so this
	// handler stays a pure reader and a scrape can never trigger a database query.
	if a.ingestFreshness != nil {
		out = append(out, a.ingestFreshness()...)
	}

	// The premise behind TG-302 (TG-345). Same discipline as the intake gauges: computed by the background
	// job, read here, so a scrape never queries the database.
	if a.predictionWidth != nil {
		out = append(out, a.predictionWidth()...)
	}
	if a.categoryCover != nil {
		out = append(out, a.categoryCover()...)
	}
	if a.loopClosure != nil {
		out = append(out, a.loopClosure()...)
	}
	if a.ledgerShape != nil {
		out = append(out, a.ledgerShape()...)
	}
	if a.evidenceShape != nil {
		out = append(out, a.evidenceShape()...)
	}

	// The actuation governor (TG-286). The BUDGET is emitted beside the counts deliberately: a refusal
	// count is unreadable without the limits it was measured against, and a window silently falling back
	// to a default is exactly how a governor ends up admitting everything while looking installed.
	// `ok` is the absence signal, and it is deliberately not a zero-valued struct: a deployment with no
	// policy engine (no pool ⇒ no engine ⇒ no governor) must publish NOTHING here, so the series is ABSENT
	// rather than reporting a confident all-zero. A fabricated zero is exactly what makes a dark control
	// look healthy, which is the defect this whole ticket is about.
	if st, ok := policyRateStats(a.policyRateGov); ok {
		out = append(out,
			metrics.Sample{Name: "tg_policy_rate_governed_total", Kind: metrics.Counter,
				Help: "policy verdicts where the rate governor had a POSITIVE limit in force. Read as the " +
					"denominator for tg_policy_rate_clamped_total, and beside tg_policy_rate_ungoverned_total.",
				Value: float64(st.Governed)},
			metrics.Sample{Name: "tg_policy_rate_ungoverned_total", Kind: metrics.Counter,
				Help: "policy verdicts where the governor was consulted and the resolved rate_limit was <= 0, " +
					"so NOTHING was enforced. This climbing while tg_policy_rate_governed_total stays 0 means " +
					"the control is decoration: the template advertises a rate_limit that never reaches the " +
					"governor. That is the state TG-339 found it in and it is invisible in a clamp count.",
				Value: float64(st.Ungoverned)},
			metrics.Sample{Name: "tg_policy_rate_clamped_total", Kind: metrics.Counter,
				Help: "auto verdicts the governor tightened to approve for exceeding the per-window budget. " +
					"A routing decision, not a failure — the action was sent to a human, not dropped.",
				Value: float64(st.Clamped)},
			metrics.Sample{Name: "tg_policy_rate_window_seconds", Kind: metrics.Gauge,
				Help:  "the governor's trailing window — the denominator for the counts above.",
				Value: st.Window.Seconds()},
		)
	}
	if a.actuationLimiter != nil {
		st := a.actuationLimiter()
		out = append(out,
			metrics.Sample{Name: "tg_actuation_admitted_total", Kind: metrics.Counter,
				Help: "actuations the frequency governor ADMITTED to the effect. Zero with a live estate " +
					"means either nothing actuated or the interceptor is not calling Admit at all.",
				Value: float64(st.Admitted)},
			metrics.Sample{Name: "tg_actuation_refused_total", Kind: metrics.Counter,
				Help: "actuations REFUSED for exceeding the rate budget. A throttle, not an execution " +
					"failure — the action was not run.",
				Value: float64(st.Refused)},
			metrics.Sample{Name: "tg_actuation_in_flight", Kind: metrics.Gauge,
				Help: "actuations currently between Admit and Release. A value that only ever grows is a " +
					"LEAKED LEASE, which wedges the lane and is invisible in every other series here.",
				Value: float64(st.InFlight)},
			metrics.Sample{Name: "tg_actuation_limit_window_seconds", Kind: metrics.Gauge,
				Help:  "the governor's trailing window — the denominator for the counts above.",
				Value: st.Window.Seconds()},
			metrics.Sample{Name: "tg_actuation_limit", Kind: metrics.Gauge,
				Help:  "the configured actuation budget, by scope.",
				Value: float64(st.SessionPerWindow), Labels: map[string]string{"scope": "session_per_window"}},
			metrics.Sample{Name: "tg_actuation_limit", Kind: metrics.Gauge,
				Help:  "the configured actuation budget, by scope.",
				Value: float64(st.TargetPerWindow), Labels: map[string]string{"scope": "target_per_window"}},
		)
	}

	return out
}

// startWorkerAdmin serves the admin surface on addr in a background goroutine (never blocks worker boot).
func startWorkerAdmin(addr string, a *workerAdmin) {
	srv := &http.Server{Addr: addr, Handler: a.mux(), ReadHeaderTimeout: 5 * time.Second}
	halt := "/halt DISABLED (no TG_ADMIN_TOKEN_REF resolved — fail closed)"
	if a.haltAuthed {
		halt = "/halt armed (bearer-authenticated, halt-only — no enable path)"
	}
	go func() {
		log.Printf("worker admin listener up on %s — /metrics (read-only); %s", addr, halt)
		if err := srv.ListenAndServe(); err != nil {
			log.Printf("worker admin listener exited: %v", err)
		}
	}()
}

// policyRateStats reads the policy rate governor's counters, reporting ok=false when no governor exists.
// Separated from the metrics block so the "absent, not zero" decision has one place to be read and one
// place to be mutated against.
func policyRateStats(f func() (policy.RateGovernorStats, bool)) (policy.RateGovernorStats, bool) {
	if f == nil {
		return policy.RateGovernorStats{}, false
	}
	return f()
}
