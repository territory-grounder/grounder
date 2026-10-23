// Command evidencepack renders one or more LIVE sessions into Harness²-shaped evidence packs (TG-581, Harness²
// learnings H2-6b): the data an improver reads as FILES — the task, the full trajectory with its recorded
// observations, the deliverable exactly as the judge saw it, the skill bodies the session composed with and
// candidate bodies with their diffs. ANSWER-FREE: no judge score or comment is written anywhere in a pack.
//
//	TG_RUNTIME_DSN=postgres://... evidencepack -ref <external_ref>[,<external_ref>...] -out <dir> [-candidate name=<version id>]...
//
// One pack per ref under <dir>/<external_ref>/. -candidate adds a pool entry whose body is the named skill_version
// row (repeatable; label pool/cand<N>; the name must be that row's skill). The database is opened READ-ONLY
// (db.ReadOnlyDSN — the server refuses any write). A session whose skill bodies cannot all be read gets a pack
// WITHOUT those bodies and the run exits 1: a degraded pack is never silently complete. Exit 1 on any failure, 2
// on usage.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/evidencepack"
)

type candidateFlags []string

func (c *candidateFlags) String() string     { return strings.Join(*c, ",") }
func (c *candidateFlags) Set(v string) error { *c = append(*c, v); return nil }

type candidate struct {
	name string
	id   int64
}

// parseCandidates validates every -candidate flag BEFORE anything connects or writes: a usage error must not leave
// half a run behind.
func parseCandidates(flags []string) ([]candidate, error) {
	out := make([]candidate, 0, len(flags))
	for _, c := range flags {
		name, idStr, ok := strings.Cut(c, "=")
		id, perr := strconv.ParseInt(strings.TrimSpace(idStr), 10, 64)
		name = strings.TrimSpace(name)
		if !ok || perr != nil || id <= 0 || name == "" {
			return nil, fmt.Errorf("-candidate %q must be name=<positive skill_version id>", c)
		}
		out = append(out, candidate{name: name, id: id})
	}
	return out, nil
}

// safeRef mirrors the pack's own file-name rule for the OUTERMOST path component: a ref is a directory name under -out.
func safeRef(ref string) error {
	if ref == "" || ref == "." || ref == ".." || len(ref) > 200 || strings.ContainsAny(ref, "/\\\x00\n\r\t") {
		return fmt.Errorf("ref %q is not a safe directory name", ref)
	}
	return nil
}

func main() {
	dsn := flag.String("dsn", os.Getenv("TG_RUNTIME_DSN"), "session_triage DSN (default $TG_RUNTIME_DSN; opened read-only)")
	refs := flag.String("ref", "", "comma-separated external_refs to render")
	out := flag.String("out", "", "directory to write <dir>/<external_ref>/ packs under")
	var candFlags candidateFlags
	flag.Var(&candFlags, "candidate", "name=<skill_version id> — a candidate body for pool/ (repeatable)")
	flag.Parse()
	if *dsn == "" || *refs == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "usage: TG_RUNTIME_DSN=... evidencepack -ref REF[,REF...] -out DIR [-candidate name=ID]...")
		os.Exit(2)
	}
	cands, err := parseCandidates(candFlags)
	if err != nil {
		fmt.Fprintln(os.Stderr, "evidencepack:", err)
		os.Exit(2)
	}
	var refList []string
	for _, r := range strings.Split(*refs, ",") {
		if r = strings.TrimSpace(r); r != "" {
			if err := safeRef(r); err != nil {
				fmt.Fprintln(os.Stderr, "evidencepack:", err)
				os.Exit(2)
			}
			refList = append(refList, r)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	pool, err := db.Connect(ctx, db.ReadOnlyDSN(*dsn))
	cancel()
	if err != nil {
		fmt.Fprintln(os.Stderr, "evidencepack: connect:", err)
		os.Exit(1)
	}
	defer pool.Close()
	triage, steps, evidence, skills := db.NewTriageStore(pool), db.NewAgentStepStore(pool), db.NewAgentStepEvidenceStore(pool), db.NewSkillStore(pool)
	rc := 0
	for _, ref := range refList {
		if !renderOne(context.Background(), ref, *out, cands, triage, steps, evidence, skills) {
			rc = 1
		}
	}
	os.Exit(rc)
}

func renderOne(parent context.Context, ref, outDir string, cands []candidate, triage *db.TriageStore, steps *db.AgentStepStore, evidence *db.AgentStepEvidenceStore, skills *db.SkillStore) bool {
	ctx, cancel := context.WithTimeout(parent, 60*time.Second) // per session, so a long ref list never times out as a whole
	defer cancel()
	row, ok, err := triage.Row(ctx, ref)
	if err != nil {
		fmt.Fprintln(os.Stderr, "evidencepack:", err)
		return false
	}
	if !ok {
		fmt.Fprintf(os.Stderr, "evidencepack: %s — no session_triage row (unknown ref); nothing written\n", ref)
		return false
	}
	st, err := steps.Steps(ctx, ref)
	if err != nil {
		fmt.Fprintln(os.Stderr, "evidencepack:", err)
		return false
	}
	ev, err := evidence.EvidenceFor(ctx, ref)
	if err != nil {
		fmt.Fprintln(os.Stderr, "evidencepack:", err)
		return false
	}
	complete := true
	harness := map[string]string{}
	for name, id := range evidencepack.HarnessLoads(row.SkillLoads) {
		v, err := skills.GetVersion(ctx, id)
		if err != nil {
			// Omitted, said, and the run fails: an error string is not a body and must never be diffed against.
			fmt.Fprintf(os.Stderr, "evidencepack: %s — skill %s version %d could not be read (%v); harness/%s.md OMITTED\n", ref, name, id, err, name)
			complete = false
			continue
		}
		harness[name] = v.Body
	}
	pack := evidencepack.Assemble("live", evidencepack.Source{Row: row, Steps: st, Evidence: ev, Harness: harness})
	for i, c := range cands {
		v, err := skills.GetVersion(ctx, c.id)
		if err != nil {
			fmt.Fprintf(os.Stderr, "evidencepack: %s — candidate %s version %d: %v\n", ref, c.name, c.id, err)
			complete = false
			continue
		}
		if v.SkillName != c.name {
			fmt.Fprintf(os.Stderr, "evidencepack: %s — candidate version %d is skill %q, not %q; refused (a body diffed against the wrong base is a lie)\n", ref, c.id, v.SkillName, c.name)
			complete = false
			continue
		}
		pack.Pool = append(pack.Pool, evidencepack.Candidate{Label: fmt.Sprintf("cand%d", i+1), Bodies: map[string]string{c.name: v.Body}})
	}
	dir := filepath.Join(outDir, ref)
	if err := evidencepack.Write(dir, pack); err != nil {
		fmt.Fprintln(os.Stderr, "evidencepack: write:", err)
		return false
	}
	state := "complete"
	if !complete {
		state = "DEGRADED (see stderr)"
	}
	fmt.Printf("evidencepack: %s — %d step(s), %d recorded observation(s), %d harness bodies, %d candidate(s) -> %s [%s]\n",
		ref, len(st), len(ev), len(harness), len(pack.Pool), dir, state)
	return complete
}
