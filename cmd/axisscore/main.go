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
// Usage (on a host that reaches the grounder DB):
//
//	TG_RUNTIME_DSN=postgres://… axisscore [-window 168h] [-json]
//	TG_RUNTIME_DSN=postgres://… axisscore -since 2026-11-10 [-until 2026-11-28] [-site dc2] [-json]
//	TG_RUNTIME_DSN=postgres://… axisscore -benchmark-window -since 2026-11-10 [-until 2026-11-28] [-json]
//
// -since/-until fix the window (RFC 3339, or a bare YYYY-MM-DD read as UTC midnight; -until defaults to now
// and is exclusive). -site scopes the scorecard to one declared site (TG-632). -benchmark-window prints the
// G4 report instead — every declared site × every axis, n against the bar, plus the window's rubric and
// harness uniformity — and exits 3 when the window does not meet the bar, so a script can gate on it.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/territory-grounder/grounder/core/axis"
	"os"
	"strings"
	"time"

	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/ingest"
	"github.com/territory-grounder/grounder/core/judge"
)

// exitBelowBar is -benchmark-window's exit status when the window does not meet the G4 bar: distinct from
// 1 (a read failed) and 2 (usage), so "the tool broke" and "the evidence is not there yet" never look alike.
const exitBelowBar = 3

func main() {
	window := flag.Duration("window", 168*time.Hour, "score triages created within this trailing window (ignored when -since is set)")
	asJSON := flag.Bool("json", false, "emit the scorecard as JSON instead of text")
	dsn := flag.String("dsn", os.Getenv("TG_RUNTIME_DSN"), "grounder DSN (defaults to $TG_RUNTIME_DSN)")
	sinceFlag := flag.String("since", "", "fixed window start, inclusive (RFC 3339 or YYYY-MM-DD as UTC midnight); overrides -window")
	untilFlag := flag.String("until", "", "fixed window end, exclusive (RFC 3339 or YYYY-MM-DD as UTC midnight); default now")
	site := flag.String("site", "", fmt.Sprintf("score one declared site %v instead of the whole estate", ingest.DeclaredSites()))
	bench := flag.Bool("benchmark-window", false, "print the G4 benchmark-window report (every site × axis against n ≥ 30) — needs -since; the same as -bar g4")
	barFlag := flag.String("bar", "", "print the benchmark-window report against a bar: g4 (the 14-day bar) or g4a (accelerated: no duration, one instrument identity) — needs -since")
	excludeFlag := flag.String("exclude-refs", "", "file of session external_refs (one per line, # comments) to remove from every session-derived cell; needs -benchmark-window or -bar")
	flag.Parse()

	bar := axis.BarG4
	if *barFlag != "" {
		b, err := axis.ParseBar(*barFlag)
		if err != nil {
			fmt.Fprintf(os.Stderr, "axisscore: %v\n", err)
			os.Exit(2)
		}
		bar = b
		*bench = true
	}

	if *dsn == "" {
		fmt.Fprintln(os.Stderr, "axisscore: no DSN — set $TG_RUNTIME_DSN or pass -dsn")
		os.Exit(2)
	}
	windowSet := false
	flag.Visit(func(f *flag.Flag) { windowSet = windowSet || f.Name == "window" })
	now := time.Now().UTC()
	scope, err := parseScope(*sinceFlag, *untilFlag, *site, *bench, *window, windowSet, now)
	if err != nil {
		fmt.Fprintf(os.Stderr, "axisscore: %v\n", err)
		os.Exit(2)
	}
	ctx := context.Background()
	pool, err := db.Connect(ctx, *dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "axisscore: connect: %v\n", err)
		os.Exit(1)
	}
	defer pool.Close()
	store := db.NewAxisReadStore(pool)

	if *excludeFlag != "" && !*bench {
		fmt.Fprintln(os.Stderr, "axisscore: -exclude-refs needs -benchmark-window or -bar")
		os.Exit(2)
	}
	if *bench {
		// The fixed window is closed at `now` when -until was omitted, so the report names an exact window.
		if scope.Until.IsZero() {
			scope.Until = now
		}
		excludeRefs, err := loadExcludeRefs(*excludeFlag)
		if err != nil {
			fmt.Fprintf(os.Stderr, "axisscore: %v\n", err)
			os.Exit(1)
		}
		rep, err := axis.BenchmarkWindowBar(ctx, store, bar, scope.Since, scope.Until, judge.RubricVersion(), excludeRefs...)
		if err != nil {
			fmt.Fprintf(os.Stderr, "axisscore: benchmark window: %v\n", err)
			os.Exit(1)
		}
		if err := emit(rep, rep.Text(), *asJSON); err != nil {
			fmt.Fprintf(os.Stderr, "axisscore: encode: %v\n", err)
			os.Exit(1)
		}
		if !rep.Passed() {
			os.Exit(exitBelowBar)
		}
		return
	}

	since := scope.Since
	agg, err := store.AggregateScoped(ctx, scope)
	if err != nil {
		fmt.Fprintf(os.Stderr, "axisscore: aggregate: %v\n", err)
		os.Exit(1)
	}

	// G5 is read separately: it aggregates infragraph_cascade_stats, not the triage/judge join every other
	// axis derives from. A read failure is NOT fatal — the other eight axes are still worth printing, and an
	// exceed-proof that refuses to render because one axis is unavailable teaches people to skip the tool.
	fals, ferr := store.FalsifiabilityScoped(ctx, scope)
	if ferr != nil {
		fmt.Fprintf(os.Stderr, "axisscore: falsifiability axis unavailable: %v (other axes still scored)\n", ferr)
	}

	// G6 (loop-bypass) reads action_execution joined to the prediction/verify record — a different spine from
	// the triage/judge aggregate. Non-fatal for the same reason as G5: the guardrail axis being unavailable
	// must not blank the whole scorecard.
	lb, lberr := store.LoopBypassScoped(ctx, scope)
	if lberr != nil {
		fmt.Fprintf(os.Stderr, "axisscore: loop-bypass axis unavailable: %v (other axes still scored)\n", lberr)
	}

	span := *window
	if *sinceFlag != "" {
		end := scope.Until
		if end.IsZero() {
			end = now
		}
		span = end.Sub(since)
	}
	sc := axis.Score(agg, span)
	sc.Falsifiability = fals
	sc.LoopBypass = lb
	if err := emit(sc, sc.Text(), *asJSON); err != nil {
		fmt.Fprintf(os.Stderr, "axisscore: encode: %v\n", err)
		os.Exit(1)
	}
}

