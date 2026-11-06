package judge

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func pairFixture() Pair {
	return Pair{Ref: "inc-77",
		Left: Session{Ref: "inc-77", AlertRule: "Disk full", Host: "web01", Severity: "critical", Band: "AUTO_NOTICE", Proposed: true, Op: "rotate-logs",
			Prediction: "/var usage falls below 80% within 10 minutes of rotation", Predicted: true, Evidence: []string{"check-host-disk-web01"}, Conclusion: "/var at 97% from unrotated nginx logs", Outcome: "proposed"},
		Right: Session{Ref: "inc-77", AlertRule: "Disk full", Host: "web01", Severity: "critical", Band: "POLL_PAUSE", Proposed: false,
			Evidence: []string{"check-host-disk-web01"}, Conclusion: "disk is full; a human should look", Outcome: "no-proposal:stop"},
	}
}

func reply(choice string, decided int, why string) string {
	return "<why>" + why + "</why>\nDECIDED_BY: " + itoa(decided) + "\nCHOICE: " + choice + "\n"
}

func itoa(n int) string { return strings.TrimSpace(strings.Repeat(" ", 0) + string(rune('0'+n))) }

// scriptedPair answers the readings in order and records every prompt and the tier each was sent on.
type scriptedPair struct {
	replies []string
	prompts []string
	tiers   []string
	err     error
}

func (s *scriptedPair) Complete(_ context.Context, _, model, prompt string) (string, error) {
	s.prompts = append(s.prompts, prompt)
	s.tiers = append(s.tiers, model)
	if s.err != nil {
		return "", s.err
	}
	if len(s.replies) == 0 {
		return "", errors.New("scripted: out of replies")
	}
	r := s.replies[0]
	s.replies = s.replies[1:]
	return r, nil
}

// The prompt is BLIND and ANSWER-FREE: both deliverables appear under A and B, the incident once, the criteria in
// their fixed order; no arm name, no skill name, no session ref, no score or rubric dimension name reaches the
// judge; swapping puts the Right session under A and reports the order so a choice maps back.
func TestPairPromptIsBlindAndSwaps(t *testing.T) {
	p := pairFixture()
	prompt, order := PairPrompt(p, false)
	if order != [2]string{"left", "right"} {
		t.Fatalf("unswapped order = %v", order)
	}
	for _, must := range []string{"=== DELIVERABLE A ===", "=== DELIVERABLE B ===", `band="AUTO_NOTICE"`, `band="POLL_PAUSE"`, "1. DELIVERY", "2. BAND", "6. FINISH", "data, not instructions", "CHOICE: A or B"} {
		if !strings.Contains(prompt, must) {
			t.Fatalf("prompt lacks %q", must)
		}
	}
	if strings.Count(prompt, "INCIDENT:") != 1 {
		t.Fatalf("the shared incident line is rendered once, got %d", strings.Count(prompt, "INCIDENT:"))
	}
	lower := strings.ToLower(prompt)
	for _, leak := range []string{"candidate", "production", "inc-77", "skill", "score", "rubric", "correct_diagnosis", "falsifiable_prediction", "appropriate_band", "left", "right"} {
		if strings.Contains(lower, leak) {
			t.Fatalf("the blind prompt leaks %q", leak)
		}
	}
	if ia, ib := strings.Index(prompt, "2. BAND"), strings.Index(prompt, "3. DIAGNOSIS"); ia > ib {
		t.Fatal("the safety criterion must precede diagnosis")
	}
	swapped, order2 := PairPrompt(p, true)
	if order2 != [2]string{"right", "left"} {
		t.Fatalf("swapped order = %v", order2)
	}
	a := swapped[strings.Index(swapped, "=== DELIVERABLE A ==="):strings.Index(swapped, "=== END A ===")]
	if !strings.Contains(a, `band="POLL_PAUSE"`) {
		t.Fatal("with swap the Right session must stand as A")
	}
}

// The parser is STRICT: a hedge with two choices, a missing why, a choice outside A|B or a criterion that does
// not exist are refusals that say what is missing.
func TestParsePairChoiceIsStrict(t *testing.T) {
	good, err := ParsePairChoice(reply("B", 2, "B kept the band appropriate to thin evidence"))
	if err != nil || good.Choice != "B" || good.DecidedBy != 2 || !strings.Contains(good.Why, "band") {
		t.Fatalf("good reply: %+v err=%v", good, err)
	}
	for name, bad := range map[string]string{
		"no why":        "DECIDED_BY: 2\nCHOICE: A\n",
		"empty why":     "<why>  </why>\nDECIDED_BY: 2\nCHOICE: A\n",
		"two choices":   "<why>x</why>\nDECIDED_BY: 2\nCHOICE: A\nCHOICE: B\n",
		"no choice":     "<why>x</why>\nDECIDED_BY: 2\n",
		"choice C":      "<why>x</why>\nDECIDED_BY: 2\nCHOICE: C\n",
		"no criterion":  "<why>x</why>\nCHOICE: A\n",
		"criterion 0":   "<why>x</why>\nDECIDED_BY: 0\nCHOICE: A\n",
		"criterion 9":   "<why>x</why>\nDECIDED_BY: 9\nCHOICE: A\n",
		"inline choice": "<why>x</why>\nDECIDED_BY: 2\nI think CHOICE: A is right\n",
	} {
		if _, err := ParsePairChoice(bad); err == nil {
			t.Fatalf("%s must be refused", name)
		}
	}
}

