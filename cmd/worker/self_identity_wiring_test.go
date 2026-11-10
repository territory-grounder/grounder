package main

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/territory-grounder/grounder/core/attribution"
	"github.com/territory-grounder/grounder/core/credential"
	"github.com/territory-grounder/grounder/modules/actorevidence/journal"
	"github.com/territory-grounder/grounder/modules/observability/syslogng"
)

// THE CROSS-PLANE CHAIN (TG-684, owner ruling 2026-11-07), end to end without a database: the ACTUATION plane derives
// TG's identity from the key it holds and publishes it; the TRIAGE plane — which holds no key — reads the
// publication; its journal reader checks each use of that identity against TG's execution record; attribution
// decides. The required regression set, each named in its failure message.

type memSelfIdentity struct {
	mu   sync.Mutex
	rows []string
	err  error
}

func (m *memSelfIdentity) Publish(_ context.Context, domain, actor, plane string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if n := len(m.rows); n > 0 && m.rows[n-1] == actor {
		return false, nil
	}
	m.rows = append(m.rows, actor)
	return true, nil
}

func (m *memSelfIdentity) Latest(_ context.Context, domain string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return "", false, m.err
	}
	if len(m.rows) == 0 {
		return "", false, nil
	}
	return m.rows[len(m.rows)-1], true, nil
}

type fixedJournal struct{ stdout string }

func (f fixedJournal) Run(context.Context, syslogng.Server, []string) (syslogng.RunResult, error) {
	return syslogng.RunResult{Stdout: []byte(f.stdout)}, nil
}

type anyBundle struct{ b credential.Bundle }

func (r anyBundle) Resolve(context.Context, credential.Target) (credential.Bundle, error) {
	return r.b, nil
}

func loginLine(fp string, at time.Time) string {
	return `{"SYSLOG_IDENTIFIER":"sshd","_COMM":"sshd","MESSAGE":"Accepted publickey for root from 192.0.2.9 port 4242 ssh2: ED25519 ` + fp +
		`","__REALTIME_TIMESTAMP":"` + strconv.FormatInt(at.UnixMicro(), 10) + `","__CURSOR":"c-` + fp + `"}` + "\n"
}

