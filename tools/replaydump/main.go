// Command replaydump turns a LIVE session's recorded trajectory into a replay fixture, and verifies that a
// recording round-trips through the replay tool-set before it is committed (TG-580, Harness² learnings H2-6a).
//
//	replaydump -dsn postgres://... -ref <external_ref> -out eval/replays/<ref>.json   # dump one session's recording
//	replaydump -verify eval/replays/<ref>.json [-corpus eval/corpus.json]             # every observation served back byte-identically
//	replaydump -verify-corpus eval/corpus.json                                        # the captured fixture incidents, through the replay arm
//
// WHY A DUMP STEP. spec/025 documents the 200-300-incident replay corpus as not existing while agent_step +
// agent_step_evidence hold every session's scrubbed observations. A recording file is the committed, DB-free
// form the eval harness and the offline probe (TG-583) can load on any box, including CI's. The verify step is
// the honesty check the fixture arm already has: a recording whose observations the replay arm cannot serve
// back is not a fixture, and the tool says which call failed rather than letting a half-recording ship.
//
// Read-only against the database. Prints what it did; exit 1 on any mismatch, 2 on usage.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/estate"
	"github.com/territory-grounder/grounder/eval"
)

func main() {
	dsn := flag.String("dsn", "", "session_triage/agent_step DSN (a read-only role is enough) — dump mode")
	ref := flag.String("ref", "", "the session (external_ref) to dump — dump mode")
	out := flag.String("out", "", "where to write the recording JSON — dump mode")
	verify := flag.String("verify", "", "a recording JSON to verify through the replay tool-set")
	corpus := flag.String("corpus", "eval/corpus.json", "corpus whose incident framing a -verify run uses (matched by external_ref; a plain frame is used when absent)")
	verifyCorpus := flag.String("verify-corpus", "", "a corpus JSON whose fixture-armed incidents are verified through the replay arm")
	flag.Parse()
	switch {
	case *verifyCorpus != "":
		os.Exit(runVerifyCorpus(*verifyCorpus))
	case *verify != "":
		os.Exit(runVerify(*verify, *corpus))
	case *dsn != "" && *ref != "" && *out != "":
		os.Exit(runDump(*dsn, *ref, *out))
	default:
		fmt.Fprintln(os.Stderr, "usage: replaydump -dsn DSN -ref EXTERNAL_REF -out FILE | -verify FILE [-corpus FILE] | -verify-corpus FILE")
		os.Exit(2)
	}
}

func runDump(dsn, ref, out string) int {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "replaydump: connect:", err)
		return 1
	}
	defer pool.Close()
	rec, err := eval.LoadRecordedSession(ctx, db.NewAgentStepStore(pool), db.NewAgentStepEvidenceStore(pool), ref)
	if err != nil {
		fmt.Fprintln(os.Stderr, "replaydump:", err)
		return 1
	}
	if len(rec.Calls) == 0 {
		// A real answer, said plainly: nothing to replay is not a file worth committing.
		fmt.Fprintf(os.Stderr, "replaydump: session %s has NO recorded observations (recorded before evidence capture, or it called no tool) — nothing written\n", ref)
		return 1
	}
	raw, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "replaydump: encode:", err)
		return 1
	}
	if err := os.WriteFile(out, append(raw, '\n'), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "replaydump: write:", err)
		return 1
	}
	fmt.Printf("replaydump: %s — %d recorded observation(s) across %d cycle(s) -> %s\n", ref, len(rec.Calls), rec.Calls[len(rec.Calls)-1].Cycle, out)
	return 0
}

