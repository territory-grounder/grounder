package main

// TG-693 (live 2026-11-08 07:07Z): on the split deployment the triage plane never holds the Proxmox actuation token,
// so TG's own heal task in the PVE task log read as a stranger's token — attributed-suspicious — and a pool guest
// re-faulted inside the 30-minute lookback was HELD (ledger 22561) instead of healed. These tests replay that case
// through the production wiring: the actuation plane publishes the token principal under "pve", the triage plane
// reads it, the PVE reader pairs TG's task with TG's Proxmox execution, and attribution decides.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/attribution"
	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/manifest"
	pveattr "github.com/territory-grounder/grounder/modules/actorevidence/pve"
)

// tg693Store is a self_identity projection keyed by DOMAIN (memSelfIdentity holds one domain only).
type tg693Store struct {
	mu   sync.Mutex
	rows map[string][]string
}

func (s *tg693Store) Publish(_ context.Context, domain, actor, _ string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rows == nil {
		s.rows = map[string][]string{}
	}
	if r := s.rows[domain]; len(r) > 0 && r[len(r)-1] == actor {
		return false, nil
	}
	s.rows[domain] = append(s.rows[domain], actor)
	return true, nil
}

func (s *tg693Store) Latest(_ context.Context, domain string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.rows[domain]; len(r) > 0 {
		return r[len(r)-1], true, nil
	}
	return "", false, nil
}

// tg693PVE answers the two endpoints the reader calls with the live sequence on one pool guest.
type tg693PVE struct{ guest, vmid string }

func tg693T(h, m, s, ms int) time.Time { return time.Date(2026, 11, 8, h, m, s, ms*1e6, time.UTC) }

