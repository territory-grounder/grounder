package runner

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/agent"
	"github.com/territory-grounder/grounder/core/actuate"
	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/manifest"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/verify"
)

// necessity_ledger_belt_test.go — TG-454, at the SAME wiring seam necessity_wire_test.go proves. That file
// pins that the runner SUPPLIES the necessity probe and reads its (present, ok) honestly. This one pins what
// the probe does when the LIVE active-alert surface is UNREADABLE (a fetch/token/HTTP error from LibreNMS's
// FetchActive), which on the actuate plane is the STEADY-STATE, not a rare blip: ClearObserve returns ok=false
// on every approved service heal, so before this belt EVERY such heal ended Executed=false with "the fault
// could not be re-observed at execute time (read error)".
//
// The asymmetry that was the bug: the CLEAR direction (ObserveClearedActivity) already carries a durable belt
// for exactly this failure mode — a.D.OpenIncidents, TG's OWN open-incident ledger (cmd/worker/main.go,
// "Durable — it does not share the LibreNMS HTTP surface's failure mode"). The NECESSITY direction had no such
// belt, so a single unreadable HTTP surface refused the heal AND (via the clear-check) refused to auto-close —
// a dead-end. TG-454 gives the necessity probe the SAME belt, bounded hard:
//
//   • read-error ONLY  — the belt is consulted solely on the ClearObserve ok=false branch. A live reading that
//     SUCCEEDS is authoritative in both directions; the ledger never gets a vote when the live surface spoke.
//   • positive ONLY    — only a POSITIVE ledger hit (this host still carries an un-recovered incident) re-
//     confirms necessity. An unreadable ledger, a silent ledger, or a ledger naming only OTHER hosts all keep
//     the original read-error refusal. The belt can only ever RESCUE a no-live-signal refusal, never mint one.
//
// GROUNDING: validated against the real failed heal — incident librenms-dc1-184763, host
// dc1librespeed01. The alert sat in TG's own ingest ledger at execute time and did not recover until 16
// minutes later, so OpenIncidents would have correctly re-confirmed necessity while ClearObserve could not read
// it at all.
//
// KILLING EVIDENCE (RED): stub the TG-454 `if !ok` ledger fallback in ExecuteActivity back to a bare
// `return false, false` and TestNecessityBeltReadErrorWithLedgerOpenHeals +
// TestNecessityBeltLedgerMatchIsCaseAndSpaceInsensitive FAIL — the read-error path refuses even though TG's own
// durable ledger positively shows the fault still open. That is the live defect, exactly. Restored → green.

// executeWithBelt runs ONE ExecuteActivity against a fully-grounded sealed restart-service action on `target`,
// with the supplied ClearObserve (the LIVE active-alert surface) AND OpenIncidents (the durable ledger belt)
// wiring, and reports the result plus how many times the effect leaf was actually reached. It mirrors
// necessity_wire_test.go's executeWith, adding the OpenIncidents seam TG-454 introduces and a parametrised
// target so the case/space-insensitivity case can use the real incident host.
//
// The pair-arm baseline (PostStateObserve) returns ok=true, so the baseline gate ALWAYS establishes and never
// masks the necessity verdict — even though wiring OpenIncidents also arms the verifier's host baseline
// (req.PreAnomalous, the SAME reader). That shared arm is precisely why a "fail the test if OpenIncidents is
// called at all" fake cannot express case 2 at THIS seam: the baseline gate reads OpenIncidents on every
// execution, so live-quiet's "the ledger must not override me" invariant is pinned behaviourally instead — a
// ledger screaming OPEN must still not flip a live quiet into a mutation.
func executeWithBelt(
	t *testing.T,
	target string,
	clear func(context.Context, string, string) ([]verify.ObservedAlert, bool),
	open func(context.Context, time.Time) (map[string]bool, bool),
) (ExecuteResult, int) {
	t.Helper()
	ctx := context.Background()
	gate := safety.NewActuatingChokepoint() // mutation ON (test-only)
	act := &recordingActuator{}
	m, err := manifest.New(
		manifest.Action{Target: target, OpClass: "restart-service", Op: "restart", Params: map[string]string{"unit": "nginx"}, Reversible: true},
		safety.BandAuto, "plan#belt", "pred#belt")
	if err != nil {
		t.Fatalf("seal manifest: %v", err)
	}
	sink := &fakeManifestSink{}
	if err := sink.Seal(ctx, m); err != nil {
		t.Fatalf("seal: %v", err)
	}
	deps := Deps{
		Interceptor: withPermissivePolicy(actuate.NewInterceptor(gate, act, audit.NewLedger())),
		Manifests:   sink,
		Mutation:    gate,
		PostStateObserve: func(context.Context, string, string) ([]verify.ObservedAlert, bool) {
			return []verify.ObservedAlert{}, true
		},
		ClearObserve:  clear,
		OpenIncidents: open,
	}
	res, err := NewActivities(deps).ExecuteActivity(ctx, ExecuteInput{
		ActionID: m.ActionID, ExternalRef: "TG-454", PlanHash: "plan#belt", TargetHost: target, Site: "nl",
		Band:        safety.BandAuto,
		EvidenceIDs: []string{"tr-1"},
		ToolResults: []agent.ToolResult{{ID: "tr-1", Target: target, Output: target + " nginx is failed", Success: true}},
	})
	if err != nil {
		t.Fatalf("execute must be a recorded refusal or an execution, never an error: %v", err)
	}
	return res, act.execs
}

