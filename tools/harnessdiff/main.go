// Command harnessdiff reads the rendering behind a harness fingerprint (TG-600, Harness² learnings H2-1c):
// what the model actually saw under a configuration, and what changed between two of them — per
// component (protocol prose, tool catalog, op-class catalog, guidance half), with the build lineage and
// the number of sessions each configuration composed. It answers "what did the agent run under on that
// session" and "what moved between the configuration that scored X and this one" from the database,
// without re-rendering from a checkout.
//
//	harnessdiff -dsn postgres://…                       # list every recorded configuration (default)
//	harnessdiff -dsn … -show 6e2de4caf307               # the exact system half (or -component guidance)
//	harnessdiff -dsn … -diff 6e2de4caf307 1f9ab2…       # per-component diff, A -> B
//
// Fingerprints are given by prefix (≥4 hex); an ambiguous prefix is refused with the candidates named.
// The DSN defaults to $TG_RUNTIME_DSN; HARNESSDIFF_DSN may carry it instead of the flag (keeps it out of
// `ps`). The connection is opened with default_transaction_read_only=on, so the server refuses any write
// through this tool whatever role the DSN carries. Exit 0 on success (a diff that differs is still a
// success — the summary line says so), 1 on failure, 2 on usage.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/harnessid"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("harnessdiff", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dsnDefault := os.Getenv("HARNESSDIFF_DSN")
	if dsnDefault == "" {
		dsnDefault = os.Getenv("TG_RUNTIME_DSN")
	}
	dsn := fs.String("dsn", dsnDefault, "grounder DSN (defaults to $HARNESSDIFF_DSN, then $TG_RUNTIME_DSN)")
	fs.Bool("list", false, "list every recorded configuration (the default when nothing else is asked)")
	show := fs.String("show", "", "fingerprint (prefix) to print in full")
	only := fs.String("component", "", "narrow -show / -diff to one component: prose | tool_catalog | opclass_catalog | guidance")
	diff := fs.Bool("diff", false, "diff two fingerprints (prefixes) given as the two positional arguments, A then B")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*dsn) == "" {
		fmt.Fprintln(stderr, "harnessdiff: no DSN (-dsn, $HARNESSDIFF_DSN or $TG_RUNTIME_DSN)")
		return 2
	}
	if *diff && fs.NArg() != 2 {
		fmt.Fprintln(stderr, "harnessdiff: -diff takes exactly two fingerprint prefixes: A B")
		return 2
	}
	if !*diff && fs.NArg() != 0 {
		fmt.Fprintln(stderr, "harnessdiff: positional arguments are only read with -diff")
		return 2
	}
	if *only != "" {
		if _, ok := component(harnessid.Parts{}, *only); !ok { // a usage error must not cost a database round trip
			fmt.Fprintf(stderr, "harnessdiff: unknown component %q (one of %s)\n", *only, strings.Join(componentNames, ", "))
			return 2
		}
	}
	ro, applied, err := readOnlyDSN(*dsn)
	if err != nil {
		fmt.Fprintf(stderr, "harnessdiff: %v\n", err)
		return 2
	}
	if !applied {
		fmt.Fprintln(stderr, "harnessdiff: DSN already carries `options`; the read-only guard was NOT added (operator's choice)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool, err := db.Connect(ctx, ro)
	if err != nil {
		fmt.Fprintf(stderr, "harnessdiff: connect: %v\n", err)
		return 1
	}
	defer pool.Close()
	store := db.NewHarnessRenderStore(pool)

	switch {
	case *diff:
		a, err := store.Get(ctx, fs.Arg(0))
		if err != nil {
			fmt.Fprintf(stderr, "harnessdiff: A: %v\n", err)
			return 1
		}
		b, err := store.Get(ctx, fs.Arg(1))
		if err != nil {
			fmt.Fprintf(stderr, "harnessdiff: B: %v\n", err)
			return 1
		}
		text, n, err := renderDiff(a, b, *only)
		if err != nil {
			fmt.Fprintf(stderr, "harnessdiff: %v\n", err)
			return 2
		}
		fmt.Fprint(stdout, text)
		fmt.Fprintf(stdout, "%d component(s) differ\n", n)
		return 0
	case *show != "":
		r, err := store.Get(ctx, *show)
		if err != nil {
			fmt.Fprintf(stderr, "harnessdiff: %v\n", err)
			return 1
		}
		text, err := renderShow(r, *only)
		if err != nil {
			fmt.Fprintf(stderr, "harnessdiff: %v\n", err)
			return 2
		}
		fmt.Fprint(stdout, text)
		return 0
	default:
		rows, err := store.List(ctx)
		if err != nil {
			fmt.Fprintf(stderr, "harnessdiff: %v\n", err)
			return 1
		}
		fmt.Fprint(stdout, renderList(rows))
		return 0
	}
}

// readOnlyDSN makes the connection itself unable to write (the proposerdryrun pattern): PostgreSQL's
// default_transaction_read_only=on is passed as a startup option, so any INSERT/UPDATE/DELETE on this pool
// fails at the server whatever role the DSN carries. The second result says whether the guard was applied;
// a DSN that already sets `options` is left alone and the caller says so out loud.
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
