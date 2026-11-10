package journal

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/attribution"
	"github.com/territory-grounder/grounder/core/credential"
)

// THE OWNER'S RULE (2026-11-07, TG-684): a journal record whose actor is TG's actuation identity counts as TG's own
// ONLY when a TG-recorded execution on the SAME host lies inside the measured tolerance. Otherwise it is
// SelfUnexplained — key misuse or a ledger gap — and Attribute reads it suspicious.

const tgSelf = "root!SHA256:TGactuationKey0123456789abcdefGHIJKLMNOPQRSTU"

func sshdLine(actor string, at time.Time) string {
	fp := strings.TrimPrefix(actor, "root!")
	return `{"SYSLOG_IDENTIFIER":"sshd","_COMM":"sshd","MESSAGE":"Accepted publickey for root from 192.0.2.9 port 4242 ssh2: ED25519 ` + fp +
		`","__REALTIME_TIMESTAMP":"` + strconv.FormatInt(at.UnixMicro(), 10) + `","__CURSOR":"c-` + fp + `"}` + "\n"
}

// execOn returns an ExecutionTimes that knows executions on ONE host only — a lookup for any other host is empty.
func execOn(host string, at ...time.Time) ExecutionTimes {
	return func(_ context.Context, h string, from, to time.Time) ([]time.Time, error) {
		if !strings.EqualFold(h, host) {
			return nil, nil
		}
		var out []time.Time
		for _, t := range at {
			if !t.Before(from) && !t.After(to) {
				out = append(out, t)
			}
		}
		return out, nil
	}
}

func readWith(t *testing.T, lines string, identity func() string, ex ExecutionTimes) ([]attribution.Evidence, error) {
	t.Helper()
	fr := &fakeRunner{stdout: lines}
	m := New([]Access{{Site: "NL", HostGlob: "*"}}, fr, fakeResolver{bundle: testBundle(t)}, WithSSHSessions(true), WithSelfCorrelation(identity, ex))
	now := time.Now()
	return m.Read(context.Background(), "web01", now.Add(-30*time.Minute), now)
}

func self() string { return tgSelf }

func attributeService(ev []attribution.Evidence) attribution.Finding {
	_, cfg, _ := attribution.ParseConfig(attribution.DefaultConfigDocument())
	cfg.Window = 30 * time.Minute
	return attribution.Attribute("web01", "start-service", ev, nil, cfg)
}

// KILLING MUTATION for the whole rule: make correlateSelf mark every TG row SelfCorrelated (drop the check). RED here.
func TestAnUncorrelatedSelfKeyLoginIsSuspicious(t *testing.T) {
	login := time.Now().Add(-10 * time.Minute).Truncate(time.Second)
	ev, err := readWith(t, sshdLine(tgSelf, login), self, execOn("web01"))
	if err != nil {
		t.Fatal(err)
	}
	if len(ev) != 1 || !ev[0].SelfUnexplained || ev[0].SelfCorrelated {
		t.Fatalf("TG's key with no TG execution must be marked unexplained, got %+v", ev)
	}
	f := attributeService(ev)
	if f.Taxonomy != attribution.AttributedSuspicious || !strings.Contains(strings.Join(f.Warnings, " "), "self-key-unexplained") {
		t.Fatalf("an unexplained use of TG's key must read suspicious with the reason, got %s %v", f.Taxonomy, f.Warnings)
	}
}

func TestASelfKeyLoginExplainedByAnExecutionOnAnotherHostIsSuspicious(t *testing.T) {
	login := time.Now().Add(-10 * time.Minute).Truncate(time.Second)
	ev, err := readWith(t, sshdLine(tgSelf, login), self, execOn("db02", login.Add(2*time.Second)))
	if err != nil {
		t.Fatal(err)
	}
	if !ev[0].SelfUnexplained {
		t.Fatalf("an execution on ANOTHER host explains nothing here, got %+v", ev[0])
	}
}

