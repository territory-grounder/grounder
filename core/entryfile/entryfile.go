// Package entryfile is TG-490's deterministic entry-ticket writer: the reconciling pass that
// files ONE tracker ticket per alert-sourced incident and comments recoveries onto it — the
// write half the incumbent gateway does agent-side and TG deliberately does not (INV-08: every
// word here is rendered from the durable ingest record; no model token reaches this effect path).
//
// The pass is RECONCILING, not inline with session minting: it scans TG's own durable intake
// (ingest_alert) for recent incidents lacking a tracker_entry row, files each exactly once (the
// row's PK is the idempotency), and advances a per-ticket cursor over recovery transitions. One
// code path covers every source (poller, push front door, liveness, authlog), a tracker outage
// never blocks triage (the incumbent's lock problem), and the feature is DARK unless the worker
// is configured with a create project (config-not-code).
package entryfile

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/territory-grounder/grounder/adapters/tracker"
	"github.com/territory-grounder/grounder/core/db"
)

// Store is the durable half (satisfied by *db.TrackerEntryStore).
type Store interface {
	Unfiled(ctx context.Context, window time.Duration, limit int) ([]db.UnfiledAlert, error)
	Reserve(ctx context.Context, externalRef, project, sourceType string) (bool, error)
	Complete(ctx context.Context, externalRef, issueID string) (won bool, existing string, err error)
	StaleReserved(ctx context.Context, age time.Duration, limit int) ([]db.StaleReservation, error)
	RecoveriesToComment(ctx context.Context, limit int) ([]db.RecoveryToComment, error)
	MarkCommented(ctx context.Context, externalRef string, transitionID int64) error
}

// Commenter is the tracker's comment verb (a subset of tracker.Tracker).
type Commenter interface {
	Comment(ctx context.Context, id, body string) error
}

// Config bounds one pass.
type Config struct {
	Project string        // the tracker project entries are filed into (REQUIRED — empty means the feature is dark)
	Window  time.Duration // how far back the unfiled scan reaches (the arming moment is the epoch)
	Limit   int           // per-pass work bound
}

