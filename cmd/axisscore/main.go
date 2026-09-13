// Command axisscore is the LIVE half of the R1 benchmark harness: it rolls up the durable session_triage +
// session_judgment tables — the record of REAL triages the worker produced against live LibreNMS-detected
// faults — into the scored benchmark axes (docs/BENCHMARK-AXES.md). Where the eval corpus re-runs the Runner
// over a fixed session set (a controlled A/B), this scores the axes over the incidents the system ACTUALLY
// handled, so a governed MR's axis movement is read off production data rather than a hand-run SQL query.
//
// It is READ-ONLY (bound aggregate queries) and prints a scorecard — text by default, `-json` for a machine
// artifact. It measures ALL EIGHT scored axes off the durable tables — A1 detection recall, A2 diagnosis
// correctness, A3 heal success, A4 autonomy rate, A5 fault-class breadth, A6a decision steps + A6b wall-clock
// (TG-205 split the axis: A6 was defined as MTTR and measured only in steps), A7
// false-actuation rate, A8 safety-violation count — and, for the axes that need a live event that may not have
// occurred yet (A1 an injected fault, A3/A7 an actuated mutation), NAMES the coverage gap with the concrete
// missing input rather than reporting a false 0.
//
// Usage (on a host that reaches the grounder DB, e.g. dc1tg01):
//
//	TG_RUNTIME_DSN=postgres://… axisscore [-window 168h] [-json]
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/territory-grounder/grounder/core/axis"
	"os"
	"time"

	"github.com/territory-grounder/grounder/core/db"
)

func main() {
	window := flag.Duration("window", 168*time.Hour, "score triages created within this trailing window")
	asJSON := flag.Bool("json", false, "emit the scorecard as JSON instead of text")
	dsn := flag.String("dsn", os.Getenv("TG_RUNTIME_DSN"), "grounder DSN (defaults to $TG_RUNTIME_DSN)")
	flag.Parse()

	if *dsn == "" {
		fmt.Fprintln(os.Stderr, "axisscore: no DSN — set $TG_RUNTIME_DSN or pass -dsn")
		os.Exit(2)
	}
	ctx := context.Background()
	pool, err := db.Connect(ctx, *dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "axisscore: connect: %v\n", err)
		os.Exit(1)
	}
	defer pool.Close()

	since := time.Now().Add(-*window)
	agg, err := db.NewAxisReadStore(pool).Aggregate(ctx, since)
	if err != nil {
		fmt.Fprintf(os.Stderr, "axisscore: aggregate: %v\n", err)
		os.Exit(1)
	}

	// G5 is read separately: it aggregates infragraph_cascade_stats, not the triage/judge join every other
	// axis derives from. A read failure is NOT fatal — the other eight axes are still worth printing, and an
	// exceed-proof that refuses to render because one axis is unavailable teaches people to skip the tool.
	fals, ferr := db.NewAxisReadStore(pool).Falsifiability(ctx, since)
	if ferr != nil {
		fmt.Fprintf(os.Stderr, "axisscore: falsifiability axis unavailable: %v (other axes still scored)\n", ferr)
	}

	// G6 (loop-bypass) reads action_execution joined to the prediction/verify record — a different spine from
	// the triage/judge aggregate. Non-fatal for the same reason as G5: the guardrail axis being unavailable
	// must not blank the whole scorecard.
	lb, lberr := db.NewAxisReadStore(pool).LoopBypass(ctx, since)
	if lberr != nil {
		fmt.Fprintf(os.Stderr, "axisscore: loop-bypass axis unavailable: %v (other axes still scored)\n", lberr)
	}

	sc := axis.Score(agg, *window)
	sc.Falsifiability = fals
	sc.LoopBypass = lb
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(sc); err != nil {
			fmt.Fprintf(os.Stderr, "axisscore: encode: %v\n", err)
			os.Exit(1)
		}
		return
	}
	fmt.Print(sc.Text())
}
