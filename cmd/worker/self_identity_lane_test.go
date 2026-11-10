package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/territory-grounder/grounder/core/actuate/opschema"
	"github.com/territory-grounder/grounder/core/attribution"
	"github.com/territory-grounder/grounder/core/credential"
	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/modules/actorevidence/journal"
)

// THE OWNER'S RULE NAMES THE ACTION, NOT ONLY THE HOST AND THE TIME (TG-684, ruling 2026-11-07): a login with TG's key
// counts as TG's own only when TG's recorded ACTION explains it. A start-guest runs through the Proxmox API, an AWX
// launch through AWX, a gitops change through GitLab — none of them logs into the guest. So only an execution whose
// op-class routes over SSH (opschema.EffectSSHArgv) can explain an sshd login; anything else, including a manifest
// that names no op-class or one the registry does not know, leaves the login unexplained (suspicious).

// KILLING MUTATION: make sshLaneExecutionTimes keep every execution (drop the kind check). RED here and in
// TestATGKeyLoginBesideOnlyAProxmoxStartIsSuspicious.
func TestOnlyAnSSHLaneExecutionExplainsTGsOwnLogin(t *testing.T) {
	// Control: the registry routes these classes the way the assertions below assume.
	for oc, want := range map[string]string{
		"start-guest":     opschema.EffectProxmoxLifecycle,
		"restart-service": opschema.EffectSSHArgv,
		"start-container": opschema.EffectSSHArgv,
	} {
		spec, ok := opschema.Lookup(oc)
		if !ok || spec.Kind() != want {
			t.Fatalf("control: %s must be registered with effect kind %q, got ok=%v kind=%q", oc, want, ok, spec.Kind())
		}
	}
	if _, ok := opschema.Lookup("tg684-no-such-class"); ok {
		t.Fatal("control: the unknown class must be unknown to the registry")
	}

	base := time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)
	rows := []db.ExecutionAt{
		{At: base.Add(1 * time.Second), OpClass: "start-guest"},
		{At: base.Add(2 * time.Second), OpClass: "restart-service"},
		{At: base.Add(3 * time.Second), OpClass: ""},
		{At: base.Add(4 * time.Second), OpClass: "tg684-no-such-class"},
		{At: base.Add(5 * time.Second), OpClass: "start-container"},
	}
	list := func(context.Context, string, time.Time, time.Time) ([]db.ExecutionAt, error) { return rows, nil }
	got, err := sshLaneExecutionTimes(list)(context.Background(), "web01", base, base.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	want := []time.Time{base.Add(2 * time.Second), base.Add(5 * time.Second)}
	if len(got) != len(want) || !got[0].Equal(want[0]) || !got[1].Equal(want[1]) {
		t.Fatalf("only the SSH-lane executions (restart-service, start-container) may explain a login; got %v, want %v", got, want)
	}

	broken := func(context.Context, string, time.Time, time.Time) ([]db.ExecutionAt, error) {
		return nil, errors.New("db: connection refused")
	}
	if got, err := sshLaneExecutionTimes(broken)(context.Background(), "web01", base, base.Add(time.Minute)); err == nil || got != nil {
		t.Fatalf("an unreadable execution record must stay an error (fail closed), got %v err=%v", got, err)
	}
}

// tg684LaneDecide reads one host's journal through the production correlation option and attributes a service fault.
func tg684LaneDecide(t *testing.T, host, lines string, identity func() string, ex journal.ExecutionTimes, now time.Time) attribution.Finding {
	t.Helper()
	bundle, err := credential.NewBundle(credential.BundleSpec{User: "root", Port: 22, Scheme: credential.SchemeSSH, SSHKeyRef: "env:UNUSED"})
	if err != nil {
		t.Fatal(err)
	}
	m := journal.New([]journal.Access{{Site: "nl", HostGlob: "*"}}, fixedJournal{lines}, anyBundle{bundle},
		journal.WithSSHSessions(true), journal.WithSelfCorrelation(identity, ex))
	ev, err := m.Read(context.Background(), host, now.Add(-30*time.Minute), now)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	_, cfg, err := attribution.ParseConfig(attribution.DefaultConfigDocument())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Window = 30 * time.Minute
	return attribution.Attribute(host, "start-service", ev, nil, cfg)
}

func TestATGKeyLoginBesideOnlyAProxmoxStartIsSuspicious(t *testing.T) {
	_, actPub := writeKey(t)
	act := "root!" + ssh.FingerprintSHA256(actPub)
	identity := func() string { return act }
	now := time.Now().Truncate(time.Second)
	login := now.Add(-10 * time.Minute)
	beside := func(opClass string) journal.ExecutionTimes {
		return sshLaneExecutionTimes(func(context.Context, string, time.Time, time.Time) ([]db.ExecutionAt, error) {
			return []db.ExecutionAt{{At: login.Add(2 * time.Second), OpClass: opClass}}, nil
		})
	}
	lines := loginLine(ssh.FingerprintSHA256(actPub), login)

	if f := tg684LaneDecide(t, "web01", lines, identity, beside("start-guest"), now); f.Taxonomy != attribution.AttributedSuspicious {
		t.Errorf("TG's key beside only a Proxmox start-guest (which never logs in) must read suspicious, got %s %v", f.Taxonomy, f.Warnings)
	}
	// Positive control: the same login beside an SSH-lane restart is TG's own and mints no candidate.
	if f := tg684LaneDecide(t, "web01", lines, identity, beside("restart-service"), now); f.Taxonomy != attribution.Unattributable {
		t.Errorf("TG's key beside its own SSH-lane restart must mint no candidate, got %s %v", f.Taxonomy, f.Candidates)
	}
}

// A ROTATED-OUT key is no longer TG's: once the actuation plane publishes the new identity, a login with the old key
// is a stranger's (suspicious) even beside a real TG execution, and the new key is recognised.
func TestALoginWithARotatedOutKeyIsNotTGs(t *testing.T) {
	ctx := context.Background()
	_, oldPub := writeKey(t)
	_, newPub := writeKey(t)
	oldFP, newFP := ssh.FingerprintSHA256(oldPub), ssh.FingerprintSHA256(newPub)
	store := &memSelfIdentity{rows: []string{"root!" + oldFP}}
	publishSelfIdentity(ctx, store, "journal", "root!"+newFP, "actuation")
	identity := selfIdentityHolder(ctx, "journal", "", store, time.Hour)
	if identity() != "root!"+newFP {
		t.Fatalf("control: the triage plane must hold the rotated identity, got %q", identity())
	}
	now := time.Now().Truncate(time.Second)
	login := now.Add(-10 * time.Minute)
	sshExec := sshLaneExecutionTimes(func(context.Context, string, time.Time, time.Time) ([]db.ExecutionAt, error) {
		return []db.ExecutionAt{{At: login.Add(3 * time.Second), OpClass: "restart-service"}}, nil
	})
	if f := tg684LaneDecide(t, "web01", loginLine(oldFP, login), identity, sshExec, now); f.Taxonomy != attribution.AttributedSuspicious {
		t.Errorf("a login with the rotated-out key must read suspicious even beside a TG execution, got %s", f.Taxonomy)
	}
	if f := tg684LaneDecide(t, "web01", loginLine(newFP, login), identity, sshExec, now); f.Taxonomy != attribution.Unattributable {
		t.Errorf("the current key beside a TG SSH execution must mint no candidate, got %s %v", f.Taxonomy, f.Candidates)
	}
}
