package runner

import (
	"bytes"
	"context"
	"errors"

	"log"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/safety"
)

// captureLog redirects the standard logger for one test and returns a reader for what was written. The
// prior-verdict read path must ANNOUNCE a failure rather than degrade in silence, so the log line is asserted
// as part of the contract, not treated as incidental output.
func captureLog(t *testing.T) func() string {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return buf.String
}

// spec/001 REQ-015 (TG-223) oracles for the prior-verdict band.
//
// WHY EVERY ASSERTION DRIVES ClassifyActivity. HasVerdict/Verdict were consumed at classifier.go:111 and set
// by NOTHING for the whole life of the classifier — an unreachable branch that read as covered because the
// classifier's own unit tests constructed a GatedInput by hand. That is this repo's #1 defect class: an oracle
// that stubs the very path the feature lives on. The feature here IS the wiring (a durable read, a rule-family
// fold, a recency bound, a fail-toward-caution error path), so these tests call the REAL ClassifyActivity —
// the same function the Temporal worker registers — and let it build the GatedInput itself. A test that filled
// risk.GatedInput{HasVerdict: true} directly would pass whether or not one line of this feature shipped.

// priorVerdictDeps returns test deps whose PriorVerdicts seam serves `rows` for `host` and nothing for any
// other host. A nil rows slice means "the ledger holds nothing for this host".
func priorVerdictDeps(host string, rows []PriorVerdict) Deps {
	d := testDeps(proposeWeb01)
	d.PriorVerdicts = func(_ context.Context, h string) ([]PriorVerdict, error) {
		if h == host {
			return rows, nil
		}
		return nil, nil
	}
	return d
}

// classifyIn is the auto-eligible baseline: a low-risk reversible restart on a prediction-eligible host.
// Without a prior verdict it reaches AUTO, so any POLL_PAUSE below is attributable to the verdict alone.
func classifyIn(rule string) ClassifyInput {
	return ClassifyInput{
		ExternalRef: "TG-1", ActionID: "act-1", PlanHash: "plan-1", RiskLevel: "low",
		OpClass: "restart-service", Op: "restart", Host: "web01", IncidentHost: "web01",
		AlertRule: rule, Reversible: true,
	}
}

// A DEVIATION recorded for this target inside the window, under the same rule family, forces POLL_PAUSE
// through the real classify path. This is "a deviation can never auto-resolve again" applied at
// classification: the very next same-family remediation on a host TG just deviated on meets a human, instead
// of the class having to fail again before the graduation ladder demotes it.
func TestClassifyActivityRecentDeviationForcesPoll(t *testing.T) {
	acts := NewActivities(priorVerdictDeps("web01", []PriorVerdict{
		{Verdict: safety.VerdictDeviation, AlertRule: "HostDown", At: time.Now().Add(-time.Hour)},
	}))
	d, err := acts.ClassifyActivity(context.Background(), classifyIn("HostDown"))
	if err != nil {
		t.Fatal(err)
	}
	if d.Band != safety.BandPollPause || d.AutoApproved || d.AutoResolve {
		t.Fatalf("a recent same-family deviation must force POLL_PAUSE with autonomy withheld, got %+v", d)
	}
	if got := d.Signals["poll_reason"]; got != "verdict-deviation-or-invalid" {
		t.Fatalf("poll_reason = %q, want verdict-deviation-or-invalid — an operator reading the audit row must "+
			"see WHY they were asked", got)
	}
	// The reading, not just the ruling (the REQ-014 discipline extended to REQ-015): the signature the deciding
	// verdict was read under, keyed on the canonical FAMILY the fold actually matched — never the raw spelling.
	if got := d.Signals["prior_verdict_key"]; got != "web01|device-down" {
		t.Fatalf("prior_verdict_key = %q, want web01|device-down — the audit row must record the (host, family) "+
			"signature the verdict was read under, or the fold is unreconstructible afterwards", got)
	}
	if got := d.Signals["prior_verdict"]; got != string(safety.VerdictDeviation) {
		t.Fatalf("prior_verdict = %q, want deviation", got)
	}
}

