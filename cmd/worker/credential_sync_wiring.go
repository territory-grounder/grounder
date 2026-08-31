package main

import (
	"context"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/territory-grounder/grounder/core/credential"
	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/modules/bootstrap"
)

// wireCredentialSync arms the credential engine's boot-time SyncAll plus its optional scheduled re-sync
// (TG-109), carved out of main()'s composition root (TG-501 LOC-debt paydown). See the comments below for
// the full rationale. Behaviour is unchanged by the move. Returns the on-demand per-source sync seam
// (nil when no credential source is configured) that the temporal/credentialsync activity holds.
func wireCredentialSync(credEngine *credential.SyncEngine, credSources []bootstrap.RegisteredCredentialSource, credCoverage map[string]int, publishCredentialState func([]credential.SyncRun, []db.CredentialCoverage)) func(ctx context.Context, sourceID string) (credential.SyncRun, error) {
	// Turn the credential engine ON: run an initial read-only SyncAll now (best-effort — a source that is
	// unreachable/denied fails closed and contributes nothing, NEVER fatal, exactly like the estate publish),
	// publish the non-secret coverage + sync state, then optionally re-sync on a schedule
	// (TG_CREDENTIAL_SYNC_INTERVAL, OFF by default like the observability export loop). Mutation stays OFF —
	// this resolves identities read-only; it never actuates.
	// credentialSyncOne is the on-demand per-source sync seam the temporal/credentialsync activity holds
	// (TG-109). nil until the credential engine block below assigns it (no sources ⇒ stays nil ⇒ the
	// activity answers "lane not wired" — a definitive result, not an error).
	var credentialSyncOne func(ctx context.Context, sourceID string) (credential.SyncRun, error)
	if len(credSources) > 0 {
		// The published coverage row now carries each source's compiled precedence (TG-109) so the console
		// can say WHICH source wins a contested target instead of omitting it.
		sourcePrecedence := make(map[string]int, len(credSources))
		for _, rs := range credSources {
			sourcePrecedence[rs.ID] = rs.Precedence
		}
		// credPubMu serializes the coverage recompute + publish between the boot/scheduled sync path and the
		// operator's on-demand Sync-now activity (TG-109) — credCoverage is a plain map and the projection
		// write should not interleave either.
		var credPubMu sync.Mutex
		runCredentialSync := func() {
			sctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			runs, serr := credEngine.SyncAll(sctx)
			if serr != nil {
				log.Printf("credential engine: sync error: %v (prior converged state retained; never actuates)", serr)
			}
			credPubMu.Lock()
			defer credPubMu.Unlock()
			cov := make([]db.CredentialCoverage, 0, len(runs))
			for _, r := range runs {
				credCoverage[r.SourceID] += r.Added - r.Removed
				if credCoverage[r.SourceID] < 0 {
					credCoverage[r.SourceID] = 0 // defensive: coverage can never be negative
				}
				cov = append(cov, db.CredentialCoverage{SourceID: r.SourceID, Plane: r.Plane, Targets: credCoverage[r.SourceID], Precedence: sourcePrecedence[r.SourceID]})
			}
			// A SOURCE THAT SUCCEEDS AND PRODUCES NOTHING MUST SAY SO.
			//
			// Until 2026-08-02 this loop logged only errors, and a source that read an empty subtree was
			// indistinguishable from a converged one: outcome ok, no drift, no error, silence. The
			// deployment's OpenBao source — the HIGHEST precedence of the four — had been contributing
			// zero host bindings since it was configured, so every credential lookup fell through to a
			// lower-precedence source and nothing anywhere said the top of the chain was empty. The count
			// is the fact that distinguishes them, and precedence is printed with it because a starved
			// source at precedence 10 means something quite different from one at 100.
			var starved []string
			for _, r := range runs {
				if r.Starved() {
					starved = append(starved, fmt.Sprintf("%s(plane=%s)", r.SourceID, r.Plane))
				}
			}
			if len(starved) > 0 {
				sort.Strings(starved)
				log.Printf("credential engine: %d configured source(s) synced OK and produced ZERO host "+
					"bindings: %v — they take no part in resolution and every lookup falls through to a "+
					"lower-precedence source. Check each one's configured path/prefix and what its account "+
					"can actually see.", len(starved), starved)
			}
			publishCredentialState(runs, cov)
		}
		// credentialSyncOne is the operator's "Sync now" (TG-109, temporal/credentialsync): re-sync ONE
		// registered source and republish the projection, under the same mutex + timeout discipline as the
		// scheduled path. Assigned to the outer seam the workflow activity holds.
		credentialSyncOne = func(ctx context.Context, sourceID string) (credential.SyncRun, error) {
			sctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			defer cancel()
			run, err := credEngine.Sync(sctx, sourceID)
			if err != nil {
				return run, err
			}
			credPubMu.Lock()
			defer credPubMu.Unlock()
			credCoverage[run.SourceID] += run.Added - run.Removed
			if credCoverage[run.SourceID] < 0 {
				credCoverage[run.SourceID] = 0
			}
			publishCredentialState([]credential.SyncRun{run},
				[]db.CredentialCoverage{{SourceID: run.SourceID, Plane: run.Plane, Targets: credCoverage[run.SourceID], Precedence: sourcePrecedence[run.SourceID]}})
			return run, nil
		}
		runCredentialSync() // initial sync at boot
		if iv := getenv("TG_CREDENTIAL_SYNC_INTERVAL", ""); iv != "" {
			if d, derr := time.ParseDuration(iv); derr == nil && d > 0 {
				go func() {
					t := time.NewTicker(d)
					defer t.Stop()
					for range t.C {
						runCredentialSync()
					}
				}()
				log.Printf("credential engine: scheduled re-sync every %s (read-only; never actuates)", d)
			} else {
				log.Printf("credential engine: invalid TG_CREDENTIAL_SYNC_INTERVAL %q — scheduled sync disabled (the initial sync still ran)", iv)
			}
		} else {
			log.Printf("credential engine: scheduled re-sync disabled (TG_CREDENTIAL_SYNC_INTERVAL unset) — synced once at boot")
		}
	}
	return credentialSyncOne
}
