package main

// TG-662 causality composer: every source is read POSITIVELY — a failed reader leaves its Known=false, it never
// contributes a silent zero; the actor reading is known only when every wired reader answered for every host,
// COVERED only when a reader affirmatively covered the SUBJECT's audit trail, and attribution runs PER host with
// the worst candidate taxonomy winning (review of !1819, round 2: {self, sanctioned admin} must not read as
// nobody; a record on the action's target must not be dropped).
//
// KILLING MUTATIONS (executed 2026-11-07, whole-package runs, restored from a copy):
//   M10 causalityEvidenceReader: attribute every host's records against the SUBJECT (`Attribute(q.Host, …)` in the
//       per-host loop) — the target's records are dropped by the core's subject filter. RED:
//       TestCausalityEvidenceReaderReadsEverySourcePositively ("a record on the action's TARGET is attributed") and
//       TestCausalityWireClassifiesTheReviewCases.
//   M11 causalityEvidenceReader: drop the worst-of fold over f.Candidates (read Taxonomy alone). RED: the same two
//       tests ("a sanctioned admin beside TG's own action" reads unattributable).
//   M14 causalityEvidenceReader: count nothing as unattributed (drop the `records - len(f.Evidence)` line). RED:
//       TestCausalityEvidenceReaderReadsEverySourcePositively ("a record stamped with another identity") and
//       TestCausalityWireClassifiesTheReviewCases.
//   M15 causalityEvidenceReader: treat a failing TARGET read like a failing subject read (blinding). RED: the two
//       "target read error" cases.
//   M16 causalityEvidenceReader: drop the `strings.EqualFold(h, q.Host)` guard on ActorCovered (a marker on the target
//       covers the subject). RED: "a coverage marker only on the TARGET" and the review case. Review round 4.
//   M17 causalityEvidenceReader: note EVERY target read error (drop the not-found distinction). RED: "a TARGET read
//       that fails for any other reason blinds". Review round 4.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/territory-grounder/grounder/adapters/actorevidence"
	"github.com/territory-grounder/grounder/core/attribution"
	"github.com/territory-grounder/grounder/temporal/runner"
)

type tg662Injector struct {
	restores, injections int
	err                  error
	hosts                []string
}

func (f *tg662Injector) InjectorActivity(_ context.Context, hosts []string, _, _, _ time.Time) (int, int, error) {
	f.hosts = hosts
	return f.restores, f.injections, f.err
}

type tg662Recovery struct {
	n   int
	err error
}

func (f *tg662Recovery) RecoveriesBetween(context.Context, string, string, time.Time, time.Time) (int, error) {
	return f.n, f.err
}

// tg662ActorReader answers per queried host: `byHost` holds the action records for a host (stamped with that host
// as Target, as the live readers do), `covers` lists the hosts it affirmatively covers (a coverage marker is
// emitted for them like modules/actorevidence/pve does on a clean miss).
type tg662ActorReader struct {
	domain string
	byHost map[string][]attribution.Evidence
	covers []string
	err    error
	errFor map[string]error
	stamp  string
}

func (r *tg662ActorReader) Domain() string { return r.domain }
func (r *tg662ActorReader) ReadOnly() bool { return true }
func (r *tg662ActorReader) Read(_ context.Context, target string, _, until time.Time) ([]attribution.Evidence, error) {
	if r.err != nil {
		return nil, r.err
	}
	if e, ok := r.errFor[target]; ok && e != nil {
		return nil, e
	}
	var out []attribution.Evidence
	for _, e := range r.byHost[target] {
		e.Target = target
		if r.stamp != "" {
			e.Target = r.stamp
		}
		out = append(out, e)
	}
	for _, h := range r.covers {
		if h == target && len(out) == 0 {
			out = append(out, attribution.CoverageMarker(r.domain, target, until))
		}
	}
	return out, nil
}

func tg662Query() runner.CausalityQuery {
	now := time.Now().UTC()
	return runner.CausalityQuery{Host: "guest01", TargetHost: "pvenode01", AlertRule: "Devices-up/down", OpClass: "start-guest",
		IncidentAt: now.Add(-20 * time.Minute), ExecutedAt: now.Add(-10 * time.Minute), ClearedAt: now.Add(-2 * time.Minute)}
}

func tg662Record(actor, kind string) attribution.Evidence {
	return attribution.Evidence{Domain: "pve", Actor: actor, ActionKind: kind, ObservedAt: time.Now().Add(-5 * time.Minute), Covered: true}
}