func TestTheCrossPlaneSelfIdentityChainHoldsEveryRuledCase(t *testing.T) {
	ctx := context.Background()
	actPath, actPub := writeKey(t)
	readPath, readPub := writeKey(t)
	_, adminPub := writeKey(t)
	_, strangerPub := writeKey(t)
	act, rd, admin, stranger := ssh.FingerprintSHA256(actPub), ssh.FingerprintSHA256(readPub), ssh.FingerprintSHA256(adminPub), ssh.FingerprintSHA256(strangerPub)

	// ACTUATION plane: derives from the private key and publishes.
	store := &memSelfIdentity{}
	publishSelfIdentity(ctx, store, "journal", resolveSelfSSHActor(envGetter(map[string]string{"TG_ACTUATION_SSH_KEY": "file:" + actPath})), "actuation")
	if len(store.rows) != 1 || store.rows[0] != "root!"+act {
		t.Fatalf("the actuation plane must publish the identity derived from its key, got %v", store.rows)
	}
	publishSelfIdentity(ctx, store, "journal", "root!"+act, "actuation")
	if len(store.rows) != 1 {
		t.Fatal("a reboot with the same key must not add a row")
	}

	// TRIAGE plane: no key in its environment; it reads the publication.
	triageGet := envGetter(map[string]string{"TG_HOSTDIAG_DEPLOYMENTS": "nl|*|root|file:" + readPath})
	if resolveSelfSSHActor(triageGet) != "" {
		t.Fatal("control: the triage plane holds no actuation key")
	}
	identity := selfIdentityHolder(ctx, "journal", resolveSelfSSHActor(triageGet), store, time.Hour)
	if identity() != "root!"+act {
		t.Fatalf("the triage plane must read the published identity, got %q", identity())
	}

	now := time.Now().Truncate(time.Second)
	login := now.Add(-10 * time.Minute)
	pool, other := "pool01", "web01"
	execs := func(_ context.Context, host string, from, to time.Time) ([]time.Time, error) {
		if strings.EqualFold(host, pool) || strings.EqualFold(host, other) {
			return []time.Time{login.Add(9 * time.Second)}, nil
		}
		return nil, nil
	}
	_, base, err := attribution.ParseConfig(attribution.DefaultConfigDocument())
	if err != nil {
		t.Fatal(err)
	}
	base.Window = 30 * time.Minute
	base.SelfReaders["journal"] = resolveSelfSSHReaders(triageGet)
	base.Sanctioned["journal"] = []string{"root!" + admin}
	base.CarveOuts = append(base.CarveOuts, attribution.CarveOut{ID: "pool-ssh", Domain: "journal", Actors: []string{"root!" + admin},
		Hosts: []string{pool}, ValidFrom: now.Add(-time.Hour), ValidUntil: now.Add(time.Hour)})

	read := func(host, lines string, ex journal.ExecutionTimes) ([]attribution.Evidence, error) {
		bundle, err := credential.NewBundle(credential.BundleSpec{User: "root", Port: 22, Scheme: credential.SchemeSSH, SSHKeyRef: "env:UNUSED"})
		if err != nil {
			t.Fatal(err)
		}
		m := journal.New([]journal.Access{{Site: "nl", HostGlob: "*"}}, fixedJournal{lines}, anyBundle{bundle},
			journal.WithSSHSessions(true), journal.WithSelfCorrelation(identity, ex))
		return m.Read(ctx, host, now.Add(-30*time.Minute), now)
	}
	decide := func(name, host, lines string, ex journal.ExecutionTimes) attribution.Finding {
		t.Helper()
		ev, err := read(host, lines, ex)
		if err != nil {
			t.Fatalf("%s: read: %v", name, err)
		}
		return attribution.Attribute(host, "start-service", ev, nil, base)
	}

	cases := []struct {
		name, host, lines string
		ex                journal.ExecutionTimes
		want              attribution.Taxonomy
	}{
		{"TG key + matching execution ⇒ no candidate", other, loginLine(act, login), execs, attribution.Unattributable},
		{"TG key + no execution ⇒ suspicious", "web99", loginLine(act, login), execs, attribution.AttributedSuspicious},
		{"TG key + execution outside tolerance ⇒ suspicious", other, loginLine(act, login.Add(-5*time.Minute)), execs, attribution.AttributedSuspicious},
		{"shared admin key on a non-carve-out host ⇒ authorized (stand-down)", other, loginLine(admin, login), execs, attribution.AttributedAuthorized},
		{"shared admin key on a carve-out pool host ⇒ authorized-test (heal)", pool, loginLine(admin, login), execs, attribution.AuthorizedTest},
		{"TG read-only key ⇒ excluded (no candidate)", other, loginLine(rd, login), execs, attribution.Unattributable},
		{"TG read-only key never masks an unknown actor", other, loginLine(rd, login) + loginLine(stranger, login), execs, attribution.AttributedSuspicious},
		{"unknown key ⇒ suspicious", other, loginLine(stranger, login), execs, attribution.AttributedSuspicious},
		{"unknown key + correlated TG record ⇒ suspicious still dominates", other, loginLine(act, login) + loginLine(stranger, login), execs, attribution.AttributedSuspicious},
	}
	for _, c := range cases {
		if f := decide(c.name, c.host, c.lines, c.ex); f.Taxonomy != c.want {
			t.Errorf("%s: got %s (candidates %v, warnings %v)", c.name, f.Taxonomy, f.Candidates, f.Warnings)
		}
	}
	// "TG key + execution on ANOTHER host ⇒ suspicious": the record explains pool01, the login is on web77.
	onlyPool := func(_ context.Context, host string, _, _ time.Time) ([]time.Time, error) {
		if strings.EqualFold(host, pool) {
			return []time.Time{login.Add(time.Second)}, nil
		}
		return nil, nil
	}
	if f := decide("another host", "web77", loginLine(act, login), onlyPool); f.Taxonomy != attribution.AttributedSuspicious {
		t.Errorf("TG key + execution on another host ⇒ suspicious: got %s", f.Taxonomy)
	}
	// Missing / unreadable execution record ⇒ the read FAILS (indeterminate under TG-668), never self.
	broken := func(context.Context, string, time.Time, time.Time) ([]time.Time, error) {
		return nil, errors.New("db: connection refused")
	}
	if ev, err := read(other, loginLine(act, login), broken); err == nil || len(ev) != 0 {
		t.Errorf("an unreadable execution record must fail the read, got %+v err=%v", ev, err)
	}
	if ev, err := read(other, loginLine(act, login), nil); err == nil || len(ev) != 0 {
		t.Errorf("a missing execution record must fail the read, got %+v err=%v", ev, err)
	}
}

// The holder: a key-holding process answers its own derivation; a reader keeps the last good identity across a read
// error; nothing is invented before the first publication.
func TestTheSelfIdentityHolderNeverInventsAnIdentity(t *testing.T) {
	ctx := context.Background()
	if got := selfIdentityHolder(ctx, "journal", "root!SHA256:local", nil, time.Hour)(); got != "root!SHA256:local" {
		t.Fatalf("a process holding the key answers its own derivation, got %q", got)
	}
	empty := &memSelfIdentity{}
	if got := selfIdentityHolder(ctx, "journal", "", empty, time.Hour)(); got != "" {
		t.Fatalf("before any publication the identity is unknown, got %q", got)
	}
	if got := selfIdentityHolder(ctx, "journal", "", nil, time.Hour)(); got != "" {
		t.Fatalf("no database ⇒ unknown, got %q", got)
	}
	st := &memSelfIdentity{rows: []string{"root!SHA256:published"}}
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	h := selfIdentityHolder(cctx, "journal", "", st, 10*time.Millisecond)
	if h() != "root!SHA256:published" {
		t.Fatalf("the published identity must be read at once, got %q", h())
	}
	st.mu.Lock()
	st.err = errors.New("db: gone")
	st.mu.Unlock()
	time.Sleep(40 * time.Millisecond)
	if h() != "root!SHA256:published" {
		t.Fatalf("a read error must keep the last good identity, got %q", h())
	}
	st.mu.Lock()
	st.err = nil
	st.rows = append(st.rows, "root!SHA256:rotated")
	st.mu.Unlock()
	deadline := time.Now().Add(2 * time.Second)
	for h() != "root!SHA256:rotated" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if h() != "root!SHA256:rotated" {
		t.Fatalf("a rotation must reach the triage plane on the next refresh, got %q", h())
	}
}
