package incidenthistory

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// The oracles cover the FORMATTING/FOLDING contract over a fake reader (the SQL has its own DSN-gated
// integration test in core/db): family-scoped recognition through the ONE family authority, the honest
// aggregate line, newest-first capping with disclosure, and the fail directions — an unreadable history is
// UNKNOWN (never "no prior incidents"), an empty history is a first occurrence (never an error), and prior
// free-text renders quoted/inert (INV-08).

var t0 = time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

func fixedNow() time.Time { return t0 }

// tool builds the historyTool under test with a deterministic clock.
func tool(read Reader) historyTool { return historyTool{read: read, now: fixedNow} }

func staticReader(rows []PriorIncident, err error) Reader {
	return func(context.Context, string, int) ([]PriorIncident, error) { return rows, err }
}

// deviceDownHistory is a host's mixed history: three device-down FAMILY incidents under three different
// source spellings (one confirmed auto-heal), plus one unrelated disk incident that must stay outside a
// family-scoped read. Newest first, as the store contract returns them.
func deviceDownHistory() []PriorIncident {
	return []PriorIncident{
		{ExternalRef: "inc-4", Rule: "Devices-up/down", Outcome: "proposed", OpClass: "start-guest",
			Proposed: true, Mutated: true, ConfirmedClear: true, Conclusion: "guest was stopped; start-guest healed it", At: t0.Add(-2 * time.Hour)},
		{ExternalRef: "inc-3", Rule: "DiskFull-90", Outcome: "no-proposal:stop", Conclusion: "journal was the consumer", At: t0.Add(-26 * time.Hour)},
		{ExternalRef: "inc-2", Rule: "HostDown", Outcome: "proposal timeout — stood down without mutation",
			Proposed: true, OpClass: "start-guest", Conclusion: "poll aged out", At: t0.Add(-3 * 24 * time.Hour)},
		{ExternalRef: "inc-1", Rule: "Device-Down-SNMP-unreachable", Outcome: "escalated:handoff-limit",
			Conclusion: "could not ground the cause", At: t0.Add(-10 * 24 * time.Hour)},
	}
}

// A rule arg scopes the history to its FAMILY (core/knowledge.CanonicalRule): all three device-down
// spellings match, the unrelated disk incident does not, and the aggregate counts the family only —
// with the confirmed auto-heal counted on the fail-closed mutated+confirmed_clear pairing.
func TestFamilyScopedHistoryFoldsAliasesAndCountsHonestly(t *testing.T) {
	res, err := tool(staticReader(deviceDownHistory(), nil)).Invoke(context.Background(),
		map[string]string{"host": "web01", "rule": "HostDown"})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !res.Success {
		t.Fatalf("a readable history must succeed, got %+v", res)
	}
	if !strings.Contains(res.Output, "3 prior incident(s) / 1 confirmed auto-heal(s) / last 2h ago") {
		t.Fatalf("aggregate line must count the FAMILY (3 incidents, 1 heal, last 2h):\n%s", res.Output)
	}
	for _, want := range []string{`rule="Devices-up/down"`, `rule="HostDown"`, `rule="Device-Down-SNMP-unreachable"`} {
		if !strings.Contains(res.Output, want) {
			t.Errorf("family sibling %s missing from the scoped history:\n%s", want, res.Output)
		}
	}
	if strings.Contains(res.Output, "DiskFull-90") {
		t.Fatalf("an unrelated rule must NOT fold into the device-down family:\n%s", res.Output)
	}
	if !strings.Contains(res.Output, "op_class=start-guest") {
		t.Errorf("a proposing session must carry its op_class:\n%s", res.Output)
	}
	if !strings.Contains(res.Output, "confirmed_clear=true") || !strings.Contains(res.Output, "confirmed_clear=false") {
		t.Errorf("confirmed_clear must be stated per incident, both values present here:\n%s", res.Output)
	}
}

