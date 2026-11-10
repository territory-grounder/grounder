// Command episodes rebuilds an injection campaign's fault → alert → session → execution → credit attribution
// from immutable identifiers and prints the disposition table (TG-672, spec/025 REQ-2534).
//
// A fault owns its host from its injection until the EARLIER of the next injection on that host and its restore
// deadline plus ten minutes; an alert belongs to its owner only when its rule class matches the fault class;
// executions and credits ride the alert's external_ref. One fault may own several alerts; an alert has one
// owner. A same-guest OVERLAP (B injected inside A's raw window) keeps the ownership cut only where evidence
// separates the two faults — A acted on first, the op class of each execution, an alert observed after B was
// injected — else both are `ambiguous` (TG-679): in every denominator, never credited, the carried credit
// disclosed; -overlaps writes every pair with the rule that decided it. A fault with no alert stays in every
// denominator; an in-window execution the cohort cannot claim is listed with its reason. See core/episodes.
//
//	episodes -since 2026-11-06T11:04:00Z -until 2026-11-07T17:00:00Z [-note-prefix "TG-624 G3"] [-csv out.csv] [-json out.json]
//
// The DSN defaults to $EPISODES_DSN, then $TG_RUNTIME_DSN, and is opened with default_transaction_read_only=on
// so the server refuses any write whatever role the DSN carries. Exit 0 on success, 1 on a read failure, 2 on
// usage. The summary is printed to stdout; -csv writes one row per fault (cohort first, then the pre-window
// owners of in-window evidence, flagged in_cohort=false); -json writes the whole report.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/episodes"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("episodes", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dsnDefault := os.Getenv("EPISODES_DSN")
	if dsnDefault == "" {
		dsnDefault = os.Getenv("TG_RUNTIME_DSN")
	}
	dsn := fs.String("dsn", dsnDefault, "grounder DSN (defaults to $EPISODES_DSN, then $TG_RUNTIME_DSN)")
	sinceFlag := fs.String("since", "", "cohort window start, inclusive (RFC 3339 or YYYY-MM-DD as UTC midnight)")
	untilFlag := fs.String("until", "", "cohort window end, exclusive (RFC 3339 or YYYY-MM-DD as UTC midnight)")
	notePrefix := fs.String("note-prefix", "", "select only faults whose injector note starts with this prefix (a campaign tag); empty = every fault")
	lookback := fs.Duration("lookback", db.DefaultEpisodeLookback, "how far before/after the window to read faults, alerts, executions and credits")
	csvPath := fs.String("csv", "", "write the per-fault CSV here ('-' for stdout)")
	jsonPath := fs.String("json", "", "write the full report as JSON here ('-' for stdout)")
	overlapsPath := fs.String("overlaps", "", "write the same-guest overlap table here, one row per pair: .md for Markdown, anything else CSV ('-' for stdout, CSV)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "episodes: no positional arguments")
		return 2
	}
	if strings.TrimSpace(*dsn) == "" {
		fmt.Fprintln(stderr, "episodes: no DSN (-dsn, $EPISODES_DSN or $TG_RUNTIME_DSN)")
		return 2
	}
	if *sinceFlag == "" || *untilFlag == "" {
		fmt.Fprintln(stderr, "episodes: -since and -until are required (a cohort is a fixed window)")
		return 2
	}
	since, err := parseInstant(*sinceFlag)
	if err != nil {
		fmt.Fprintf(stderr, "episodes: -since: %v\n", err)
		return 2
	}
	until, err := parseInstant(*untilFlag)
	if err != nil {
		fmt.Fprintf(stderr, "episodes: -until: %v\n", err)
		return 2
	}
	if !until.After(since) {
		fmt.Fprintln(stderr, "episodes: -until must be after -since")
		return 2
	}
	ro, applied, err := readOnlyDSN(*dsn)
	if err != nil {
		fmt.Fprintf(stderr, "episodes: %v\n", err)
		return 2
	}
	if !applied {
		fmt.Fprintln(stderr, "episodes: DSN already carries `options`; the read-only guard was NOT added (operator's choice)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	pool, err := db.Connect(ctx, ro)
	if err != nil {
		fmt.Fprintf(stderr, "episodes: connect: %v\n", err)
		return 1
	}
	defer pool.Close()
	in, err := db.NewEpisodeReadStore(pool).Load(ctx, db.EpisodeScope{Since: since, Until: until, NotePrefix: *notePrefix, Lookback: *lookback})
	if err != nil {
		fmt.Fprintf(stderr, "episodes: %v\n", err)
		return 1
	}
	rep := episodes.Attribute(in)
	fmt.Fprint(stdout, rep.Text())
	if *csvPath != "" {
		if err := writeTo(*csvPath, stdout, func(w io.Writer) error { return episodes.WriteCSV(w, rep) }); err != nil {
			fmt.Fprintf(stderr, "episodes: csv: %v\n", err)
			return 1
		}
	}
	if *overlapsPath != "" {
		write := episodes.WriteOverlapsCSV
		if strings.HasSuffix(*overlapsPath, ".md") {
			write = episodes.WriteOverlapsMarkdown
		}
		if err := writeTo(*overlapsPath, stdout, func(w io.Writer) error { return write(w, rep) }); err != nil {
			fmt.Fprintf(stderr, "episodes: overlaps: %v\n", err)
			return 1
		}
	}
	if *jsonPath != "" {
		if err := writeTo(*jsonPath, stdout, func(w io.Writer) error {
			enc := json.NewEncoder(w)
			enc.SetIndent("", "  ")
			return enc.Encode(rep)
		}); err != nil {
			fmt.Fprintf(stderr, "episodes: json: %v\n", err)
			return 1
		}
	}
	return 0
}

func writeTo(path string, stdout io.Writer, f func(io.Writer) error) error {
	if path == "-" {
		return f(stdout)
	}
	fh, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := f(fh); err != nil {
		fh.Close()
		return err
	}
	return fh.Close()
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

// readOnlyDSN adds default_transaction_read_only=on to a URL or keyword DSN (the tools/harnessdiff guard): the
// server then refuses every write on the connection. An operator-supplied `options` is left alone, and said so.
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
	if strings.Contains(dsn, "options=") {
		return dsn, false, nil
	}
	return strings.TrimSpace(dsn) + " options='" + opt + "'", true, nil
}
