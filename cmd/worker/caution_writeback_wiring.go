package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/territory-grounder/grounder/agent"
	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/knowledge"
	"github.com/territory-grounder/grounder/core/lessons"
	"github.com/territory-grounder/grounder/core/reflect"
	"github.com/territory-grounder/grounder/core/skillstore"
	"github.com/territory-grounder/grounder/core/trace"
	"github.com/territory-grounder/grounder/temporal/runner"
	"github.com/territory-grounder/grounder/temporal/skillgen"
)

// The TG-579 reflector's DB handles, published by newStepStores when a DB is wired (nil otherwise) and read by
// wireComplementSeams — kept out of main() so the TG-501 ratchet stays where it is.
var (
	experienceSteps stepReader
	experienceStore *db.ExperienceStore
)

// stepReader is the read side of the per-cycle transcript the reflector renders (core/db.AgentStepStore).
type stepReader interface {
	Steps(ctx context.Context, externalRef string) ([]trace.AgentStep, error)
}

// newStepStores builds the per-cycle transcript store once and publishes it to both its consumers: the agent
// loop's observe-only sink (the returned value) and the reflector's reader.
func newStepStores(pool *db.Pool) trace.AgentStepSink {
	st := db.NewAgentStepStore(pool)
	experienceSteps = st
	experienceStore = db.NewExperienceStore(pool)
	harnessRenderStore = db.NewHarnessRenderStore(pool) // TG-600: the rendering behind each harness fingerprint, written on a configuration's first session
	return st
}

// withExperienceFeed joins the reflector's evidence-backed lessons (session_experience at or above
// TG_SKILL_EXPERIENCE_MIN_CONFIDENCE, default medium) to the lesson flywheel's source — same target skill +
// dimension, same offline gate + trial (Harness² harnessing-level recursion wired into the loop TG already
// has). Unchanged input when the lesson flywheel is not armed (TG_SKILL_LESSON_* blank, the default).
func withExperienceFeed(pool *db.Pool, lessonSource skillstore.NotableIncidentSource) skillstore.NotableIncidentSource {
	ls, ld := getenv("TG_SKILL_LESSON_SKILL", ""), getenv("TG_SKILL_LESSON_DIMENSION", "")
	if ls == "" || ld == "" || pool == nil {
		return lessonSource
	}
	minConf, ignored := experienceMinConfidence()
	if ignored != "" {
		log.Printf("skillgen: TG_SKILL_EXPERIENCE_MIN_CONFIDENCE=%q is not one of low/medium/high — ignored, the feed runs at %s", ignored, minConf)
	}
	log.Printf("skillgen: EXPERIENCE feed ON (TG-579) — reflector lessons at confidence >= %s join the lesson flywheel for skill %q / dimension %q; generate-only; never actuates", minConf, ls, ld)
	return skillstore.CombineNotableSources(lessonSource, db.NewExperienceLessonSource(pool, ls, ld, minConf, envInt("TG_SKILL_LESSON_MAX", 20)))
}

// experienceMinConfidence is the ONE reading of TG_SKILL_EXPERIENCE_MIN_CONFIDENCE: the value the boot line prints
// is the value the lesson source is built with. Compose forwards the knob PRESENT-BUT-EMPTY when the operator
// leaves it blank, and getenv keeps a present key's value — so the first boot of TG-579 logged `confidence >=  join`
// with nothing after the operator (TG-597). The source itself already treated "" as its default, so only the log
// lied; here the fallback happens before either consumer sees the value, and an unknown word falls back the same
// way the source's own switch does, so the two can never disagree. Same shape as reflectConfig's TG_REFLECT_MODEL fix.
// The second value is the operator's word when it was NOT one of the three (empty otherwise), so the caller can say
// it was ignored — a typo that silently became "medium" would read as deliberate (review of TG-597).
func experienceMinConfidence() (value, ignored string) {
	v := strings.ToLower(strings.TrimSpace(getenv("TG_SKILL_EXPERIENCE_MIN_CONFIDENCE", "")))
	switch v {
	case "low", "medium", "high":
		return v, ""
	case "":
		return "medium", ""
	default:
		return "medium", v
	}
}