func TestCausalityEvidenceReaderReadsEverySourcePositively(t *testing.T) {
	cfg := attribution.Config{SelfActors: map[string]string{"pve": "tg-actuate@pve!lane"}, Sanctioned: map[string][]string{"pve": {"root@pam"}}}
	covering := func(byHost map[string][]attribution.Evidence) *tg662ActorReader {
		return &tg662ActorReader{domain: "pve", byHost: byHost, covers: []string{"guest01", "pvenode01"}}
	}
	read := func(inj *tg662Injector, rec *tg662Recovery, readers ...actorevidence.Reader) runner.CausalityEvidence {
		ev, err := causalityEvidenceReader(inj, rec, readers, cfg)(context.Background(), tg662Query())
		if err != nil {
			t.Fatal(err)
		}
		return ev
	}
	t.Run("all quiet, covered and known", func(t *testing.T) {
		inj := &tg662Injector{}
		ev := read(inj, &tg662Recovery{}, covering(nil))
		if !ev.InjectorKnown || !ev.RecoveryKnown || !ev.ActorsKnown || !ev.ActorCovered || ev.InjectorRestores != 0 || ev.RecoveredBeforeExecute || ev.ActorCandidates != 0 {
			t.Fatalf("quiet covering readers must read KNOWN, COVERED and empty, got %+v", ev)
		}
		if len(inj.hosts) != 2 || inj.hosts[0] != "guest01" || inj.hosts[1] != "pvenode01" {
			t.Fatalf("the injector must be asked about the subject AND the target, got %v", inj.hosts)
		}
	})
	t.Run("a reader that answers but covers nobody is blind, not quiet", func(t *testing.T) {
		ev := read(&tg662Injector{}, &tg662Recovery{}, &tg662ActorReader{domain: "pve"})
		if !ev.ActorsKnown || ev.ActorCovered {
			t.Fatalf("no coverage marker and no record ⇒ answered but NOT covered, got %+v", ev)
		}
	})
	t.Run("TG's own actuation is self", func(t *testing.T) {
		ev := read(&tg662Injector{}, &tg662Recovery{}, covering(map[string][]attribution.Evidence{"guest01": {tg662Record("tg-actuate@pve!lane", "vzstart")}}))
		if !ev.ActorsKnown || !ev.ActorCovered || ev.ActorTaxonomy != attribution.AttributedSelf.String() || ev.ActorCandidates != 1 {
			t.Fatalf("TG's own action must read attributed-self, got %+v", ev)
		}
	})
	t.Run("a sanctioned admin beside TG's own action is ANOTHER actor, not nobody", func(t *testing.T) {
		ev := read(&tg662Injector{}, &tg662Recovery{}, covering(map[string][]attribution.Evidence{
			"guest01": {tg662Record("tg-actuate@pve!lane", "vzstart"), tg662Record("root@pam", "vzstart")}}))
		if ev.ActorTaxonomy != attribution.AttributedAuthorized.String() || ev.ActorCandidates != 2 || ev.ActorActions != 2 {
			t.Fatalf("{self, authorized} must fold to attributed-authorized with 2 candidates, got %+v", ev)
		}
	})
	t.Run("a record on the action's TARGET is attributed, not dropped", func(t *testing.T) {
		ev := read(&tg662Injector{}, &tg662Recovery{}, covering(map[string][]attribution.Evidence{"pvenode01": {tg662Record("root@pam", "vzstart")}}))
		if ev.ActorTaxonomy != attribution.AttributedAuthorized.String() || ev.ActorActions != 1 {
			t.Fatalf("an admin acting on the target must read attributed-authorized, got %+v", ev)
		}
	})
	t.Run("an unknown principal is suspicious and dominates", func(t *testing.T) {
		ev := read(&tg662Injector{}, &tg662Recovery{}, covering(map[string][]attribution.Evidence{
			"guest01": {tg662Record("root@pam", "vzstart")}, "pvenode01": {tg662Record("stranger@pve", "vzstop")}}))
		if ev.ActorTaxonomy != attribution.AttributedSuspicious.String() {
			t.Fatalf("suspicious must dominate authorized across hosts, got %+v", ev)
		}
	})
	t.Run("a failed reader is unknown, never a silent zero", func(t *testing.T) {
		ev := read(&tg662Injector{err: errors.New("pg down")}, &tg662Recovery{err: errors.New("no aliases")},
			&tg662ActorReader{domain: "pve", err: errors.New("api 500")})
		if ev.InjectorKnown || ev.RecoveryKnown || ev.ActorsKnown || len(ev.Notes) != 4 {
			t.Fatalf("every failed source must read UNKNOWN with a note (2 actor notes: one per host), got %+v", ev)
		}
	})
	t.Run("one failed reader among two blinds the actor reading", func(t *testing.T) {
		ev := read(&tg662Injector{}, &tg662Recovery{}, covering(nil), &tg662ActorReader{domain: "journal", err: errors.New("openobserve 503")})
		if ev.ActorsKnown {
			t.Fatalf("one failed domain is one domain another actor could hide in, got %+v", ev)
		}
	})
	t.Run("a failing TARGET read is noted, not blinding", func(t *testing.T) {
		r := covering(nil)
		r.errFor = map[string]error{"pvenode01": errors.New("guest not found")}
		ev := read(&tg662Injector{}, &tg662Recovery{}, r)
		if !ev.ActorsKnown || !ev.ActorCovered || len(ev.Notes) != 1 {
			t.Fatalf("the target is a best-effort widening — its read error must not blind the subject's reading, got %+v", ev)
		}
	})
	t.Run("a TARGET read that fails for any other reason blinds", func(t *testing.T) {
		r := covering(nil)
		r.errFor = map[string]error{"pvenode01": errors.New("api 503")}
		ev := read(&tg662Injector{}, &tg662Recovery{}, r)
		if ev.ActorsKnown {
			t.Fatalf("a transient failure on the target is a domain another actor could hide in — it must blind, got %+v", ev)
		}
	})
	t.Run("a coverage marker only on the TARGET does not cover the subject", func(t *testing.T) {
		r := &tg662ActorReader{domain: "pve", covers: []string{"pvenode01"}}
		ev := read(&tg662Injector{}, &tg662Recovery{}, r)
		if !ev.ActorsKnown || ev.ActorCovered {
			t.Fatalf("coverage is read on the SUBJECT; a marker for the target alone leaves it uncovered, got %+v", ev)
		}
	})
	t.Run("a failing SUBJECT read blinds but keeps a sibling's positive evidence", func(t *testing.T) {
		// A SERVICE heal: the strict every-reader rule (a guest-lifecycle heal is judged on its task log, TG-694 —
		// causality_power_witness_test.go).
		blind := &tg662ActorReader{domain: "journal", errFor: map[string]error{"guest01": errors.New("openobserve 503")}}
		seen := covering(map[string][]attribution.Evidence{"guest01": {tg662Record("root@pam", "vzstart")}})
		q := tg662Query()
		q.OpClass = "start-service"
		ev, err := causalityEvidenceReader(&tg662Injector{}, &tg662Recovery{}, []actorevidence.Reader{blind, seen}, cfg)(context.Background(), q)
		if err != nil {
			t.Fatal(err)
		}
		if ev.ActorsKnown || ev.ActorTaxonomy != attribution.AttributedAuthorized.String() || ev.ActorActions != 1 {
			t.Fatalf("unknown quiet, but the admin seen by the other reader must survive the fold, got %+v", ev)
		}
	})
	t.Run("a record stamped with another identity is unattributed, never quiet", func(t *testing.T) {
		r := covering(map[string][]attribution.Evidence{"guest01": {tg662Record("root@pam", "vzstart")}})
		r.stamp = "101"
		ev := read(&tg662Injector{}, &tg662Recovery{}, r)
		if ev.ActorUnattributed != 1 || ev.ActorActions != 0 || ev.ActorCovered {
			t.Fatalf("a VMID-stamped record is dropped by the core and must be COUNTED as unattributed, got %+v", ev)
		}
	})
	t.Run("no actor readers wired is unknown", func(t *testing.T) {
		ev := read(&tg662Injector{restores: 1}, &tg662Recovery{n: 1})
		if ev.ActorsKnown || !ev.InjectorKnown || ev.InjectorRestores != 1 || !ev.RecoveredBeforeExecute {
			t.Fatalf("got %+v", ev)
		}
	})
}

