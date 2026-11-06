// Package worlddiscovery is the re-discovery and drift pass for the auto-drafted world model (spec/027
// REQ-2705, epic TG-227 plane 2).
//
// Once per interval it re-runs the discovery EdgeSources, diffs the fresh snapshot against what the
// manifest holds, and does exactly two things: drafts what is newly present, and marks STALE what has
// disappeared. It cannot do a third thing.
//
// THE SAFE DIRECTION IS THE WHOLE DESIGN. Absence of evidence is not evidence of absence: a source that
// blinks, a host briefly unreachable, an ssh transport that times out — each of these makes units
// "disappear" without anything having changed on the estate. So a disappearance NEVER retires an operator's
// grant. It marks the row stale, the row KEEPS materializing into the allowlist (worldmodel's
// ApprovedEntries deliberately includes stale), and the console surfaces it for a human to decide. Only an
// operator retires. The failure mode this forecloses is the one that would be invisible: TG silently
// narrowing its own permissions because a discovery source had a bad afternoon, and nobody noticing until
// an incident needed the grant that quietly evaporated.
//
// A FAILING SOURCE CONTRIBUTES NOTHING, LOUDLY. estate.Build isolates per source; this pass keeps that
// property and adds the one that matters here: a source that errored is EXCLUDED from the disappearance
// diff entirely. Diffing against a snapshot a broken source did not contribute to would mark every one of
// its entries stale — turning one transport error into estate-wide drift noise, which is exactly how a
// human learns to ignore the drift feed.
//
// Shape: the finalizer/opclasscluster precedent — a Job with injected dependencies, one pure Run pass,
// RunPeriodically that runs immediately then on the interval, and per-item errors that never abort the pass.
package worlddiscovery

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/territory-grounder/grounder/core/estate"
	"github.com/territory-grounder/grounder/core/worldmodel"
)

// ErrNoSources refuses a pass with nothing to discover from. A cron that "succeeded" over zero sources
// would report a clean run while observing nothing — and, worse, would see the entire estate as
// disappeared. Refusing loudly is the only honest answer.
var ErrNoSources = errors.New("worlddiscovery: no discovery sources wired")

// DriftStore is the persistence this pass drives. It is deliberately NARROW: the pass can draft rows and
// read what is granted, and it reaches status changes only through worldmodel.Transition (which this
// interface also satisfies via UpdateEntry). There is no retire path and no delete path anywhere in it.
type DriftStore interface {
	// DraftEntry records a discovered fact as a DRAFT. It cannot create an approved row — the status is
	// not a parameter of the underlying writer.
	DraftEntry(ctx context.Context, e worldmodel.Entry) error
	// ApprovedEntries returns what currently materializes (approved AND stale) — the set the
	// disappearance diff is computed against.
	ApprovedEntries(ctx context.Context) ([]worldmodel.Entry, error)
	// UpdateEntry is what worldmodel.Transition calls; the pass never calls it directly.
	UpdateEntry(ctx context.Context, e worldmodel.Entry) error
}

// Job is one re-discovery pass.
type Job struct {
	// Sources are the discovery EdgeSources (systemd, docker, and any other estate contributor).
	Sources []estate.EdgeSource
	Store   DriftStore
	Ledger  worldmodel.Ledger
	// Now is injectable for the oracles; nil ⇒ time.Now().UTC().
	Now func() time.Time
	// OnPass is called after every SUCCESSFUL pass with that pass's Result. Optional.
	//
	// It exists because the log line below only speaks when something CHANGED
	// (`res.Drafted > 0 || res.MarkedStale > 0 || res.SourceErrors > 0`), so a pass that observed five
	// hundred entities and drafted NOTHING says nothing at all — which is precisely the starvation this
	// lane is most likely to suffer and the one an operator most needs to hear about. The hook hands the
	// pair (Observed, Drafted) to the seam-yield register, where zero-with-input is an alarm.
	OnPass func(Result)
}

// Result is what one pass did — counts only, so the log line is honest and greppable.
type Result struct {
	Observed     int // distinct entities the healthy sources saw
	Drafted      int // new or re-seen entities recorded as drafts
	MarkedStale  int // approved entries no source could see this pass
	SourceErrors int // sources that failed to build (their entries are NOT diffed)
	ItemErrors   int
}

func (j Job) now() time.Time {
	if j.Now != nil {
		return j.Now().UTC()
	}
	return time.Now().UTC()
}

// observed is the fresh snapshot: the entity identities the healthy sources saw, and the set of sources
// that failed. Both are needed — the failures decide which entries are EXCLUDED from the stale diff.
type observed struct {
	entities map[string]worldmodel.Entry // key: entityKey(type,name)
	failed   map[estate.Source]bool
}

func entityKey(t estate.EntityType, name string) string {
	return string(t) + "\x00" + strings.TrimSpace(name)
}

