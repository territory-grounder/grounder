package main

// TG-694: a guest's power state has one witness, the PVE task log. For a guest-lifecycle heal a failed subject read
// in another domain (the NIC-less pool guests have no journal, no AWX host, no NetBox object) is noted, not blinding
// — but only when the task log covered the subject. Every other class keeps the strict every-reader rule.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/adapters/actorevidence"
	"github.com/territory-grounder/grounder/core/actuate/opschema"
	"github.com/territory-grounder/grounder/core/attribution"
	"github.com/territory-grounder/grounder/temporal/runner"
)

func tg694Classify(t *testing.T, opClass string, readers ...actorevidence.Reader) (string, runner.CausalityEvidence) {
	t.Helper()
	cfg := attribution.Config{SelfActors: map[string]string{"pve": "tg-actuate@pve!lane"}, Sanctioned: map[string][]string{"pve": {"root@pam"}}}
	q := tg662Query()
	q.OpClass = opClass
	ev, _ := causalityEvidenceReader(&tg662Injector{}, &tg662Recovery{}, readers, cfg)(context.Background(), q)
	acts := runner.NewActivities(runner.Deps{CausalityEvidence: func(context.Context, runner.CausalityQuery) (runner.CausalityEvidence, error) { return ev, nil }})
	out, _ := acts.MarkTerminusFactsActivity(context.Background(), runner.MarkTerminusFactsInput{ExternalRef: "r", Mutated: true, ConfirmedClear: true,
		CollateralKnown: true, VerdictMatch: true, Host: "guest01", TargetHost: "pvenode01", AlertRule: "Devices-up/down", OpClass: opClass})
	return out.Causality, ev
}

// The live shape (dc1tgpool02/03): the three non-PVE readers fail for the subject, as they do on a guest outside
// the journal allowlist, AWX and NetBox.
func tg694BlindElsewhere() []actorevidence.Reader {
	return []actorevidence.Reader{
		&tg662ActorReader{domain: "journal", errFor: map[string]error{"guest01": errors.New(`journal: host "guest01" is not in the operator read allowlist`)}},
		&tg662ActorReader{domain: "awx", errFor: map[string]error{"guest01": errors.New(`awx: host "guest01" not found in AWX (fail closed)`)}},
		&tg662ActorReader{domain: "netbox", errFor: map[string]error{"guest01": errors.New(`netbox: "guest01" is neither a device nor a virtual machine (fail closed)`)}},
	}
}

func tg694PVE(byHost map[string][]attribution.Evidence) *tg662ActorReader {
	return &tg662ActorReader{domain: "pve", byHost: byHost, covers: []string{"guest01", "pvenode01"}}
}