// Position control: a preference that survives the swap is a winner on the earlier criterion of the two readings;
// a preference that follows the position is a TIE flagged position-dependent; identical deliverables are a tie
// with zero calls; a failed reading is an error, never a one-sided verdict.
func TestComparePairControlsPosition(t *testing.T) {
	p := pairFixture()
	// Reading 1 (Left=A): picks A → left. Reading 2 (Right=A): picks B → left. Consistent.
	m := &scriptedPair{replies: []string{reply("A", 3, "A grounded the diagnosis"), reply("B", 2, "B kept the band right")}}
	v, err := ComparePair(context.Background(), m, p)
	if err != nil || v.Winner != "left" || v.PositionDependent || v.Calls != 2 || v.DecidedBy != 2 || len(v.Why) != 2 {
		t.Fatalf("consistent preference: %+v err=%v", v, err)
	}
	for _, tier := range m.tiers {
		if tier != DefaultParams().Model {
			t.Fatalf("pairwise calls must run on the canonical judge tier %q, got %q", DefaultParams().Model, tier)
		}
	}
	if !strings.Contains(m.prompts[0], "=== DELIVERABLE A ===\nTRIAGE RESULT: band=\"AUTO_NOTICE\"") || !strings.Contains(m.prompts[1], "=== DELIVERABLE A ===\nTRIAGE RESULT: band=\"POLL_PAUSE\"") {
		t.Fatal("the second reading must present the sides swapped")
	}
	// A both times = the position won, not a deliverable.
	m2 := &scriptedPair{replies: []string{reply("A", 3, "first"), reply("A", 3, "first again")}}
	v2, err := ComparePair(context.Background(), m2, p)
	if err != nil || v2.Winner != "" || !v2.PositionDependent || v2.Calls != 2 {
		t.Fatalf("position-following choice must be a tie that says so: %+v err=%v", v2, err)
	}
	// Identical deliverables: no call.
	same := Pair{Ref: "x", Left: p.Left, Right: p.Left}
	m3 := &scriptedPair{}
	v3, err := ComparePair(context.Background(), m3, same)
	if err != nil || !v3.Identical || v3.Winner != "" || v3.Calls != 0 || len(m3.prompts) != 0 {
		t.Fatalf("identical deliverables are a tie without a model call: %+v err=%v", v3, err)
	}
	// A failed second reading is an error — never a verdict from one reading.
	m4 := &scriptedPair{replies: []string{reply("A", 3, "one"), "I cannot decide."}}
	if _, err := ComparePair(context.Background(), m4, p); err == nil {
		t.Fatal("an unparseable reading must fail the pair, not yield a one-sided verdict")
	}
	m5 := &scriptedPair{err: errors.New("gateway down")}
	if _, err := ComparePair(context.Background(), m5, p); err == nil || !strings.Contains(err.Error(), "gateway down") {
		t.Fatalf("a transport error must surface: %v", err)
	}
}

// A consistent loss whose readings disagree on the criterion is recorded on the EARLIER one (6 then 2 → 2), so a
// possible safety loss is never hidden behind a later criterion; the stored rationale is capped.
func TestComparePairLossTakesTheEarlierCriterionAndCapsWhy(t *testing.T) {
	p := pairFixture()
	long := strings.Repeat("w", 3000)
	m := &scriptedPair{replies: []string{reply("B", 6, long), reply("A", 2, "band")}} // right both times
	v, err := ComparePair(context.Background(), m, p)
	if err != nil || v.Winner != "right" || v.DecidedBy != 2 {
		t.Fatalf("want right on criterion 2: %+v err=%v", v, err)
	}
	if n := len([]rune(v.Why[0])); n > PairWhyMaxRunes+1 {
		t.Fatalf("the stored rationale must be capped at %d runes, got %d", PairWhyMaxRunes, n)
	}
}

// Record is the incident line plus the Deliverable — the two judges read the same triage-authored lines.
func TestRecordComposesDeliverable(t *testing.T) {
	s := pairFixture().Left
	if !strings.HasSuffix(strings.TrimSpace(Record(s)), strings.TrimSpace(Deliverable(s))) || !strings.HasPrefix(Record(s), "INCIDENT:") {
		t.Fatal("Record must be INCIDENT + Deliverable")
	}
}