// discover runs every source with per-source isolation and folds the resulting edges into the entity set.
//
// Both ENDS of an edge are observed facts: a `service runs_on host` edge is evidence the service exists AND
// evidence the host exists. Taking only the From side would leave hosts permanently undiscovered.
func (j Job) discover(ctx context.Context) (observed, []estate.SourceError) {
	obs := observed{entities: map[string]worldmodel.Entry{}, failed: map[estate.Source]bool{}}
	var errs []estate.SourceError
	for _, s := range j.Sources {
		if s == nil {
			continue
		}
		edges, err := s.Edges(ctx)
		if err != nil {
			// LOUD and isolated: this source contributes nothing to the snapshot AND is excluded from
			// the disappearance diff below, so its transport error cannot masquerade as estate drift.
			errs = append(errs, estate.SourceError{Source: s.Source(), Err: err})
			obs.failed[s.Source()] = true
			// A source may return partial edges alongside its error (the systemd source does exactly
			// this when some hosts answered). Those are real observations — keep them; the exclusion
			// above already prevents the missing ones from reading as disappearances.
		}
		for _, e := range edges {
			conf, _ := worldmodel.SourceConfidence(e.Source)
			if e.Confidence > 0 {
				conf = e.Confidence
			}
			for _, side := range []struct {
				ent  estate.Entity
				host string
			}{
				{e.From, e.To.Name},
				{e.To, ""},
			} {
				if strings.TrimSpace(side.ent.Name) == "" || !worldmodel.KnownEntityType(side.ent.Type) {
					// An unknown type from a corrupted source is DROPPED here rather than drafted:
					// seeding a phantom target that later reads as operator-adopted truth is the
					// failure this closed vocabulary exists to prevent (REQ-2701).
					continue
				}
				k := entityKey(side.ent.Type, side.ent.Name)
				prev, seen := obs.entities[k]
				if seen && prev.Confidence >= conf {
					continue
				}
				obs.entities[k] = worldmodel.Entry{
					EntityType: side.ent.Type,
					Name:       strings.TrimSpace(side.ent.Name),
					Host:       side.host,
					Source:     e.Source,
					// MAX-ratchet across sources within the pass (REQ-2706): the strongest
					// observer of an entity is the one whose confidence the draft carries.
					Confidence: worldmodel.RatchetConfidence(prev.Confidence, conf),
				}
			}
		}
	}
	return obs, errs
}

// Run performs one pass: discover, draft what is present, mark stale what is gone.
func (j Job) Run(ctx context.Context) (Result, error) {
	var res Result
	if len(j.Sources) == 0 {
		return res, ErrNoSources
	}
	obs, srcErrs := j.discover(ctx)
	res.SourceErrors = len(srcErrs)
	res.Observed = len(obs.entities)
	for _, e := range srcErrs {
		log.Printf("worlddiscovery: source %s failed: %v (its entries are excluded from the stale diff)", e.Source, e.Err)
	}

	// Draft everything observed. DraftEntry is an upsert that refreshes last_seen_at and ratchets
	// confidence but NEVER resets status — re-seeing an adopted unit must not un-adopt it, and re-seeing
	// a rejected one must not resurrect it.
	for _, e := range obs.entities {
		if err := j.Store.DraftEntry(ctx, e); err != nil {
			res.ItemErrors++
			log.Printf("worlddiscovery: draft %s/%s: %v (pass continues)", e.EntityType, e.Name, err)
			continue
		}
		res.Drafted++
	}

	granted, err := j.Store.ApprovedEntries(ctx)
	if err != nil {
		return res, fmt.Errorf("approved entries: %w", err)
	}
	now := j.now()
	for _, g := range granted {
		// Only entries whose OWN source was healthy this pass are eligible to be called disappeared.
		if obs.failed[g.Source] {
			continue
		}
		if _, stillThere := obs.entities[entityKey(g.EntityType, g.Name)]; stillThere {
			continue
		}
		// Already stale: leave it. Re-marking would append a ledger row per pass for a fact that has
		// not changed, and a drift feed that repeats itself is a drift feed nobody reads.
		if g.Status == worldmodel.StatusStale {
			continue
		}
		if _, err := worldmodel.Transition(ctx, j.Store, j.Ledger, g, worldmodel.StatusStale,
			"discovery", fmt.Sprintf("no source observed %s/%s on the %s pass — still granted, awaiting an operator decision",
				g.EntityType, g.Name, now.Format(time.RFC3339))); err != nil {
			res.ItemErrors++
			log.Printf("worlddiscovery: stale %s/%s: %v (pass continues)", g.EntityType, g.Name, err)
			continue
		}
		res.MarkedStale++
	}
	return res, nil
}

// RunPeriodically performs one pass IMMEDIATELY and then one every `every` until ctx is done. It blocks;
// callers run it in a goroutine.
//
// The immediate pass is deliberate (the opclasscluster/calibrate precedent): a bare `for range t.C` leaves
// the manifest empty for a full interval after every worker start, and an empty review queue is
// indistinguishable from "the estate has nothing to adopt" on a surface whose entire job is to show an
// operator what is waiting for them.
func RunPeriodically(ctx context.Context, j Job, every time.Duration, onErr func(error)) {
	pass := func() {
		res, err := j.Run(ctx)
		if err != nil {
			if onErr != nil {
				onErr(err)
			}
			return
		}
		if j.OnPass != nil {
			j.OnPass(res) // every pass, including the silent ones — see the OnPass doc
		}
		if res.Drafted > 0 || res.MarkedStale > 0 || res.SourceErrors > 0 {
			log.Printf("worlddiscovery: observed %d, drafted %d, marked stale %d, %d source error(s), %d item error(s)",
				res.Observed, res.Drafted, res.MarkedStale, res.SourceErrors, res.ItemErrors)
		}
	}
	pass()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pass()
		}
	}
}
