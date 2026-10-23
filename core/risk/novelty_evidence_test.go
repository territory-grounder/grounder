package risk

import (
	"strconv"
	"testing"

	"github.com/territory-grounder/grounder/core/safety"
)

// A NOVELTY POLL THAT RECORDS ONLY THAT IT FIRED CANNOT BE AUDITED.
//
// Measured live 2026-07-28: `ood-novel-incident` is the second-largest driver of POLL_PAUSE — 140 decisions in
// 7 days — and ZERO of them mutated. Every one asked a human, none produced an action. Whether that was 140
// correct refusals or 140 spurious escalations is UNANSWERABLE, because the classifier recorded the verdict
// (`poll_reason=ood-novel-incident`) and never the evidence: which (host, rule) signature it consulted, and
// what count came back.
//
// The evidence cannot be recovered afterwards either. The prior-incident corpus is a MUTABLE FILE with no
// history and no per-decision snapshot, so "what did the count say at 04:12 last Tuesday" has no answer. An
// investigation into these 140 got as far as comparing them against session_triage precedent — which is the
// WRONG corpus, since novelty reads the knowledge file and that only gains a row on a confirmed-clean
// closure — and would have reported a 70% false-positive rate that the data does not support.
//
// So: bind the evidence to the record at the moment the rule fires. Same shape as every other defect here.

func novelInput() GatedInput {
	return GatedInput{
		ExternalRef: "TG-nov", ActionID: "a1", PlanHash: "p1", RiskLevel: "low",
		OpClass: "start-guest", Reversible: Reversible, HasPrediction: true,
		NovelIncident: true, NoveltyKey: "dc1mealie01|Device-Down", NoveltyCount: 0,
	}
}

// TestANoveltyPollRecordsWhatItRead is the defect as an oracle.
func TestANoveltyPollRecordsWhatItRead(t *testing.T) {
	d := Classify(novelInput())
	if d.Band != safety.BandPollPause || d.Signals["poll_reason"] != "ood-novel-incident" {
		t.Fatalf("fixture did not produce a novelty poll: band=%v reason=%q", d.Band, d.Signals["poll_reason"])
	}
	if got := d.Signals["novelty_key"]; got != "dc1mealie01|Device-Down" {
		t.Errorf("novelty_key = %q, want the consulted signature — without it nobody can tell WHICH key had no "+
			"precedent, and the corpus that answered is a mutable file with no history", got)
	}
	if _, ok := d.Signals["novelty_count"]; !ok {
		t.Error("novelty_count is absent — the count IS the finding; a poll that records only its own verdict " +
			"cannot be audited, and 140 such polls in 7 days are currently unanswerable")
	}
}

// TestTheRecordedCountIsTheONEThatDecided — the count must be the deciding zero, not a placeholder. A count
// that does not correspond to the key would be worse than none: it would look like evidence.
func TestTheRecordedCountIsTheOneThatDecided(t *testing.T) {
	in := novelInput()
	in.NoveltyCount = 0
	d := Classify(in)
	if got := d.Signals["novelty_count"]; got != strconv.Itoa(0) {
		t.Errorf("novelty_count = %q, want %q — the rule fires precisely BECAUSE the count is zero, so any "+
			"other value means the field is not reporting what the classifier acted on", got, "0")
	}
}

// TestEvidenceIsRecordedONLYForTheNoveltyRule — a decision polled for a different, stronger reason must not
// carry novelty evidence. Attaching it everywhere would make the field meaningless as a filter, and an
// operator reading the row would think novelty drove a decision that a suspicious actor drove.
func TestEvidenceIsRecordedOnlyForTheNoveltyRule(t *testing.T) {
	in := novelInput()
	in.AttributionSecurity = true // a strictly stronger reason, evaluated earlier
	d := Classify(in)
	if r := d.Signals["poll_reason"]; r == "ood-novel-incident" {
		t.Fatalf("fixture did not exercise a stronger rule: poll_reason=%q", r)
	}
	if _, ok := d.Signals["novelty_key"]; ok {
		t.Errorf("a decision polled for %q carries novelty evidence — the field must identify decisions the "+
			"NOVELTY rule actually drove, or it cannot be used to audit them", d.Signals["poll_reason"])
	}
}

// TestANonNovelDecisionCarriesNoNoveltyEvidence — the ordinary case. Most decisions are not novel; none of
// them may claim to be.
func TestANonNovelDecisionCarriesNoNoveltyEvidence(t *testing.T) {
	in := novelInput()
	in.NovelIncident = false
	d := Classify(in)
	if _, ok := d.Signals["novelty_key"]; ok {
		t.Error("a decision where novelty did NOT fire carries novelty evidence")
	}
	if _, ok := d.Signals["novelty_count"]; ok {
		t.Error("a decision where novelty did NOT fire carries a novelty count")
	}
}

// TestAnEmptyKeyIsOmittedRatherThanRecordedBlank — an empty signature is the absence this change exists to
// remove. Writing "" would satisfy a presence check while carrying nothing, which is how a field becomes
// decorative.
func TestAnEmptyKeyIsOmittedRatherThanRecordedBlank(t *testing.T) {
	in := novelInput()
	in.NoveltyKey = ""
	d := Classify(in)
	if v, ok := d.Signals["novelty_key"]; ok && v == "" {
		t.Error("an empty novelty_key was recorded as a blank value — omit it instead, so a presence check " +
			"means the evidence is actually there")
	}
}