// reflectConfig reads the reflector's knobs. OFF unless TG_REFLECT_ENABLE is truthy (ship-inert: one bounded
// model call per non-clean remediation is a cost the operator turns on, never a default). A present-but-EMPTY
// TG_REFLECT_MODEL — exactly what compose forwards when the operator leaves it blank — falls back to "primary"
// (the review of TG-579 caught that getenv's LookupEnv semantics would otherwise hand the gateway an empty
// model name, which it refuses). The timeout is the per-call bound.
func reflectConfig() (enabled bool, tier string, timeout time.Duration) {
	tier = strings.TrimSpace(getenv("TG_REFLECT_MODEL", ""))
	if tier == "" {
		tier = "primary"
	}
	return truthyEnv("TG_REFLECT_ENABLE"), tier, time.Duration(envInt("TG_REFLECT_TIMEOUT_SECONDS", 25)) * time.Second
}

// wireComplementSeams arms both seams of the novelty writeback's COMPLEMENT (the deviated / partial /
// unconfirmed terminus): the live caution writer and — only when TG_REFLECT_ENABLE is set — the experience
// reflector.
func wireComplementSeams(deps *runner.Deps, cautionHolder *knowledge.Holder, cautionPath string, gw agent.Completer) {
	wireCautionWriteback(deps, cautionHolder, cautionPath)
	enabled, tier, timeout := reflectConfig()
	if !enabled {
		log.Printf("experience reflector: OFF (set TG_REFLECT_ENABLE=1 to arm; one bounded %q-tier call per non-clean remediation when on)", tier)
		return
	}
	var c reflect.Completer
	if gw != nil {
		c = skillgen.NewPromptCompleter(gw) // the same gateway adapter the skill generator uses (messages -> one prompt)
	}
	wireExperienceReflector(deps, c, experienceSteps, experienceStore, tier, timeout)
}

// cautionLaneMu serializes every writer of the caution corpus FILE: the operator-export fold-in (appendCautions in
// main.go) and the live close-out writer below. One mutex for one file, so the two never interleave a
// read-merge-write.
var cautionLaneMu sync.Mutex

// wireCautionWriteback arms the LIVE caution-lane writer (TG-52's missing half, delivered by TG-579): the
// reconcile terminus emits every executed, terminal, poll-answered session that was NOT a confirmed-clean match
// through runner.Deps.LearnCaution; lessons.CautionMerge distills it (lessons.Caution — the exact negation of the
// precedent gate, so a clean success contributes 0) into the SEPARATE caution corpus the <caution> block reads,
// and the holder reloads. Requires TG_CAUTION_FILE (a nil holder / blank path leaves the seam nil — the lane
// stays config-gated, exactly as the export feed is). Writes ONLY the caution file; never the precedent corpus,
// never the estate.
func wireCautionWriteback(deps *runner.Deps, cautionHolder *knowledge.Holder, cautionPath string) {
	if cautionHolder == nil || cautionPath == "" {
		log.Printf("caution: live close-out writer NOT armed (TG_CAUTION_FILE unset) — deviated/partial/unconfirmed remediations record no caution")
		return
	}
	deps.LearnCaution = func(_ context.Context, ri lessons.ResolvedIncident) error {
		cautionLaneMu.Lock()
		defer cautionLaneMu.Unlock()
		var existing []knowledge.Incident
		if cf, err := os.Open(cautionPath); err == nil {
			parsed, perr := knowledge.ParseCorpus(cf)
			cf.Close()
			if perr != nil {
				// A corrupt or truncated lane must NOT be rewritten as "one entry": the merge would silently
				// drop every caution that did not parse. Refuse the write; the operator sees the reason.
				return fmt.Errorf("caution writeback: %s unreadable (%v) — refusing to overwrite the lane", cautionPath, perr)
			}
			existing = parsed
		}
		merged, added := lessons.CautionMerge(existing, []lessons.ResolvedIncident{ri})
		if added == 0 {
			return nil // a clean success (not a caution) or an already-recorded ref — idempotent no-op
		}
		if werr := knowledge.WriteCorpusFile(cautionPath, merged); werr != nil {
			return fmt.Errorf("caution writeback: %w", werr)
		}
		// Reload the lane's retriever the SAME way the export fold-in does (knowledge_corpus.go arms the
		// min-score floor and the IDF tags on the initial holder; a reload that dropped them would silently
		// un-arm TG-50/TG-508 for this lane).
		cautionHolder.Set(knowledge.NewLexicalRetriever(merged).SetMinScore(envFloat("TG_RETRIEVE_MIN_SCORE", 0)).SetIDFTags(truthyEnv("TG_RETRIEVAL_IDF_TAGS")))
		log.Printf("caution writeback: recorded %s (host=%s rule=%s verdict=%s confirmed_clear=%v) into %s — separate from the precedent corpus", ri.ExternalRef, ri.Host, ri.AlertRule, ri.Verdict, ri.ConfirmedClear, cautionPath)
		return nil
	}
	log.Printf("caution: live close-out writer armed (lane %s) — a deviated/partial/unconfirmed remediation records a factual caution", cautionPath)
}