// The tolerance edges, both sides: inside ⇒ correlated, one second past ⇒ unexplained.
func TestTheCorrelationToleranceIsEnforcedOnBothSides(t *testing.T) {
	login := time.Now().Add(-10 * time.Minute).Truncate(time.Second)
	for name, tc := range map[string]struct {
		exec time.Time
		want bool
	}{
		"exec +120s (late edge)":   {login.Add(SelfExecutionAfter), true},
		"exec -30s (early edge)":   {login.Add(-SelfExecutionBefore), true},
		"exec +121s (outside)":     {login.Add(SelfExecutionAfter + time.Second), false},
		"exec -31s (outside)":      {login.Add(-SelfExecutionBefore - time.Second), false},
		"exec +11s (measured max)": {login.Add(11 * time.Second), true},
	} {
		ev, err := readWith(t, sshdLine(tgSelf, login), self, execOn("web01", tc.exec))
		if err != nil {
			t.Fatal(err)
		}
		if ev[0].SelfCorrelated != tc.want || ev[0].SelfUnexplained == tc.want {
			t.Errorf("%s: correlated=%v unexplained=%v, want correlated=%v", name, ev[0].SelfCorrelated, ev[0].SelfUnexplained, tc.want)
		}
	}
}

// A correlated TG login takes the self path unchanged: an SSH login does not accomplish a service start
// (REQ-2302's fault-class narrowing), so it mints NO candidate — neither suspicious nor self-noop — and the heal
// proceeds.
func TestACorrelatedSelfKeyLoginMintsNoCandidateForAServiceHeal(t *testing.T) {
	login := time.Now().Add(-10 * time.Minute).Truncate(time.Second)
	ev, err := readWith(t, sshdLine(tgSelf, login), self, execOn("web01", login.Add(8*time.Second)))
	if err != nil {
		t.Fatal(err)
	}
	if !ev[0].SelfCorrelated {
		t.Fatalf("an execution 8 s after the login explains it, got %+v", ev[0])
	}
	f := attributeService(ev)
	if f.Taxonomy == attribution.AttributedSuspicious || f.Taxonomy == attribution.AttributedSelf || len(f.Candidates) != 0 {
		t.Fatalf("a correlated TG login must mint no candidate for a service heal, got %s %v", f.Taxonomy, f.Candidates)
	}
}

// FAIL CLOSED, never self: an unreadable execution record, no record wired, or no published identity — for a read
// that holds a login — is an ERROR (the domain reads indeterminate under TG-668), never a guess.
func TestAnUnverifiableSelfKeyLoginFailsTheReadNeverReadsSelf(t *testing.T) {
	login := time.Now().Add(-10 * time.Minute).Truncate(time.Second)
	broken := func(context.Context, string, time.Time, time.Time) ([]time.Time, error) {
		return nil, errors.New("db: connection refused")
	}
	if ev, err := readWith(t, sshdLine(tgSelf, login), self, broken); err == nil || len(ev) != 0 {
		t.Fatalf("an unreadable execution record must fail the read, got %+v err=%v", ev, err)
	}
	if ev, err := readWith(t, sshdLine(tgSelf, login), self, nil); err == nil || len(ev) != 0 {
		t.Fatalf("no execution record wired must fail the read, got %+v err=%v", ev, err)
	}
	unpublished := func() string { return "" }
	if ev, err := readWith(t, sshdLine("root!SHA256:SomeoneKey0123456789abcdefGHIJKLMNOPQRSTUVW", login), unpublished, execOn("web01")); err == nil || len(ev) != 0 {
		t.Fatalf("with TG's identity unpublished a login cannot be classified — the read must fail, got %+v err=%v", ev, err)
	}
	// With no login at all there is nothing to verify: the clean miss stands even while unpublished.
	if ev, err := readWith(t, "", unpublished, nil); err != nil || len(ev) != 1 || !attribution.IsCoverageMarker(ev[0]) {
		t.Fatalf("an empty read is the clean miss regardless of the identity, got %+v err=%v", ev, err)
	}
}

// A stranger beside a CORRELATED TG login still dominates (REQ-2304): TG's own correlated record never masks it.
func TestAStrangerBesideACorrelatedSelfLoginIsStillSuspicious(t *testing.T) {
	login := time.Now().Add(-10 * time.Minute).Truncate(time.Second)
	lines := sshdLine(tgSelf, login) + sshdLine("root!SHA256:StrangerKey0123456789abcdefGHIJKLMNOPQRSTUV", login.Add(time.Minute))
	ev, err := readWith(t, lines, self, execOn("web01", login.Add(time.Second)))
	if err != nil {
		t.Fatal(err)
	}
	if f := attributeService(ev); f.Taxonomy != attribution.AttributedSuspicious {
		t.Fatalf("a stranger beside TG's correlated login must read suspicious, got %s %v", f.Taxonomy, f.Candidates)
	}
}

