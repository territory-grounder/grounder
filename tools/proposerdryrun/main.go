// Command proposerdryrun runs the flywheel's evidence-grounded proposers (INV + DOM) and the composer ONCE, by hand,
// against the LIVE judged pool — the rehearsal an operator takes before trusting the first organic trigger
// (TG-582 proposers, TG-583 slice C composer). The real code paths run: the trigger's own pack source
// (core/db.EvidenceSource.PacksFor over the production version's pool), GenerateCandidatesFromEvidence, then
// ComposeCandidates with the real replay prober. Only the WRITES are intercepted: every CreateVersion/UpdateVersion
// lands in an in-memory shadow (negative ids) and the governance ledger is a list, so the database is read and never
// written — the tool proves that by counting the skill's rows before and after.
//
//	LITELLM_MASTER_KEY=... proposerdryrun -gateway http://127.0.0.1:4000 -dsn postgres://... -skill triage-protocol -dimension correct_diagnosis [-window 336h] [-packs 6] [-probe-k 1] [-out DIR]
//
// The database connection is opened with default_transaction_read_only=on (readOnlyDSN), so the server itself
// refuses any write through this tool whatever role the DSN carries; the shadow store is the design, the server
// setting is the guarantee. PROPOSERDRYRUN_DSN may carry the DSN instead of the flag (it then stays out of `ps`).
//
// Cost: two proposer calls (+ one retry each at most) and one composer call on the primary tier, plus the probe
// (k replicates × discovery incidents × two bodies through the real loop, judged pairwise on the judge tier) when
// -probe-k is 1 or more. Exit 1 on any failure, 2 on usage.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/territory-grounder/grounder/adapters/model"
	"github.com/territory-grounder/grounder/agent"
	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/estate"
	"github.com/territory-grounder/grounder/core/skillstore"
	"github.com/territory-grounder/grounder/temporal/runner"
	"github.com/territory-grounder/grounder/temporal/skillgen"
)