// wireExperienceReflector arms the EXPERIENCE REFLECTOR seam (TG-579, Harness² learnings H2-5): on the same
// non-clean terminus the caution writer sees, one bounded model session reads the session's scrubbed trajectory
// + action + committed prediction + mechanical verdict and writes at most ONE transferable lesson (core/reflect)
// into session_experience — the input the skill flywheel's lesson feed reads (core/db.ExperienceLessonSource),
// never the agent's prompt. Requires a DB (steps + store) and a gateway; any nil leaves the seam nil. The call
// is bounded by its own timeout AND by what remains of the reconcile activity's deadline (minus a margin for
// the activity's own close-out), so a slow model can never push the activity into a Temporal retry that would
// repeat the close-out; too little budget skips the call and says so. Failures return to the best-effort
// caller and are logged there.
func wireExperienceReflector(deps *runner.Deps, c reflect.Completer, steps stepReader, store *db.ExperienceStore, modelTier string, timeout time.Duration) {
	if c == nil || steps == nil || store == nil {
		log.Printf("experience reflector: NOT armed (no DB or no gateway) — non-clean remediations write no experience")
		return
	}
	if timeout <= 0 {
		timeout = 25 * time.Second
	}
	const deadlineMargin, minBudget = 10 * time.Second, 8 * time.Second
	deps.Reflect = func(ctx context.Context, req runner.ReflectRequest) error {
		ri := req.Incident
		budget := timeout
		if dl, ok := ctx.Deadline(); ok {
			if rem := time.Until(dl) - deadlineMargin; rem < budget {
				if rem < minBudget {
					return fmt.Errorf("experience reflector: %s skipped — %s left of the activity deadline is not enough for a bounded model call", ri.ExternalRef, rem.Round(time.Second))
				}
				budget = rem
			}
		}
		ctx, cancel := context.WithTimeout(ctx, budget)
		defer cancel()
		st, err := steps.Steps(ctx, ri.ExternalRef)
		if err != nil {
			return fmt.Errorf("experience reflector: steps for %s: %w", ri.ExternalRef, err)
		}
		exp, ok, err := reflect.Reflect(ctx, c, modelTier, reflect.Input{
			ExternalRef: ri.ExternalRef, Host: ri.Host, AlertRule: ri.AlertRule, Site: ri.Site, Summary: ri.Summary,
			Action: ri.Action, Verdict: ri.Verdict, ConfirmedClear: ri.ConfirmedClear, Prediction: req.Prediction, Steps: st,
		})
		if err != nil {
			return err
		}
		if !ok {
			log.Printf("experience reflector: %s — NO_ENTRY (the record shows no reusable contrast; nothing stored)", ri.ExternalRef)
			return nil
		}
		if werr := store.Write(ctx, exp); werr != nil {
			return werr
		}
		log.Printf("experience reflector: %s — lesson stored (confidence %s): cue=%q", ri.ExternalRef, exp.Confidence, exp.Cue)
		return nil
	}
	log.Printf("experience reflector: armed (model tier %q, per-call timeout %s) — a non-clean remediation yields at most one transferable lesson for the skill flywheel", modelTier, timeout)
}