// ClearObserve fakes — the LIVE active-alert surface.
func liveAlerting(host string) func(context.Context, string, string) ([]verify.ObservedAlert, bool) {
	return func(context.Context, string, string) ([]verify.ObservedAlert, bool) {
		return []verify.ObservedAlert{{Host: host, Rule: "ServiceFault", Site: "nl"}}, true // still faulted, live-confirmed
	}
}
func liveQuiet(context.Context, string, string) ([]verify.ObservedAlert, bool) {
	return nil, true // the host is QUIET at execute time — a SUCCESSFUL live read that says "no active alert"
}
func liveReadError(context.Context, string, string) ([]verify.ObservedAlert, bool) {
	return nil, false // fetch/token/HTTP error — the surface could not be read (this is NOT a clear)
}

// OpenIncidents fakes — TG's OWN durable ledger belt.
func ledgerHolds(hosts ...string) func(context.Context, time.Time) (map[string]bool, bool) {
	return func(context.Context, time.Time) (map[string]bool, bool) {
		m := make(map[string]bool, len(hosts))
		for _, h := range hosts {
			m[h] = true
		}
		return m, true
	}
}
func ledgerUnreadable(context.Context, time.Time) (map[string]bool, bool) {
	return nil, false // the durable store could not be read either
}

// CASE 1 — LIVE STILL-ALERTING HEALS. The live surface reads fine and shows the target still faulted, so the
// heal proceeds. The ledger says CLEAR here, and must NOT suppress a live positive: a successful live read is
// authoritative, the belt does not get a vote.
func TestNecessityBeltLiveStillAlertingHeals(t *testing.T) {
	res, execs := executeWithBelt(t, "web01", liveAlerting("web01"), ledgerHolds( /* empty: ledger clear */ ))
	if !res.Executed || execs != 1 {
		t.Fatalf("a host whose fault is STILL present on the LIVE surface must be healed regardless of the ledger; "+
			"a successful live read is authoritative: %+v execs=%d", res, execs)
	}
}

// CASE 2 — A LIVE QUIET READING IS NEVER OVERRIDDEN BY THE LEDGER. This is the safety spine of TG-454. The live
// surface reads fine and says the target is quiet ⇒ (false, true) ⇒ refuse. The ledger is wired to scream that
// the target is STILL OPEN — the laggier durable store can trail a real recovery by minutes — and it MUST NOT
// flip the refusal into a mutation on a healed box. The belt is a read-error-only rescue; a genuine live clear
// closes the incident through the normal clear-check, not a restart.
//
// (Why not "fail the test if OpenIncidents is called"? At this seam OpenIncidents also arms the verifier's host
// baseline (req.PreAnomalous), so it is legitimately read on every execution. The invariant that actually
// matters — the ledger cannot override a live quiet — is stronger stated as behaviour: OPEN ledger + live quiet
// ⇒ still refuse. A regression that consulted the belt on a successful live read would read OPEN here and
// EXECUTE, which res.Executed==false catches.)
func TestNecessityBeltLiveQuietIsNeverOverriddenByTheLedger(t *testing.T) {
	res, execs := executeWithBelt(t, "web01", liveQuiet, ledgerHolds("web01" /* ledger insists it is still open */))
	if res.Executed || execs != 0 {
		t.Fatalf("a LIVE quiet reading must refuse even when the durable ledger still shows the host open — the belt "+
			"rescues a read ERROR, it must never override a successful live clear onto a healthy box: %+v execs=%d", res, execs)
	}
	if !strings.Contains(res.Note, "NO LONGER NECESSARY") {
		t.Fatalf("a live-quiet refusal must be the necessity gate's NO LONGER NECESSARY, not a laundered read-error "+
			"or ledger verdict: %q", res.Note)
	}
}