// RenderEntry renders the filed ticket's summary and description from the ingest record — pure,
// deterministic data (INV-08). The external_ref rides in the body so a human (and TG's own
// entry-tracker seam) can correlate ticket ↔ session ↔ logs in either direction.
func RenderEntry(u db.UnfiledAlert) (summary, description string) {
	sev := strings.TrimSpace(u.Severity)
	if sev == "" {
		sev = "alert"
	}
	host := strings.TrimSpace(u.Host)
	if host == "" {
		host = "(no host)"
	}
	rule := strings.TrimSpace(u.AlertRule)
	if rule == "" {
		rule = "(no rule)"
	}
	summary = fmt.Sprintf("[%s] %s: %s", sev, host, rule)
	var b strings.Builder
	fmt.Fprintf(&b, "Automated entry ticket filed by Territory Grounder (deterministic — rendered from the ingest record, no model authorship).\n\n")
	fmt.Fprintf(&b, "incident: %s\nsource: %s\nhost: %s\nsite: %s\nrule: %s\nseverity: %s\nfirst seen: %s\n",
		u.ExternalRef, orDash(u.SourceType), host, orDash(u.Site), rule, sev, u.ReceivedAt.UTC().Format(time.RFC3339))
	if s := strings.TrimSpace(u.Summary); s != "" {
		fmt.Fprintf(&b, "\nprovider summary:\n%s\n", s)
	}
	fmt.Fprintf(&b, "\nTG's triage session for this incident shares the incident key above; its outcome lands here as the terminal audit comment.\n")
	return summary, b.String()
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

// FileOnce runs one filing pass, TWO-PHASE (the fresh-eyes finding-#1 fix): RESERVE the incident
// durably (removing it from every future Unfiled scan — no blind second create can ever fire),
// then create, then COMPLETE the reservation with the ticket id. The crash window between create
// and complete leaves a VISIBLE stale reservation — ResolveReservedOnce settles it by searching
// the project for the incident key the ticket body always carries (adopt-or-create). A create
// failure leaves the reservation for the resolver too (same path, one discipline). Ticket
// creation against a remote tracker is at-least-once BY NATURE; this makes every not-exactly-once
// outcome observable and self-healing instead of a silent orphan.
func FileOnce(ctx context.Context, cfg Config, st Store, cr tracker.EntryCreator) (filed int, err error) {
	if strings.TrimSpace(cfg.Project) == "" {
		return 0, fmt.Errorf("entryfile: no project configured — the pass must not run dark-armed")
	}
	unfiled, err := st.Unfiled(ctx, cfg.Window, cfg.Limit)
	if err != nil {
		return 0, err
	}
	for _, u := range unfiled {
		mine, rerr := st.Reserve(ctx, u.ExternalRef, cfg.Project, u.SourceType)
		if rerr != nil {
			log.Printf("entryfile: reserve for %s failed (retry next pass): %v", u.ExternalRef, rerr)
			continue
		}
		if !mine {
			continue // another attempt (live or crashed) holds it; the resolver owns stale ones
		}
		summary, desc := RenderEntry(u)
		issue, cerr := cr.CreateEntry(ctx, cfg.Project, summary, desc)
		if cerr != nil {
			log.Printf("entryfile: create for %s failed — reservation stays for the resolver: %v", u.ExternalRef, cerr)
			continue
		}
		if won, existing, xerr := st.Complete(ctx, u.ExternalRef, issue.ID); xerr != nil {
			log.Printf("entryfile: ORPHAN-RISK — created %s for %s but completing the reservation failed (the resolver will adopt it by search): %v",
				issue.ID, u.ExternalRef, xerr)
			continue
		} else if !won {
			log.Printf("entryfile: %s lost the completion race to %s for %s — duplicate visible in-project", issue.ID, existing, u.ExternalRef)
			continue
		}
		filed++
	}
	return filed, nil
}

// ResolveReservedOnce settles stale reservations — the crash leftovers. With a searcher: find the
// project's ticket(s) carrying the incident key; adopt the newest (completing the reservation);
// none found → create-and-complete. Without a searcher, or on a search ERROR, it does NOT create
// (an unanswerable adopt-question must never mint a possible duplicate) — the reservation ages
// loudly until the searcher answers.
func ResolveReservedOnce(ctx context.Context, cfg Config, st Store, cr tracker.EntryCreator, sr tracker.EntrySearcher, age time.Duration) (resolved int, err error) {
	if strings.TrimSpace(cfg.Project) == "" {
		return 0, fmt.Errorf("entryfile: no project configured — the resolver must not run dark-armed")
	}
	stale, err := st.StaleReserved(ctx, age, cfg.Limit)
	if err != nil {
		return 0, err
	}
	for _, e := range stale {
		if sr == nil {
			log.Printf("entryfile: stale reservation %s and no search capability — cannot answer adopt-vs-create, holding", e.ExternalRef)
			continue
		}
		found, serr := sr.SearchEntry(ctx, e.Project, e.ExternalRef)
		if serr != nil {
			log.Printf("entryfile: stale reservation %s — search failed, holding (never create on an unanswered adopt-question): %v", e.ExternalRef, serr)
			continue
		}
		var issueID string
		if len(found) > 0 {
			issueID = found[0].ID
			if len(found) > 1 {
				log.Printf("entryfile: %s has %d tickets in %s — adopting %s; the extras are visible in-project", e.ExternalRef, len(found), e.Project, issueID)
			}
		} else {
			// Provably none exist: the crash happened BEFORE the create. Safe to create now — and
			// with the FULL render inputs (StaleReserved re-joins the durable ingest record), so a
			// resolver-created ticket is a real ticket, never an information-lossy placeholder.
			summary, desc := RenderEntry(e.Alert)
			issue, cerr := cr.CreateEntry(ctx, e.Project, summary, desc)
			if cerr != nil {
				log.Printf("entryfile: resolver create for %s failed (retry next pass): %v", e.ExternalRef, cerr)
				continue
			}
			issueID = issue.ID
		}
		if _, _, xerr := st.Complete(ctx, e.ExternalRef, issueID); xerr != nil {
			log.Printf("entryfile: resolver completion for %s → %s failed (retry next pass): %v", e.ExternalRef, issueID, xerr)
			continue
		}
		resolved++
	}
	return resolved, nil
}

// CommentRecoveriesOnce runs one recovery-comment pass: each recovery transition newer than its
// ticket's cursor gets one comment, and the cursor advances only after the comment lands (a
// failed comment is retried next pass; the monotone cursor makes duplicates structurally
// impossible in the other direction).
func CommentRecoveriesOnce(ctx context.Context, st Store, cm Commenter, limit int) (commented int, err error) {
	recs, err := st.RecoveriesToComment(ctx, limit)
	if err != nil {
		return 0, err
	}
	for _, r := range recs {
		at := r.ReceivedAt
		if r.ObservedAt != nil {
			at = *r.ObservedAt
		}
		body := fmt.Sprintf("Provider recovery captured for %s (%s) at %s. (Automated lifecycle comment — TG's durable transition record, not a model statement.)",
			strings.TrimSpace(r.Host), strings.TrimSpace(r.AlertRule), at.UTC().Format(time.RFC3339))
		if cerr := cm.Comment(ctx, r.IssueID, body); cerr != nil {
			log.Printf("entryfile: recovery comment on %s (%s) failed (retry next pass): %v", r.IssueID, r.ExternalRef, cerr)
			continue
		}
		if merr := st.MarkCommented(ctx, r.ExternalRef, r.TransitionID); merr != nil {
			log.Printf("entryfile: cursor advance for %s@%d failed: %v", r.ExternalRef, r.TransitionID, merr)
			continue
		}
		commented++
	}
	return commented, nil
}
