// Command forensic reconstructs a CROSS-INCIDENT timeline (TG-168) from the corpora TG already writes — the
// governance ledger, ingest alerts, agent steps, credential resolutions, exec-class decisions — over a
// bounded window, optionally scoped to one host. It is the offline, read-only CONSUMER the forensic timeline
// layer (core/forensic + core/db.ForensicStore) never had: the deterministic reconstruction was fully built
// and unit-tested, but nothing in any binary called it (measured: zero consumers) — so an operator had no way
// to actually RUN one. Present, not reaching.
//
// Deterministic and read-only: no model, no IOC extraction, no decoy-vs-real separation — those are TG-168's
// later, model-driven slice. This is the "buildable without the model" half the ticket's own 2026-08-06
// comment names, made usable.
//
// Usage (on a host that reaches the grounder DB, e.g. dc1tg01):
//
//	TG_RUNTIME_DSN=postgres://… forensic -since 72h
//	forensic -since 2026-08-06T00:00:00Z -until 2026-08-07T00:00:00Z -host dc1pve03   # the pve03 cascade
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/territory-grounder/grounder/adapters/model"
	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/forensic"
)

// parseSince accepts either an RFC3339 instant or a Go duration meaning "that long before now" (e.g. 72h), so
// an operator can say `-since 72h` without computing a timestamp. now is a parameter for testability.
func parseSince(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, fmt.Errorf("empty (need an RFC3339 instant or a duration like 72h)")
	}
	if d, err := time.ParseDuration(s); err == nil {
		return now.Add(-d), nil
	}
	return time.Parse(time.RFC3339, s)
}

// renderTimeline writes the ordered narrative plus the honesty about what it did NOT return — a partial
// reconstruction must never read as a complete one (the Truncated / Dropped footers exist for exactly that).
func renderTimeline(out io.Writer, w forensic.Window, host string, r db.ForensicRead) {
	to := "now"
	if !w.To.IsZero() {
		to = w.To.UTC().Format(time.RFC3339)
	}
	scope := "(all hosts)"
	if host != "" {
		scope = host
	}
	fmt.Fprintf(out, "# forensic timeline  window=[%s, %s)  host=%s  events=%d\n",
		w.From.UTC().Format(time.RFC3339), to, scope, len(r.Events))
	if hs := forensic.Hosts(r.Events); len(hs) > 0 {
		fmt.Fprintf(out, "# blast radius: %d host(s): %s\n", len(hs), strings.Join(hs, " "))
	}
	for _, e := range r.Events {
		h, ref := e.Host, e.SubjectRef
		if h == "" {
			h = "-"
		}
		if ref == "" {
			ref = "-"
		}
		fmt.Fprintf(out, "%s  %-22s host=%-26s ref=%-18s %s: %s\n",
			e.At.UTC().Format(time.RFC3339), e.Source, h, ref, e.Kind, e.Detail)
	}
	if len(r.Truncated) > 0 {
		fmt.Fprintf(out, "# INCOMPLETE: %d corpus/corpora hit the per-corpus cap — narrow the window or raise -cap: %s\n",
			len(r.Truncated), strings.Join(r.Truncated, " "))
	}
	if r.Dropped > 0 {
		fmt.Fprintf(out, "# %d event(s) dropped: no usable timestamp, so unplaceable on a timeline\n", r.Dropped)
	}
}

func main() {
	since := flag.String("since", "", "window start: an RFC3339 instant, or a duration before now (e.g. 72h)")
	until := flag.String("until", "", "window end (RFC3339); empty ⇒ up to now")
	host := flag.String("host", "", "scope to one estate host (matches ingest_alert.host); empty ⇒ all hosts")
	perCorpusCap := flag.Int("cap", 0, "max events per corpus (0 ⇒ default 2000) — bounds the chattiest lane")
	dsn := flag.String("dsn", os.Getenv("TG_RUNTIME_DSN"), "grounder DSN (defaults to $TG_RUNTIME_DSN)")
	analyze := flag.Bool("analyze", false, "after the timeline, run the forensic IR model over it (needs a live forensic lane: TG_LITELLM_URL + the litellm key)")
	forensicModel := flag.String("model", os.Getenv("TG_FORENSIC_MODEL"), "the forensic IR model alias (litellm); required with -analyze")
	flag.Parse()

	from, err := parseSince(*since, time.Now().UTC())
	if err != nil {
		fmt.Fprintf(os.Stderr, "forensic: -since: %v\n", err)
		os.Exit(2)
	}
	var to time.Time
	if s := strings.TrimSpace(*until); s != "" {
		if to, err = time.Parse(time.RFC3339, s); err != nil {
			fmt.Fprintf(os.Stderr, "forensic: -until must be RFC3339: %v\n", err)
			os.Exit(2)
		}
	}
	if *dsn == "" {
		fmt.Fprintln(os.Stderr, "forensic: no DSN — set $TG_RUNTIME_DSN or pass -dsn")
		os.Exit(2)
	}
	w := forensic.Window{From: from, To: to}
	if !w.Valid() {
		fmt.Fprintln(os.Stderr, "forensic: invalid window — need a bounded [from,to) with from<to; an unbounded reconstruction is a dump, not an answer")
		os.Exit(2)
	}

	ctx := context.Background()
	pool, err := db.Connect(ctx, *dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "forensic: connect: %v\n", err)
		os.Exit(1)
	}
	defer pool.Close()

	read, err := db.NewForensicStore(pool).Window(ctx, w, strings.TrimSpace(*host), *perCorpusCap)
	if err != nil {
		fmt.Fprintf(os.Stderr, "forensic: window read: %v\n", err)
		os.Exit(1)
	}
	renderTimeline(os.Stdout, w, strings.TrimSpace(*host), read)

	// TG-168 part 2: optionally run the IR model over the reconstructed window. Build-ahead — the timeline
	// above is the always-available deterministic answer; -analyze layers the model's IOC/decoy/credentials
	// account on top, and needs a forensic model lane to be armed.
	if *analyze {
		if strings.TrimSpace(*forensicModel) == "" {
			fmt.Fprintln(os.Stderr, "forensic: -analyze needs -model (or $TG_FORENSIC_MODEL) — the forensic IR model alias")
			os.Exit(2)
		}
		gw := model.NewGateway(
			getenvOr("TG_LITELLM_URL", "http://litellm:4000"),
			config.SecretRef(getenvOr("TG_LITELLM_KEY_REF", "env:LITELLM_MASTER_KEY")),
		)
		f, err := analyzeTimeline(ctx, gw, strings.TrimSpace(*forensicModel), "forensic-cli", w, strings.TrimSpace(*host), read)
		if err != nil {
			fmt.Fprintf(os.Stderr, "forensic: analysis: %v\n", err)
			os.Exit(1)
		}
		renderFindings(os.Stdout, f)
	}
}

// getenvOr is os.Getenv with a default for an unset/empty var.
func getenvOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
