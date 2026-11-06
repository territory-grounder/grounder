package diagcorpus

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Read builds the corpus by joining every triage session to the INJECTED FAULT that was live on its host when
// the session ran. That join IS the labelling: the injector recorded what it broke, so the correct answer is
// known without anyone reading the incident.
//
// The window closes at restored_at when the fault was discharged, and otherwise at injected_at + grace. Using
// the ACTUAL restore time rather than a fixed window matters: a fault restored early would otherwise keep
// capturing sessions that ran against a healthy host, labelling them with a fault that was no longer present
// — items that are not merely noise but WRONGLY labelled, which is worse than missing.
func Read(ctx context.Context, pool *pgxpool.Pool, grace time.Duration) ([]Item, error) {
	// DISTINCT ON assigns each session to EXACTLY ONE fault. The join is many-to-many: overlapping fault
	// windows on one host (measured: 18 such pairs live) let a single session match two faults, and it was then
	// scored twice — once under each, with CONTRADICTORY ground truth, since the two faults are different
	// classes. Live that was 777 joined rows over 768 distinct sessions: 9 double-counted, small but wrong in a
	// way that cannot be reasoned about, because the same session argues for two different correct answers.
	//
	// The tie-break is the NEAREST PRECEDING injection (f.injected_at DESC). A session is evidence about the
	// most recent fault live on its host when it ran — an older fault still inside its grace window is the less
	// likely subject, and choosing by recency is a rule fixed in advance rather than one that depends on row
	// order. f.id breaks an exact timestamp tie so the result is deterministic and reproduces to the digit.
	rows, err := pool.Query(ctx, `
		SELECT DISTINCT ON (st.external_ref)
		       st.external_ref, st.host, st.alert_rule, f.fault_type, COALESCE(st.op_class,''),
		       COALESCE(st.conclusion,''), COALESCE(st.diagnosis::text,''), st.created_at, f.id
		FROM session_triage st
		JOIN injected_fault f
		  ON f.host = st.host
		 AND st.created_at >= f.injected_at
		 AND st.created_at <= COALESCE(f.restored_at, f.injected_at + $1::interval)
		ORDER BY st.external_ref, f.injected_at DESC, f.id DESC`,
		fmt.Sprintf("%d seconds", int(grace.Seconds())))
	if err != nil {
		return nil, fmt.Errorf("diagcorpus: read: %w", err)
	}
	defer rows.Close()
	var out []Item
	for rows.Next() {
		var it Item
		if err := rows.Scan(&it.ExternalRef, &it.Host, &it.AlertRule, &it.FaultType, &it.Proposed, &it.Conclusion, &it.Diagnosis, &it.At, &it.FaultID); err != nil {
			return nil, fmt.Errorf("diagcorpus: scan: %w", err)
		}
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// DISTINCT ON dictates its own ORDER BY, so chronological order is restored here rather than in SQL.
	sort.Slice(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out, nil
}

// LoadRuleset reads the operator-declared expectations. It is a FILE, not a compiled default: what correctly
// addresses a fault class is an estate policy question, and a compiled answer would score TG against this
// author's opinion. An absent path is an ERROR rather than a silent fallback — scoring a diagnosis corpus
// against assumed expectations is exactly the kind of quiet substitution that produces a confident wrong
// number.
func LoadRuleset(path string) (Ruleset, error) {
	if path == "" {
		return Ruleset{}, fmt.Errorf("diagcorpus: no expectations file — refusing to score against assumed expectations")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return Ruleset{}, fmt.Errorf("diagcorpus: read expectations: %w", err)
	}
	var rs Ruleset
	if err := json.Unmarshal(b, &rs); err != nil {
		return Ruleset{}, fmt.Errorf("diagcorpus: parse expectations: %w", err)
	}
	if len(rs.Expectations) == 0 {
		return Ruleset{}, fmt.Errorf("diagcorpus: expectations file declares nothing — every item would be excluded")
	}
	// A contradictory expectation is rejected rather than resolved by precedence. Silently letting one flag win
	// would score a whole fault class under a rule its author did not write — and the author would never see it,
	// because the scorecard reports the verdict, not the rule that produced it.
	for _, e := range rs.Expectations {
		switch {
		case e.StandDownIsCorrect && e.Unhealable:
			return Ruleset{}, fmt.Errorf("diagcorpus: %q declares both stand_down_is_correct and unhealable — "+
				"the first SCORES the stand-down as the right answer, the second EXCLUDES the class from scoring "+
				"entirely; pick one", e.FaultType)
		case e.StandDownIsCorrect && len(e.Accept) > 0:
			return Ruleset{}, fmt.Errorf("diagcorpus: %q declares stand_down_is_correct yet accepts %v — "+
				"if any op-class is a correct answer then standing down is not", e.FaultType, e.Accept)
		case !e.StandDownIsCorrect && !e.Unhealable && len(e.Accept) == 0:
			return Ruleset{}, fmt.Errorf("diagcorpus: %q accepts no op-class but is neither unhealable nor "+
				"stand_down_is_correct — every item in it would score WRONG whatever TG did", e.FaultType)
		}
	}
	return rs, nil
}
