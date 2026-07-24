package acceptance

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cucumber/godog"

	"github.com/territory-grounder/grounder/core/estate"
	"github.com/territory-grounder/grounder/core/falsify"
	"github.com/territory-grounder/grounder/core/knowledge"
	"github.com/territory-grounder/grounder/core/learn"
	"github.com/territory-grounder/grounder/core/lessons"
	"github.com/territory-grounder/grounder/core/verify"
	"github.com/territory-grounder/grounder/eval"
)

// world is the per-scenario state driving the real recency/decay code (spec/018).
type world struct {
	// lessons (REQ-1800/1701)
	now       time.Time
	halfLife  time.Duration
	horizon   time.Duration
	resolved  []lessons.ResolvedIncident
	recResult lessons.ReconcileResult
	corpus    []knowledge.Incident

	// learn (REQ-1802)
	learner  *learn.CoOccurrenceLearner
	learnNow time.Time
	learnHL  time.Duration

	// estate (REQ-1803/1704)
	graph   *estate.Graph
	decayed *estate.Graph
	rep     estate.DecayReport
	edgeNow time.Time

	// discovery flush (REQ-1805)
	disc     *falsify.MemDiscoveryCorpus
	discRec  falsify.DiscoveryRecord
	discPath string
	added    int
}

func edgeConf(g *estate.Graph, from, to string) float64 {
	for _, e := range g.Export().Edges {
		if e.FromName == from && e.ToName == to {
			return e.Confidence
		}
	}
	return -1
}

func dependsInBlast(g *estate.Graph, target, dependent string) bool {
	for _, imp := range g.BlastRadius(estate.Entity{Type: estate.TypeHost, Name: target}, 3) {
		if imp.Entity.Name == dependent {
			return true
		}
	}
	return false
}

func dbAppCount(l *learn.CoOccurrenceLearner) int {
	for _, c := range l.CoOccurrences() {
		if c.Primary == "db01" && c.Dependent == "app01" {
			return c.Count
		}
	}
	return 0
}

// TestRecencyDecayAcceptance runs the spec/018 acceptance feature. No scenario is @pending — every step
// drives the real core/lessons, core/learn, core/estate, core/falsify and eval code.
func TestRecencyDecayAcceptance(t *testing.T) {
	suite := godog.TestSuite{
		Name:                "spec/018 recency-decay",
		ScenarioInitializer: initializeScenario,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"."},
			Tags:     "~@pending",
			Strict:   true,
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("spec/018 acceptance scenarios failed")
	}
}