func (p tg693PVE) Do(req *http.Request) (*http.Response, error) {
	body := `{"data":[]}`
	switch {
	case strings.Contains(req.URL.Path, "/cluster/resources"):
		body = `{"data":[{"node":"pve03","name":"` + p.guest + `","vmid":` + p.vmid + `,"type":"lxc","status":"stopped"}]}`
	case strings.Contains(req.URL.Path, "/tasks"):
		row := func(upid, user, tok, typ string, at time.Time) string {
			return `{"upid":"` + upid + `","user":"` + user + `","tokenid":"` + tok + `","type":"` + typ + `","id":"` + p.vmid +
				`","starttime":` + strconv.FormatInt(at.Unix(), 10) + `,"status":"OK"}`
		}
		body = `{"data":[` + strings.Join([]string{
			row("UPID:a", "root@pam", "", "vzstop", tg693T(6, 46, 38, 0)),            // the injector's first fault
			row("UPID:b", "tg-actuate@pve", "lane", "vzstart", tg693T(6, 46, 54, 0)), // TG's heal
			row("UPID:c", "root@pam", "", "vzstop", tg693T(7, 7, 14, 0)),             // the injector's re-fault
		}, ",") + `]}`
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
}

// tg693Decide runs one start-guest attribution the way the triage plane does: the identity comes ONLY from the
// publication (no token on this plane), the executions from the given lister.
func tg693Decide(t *testing.T, store *tg693Store, list executionsSinceLister) attribution.Finding {
	t.Helper()
	t.Setenv("TG693_RO", "root@pam!tg-ro=secret")
	const guest = "dc1tgpool03"
	identity := selfIdentityHolder(context.Background(), "pve", "", store, time.Hour)
	r := pveattr.New("https://pve03:8006", config.SecretRef("env:TG693_RO"), pveattr.WithHTTPClient(tg693PVE{guest, "101101233"}),
		pveattr.WithSelfCorrelation(identity, proxmoxLaneExecutionTimes(list)))
	now := tg693T(7, 8, 0, 0)
	ev, err := r.Read(context.Background(), guest, now.Add(-30*time.Minute), now)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	cfg := attribution.Config{
		Window:     30 * time.Minute,
		Now:        func() time.Time { return now },
		SelfActors: map[string]string{}, // the triage plane: no token, so no local pve self-actor
		Sanctioned: map[string][]string{"pve": {"root@pam"}},
		CarveOuts: []attribution.CarveOut{{ID: "shadowbench-pool", Domain: "pve", Actors: []string{"root@pam"},
			Hosts: []string{guest}, ValidFrom: now.Add(-24 * time.Hour), ValidUntil: now.Add(24 * time.Hour)}},
	}
	return attribution.Attribute(guest, "start-guest", ev, nil, cfg)
}

// tg693Execs returns the execution page a guest's ExecutionsSince read would return.
func tg693Execs(rows ...db.ExecutionSince) executionsSinceLister {
	return func(context.Context, time.Time, string, int) ([]db.ExecutionSince, error) { return rows, nil }
}

var tg693Exec589 = db.ExecutionSince{ID: 589, At: tg693T(6, 46, 59, 717),
	Action: manifest.Action{OpClass: "start-guest", Op: "start", Target: "dc1tgpool03", Params: map[string]string{"guest": "dc1tgpool03"}}}

// The actuation plane publishes the pve identity derived from the token it holds — the principal only, never the
// secret. KILLING MUTATION: drop the pve startSelfIdentityPublisher call in publishSelfIdentitiesTo — RED here.
func TestTheActuationPlanePublishesTheTokenPrincipalUnderPVE(t *testing.T) {
	t.Setenv("TG693_ACT_TOKEN", "tg-actuate@pve!lane=tg693-secret-half")
	store := &tg693Store{}
	publishSelfIdentitiesTo(context.Background(), store, envGetter(map[string]string{"TG_PROXMOX_TOKEN_REF": "env:TG693_ACT_TOKEN"}))
	a, ok, _ := store.Latest(context.Background(), "pve")
	if !ok || a != "tg-actuate@pve!lane" {
		t.Fatalf("the actuation plane must publish the token principal under pve, got %q ok=%v", a, ok)
	}
	for d, rows := range store.rows {
		for _, r := range rows {
			if strings.Contains(r, "tg693-secret-half") {
				t.Fatalf("the token SECRET was published under %q", d)
			}
		}
	}
	// The triage plane holds no token: it publishes nothing under pve.
	empty := &tg693Store{}
	publishSelfIdentitiesTo(context.Background(), empty, envGetter(map[string]string{}))
	if _, ok, _ := empty.Latest(context.Background(), "pve"); ok {
		t.Fatal("a process without the actuation token must publish no pve identity")
	}
}

// THE LIVE CASE. KILLING MUTATION: make selfIdentityHolder read "journal" for every domain — RED (no pve identity is
// found, so TG's own task cannot be told from a stranger's and the read fails closed).
func TestAPoolGuestReFaultedAfterTGsOwnHealIsHealedNotHeld(t *testing.T) {
	store := &tg693Store{}
	// The actuation plane publishes BOTH identities; a journal identity must never stand in for the pve one.
	if !startSelfIdentityPublisher(context.Background(), store, "journal", "root!SHA256:tg693journal", "actuation", time.Hour) ||
		!startSelfIdentityPublisher(context.Background(), store, "pve", "tg-actuate@pve!lane", "actuation", time.Hour) {
		t.Fatal("publication must settle at once against a working store")
	}
	if a, ok, _ := store.Latest(context.Background(), "pve"); !ok || a != "tg-actuate@pve!lane" {
		t.Fatalf("the pve identity must be published under its own domain, got %q ok=%v", a, ok)
	}
	f := tg693Decide(t, store, tg693Execs(tg693Exec589))
	if f.Taxonomy != attribution.AuthorizedTest {
		t.Fatalf("the injector's re-fault after TG's own correlated heal must heal (authorized-test), got %s %v %v", f.Taxonomy, f.Candidates, f.Warnings)
	}

	// The pre-fix state, for contrast: nothing published under "pve" ⇒ TG's token task cannot be told from a
	// stranger's ⇒ the read fails closed (indeterminate), never suspicious-by-default and never healed blind.
	journalOnly := &tg693Store{}
	_, _ = journalOnly.Publish(context.Background(), "journal", "root!SHA256:tg693journal", "actuation")
	t.Setenv("TG693_RO", "root@pam!tg-ro=secret")
	r := pveattr.New("https://pve03:8006", config.SecretRef("env:TG693_RO"), pveattr.WithHTTPClient(tg693PVE{"dc1tgpool03", "101101233"}),
		pveattr.WithSelfCorrelation(selfIdentityHolder(context.Background(), "pve", "", journalOnly, time.Hour), proxmoxLaneExecutionTimes(tg693Execs(tg693Exec589))))
	now := tg693T(7, 8, 0, 0)
	if _, err := r.Read(context.Background(), "dc1tgpool03", now.Add(-30*time.Minute), now); err == nil || !strings.Contains(err.Error(), "not published") {
		t.Fatalf("with no pve identity published the read must fail closed, got %v", err)
	}
}

// The task is TG's only beside a Proxmox-lane execution ON THAT GUEST, in the window. KILLING MUTATIONS in
// proxmoxLaneExecutionTimes: drop the effect-kind check, the resource match, or the `to` bound — RED on the case.
func TestOnlyAProxmoxExecutionOnTheSameGuestExplainsTGsTask(t *testing.T) {
	store := &tg693Store{}
	_, _ = store.Publish(context.Background(), "pve", "tg-actuate@pve!lane", "actuation")
	at := tg693Exec589.At
	cases := []struct {
		name string
		rows []db.ExecutionSince
		want attribution.Taxonomy
	}{
		{"no execution at all (a ledger gap, or a stolen token)", nil, attribution.AttributedSuspicious},
		{"only an SSH-lane heal inside the guest", []db.ExecutionSince{{ID: 1, At: at,
			Action: manifest.Action{OpClass: "start-service", Target: "dc1tgpool03", Params: map[string]string{"unit": "nginx"}}}}, attribution.AttributedSuspicious},
		{"a start on another guest", []db.ExecutionSince{{ID: 2, At: at,
			Action: manifest.Action{OpClass: "start-guest", Target: "dc1tgpool02"}}}, attribution.AttributedSuspicious},
		{"an execution with no readable manifest", []db.ExecutionSince{{ID: 3, At: at, Action: manifest.Action{Target: "dc1tgpool03"}}}, attribution.AttributedSuspicious},
		{"the start, sealed as node + params.guest (TG-664 spelling)", []db.ExecutionSince{{ID: 4, At: at,
			Action: manifest.Action{OpClass: "start-guest", Target: "pve03", Params: map[string]string{"guest": "dc1tgpool03"}}}}, attribution.AuthorizedTest},
		{"the start, recorded after the read's window", []db.ExecutionSince{{ID: 5, At: tg693T(7, 10, 5, 0),
			Action: manifest.Action{OpClass: "start-guest", Target: "dc1tgpool03"}}}, attribution.AttributedSuspicious},
		{"the start, FQDN-spelled", []db.ExecutionSince{{ID: 6, At: at,
			Action: manifest.Action{OpClass: "start-guest", Target: "NLLEI01TGPOOL03.example.net"}}}, attribution.AuthorizedTest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if f := tg693Decide(t, store, tg693Execs(tc.rows...)); f.Taxonomy != tc.want {
				t.Fatalf("got %s, want %s (%v)", f.Taxonomy, tc.want, f.Warnings)
			}
		})
	}
}