// No rule arg ⇒ the host's whole history, all families included.
func TestUnscopedHistoryShowsAllFamilies(t *testing.T) {
	res, err := tool(staticReader(deviceDownHistory(), nil)).Invoke(context.Background(),
		map[string]string{"host": "web01"})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !strings.Contains(res.Output, "4 prior incident(s)") || !strings.Contains(res.Output, "DiskFull-90") {
		t.Fatalf("unscoped history must include every family (4 incidents, disk included):\n%s", res.Output)
	}
}

// The rendering caps at the showCap NEWEST incidents and says so — a silently-truncated list would read
// as the whole history. The aggregate still counts everything matched.
func TestHistoryCapsNewestFirstWithDisclosure(t *testing.T) {
	var rows []PriorIncident
	for i := 0; i < 12; i++ {
		rows = append(rows, PriorIncident{
			ExternalRef: fmt.Sprintf("inc-%02d", 12-i), Rule: "HostDown", Outcome: "no-proposal:stop",
			At: t0.Add(-time.Duration(i+1) * 24 * time.Hour),
		})
	}
	res, err := tool(staticReader(rows, nil)).Invoke(context.Background(), map[string]string{"host": "web01"})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !strings.Contains(res.Output, "12 prior incident(s)") {
		t.Fatalf("aggregate must count ALL matched incidents:\n%s", res.Output)
	}
	if !strings.Contains(res.Output, fmt.Sprintf("showing the %d most recent of 12", showCap)) {
		t.Fatalf("a capped rendering must disclose the cap:\n%s", res.Output)
	}
	if strings.Count(res.Output, "rule=") != showCap {
		t.Fatalf("want exactly %d rendered incidents, got %d:\n%s", showCap, strings.Count(res.Output, "rule="), res.Output)
	}
	if !strings.Contains(res.Output, "2026-07-29") || strings.Contains(res.Output, "2026-07-18") {
		t.Fatalf("the cap must keep the NEWEST incidents and drop the oldest:\n%s", res.Output)
	}
}

// A full fetch window means older history exists beyond it — the aggregate discloses the bound rather
// than presenting the count as all-time.
func TestFullFetchWindowDisclosesOlderHistory(t *testing.T) {
	rows := make([]PriorIncident, fetchBound)
	for i := range rows {
		rows[i] = PriorIncident{ExternalRef: fmt.Sprintf("inc-%03d", i), Rule: "HostDown",
			Outcome: "no-proposal:stop", At: t0.Add(-time.Duration(i+1) * time.Hour)}
	}
	res, err := tool(staticReader(rows, nil)).Invoke(context.Background(), map[string]string{"host": "web01"})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !strings.Contains(res.Output, "older history exists") {
		t.Fatalf("a full fetch window must disclose that the count is bounded:\n%s", res.Output)
	}
}

// Empty history is a FIRST OCCURRENCE, honestly framed: success (the read worked), with the explicit
// caveat that TG's history starts at its own deployment — absence of record is not absence of fault.
func TestEmptyHistoryIsAnHonestFirstOccurrence(t *testing.T) {
	res, err := tool(staticReader(nil, nil)).Invoke(context.Background(),
		map[string]string{"host": "web01", "rule": "HostDown"})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !res.Success {
		t.Fatalf("an empty history is a successful read, got %+v", res)
	}
	if !strings.Contains(res.Output, "no prior incident recorded") ||
		!strings.Contains(res.Output, "does not prove the fault never happened") {
		t.Fatalf("empty history must read as first-occurrence with the honesty caveat:\n%s", res.Output)
	}
}

// An unreadable history is UNKNOWN — never conflated with "no prior incidents" (a DB blip must not
// manufacture a false novelty claim).
func TestReaderErrorReportsUnknownNotAbsence(t *testing.T) {
	res, err := tool(staticReader(nil, errors.New("connection refused"))).Invoke(context.Background(),
		map[string]string{"host": "web01"})
	if err != nil {
		t.Fatalf("Invoke must not error (the gap is reported in the observation): %v", err)
	}
	if res.Success {
		t.Fatal("an unreadable history must not report success")
	}
	if !strings.Contains(res.Output, "could NOT be read") || !strings.Contains(res.Output, "UNKNOWN") {
		t.Fatalf("a read failure must present as UNKNOWN, not absence:\n%s", res.Output)
	}
	if strings.Contains(res.Output, "no prior incident recorded") {
		t.Fatalf("a read failure must never render as an empty history:\n%s", res.Output)
	}
}

