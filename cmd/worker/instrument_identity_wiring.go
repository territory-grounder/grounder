package main

import (
	"context"
	"errors"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/territory-grounder/grounder/adapters/model"
	"github.com/territory-grounder/grounder/core/buildinfo"
	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/instrumentid"
	"github.com/territory-grounder/grounder/core/judge"
	"github.com/territory-grounder/grounder/core/policy"
	"github.com/territory-grounder/grounder/core/skillstore"
	"github.com/territory-grounder/grounder/temporal/runner"
)

// instrumentIdentityStore and instrumentPolicyRulesets are set by newStepStores where the pool is live and read
// by wireInstrumentIdentity once deps exists — the package-level handoff harnessRenderStore uses, because the
// pool's scope and deps's scope do not overlap in main(). Nil on a plane without a pool. knowledgeCorpusHead is
// the head hash of the SEED knowledge corpus, set by loadKnowledgeCorpus on every load (boot and reload). The
// maintained precedent corpus and the caution lane are learned state and are not part of it (TG-672).
var (
	instrumentIdentityStore  *db.InstrumentIdentityStore
	instrumentPolicyRulesets *db.PolicyRulesetStore
	knowledgeCorpusHead      atomic.Value
)

// instrumentTiers are the gateway aliases whose upstream resolution is part of the identity: the model plane's
// two agent tiers and the judge's (owner ruling 2026-08-31; the aliases are the deployed gateway's names).
func instrumentTiers() []string { return []string{"primary", "fast", judge.DefaultParams().Model} }

// Cache lifetimes. The alias map and the skill set are re-read every componentTTL, so a deploy that re-points an
// alias or a promotion that changes the production set is a new identity within that many minutes (a promotion
// also moves the harness fingerprint at once, through the guidance it composes). A FAILED refresh is retried no
// sooner than failureTTL, and while refreshes fail the LAST GOOD value is still served for up to staleGrace after
// it was read — a one-minute gateway blip must not split a cohort into two identities — after which the component
// is unrecorded (named on the row) rather than stale-as-current. The policy bundle is read ONCE per process: a
// ruleset write takes effect only at the next worker restart (TG-637), so the bundle this process evaluates is
// the one it loaded. Reads use their own bounded context, never the first caller's (a cancelled activity must not
// poison the cache for everyone), and concurrent callers share one in-flight read.
const (
	componentTTL = 15 * time.Minute
	failureTTL   = time.Minute
	staleGrace   = time.Hour
	readTimeout  = 10 * time.Second
)

// cached is one lazily-read, TTL-cached component value.
type cached struct {
	mu       sync.Mutex
	value    string
	ok       bool
	at       time.Time
	failedAt time.Time
	inflight chan struct{}
	now      func() time.Time
}

func (c *cached) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// current is what get serves without reading: the last good value while it is within staleGrace, else "".
func (c *cached) current(now time.Time) string {
	if c.ok && now.Sub(c.at) < staleGrace {
		return c.value
	}
	return ""
}

// get returns the cached value when fresh; otherwise ONE caller reads (outside the lock) while the others wait
// for that read, and every caller then serves the outcome: the new value, or on failure the last good value
// within staleGrace, or "".
func (c *cached) get(ttl time.Duration, read func() (string, bool)) string {
	c.mu.Lock()
	now := c.clock()
	// A value inside its TTL is served as is. staleGrace bounds only a value served AFTER a failed refresh (TG-698):
	// the policy bundle is read once per process with a one-year TTL, and routing it through current() blanked it one
	// hour after the read, which changed every process's instrument id with no configuration change at all.
	if c.ok && now.Sub(c.at) < ttl {
		v := c.value
		c.mu.Unlock()
		return v
	}
	if !c.failedAt.IsZero() && now.Sub(c.failedAt) < failureTTL {
		v := c.current(now)
		c.mu.Unlock()
		return v
	}
	if ch := c.inflight; ch != nil {
		c.mu.Unlock()
		<-ch
		c.mu.Lock()
		v := c.current(c.clock())
		c.mu.Unlock()
		return v
	}
	ch := make(chan struct{})
	c.inflight = ch
	c.mu.Unlock()
	v, ok := read()
	c.mu.Lock()
	defer c.mu.Unlock()
	defer close(ch)
	c.inflight = nil
	if ok {
		c.value, c.ok, c.at, c.failedAt = v, true, c.clock(), time.Time{}
		return v
	}
	c.failedAt = c.clock()
	return c.current(c.failedAt)
}

