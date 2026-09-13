package main

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/entryfile"

	tracker "github.com/territory-grounder/grounder/adapters/tracker"
)

// wireTrackerCreate arms the entry-ticket creator's reconciling pass (TG-490), carved out of main()'s
// composition root (TG-501 LOC-debt paydown). See the comments below for the full rationale. Behaviour is
// unchanged by the move.
func wireTrackerCreate(dbPool *db.Pool, trackersByName map[string]tracker.Tracker, trackerSrcs []string, entryTracker tracker.Tracker) {
	// TG-490: THE ENTRY-TICKET CREATOR — the deterministic write half. A reconciling pass (never
	// inline with session minting) files ONE ticket per recent alert-sourced incident and comments
	// recoveries onto it, all rendered from the durable ingest record (INV-08: no model token on
	// this effect path). DARK unless TG_TRACKER_CREATE_PROJECT is set (config-not-code), and the
	// capability is asserted on the RAW backend module — the wiring decorator forwards the
	// four-verb contract, not optional interfaces. v1 requires exactly ONE configured tracker
	// (creation routing across trackers is a decision nobody has needed yet — dark, loudly).
	if createProject := strings.TrimSpace(getenv("TG_TRACKER_CREATE_PROJECT", "")); createProject != "" && credentialPlane.HoldsTriage() {
		switch {
		case dbPool == nil:
			log.Printf("tracker-create: TG_TRACKER_CREATE_PROJECT=%s set but no database is configured — creator DARK (the durable ledger is the idempotency)", createProject)
		case len(trackersByName) != 1:
			log.Printf("tracker-create: TG_TRACKER_CREATE_PROJECT=%s set but %d trackers are configured — creator DARK (v1 files into exactly one backend)", createProject, len(trackersByName))
		default:
			rawTracker := trackersByName[trackerSrcs[0]]
			creator, ok := rawTracker.(tracker.EntryCreator)
			if !ok {
				log.Printf("tracker-create: TG_TRACKER_CREATE_PROJECT=%s set but tracker %s does not implement entry creation — creator DARK", createProject, trackerSrcs[0])
			} else {
				entryStore := db.NewTrackerEntryStore(dbPool)
				commentTracker := entryTracker // the wrapped four-verb contract; Comment forwards
				go func() {
					t := time.NewTicker(time.Minute)
					defer t.Stop()
					for range t.C {
						tctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
						if n, ferr := entryfile.FileOnce(tctx, entryfile.Config{
							Project: createProject, Window: 30 * time.Minute, Limit: 20,
						}, entryStore, creator); ferr != nil {
							log.Printf("tracker-create: filing pass failed: %v", ferr)
						} else if n > 0 {
							log.Printf("tracker-create: filed %d entry ticket(s) into %s", n, createProject)
						}
						searcher, _ := rawTracker.(tracker.EntrySearcher)
						if n, rerr := entryfile.ResolveReservedOnce(tctx, entryfile.Config{
							Project: createProject, Limit: 10,
						}, entryStore, creator, searcher, 5*time.Minute); rerr != nil {
							log.Printf("tracker-create: resolver pass failed: %v", rerr)
						} else if n > 0 {
							log.Printf("tracker-create: resolved %d stale reservation(s)", n)
						}
						if n, cerr := entryfile.CommentRecoveriesOnce(tctx, entryStore, commentTracker, 20); cerr != nil {
							log.Printf("tracker-create: recovery-comment pass failed: %v", cerr)
						} else if n > 0 {
							log.Printf("tracker-create: commented %d recovery transition(s)", n)
						}
						cancel()
					}
				}()
				log.Printf("tracker-create: ARMED — filing alert-sourced incidents into %s via %s (reconciling pass, 60s cadence, 30m window). "+
					"NOTE: creation is a tracker WRITE — with tracker writes not armed (e.g. TG_YOUTRACK_WRITES unset) every create refuses loudly per pass",
					createProject, trackerSrcs[0])
			}
		}
	}
}