// The composed classification end to end through the real classifier: the cases the round-2 review named.
func TestCausalityWireClassifiesTheReviewCases(t *testing.T) {
	cfg := attribution.Config{SelfActors: map[string]string{"pve": "tg-actuate@pve!lane"}, Sanctioned: map[string][]string{"pve": {"root@pam"}}}
	classify := func(readers ...actorevidence.Reader) string {
		ev, _ := causalityEvidenceReader(&tg662Injector{}, &tg662Recovery{}, readers, cfg)(context.Background(), tg662Query())
		acts := runner.NewActivities(runner.Deps{CausalityEvidence: func(context.Context, runner.CausalityQuery) (runner.CausalityEvidence, error) { return ev, nil }})
		out, _ := acts.MarkTerminusFactsActivity(context.Background(), runner.MarkTerminusFactsInput{ExternalRef: "r", Mutated: true, ConfirmedClear: true,
			CollateralKnown: true, VerdictMatch: true, Host: "guest01", TargetHost: "pvenode01", AlertRule: "Devices-up/down", OpClass: "start-guest"})
		return out.Causality
	}
	covering := func(byHost map[string][]attribution.Evidence) *tg662ActorReader {
		return &tg662ActorReader{domain: "pve", byHost: byHost, covers: []string{"guest01", "pvenode01"}}
	}
	if got := classify(covering(map[string][]attribution.Evidence{"guest01": {tg662Record("tg-actuate@pve!lane", "vzstart")}})); got != runner.CausalityTGCaused {
		t.Fatalf("TG alone on a covered subject is tg-caused, got %q", got)
	}
	if got := classify(covering(map[string][]attribution.Evidence{"guest01": {tg662Record("tg-actuate@pve!lane", "vzstart"), tg662Record("root@pam", "vzstart")}})); got != runner.CausalityExternal {
		t.Fatalf("an operator's start seconds before TG's is EXTERNAL, got %q", got)
	}
	if got := classify(covering(map[string][]attribution.Evidence{"pvenode01": {tg662Record("root@pam", "qmstart")}})); got != runner.CausalityExternal {
		t.Fatalf("an operator acting on the TARGET is EXTERNAL, got %q", got)
	}
	if got := classify(&tg662ActorReader{domain: "pve"}); got != runner.CausalityIndeterminate {
		t.Fatalf("a blind reader is INDETERMINATE, never tg-caused, got %q", got)
	}
	stamped := covering(map[string][]attribution.Evidence{"guest01": {tg662Record("root@pam", "vzstart")}})
	stamped.stamp = "101"
	if got := classify(stamped); got != runner.CausalityIndeterminate {
		t.Fatalf("a record the core cannot bind to the subject is INDETERMINATE, got %q", got)
	}
	blind := &tg662ActorReader{domain: "journal", errFor: map[string]error{"guest01": errors.New("openobserve 503")}}
	if got := classify(blind, covering(map[string][]attribution.Evidence{"guest01": {tg662Record("root@pam", "vzstart")}})); got != runner.CausalityExternal {
		t.Fatalf("an admin seen by one reader while another failed is EXTERNAL, not indeterminate, got %q", got)
	}
	targetOnly := &tg662ActorReader{domain: "pve", covers: []string{"pvenode01"}}
	if got := classify(targetOnly); got != runner.CausalityIndeterminate {
		t.Fatalf("a marker on the target alone leaves the subject blind — INDETERMINATE, got %q", got)
	}
	nodeErr := covering(map[string][]attribution.Evidence{"guest01": {tg662Record("tg-actuate@pve!lane", "vzstart")}})
	nodeErr.errFor = map[string]error{"pvenode01": errors.New("guest not found")}
	if got := classify(nodeErr); got != runner.CausalityTGCaused {
		t.Fatalf("a target read error (the PVE node is not a guest) must not blind a covered, self-only subject, got %q", got)
	}
}