// THE FINGERPRINT ALONE NEVER WINS, even where it is configured as a self-actor. On a single-process deployment the
// worker holds the key, so cfg.SelfActors["journal"] names TG's identity directly; an UNEXPLAINED login by it must
// still read suspicious, because SelfUnexplained is checked before the SelfActors match.
// KILLING MUTATION: drop the `if e.SelfUnexplained` branch in attribution.AttributeObserving — RED here only (on a
// split deployment SelfActors is empty and the row falls through to unsanctioned anyway).
func TestAnUnexplainedSelfKeyLoginIsSuspiciousEvenWhereTheKeyIsAConfiguredSelfActor(t *testing.T) {
	login := time.Now().Add(-10 * time.Minute).Truncate(time.Second)
	ev, err := readWith(t, sshdLine(tgSelf, login), self, execOn("web01"))
	if err != nil {
		t.Fatal(err)
	}
	_, cfg, _ := attribution.ParseConfig(attribution.DefaultConfigDocument())
	cfg.Window = 30 * time.Minute
	cfg.SelfActors["journal"] = tgSelf
	if f := attribution.Attribute("web01", "start-service", ev, nil, cfg); f.Taxonomy != attribution.AttributedSuspicious {
		t.Fatalf("an unexplained login by a configured self-actor must still read suspicious, got %s %v", f.Taxonomy, f.Warnings)
	}
	// Control: the same configured self-actor beside a real execution is TG's own (no candidate for a service heal).
	ev, err = readWith(t, sshdLine(tgSelf, login), self, execOn("web01", login.Add(5*time.Second)))
	if err != nil {
		t.Fatal(err)
	}
	if f := attribution.Attribute("web01", "start-service", ev, nil, cfg); f.Taxonomy == attribution.AttributedSuspicious || len(f.Candidates) != 0 {
		t.Fatalf("control: a correlated configured self-actor must mint no candidate, got %s %v", f.Taxonomy, f.Candidates)
	}
}

// ABSOLUTE bounds on the tolerance. TestTheCorrelationToleranceIsEnforcedOnBothSides measures its edges FROM the
// constants, so a widened window moves the test with it; this one pins the ruled scale in plain durations: an
// execution three minutes after, or one minute before, a login does not explain it (measured: −0.1 s … +10.9 s).
// KILLING MUTATION: raise SelfExecutionAfter or SelfExecutionBefore past these values — RED here.
func TestTheToleranceStaysSecondsToMinutesNotMore(t *testing.T) {
	login := time.Now().Add(-20 * time.Minute).Truncate(time.Second)
	for name, exec := range map[string]time.Time{
		"execution 3 min after the login":  login.Add(3 * time.Minute),
		"execution 1 min before the login": login.Add(-1 * time.Minute),
	} {
		ev, err := readWith(t, sshdLine(tgSelf, login), self, execOn("web01", exec))
		if err != nil {
			t.Fatal(err)
		}
		if !ev[0].SelfUnexplained || ev[0].SelfCorrelated {
			t.Errorf("%s must NOT explain it, got correlated=%v unexplained=%v", name, ev[0].SelfCorrelated, ev[0].SelfUnexplained)
		}
	}
}

// ONE LOGIN PER EXECUTION (fresh-eyes review 2026-11-08). TG's SSH leaf opens one session per execution and the live
// pairing was exactly one-to-one, so a SECOND login with TG's key riding on the same heal is not explained by it.
// KILLING MUTATION: drop the `used[j] = true` claim in correlateSelf — RED here.
func TestOneExecutionExplainsAtMostOneTGLogin(t *testing.T) {
	first := time.Now().Add(-10 * time.Minute).Truncate(time.Second)
	second := first.Add(20 * time.Second)
	lines := sshdLine(tgSelf, first) + sshdLine(tgSelf, second)

	ev, err := readWith(t, lines, self, execOn("web01", first.Add(5*time.Second)))
	if err != nil {
		t.Fatal(err)
	}
	var corr, unexpl int
	for _, e := range ev {
		if e.SelfCorrelated {
			corr++
		}
		if e.SelfUnexplained {
			unexpl++
		}
	}
	if corr != 1 || unexpl != 1 {
		t.Fatalf("one execution must explain exactly one of two TG logins, got correlated=%d unexplained=%d (%+v)", corr, unexpl, ev)
	}
	if f := attributeService(ev); f.Taxonomy != attribution.AttributedSuspicious {
		t.Fatalf("the second, unexplained TG login must make the finding suspicious, got %s", f.Taxonomy)
	}

	// Control: two logins, two executions (one each) — both are TG's own, no candidate.
	ev, err = readWith(t, lines, self, execOn("web01", first.Add(5*time.Second), second.Add(4*time.Second)))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ev {
		if !e.SelfCorrelated {
			t.Fatalf("control: with one execution per login every login is correlated, got %+v", ev)
		}
	}
	if f := attributeService(ev); f.Taxonomy == attribution.AttributedSuspicious || len(f.Candidates) != 0 {
		t.Fatalf("control: two correlated TG logins mint no candidate, got %s %v", f.Taxonomy, f.Candidates)
	}
}

