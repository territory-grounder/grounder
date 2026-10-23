// Command tierab is the deterministic half of TG-204's three-arm model-tier A/B (see eval/tierab).
//
// It reads a RUN MANIFEST (one entry per arm: the tier aliases it declared, the scorecard it produced, and
// the wall-clock window it ran in — written by eval/tier-ab.sh) plus the tg-claude-proxy's own telemetry,
// attributes each served completion to an arm by window, and prints the three-arm comparison. It performs
// NO SSH and NO model calls: the noisy on-box run happens in the shell, this binary is pure comparison,
// unit-tested in eval/tierab. Same split as eval/eval-gate.sh + tools/evalgate.
//
//	go run ./tools/tierab --manifest eval/out/tierab/run.json --proxy-log eval/out/tierab/proxy.log \
//	  --archive-dir eval/history --git-sha $(git rev-parse HEAD)
//
// ★ WHY THE PROXY LOG IS A REQUIRED INPUT AND NOT AN OPTIONAL NICETY. The one thing this experiment must
// establish before any number it prints means anything is that the three arms ran DIFFERENT MODELS — and
// the model gateway cannot establish it. LiteLLM echoes the requested ALIAS back in the completion
// response's `model` field (probed live 2026-08-04: alias "fast" -> {"model":"fast"}), so an arm-identity
// check reading the gateway would pass vacuously on aliases that are the same brain. The tg-claude-proxy
// logs the model that actually served (`served_model`), so that is the evidence this tool requires. With no
// --proxy-log there is no evidence, and a run with no evidence is refused rather than assumed distinct.
//
// Exit status: 0 MEASURED · 1 COLLAPSED (arms shared a model — one arm measured N times) · 3 UNKNOWN (an
// arm's served model is unproven) · 2 usage/IO error. Every non-zero outcome means the same thing to a
// caller: nothing about model tier was measured, so nothing may be concluded about it.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/territory-grounder/grounder/eval/gate"
	"github.com/territory-grounder/grounder/eval/tierab"
)

// manifest is what eval/tier-ab.sh writes: the run's identity plus one entry per measured arm.
type manifest struct {
	MeasuredAt string     `json:"measured_at"`
	GitSHA     string     `json:"git_sha"`
	Gateway    string     `json:"gateway"`
	Note       string     `json:"note,omitempty"`
	Arms       []armEntry `json:"arms"`
}

type armEntry struct {
	Name            string `json:"name"`
	InvestigateTier string `json:"investigate_tier"`
	DecideTier      string `json:"decide_tier"`
	Scorecard       string `json:"scorecard"`
	Start           string `json:"start"` // RFC3339, UTC — the arm's measurement window
	End             string `json:"end"`
	// CallerPrefix defaults to tierab.AgentCallerPrefix ("runner:"). A manifest may set it to "" to
	// deliberately attribute ALL in-window traffic — never what the driver asks for, because the eval judge
	// runs on `primary` through this same proxy in every arm.
	CallerPrefix *string `json:"caller_prefix,omitempty"`
}

// callerPrefix resolves the arm's attribution filter, defaulting to the agent prefix. The field is a
// POINTER so "unset" (use the safe default) is distinguishable from an explicit "" (attribute everything);
// with a plain string the safe default would be unreachable from a manifest that simply omitted the key.
func (e armEntry) callerPrefix() string {
	if e.CallerPrefix == nil {
		return tierab.AgentCallerPrefix
	}
	return *e.CallerPrefix
}

