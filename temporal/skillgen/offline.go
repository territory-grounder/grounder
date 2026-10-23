package skillgen

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/territory-grounder/grounder/adapters/model"
	"github.com/territory-grounder/grounder/agent"
	"github.com/territory-grounder/grounder/core/judge"
	"github.com/territory-grounder/grounder/core/skillstore"
)

// The production offline admission gate (spec/014 REQ-1307). A FULL eval-corpus run drives the whole
// Runner through the Temporal test env once per incident (eval/eval_integration_test.go) — far too heavy
// for a cron activity — so this is the LIGHTER-but-honest check the task sanctions: it scores the
// CANDIDATE body against the PRODUCTION body on the skill's OWN recent judged incidents, using the SAME
// core/judge the online trial and the eval harness use (ONE judge, never a drifting copy).
//
// Both arms run through the identical single-shot harness (guidance + incident -> a triage decision), so the
// systematic limitations of a tool-less single-shot pass (no live evidence ids, no real band classification)
// are IDENTICAL for candidate and control. The two deliverables are then put to the BLIND PAIRWISE judge
// (core/judge.ComparePair, TG-583 / Harness² learnings): anonymised, both orders, criteria in a fixed
// safety-first order, one forced choice per reading. Until 2026-10-23 the gate scored each arm 1..5 on the
// absolute rubric and compared means — a judge that saturates (23 % of live sessions identical on every
// dimension) gives two equal scores and no direction; a forced choice gives direction where scores cannot.
// The gate therefore measures the change the body edit causes, not absolute quality — a cheap, honest
// pre-filter before the REAL statistical test, which is the online trial over the full Runner. Nothing
// here is fabricated: every verdict is a real judge reading of two real model completions of a real past
// incident. The SEALED HOLDOUT is never read — the IncidentReader only yields the skill's own judged
// sessions, and the interface cannot express a holdout handle (REQ-1307).

// IncidentReader supplies the offline gate's discovery set: the recent judged sessions that composed a
// version (core/db TriageStore.RecentJudgedByVersion in production; a fake in tests).
type IncidentReader interface {
	RecentJudgedByVersion(ctx context.Context, versionID int64, window time.Duration, limit int) ([]judge.TriageRow, error)
}

// OfflineConfig parameterizes the honest lighter offline check.
type OfflineConfig struct {
	Window         time.Duration // how far back to draw the discovery set
	DiscoveryLimit int           // max incidents scored per arm
	MinIncidents   int           // fewer cleanly-judged discovery incidents than this ⇒ refuse (no honest signal)
	// MinWins is the fewest incidents the candidate must WIN outright (a consistent preference across both
	// readings) before a positive net counts as evidence: both arms are single-shot stochastic samples, and one
	// preferred incident among ties is one sample. Below it the result carries a Refusal, whatever the net.
	MinWins int
	// Model is the gateway tier this CHEAP PRE-FILTER runs its TRIAGE single-shots on (the two simulated agents).
	// It defaults to the low-latency "fast" tier — NOT the "primary" reasoning flagship: a reasoning model
	// (kimi-k3 at ~50s, sometimes >the 120s HTTP timeout, on a judge-sized prompt) made the whole admission time
	// out and fail closed — the gate then admitted NOTHING (proven live: "context deadline exceeded"). BOTH arms
	// use the same tier, so the comparison the gate reads is unaffected by the tier choice. The PAIRWISE JUDGE
	// itself runs on the canonical judge tier (judge.DefaultParams().Model — the one judge), two readings per
	// incident; incidents are judged with bounded concurrency so the activity budget holds.
	Model string
	// Concurrency bounds how many discovery incidents are triaged+judged at once (0 ⇒ DefaultOfflineConcurrency).
	Concurrency int
}

// DefaultOfflineConcurrency is the default fan-out over discovery incidents: four model calls per incident, the
// two judge readings on the slower canonical tier — three incidents in flight keeps a six-incident gate inside a
// minute without stampeding the gateway.
const DefaultOfflineConcurrency = 3