// parseScope turns the flags into an AxisScope. Without -since it is the trailing window every earlier
// release measured (estate-wide unless -site). Usage errors: an undeclared -site; -until without -since;
// -window together with -since (one would be silently ignored); -benchmark-window without -since, with
// -site (the report always covers every site), or with an -until still in the future — a window that has
// not elapsed has a nominal length the evidence does not have, and must not be able to PASS the 14-day bar.
func parseScope(since, until, site string, bench bool, window time.Duration, windowSet bool, now time.Time) (db.AxisScope, error) {
	var sc db.AxisScope
	if windowSet && since != "" {
		return sc, fmt.Errorf("-window and -since both set: a fixed window is -since/-until, a trailing one is -window")
	}
	if site != "" && !ingest.IsDeclaredSite(site) {
		return sc, fmt.Errorf("-site %q is not a declared site %v", site, ingest.DeclaredSites())
	}
	if bench && site != "" {
		return sc, fmt.Errorf("-benchmark-window reports every declared site; drop -site")
	}
	if bench && since == "" {
		return sc, fmt.Errorf("-benchmark-window needs a fixed window: pass -since (and optionally -until)")
	}
	if since == "" && until != "" {
		return sc, fmt.Errorf("-until needs -since")
	}
	sc.Site = site
	if since == "" {
		sc.Since = now.Add(-window)
		return sc, nil
	}
	var err error
	if sc.Since, err = parseInstant(since); err != nil {
		return sc, fmt.Errorf("-since: %w", err)
	}
	if until != "" {
		if sc.Until, err = parseInstant(until); err != nil {
			return sc, fmt.Errorf("-until: %w", err)
		}
		if !sc.Until.After(sc.Since) {
			return sc, fmt.Errorf("-until %s is not after -since %s", until, since)
		}
		if bench && sc.Until.After(now) {
			return sc, fmt.Errorf("-until %s has not happened yet: a benchmark window is scored once it has closed", until)
		}
	} else if !now.After(sc.Since) {
		return sc, fmt.Errorf("-since %s is in the future", since)
	}
	return sc, nil
}

// parseInstant accepts RFC 3339 or a bare date (UTC midnight).
func parseInstant(v string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t.UTC(), nil
	}
	t, err := time.Parse("2006-01-02", v)
	if err != nil {
		return time.Time{}, fmt.Errorf("%q is neither RFC 3339 nor YYYY-MM-DD", v)
	}
	return t.UTC(), nil
}

// emit writes v as indented JSON, or the text rendering.
func emit(v any, text string, asJSON bool) error {
	if !asJSON {
		_, err := fmt.Print(text)
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func loadExcludeRefs(path string) ([]string, error) {
	if path == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("exclude-refs: %w", err)
	}
	seen := map[string]bool{}
	var out []string
	for _, line := range strings.Split(string(raw), "\n") {
		ref := strings.TrimSpace(line)
		if ref == "" || strings.HasPrefix(ref, "#") {
			continue
		}
		if seen[ref] {
			return nil, fmt.Errorf("exclude-refs: %q is listed twice", ref)
		}
		seen[ref] = true
		out = append(out, ref)
	}
	return out, nil
}
