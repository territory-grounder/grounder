// corpusdate backfills `resolved_at` onto an existing precedent corpus from a ref→timestamp map.
//
// WHY IT EXISTS. `knowledge.Incident` gained a ResolvedAt field so retrieval could score and disclose a
// precedent's age (MECH-105/107). Both mechanisms are INERT on the deployed corpus, because every row
// there was written by code that dropped the timestamp: 0 of 670 carry a date, so the agent currently
// sees "[age unknown]" on every precedent. That rendering is honest, and it is also a standing invitation
// to fix the data rather than the message.
//
// WHAT IT WILL AND WILL NOT DO. It sets ResolvedAt on rows whose ExternalRef appears in the supplied map,
// and it does nothing else: no field is added, removed, reordered or rewritten, no row is created or
// dropped, and a ref absent from the map keeps its zero timestamp rather than receiving a guess. A corpus
// that cannot date a row must keep saying so — an invented date is invented evidence, and it would feed
// straight into a ranking signal.
//
// It is DETERMINISTIC and re-runnable: running it twice with the same inputs produces byte-identical
// output, and running it against an already-backfilled corpus is a no-op.
//
// The timestamp map is supplied as a file rather than read from a database on purpose. The tool then has
// no credentials, no network, and no opinion about where truth lives; the operator produces the map with
// a query they can read, and the diff they apply is reviewable before it touches anything.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/territory-grounder/grounder/core/knowledge"
)

func main() {
	corpusPath := flag.String("corpus", "", "path to the corpus JSON to backfill (required)")
	datesPath := flag.String("dates", "", "path to a `ref|RFC3339` map, one per line (required)")
	out := flag.String("out", "", "where to write the result; empty means stdout (never in-place)")
	dry := flag.Bool("dry-run", false, "report what would change and exit without writing")
	flag.Parse()

	if *corpusPath == "" || *datesPath == "" {
		fmt.Fprintln(os.Stderr, "corpusdate: -corpus and -dates are both required")
		os.Exit(2)
	}
	corpus, err := readCorpus(*corpusPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "corpusdate: %v\n", err)
		os.Exit(1)
	}
	dates, err := readDates(*datesPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "corpusdate: %v\n", err)
		os.Exit(1)
	}

	backfilled, stats := Backfill(corpus, dates)
	fmt.Fprintf(os.Stderr, "corpusdate: %d row(s); %d dated, %d already dated, %d left undated (no timestamp supplied)\n",
		stats.Total, stats.Dated, stats.AlreadyDated, stats.Undated)
	if stats.Unmatched > 0 {
		fmt.Fprintf(os.Stderr, "corpusdate: %d supplied timestamp(s) matched no corpus row (ignored)\n", stats.Unmatched)
	}
	if *dry {
		return
	}

	blob, err := json.MarshalIndent(backfilled, "", " ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "corpusdate: encode: %v\n", err)
		os.Exit(1)
	}
	blob = append(blob, '\n')
	if *out == "" {
		os.Stdout.Write(blob)
		return
	}
	// Written to a NEW path, never in place: the operator keeps the original until they have compared them.
	if err := os.WriteFile(*out, blob, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "corpusdate: write %s: %v\n", *out, err)
		os.Exit(1)
	}
}

// Stats is what the backfill did, reported so an operator can sanity-check the diff before applying it.
type Stats struct {
	Total, Dated, AlreadyDated, Undated, Unmatched int
}

// Backfill stamps ResolvedAt from the map and changes nothing else.
//
// It returns a COPY: the input slice is untouched, so a caller that also holds the original can diff the
// two. Rows are emitted in their input order — a corpus file whose row order changed would produce a
// diff nobody can read, and reordering is exactly the kind of silent change this tool must not make.
func Backfill(corpus []knowledge.Incident, dates map[string]time.Time) ([]knowledge.Incident, Stats) {
	out := make([]knowledge.Incident, len(corpus))
	copy(out, corpus)
	st := Stats{Total: len(corpus)}
	used := map[string]struct{}{}

	for i := range out {
		ref := strings.TrimSpace(out[i].ExternalRef)
		switch {
		case !out[i].ResolvedAt.IsZero():
			st.AlreadyDated++
			if _, ok := dates[ref]; ok {
				used[ref] = struct{}{}
			}
		default:
			ts, ok := dates[ref]
			if !ok || ts.IsZero() {
				// NO GUESS. A row with no supplied timestamp keeps its zero value and keeps rendering
				// "[age unknown]" — which is true, and is the only honest thing to show.
				st.Undated++
				continue
			}
			out[i].ResolvedAt = ts.UTC()
			st.Dated++
			used[ref] = struct{}{}
		}
	}
	for ref := range dates {
		if _, ok := used[ref]; !ok {
			st.Unmatched++
		}
	}
	return out, st
}

func readCorpus(path string) ([]knowledge.Incident, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open corpus: %w", err)
	}
	defer f.Close()
	var corpus []knowledge.Incident
	if err := json.NewDecoder(f).Decode(&corpus); err != nil {
		return nil, fmt.Errorf("parse corpus %s: %w", path, err)
	}
	return corpus, nil
}

// readDates parses `ref|RFC3339` lines. A malformed line is a hard error, never a skipped row: silently
// dropping a date would produce a partial backfill that looks complete.
func readDates(path string) (map[string]time.Time, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("open dates: %w", err)
	}
	out := map[string]time.Time{}
	for n, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		ref, stamp, ok := strings.Cut(line, "|")
		if !ok {
			return nil, fmt.Errorf("dates line %d: want `ref|RFC3339`, got %q", n+1, line)
		}
		ts, perr := time.Parse(time.RFC3339, strings.TrimSpace(stamp))
		if perr != nil {
			return nil, fmt.Errorf("dates line %d: %w", n+1, perr)
		}
		out[strings.TrimSpace(ref)] = ts
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("dates file %s yielded no entries", path)
	}
	return out, nil
}

// sortedRefs is used by the oracles to compare corpora deterministically.
func sortedRefs(c []knowledge.Incident) []string {
	refs := make([]string, 0, len(c))
	for _, i := range c {
		refs = append(refs, i.ExternalRef)
	}
	sort.Strings(refs)
	return refs
}