// ABSENT ⇒ EXACTLY current behavior. An empty ledger for the target must leave the band untouched, so the
// rule can never invent a poll from missing data. Same fixture as the deviation case, so the verdict is
// provably the only difference.
func TestClassifyActivityAbsentVerdictLeavesTheBandUnchanged(t *testing.T) {
	acts := NewActivities(priorVerdictDeps("web01", nil))
	d, err := acts.ClassifyActivity(context.Background(), classifyIn("HostDown"))
	if err != nil {
		t.Fatal(err)
	}
	if d.Band != safety.BandAuto {
		t.Fatalf("no prior verdict must leave the pre-feature band (AUTO), got %+v", d)
	}
	if _, recorded := d.Signals["prior_verdict_key"]; recorded {
		t.Fatal("a decision this rule did not drive must carry no prior-verdict evidence — the key identifies " +
			"the decisions the rule fired on, it does not decorate every row")
	}
	// A completely unwired seam (no DSN in production) must behave identically.
	nilDeps := testDeps(proposeWeb01)
	nilDeps.PriorVerdicts = nil
	if d, err := NewActivities(nilDeps).ClassifyActivity(context.Background(), classifyIn("HostDown")); err != nil || d.Band != safety.BandAuto {
		t.Fatalf("an unwired prior-verdict seam must be inert, got %+v (err=%v)", d, err)
	}
}

// A READ ERROR must not fail the classification OPEN, and must not fail it CLOSED either: it degrades to
// exactly the pre-feature decision, and says so in the log. A safety input that dies silently is the failure
// mode this project pays for most often, so the log line is part of the contract.
func TestClassifyActivityVerdictReadErrorLeavesTheBandUnchangedAndIsLogged(t *testing.T) {
	deps := testDeps(proposeWeb01)
	deps.PriorVerdicts = func(context.Context, string) ([]PriorVerdict, error) {
		return nil, errors.New("connection refused")
	}
	logged := captureLog(t)
	d, err := NewActivities(deps).ClassifyActivity(context.Background(), classifyIn("HostDown"))
	if err != nil {
		t.Fatalf("a prior-verdict read error must never fail the classification: %v", err)
	}
	if d.Band != safety.BandAuto {
		t.Fatalf("an unreadable verdict ledger must leave the pre-feature band (AUTO), got %+v — failing the "+
			"read closed would let a DB hiccup poll the whole fleet", d)
	}
	if out := logged(); !strings.Contains(out, "prior-verdict read failed") || !strings.Contains(out, "connection refused") {
		t.Fatalf("the read failure must be LOGGED, not swallowed; got %q", out)
	}
}

// A MATCH or a PARTIAL is not adverse and must change nothing. `partial` is deliberately non-adverse here and
// the choice is not arbitrary: the graduation ladder maps partial to OutcomeUnverified — neither promoting nor
// demoting — so treating it as adverse at classification would make the two gates disagree about the same
// verdict.
func TestClassifyActivityNonAdverseVerdictsDoNotTightenTheBand(t *testing.T) {
	for _, v := range []safety.Verdict{safety.VerdictMatch, safety.VerdictPartial} {
		acts := NewActivities(priorVerdictDeps("web01", []PriorVerdict{
			{Verdict: v, AlertRule: "HostDown", At: time.Now().Add(-time.Hour)},
		}))
		d, err := acts.ClassifyActivity(context.Background(), classifyIn("HostDown"))
		if err != nil {
			t.Fatal(err)
		}
		if d.Band != safety.BandAuto {
			t.Fatalf("a %q verdict must not tighten the band, got %+v", v, d)
		}
	}
}

// RULE-FAMILY SCOPING, both directions. A deviation on a DIFFERENT fault must not tighten this remediation
// (or the rule degenerates into "this host is untouchable"), and a deviation recorded under a family SIBLING
// spelling MUST tighten it (or the two-vocabulary drift that made the recovery belt answer "not recovered"
// forever silently disarms the rule on exactly the fleet's most common fault).
func TestClassifyActivityVerdictIsScopedToTheRuleFamily(t *testing.T) {
	// Different fault entirely: a disk-full deviation says nothing about a host-down restart.
	unrelated := NewActivities(priorVerdictDeps("web01", []PriorVerdict{
		{Verdict: safety.VerdictDeviation, AlertRule: "DiskFull", At: time.Now().Add(-time.Hour)},
	}))
	if d, err := unrelated.ClassifyActivity(context.Background(), classifyIn("HostDown")); err != nil || d.Band != safety.BandAuto {
		t.Fatalf("a deviation on an unrelated rule must not tighten this band, got %+v (err=%v)", d, err)
	}
	// Family SIBLING: the incident fires as "HostDown" (Prometheus blackbox) while the deviation was recorded
	// under the LibreNMS spelling of the same physical fault. String equality would miss it; the family map
	// (core/knowledge.CanonicalRule) is what makes them the same condition.
	sibling := NewActivities(priorVerdictDeps("web01", []PriorVerdict{
		{Verdict: safety.VerdictDeviation, AlertRule: "Device-Down-SNMP-unreachable", At: time.Now().Add(-time.Hour)},
	}))
	d, err := sibling.ClassifyActivity(context.Background(), classifyIn("HostDown"))
	if err != nil {
		t.Fatal(err)
	}
	if d.Band != safety.BandPollPause || d.Signals["poll_reason"] != "verdict-deviation-or-invalid" {
		t.Fatalf("a deviation under a rule-FAMILY sibling spelling must tighten the band (the same physical "+
			"fault under another source's name), got %+v", d)
	}
}

