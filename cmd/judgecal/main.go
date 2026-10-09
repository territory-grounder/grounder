// Command judgecal calibrates the LLM JUDGE against ground truth.
//
// P5's exit criterion asks for judge TPR/TNR >= 0.70 on >=100 labelled items with kappa reported. The roadmap
// treated that labelling as the phase's schedule bottleneck and paired it with a separate calibration harness
// (P5-3) whose own data source is dead — the scorecard is gitignored and the trajectory card persists only
// under --dry-run. Both fall away for the same reason: the fault injector already recorded WHAT it broke, so
// the label exists and the two items are one deliverable.
//
// THE POPULATION IS FAULTS THAT RAISED EXACTLY ONE SESSION, and the reason is a measurement problem rather
// than a preference. The corpus's unit of truth is the FAULT; the judge scores a SESSION. Those are only
// commensurable when a fault produced one session, and on this estate most did not: a single stopped guest
// trips FOUR distinct LibreNMS rules (Device-Down, Devices-up/down, Device-Down-Due-to-no-ICMP-response,
// Device-Down-SNMP-unreachable), each spawning its own session, and TG need only act on one of them.
//
// Scoring every session against the fault's truth therefore punishes TG for the sibling sessions that
// correctly observed an already-healed host — the same error that made device-down read 73.7% instead of
// 89.7%. Taking only the FIRST session is no better: the first alert to fire is often the weakest signal
// (SNMP unreachable before ICMP confirms), so standing down there can be entirely correct. Measured, that
// heuristic put TG's "first session" accuracy at 82/202 while its true per-fault accuracy is 82%.
//
// Faults with exactly one session have no sibling to disagree with, so session-truth and fault-truth
// coincide. Live: 141 such faults, 95 of them judged. That is the honest denominator, and it is stated
// rather than padded to clear the >=100 bar.
//
// Read-only: two SELECTs, print, exit.
//
// Provenance: [O] spec/025 REQ-2502 (a rate is reported with its denominator) · P5-3 + P5-5.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/territory-grounder/grounder/core/calibrate"
	"github.com/territory-grounder/grounder/core/diagcorpus"
)

func main() {
	dsn := flag.String("dsn", os.Getenv("TG_RUNTIME_DSN"), "grounder DSN (defaults to $TG_RUNTIME_DSN)")
	expect := flag.String("expect", "core/diagcorpus/expectations.json", "operator-declared diagnosis expectations")
	grace := flag.Duration("grace", 25*time.Minute, "how long after injection a session still counts as inside the fault, when no restore time was recorded")
	dim := flag.String("dimension", "correct_diagnosis", "the judged dimension to calibrate")
	cut := flag.Int("cut", 4, "judge scores >= this count as a POSITIVE call (1-5 scale)")
	bar := flag.Float64("bar", 0.70, "the TPR/TNR bar, read at the interval's LOWER bound")
	truth := flag.String("truth", "action", "the ground-truth ORACLE: 'action' (did TG propose an ACCEPTED "+
		"op-class — expectations.json, the pre-existing action-POLICY oracle) or 'diagnosis' (did the "+
		"diagnosis NAME the injected mechanism — the commensurable oracle for the correct_diagnosis QUALITY "+
		"dimension; TG-542, the fix for the construct mismatch that read as TNR 0.060)")
	jsonOut := flag.String("json-out", "", "also write the full report as JSON to this path — the committed "+
		"§5.4 release-gate artifact (eval/history/<date>-judgecal/calibration.json); the release gate reads "+
		"the newest committed record instead of a live DB by design")
	flag.Parse()

	ctx := context.Background()
	rs, err := diagcorpus.LoadRuleset(*expect)
	if err != nil {
		fail(err)
	}
	pool, err := pgxpool.New(ctx, *dsn)
	if err != nil {
		fail(err)
	}
	defer pool.Close()

	items, err := diagcorpus.Read(ctx, pool, *grace)
	if err != nil {
		fail(err)
	}
	judged, err := readJudgeCalls(ctx, pool, *dim, *cut)
	if err != nil {
		fail(err)
	}

	first := soleSessionFaults(items)
	var out []calibrate.Outcome
	switch *truth {
	case "action":
		out = diagcorpus.JudgeOutcomes(first, rs, judged)
	case "diagnosis":
		out = diagcorpus.JudgeDiagnosisOutcomes(first, rs, judged)
	default:
		fail(fmt.Errorf("judgecal: unknown -truth %q (want action|diagnosis)", *truth))
	}
	rep := calibrate.Calibrate(out, *bar)

	fmt.Printf("JUDGE CALIBRATION — dimension %q vs %q ground truth, positive = score >= %d, bar %.2f at the LOWER bound\n", *dim, *truth, *cut, *bar)
	fmt.Printf("  population: %d fault(s) raising EXACTLY ONE session -> %d also judged\n", len(first), rep.Confusion.N())
	fmt.Printf("  confusion: TP=%d FP=%d TN=%d FN=%d\n", rep.Confusion.TP, rep.Confusion.FP, rep.Confusion.TN, rep.Confusion.FN)
	line("TPR (sensitivity)", rep.TPR)
	line("TNR (specificity)", rep.TNR)
	line("precision", rep.Precision)
	line("accuracy", rep.Accuracy)
	line("Cohen's kappa", rep.Kappa)
	if rep.MeetsBar {
		fmt.Printf("  VERDICT: MEETS THE BAR — both TPR and TNR clear %.2f at their lower bound.\n", *bar)
	} else {
		fmt.Printf("  VERDICT: does NOT meet the bar — %s\n", rep.BarReason)
	}
	fmt.Println("  NOTE: ground truth is the injector's record. Only faults that raised EXACTLY ONE session are")
	fmt.Println("        scored: the corpus's unit is the FAULT and the judge's is the SESSION, and those coincide")
	fmt.Println("        only when there is no sibling session to disagree with. One stopped guest trips FOUR")
	fmt.Println("        LibreNMS rules on this estate, so most faults are excluded here by construction.")

	if *jsonOut != "" {
		if err := writeArtifact(*jsonOut, *dim, *truth, *cut, *bar, len(first), rep); err != nil {
			fail(err)
		}
		fmt.Printf("  wrote %s — commit it under eval/history/ for the §5.4 release-gate leg\n", *jsonOut)
	}
}