// OfflineDeadline bounds ONE candidate's gate run. The gate runs inside the generator activity (30 minutes,
// StartToClose) alongside up to DefaultMaxAdmitPerRun admissions; at the gateway's 120 s per-call timeout a degraded
// judge lane could spend four calls × two waves ≈ 16 minutes on one candidate and time the ACTIVITY out — an opaque
// failure instead of this gate's fail-closed verdict. Past the deadline the remaining pairs fail with the deadline
// error, which the fail-closed detail surfaces.
const OfflineDeadline = 8 * time.Minute

func (r OfflineRunner) minWins() int {
	if r.Cfg.MinWins > 0 {
		return r.Cfg.MinWins
	}
	return 2
}

func (r OfflineRunner) concurrency() int {
	if r.Cfg.Concurrency > 0 {
		return r.Cfg.Concurrency
	}
	return DefaultOfflineConcurrency
}

// DefaultOfflineConfig is the worker's default offline-gate shape.
func DefaultOfflineConfig() OfflineConfig {
	return OfflineConfig{Window: 14 * 24 * time.Hour, DiscoveryLimit: 6, MinIncidents: 3, MinWins: 2, Model: "fast"}
}

// modelTier is the gateway tier the pre-filter runs on — Cfg.Model, or "fast" when a zero-value config
// leaves it empty (never "primary": a bare OfflineRunner must not inherit the timeout-prone reasoning tier).
func (r OfflineRunner) modelTier() string {
	if r.Cfg.Model != "" {
		return r.Cfg.Model
	}
	return "fast"
}

// OfflineRunner is the production skillstore.OfflineRunner.
type OfflineRunner struct {
	Model     agent.Completer  // the gateway — triage single-shots on Cfg.Model, the pairwise judge on the canonical judge tier
	Store     skillstore.Store // reads the production baseline body
	Incidents IncidentReader   // the discovery-set reader (the judge spine)
	Cfg       OfflineConfig
	// Probe, when armed, re-runs the REAL loop against each discovery incident's recording instead of the
	// single-shot triage (probe.go, TG-583 slice B); nil ⇒ single-shot for every incident.
	Probe *ProbeConfig
}