// The NEWEST relevant verdict decides. A host that deviated yesterday and has since re-executed cleanly is
// not still pinned by the stale row — otherwise the recency bound would be the only way out of a poll, and a
// verified recovery would count for nothing.
func TestClassifyActivityTheNewestRelevantVerdictDecides(t *testing.T) {
	acts := NewActivities(priorVerdictDeps("web01", []PriorVerdict{
		{Verdict: safety.VerdictMatch, AlertRule: "HostDown", At: time.Now().Add(-time.Hour)},
		{Verdict: safety.VerdictDeviation, AlertRule: "Devices-up/down", At: time.Now().Add(-20 * time.Hour)},
	}))
	if d, err := acts.ClassifyActivity(context.Background(), classifyIn("HostDown")); err != nil || d.Band != safety.BandAuto {
		t.Fatalf("a newer clean verdict must supersede an older deviation, got %+v (err=%v)", d, err)
	}
	// …and the mirror: an older match must NOT mask a newer deviation.
	acts = NewActivities(priorVerdictDeps("web01", []PriorVerdict{
		{Verdict: safety.VerdictMatch, AlertRule: "HostDown", At: time.Now().Add(-20 * time.Hour)},
		{Verdict: safety.VerdictDeviation, AlertRule: "Devices-up/down", At: time.Now().Add(-time.Hour)},
	}))
	if d, err := acts.ClassifyActivity(context.Background(), classifyIn("HostDown")); err != nil || d.Band != safety.BandPollPause {
		t.Fatalf("a newer deviation must not be masked by an older match, got %+v (err=%v)", d, err)
	}
}

// A ledger row outside {match, partial, deviation} is CORRUPT evidence, and the classifier's standing rule is
// that an unknown verdict is treated as a deviation. It must reach the classifier as such rather than being
// silently dropped as if the target were clean.
func TestClassifyActivityAnInvalidLedgerVerdictIsTreatedAsADeviation(t *testing.T) {
	acts := NewActivities(priorVerdictDeps("web01", []PriorVerdict{
		{Verdict: safety.Verdict("garbled"), AlertRule: "HostDown", At: time.Now().Add(-time.Hour)},
	}))
	d, err := acts.ClassifyActivity(context.Background(), classifyIn("HostDown"))
	if err != nil {
		t.Fatal(err)
	}
	if d.Band != safety.BandPollPause || d.Signals["poll_reason"] != "verdict-deviation-or-invalid" {
		t.Fatalf("a corrupt verdict must be treated as a deviation (fail closed), got %+v", d)
	}
}

// An empty alert rule has no family to scope on. Querying anyway would fold every row to the empty family and
// let a deviation on ANY fault tighten the band, so the rule must stay inert instead — the same
// "no signature ⇒ do not fire" discipline the novelty gate uses.
func TestClassifyActivityAnEmptyAlertRuleDoesNotFireTheVerdictRule(t *testing.T) {
	acts := NewActivities(priorVerdictDeps("web01", []PriorVerdict{
		{Verdict: safety.VerdictDeviation, AlertRule: "", At: time.Now().Add(-time.Hour)},
	}))
	if d, err := acts.ClassifyActivity(context.Background(), classifyIn("")); err != nil || d.Band != safety.BandAuto {
		t.Fatalf("an incident with no alert rule must not fire the verdict rule, got %+v (err=%v)", d, err)
	}
}

// BOTH host expressions are consulted. The alerted device and the LLM-expressed action target alternate
// across proposals for the same fault (TG-124), so a verdict recorded under only one of them must still be
// found — consulting a single leg would silently drop half the evidence.
func TestClassifyActivityConsultsBothHostExpressions(t *testing.T) {
	// The deviation is recorded only under the PVE node, while the action targets the guest.
	acts := NewActivities(priorVerdictDeps("pve01", []PriorVerdict{
		{Verdict: safety.VerdictDeviation, AlertRule: "HostDown", At: time.Now().Add(-time.Hour)},
	}))
	in := classifyIn("HostDown")
	in.IncidentHost = "pve01" // the alerted device
	in.Host = "web01"         // the LLM-expressed action target
	d, err := acts.ClassifyActivity(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if d.Band != safety.BandPollPause {
		t.Fatalf("a verdict recorded under the OTHER host expression must still be found, got %+v", d)
	}
}
