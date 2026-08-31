package eval

// The DISCOVERY set of the three-set eval flywheel (regression / discovery / sealed holdout), and the
// deterministic PROMOTION step that graduates a discovered misprediction into the regression suite.
//
// Source: core/falsify.Scorer captures every live-scored DEVIATION (a committed prediction reality falsified)
// into a rolling discovery corpus. This file (1) DRAINS those captures into the durable rolling corpus
// (discovery-corpus.json), and (2) PROMOTES qualifying cases into the deterministic falsifiability regression
// suite — the plane whose known-correct expected outcomes are computed by the same deterministic scorers
// (predict.ScoreControl over the real estate graph) the frozen fixture uses, and which is gated by
// TestFalsifiabilityFixture in `make all`.
//
// The eval GATE is a deploy-admission gate and its scoring/thresholds/existing cases are FROZEN. Promotion is
// strictly ADDITIVE and AUDITED: a promoted case ENTERS the suite carrying the KNOWN-CORRECT expected outcome
// the deterministic scorer produces for its real observed cascade over the estate graph; promotion NEVER
// removes, reweights, or relaxes an existing case or threshold, and the SEALED HOLDOUT is NEVER a promotion
// target (a holdout-adjacent case is refused and logged, and this code never writes the holdout file).
// Promotion is CONSERVATIVE: a case promotes only if it reproduces, is de-duplicated against the frozen
// fixture + already-promoted set + the holdout, is not operator-held, and SETTLES to a real cascade
// (RealTP>0) over the estate graph. Every skip and every cap-drop is reported — there are no silent caps.

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"sort"

	"github.com/territory-grounder/grounder/core/estate"
	"github.com/territory-grounder/grounder/core/falsify"
	"github.com/territory-grounder/grounder/core/predict"
	"github.com/territory-grounder/grounder/core/verify"
)

// DiscoveryCase is one durable entry in the rolling discovery corpus: a distinct misprediction signature, the
// observed cascade that falsified the prediction, how many incidents reproduced it, and an optional operator
// HOLD brake. Its Key is falsify.DiscoveryRecord.DeviationKey — the (target, site, surprise-hosts) signature.
type DiscoveryCase struct {
	Key            string       `json:"key"`
	TargetHost     string       `json:"target_host"`
	Site           string       `json:"site"`
	Observed       []ObservedFx `json:"observed"`
	SurpriseHosts  []string     `json:"surprise_hosts,omitempty"`
	Reproductions  int          `json:"reproductions"`
	Hold           bool         `json:"hold,omitempty"` // operator brake: never promote while true
	SourceActionID string       `json:"source_action_id,omitempty"`
	FirstSeen      string       `json:"first_seen,omitempty"`
	LastSeen       string       `json:"last_seen,omitempty"`
}

// DiscoveryCorpus is the durable rolling holding area — the flywheel's discovery set, kept in
// discovery-corpus.json (the same file-per-corpus pattern as corpus.json / holdout-corpus.json).
type DiscoveryCorpus struct {
	Comment string          `json:"_comment,omitempty"`
	Cases   []DiscoveryCase `json:"cases"`
}

// LoadDiscoveryCorpus reads the rolling discovery corpus. A missing file is an EMPTY corpus (the flywheel has
// simply captured nothing yet) — not an error.
func LoadDiscoveryCorpus(path string) (DiscoveryCorpus, error) {
	var c DiscoveryCorpus
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return c, nil
	}
	if err != nil {
		return c, fmt.Errorf("discovery corpus: %w", err)
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("discovery corpus json: %w", err)
	}
	return c, nil
}