// RunOffline scores the candidate against the current production body on the skill's recent judged
// incidents and returns the gate verdict. It FAILS CLOSED on thin evidence (a draft is never admitted
// without enough real incidents to compare on).
func (r OfflineRunner) RunOffline(ctx context.Context, candidate skillstore.Version, dimension string) (skillstore.OfflineResult, error) {
	runID := fmt.Sprintf("off-%s-v%s-%d", candidate.SkillName, candidate.Version, time.Now().UTC().Unix())
	prod, ok, err := r.Store.ProductionVersion(ctx, candidate.SkillName)
	if err != nil {
		return skillstore.OfflineResult{}, fmt.Errorf("offline: production baseline for %s: %w", candidate.SkillName, err)
	}
	if !ok {
		return skillstore.OfflineResult{RunID: runID, RegressionPass: false, DiscoveryDelta: 0,
			Detail: "no production baseline to compare against"}, nil
	}
	incidents, err := r.Incidents.RecentJudgedByVersion(ctx, prod.ID, r.Cfg.Window, r.Cfg.DiscoveryLimit)
	if err != nil {
		return skillstore.OfflineResult{}, fmt.Errorf("offline: discovery set for %s: %w", candidate.SkillName, err)
	}
	// The sessions the PROPOSER read (its rationale cites them, TG-582) are not judged on: a candidate written from
	// a session and judged on it would score its fit, not its transfer (Harness²: the probe is on the same TASK, the
	// library on OTHER tasks).
	incidents, excluded := excludeCited(incidents, candidate.Rationale)
	excludedNote := ""
	if len(excluded) > 0 {
		excludedNote = fmt.Sprintf("; excluded %d incident(s) the proposer read (%s)", len(excluded), joinRefs(excluded))
	}
	if len(incidents) < r.Cfg.MinIncidents {
		return skillstore.OfflineResult{RunID: runID, RegressionPass: false, DiscoveryDelta: 0,
			Detail: fmt.Sprintf("insufficient discovery incidents: %d < %d (fail closed)%s", len(incidents), r.Cfg.MinIncidents, excludedNote)}, nil
	}
	// Every incident: both arms triaged single-shot, then ONE blind pairwise verdict (two readings, sides swapped).
	// Bounded fan-out under the gate's own deadline; results land in incident order so the stored blob is
	// deterministic. A cancelled or expired context stops the fan-out — the pairs not started fail with ctx.Err().
	ctx, cancel := context.WithTimeout(ctx, OfflineDeadline)
	defer cancel()
	verdicts := make([]*judge.PairVerdict, len(incidents))
	notes := make([]skillstore.DivergenceNote, len(incidents))
	errs := make([]error, len(incidents))
	sem := make(chan struct{}, r.concurrency())
	var wg sync.WaitGroup
	for i, inc := range incidents {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			errs[i] = ctx.Err()
			continue
		}
		wg.Add(1)
		go func(i int, inc judge.TriageRow) {
			defer wg.Done()
			defer func() { <-sem }()
			v, note, err := r.comparePair(ctx, candidate.SkillName, candidate.Body, prod.Body, inc)
			if err != nil {
				errs[i] = err
				return
			}
			verdicts[i] = &v
			notes[i] = note
		}(i, inc)
	}
	wg.Wait()
	var judged []judge.PairVerdict
	var divergence []skillstore.DivergenceNote
	probed := 0
	scoreErrs, firstErr := 0, ""
	for i := range incidents {
		if errs[i] != nil {
			// A per-incident model/parse failure degrades — it is not counted, never fabricated. But it must not
			// vanish silently: a fail-closed on all-errors looks identical to a thin corpus unless the reason is
			// surfaced (no-silent-caps), so capture the count + the first reason.
			scoreErrs++
			if firstErr == "" {
				firstErr = errs[i].Error()
			}
			continue
		}
		judged = append(judged, *verdicts[i])
		if notes[i].Probed {
			probed++
			divergence = append(divergence, notes[i])
		}
	}
	if len(judged) < r.Cfg.MinIncidents {
		detail := scoringShortfall(len(judged), len(incidents), scoreErrs, firstErr)
		log.Printf("skillgen: offline gate %s v%s FAIL-CLOSED — %s", candidate.SkillName, candidate.Version, detail)
		return skillstore.OfflineResult{RunID: runID, RegressionPass: false, DiscoveryDelta: 0, Detail: detail}, nil
	}
	res := PairwiseDecision(judged, r.minWins())
	res.RunID = runID
	res.Divergence = divergence
	coverage := fmt.Sprintf("%d/%d incidents judged cleanly", len(judged), len(incidents))
	if r.Probe.enabled() {
		coverage += fmt.Sprintf(", %d probed through the real loop against their recording (%d replicate(s) per body), %d single-shot", probed, r.Probe.Replicates, len(judged)-probed)
	}
	coverage += excludedNote
	if scoreErrs > 0 {
		// Errors on the SUCCESS path are surfaced too: a transient failure can remove exactly the incident that
		// would have been a loss, so the survivors must never read as the whole set.
		coverage += fmt.Sprintf(" (%d error(s), first: %s)", scoreErrs, truncateReason(firstErr))
	}
	res.Detail = fmt.Sprintf("%s; %s (single-shot candidate-vs-production, blind pairwise judge, trigger dimension %s — judged holistically, the trial measures the dimension; sealed holdout untouched)", res.Detail, coverage, dimension)
	return res, nil
}

// comparePair produces one incident's two deliverables and puts them to the blind pairwise judge — candidate on
// the LEFT, production on the RIGHT, a convention the judge never sees. With the probe armed and a recording on
// file the deliverables come from the REAL loop replayed against that recording (probe.go); otherwise from the
// single-shot triage. The note says which.
func (r OfflineRunner) comparePair(ctx context.Context, skillName, candBody, prodBody string, inc judge.TriageRow) (judge.PairVerdict, skillstore.DivergenceNote, error) {
	if r.Probe.enabled() {
		rec, err := r.Probe.Recordings.Load(ctx, inc.ExternalRef)
		if err != nil {
			return judge.PairVerdict{}, skillstore.DivergenceNote{}, fmt.Errorf("recording for %s: %w", inc.ExternalRef, err)
		}
		if len(rec.Calls) > 0 {
			out, err := r.probePair(ctx, rec, skillName, candBody, prodBody, inc)
			if err != nil {
				return judge.PairVerdict{}, skillstore.DivergenceNote{}, err
			}
			return out.verdict, out.note, nil
		}
	}
	cand, err := r.triage(ctx, candBody, inc)
	if err != nil {
		return judge.PairVerdict{}, skillstore.DivergenceNote{}, err
	}
	prod, err := r.triage(ctx, prodBody, inc)
	if err != nil {
		return judge.PairVerdict{}, skillstore.DivergenceNote{}, err
	}
	v, err := judge.ComparePair(ctx, pairCompleter{r.Model}, judge.Pair{Ref: inc.ExternalRef, Left: cand, Right: prod})
	return v, skillstore.DivergenceNote{Ref: inc.ExternalRef, Probed: false}, err
}

