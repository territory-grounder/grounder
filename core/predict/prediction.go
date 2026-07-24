// Package predict is the remediation lane's fail-closed prediction gate: it commits a plan_hash-keyed
// machine consequence prediction — computed entirely OUTSIDE the LLM by the infragraph dependency
// model — to the append-only prediction store BEFORE any approval poll, and is the only constructor of
// a GatedProposal.
//
// Provenance: [O] INV-06/INV-07/INV-10 (default-deny; one action-bound prediction; the gate is a
// structural precondition of an approval poll), spec/002 REQ-101/REQ-102/REQ-105 · [F] the predecessor
// infragraph.py blast-radius/prediction logic + its degree-preserving shuffled-graph negative control,
// re-expressed under the typed spine. This lane fails CLOSED.
package predict

import (
	"context"
	"sort"
	"strings"
	"sync"

	"github.com/territory-grounder/grounder/core/estate"
	"github.com/territory-grounder/grounder/core/schema"
	"github.com/territory-grounder/grounder/core/verify"
)

// DependencyGraph is the infragraph dependency model: host → the hosts that depend on it (its blast
// radius). It is computed from CMDB/estate data, entirely outside the LLM.
type DependencyGraph struct {
	dependents map[string][]string
}

// NewDependencyGraph builds a graph from an adjacency map (host → its direct dependents).
func NewDependencyGraph(adjacency map[string][]string) *DependencyGraph {
	dep := make(map[string][]string, len(adjacency))
	for h, ds := range adjacency {
		cp := append([]string(nil), ds...)
		sort.Strings(cp)
		dep[h] = cp
	}
	return &DependencyGraph{dependents: dep}
}

