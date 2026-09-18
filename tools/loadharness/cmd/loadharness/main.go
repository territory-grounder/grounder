// Command loadharness drives the TG-80 P1#2 real-run e2e concurrency/throughput harness: N concurrent
// synthetic incidents through the REAL ingest→triage pipeline of ANY TG deployment, measured (throughput,
// p50/p95/max end-to-end latency per concurrency level) and judged (no session lost, exactly one session
// per incident with a mid-run duplicate-ref probe, no cross-contamination). Exit 0 only when every run
// completed and every invariant held; otherwise non-zero with the failing refs named.
//
// The synthetic incidents live under the RFC-2606 fixture namespace *.loadharness.invalid and can never
// name a real estate host. Against a live deployment the runs are REAL: each one spends a real Runner
// triage session (model calls included) on the read-only path — Phase-0/1 triage drives to a gated
// proposal and stops; nothing actuates.
//
// Credentials arrive via the environment, never argv (argv is world-readable in /proc):
//
//	TG_LOADHARNESS_INGEST_TOKEN   per-source static bearer for POST /v1/ingest/{source_type}
//	                              (optional — without it the POST is HMAC-signed)
//	TG_LOADHARNESS_HMAC_SOURCE    machine source id for the HMAC path (required: the sessions
//	TG_LOADHARNESS_HMAC_SECRET    read-back is authenticated)
//
// Usage:
//
//	loadharness -base-url https://tg.example -n 8 -sweep 1,4,8,16,32 -json report.json
//	loadharness -selftest                            # drive the in-process rig; no deployment needed
//
// Exit codes: 0 all runs green · 1 any failed run/invariant · 2 configuration refused.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/territory-grounder/grounder/tools/loadharness"
)

func main() {
	os.Exit(run())
}

func run() int {
	var (
		baseURL     = flag.String("base-url", "", "target deployment API origin (e.g. the compose stack); omit with -selftest")
		sourceType  = flag.String("source", "prometheus-alertmanager", "ingest source slug to POST to")
		runs        = flag.Int("n", 8, "incidents per concurrency level (1..200)")
		concurrency = flag.Int("c", 8, "single concurrency level (ignored when -sweep is set)")
		sweep       = flag.String("sweep", "1,4,8,16,32", "comma-separated concurrency sweep (TG-80 P1-2 default: the 1/4/8/16/32 ladder); set to a single level to skip the sweep")
		terminal    = flag.String("terminal", "proposed,executed,stopped", "comma-separated session statuses that count as a COMPLETED run (the detail surface's status)")
		runID       = flag.String("run-id", "", "fixture-namespace discriminator (default: random per invocation; re-using one against the same deployment dedups into finished sessions and passes vacuously)")
		pollEvery   = flag.Duration("poll-interval", 250*time.Millisecond, "spine polling cadence (bounds latency resolution)")
		sessionTO   = flag.Duration("session-timeout", 2*time.Minute, "per-incident visibility deadline")
		runTO       = flag.Duration("run-timeout", 15*time.Minute, "whole-invocation wall-clock cap")
		quietSpine  = flag.Bool("expect-quiet-spine", false, "additionally assert the spine population grew by exactly the accepted refs (hermetic targets only — organic traffic false-flags)")
		jsonOut     = flag.String("json", "", "also write the machine-readable report to this path")
		selftest    = flag.Bool("selftest", false, "stand up the in-process rig and drive it (no deployment, no credentials needed)")
	)
	flag.Parse()

	cfg := loadharness.Config{
		BaseURL:          *baseURL,
		SourceType:       *sourceType,
		IngestToken:      os.Getenv("TG_LOADHARNESS_INGEST_TOKEN"),
		HMACSource:       os.Getenv("TG_LOADHARNESS_HMAC_SOURCE"),
		HMACSecret:       []byte(os.Getenv("TG_LOADHARNESS_HMAC_SECRET")),
		RunID:            *runID,
		Runs:             *runs,
		PollInterval:     *pollEvery,
		SessionTimeout:   *sessionTO,
		RunTimeout:       *runTO,
		ExpectQuietSpine: *quietSpine,
	}
	for _, s := range strings.Split(*terminal, ",") {
		if s = strings.TrimSpace(s); s != "" {
			cfg.TerminalStatuses = append(cfg.TerminalStatuses, s)
		}
	}
	if *sweep != "" {
		for _, part := range strings.Split(*sweep, ",") {
			lvl, err := strconv.Atoi(strings.TrimSpace(part))
			if err != nil {
				fmt.Fprintf(os.Stderr, "loadharness: bad -sweep entry %q: %v\n", part, err)
				return 2
			}
			cfg.Levels = append(cfg.Levels, lvl)
		}
	} else {
		cfg.Levels = []int{*concurrency}
	}

	if *selftest {
		rig, err := loadharness.StartRig(loadharness.RigFaults{})
		if err != nil {
			fmt.Fprintf(os.Stderr, "loadharness: rig: %v\n", err)
			return 2
		}
		defer rig.Close()
		rigCfg := rig.HarnessConfig()
		// Keep the operator's shape; take the rig's origin, credentials and CI-scale timing.
		rigCfg.Runs, rigCfg.Levels, rigCfg.RunID = cfg.Runs, cfg.Levels, cfg.RunID
		cfg = rigCfg
	} else if cfg.BaseURL == "" {
		fmt.Fprintln(os.Stderr, "loadharness: -base-url required (or -selftest)")
		return 2
	}

	rep := loadharness.Run(context.Background(), cfg)
	rep.WriteHuman(os.Stdout)
	if *jsonOut != "" {
		b, err := json.MarshalIndent(rep, "", "  ")
		if err == nil {
			err = os.WriteFile(*jsonOut, b, 0o644)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "loadharness: json report: %v\n", err)
			return 1
		}
	}
	return rep.ExitCode()
}
