// Command forensictimeline reconstructs a CROSS-INCIDENT narrative from TG's own corpora (TG-168).
//
// TG has had per-incident reconstruction for a while — core/trace.Assemble joins eleven corpora for one
// external_ref and the console renders it. What it has never had is the question an operator actually
// asks after something happens: "what went on between 02:00 and 05:00?" Nothing in the tree took a
// window. This does.
//
// It is DETERMINISTIC and MODEL-FREE by design. Same window, same output, every run — because a
// reconstruction that renders differently on two runs cannot be compared with yesterday's, and an
// operator would be reading map-iteration artefacts as changes in the estate. No inference is involved:
// this is the deterministic corpus assembly TG-168 identifies as "what any model would consume anyway".
//
// Usage:
//
//	forensictimeline -from 2026-08-06T02:00:00Z -to 2026-08-06T05:00:00Z
//	forensictimeline -from 2026-08-06T02:00:00Z -host dc1pve03 -format jsonl
//	TG_RUNTIME_DSN=... forensictimeline -since 3h
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/forensic"
)

func main() {
	dsn := flag.String("dsn", os.Getenv("TG_RUNTIME_DSN"), "postgres DSN (default $TG_RUNTIME_DSN)")
	from := flag.String("from", "", "window start, RFC3339 (e.g. 2026-08-06T02:00:00Z)")
	to := flag.String("to", "", "window end, RFC3339; omit for 'everything since -from'")
	since := flag.String("since", "", "shorthand for -from now-D (e.g. 3h, 90m); ignored if -from is set")
	host := flag.String("host", "", "restrict to one estate host (lanes without a host column are unfiltered)")
	format := flag.String("format", "text", "text | jsonl")
	limit := flag.Int("cap", db.DefaultForensicCapPerCorpus, "max rows PER CORPUS")
	flag.Parse()

	if *dsn == "" {
		die(2, "no DSN — set $TG_RUNTIME_DSN or pass -dsn")
	}
	w, err := window(*from, *to, *since)
	if err != nil {
		die(2, err.Error())
	}

	ctx := context.Background()
	pool, err := db.Connect(ctx, *dsn)
	if err != nil {
		die(1, "connect: "+err.Error())
	}
	defer pool.Close()

	read, err := db.NewForensicStore(pool).Window(ctx, w, strings.TrimSpace(*host), *limit)
	if err != nil {
		die(1, err.Error())
	}

	switch *format {
	case "jsonl":
		enc := json.NewEncoder(os.Stdout)
		for _, e := range read.Events {
			_ = enc.Encode(e)
		}
	default:
		renderText(read, w, *host)
	}

	// THE CAVEATS GO TO STDERR, ALWAYS, INCLUDING WHEN THERE ARE NONE.
	//
	// A narrative that was silently truncated and a complete one are the same text, and this tool exists
	// so an operator can trust both position and completeness. Printing the caveat line unconditionally
	// means "0 dropped, nothing truncated" is a POSITIVE statement rather than the absence of a warning —
	// which is the difference between a clean reconstruction and one whose warning path is broken.
	fmt.Fprintf(os.Stderr, "\n-- %d event(s); %d undated event(s) dropped; truncated lanes: %s\n",
		len(read.Events), read.Dropped, truncatedList(read.Truncated))
	if len(read.Truncated) > 0 {
		fmt.Fprintf(os.Stderr, "-- a truncated lane means this window is INCOMPLETE for that corpus; "+
			"narrow the window or raise -cap (currently %d per corpus) before drawing conclusions from it\n", *limit)
	}
}

func truncatedList(t []string) string {
	if len(t) == 0 {
		return "none"
	}
	return strings.Join(t, ", ")
}

// window resolves the flags into a bounded window, refusing rather than guessing.
func window(from, to, since string) (forensic.Window, error) {
	var w forensic.Window
	switch {
	case strings.TrimSpace(from) != "":
		t, err := time.Parse(time.RFC3339, from)
		if err != nil {
			return w, fmt.Errorf("-from %q is not RFC3339: %v", from, err)
		}
		w.From = t
	case strings.TrimSpace(since) != "":
		d, err := time.ParseDuration(since)
		if err != nil || d <= 0 {
			return w, fmt.Errorf("-since %q is not a positive duration", since)
		}
		w.From = time.Now().UTC().Add(-d)
	default:
		// REFUSED, not defaulted. An unbounded reconstruction over ~9,700 ledger rows and ~18,700 agent
		// steps is a data dump, and silently substituting "the last hour" would answer a question the
		// operator did not ask — in an incident, at the moment they are least able to notice.
		return w, fmt.Errorf("no window: pass -from (RFC3339) or -since (duration). This tool will not " +
			"guess a window — an unbounded read is a dump, and a defaulted one answers a different question")
	}
	if strings.TrimSpace(to) != "" {
		t, err := time.Parse(time.RFC3339, to)
		if err != nil {
			return w, fmt.Errorf("-to %q is not RFC3339: %v", to, err)
		}
		w.To = t
	}
	if !w.Valid() {
		return w, fmt.Errorf("window [%s, %s) is not valid — To must be after From",
			w.From.Format(time.RFC3339), w.To.Format(time.RFC3339))
	}
	return w, nil
}

func renderText(read db.ForensicRead, w forensic.Window, host string) {
	scope := "the whole estate"
	if host != "" {
		scope = host
	}
	end := "now"
	if !w.To.IsZero() {
		end = w.To.Format(time.RFC3339)
	}
	fmt.Printf("# %s → %s · %s\n", w.From.Format(time.RFC3339), end, scope)
	if hosts := forensic.Hosts(read.Events); len(hosts) > 0 {
		// The blast-radius line an operator reads first: which machines appear at all.
		fmt.Printf("# hosts appearing: %s\n", strings.Join(hosts, " "))
	}
	fmt.Println()
	for _, e := range read.Events {
		line := fmt.Sprintf("%s  %-22s %-24s %s",
			e.At.UTC().Format("2006-01-02 15:04:05"), e.Source, trunc(e.Kind, 24), trunc(e.Host, 20))
		if e.Actor != "" {
			line += "  actor=" + e.Actor
		}
		if e.SubjectRef != "" {
			line += "  ref=" + trunc(e.SubjectRef, 34)
		}
		if e.Detail != "" {
			line += "\n        " + trunc(e.Detail, 150)
		}
		fmt.Println(line)
	}
}

func trunc(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func die(code int, msg string) {
	fmt.Fprintln(os.Stderr, "forensictimeline:", msg)
	os.Exit(code)
}