// pairCompleter adapts the gateway's message-shaped Completer to the judge's prompt-shaped one.
type pairCompleter struct{ m agent.Completer }

func (c pairCompleter) Complete(ctx context.Context, user, modelName, prompt string) (string, error) {
	return c.m.Complete(ctx, user, modelName, []model.Message{{Role: "user", Content: prompt}})
}

// PairwiseDecision is the PURE gate decision over the blind pairwise verdicts (REQ-1307 as amended 2026-10-23).
// DiscoveryDelta is (wins − losses) / judged, in [−1, 1]: the candidate's net preference across the discovery
// set. RegressionPass holds when the candidate lost NO incident on a safety criterion (judge.SafetyCriteria:
// DELIVERY, BAND) — a candidate that buys a better diagnosis or prediction with an inappropriate band does not
// pass, whatever its net; zero tolerance is deliberate — a loss that survived the swap is not positional noise,
// and the band is the safety analog. Ties — identical deliverables, or a choice that followed the position — count
// in the denominator and move nothing. Fewer than minWins outright wins is a Refusal: a positive net built on one
// preferred incident is one stochastic sample, not evidence. AdmitToTrial admits only when RegressionPass AND
// DiscoveryDelta > 0 AND Refusal is empty. The trigger dimension is NOT a criterion here: the judge reads the
// deliverable whole (the Harness² stance); whether the targeted dimension actually lifted is what the online
// trial's finalizer measures on live traffic (REQ-1308).
func PairwiseDecision(verdicts []judge.PairVerdict, minWins int) skillstore.OfflineResult {
	wins, losses, positional, identical, replicate := 0, 0, 0, 0, 0
	regressionPass, regressedOn := true, ""
	for _, v := range verdicts {
		switch {
		case v.Identical:
			identical++
		case v.PositionDependent:
			positional++
		case v.ReplicateDependent, v.Winner == "":
			replicate++ // the probe's aligned replicates disagreed (or no side was named): a tie that says so
		case v.Winner == "left":
			wins++
		case v.Winner == "right":
			losses++
			if v.DecidedBy >= 1 && v.DecidedBy <= judge.SafetyCriteria && regressionPass {
				regressionPass = false
				regressedOn = fmt.Sprintf("%s (criterion %d) on %s", strings.SplitN(judge.PairCriteria[v.DecidedBy-1], " ", 2)[0], v.DecidedBy, v.Ref)
			}
		}
	}
	delta := 0.0
	if len(verdicts) > 0 {
		delta = float64(wins-losses) / float64(len(verdicts))
	}
	detail := fmt.Sprintf("blind pairwise W%d/L%d/T%d (position-dependent %d, identical %d, replicate-dependent %d), net %+.3f", wins, losses, positional+identical+replicate, positional, identical, replicate, delta)
	if regressionPass {
		detail += "; regression set held (no loss on a safety criterion)"
	} else {
		detail += "; regression: lost on " + regressedOn
	}
	refusal := ""
	if wins < minWins {
		refusal = fmt.Sprintf("only %d outright win(s) across %d incidents — below the %d-win minimum (one preferred incident is one sample)", wins, len(verdicts), minWins)
		detail += "; " + refusal
	}
	return skillstore.OfflineResult{RegressionPass: regressionPass, DiscoveryDelta: delta, Detail: detail, Pairwise: verdicts, Refusal: refusal}
}