func runVerify(path, corpusPath string) int {
	raw, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "replaydump: read:", err)
		return 1
	}
	var rec eval.RecordedSession
	if err := json.Unmarshal(raw, &rec); err != nil {
		fmt.Fprintln(os.Stderr, "replaydump: parse:", err)
		return 1
	}
	if len(rec.Calls) == 0 {
		// An empty recording verifies NOTHING; passing it would let a truncated or mis-dumped file ship as a fixture.
		fmt.Fprintf(os.Stderr, "replaydump: %s carries no recorded observations — nothing to verify, REFUSED\n", path)
		return 1
	}
	inc := eval.Incident{ExternalRef: rec.ExternalRef}
	if c, err := eval.LoadCorpus(corpusPath); err == nil {
		for _, x := range c {
			if x.ExternalRef == rec.ExternalRef {
				inc = x
			}
		}
	}
	bad := verifyRecording(rec, inc)
	// What a pass PROVES, said plainly: every recorded id is in a family the arm can read, and a call for the target
	// the id names comes back byte-identical. The recording holds no record of the spelling the ORIGINAL agent used
	// in its call, so this does not prove that a replayed agent's own call will hit — the probe (TG-583) measures that.
	fmt.Printf("replaydump: verify %s — %d observation(s), %d mismatch(es) [a pass = every id readable and served back byte-identically for the target it names; the agent's own call spelling is not recorded]\n", rec.ExternalRef, len(rec.Calls), bad)
	if bad > 0 {
		return 1
	}
	return 0
}

func runVerifyCorpus(path string) int {
	c, err := eval.LoadCorpus(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "replaydump: corpus:", err)
		return 1
	}
	incidents, observations, bad := 0, 0, 0
	for _, inc := range c {
		if !inc.FixtureArmed() {
			continue
		}
		rec := eval.RecordedFromFixtures(inc)
		incidents++
		observations += len(rec.Calls)
		bad += verifyRecording(rec, inc)
	}
	fmt.Printf("replaydump: verify-corpus %s — %d fixture-armed incident(s), %d observation(s), %d mismatch(es)\n", path, incidents, observations, bad)
	if bad > 0 || incidents == 0 {
		return 1
	}
	return 0
}

// verifyRecording serves every recorded observation back through a fresh replay tool-set and counts the ones that
// do not come back EXACTLY: same result id, same Success, and the recorded Output (plus the truncation note when
// the capture was bounded) — never a prefix match, which an empty or shared-prefix output would satisfy. A
// targeted recording is asked for by the target its id names; a target-less one by the bare tool; an id in no
// known family is a mismatch by itself (the arm cannot serve what it cannot read).
func verifyRecording(rec eval.RecordedSession, inc eval.Incident) int {
	tools := eval.NewReplayToolSet(rec, inc, estate.NewGraph())
	bad := 0
	fromRecording := map[string]int{}
	for _, c := range rec.Calls {
		if eval.ReplayPreferred(c.Tool) {
			fromRecording[c.Tool]++
		}
		tl, ok := tools.Get(c.Tool)
		if !ok {
			fmt.Printf("  MISMATCH %s cycle %d: tool %q is not on the replay surface\n", rec.ExternalRef, c.Cycle, c.Tool)
			bad++
			continue
		}
		target, known := eval.RecordedTarget(c.Tool, c.EvidenceID)
		if !known {
			fmt.Printf("  MISMATCH %s cycle %d %s: result id %q is in no known family — the replay arm cannot serve it\n", rec.ExternalRef, c.Cycle, c.Tool, c.EvidenceID)
			bad++
			continue
		}
		args := map[string]string{}
		if target != "" {
			args["host"] = target
		}
		want := c.Output
		if c.Truncated {
			want += eval.ReplayTruncatedNote
		}
		got, err := tl.Invoke(context.Background(), args)
		if err != nil || got.ID != c.EvidenceID || got.Success != c.Success || got.Output != want {
			fmt.Printf("  MISMATCH %s cycle %d %s(%v): err=%v id=%q/%q success=%v/%v output=%q\n", rec.ExternalRef, c.Cycle, c.Tool, args, err, got.ID, c.EvidenceID, got.Success, c.Success, clip(got.Output, 80))
			bad++
		}
	}
	tools2 := make([]string, 0, len(fromRecording))
	for tool := range fromRecording {
		tools2 = append(tools2, tool)
	}
	sort.Strings(tools2)
	for _, tool := range tools2 {
		fmt.Printf("  NOTE %s: %d %s observation(s) are served from the recording — the tool is real in the fixture arm, but what it would read at replay time is not what this session saw\n", rec.ExternalRef, fromRecording[tool], tool)
	}
	return bad
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