// workerInstrumentSource reads the process-level components of the instrument identity (instrumentid.Source).
// Every read is fail-soft: a component that cannot be read is "" and the identity row names it.
type workerInstrumentSource struct {
	gw        *model.Gateway
	tiers     []string
	skillRows func(context.Context) ([]skillstore.ProductionRow, error)
	rulesets  *db.PolicyRulesetStore

	aliases cached
	skills  cached
	policy  cached
}

func (s *workerInstrumentSource) Components(_ context.Context) instrumentid.Components {
	var c instrumentid.Components
	if s.rulesets != nil {
		// Once per process (see the TTL note): a very long TTL stands in for "until restart".
		c.PolicyBundle = s.policy.get(365*24*time.Hour, func() (string, bool) {
			rctx, cancel := context.WithTimeout(context.Background(), readTimeout)
			defer cancel()
			rs, _, err := s.rulesets.Load(rctx)
			if err != nil {
				if errors.Is(err, policy.ErrRulesetAbsent) {
					log.Printf("instrument identity: no active policy ruleset on this plane — policy bundle recorded as unrecorded")
				} else {
					log.Printf("instrument identity: policy bundle unreadable (%v) — recorded as unrecorded, retried after %s", err, failureTTL)
				}
				return "", false
			}
			return policy.BundleVersion(rs), true
		})
	}
	if s.gw != nil {
		c.ModelAliasHash = s.aliases.get(componentTTL, func() (string, bool) {
			rctx, cancel := context.WithTimeout(context.Background(), readTimeout)
			defer cancel()
			m, err := s.gw.ResolveTiers(rctx, s.tiers...)
			if err != nil {
				log.Printf("instrument identity: model alias map unreadable (%v) — last good value served for up to %s, then unrecorded; retried after %s", err, staleGrace, failureTTL)
				return "", false
			}
			parts := make([]string, 0, len(m))
			for alias, upstream := range m {
				parts = append(parts, alias+"="+upstream)
			}
			return instrumentid.HashStrings(parts), true
		})
	}
	if s.skillRows != nil {
		c.SkillSetHash = s.skills.get(componentTTL, func() (string, bool) {
			rctx, cancel := context.WithTimeout(context.Background(), readTimeout)
			defer cancel()
			rows, err := s.skillRows(rctx)
			if err != nil {
				log.Printf("instrument identity: production skill set unreadable (%v) — last good value served for up to %s, then unrecorded; retried after %s", err, staleGrace, failureTTL)
				return "", false
			}
			parts := make([]string, 0, len(rows))
			for _, r := range rows {
				parts = append(parts, r.SkillName+"@"+r.Version+":"+r.ContentHash)
			}
			return instrumentid.HashStrings(parts), true
		})
	}
	if v, ok := knowledgeCorpusHead.Load().(string); ok {
		c.KnowledgeHash = v
	}
	return c
}

// wireInstrumentIdentity attaches the identity source and its store to the activity deps (TG-672). Observability
// only; nothing on the decision path reads either.
func wireInstrumentIdentity(deps *runner.Deps, gw *model.Gateway, skillRows func(context.Context) ([]skillstore.ProductionRow, error)) {
	deps.Instrument = &workerInstrumentSource{gw: gw, tiers: instrumentTiers(), skillRows: skillRows, rulesets: instrumentPolicyRulesets}
	if instrumentIdentityStore == nil {
		log.Printf("instrument identity: components stamped per session; identity rows NOT recorded — no database pool on this plane (build %s)", buildinfo.Describe())
		return
	}
	deps.InstrumentIdentities = instrumentIdentityStore
	log.Printf("instrument identity: ON (TG-672) — build %s; each configuration's components are recorded on its first session (instrument_identity); the session row carries instrument_id", buildinfo.Describe())
}