// The lister reads with no upper bound (ExecutionsSince); the reader's contract is [from, to], so nothing later may
// come back. KILLING MUTATION: drop the `to` bound in proxmoxLaneExecutionTimes — RED here. (The pairing tolerance
// would also reject such an execution today; this pins the contract, not the coincidence.)
func TestProxmoxLaneExecutionTimesHonoursItsWindow(t *testing.T) {
	from, to := tg693T(6, 37, 30, 0), tg693T(7, 10, 0, 0)
	rows := []db.ExecutionSince{
		{ID: 1, At: tg693T(6, 46, 59, 0), Action: manifest.Action{OpClass: "start-guest", Target: "g"}},
		{ID: 2, At: to, Action: manifest.Action{OpClass: "start-guest", Target: "g"}},
		{ID: 3, At: to.Add(time.Second), Action: manifest.Action{OpClass: "start-guest", Target: "g"}},
	}
	got, err := proxmoxLaneExecutionTimes(tg693Execs(rows...))(context.Background(), "g", from, to)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !got[0].Equal(rows[0].At) || !got[1].Equal(to) {
		t.Fatalf("only executions inside [from, to] may come back, got %v", got)
	}
}

// An unreadable or truncated execution record is an error (fail closed), never "nothing to pair".
func TestAnUnreadableOrTruncatedProxmoxExecutionRecordFailsClosed(t *testing.T) {
	ctx := context.Background()
	from, to := tg693T(6, 37, 30, 0), tg693T(7, 10, 0, 0)
	broken := func(context.Context, time.Time, string, int) ([]db.ExecutionSince, error) {
		return nil, errors.New("db: connection refused")
	}
	if got, err := proxmoxLaneExecutionTimes(broken)(ctx, "g", from, to); err == nil || got != nil {
		t.Fatalf("an unreadable record must stay an error, got %v err=%v", got, err)
	}
	full := make([]db.ExecutionSince, proxmoxLaneExecutionLimit)
	var asked int
	page := func(_ context.Context, _ time.Time, _ string, limit int) ([]db.ExecutionSince, error) {
		asked = limit
		return full, nil
	}
	if got, err := proxmoxLaneExecutionTimes(page)(ctx, "g", from, to); err == nil || got != nil || asked != proxmoxLaneExecutionLimit {
		t.Fatalf("a full page is a truncated record and must fail closed, got %v err=%v (limit asked %d)", got, err, asked)
	}
}

