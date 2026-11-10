package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/adapters/model"
	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/instrumentid"
	"github.com/territory-grounder/grounder/core/judge"
	"github.com/territory-grounder/grounder/core/skillstore"
)

// TG-672: the worker's identity source reads each process-level component from the object that owns it — the
// gateway's alias map, the production skill set, the seed knowledge corpus head — caches what it read, and
// leaves what it cannot read EMPTY so the identity row names it.
func TestWorkerInstrumentSourceReadsEachComponentFailSoft(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/model/info" {
			http.NotFound(w, r)
			return
		}
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"model_name":"primary","litellm_params":{"model":"vendor-a/model-x"}},
			{"model_name":"fast","litellm_params":{"model":"vendor-a/model-y"}},
			{"model_name":"` + judge.DefaultParams().Model + `","litellm_params":{"model":"vendor-b/model-z"}},
			{"model_name":"embed-x","litellm_params":{"model":"local/embed"}}]}`))
	}))
	defer srv.Close()
	t.Setenv("TG_TEST_GW_KEY", "k")
	gw := &model.Gateway{BaseURL: srv.URL, APIKeyRef: config.SecretRef("env:TG_TEST_GW_KEY"), HTTP: srv.Client()}
	knowledgeCorpusHead.Store("seed-head-hash")
	t.Cleanup(func() { knowledgeCorpusHead.Store("") })
	var skillReads atomic.Int32
	src := &workerInstrumentSource{gw: gw, tiers: instrumentTiers(),
		skillRows: func(context.Context) ([]skillstore.ProductionRow, error) {
			skillReads.Add(1)
			return []skillstore.ProductionRow{{SkillName: "linux-triage", Version: "2.0.0", ContentHash: "h2"}, {SkillName: "k8s-triage", Version: "1.0.0", ContentHash: "h1"}}, nil
		}}
	c := src.Components(context.Background())
	wantAlias := instrumentid.HashStrings([]string{"primary=vendor-a/model-x", "fast=vendor-a/model-y", judge.DefaultParams().Model + "=vendor-b/model-z"})
	if c.ModelAliasHash != wantAlias {
		t.Fatalf("alias hash = %s, want the three instrument tiers resolved (the embed alias is not one): %s", c.ModelAliasHash, wantAlias)
	}
	if c.SkillSetHash != instrumentid.HashStrings([]string{"k8s-triage@1.0.0:h1", "linux-triage@2.0.0:h2"}) {
		t.Fatalf("skill set hash = %s", c.SkillSetHash)
	}
	if c.KnowledgeHash != "seed-head-hash" {
		t.Fatalf("knowledge=%q", c.KnowledgeHash)
	}
	if c.PolicyBundle != "" || !strings.Contains(strings.Join(c.Unrecorded(), ","), "policy_bundle") {
		t.Fatalf("no ruleset store ⇒ policy bundle must be unrecorded and named, got %q / %v", c.PolicyBundle, c.Unrecorded())
	}
	// Every component read is cached: a second session does not hit the gateway or the skill store again, and
	// the identity it derives is the same one.
	c2 := src.Components(context.Background())
	if hits.Load() != 1 || skillReads.Load() != 1 {
		t.Fatalf("model/info hit %d times, skill rows read %d times — want 1 each (cached for %s)", hits.Load(), skillReads.Load(), componentTTL)
	}
	if c2.ID() != c.ID() {
		t.Fatal("two reads of an unchanged configuration derived different identities")
	}
	// A gateway that cannot be reached leaves the alias map unrecorded, never stale-as-current — and the failure
	// is negative-cached, so a down gateway costs one attempt per failureTTL, not one per session.
	var downHits atomic.Int32
	downSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downHits.Add(1)
		http.Error(w, "gateway unavailable", http.StatusBadGateway)
	}))
	defer downSrv.Close()
	down := &workerInstrumentSource{gw: &model.Gateway{BaseURL: downSrv.URL, APIKeyRef: config.SecretRef("env:TG_TEST_GW_KEY"), HTTP: downSrv.Client()}, tiers: instrumentTiers()}
	for i := 0; i < 3; i++ {
		if c3 := down.Components(context.Background()); c3.ModelAliasHash != "" {
			t.Fatalf("unreachable gateway produced an alias hash %q", c3.ModelAliasHash)
		}
	}
	if downHits.Load() != 1 {
		t.Fatalf("a failing gateway was asked %d times in a row, want 1 (negative-cached for %s)", downHits.Load(), failureTTL)
	}
}

// The identity's components are the CONFIGURED ones. The learner's co-occurrence state and the maintained
// (runtime-written) precedent corpus are learning, not configuration: neither is a Components field, so no
// observation and no precedent writeback can restart a cohort.
func TestLearnedStateIsNotAnIdentityComponent(t *testing.T) {
	for _, f := range instrumentid.Fields {
		if strings.Contains(f, "learn") || strings.Contains(f, "graph") || strings.Contains(f, "maintained") {
			t.Fatalf("instrumentid.Fields names learned state (%s) — learning must not move the identity", f)
		}
	}
}

// TG-698: a once-per-process component (a TTL longer than staleGrace, like the policy bundle's one year) must stay the
// same value for the life of the process. Live 2026-11-08 it went "" one hour after its read and split the G4A
// cohort into two instrument ids. KILLING MUTATION: serve the fresh branch through c.current(now) again — RED here.
func TestAOncePerProcessComponentDoesNotAgeOut(t *testing.T) {
	t0 := time.Date(2026, 11, 8, 9, 16, 27, 0, time.UTC)
	now := t0
	c := &cached{now: func() time.Time { return now }}
	var reads atomic.Int32
	read := func() (string, bool) { reads.Add(1); return "sha256:bundle", true }
	const oncePerProcess = 365 * 24 * time.Hour
	for _, after := range []time.Duration{0, staleGrace - time.Second, staleGrace + time.Second, 2 * staleGrace, 47 * time.Hour} {
		now = t0.Add(after)
		if v := c.get(oncePerProcess, read); v != "sha256:bundle" {
			t.Fatalf("%s after the read the once-per-process value must still be served, got %q", after, v)
		}
	}
	if reads.Load() != 1 {
		t.Fatalf("a once-per-process value is read once, got %d reads", reads.Load())
	}
}

// A cached component: fresh within the TTL; after it, one refresh — a failing refresh serves the LAST GOOD value
// while that is younger than staleGrace and is retried no sooner than failureTTL; past the grace the component is
// unrecorded. Concurrent callers share one in-flight read.
func TestCachedComponentServesLastGoodWithinGraceThenUnrecorded(t *testing.T) {
	t0 := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	now := t0
	c := &cached{now: func() time.Time { return now }}
	var reads atomic.Int32
	healthy := true
	read := func() (string, bool) {
		reads.Add(1)
		if healthy {
			return "v1", true
		}
		return "", false
	}
	if v := c.get(componentTTL, read); v != "v1" || reads.Load() != 1 {
		t.Fatalf("first read: %q reads=%d", v, reads.Load())
	}
	now = t0.Add(componentTTL - time.Second)
	if v := c.get(componentTTL, read); v != "v1" || reads.Load() != 1 {
		t.Fatalf("within TTL must not re-read: %q reads=%d", v, reads.Load())
	}
	healthy = false
	now = t0.Add(componentTTL + time.Second)
	if v := c.get(componentTTL, read); v != "v1" || reads.Load() != 2 {
		t.Fatalf("a failing refresh within the grace must serve the last good value: %q reads=%d", v, reads.Load())
	}
	now = now.Add(failureTTL / 2)
	if v := c.get(componentTTL, read); v != "v1" || reads.Load() != 2 {
		t.Fatalf("a failure is negative-cached for %s: %q reads=%d", failureTTL, v, reads.Load())
	}
	now = t0.Add(staleGrace + time.Minute)
	if v := c.get(componentTTL, read); v != "" || reads.Load() != 3 {
		t.Fatalf("past the grace a still-failing component is unrecorded: %q reads=%d", v, reads.Load())
	}
	healthy = true
	now = now.Add(failureTTL + time.Second)
	if v := c.get(componentTTL, read); v != "v1" || reads.Load() != 4 {
		t.Fatalf("recovery: %q reads=%d", v, reads.Load())
	}
	// Singleflight: ten concurrent callers on an expired value cost one read, and all see its result.
	c2 := &cached{}
	var slowReads atomic.Int32
	gate := make(chan struct{})
	var wg sync.WaitGroup
	results := make([]string, 10)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = c2.get(componentTTL, func() (string, bool) {
				slowReads.Add(1)
				<-gate
				return "shared", true
			})
		}(i)
	}
	time.Sleep(50 * time.Millisecond)
	close(gate)
	wg.Wait()
	if slowReads.Load() != 1 {
		t.Fatalf("concurrent callers made %d reads, want 1", slowReads.Load())
	}
	for i, r := range results {
		if r != "shared" {
			t.Fatalf("caller %d saw %q, want the shared read's value", i, r)
		}
	}
}

// The production path for the knowledge component (REQ-2535): the SEED corpus head is the component; the MAINTAINED
// corpus — runtime-written precedent, i.e. learning — is merged into the retriever but never into the identity.
// A different maintained file leaves the head unchanged; a different seed moves it; no seed leaves it unrecorded.
func TestKnowledgeComponentIsTheSeedHeadNotTheMaintainedCorpus(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	seed := write("seed.json", `[{"external_ref":"seed-1","summary":"disk full on a build host","resolution":"pruned the cache"}]`)
	maintainedA := write("maintained-a.json", `[{"external_ref":"runtime-1","summary":"a learned precedent","resolution":"restarted the unit"}]`)
	maintainedB := write("maintained-b.json", `[{"external_ref":"runtime-1","summary":"a learned precedent","resolution":"restarted the unit"},
		{"external_ref":"runtime-2","summary":"another learned precedent","resolution":"started the guest"}]`)
	logf := func(string, ...any) {}
	t.Cleanup(func() { knowledgeCorpusHead.Store("") })

	if r := loadKnowledgeCorpus(seed, maintainedA, logf); r == nil {
		t.Fatal("seed + maintained A did not load")
	}
	headA, _ := knowledgeCorpusHead.Load().(string)
	if headA == "" {
		t.Fatal("a readable seed must record a knowledge head")
	}
	if r := loadKnowledgeCorpus(seed, maintainedB, logf); r == nil {
		t.Fatal("seed + maintained B did not load")
	}
	if headB, _ := knowledgeCorpusHead.Load().(string); headB != headA {
		t.Fatalf("a precedent writeback moved the knowledge component (%s -> %s) — learning would restart the cohort", headA[:12], headB[:12])
	}
	seed2 := write("seed2.json", `[{"external_ref":"seed-1","summary":"disk full on a build host","resolution":"pruned the cache"},
		{"external_ref":"seed-2","summary":"a second shipped precedent","resolution":"rotated the log"}]`)
	if r := loadKnowledgeCorpus(seed2, maintainedA, logf); r == nil {
		t.Fatal("seed2 did not load")
	}
	if head2, _ := knowledgeCorpusHead.Load().(string); head2 == headA {
		t.Fatal("a changed SEED corpus did not move the knowledge component — a deploy-synced configuration change went unrecorded")
	}
	if r := loadKnowledgeCorpus("", maintainedA, logf); r == nil {
		t.Fatal("maintained-only did not load")
	}
	if head, _ := knowledgeCorpusHead.Load().(string); head != "" {
		t.Fatalf("no seed configured must leave the component unrecorded, got %q", head)
	}
}