// CASE 3 — READ-ERROR + LEDGER OPEN HEALS. THE TG-454 FIX, and the RED case against current code. The live
// surface is unreadable (ok=false); TG's durable ledger positively holds the target's incident open ⇒ the belt
// re-confirms necessity ⇒ (true, true) ⇒ heal. Before TG-454 this returned (false, false) and every approved
// service heal died here as "could not be re-observed", because ClearObserve fails on the actuate plane
// EVERY time.
func TestNecessityBeltReadErrorWithLedgerOpenHeals(t *testing.T) {
	res, execs := executeWithBelt(t, "web01", liveReadError, ledgerHolds("web01"))
	if !res.Executed || execs != 1 {
		t.Fatalf("with the live surface unreadable and TG's OWN durable ledger positively showing the fault still "+
			"open, the heal MUST proceed — this is the belt that unblocks the service-fault auto-heal path: %+v execs=%d", res, execs)
	}
}

// CASE 4 — READ-ERROR + LEDGER CLEAR REFUSES. The live surface is unreadable AND the durable ledger names only
// OTHER hosts (never the target) ⇒ no positive confirmation from either source ⇒ (false, false) ⇒ refuse. The
// belt must not match a host it was never given; a ledger that does not hold THIS target is not a licence.
func TestNecessityBeltReadErrorWithLedgerClearRefuses(t *testing.T) {
	res, execs := executeWithBelt(t, "web01", liveReadError, ledgerHolds("some-other-host"))
	if res.Executed || execs != 0 {
		t.Fatalf("an unreadable live surface AND a ledger that does not hold THIS target open is no confirmation at "+
			"all — the mutation must be refused: %+v execs=%d", res, execs)
	}
	if !strings.Contains(res.Note, "could not be re-observed") {
		t.Fatalf("with no positive ledger hit the refusal must remain the read-error refusal, not be laundered into "+
			"a clear: %q", res.Note)
	}
}

// CASE 5 — READ-ERROR + LEDGER UNREADABLE REFUSES. Both TG's live surface AND its durable ledger are
// unreadable ⇒ the belt offers nothing ⇒ (false, false) ⇒ refuse. Two unreadable stores are not evidence of a
// healthy estate; the fail-closed floor holds.
func TestNecessityBeltReadErrorWithLedgerUnreadableRefuses(t *testing.T) {
	res, execs := executeWithBelt(t, "web01", liveReadError, ledgerUnreadable)
	if res.Executed || execs != 0 {
		t.Fatalf("with BOTH the live surface and the durable ledger unreadable, necessity cannot be confirmed and the "+
			"mutation must be refused — an unreadable belt is not a licence: %+v execs=%d", res, execs)
	}
	if !strings.Contains(res.Note, "could not be re-observed") {
		t.Fatalf("an unreadable belt must keep the read-error refusal, not invent a different verdict: %q", res.Note)
	}
}

// CASE 6 — THE BELT MATCH IS CASE- AND SPACE-INSENSITIVE, on the REAL incident host. Read-error, and the ledger
// key is " NLLEI01librespeed01 " (surrounding spaces, upper-cased) while the target is "dc1librespeed01".
// EqualFold(TrimSpace(...)) must still confirm the hit ⇒ heal. This locks the exact matching the fix ships and
// is ALSO RED against current code (read-error ⇒ refuse). librenms-dc1-184763 is the incident this rescues.
func TestNecessityBeltLedgerMatchIsCaseAndSpaceInsensitive(t *testing.T) {
	res, execs := executeWithBelt(t, "dc1librespeed01", liveReadError, ledgerHolds(" NLLEI01librespeed01 "))
	if !res.Executed || execs != 1 {
		t.Fatalf("a ledger key differing only by case and surrounding whitespace must still confirm the target and "+
			"heal — the belt matches on EqualFold(TrimSpace), like targetRelevant does: %+v execs=%d", res, execs)
	}
}