func main() {
	manifestPath := flag.String("manifest", "", "run manifest JSON (one entry per arm: tiers, scorecard path, window) — written by eval/tier-ab.sh")
	proxyLog := flag.String("proxy-log", "", "tg-claude-proxy JSON log covering the run window — the ONLY source of served-model ground truth (the gateway echoes the requested alias, so it cannot supply it)")
	archiveDir := flag.String("archive-dir", "", "append-only quality-record dir (eval/history): write <dir>/<date>-tierab-<sha>/verdict.json for the MR to commit")
	gitSHA := flag.String("git-sha", "", "git SHA recorded on the archived record")
	emitJSON := flag.Bool("json", false, "print the verdict as JSON in addition to the table")
	preflight := flag.Bool("preflight", false, "ARM-DISTINCTNESS ONLY: prove the arms run different models before spending three corpus passes on them; scorecards are not required and no delta is computed")
	flag.Parse()

	if *manifestPath == "" {
		fatal("no --manifest given (run eval/tier-ab.sh, which writes one)")
	}
	// A missing proxy log is refused up front rather than degrading to "no calls observed". The failure would
	// otherwise arrive as OutcomeUnknown, which is correct but blames the ARMS for a missing INPUT — and a
	// reader who sees "arm identity unproven" three times will re-run the expensive benchmark instead of
	// pointing the tool at the log it needed.
	if *proxyLog == "" {
		fatal("no --proxy-log given: the model gateway echoes back the requested alias, so it cannot prove which\n" +
			"  model actually served an arm. Without the tg-claude-proxy's served_model telemetry this run cannot\n" +
			"  establish that the arms differed at all, and an A/B that cannot prove its arms differed is not an A/B.")
	}

	var m manifest
	readJSON(*manifestPath, &m)
	if len(m.Arms) == 0 {
		fatal("manifest %s declares no arms", *manifestPath)
	}

	rawLog, err := os.ReadFile(*proxyLog)
	if err != nil {
		fatal("read proxy log: %v", err)
	}
	calls, err := tierab.ParseProxyLog(string(rawLog))
	if err != nil {
		fatal("%v", err) // the vacuity floor: a log naming zero served completions is an error, never an empty run
	}

	var arms []tierab.Arm
	for _, e := range m.Arms {
		var card gate.Scorecard
		// A preflight scores nothing, so it must not demand a scorecard — requiring one would make the cheap
		// check depend on the expensive run it exists to gate.
		if !*preflight {
			var err error
			if card, err = gate.LoadScorecard(e.Scorecard); err != nil {
				fatal("arm %s: %v", e.Name, err)
			}
			// SAFETY, mirroring tools/evalgate: this is a read-only eval and a mutation during it is a hard
			// stop, not a footnote on a benchmark table.
			if card.MutationCount != 0 {
				fatal("SAFETY: arm %s reports mutation_count=%d during a read-only eval — must be 0", e.Name, card.MutationCount)
			}
		}
		arms = append(arms, tierab.Arm{
			Name: e.Name, InvestigateTier: e.InvestigateTier, DecideTier: e.DecideTier,
			Window:       tierab.Window{Start: mustTime(e.Name, "start", e.Start), End: mustTime(e.Name, "end", e.End)},
			CallerPrefix: e.callerPrefix(),
			Card:         card,
		})
	}

	attributed, unattributed := tierab.AttributeCalls(arms, calls)
	var v tierab.Verdict
	if *preflight {
		v = tierab.Distinctness(attributed)
	} else {
		v = tierab.Compare(attributed)
	}
	v.Unattributed = unattributed

	fmt.Print(tierab.Render(v))
	// The single most likely operational failure of this harness is an arm-identity check that starves
	// because the gateway did not forward the `user` field — LiteLLM drops it before an openai/-provider
	// upstream, so the proxy logs caller="" (measured 2026-08-04). Without this line the operator sees three
	// UNKNOWN arms and no cause, and the obvious guess ("the proxy is broken") is wrong.
	if starved := tierab.CallerFilterStarved(attributed, calls); len(starved) > 0 {
		fmt.Printf("\n  CAUSE: %v had in-window proxy calls but the caller prefix matched none of them.\n"+
			"  LiteLLM DROPS the OpenAI `user` field before an openai/-provider upstream, so the proxy logs\n"+
			"  caller=\"\" for every request TG makes. Set \"caller_prefix\": \"\" in the manifest and rely on the\n"+
			"  session-phase window to exclude the judge (which is what eval/tier-ab.sh does).\n", starved)
	}
	if *emitJSON {
		fmt.Println(string(v.JSON()))
	}
	if *archiveDir != "" {
		date := m.MeasuredAt
		if date == "" {
			date = time.Now().UTC().Format("2006-01-02")
		}
		// A preflight record is labelled as one in the DIRECTORY NAME, not only inside the JSON: eval/history
		// is read by listing it, and a preflight archived as "-tierab-" would be mistaken for a corpus run.
		kind := "tierab"
		if *preflight {
			kind = "tierab-preflight"
		}
		// ★ THE ARM SET IS PART OF THE RECORD'S IDENTITY. Two runs on the same day at the same SHA with
		// DIFFERENT arms are different experiments, and a (date, kind, sha) key silently overwrites one with
		// the other — observed 2026-08-04, when the arm-haiku/arm-opus positive control landed on top of the
		// collapsed default-arm verdict and the finding was gone. The tier slug keys them apart.
		dir := filepath.Join(*archiveDir, fmt.Sprintf("%s-%s-%s-%s", date, kind, armSlug(m.Arms), shortSHA(*gitSHA)))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fatal("archive: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "verdict.json"), v.JSON(), 0o644); err != nil {
			fatal("archive: %v", err)
		}
		// The manifest rides along: a verdict without the arms' declared tiers and windows cannot be
		// re-derived a month later, and an un-reproducible benchmark record is a claim, not evidence.
		b, _ := json.MarshalIndent(m, "", "  ")
		if err := os.WriteFile(filepath.Join(dir, "manifest.json"), append(b, '\n'), 0o644); err != nil {
			fatal("archive: %v", err)
		}
		fmt.Printf("\nARCHIVE: quality record written to %s — commit it with the MR.\n", dir)
	}
	os.Exit(exitFor(v))
}