// The boot gap report: a domain this process learns from the publication is not missing a self-actor — but only once
// an identity IS published there. KILLING MUTATIONS: clear the gap without reading the publication (the pre-review
// `haveDB` rule), or ignore the read's ok/err — RED on the unpublished / unreadable / no-database cases.
func TestPublishedSelfDomainsAreNotReportedAsSelfActorGaps(t *testing.T) {
	cfg := attribution.Config{SelfActors: map[string]string{}, Sanctioned: map[string][]string{"pve": {"root@pam"}, "journal": {"x"}}}
	armed := []string{"pve", "journal", "awx"}
	gaps := func(rd selfIdentityReader) map[string]attribution.DomainConfigGap {
		out := map[string]attribution.DomainConfigGap{}
		for _, g := range domainConfigGaps(cfg, armed, rd) {
			out[g.Domain] = g
		}
		return out
	}
	published := &tg693Store{}
	_, _ = published.Publish(context.Background(), "pve", "tg-actuate@pve!lane", "actuation")
	_, _ = published.Publish(context.Background(), "journal", "root!SHA256:x", "actuation")
	with := gaps(published)
	if _, ok := with["pve"]; ok {
		t.Fatalf("pve's identity is published: no gap, got %+v", with["pve"])
	}
	if _, ok := with["journal"]; ok {
		t.Fatalf("journal's identity is published: no gap, got %+v", with["journal"])
	}
	if g, ok := with["awx"]; !ok || !g.NoSelfActor || !g.NoSanctioned {
		t.Fatalf("awx has no publication and no sanctioned principal: both gaps stay, got %+v ok=%v", g, ok)
	}
	pveOnly := &tg693Store{}
	_, _ = pveOnly.Publish(context.Background(), "pve", "tg-actuate@pve!lane", "actuation")
	if g, ok := gaps(pveOnly)["journal"]; !ok || !g.NoSelfActor {
		t.Fatalf("a database with NOTHING published for journal is a real gap (the actuation credential never resolved), got %+v ok=%v", g, ok)
	}
	if g, ok := gaps(&memSelfIdentity{err: errors.New("db: down")})["pve"]; !ok || !g.NoSelfActor {
		t.Fatalf("an unreadable publication keeps the gap, got %+v ok=%v", g, ok)
	}
	if g, ok := gaps(nil)["pve"]; !ok || !g.NoSelfActor {
		t.Fatalf("with no database there is no publication to read: the pve gap is real, got %+v ok=%v", g, ok)
	}
}
