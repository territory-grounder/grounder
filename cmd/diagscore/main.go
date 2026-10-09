// Command diagscore reports TG's DIAGNOSIS accuracy against ground truth.
//
// It answers "when a fault of a known class was live on a host, did TG propose the op-class that addresses
// it" — measured from the fault injector's own durable record rather than from a judge's opinion. That makes
// it the strongest diagnosis evidence available for the classes the injector covers, and the ground truth the
// judge is calibrated against.
//
//	TG_RUNTIME_DSN=postgres://… diagscore [-expect core/diagcorpus/expectations.json] [-grace 25m]
//
// It is READ-ONLY: it opens the runtime pool, runs one SELECT, and prints. It writes nothing, gates nothing,
// and reaches no actuator.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/diagcorpus"
)

func main() {
	dsn := flag.String("dsn", os.Getenv("TG_RUNTIME_DSN"), "grounder DSN (defaults to $TG_RUNTIME_DSN)")
	expect := flag.String("expect", "core/diagcorpus/expectations.json", "operator-declared diagnosis expectations")
	grace := flag.Duration("grace", 25*time.Minute, "how long after injection a session still counts as inside the fault, when no restore time was recorded")
	flag.Parse()

	if *dsn == "" {
		fmt.Fprintln(os.Stderr, "diagscore: no DSN — set $TG_RUNTIME_DSN or pass -dsn")
		os.Exit(2)
	}
	rs, err := diagcorpus.LoadRuleset(*expect)
	if err != nil {
		fmt.Fprintln(os.Stderr, "diagscore:", err)
		os.Exit(2)
	}
	ctx := context.Background()
	pool, err := db.Connect(ctx, *dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "diagscore: connect:", err)
		os.Exit(1)
	}
	defer pool.Close()

	items, err := diagcorpus.Read(ctx, pool.Pool, *grace)
	if err != nil {
		fmt.Fprintln(os.Stderr, "diagscore:", err)
		os.Exit(1)
	}
	fmt.Print(diagcorpus.Build(items, rs).Render())
}