// exitFor maps the verdict onto the process status. COLLAPSED and UNKNOWN get DISTINCT non-zero codes so a
// caller can tell "the arms were the same brain" (a configuration fact, fixable in litellm) from "we cannot
// prove what the arms ran" (a telemetry fact, fixable by pointing at the right log) without parsing text.
// Both are non-zero because both mean the same thing to anyone about to act on the result: nothing about
// model tier was measured.
func exitFor(v tierab.Verdict) int {
	switch v.Outcome {
	case tierab.OutcomeMeasured:
		return 0
	case tierab.OutcomeCollapsed:
		return 1
	default:
		return 3
	}
}

func mustTime(arm, which, s string) time.Time {
	// RFC3339Nano, so a fractional-second boundary parses. The layout's ".999999999" makes the fraction
	// OPTIONAL, so a whole-second timestamp from an older manifest still parses — but a whole-second END
	// silently narrows the window and drops the arm's last (decide-tier) call, which is why both writers
	// emit milliseconds now. See eval/tierab.Window.
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		fatal("arm %s: %s window timestamp %q is not RFC3339: %v — window attribution is the ONLY thing that "+
			"separates one arm's proxy calls from another's, so a bad boundary silently mixes the arms", arm, which, s, err)
	}
	return t.UTC()
}

func readJSON(path string, v any) {
	b, err := os.ReadFile(path)
	if err != nil {
		fatal("read %s: %v", path, err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		fatal("parse %s: %v", path, err)
	}
}

// armSlug is a short, stable, filesystem-safe digest of the tier aliases under test — the part of a run's
// identity that (date, kind, sha) misses. Stable across re-runs of the SAME arms (so an MR re-running its
// own experiment overwrites its own record, which is intended) and different for any other arm set.
func armSlug(arms []armEntry) string {
	var parts []string
	for _, a := range arms {
		parts = append(parts, a.InvestigateTier+"-"+a.DecideTier)
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(sum[:4])
}

func shortSHA(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	if s == "" {
		return "nosha"
	}
	return s
}

func fatal(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "tierab: "+format+"\n", a...)
	os.Exit(2)
}