func TestTheFoldNamesTheActorBehindEachOtherActorReading(t *testing.T) {
	cfg := attribution.Config{SelfActors: map[string]string{"pve": "tg-actuate@pve!lane"}, Sanctioned: map[string][]string{"pve": {"root@pam"}},
		CarveOuts: []attribution.CarveOut{{ID: "pool", Domain: "pve", Actors: []string{"root@pam"}, Hosts: []string{"guest01"},
			ValidFrom: time.Now().Add(-time.Hour), ValidUntil: time.Now().Add(time.Hour)}}}
	unexplained := tg662Record("tg-actuate@pve!lane", "vzstart")
	unexplained.SelfUnexplained = true
	paired := tg662Record("tg-actuate@pve!lane", "vzstart")
	paired.SelfCorrelated = true
	reader := func(records ...attribution.Evidence) *tg662ActorReader {
		return &tg662ActorReader{domain: "pve", byHost: map[string][]attribution.Evidence{"guest01": records}, covers: []string{"guest01", "pvenode01"}}
	}
	ev, err := causalityEvidenceReader(&tg662Injector{}, &tg662Recovery{}, []actorevidence.Reader{reader(
		tg662Record("root@pam", "vzstop"), tg662Record("root@pam", "vzstart"), unexplained, paired)}, cfg)(context.Background(), tg662Query())
	if err != nil {
		t.Fatal(err)
	}
	if ev.ActorTaxonomy != attribution.AttributedSuspicious.String() || ev.ActorCandidates != 3 {
		t.Fatalf("the reading itself must not change: suspicious over 3 candidates, got %+v", ev)
	}
	notes := strings.Join(ev.Notes, "\n")
	for _, want := range []string{"authorized-test on guest01: pve root@pam x2", "attributed-suspicious on guest01: pve tg-actuate@pve!lane x1", "TG's own key with no matching TG execution"} {
		if !strings.Contains(notes, want) {
			t.Fatalf("the notes must name %q, got %q", want, notes)
		}
	}
	if strings.Count(notes, "tg-actuate@pve!lane") != 1 {
		t.Fatalf("TG's own paired record is not another actor and must not be named, got %q", notes)
	}
	clean, err := causalityEvidenceReader(&tg662Injector{}, &tg662Recovery{}, []actorevidence.Reader{reader(paired)}, cfg)(context.Background(), tg662Query())
	if err != nil {
		t.Fatal(err)
	}
	if len(clean.Notes) != 0 {
		t.Fatalf("a window with only TG's own paired record adds no note, got %v", clean.Notes)
	}
}

