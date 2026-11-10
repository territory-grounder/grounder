package main

import (
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/territory-grounder/grounder/core/knowledge"
	"github.com/territory-grounder/grounder/core/lessons"
)

// lessonsHorizon resolves TG_LESSONS_MAX_AGE once for BOTH source paths — the operator-export feed and the live
// close-out writeback — with the same three-state rule (lessons.ResolveMaxAge: blank ⇒ the 90-day default,
// 0/off ⇒ disabled, a duration ⇒ itself), so the two can never disagree about how old a precedent may be
// (TG-673, deep-review F14). Says how it decided, because "the default applied" and "the operator set 90 days"
// must be distinguishable in the boot log. Kept out of main() under the TG-501 LOC ratchet.
func lessonsHorizon(raw string) time.Duration {
	d, how := lessons.ResolveMaxAge(raw)
	lessonsHorizonLogOnce.Do(func() { logLessonsHorizon(raw, d, how) })
	return d
}

var lessonsHorizonLogOnce sync.Once

func logLessonsHorizon(raw string, d time.Duration, how string) {
	log.Printf("lessons: retention horizon %s (%s; TG_LESSONS_MAX_AGE blank ⇒ %s default, 0/off disables; applied by the feed, the live writeback and the decay pass; TG-673)", d, how, lessons.DefaultMaxAge)
	if how == "invalid-using-default" {
		log.Printf("lessons: TG_LESSONS_MAX_AGE=%q is not a duration — running at the %s default", strings.TrimSpace(raw), d)
	}
}

// cautionsForSubject reads the caution lane's corpus file and returns the cautions recorded for one (host,
// rule) — the contradiction check's evidence (lessons.Contradicted). A blank path (the lane is dormant) or an
// unreadable file yields nil, which the writeback treats as "no caution known" (the pre-TG-673 behaviour);
// the file is re-read per writeback because writebacks are rare and the lane's writer may be another path.
func cautionsForSubject(cautionPath string) func(host, rule string) []knowledge.Incident {
	if strings.TrimSpace(cautionPath) == "" {
		return nil
	}
	return func(host, rule string) []knowledge.Incident {
		f, err := os.Open(cautionPath)
		if err != nil {
			return nil
		}
		defer f.Close()
		all, perr := knowledge.ParseCorpus(f)
		if perr != nil {
			return nil
		}
		h, r := strings.ToLower(strings.TrimSpace(host)), strings.ToLower(strings.TrimSpace(rule))
		var out []knowledge.Incident
		for _, inc := range all {
			if strings.ToLower(strings.TrimSpace(inc.Host)) == h && strings.ToLower(strings.TrimSpace(inc.AlertRule)) == r {
				out = append(out, inc)
			}
		}
		return out
	}
}
