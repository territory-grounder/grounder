package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/db"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("ledgerverify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dsn := fs.String("dsn", os.Getenv("TG_RUNTIME_DSN"), "grounder DSN (defaults to $TG_RUNTIME_DSN)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *dsn == "" {
		fmt.Fprintln(stderr, "ledgerverify: no DSN")
		return 2
	}
	ctx := context.Background()
	pool, err := db.Connect(ctx, *dsn)
	if err != nil {
		fmt.Fprintf(stderr, "ledgerverify: connect: %v\n", err)
		return 2
	}
	defer pool.Close()
	entries, err := db.NewLedgerStore(pool).All(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "ledgerverify: read ledger: %v\n", err)
		return 2
	}
	anchors, err := db.NewAnchorStore(pool).Anchors(ctx, audit.DomainGovernanceLedger)
	if err != nil {
		fmt.Fprintf(stderr, "ledgerverify: read anchors: %v\n", err)
		return 2
	}
	res := Verify(entries, anchors, time.Now())
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(res); err != nil {
		fmt.Fprintf(stderr, "ledgerverify: encode: %v\n", err)
		return 2
	}
	if !res.ChainOK || !res.AnchorsOK {
		return 1
	}
	return 0
}