func main() {
	gwURL := flag.String("gateway", os.Getenv("TG_EVAL_GATEWAY"), "model gateway base url (LITELLM_MASTER_KEY in the environment)")
	dsn := flag.String("dsn", os.Getenv("PROPOSERDRYRUN_DSN"), "skill_version / session_judgment / agent_step DSN (or PROPOSERDRYRUN_DSN); the connection is opened READ-ONLY (default_transaction_read_only=on), so even a read-write role cannot write through this tool")
	skill := flag.String("skill", "", "the production skill to rehearse a trigger for")
	dimension := flag.String("dimension", "", "the judged dimension the rehearsed trigger names (e.g. correct_diagnosis)")
	window := flag.Duration("window", skillgen.DefaultOfflineConfig().Window, "the pool window (the flywheel's generation window)")
	packs := flag.Int("packs", skillstore.DefaultProposerPacks, "evidence packs the proposers read (lowest-scoring sessions first)")
	probeK := flag.Int("probe-k", 1, "replay-probe replicates the composer's prober runs per body (0 = no probe report)")
	timeout := flag.Duration("timeout", 25*time.Minute, "overall deadline")
	outDir := flag.String("out", "", "directory for the drafts, the composition and summary.json (default: proposerdryrun-<UTC stamp>)")
	flag.Parse()
	if *gwURL == "" || *dsn == "" || *skill == "" || *dimension == "" {
		fmt.Fprintln(os.Stderr, "usage: proposerdryrun -gateway URL -dsn DSN -skill NAME -dimension DIM [-window D] [-packs N] [-probe-k N] [-out DIR]")
		os.Exit(2)
	}
	if *outDir == "" {
		*outDir = "proposerdryrun-" + time.Now().UTC().Format("20060102T150405Z")
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		fail("out dir: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	ro, guarded, err := readOnlyDSN(*dsn)
	if err != nil {
		fail("dsn: %v", err)
	}
	if !guarded {
		fmt.Println("proposerdryrun: NOTE — the DSN carries its own `options`, so the server-side read-only guard was NOT added; the shadow store is the only guard on this run")
	}
	pool, err := db.Connect(ctx, ro)
	if err != nil {
		fail("connect (a pooler that rejects the `options` startup parameter fails here — connect to PostgreSQL directly): %v", err)
	}
	defer pool.Close()
	live := db.NewSkillStore(pool)
	shadow := newShadowStore(live)
	ledger := &memLedger{}
	logf := func(format string, args ...any) { fmt.Printf("  │ "+format+"\n", args...) }

	before := countRows(ctx, pool, *skill)
	sk, err := live.GetSkill(ctx, *skill)
	if err != nil {
		fail("skill %s: %v", *skill, err)
	}
	prod, ok, err := live.ProductionVersion(ctx, *skill)
	if err != nil || !ok {
		fail("production version of %s: %v (found=%v)", *skill, err, ok)
	}
	fmt.Printf("proposerdryrun: %s (class %s) production v%s #%d — %d bytes; %d skill_version row(s) before the run\n",
		sk.Name, sk.Class, prod.Version, prod.ID, len(prod.Body), before)

	// The trigger as the flywheel would see it: the dimension's fingerprint-pooled mean in the window.
	stats, err := live.DimensionMeans(ctx, prod.ID, *window, skillstore.DefaultGenMinPoolSessions)
	if err != nil {
		fail("dimension means: %v", err)
	}
	trig := skillstore.GenTrigger{SkillName: *skill, Dimension: *dimension, Threshold: skillstore.DefaultGenThreshold,
		Source: "flywheel:eval-failure:dryrun-" + time.Now().UTC().Format("20060102T1504Z")}
	found := false
	for _, s := range stats {
		if s.Dimension == *dimension {
			trig.Mean, trig.Window, found = s.Mean, s.Samples, true
			fmt.Printf("  pool: %s mean %.2f over %d judged session(s) in %s (%d of another harness excluded)", s.Dimension, s.Mean, s.Samples, *window, s.Excluded)
			if s.Mean < skillstore.DefaultGenThreshold && s.Samples >= skillstore.DefaultGenMinSamples {
				fmt.Println(" — this IS a live trigger")
			} else {
				fmt.Printf(" — NOT a live trigger (threshold %.1f, min %d samples); rehearsal only\n", skillstore.DefaultGenThreshold, skillstore.DefaultGenMinSamples)
			}
		}
	}
	if !found {
		fmt.Printf("  pool: dimension %q has no judged samples in the pool — rehearsal runs with mean 0\n", *dimension)
	}

	ep, err := db.NewEvidenceSource(pool).PacksFor(ctx, prod.ID, *dimension, *window, *packs)
	if err != nil {
		fail("evidence packs: %v", err)
	}
	if len(ep) == 0 {
		fail("no evidence packs for %s/%s in %s — the flywheel would fall back to lens generation here; nothing to rehearse", *skill, *dimension, *window)
	}
	refs := make([]string, 0, len(ep))
	for _, p := range ep {
		refs = append(refs, p.Incident.ExternalRef)
	}
	fmt.Printf("  evidence: %d pack(s): %s\n", len(ep), strings.Join(refs, ", "))

	gw := model.NewGateway(*gwURL, config.SecretRef("env:LITELLM_MASTER_KEY"))
	completer := skillgen.NewPromptCompleter(gw)

	// ---- proposers
	fmt.Println("== proposers (INV + DOM) ==")
	t0 := time.Now()
	drafts, err := skillstore.GenerateCandidatesFromEvidence(ctx, shadow, completer, trig, ep, logf)
	if err != nil {
		fail("proposers (%s): %v", time.Since(t0).Round(time.Second), err)
	}
	fmt.Printf("  %d draft(s) in %s\n", len(drafts), time.Since(t0).Round(time.Second))
	for _, d := range drafts {
		cited := skillstore.CitedRefs(d.Rationale)
		fmt.Printf("  - %s (shadow #%d): %d bytes, author %s, cites %d record ref(s): %s\n", d.Version, d.ID, len(d.Body), d.Author, len(cited), joinKeys(cited))
		writeFile(*outDir, d.Version+".md", d.Body)
		writeFile(*outDir, d.Version+".rationale.txt", d.Rationale)
	}

	// ---- composer (with the real prober when -probe-k > 0)
	var composed skillstore.Version
	kept := false
	if len(drafts) >= 2 {
		fmt.Println("== composer ==")
		g := estate.NewGraph() // the probe's graph is NOT the sessions'; the probe states it
		var probe *skillgen.ProbeConfig
		if *probeK > 0 {
			probe = &skillgen.ProbeConfig{
				Replicates: *probeK,
				Deps:       runner.ProbeDeps{Model: gw, Limits: agent.DefaultLimits(), Graph: func() *estate.Graph { return g }, SkillRows: live.ProductionRows},
				Recordings: skillgen.StepRecordings{Steps: db.NewAgentStepStore(pool), Evidence: db.NewAgentStepEvidenceStore(pool)},
			}
		}
		cfg := skillgen.DefaultOfflineConfig()
		cfg.Window = *window
		prober := skillgen.OfflineRunner{Model: gw, Store: shadow, Incidents: db.NewTriageStore(pool), Cfg: cfg, Probe: probe}
		t1 := time.Now()
		composed, kept, err = skillstore.ComposeCandidates(ctx, shadow, ledger, completer, trig, prod, drafts, prober, logf)
		if err != nil {
			fail("composer (%s): %v", time.Since(t1).Round(time.Second), err)
		}
		fmt.Printf("  %s (%s)\n", describeComposition(composed, kept, drafts), time.Since(t1).Round(time.Second))
		if composed.ID != 0 {
			writeFile(*outDir, composed.Version+".md", composed.Body)
			writeFile(*outDir, composed.Version+".rationale.txt", composed.Rationale)
			if i := strings.Index(composed.Rationale, "notes:\n"); i >= 0 {
				fmt.Println("  notes:")
				for _, row := range strings.Split(composed.Rationale[i+len("notes:\n"):], "\n") {
					if strings.TrimSpace(row) != "" {
						fmt.Println("    " + row)
					}
				}
			}
		}
	} else {
		fmt.Printf("== composer == skipped: %d proposer draft(s) (needs two)\n", len(drafts))
	}

	// ---- what would have been written, and the proof that it was not
	after := countRows(ctx, pool, *skill)
	fmt.Println("== shadow (nothing below reached the database) ==")
	for _, w := range shadow.Writes() {
		fmt.Println("  " + w)
	}
	for _, e := range ledger.Entries() {
		fmt.Printf("  LEDGER seq %d: %s — %s\n", e.Seq, e.Decision, e.Reason)
	}
	fmt.Printf("  skill_version rows for %s: %d before, %d after (connection read-only: default_transaction_read_only=on)\n", *skill, before, after)
	if after != before {
		// This tool's connection cannot write (the server refuses writes in a read-only transaction), so a changed
		// count is a CONCURRENT writer — the generator cycle, a trial start — not this run. Say so; keep the output.
		fmt.Printf("  WARNING: the count changed while this ran — something else wrote %s rows (the 04:37Z generator cycle?); this tool's connection is read-only and could not have\n", *skill)
	}

	summary := map[string]any{
		"skill": *skill, "dimension": *dimension, "window": window.String(), "trigger": trig, "pack_refs": refs,
		"drafts": versionSummaries(drafts), "composition": describeComposition(composed, kept, drafts),
		"composed": versionSummaries(nonZero(composed)), "shadow_writes": shadow.Writes(), "ledger": ledger.Entries(),
		"rows_before": before, "rows_after": after, "finished_at": time.Now().UTC().Format(time.RFC3339),
	}
	b, _ := json.MarshalIndent(summary, "", "  ")
	writeFile(*outDir, "summary.json", string(b))
	fmt.Printf("proposerdryrun: done — files in %s\n", *outDir)
}

// readOnlyDSN makes the connection itself unable to write: PostgreSQL's default_transaction_read_only=on is passed
// as a startup option, so any INSERT/UPDATE/DELETE on this pool fails at the server ("cannot execute … in a read-only
// transaction") whatever role the DSN carries. The shadow store is the first line of defence; this is the one that
// does not depend on reading the code right.
// The second result says whether the guard was applied; a DSN that already sets `options` is left alone (the
// operator chose) and the caller says so out loud.
func readOnlyDSN(dsn string) (string, bool, error) {
	const opt = "-c default_transaction_read_only=on"
	if strings.Contains(dsn, "://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return "", false, fmt.Errorf("parse DSN: %w", err)
		}
		q := u.Query()
		if q.Get("options") != "" {
			return dsn, false, nil
		}
		q.Set("options", opt)
		u.RawQuery = q.Encode()
		return u.String(), true, nil
	}
	// keyword/value form: `host=… dbname=…`
	if strings.Contains(dsn, "options=") {
		return dsn, false, nil
	}
	return strings.TrimSpace(dsn) + " options='" + opt + "'", true, nil
}

// countRows is the before/after witness (not the only guard — see readOnlyDSN); a single parameterised read.
func countRows(ctx context.Context, pool *db.Pool, skill string) int {
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM skill_version WHERE skill_name = $1`, skill).Scan(&n); err != nil {
		fail("count skill_version rows: %v", err)
	}
	return n
}

func versionSummaries(vs []skillstore.Version) []map[string]any {
	out := make([]map[string]any, 0, len(vs))
	for _, v := range vs {
		out = append(out, map[string]any{"shadow_id": v.ID, "version": v.Version, "status": v.Status, "bytes": len(v.Body),
			"author": v.Author, "source": v.Source, "cited_refs": joinKeys(skillstore.CitedRefs(v.Rationale))})
	}
	return out
}

func nonZero(v skillstore.Version) []skillstore.Version {
	if v.ID == 0 {
		return nil
	}
	return []skillstore.Version{v}
}

func joinKeys(m map[string]bool) string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return strings.Join(ks, ", ")
}

func writeFile(dir, name, body string) {
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		fail("write %s: %v", name, err)
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "proposerdryrun: "+format+"\n", args...)
	os.Exit(1)
}