func initializeScenario(sc *godog.ScenarioContext) {
	w := &world{}

	// ---- REQ-1800/1701 — lessons provenance + decay ----
	sc.Step(`^a resolved-incident feed whose lessons carry a resolved_at provenance timestamp$`, func() error {
		w.now = time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
		w.halfLife = 30 * 24 * time.Hour
		w.horizon = 90 * 24 * time.Hour
		feed := `[
		  {"external_ref":"OLD-1","action":"a","confirmed_clear":true,"verdict":"match","resolved_at":"2026-01-01T00:00:00Z"},
		  {"external_ref":"NEW-1","action":"b","confirmed_clear":true,"verdict":"match","resolved_at":"2026-07-10T00:00:00Z"},
		  {"external_ref":"HALF-1","action":"c","confirmed_clear":true,"verdict":"match","resolved_at":"2026-06-20T00:00:00Z"},
		  {"external_ref":"UNDATED","action":"d","confirmed_clear":true,"verdict":"match"}
		]`
		r, err := lessons.ParseResolved(strings.NewReader(feed))
		if err != nil {
			return fmt.Errorf("feed with provenance must parse: %w", err)
		}
		w.resolved = r
		return nil
	})
	sc.Step(`^the lessons are reconciled against a retention horizon$`, func() error {
		w.recResult = lessons.Reconcile(w.resolved, w.now, w.horizon)
		w.corpus = []knowledge.Incident{{ExternalRef: "OLD-1"}, {ExternalRef: "NEW-1"}, {ExternalRef: "HALF-1"}, {ExternalRef: "UNDATED"}}
		return nil
	})
	sc.Step(`^the provenance timestamp round-trips and a lesson one half-life old is down-weighted$`, func() error {
		var half lessons.ResolvedIncident
		for _, ri := range w.resolved {
			if ri.ExternalRef == "HALF-1" {
				half = ri
			}
		}
		if half.ResolvedAt.IsZero() {
			return fmt.Errorf("provenance timestamp did not round-trip through ParseResolved")
		}
		oneHL := lessons.HalfLifeWeight(half.ResolvedAt, w.now, w.halfLife)
		fresh := lessons.HalfLifeWeight(w.now, w.now, w.halfLife)
		if oneHL < 0.49 || oneHL > 0.51 {
			return fmt.Errorf("a lesson one half-life old must weigh ~0.5, got %.4f", oneHL)
		}
		if !(oneHL < fresh) {
			return fmt.Errorf("an aged lesson must be down-weighted vs a fresh one: oneHL=%.4f fresh=%.4f", oneHL, fresh)
		}
		return nil
	})
	sc.Step(`^a lesson older than the horizon is pruned from the corpus while a fresh one is kept$`, func() error {
		if len(w.recResult.StaleRefs) != 1 || w.recResult.StaleRefs[0] != "OLD-1" {
			return fmt.Errorf("only OLD-1 must exceed the horizon, got %v", w.recResult.StaleRefs)
		}
		kept, removed := lessons.PruneStaleFromCorpus(w.corpus, w.recResult.StaleRefs)
		if removed != 1 {
			return fmt.Errorf("exactly one stale precedent must be pruned, got %d", removed)
		}
		var sawOld, sawNew bool
		for _, inc := range kept {
			if inc.ExternalRef == "OLD-1" {
				sawOld = true
			}
			if inc.ExternalRef == "NEW-1" {
				sawNew = true
			}
		}
		if sawOld {
			return fmt.Errorf("the stale precedent OLD-1 must be gone from the corpus")
		}
		if !sawNew {
			return fmt.Errorf("the fresh precedent NEW-1 must be kept in the corpus")
		}
		return nil
	})

	// ---- REQ-1802 — learn half-life ----
	sc.Step(`^a co-occurrence pair observed enough times to promote to a learned edge$`, func() error {
		base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
		w.learner = learn.NewCoOccurrenceLearner(10 * time.Minute)
		for day := 0; day < 8; day++ {
			d := base.AddDate(0, 0, day)
			w.learner.Observe(learn.AlertObservation{Host: "db01", At: d})
			w.learner.Observe(learn.AlertObservation{Host: "app01", At: d.Add(30 * time.Second)})
		}
		w.learnNow = base.AddDate(0, 0, 8)
		w.learnHL = 30 * 24 * time.Hour
		if got := dbAppCount(w.learner); got != 8 {
			return fmt.Errorf("the pair must start at count 8, got %d", got)
		}
		if edges, _ := w.learner.LearnedSource().Edges(nil); len(edges) != 1 {
			return fmt.Errorf("the well-observed pair must promote to one learned edge, got %d", len(edges))
		}
		return nil
	})
	sc.Step(`^the half-life decay is applied for one half-life with no fresh evidence$`, func() error {
		w.learner.Decay(w.learnNow, w.learnHL)              // baseline checkpoint
		w.learner.Decay(w.learnNow.Add(w.learnHL), w.learnHL) // one half-life
		return nil
	})
	sc.Step(`^the count halves and the learned edge still survives$`, func() error {
		if got := dbAppCount(w.learner); got != 4 {
			return fmt.Errorf("after one half-life the count must halve 8 → 4, got %d", got)
		}
		if edges, _ := w.learner.LearnedSource().Edges(nil); len(edges) != 1 {
			return fmt.Errorf("count 4 must still promote to one learned edge, got %d", len(edges))
		}
		return nil
	})
	sc.Step(`^after several half-lives with no reinforcement the pair ages out of the tier$`, func() error {
		w.learner.Decay(w.learnNow.Add(3*w.learnHL), w.learnHL) // → ~1
		w.learner.Decay(w.learnNow.Add(5*w.learnHL), w.learnHL) // → below the floor → dropped
		if got := dbAppCount(w.learner); got != 0 {
			return fmt.Errorf("a long-unreinforced pair must age out entirely, got %d", got)
		}
		if edges, _ := w.learner.LearnedSource().Edges(nil); len(edges) != 0 {
			return fmt.Errorf("an aged-out pair must promote no learned edge, got %d", len(edges))
		}
		return nil
	})

	// ---- REQ-1803/1704 — estate decay-on-disproof ----
	buildGraph := func() *estate.Graph {
		w.edgeNow = time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
		g := estate.NewGraph(estate.WithClock(func() time.Time { return w.edgeNow }))
		g.Upsert(estate.Edge{From: estate.Entity{Type: estate.TypeHost, Name: "app01"}, To: estate.Entity{Type: estate.TypeHost, Name: "db01"}, Rel: estate.RelDependsOn, Confidence: 0.75, Source: estate.SourceIncident})
		g.Upsert(estate.Edge{From: estate.Entity{Type: estate.TypeVM, Name: "web01"}, To: estate.Entity{Type: estate.TypePVENode, Name: "pve1"}, Rel: estate.RelRunsOn, Confidence: 0.95, Source: estate.SourcePVE})
		return g
	}
	sc.Step(`^an estate graph with a learned edge and a ground-truth edge$`, func() error {
		w.graph = buildGraph()
		return nil
	})
	sc.Step(`^a fresh verify disproof names the learned edge's host$`, func() error {
		w.decayed, w.rep = w.graph.DecayOnDisproof(estate.Disproof{Hosts: []string{"app01"}, At: w.edgeNow}, estate.DecayOptions{Factor: 0.5})
		return nil
	})
	sc.Step(`^the learned edge confidence decays and the ground-truth edge is untouched$`, func() error {
		if w.rep.Decayed != 1 {
			return fmt.Errorf("exactly the learned edge must decay, got %+v", w.rep)
		}
		if c := edgeConf(w.decayed, "app01", "db01"); c < 0.374 || c > 0.376 {
			return fmt.Errorf("the learned edge must halve 0.75 → 0.375, got %.4f", c)
		}
		if c := edgeConf(w.decayed, "web01", "pve1"); c != 0.95 {
			return fmt.Errorf("the ground-truth edge must be untouched at 0.95, got %.4f", c)
		}
		if c := edgeConf(w.graph, "app01", "db01"); c != 0.75 {
			return fmt.Errorf("the receiver graph must be unmutated at 0.75, got %.4f", c)
		}
		return nil
	})
	sc.Step(`^repeating the disproof at a floor ages the learned edge out of the blast radius$`, func() error {
		agedG, rep := w.graph.DecayOnDisproof(estate.Disproof{Hosts: []string{"app01"}, At: w.edgeNow}, estate.DecayOptions{Factor: 0.5, Floor: 0.4})
		if rep.AgedOut != 1 {
			return fmt.Errorf("a disproof at floor 0.4 must age out the learned edge, got %+v", rep)
		}
		if dependsInBlast(agedG, "db01", "app01") {
			return fmt.Errorf("an aged-out learned edge must be excluded from the blast radius")
		}
		return nil
	})
	sc.Step(`^a disproof names every host in the graph$`, func() error {
		w.decayed, w.rep = w.graph.DecayOnDisproof(estate.Disproof{Hosts: []string{"app01", "db01", "web01", "pve1"}, At: w.edgeNow}, estate.DecayOptions{Factor: 0.5})
		return nil
	})
	sc.Step(`^only the learned tier decays and the ground-truth edge keeps its confidence$`, func() error {
		if w.rep.Decayed != 1 {
			return fmt.Errorf("only the one learned edge may decay even when every host is named, got %+v", w.rep)
		}
		if c := edgeConf(w.decayed, "web01", "pve1"); c != 0.95 {
			return fmt.Errorf("the ground-truth edge must keep 0.95, got %.4f", c)
		}
		if c := edgeConf(w.decayed, "app01", "db01"); c < 0.374 || c > 0.376 {
			return fmt.Errorf("the learned edge must decay to 0.375, got %.4f", c)
		}
		return nil
	})
	sc.Step(`^the receiver graph is never mutated in place$`, func() error {
		if c := edgeConf(w.graph, "app01", "db01"); c != 0.75 {
			return fmt.Errorf("the learned edge on the receiver must still be 0.75, got %.4f", c)
		}
		if c := edgeConf(w.graph, "web01", "pve1"); c != 0.95 {
			return fmt.Errorf("the ground-truth edge on the receiver must still be 0.95, got %.4f", c)
		}
		return nil
	})

	// ---- REQ-1805 — discovery-corpus flush ----
	sc.Step(`^an in-memory discovery corpus that captured a scored deviation$`, func() error {
		w.disc = falsify.NewMemDiscoveryCorpus(0)
		obsAt := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
		w.discRec = falsify.DiscoveryRecord{
			ActionID: "act-1", TargetHost: "web01", Site: "nl",
			SurpriseHosts: []string{"db01"},
			Observed:      []verify.ObservedAlert{{Host: "db01", Rule: "HostDown", Site: "nl"}},
			ObservedAt:    obsAt, CommittedAt: obsAt.Add(-10 * time.Minute),
		}
		if newly, err := w.disc.Capture(context.Background(), w.discRec); err != nil || !newly {
			return fmt.Errorf("the deviation must be newly captured, newly=%v err=%v", newly, err)
		}
		w.discPath = filepath.Join(godogTempDir(sc), "discovery-corpus.json")
		return nil
	})
	sc.Step(`^the flush drains the snapshot into the durable corpus via IngestCaptured and saves it$`, func() error {
		snap := w.disc.Snapshot()
		corpus, err := eval.LoadDiscoveryCorpus(w.discPath) // missing file ⇒ empty corpus
		if err != nil {
			return fmt.Errorf("load durable corpus: %w", err)
		}
		w.added = corpus.IngestCaptured(snap)
		if err := corpus.Save(w.discPath); err != nil {
			return fmt.Errorf("save durable corpus: %w", err)
		}
		return nil
	})
	sc.Step(`^reloading the durable corpus file yields the captured deviation case$`, func() error {
		if w.added != 1 {
			return fmt.Errorf("exactly one new case must be drained, got %d", w.added)
		}
		loaded, err := eval.LoadDiscoveryCorpus(w.discPath)
		if err != nil {
			return fmt.Errorf("reload durable corpus: %w", err)
		}
		key := w.discRec.DeviationKey()
		for _, cs := range loaded.Cases {
			if cs.Key == key && cs.TargetHost == "web01" {
				return nil
			}
		}
		return fmt.Errorf("the drained corpus must contain the captured deviation case (key %q), got %d case(s)", key, len(loaded.Cases))
	})
}

// godogTempDir returns a per-scenario temp directory registered for cleanup after the scenario.
func godogTempDir(sc *godog.ScenarioContext) string {
	dir, err := os.MkdirTemp("", "spec018-discovery")
	if err != nil {
		panic(err)
	}
	sc.After(func(ctx context.Context, _ *godog.Scenario, _ error) (context.Context, error) {
		_ = os.RemoveAll(dir)
		return ctx, nil
	})
	return dir
}