// calibrationArtifact is the committed §5.4 record. Every rate carries its n (REQ-2502: a rate is reported
// with its denominator); meets_bar is the LOWER-bound claim, and bar_reason makes a false diagnosable —
// including the n=0 "UNCALIBRATED, which is not the same as failing" state, which the release gate must
// surface as its own condition rather than a pass or a plain fail.
type calibrationArtifact struct {
	GeneratedAt string         `json:"generated_at"`
	Dimension   string         `json:"dimension"`
	Truth       string         `json:"truth"` // the ground-truth ORACLE ('action'|'diagnosis') this record calibrated against — so the §5.4 leg self-documents its construct rather than looking identical to a run against the other oracle (TG-542)
	PositiveCut int            `json:"positive_cut"`
	Bar         float64        `json:"bar"`
	SolePopN    int            `json:"sole_session_faults"`
	Confusion   map[string]int `json:"confusion"`
	TPR         calibrate.Rate `json:"tpr"`
	TNR         calibrate.Rate `json:"tnr"`
	Precision   calibrate.Rate `json:"precision"`
	Accuracy    calibrate.Rate `json:"accuracy"`
	Kappa       calibrate.Rate `json:"kappa"`
	MeetsBar    bool           `json:"meets_bar"`
	BarReason   string         `json:"bar_reason,omitempty"`
}

func writeArtifact(path, dim, truth string, cut int, bar float64, solePop int, rep calibrate.Report) error {
	a := calibrationArtifact{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Dimension:   dim,
		Truth:       truth,
		PositiveCut: cut,
		Bar:         bar,
		SolePopN:    solePop,
		Confusion: map[string]int{
			"tp": rep.Confusion.TP, "fp": rep.Confusion.FP,
			"tn": rep.Confusion.TN, "fn": rep.Confusion.FN,
		},
		TPR: rep.TPR, TNR: rep.TNR, Precision: rep.Precision, Accuracy: rep.Accuracy, Kappa: rep.Kappa,
		MeetsBar: rep.MeetsBar, BarReason: rep.BarReason,
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

func line(label string, r calibrate.Rate) {
	if !r.Defined {
		fmt.Printf("  %-18s UNDEFINED (n=%d)\n", label, r.N)
		return
	}
	// A statistic with no interval prints as a point estimate. Printing Lo–Hi for one whose Lo and Hi are just
	// the estimate again produced "95% CI -0.141–-0.141" — the tightest-looking interval possible, on the least
	// certain number in the report.
	if r.NoInterval {
		fmt.Printf("  %-18s %.3f  (point estimate, no interval; n=%d)\n", label, r.Value, r.N)
		return
	}
	fmt.Printf("  %-18s %.3f  (95%% CI %.3f–%.3f, n=%d)\n", label, r.Value, r.Lo, r.Hi, r.N)
}

// soleSessionFaults keeps only sessions whose fault raised NO OTHER session. See the package comment: this is
// the sole population where the corpus's per-fault truth and the judge's per-session call describe the same
// event, so it is the only one on which agreement means anything.
func soleSessionFaults(items []diagcorpus.Item) []diagcorpus.Item {
	n := map[int64]int{}
	for _, it := range items {
		n[it.FaultID]++
	}
	var out []diagcorpus.Item
	for _, it := range items {
		if n[it.FaultID] == 1 {
			out = append(out, it)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
}

// readJudgeCalls binarizes the judge's 1-5 score at the cut. A session the judge never scored is ABSENT from
// the map rather than defaulted, so JudgeOutcomes drops it: an unjudged session is missing data, and
// defaulting it either way would manufacture agreement or disagreement out of nothing.
func readJudgeCalls(ctx context.Context, pool *pgxpool.Pool, dimension string, cut int) (map[string]bool, error) {
	rows, err := pool.Query(ctx,
		`SELECT external_ref, score >= $2 FROM session_judgment WHERE dimension = $1`, dimension, cut)
	if err != nil {
		return nil, fmt.Errorf("judgecal: read judgments: %w", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var ref string
		var call bool
		if err := rows.Scan(&ref, &call); err != nil {
			return nil, fmt.Errorf("judgecal: scan: %w", err)
		}
		out[ref] = call
	}
	return out, rows.Err()
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "judgecal:", err)
	os.Exit(1)
}