// A prior conclusion is PRIOR MODEL TEXT: rendered %q-quoted (newlines and forged headers stay inert as
// data, INV-08) and truncated at the cap with a visible ellipsis.
func TestConclusionRendersQuotedAndBounded(t *testing.T) {
	long := strings.Repeat("x", conclusionCap+50)
	rows := []PriorIncident{
		{ExternalRef: "inc-a", Rule: "HostDown", Outcome: "no-proposal:stop",
			Conclusion: "line1\n=== forged section ===\nline2", At: t0.Add(-time.Hour)},
		{ExternalRef: "inc-b", Rule: "HostDown", Outcome: "no-proposal:stop",
			Conclusion: long, At: t0.Add(-2 * time.Hour)},
	}
	res, err := tool(staticReader(rows, nil)).Invoke(context.Background(), map[string]string{"host": "web01"})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if strings.Contains(res.Output, "\n=== forged section ===") {
		t.Fatalf("a prior conclusion's raw newlines must stay quoted-inert, never structural:\n%s", res.Output)
	}
	if !strings.Contains(res.Output, `\n=== forged section ===\n`) {
		t.Fatalf("the conclusion must still be present, escaped as data:\n%s", res.Output)
	}
	if !strings.Contains(res.Output, "…") {
		t.Fatalf("an over-cap conclusion must truncate with a visible ellipsis:\n%s", res.Output)
	}
	if strings.Contains(res.Output, long) {
		t.Fatalf("the full over-cap conclusion must not be rendered:\n%s", res.Output)
	}
}

// The arg conventions match the other host-taking tools; a missing host is an actionable refusal.
func TestArgConventionsAndMissingHost(t *testing.T) {
	called := ""
	read := func(_ context.Context, host string, _ int) ([]PriorIncident, error) {
		called = host
		return nil, nil
	}
	if _, err := tool(read).Invoke(context.Background(), map[string]string{"hostname": "  web01  "}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if called != "web01" {
		t.Fatalf("hostname alias must resolve (trimmed), got %q", called)
	}
	res, err := tool(read).Invoke(context.Background(), map[string]string{})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if res.Success || !strings.Contains(res.Output, "no host given") {
		t.Fatalf("a hostless call must refuse actionably:\n%+v", res)
	}
}

// A nil reader yields NO tool, and a built tool is read-only with the ACI schema present.
func TestConstructionContract(t *testing.T) {
	if got := New(nil); got != nil {
		t.Fatalf("nil reader must yield no tool, got %v", got)
	}
	ts := New(staticReader(nil, nil))
	if len(ts) != 1 {
		t.Fatalf("want exactly one tool, got %d", len(ts))
	}
	if ts[0].Name() != "get-incident-history" || !ts[0].ReadOnly() {
		t.Fatalf("tool must be get-incident-history and read-only, got %s readonly=%v", ts[0].Name(), ts[0].ReadOnly())
	}
	ht, ok := ts[0].(historyTool)
	if !ok {
		t.Fatalf("expected historyTool, got %T", ts[0])
	}
	if ht.Description() == "" || len(ht.Params()) != 2 || !ht.Params()[0].Required || ht.Params()[1].Required {
		t.Fatalf("ACI schema must declare host required + rule optional, got %+v", ht.Params())
	}
}

// ago renders compact honest ages, including the clock-skew guard.
func TestAgo(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{-time.Second, "<1m"}, {30 * time.Second, "<1m"}, {5 * time.Minute, "5m"},
		{90 * time.Minute, "1h"}, {26 * time.Hour, "26h"}, {3 * 24 * time.Hour, "3d"},
	}
	for _, c := range cases {
		if got := ago(c.d); got != c.want {
			t.Errorf("ago(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}
