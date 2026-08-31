package pveliveness

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	coreingest "github.com/territory-grounder/grounder/core/ingest"
)

// fakeDoer returns a settable /cluster/resources body so a test can change guest status between polls.
type fakeDoer struct {
	body   string
	status int
	err    error
	calls  int
}

func (f *fakeDoer) Do(req *http.Request) (*http.Response, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	if req.Header.Get("Authorization") != "PVEAPIToken=env-tok" {
		return &http.Response{StatusCode: 401, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	}
	st := f.status
	if st == 0 {
		st = 200
	}
	return &http.Response{StatusCode: st, Body: io.NopCloser(strings.NewReader(f.body))}, nil
}

func resources(rows string) string { return `{"data":[` + rows + `]}` }

func lxc(name, status string) string {
	return `{"type":"lxc","node":"dc1pve01","name":"` + name + `","vmid":101,"status":"` + status + `"}`
}

// mkSource builds a source with the fake transport + a fixed clock.
func mkSource(t *testing.T, f *fakeDoer, allowed []string, now time.Time) *Source {
	t.Setenv("PVE_LIVENESS_TEST_TOKEN", "env-tok")
	return New("https://pve:8006", "env:PVE_LIVENESS_TEST_TOKEN", allowed, "NL",
		WithHTTPClient(f), WithClock(func() time.Time { return now }))
}

func TestEdgeTriggersOnlyOnRunningToStopped(t *testing.T) {
	f := &fakeDoer{}
	now := time.Date(2026, 7, 26, 1, 0, 0, 0, time.UTC)
	s := mkSource(t, f, []string{"dc1reactive01"}, now)

	// Poll 1: running -> records prior, no fire.
	f.body = resources(lxc("dc1reactive01", "running"))
	got, err := s.FetchActive(context.Background())
	if err != nil {
		t.Fatalf("poll1: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("poll1 (running): expected 0 envelopes, got %d", len(got))
	}

	// Poll 2: stopped -> running→stopped transition -> ONE envelope.
	f.body = resources(lxc("dc1reactive01", "stopped"))
	got, err = s.FetchActive(context.Background())
	if err != nil {
		t.Fatalf("poll2: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("poll2 (running→stopped): expected 1 envelope, got %d", len(got))
	}
	env := got[0]
	if env.Host != "dc1reactive01" {
		t.Errorf("host = %q, want dc1reactive01", env.Host)
	}
	if env.Severity != coreingest.SeverityCritical {
		t.Errorf("severity = %v, want critical", env.Severity)
	}
	if env.AlertRule != DeviceDownRule {
		t.Errorf("alert_rule = %q, want %q", env.AlertRule, DeviceDownRule)
	}
	if env.SourceID != SourceType {
		t.Errorf("source = %q, want %q", env.SourceID, SourceType)
	}
	if !strings.HasPrefix(env.ExternalRef, "tg-liveness-dc1reactive01-") {
		t.Errorf("external_ref = %q, want tg-liveness-dc1reactive01-<ts>", env.ExternalRef)
	}
	if env.Site != "dc1" {
		t.Errorf("site = %q, want dc1 (the source's 'NL' site is canonicalized to the deployment-key form at the ingest boundary, TG-456)", env.Site)
	}

	// Poll 3: still stopped -> NO re-fire (no new transition).
	got, err = s.FetchActive(context.Background())
	if err != nil {
		t.Fatalf("poll3: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("poll3 (still stopped): expected 0 (no re-fire), got %d", len(got))
	}

	// Poll 4: stopped -> running (a heal): NO fire (up transition is not a down edge).
	f.body = resources(lxc("dc1reactive01", "running"))
	got, _ = s.FetchActive(context.Background())
	if len(got) != 0 {
		t.Fatalf("poll4 (stopped→running heal): expected 0, got %d", len(got))
	}

	// Poll 5: running -> stopped AGAIN (a second, distinct fault): fires again.
	f.body = resources(lxc("dc1reactive01", "stopped"))
	got, _ = s.FetchActive(context.Background())
	if len(got) != 1 {
		t.Fatalf("poll5 (re-fault): expected 1, got %d", len(got))
	}
}

func TestGuestAlreadyStoppedAtStartupDoesNotStorm(t *testing.T) {
	f := &fakeDoer{}
	now := time.Date(2026, 7, 26, 1, 0, 0, 0, time.UTC)
	s := mkSource(t, f, []string{"dc1reactive01"}, now)
	// First-ever observation is "stopped" (unknown→stopped) — NOT a running→stopped edge.
	f.body = resources(lxc("dc1reactive01", "stopped"))
	got, err := s.FetchActive(context.Background())
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("unknown→stopped must NOT fire (avoid startup storm), got %d", len(got))
	}
}

func TestNonAllowlistedGuestNeverFires(t *testing.T) {
	f := &fakeDoer{}
	now := time.Date(2026, 7, 26, 1, 0, 0, 0, time.UTC)
	s := mkSource(t, f, []string{"dc1reactive01"}, now)
	f.body = resources(lxc("dc1infra99", "running"))
	if got, _ := s.FetchActive(context.Background()); len(got) != 0 {
		t.Fatalf("poll1 non-allowlisted: got %d", len(got))
	}
	f.body = resources(lxc("dc1infra99", "stopped"))
	if got, _ := s.FetchActive(context.Background()); len(got) != 0 {
		t.Fatalf("non-allowlisted guest transition must NOT fire, got %d", len(got))
	}
}

func TestEmptyAllowlistWatchesNothing(t *testing.T) {
	f := &fakeDoer{}
	now := time.Date(2026, 7, 26, 1, 0, 0, 0, time.UTC)
	s := mkSource(t, f, nil, now)
	f.body = resources(lxc("dc1reactive01", "running"))
	got, err := s.FetchActive(context.Background())
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("empty allowlist must watch nothing, got %d", len(got))
	}
	if f.calls != 0 {
		t.Fatalf("empty allowlist must not even call the API, calls=%d", f.calls)
	}
}

func TestMultipleGuestTransitionsInOnePoll(t *testing.T) {
	f := &fakeDoer{}
	now := time.Date(2026, 7, 26, 1, 0, 0, 0, time.UTC)
	s := mkSource(t, f, []string{"dc1reactive01", "dc1mealie01"}, now)
	// Poll 1: both running — records prior, no fire.
	f.body = resources(lxc("dc1reactive01", "running") + "," + lxc("dc1mealie01", "running"))
	if got, _ := s.FetchActive(context.Background()); len(got) != 0 {
		t.Fatalf("poll1: expected 0, got %d", len(got))
	}
	// Poll 2: BOTH transition running→stopped in one poll — two independent envelopes.
	f.body = resources(lxc("dc1reactive01", "stopped") + "," + lxc("dc1mealie01", "stopped"))
	got, err := s.FetchActive(context.Background())
	if err != nil {
		t.Fatalf("poll2: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("two simultaneous transitions must mint 2 envelopes, got %d", len(got))
	}
	hosts := map[string]bool{got[0].Host: true, got[1].Host: true}
	if !hosts["dc1reactive01"] || !hosts["dc1mealie01"] {
		t.Fatalf("expected both hosts, got %v", hosts)
	}
	if got[0].ExternalRef == got[1].ExternalRef {
		t.Fatalf("the two envelopes must carry distinct external_refs, both = %q", got[0].ExternalRef)
	}
}

func TestInvalidHostnameGuestSafelyDropped(t *testing.T) {
	f := &fakeDoer{}
	now := time.Date(2026, 7, 26, 1, 0, 0, 0, time.UTC)
	// A guest whose name is not a valid RFC-1123 host ('@') cannot form a valid envelope: core/ingest.Normalize
	// rejects it, and envelopeFor returns ok=false. It must be DROPPED — never fired, never panicked (fail-safe:
	// a miss, not a false or malformed incident). Real Proxmox guest names are valid hostnames, so this never
	// bites in practice; the property is that a pathological name degrades to a skip.
	s := mkSource(t, f, []string{"dc1reactive@01"}, now)
	f.body = resources(lxc("dc1reactive@01", "running"))
	s.FetchActive(context.Background())
	f.body = resources(lxc("dc1reactive@01", "stopped"))
	got, err := s.FetchActive(context.Background())
	if err != nil {
		t.Fatalf("an invalid-host guest must not error the poll, got %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("an invalid-host guest must be dropped (no envelope), got %d", len(got))
	}
	// A valid hyphenated name (already slug-safe) fires normally — the common case.
	s2 := mkSource(t, f, []string{"dc1-reactive01"}, now)
	f.body = resources(lxc("dc1-reactive01", "running"))
	s2.FetchActive(context.Background())
	f.body = resources(lxc("dc1-reactive01", "stopped"))
	got, _ = s2.FetchActive(context.Background())
	if len(got) != 1 {
		t.Fatalf("a valid hyphenated guest name must fire, got %d", len(got))
	}
	if !strings.HasPrefix(got[0].ExternalRef, "tg-liveness-dc1-reactive01-") {
		t.Errorf("external_ref = %q, want tg-liveness-dc1-reactive01-<ts>", got[0].ExternalRef)
	}
}

func TestFetchErrorReturnsNoEnvelopes(t *testing.T) {
	now := time.Date(2026, 7, 26, 1, 0, 0, 0, time.UTC)
	f := &fakeDoer{status: 500, body: `{}`}
	s := mkSource(t, f, []string{"dc1reactive01"}, now)
	got, err := s.FetchActive(context.Background())
	if err == nil {
		t.Fatal("a non-2xx fetch must return an error")
	}
	if len(got) != 0 {
		t.Fatalf("error path must return 0 envelopes, got %d", len(got))
	}
}

// TG-496 — the detector caches the watched guests' states from its OWN ~37s fetch so the worker can refresh
// the guest_liveness projection at that cadence (not just the 5-min estate sweep), which is what makes the
// deterministic guest-down heal + the TG-378 seal gate actually reachable on a fresh down.

// TestGuestStatesUnknownBeforeAnyFetch: before a successful fetch the accessor reports (nil,false) — the
// caller must then write NOTHING (unknown, never an invented empty cluster).
func TestGuestStatesUnknownBeforeAnyFetch(t *testing.T) {
	f := &fakeDoer{}
	now := time.Date(2026, 8, 15, 1, 0, 0, 0, time.UTC)
	s := mkSource(t, f, []string{"dc1reactive01"}, now)
	if states, ok := s.GuestStates(); ok || states != nil {
		t.Fatalf("before any fetch GuestStates must be (nil,false) — unknown, never an invented empty cluster; got (%v,%v)", states, ok)
	}
	if f.calls != 0 {
		t.Fatalf("GuestStates must not fetch, calls=%d", f.calls)
	}
}

// TestGuestStatesCacheWatchedRunningAndStopped: every WATCHED guest is cached (running AND stopped) with the
// fetch time as ObservedAt; a non-allowlisted guest is NOT cached (the estate sweep is the all-guests
// backstop); and a running→stopped transition reflects STOPPED in the SAME fetch that mints its down-envelope.
func TestGuestStatesCacheWatchedRunningAndStopped(t *testing.T) {
	f := &fakeDoer{}
	now := time.Date(2026, 8, 15, 1, 0, 0, 0, time.UTC)
	s := mkSource(t, f, []string{"g-run", "g-stop"}, now)

	f.body = resources(lxc("g-run", "running") + "," + lxc("g-stop", "running") + "," + lxc("infra99", "running"))
	if _, err := s.FetchActive(context.Background()); err != nil {
		t.Fatalf("fetch1: %v", err)
	}
	states, ok := s.GuestStates()
	if !ok {
		t.Fatal("after a successful fetch GuestStates must report ok=true")
	}
	byGuest := map[string]string{}
	for _, st := range states {
		byGuest[st.Guest] = st.Status
		if !st.ObservedAt.Equal(now) {
			t.Fatalf("guest %s: ObservedAt=%v, want the fetch time %v (the monotone-guard stamp)", st.Guest, st.ObservedAt, now)
		}
	}
	if len(byGuest) != 2 || byGuest["g-run"] != "running" || byGuest["g-stop"] != "running" {
		t.Fatalf("expected the two WATCHED guests cached as running, got %v", byGuest)
	}
	if _, present := byGuest["infra99"]; present {
		t.Fatal("a non-allowlisted guest must NOT be cached — the estate sweep is the all-guests backstop")
	}

	// g-stop transitions running→stopped: the cache must reflect STOPPED in the SAME fetch that mints the
	// down-envelope (that co-timing is what lets the worker project-before-dispatch).
	f.body = resources(lxc("g-run", "running") + "," + lxc("g-stop", "stopped"))
	envs, err := s.FetchActive(context.Background())
	if err != nil {
		t.Fatalf("fetch2: %v", err)
	}
	if len(envs) != 1 || envs[0].Host != "g-stop" {
		t.Fatalf("expected exactly one down-envelope for g-stop, got %+v", envs)
	}
	got := map[string]string{}
	states2, _ := s.GuestStates()
	for _, st := range states2 {
		got[st.Guest] = st.Status
	}
	if got["g-stop"] != "stopped" {
		t.Fatalf("the cache must reflect g-stop=stopped in the SAME fetch that minted its down-envelope, got %v", got)
	}
	if got["g-run"] != "running" {
		t.Fatalf("g-run must remain cached as running, got %v", got)
	}
}

// TestFetchFailureLeavesLastGoodCacheUnrefreshed (honesty): a failed fetch must not touch the cache — no
// invented state, no refreshed staleness. It mirrors pve.EstateSource: the reader's freshness bound retires
// an old reading, not a transport/parse error minting or re-stamping one.
func TestFetchFailureLeavesLastGoodCacheUnrefreshed(t *testing.T) {
	f := &fakeDoer{}
	now := time.Date(2026, 8, 15, 1, 0, 0, 0, time.UTC)
	s := mkSource(t, f, []string{"g1"}, now)
	f.body = resources(lxc("g1", "running"))
	if _, err := s.FetchActive(context.Background()); err != nil {
		t.Fatalf("fetch1: %v", err)
	}
	f.status = 500
	f.body = `{}`
	if _, err := s.FetchActive(context.Background()); err == nil {
		t.Fatal("a non-2xx fetch must error")
	}
	states, ok := s.GuestStates()
	if !ok || len(states) != 1 || states[0].Guest != "g1" || states[0].Status != "running" || !states[0].ObservedAt.Equal(now) {
		t.Fatalf("a failed fetch must leave the last-good cache intact (state AND its stamp), got ok=%v states=%+v", ok, states)
	}
}