func TestOtherActorNotesAreBounded(t *testing.T) {
	var records []attribution.Evidence
	for i := range 10 {
		r := tg662Record(fmt.Sprintf("stranger%02d@pve-with-a-long-realm-name", i), "vzstop")
		r.Target = "guest01"
		records = append(records, r)
	}
	notes := otherActorNotes("guest01", "start-guest", records, attribution.Config{Window: time.Hour})
	if len(notes) != maxOtherActorNotes+1 || !strings.Contains(notes[maxOtherActorNotes], "4 more group(s)") {
		t.Fatalf("notes must stop at %d groups and summarise the rest, got %d: %v", maxOtherActorNotes, len(notes), notes)
	}
	if strings.Contains(notes[0], "long-realm-name") || !strings.Contains(notes[0], "...") {
		t.Fatalf("an actor identity is cut to a prefix, got %q", notes[0])
	}
}

func TestOtherActorNotesNameEachTaxonomyAndKeepTheTextValid(t *testing.T) {
	cfg := attribution.Config{Window: time.Hour, Sanctioned: map[string][]string{"pve": {"root@pam"}},
		SelfReaders: map[string][]string{"journal": {"root!SHA256:reader"}}}
	at := func(e attribution.Evidence) attribution.Evidence { e.Target = "guest01"; return e }
	reader := at(attribution.Evidence{Domain: "journal", Actor: "root!SHA256:reader", ActionKind: "ssh-login", ObservedAt: time.Now().Add(-time.Minute), Covered: true})
	odd := at(tg662Record("ïñtérnâtiönàl-ädmïn;\nwith-a-long-name@pve", "vzstop"))
	straddle := at(tg662Record(strings.Repeat("a", 27)+"é-then-more@pve", "vzstop"))
	notes := otherActorNotes("guest01", "start-guest", []attribution.Evidence{at(tg662Record("root@pam", "vzstop")), reader, odd, straddle}, cfg)
	joined := strings.Join(notes, "\n")
	if !strings.Contains(joined, "attributed-authorized on guest01: pve root@pam x1") {
		t.Fatalf("a sanctioned admin's record must be named as attributed-authorized, got %q", joined)
	}
	if strings.Contains(joined, "SHA256:reader") {
		t.Fatalf("TG's own read-only identity is not another actor and must not be named, got %q", joined)
	}
	for _, n := range notes {
		if !utf8.ValidString(n) || strings.ContainsAny(n, ";\n") {
			t.Fatalf("a note must stay valid UTF-8 with no separator or control character, got %q", n)
		}
	}
}