// BlastRadius returns the transitive dependents of host up to maxDepth, deterministically ordered. It
// is the set of hosts a failure of host could cascade to.
func (g *DependencyGraph) BlastRadius(host string, maxDepth int) []string {
	seen := map[string]struct{}{}
	frontier := []string{host}
	for depth := 0; depth < maxDepth && len(frontier) > 0; depth++ {
		var next []string
		for _, h := range frontier {
			for _, d := range g.dependents[h] {
				if _, ok := seen[d]; ok || d == host {
					continue
				}
				seen[d] = struct{}{}
				next = append(next, d)
			}
		}
		frontier = next
	}
	out := make([]string, 0, len(seen))
	for h := range seen {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

// InfragraphModel computes a machine consequence prediction for an action, outside the LLM.
type InfragraphModel struct {
	// Estate is the multi-source causal graph. When set it is the authoritative blast-radius source
	// (path-product confidence, common-cause siblings, per-edge expected alerts, freshness). The flat Graph
	// below remains for the shuffled negative control and as the fallback when no estate graph is wired.
	Estate *estate.Graph
	// EstateProvider, when set, returns the CURRENT estate graph — an atomically-refreshable snapshot (an
	// estate.Holder) — so a runtime topology refresh takes effect without rebuilding the gate. It OVERRIDES
	// Estate. Nil ⇒ the fixed Estate field is used (the oracle/static path).
	EstateProvider func() *estate.Graph
	Graph          *DependencyGraph
	DefaultRules   []string // the rules a cascading host is expected to fire when an edge names none
	MaxDepth       int
}

// estateGraph returns the graph the model reasons over — the refreshable provider snapshot when wired, else
// the fixed Estate field.
func (m *InfragraphModel) estateGraph() *estate.Graph {
	if m.EstateProvider != nil {
		return m.EstateProvider()
	}
	return m.Estate
}

// Predict computes the real prediction for an action on targetHost/site: the predicted cascade hosts (the
// target's blast radius, plus — only for a common-cause incident — its shared-parent siblings) and the
// (host,rule) pairs expected. Deterministic — no LLM, no randomness. When an estate graph is wired, an
// unresolvable target yields an EMPTY prediction — the target is not in the graph, so the remediation lane
// fails closed on eligibility rather than predicting a vacuous cascade.
//
// commonCause gates the common-cause SIBLING expansion. Siblings model "the shared infrastructure parent
// silently failed, so its co-dependents fail together" — plausible ONLY when the incident is a host
// availability/connectivity fault (the host or its path is down). For a resource- or service-local fault
// (disk, memory, cpu, a service on the host) the parent is fine and the neighbours do not co-fail, so
// predicting them is pure false-positive: a guest disk-fill would otherwise predict its 130 co-tenants, none
// of which alert (control_tp=0, control_fp=0, real_fp≈130). Callers derive this from the alert class via
// SiblingsEligible. The negative control (controlHosts) applies the SAME gate, so the comparison stays fair.
func (m *InfragraphModel) Predict(actionID, planHash, targetHost, site string, commonCause bool) verify.Prediction {
	ph := map[string]struct{}{}
	pr := map[string]struct{}{}
	add := func(host string, alerts []string) {
		ph[host] = struct{}{}
		rules := alerts
		if len(rules) == 0 {
			rules = m.DefaultRules
		}
		for _, rule := range rules {
			pr[verify.RuleKey(host, rule)] = struct{}{}
		}
	}
	if eg := m.estateGraph(); eg != nil {
		if target, ok := eg.Resolve(targetHost); ok {
			for _, imp := range eg.BlastRadius(target, m.maxDepth()) {
				add(imp.Entity.Name, imp.ExpectedAlerts)
			}
			if commonCause {
				for _, imp := range eg.Siblings(target) {
					add(imp.Entity.Name, imp.ExpectedAlerts)
				}
			}
		}
		// unresolvable target ⇒ empty prediction ⇒ fails closed on eligibility upstream.
	} else if m.Graph != nil {
		for _, h := range m.Graph.BlastRadius(targetHost, m.maxDepth()) {
			add(h, nil)
		}
	}
	// A model with NEITHER an estate graph NOR a flat graph yields an EMPTY prediction — fail closed (the
	// upstream eligibility gate polls on an empty prediction), never a nil-graph panic on a misconfigured model.
	return verify.Prediction{
		ActionID:       actionID,
		PlanHash:       planHash,
		TargetHost:     targetHost,
		Site:           site,
		PredictedHosts: ph,
		PredictedRules: pr,
	}
}

func (m *InfragraphModel) maxDepth() int {
	if m.MaxDepth <= 0 {
		return 3
	}
	return m.MaxDepth
}

// siblingCauseKeywords are the compact markers of a whole-HOST AVAILABILITY / CONNECTIVITY failure — the only
// class for which a silently-failing shared parent (hypervisor, switch, PDU) is a plausible common cause, and
// therefore the only class that justifies predicting a target's co-hosted siblings. They are matched against
// the rule slug AFTER stripping spaces/hyphens/underscores, so "Device Down", "device-down" and "device_down"
// all reduce to "devicedown". Crucially, the host token is fused to the failure token ("devicedown", not a
// bare "down"): that is what keeps a SERVICE- or link-scoped alert ("service nginx down", "BGP session down",
// "interface down") — which does NOT implicate co-tenants — from tripping the common-cause path. Everything
// not on this list (disk, memory, cpu, a service, a certificate, latency) is a target-LOCAL fault.
var siblingCauseKeywords = []string{
	"hostdown", "devicedown", "nodedown", "instancedown", "serverdown",
	"unreach", "icmp", "pingloss", "pingfail", "offline", "notrespond", "noresponse", "probefail",
}

// SiblingsEligible reports whether an incident's alert class makes common-cause SIBLING prediction plausible.
// It is TRUE only for whole-host availability/connectivity faults, where the target being down could be a
// symptom of its shared parent silently failing; it is FALSE for resource- or service-local faults and for
// any unrecognized rule (conservative default — an unknown alert predicts no speculative sibling cascade).
// This is the guard that stops a leaf guest's local alert from predicting its whole co-tenant set as false
// positives. Pure and deterministic, so it is safe to call from the deterministic Temporal workflow.
func SiblingsEligible(alertRule string) bool {
	r := strings.ToLower(strings.TrimSpace(alertRule))
	if r == "" {
		return false
	}
	r = strings.NewReplacer(" ", "", "-", "", "_", "", ".", "", "/", "").Replace(r)
	for _, k := range siblingCauseKeywords {
		if strings.Contains(r, k) {
			return true
		}
	}
	return false
}

// controlHosts returns the negative-control host set for a prediction. When an estate graph is wired it is
// the DEGREE-PRESERVING shuffled-graph control (estate.ShuffledControl — every source keeps its out-degree
// and each rel_type keeps its target multiset, but the real who-depends-on-what topology is destroyed), so
// the control has the same graph SHAPE as the real prediction and beating it is a genuine signal (INV-22).
// An unresolvable target yields an empty control, mirroring the empty prediction. With no estate wired it
// falls back to the flat-graph count-only control (legacy/oracle path). The shuffle is seeded from planHash.
func (m *InfragraphModel) controlHosts(planHash, targetHost string, realCount int, commonCause bool) map[string]struct{} {
	if eg := m.estateGraph(); eg != nil {
		ctrl := map[string]struct{}{}
		if target, ok := eg.Resolve(targetHost); ok {
			// includeSiblings mirrors the real prediction's siblings gate so the two sets are the same shape.
			for _, imp := range eg.ShuffledControl(target, m.maxDepth(), planHash, commonCause) {
				ctrl[imp.Entity.Name] = struct{}{}
			}
		}
		return ctrl
	}
	return m.shuffledControlHosts(planHash, targetHost, realCount)
}

// shuffledControlHosts is the flat-graph fallback control: the same NUMBER of hosts as the real prediction,
// chosen deterministically from the whole graph rather than the target's actual dependents. It preserves
// only count (not degree), so it is used ONLY when no estate graph is wired; the estate path above is the
// real degree-preserving control. The offset is seeded from planHash for replay-stable determinism.
func (m *InfragraphModel) shuffledControlHosts(planHash, targetHost string, realCount int) map[string]struct{} {
	if m.Graph == nil {
		return map[string]struct{}{}
	}
	all := make([]string, 0, len(m.Graph.dependents))
	for h := range m.Graph.dependents {
		if h != targetHost {
			all = append(all, h)
		}
	}
	sort.Strings(all)
	ctrl := map[string]struct{}{}
	if len(all) == 0 {
		return ctrl
	}
	// deterministic offset derived from planHash (no Math.rand — replay-stable)
	off := 0
	for i := 0; i < len(planHash); i++ {
		off = (off*31 + int(planHash[i])) % len(all)
	}
	for i := 0; i < realCount && i < len(all); i++ {
		ctrl[all[(off+i)%len(all)]] = struct{}{}
	}
	return ctrl
}

// PredictionRecord is one immutable, append-only infragraph_prediction row: the committed prediction,
// the degree-preserving shuffled control (present on every row so the gate is falsifiable by
// construction), and the schema version. control_tp/control_fp are scored at verify time.
type PredictionRecord struct {
	Prediction     verify.Prediction
	ControlHosts   map[string]struct{}
	ControlTP      int
	ControlFP      int
	SchemaVersion  schema.Version
	PredictionHash string
	// ExternalRef is the session correlation key (ADR-0010) carried onto the prediction row so the durable
	// verified outcome (the falsifiability score on infragraph_prediction) joins back to the session's persisted
	// confidence (session_triage.confidence) in ONE hop — the confidence calibrator's join key (spec/020
	// REQ-2019/REQ-2021). Non-secret. Empty for a record committed without a session ref. It is NOT part of the
	// PredictionHash (which hashes the verify.Prediction), so carrying it does not change action identity.
	ExternalRef string
}

// ControlRatioCeiling is the falsifiability bar: the negative control may capture at most HALF as many true
// cascades as the real prediction. A control_ratio above it means the graph's real topology adds no signal
// over a same-shape random graph — the prediction is not falsifiably valid (INV-22).
const ControlRatioCeiling = 0.5

// ControlScore is the verify-time falsifiability score of a committed prediction against the observed
// post-execution alerts: host-level true/false positives for BOTH the real prediction and its degree-
// preserving control. It is a pure diff — the acting model never scores its own prediction.
type ControlScore struct {
	RealTP    int // predicted hosts that actually alerted (in-scope, non-target)
	RealFP    int // predicted hosts that did NOT alert
	RealFN    int // in-scope hosts that alerted but the prediction never named — missed cascades (the surprise hosts verify.ComputeVerdict reads as a deviation); completes the confusion matrix so recall = tp/(tp+fn)
	ControlTP int // control hosts that actually alerted
	ControlFP int // control hosts that did NOT alert
}

// Ratio is control_tp / real_tp (real_tp floored at 1, so a zero-signal real prediction reads as a full
// failure rather than dividing by zero). Lower is better: 0 means the control caught none of what the real
// prediction caught; a value above ControlRatioCeiling means the control did nearly as well and the gate
// encodes no real causal signal.
func (cs ControlScore) Ratio() float64 {
	denom := cs.RealTP
	if denom < 1 {
		denom = 1
	}
	return float64(cs.ControlTP) / float64(denom)
}

// Falsifiable reports whether the real prediction beat its negative control by the required margin (INV-22).
// A prediction that fails this named the right NUMBER and SHAPE of hosts but not the right ONES, so it is
// not a trustworthy causal claim and must not be leaned on to auto-resolve.
func (cs ControlScore) Falsifiable() bool { return cs.Ratio() <= ControlRatioCeiling }

// ScoreControl scores a committed prediction record against the observed alerts. A host "alerted" when it
// has an in-scope observed alert — applying the SAME exclusions as verify.ComputeVerdict (the action's own
// target host is the expected direct effect; a cross-site alert during a single-site action is background
// noise). It counts, over the alerting hosts, how many the real set and the control set each named (TP), and
// how many named hosts never alerted (FP), and how many alerting hosts the real set MISSED (FN — the
// surprise-host count that makes recall computable). Pure and deterministic; the model never scores itself.
func ScoreControl(rec PredictionRecord, observed []verify.ObservedAlert) ControlScore {
	pred := rec.Prediction
	alerting := map[string]struct{}{}
	for _, a := range observed {
		if a.Host == pred.TargetHost {
			continue // expected direct effect of the action on its own host
		}
		if a.Site != "" && pred.Site != "" && a.Site != pred.Site {
			continue // cross-site background noise
		}
		alerting[a.Host] = struct{}{}
	}
	var cs ControlScore
	for h := range alerting {
		if _, ok := pred.PredictedHosts[h]; ok {
			cs.RealTP++
		} else {
			cs.RealFN++ // an in-scope host alerted that the prediction never named — a missed cascade
		}
		if _, ok := rec.ControlHosts[h]; ok {
			cs.ControlTP++
		}
	}
	for h := range pred.PredictedHosts {
		if _, ok := alerting[h]; !ok {
			cs.RealFP++
		}
	}
	for h := range rec.ControlHosts {
		if _, ok := alerting[h]; !ok {
			cs.ControlFP++
		}
	}
	return cs
}

// PredictionStore is the append-only prediction store (infragraph_prediction). Committing the same
// plan_hash twice is idempotent-append (the first commit wins); there is no update or delete.
type PredictionStore interface {
	Commit(ctx context.Context, rec PredictionRecord) error
	Has(ctx context.Context, planHash string) (bool, error)
	Get(ctx context.Context, planHash string) (PredictionRecord, bool, error)
}

// MemPredictionStore is the in-memory, append-only oracle implementation. It is SHARED across the worker's
// concurrent Temporal gate activities, so its map/slice mutations are guarded — without the lock concurrent
// Commit races the map write and can double-index a plan_hash.
type MemPredictionStore struct {
	mu    sync.Mutex
	rows  []PredictionRecord
	byKey map[string]int
}

// NewMemPredictionStore returns an empty append-only store.
func NewMemPredictionStore() *MemPredictionStore {
	return &MemPredictionStore{byKey: map[string]int{}}
}

// Commit appends a prediction row. A duplicate plan_hash is ignored (append-only, first-wins).
func (s *MemPredictionStore) Commit(_ context.Context, rec PredictionRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.byKey[rec.Prediction.PlanHash]; ok {
		return nil // already committed — append-only, no overwrite
	}
	s.byKey[rec.Prediction.PlanHash] = len(s.rows)
	s.rows = append(s.rows, rec)
	return nil
}

// Has reports whether a prediction is committed for planHash.
func (s *MemPredictionStore) Has(_ context.Context, planHash string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.byKey[planHash]
	return ok, nil
}

// Get returns the committed prediction for planHash.
func (s *MemPredictionStore) Get(_ context.Context, planHash string) (PredictionRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i, ok := s.byKey[planHash]
	if !ok {
		return PredictionRecord{}, false, nil
	}
	return s.rows[i], true, nil
}