// KILLING MUTATIONS in causalityEvidenceReader: drop the deferral (every subject failure blinds, the pre-TG-694 rule)
// — RED on "the NIC-less guest"; drop the witnessCovered condition (deferred failures never blind) — RED on "no
// task-log reader is wired" (a FAILED task-log read blinds on its own arm, so "the task log failed" guards that arm,
// not this condition); apply the deferral to every class — RED on "a service heal" and "a stop-guest heal".
func TestAGuestsPowerStateIsJudgedOnItsTaskLog(t *testing.T) {
	selfOnly := map[string][]attribution.Evidence{"guest01": {tg662Record("tg-actuate@pve!lane", "vzstart")}}
	cases := []struct {
		name    string
		opClass string
		readers []actorevidence.Reader
		want    string
	}{
		{"the NIC-less guest: task log covers it, the other domains cannot see it", "start-guest",
			append(tg694BlindElsewhere(), tg694PVE(selfOnly)), runner.CausalityTGCaused},
		{"the same with nothing but the coverage marker in the task log", "start-guest",
			append(tg694BlindElsewhere(), tg694PVE(nil)), runner.CausalityTGCaused},
		{"an operator's start in the task log still wins", "start-guest",
			append(tg694BlindElsewhere(), tg694PVE(map[string][]attribution.Evidence{"guest01": {tg662Record("root@pam", "vzstart")}})), runner.CausalityExternal},
		{"the task log failed for the subject", "start-guest",
			append(tg694BlindElsewhere(), &tg662ActorReader{domain: "pve", errFor: map[string]error{"guest01": errors.New("pve: 596")}}), runner.CausalityIndeterminate},
		{"no task-log reader is wired, another reader covers the subject", "start-guest",
			[]actorevidence.Reader{
				&tg662ActorReader{domain: "journal", errFor: map[string]error{"guest01": errors.New("journal: refused")}},
				&tg662ActorReader{domain: "awx", covers: []string{"guest01"}},
			}, runner.CausalityIndeterminate},
		{"a stop-guest heal keeps the strict rule: a running guest can stop itself with no task", "stop-guest",
			append(tg694BlindElsewhere(), tg694PVE(nil)), runner.CausalityIndeterminate},
		{"a service heal keeps the strict rule: a failed journal read blinds", "start-service",
			append(tg694BlindElsewhere(), tg694PVE(nil)), runner.CausalityIndeterminate},
		{"an unknown class keeps the strict rule", "tg694-no-such-class",
			append(tg694BlindElsewhere(), tg694PVE(nil)), runner.CausalityIndeterminate},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ev := tg694Classify(t, tc.opClass, tc.readers...)
			if got != tc.want {
				t.Fatalf("causality %q, want %q (notes %v)", got, tc.want, ev.Notes)
			}
		})
	}
}

// The non-blinding failures are recorded, not dropped: the terminus evidence must still say which domains could not
// see the guest. KILLING MUTATION: skip the note for a non-blinding failure — RED here.
func TestANonBlindingFailureIsStillRecorded(t *testing.T) {
	_, ev := tg694Classify(t, "start-guest", append(tg694BlindElsewhere(), tg694PVE(nil))...)
	joined := strings.Join(ev.Notes, " | ")
	for _, d := range []string{"journal", "awx", "netbox"} {
		if !strings.Contains(joined, "actor reader "+d+" failed for the subject guest01") || !strings.Contains(joined, "not blinding") {
			t.Fatalf("the %s failure must be noted as not blinding, got %q", d, joined)
		}
	}
	if !ev.ActorsKnown || !ev.ActorCovered {
		t.Fatalf("with the task log covering the subject the reading is known and covered, got known=%v covered=%v", ev.ActorsKnown, ev.ActorCovered)
	}
	if !strings.Contains(joined, "coverage marker only") {
		t.Fatalf("a marker-only task log must be said out loud in the evidence, got %q", joined)
	}
	// With a record in the task log the marker-only note is absent.
	_, ev = tg694Classify(t, "start-guest", append(tg694BlindElsewhere(), tg694PVE(map[string][]attribution.Evidence{
		"guest01": {tg662Record("tg-actuate@pve!lane", "vzstart")}}))...)
	if strings.Contains(strings.Join(ev.Notes, " | "), "coverage marker only") {
		t.Fatalf("a task log with a record is not marker-only, got %v", ev.Notes)
	}
}

// Only the power-on class takes the task-log rule. Control: start-guest is still the registry's guest-lifecycle start.
func TestOnlyThePowerOnClassIsJudgedOnTheTaskLog(t *testing.T) {
	for oc, want := range map[string]bool{"start-guest": true, "stop-guest": false, "start-service": false, "restart-container": false, "": false} {
		if got := isGuestPowerOn(oc); got != want {
			t.Errorf("isGuestPowerOn(%q) = %v, want %v", oc, got, want)
		}
	}
	if spec, ok := opschema.Lookup("start-guest"); !ok || spec.Family != opschema.FamilyGuestLifecycle || spec.Op != "start" {
		t.Fatalf("control: start-guest must be the registry's guest-lifecycle start, got ok=%v %+v", ok, spec)
	}
}