// AN UNPUBLISHED IDENTITY DOES NOT BLIND A SUDO-ONLY READ (fresh-eyes review 2026-11-08). With SSH-session evidence
// off no row can carry TG's actuation identity, so the read stands; with it on, the same read fails closed.
// KILLING MUTATION: drop the `if !m.sshSessions { return nil }` guard — RED here.
func TestAnUnpublishedIdentityDoesNotBlindASudoOnlyRead(t *testing.T) {
	at := time.Now().Add(-5 * time.Minute)
	sudo := `{"MESSAGE":"kp : TTY=pts/0 ; PWD=/root ; USER=root ; COMMAND=/usr/bin/systemctl restart nginx","_COMM":"sudo","_AUDIT_LOGINUID":"1001","SYSLOG_IDENTIFIER":"sudo","__REALTIME_TIMESTAMP":"` +
		strconv.FormatInt(at.UnixMicro(), 10) + `","__CURSOR":"s=tg684;i=1"}` + "\n"
	unpublished := func() string { return "" }
	read := func(sessions bool) ([]attribution.Evidence, error) {
		fr := &fakeRunner{stdout: sudo}
		m := New([]Access{{Site: "NL", HostGlob: "*"}}, fr, fakeResolver{bundle: testBundle(t)}, WithSSHSessions(sessions), WithSelfCorrelation(unpublished, nil))
		now := time.Now()
		return m.Read(context.Background(), "web01", now.Add(-30*time.Minute), now)
	}
	ev, err := read(false)
	if err != nil || len(ev) != 1 || ev[0].Actor != "kp" {
		t.Fatalf("a sudo-only read must stand while TG's identity is unpublished, got %+v err=%v", ev, err)
	}
	if ev, err := read(true); err == nil || len(ev) != 0 {
		t.Fatalf("control: with SSH-session evidence on, an unpublished identity must fail the read, got %+v err=%v", ev, err)
	}
}

// tg687CaptureResolver records every Target the reader asks the credential engine for.
type tg687CaptureResolver struct {
	got    []credential.Target
	bundle credential.Bundle
}

func (r *tg687CaptureResolver) Resolve(_ context.Context, t credential.Target) (credential.Bundle, error) {
	r.got = append(r.got, t)
	return r.bundle, nil
}

// TG-687: the journal read asks for TG's guarded READ-ONLY identity (PurposeDiagRead). Without the purpose a
// per-host admin binding out-ranked the read-only key on NL hosts, so the read bypassed tg-readonly-guard and TG's
// own read showed up in the journal as the shared admin key — a sanctioned human.
// KILLING MUTATION: drop `Purpose: credential.PurposeDiagRead` from the reader's Resolve call — RED here.
func TestTheJournalReadAsksForTheReadOnlyIdentity(t *testing.T) {
	r := &tg687CaptureResolver{bundle: testBundle(t)}
	m := New([]Access{{Site: "NL", HostGlob: "*"}}, &fakeRunner{stdout: ""}, r)
	now := time.Now()
	if _, err := m.Read(context.Background(), "web01", now.Add(-30*time.Minute), now); err != nil {
		t.Fatal(err)
	}
	if len(r.got) == 0 {
		t.Fatal("control: the reader must resolve a credential for the host")
	}
	for _, tg := range r.got {
		if tg.Purpose != credential.PurposeDiagRead {
			t.Fatalf("the journal read must ask for the diag-read identity, got purpose %q for %+v", tg.Purpose, tg)
		}
	}
}