// Save writes the rolling discovery corpus deterministically (cases sorted by key).
func (c DiscoveryCorpus) Save(path string) error {
	sort.Slice(c.Cases, func(i, j int) bool { return c.Cases[i].Key < c.Cases[j].Key })
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// IngestCaptured DRAINS a batch of in-memory captures (from falsify.MemDiscoveryCorpus.Snapshot) into the
// durable rolling corpus, de-duplicated by signature: a first sighting appends a case; a reproduction adds to
// the case's reproduction count and advances last-seen. Returns how many NEW cases were appended. Pure over
// its inputs — the caller persists the result with Save. This is the flush hop a worker cron would call; it
// is a pure function so it is fully exercised in CI without a running worker.
func (c *DiscoveryCorpus) IngestCaptured(captured []falsify.CapturedDeviation) int {
	idx := make(map[string]int, len(c.Cases))
	for i, cs := range c.Cases {
		idx[cs.Key] = i
	}
	var added int
	for _, cd := range captured {
		key := cd.Record.DeviationKey()
		if i, ok := idx[key]; ok {
			c.Cases[i].Reproductions += cd.Reproductions
			if ls := cd.LastSeen.UTC().Format("2006-01-02T15:04:05Z"); ls > c.Cases[i].LastSeen {
				c.Cases[i].LastSeen = ls
			}
			continue
		}
		nc := DiscoveryCase{
			Key: key, TargetHost: cd.Record.TargetHost, Site: cd.Record.Site,
			Observed: observedToFx(cd.Record.Observed), SurpriseHosts: cd.Record.SurpriseHosts,
			Reproductions: cd.Reproductions, SourceActionID: cd.Record.ActionID,
			FirstSeen: cd.FirstSeen.UTC().Format("2006-01-02T15:04:05Z"),
			LastSeen:  cd.LastSeen.UTC().Format("2006-01-02T15:04:05Z"),
		}
		idx[key] = len(c.Cases)
		c.Cases = append(c.Cases, nc)
		added++
	}
	return added
}

func observedToFx(obs []verify.ObservedAlert) []ObservedFx {
	out := make([]ObservedFx, 0, len(obs))
	for _, o := range obs {
		out = append(out, ObservedFx{Host: o.Host, Rule: o.Rule, Site: o.Site})
	}
	return out
}

// PromotionCriteria are the conservative, deterministic promotion gates.
type PromotionCriteria struct {
	MinReproductions int // a deviation must reproduce at least this many times (default 2)
	MaxPromotions    int // 0 = unbounded; else cap promotions PER RUN and report the dropped remainder
}

// DefaultPromotionCriteria requires a deviation to have reproduced at least twice before it graduates, and
// leaves the per-run promotion count unbounded (every drop is still reported if a cap is set).
func DefaultPromotionCriteria() PromotionCriteria { return PromotionCriteria{MinReproductions: 2} }

// SkippedCase is one case the promotion step did NOT graduate, with the human-readable reason.
type SkippedCase struct {
	Key    string `json:"key"`
	Reason string `json:"reason"`
}

// PromotionReport is the AUDIT trail of one promotion run — what graduated, what was skipped (and why), what
// was refused for colliding with the sealed holdout, and what a promotion cap dropped. No silent decisions.
type PromotionReport struct {
	Promoted       []string      `json:"promoted"`        // refs graduated into the regression suite
	Skipped        []SkippedCase `json:"skipped"`         // cases not graduated, with reasons
	HoldoutRefused []string      `json:"holdout_refused"` // cases refused for touching the sealed holdout
	Dropped        []string      `json:"dropped"`         // cases dropped by the per-run promotion cap (logged)
}

// DiscoveryRefPrefix distinguishes a promoted, machine-discovered scenario from a hand-authored fixture (fx-*)
// one, so the two sets never collide and the provenance of every regression case is legible.
const DiscoveryRefPrefix = "disc-"

// discoveryRef derives a stable, deterministic ref for a promoted case from its signature.
func discoveryRef(key string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return fmt.Sprintf("%s%08x", DiscoveryRefPrefix, h.Sum32())
}

// PromoteDiscovery is the deterministic promotion step. It graduates qualifying discovery cases into new
// falsifiability regression scenarios, each carrying the KNOWN-CORRECT expected outcome the deterministic
// scorer produces for its real observed cascade over the estate graph. It is PURE (no I/O) and ADDITIVE — it
// only RETURNS new scenarios plus an audit report; it never removes, reweights, or relaxes an existing case,
// and it never touches the sealed holdout.
//
// A case graduates iff ALL hold:
//   - not operator-HELD;
//   - reproduced >= MinReproductions;
//   - de-duplicated: its ref is not already in the frozen fixture or the already-promoted set;
//   - HOLDOUT-SAFE: neither its target nor any observed host is in the sealed holdout (else it is REFUSED);
//   - SETTLES: recomputed over the estate graph it catches a real cascade (RealTP > 0). The recomputed
//     Falsifiable() is recorded as expect_falsifiable — the known-correct expected outcome.
//
// If MaxPromotions > 0 and more cases qualify, the highest-reproduction cases win and the remainder is
// reported in Dropped (never silently discarded).
func PromoteDiscovery(g *estate.Graph, corpus DiscoveryCorpus, frozen, alreadyPromoted []FalsScenario, holdoutHosts map[string]bool, crit PromotionCriteria) ([]FalsScenario, PromotionReport) {
	if crit.MinReproductions <= 0 {
		crit.MinReproductions = DefaultPromotionCriteria().MinReproductions
	}
	existing := map[string]bool{}
	for _, s := range frozen {
		existing[s.Ref] = true
	}
	for _, s := range alreadyPromoted {
		existing[s.Ref] = true
	}

	var report PromotionReport
	type qualified struct {
		scenario      FalsScenario
		reproductions int
	}
	var winners []qualified

	cases := append([]DiscoveryCase(nil), corpus.Cases...)
	sort.Slice(cases, func(i, j int) bool { return cases[i].Key < cases[j].Key }) // deterministic order
	for _, c := range cases {
		ref := discoveryRef(c.Key)
		switch {
		case c.Hold:
			report.Skipped = append(report.Skipped, SkippedCase{ref, "operator hold"})
			continue
		case existing[ref]:
			report.Skipped = append(report.Skipped, SkippedCase{ref, "already in the regression suite (de-duplicated)"})
			continue
		case c.Reproductions < crit.MinReproductions:
			report.Skipped = append(report.Skipped, SkippedCase{ref, fmt.Sprintf("insufficient reproductions (%d < %d)", c.Reproductions, crit.MinReproductions)})
			continue
		case len(c.Observed) == 0:
			report.Skipped = append(report.Skipped, SkippedCase{ref, "no observed cascade to anchor a scenario"})
			continue
		}
		if refused := holdoutHosts[c.TargetHost]; refused || observedTouchesHoldout(c.Observed, holdoutHosts) {
			// The sealed holdout is NEVER auto-fed, and a case that merely TOUCHES a holdout host could leak the
			// holdout into the tunable set — refuse it outright.
			report.HoldoutRefused = append(report.HoldoutRefused, ref)
			continue
		}
		scenario := FalsScenario{Ref: ref, TargetHost: c.TargetHost, Site: c.Site, Observed: c.Observed}
		expectFalsifiable, realTP, ok := settleExpectedOutcome(g, scenario)
		if !ok {
			report.Skipped = append(report.Skipped, SkippedCase{ref, fmt.Sprintf("does not settle over the estate graph (RealTP=%d) — no known-correct expected outcome", realTP)})
			continue
		}
		scenario.ExpectFalsifiable = expectFalsifiable
		winners = append(winners, qualified{scenario, c.Reproductions})
	}

	// Apply the per-run promotion cap (highest-reproduction first, then ref), reporting the dropped remainder.
	sort.SliceStable(winners, func(i, j int) bool {
		if winners[i].reproductions != winners[j].reproductions {
			return winners[i].reproductions > winners[j].reproductions
		}
		return winners[i].scenario.Ref < winners[j].scenario.Ref
	})
	var out []FalsScenario
	for i, w := range winners {
		if crit.MaxPromotions > 0 && i >= crit.MaxPromotions {
			report.Dropped = append(report.Dropped, w.scenario.Ref)
			continue
		}
		out = append(out, w.scenario)
		report.Promoted = append(report.Promoted, w.scenario.Ref)
	}
	sort.Strings(report.Promoted)
	sort.Strings(report.Dropped)
	sort.Strings(report.HoldoutRefused)
	sort.Slice(report.Skipped, func(i, j int) bool { return report.Skipped[i].Key < report.Skipped[j].Key })
	sort.Slice(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	return out, report
}

func observedTouchesHoldout(obs []ObservedFx, holdout map[string]bool) bool {
	for _, o := range obs {
		if holdout[o.Host] {
			return true
		}
	}
	return false
}

// settleExpectedOutcome computes the KNOWN-CORRECT expected outcome for a candidate scenario by running the
// SAME deterministic scorer the frozen fixture uses (fxPredictionAndControl + predict.ScoreControl) over the
// real estate graph. It returns (expect_falsifiable, realTP, settled). A scenario "settles" only when it
// catches a real cascade (RealTP > 0); a degenerate or unresolvable case has no useful expected outcome and
// does not promote.
func settleExpectedOutcome(g *estate.Graph, s FalsScenario) (falsifiable bool, realTP int, settled bool) {
	pred, ctrl := fxPredictionAndControl(g, s)
	observed := make([]verify.ObservedAlert, 0, len(s.Observed))
	for _, o := range s.Observed {
		observed = append(observed, verify.ObservedAlert{Host: o.Host, Rule: o.Rule, Site: o.Site})
	}
	cs := predict.ScoreControl(predict.PredictionRecord{Prediction: pred, ControlHosts: ctrl}, observed)
	if cs.RealTP == 0 {
		return false, 0, false
	}
	return cs.Falsifiable(), cs.RealTP, true
}

// HoldoutHosts loads the sealed holdout corpus and returns the set of hosts it names — the forbidden set
// promotion must never touch. A missing holdout file is a HARD error here (the guard must never fail open).
func HoldoutHosts(path string) (map[string]bool, error) {
	incs, err := LoadCorpus(path)
	if err != nil {
		return nil, err
	}
	hosts := make(map[string]bool, len(incs))
	for _, i := range incs {
		if i.Host != "" {
			hosts[i.Host] = true
		}
	}
	return hosts, nil
}

// LoadPromoted reads the append-only promoted-scenarios file (the same {"scenarios":[...]} shape as the frozen
// fixture, so it is scored by the identical machinery). A missing file is an empty set.
func LoadPromoted(path string) ([]FalsScenario, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("promoted scenarios: %w", err)
	}
	var f struct {
		Scenarios []FalsScenario `json:"scenarios"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("promoted scenarios json: %w", err)
	}
	return f.Scenarios, nil
}

// SavePromoted writes the promoted-scenarios file deterministically (scenarios sorted by ref), with a fixed
// provenance comment recording that these are machine-discovered, deterministically-settled additions.
func SavePromoted(path string, scenarios []FalsScenario) error {
	sort.Slice(scenarios, func(i, j int) bool { return scenarios[i].Ref < scenarios[j].Ref })
	payload := struct {
		Comment   string         `json:"_comment"`
		Scenarios []FalsScenario `json:"scenarios"`
	}{
		Comment:   "AUTO-PROMOTED falsifiability scenarios: live-scored deviations from core/falsify that reproduced, de-duplicated against the frozen fixture + sealed holdout, and SETTLED to a real cascade over the estate graph. Each expect_falsifiable is the deterministic scorer's OWN output for the real observed cascade (the known-correct expected outcome), not a guess. Additive only — promotion never removes, reweights, or relaxes a hand-authored case or the gate threshold, and never feeds the sealed holdout. Regenerate with `go run ./tools/evalgate --discovery`.",
		Scenarios: scenarios,
	}
	b, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// AppendPromoted merges freshly-promoted scenarios into the existing promoted set (de-duplicated by ref,
// first-wins) and returns the merged set — the append-only accumulation the flywheel grows over time.
func AppendPromoted(existing, fresh []FalsScenario) []FalsScenario {
	seen := map[string]bool{}
	out := make([]FalsScenario, 0, len(existing)+len(fresh))
	for _, s := range existing {
		if !seen[s.Ref] {
			seen[s.Ref] = true
			out = append(out, s)
		}
	}
	for _, s := range fresh {
		if !seen[s.Ref] {
			seen[s.Ref] = true
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	return out
}

// LoadEstateGraph builds the estate graph from a captured /v1/estate snapshot (the estate_fixture.json shape).
// It is the production-side twin of the eval test helper, returning an error rather than failing a test, so
// the promotion tool can settle expected outcomes over the real topology without a *testing.T.
func LoadEstateGraph(path string) (*estate.Graph, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("estate snapshot: %w", err)
	}
	var snap struct {
		Nodes []struct {
			Name string `json:"name"`
			Type string `json:"type"`
		} `json:"nodes"`
		Edges []struct {
			From       string  `json:"from"`
			To         string  `json:"to"`
			Rel        string  `json:"rel"`
			Confidence float64 `json:"confidence"`
			Source     string  `json:"source"`
		} `json:"edges"`
	}
	if err := json.Unmarshal(b, &snap); err != nil {
		return nil, fmt.Errorf("estate snapshot json: %w", err)
	}
	types := make(map[string]estate.EntityType, len(snap.Nodes))
	for _, n := range snap.Nodes {
		types[n.Name] = estate.EntityType(n.Type)
	}
	typeOf := func(name string) estate.EntityType {
		if tp, ok := types[name]; ok && tp != "" {
			return tp
		}
		return estate.TypeHost
	}
	g := estate.NewGraph()
	for _, e := range snap.Edges {
		if e.From == "" || e.To == "" {
			continue
		}
		// Bind the snapshot loader to the SAME declared relation vocabulary as the production declared-edge
		// parser (estate.ParseRelType over knownRelTypes). The previous relOf recognised only runs_on and
		// coerced EVERYTHING else — including the declared member_of/routes_via edges — into depends_on, so an
		// ontology boundary violation (an unrecognised rel) was silently mis-typed and the eval gate settled
		// cases over a wrong graph. Fail loud instead, matching ParseDeclared: a boundary violation halts the
		// load rather than presenting a quiet gap as complete truth. (TG-179a, epic TG-175.)
		rel, ok := estate.ParseRelType(e.Rel)
		if !ok {
			return nil, fmt.Errorf("estate snapshot: edge %q->%q has unknown rel %q (declared vocabulary: runs_on, member_of, depends_on, routes_via)", e.From, e.To, e.Rel)
		}
		g.Upsert(estate.Edge{
			From:       estate.Entity{Type: typeOf(e.From), Name: e.From},
			To:         estate.Entity{Type: typeOf(e.To), Name: e.To},
			Rel:        rel,
			Confidence: e.Confidence,
			Source:     estate.Source(e.Source),
		})
	}
	return g, nil
}