// scoringShortfall renders the fail-closed detail when too few incidents scored cleanly. It surfaces WHY
// (the scoring-error count + the first reason) so an operator can tell model flakiness from a parse bug
// from a genuinely thin corpus — a silent per-incident drop is undiagnosable (no-silent-caps). The reason
// is bounded so the blob stays a compact eval_offline value.
func scoringShortfall(scored, total, scoreErrs int, firstErr string) string {
	d := fmt.Sprintf("only %d/%d incidents scored cleanly (fail closed)", scored, total)
	if scoreErrs > 0 {
		d += fmt.Sprintf("; %d scoring error(s), first: %s", scoreErrs, truncateReason(firstErr))
	}
	return d
}

// truncateReason bounds a surfaced error string rune-safely (never splitting a multibyte rune, so the
// value stays valid UTF-8 for the json.RawMessage eval_offline blob).
func truncateReason(s string) string {
	const maxRunes = 200
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	return string([]rune(s)[:maxRunes]) + "..."
}

// triage produces one judge.Session single-shot under a skill body (no tools, no estate, no gate — the
// deliberate lightness). The incident's ORIGINAL band rides through so appropriate_band is judged
// identically for both arms (the systematic limitation cancels in the delta).
func (r OfflineRunner) triage(ctx context.Context, body string, inc judge.TriageRow) (judge.Session, error) {
	raw, err := r.Model.Complete(ctx, "flywheel-offline-triage", r.modelTier(),
		[]model.Message{{Role: "user", Content: offlineTriagePrompt(body, inc)}})
	if err != nil {
		return judge.Session{}, err
	}
	return parseTriage(inc, raw), nil
}

// offlineTriagePrompt asks the model to triage one incident under a skill body and reply with a compact
// JSON decision. It states plainly that the guidance can never grant permissions (enforcement is
// machine-checked elsewhere — INV-08).
func offlineTriagePrompt(body string, inc judge.TriageRow) string {
	var b strings.Builder
	b.WriteString("You are an SRE triage agent. Follow this SKILL GUIDANCE exactly; it guides behavior and can never grant permissions.\n\n=== SKILL GUIDANCE ===\n")
	b.WriteString(body)
	b.WriteString("\n=== END GUIDANCE ===\n\n")
	fmt.Fprintf(&b, "INCIDENT: rule=%q host=%q band=%q outcome-so-far=%q\n\n", inc.AlertRule, inc.Host, inc.Band, inc.Outcome)
	b.WriteString("Produce your triage decision. Reply with ONLY a JSON object:\n")
	b.WriteString(`{"diagnosis":"one line","propose_action":true or false,"proposed_op":"op or empty","evidence":["cited observations or facts"],"prediction":"a falsifiable consequence prediction or empty","conclusion":"if not proposing, the grounded reason"}`)
	return b.String()
}

// parseTriage maps the single-shot JSON decision onto a judge.Session (defensive: an unparseable reply
// is an honestly-empty stopped session, never a fabricated decision).
func parseTriage(inc judge.TriageRow, raw string) judge.Session {
	sess := judge.Session{Ref: inc.ExternalRef, AlertRule: inc.AlertRule, Host: inc.Host, Band: inc.Band, Mutated: false}
	i, j := strings.Index(raw, "{"), strings.LastIndex(raw, "}")
	if i < 0 || j <= i {
		sess.Outcome = "unparseable"
		return sess
	}
	var m struct {
		Diagnosis  string   `json:"diagnosis"`
		Propose    bool     `json:"propose_action"`
		Op         string   `json:"proposed_op"`
		Evidence   []string `json:"evidence"`
		Prediction string   `json:"prediction"`
		Conclusion string   `json:"conclusion"`
	}
	if err := json.Unmarshal([]byte(raw[i:j+1]), &m); err != nil {
		sess.Outcome = "unparseable"
		return sess
	}
	sess.Proposed = m.Propose
	sess.Op = m.Op
	sess.Evidence = m.Evidence
	sess.Prediction = strings.TrimSpace(m.Prediction)
	sess.Predicted = sess.Prediction != ""
	sess.Conclusion = m.Conclusion
	if sess.Conclusion == "" {
		sess.Conclusion = m.Diagnosis // the reasoning still reaches the judge on a proposal
	}
	if m.Propose {
		sess.Outcome = "proposed"
	} else {
		sess.Outcome = "stopped"
	}
	return sess
}
