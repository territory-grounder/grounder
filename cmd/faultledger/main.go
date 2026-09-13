// Command faultledger records a deliberately-injected fault into the injected_fault ground-truth ledger
// (migration 0038) so the live-axis scorer (cmd/axisscore) can measure benchmark axis A1 (detection recall):
// what fraction of known-injected faults TG actually detected. It is out-of-band benchmark instrumentation —
// run it at injection time, alongside the guinea-pig fault (per the injection procedure). It writes ONE row;
// it never touches the agent decision path.
//
// Usage (on a host that reaches the grounder DB, e.g. dc1tg01):
//
//	TG_RUNTIME_DSN=postgres://… faultledger -host dc1excalidraw01 -type device-down [-note "drill X"]
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/territory-grounder/grounder/core/db"
)

func main() {
	host := flag.String("host", "", "the faulted host (must match ingest_alert.host — the alerted device name)")
	ftype := flag.String("type", "", "fault type: device-down | disk-fill | service-down | memory | …")
	note := flag.String("note", "", "optional free-text context (the guinea-pig, the drill)")
	dsn := flag.String("dsn", os.Getenv("TG_RUNTIME_DSN"), "grounder DSN (defaults to $TG_RUNTIME_DSN)")
	flag.Parse()

	if strings.TrimSpace(*host) == "" || strings.TrimSpace(*ftype) == "" {
		fmt.Fprintln(os.Stderr, "faultledger: -host and -type are required")
		os.Exit(2)
	}
	if *dsn == "" {
		fmt.Fprintln(os.Stderr, "faultledger: no DSN — set $TG_RUNTIME_DSN or pass -dsn")
		os.Exit(2)
	}
	ctx := context.Background()
	pool, err := db.Connect(ctx, *dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "faultledger: connect: %v\n", err)
		os.Exit(1)
	}
	defer pool.Close()

	if _, err := pool.Exec(ctx,
		`INSERT INTO injected_fault (host, fault_type, note) VALUES ($1, $2, $3)`,
		strings.TrimSpace(*host), strings.TrimSpace(*ftype), strings.TrimSpace(*note)); err != nil {
		fmt.Fprintf(os.Stderr, "faultledger: record: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("recorded injected fault: host=%s type=%s (axis A1 denominator +1)\n", *host, *ftype)
}
