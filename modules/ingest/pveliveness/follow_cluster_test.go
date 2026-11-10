package pveliveness

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TG-623: the "watch ALL guests" ruling was encoded as a static name list, so three guests created later were
// never watched. Follow-cluster makes the watched set the allowlist PLUS every non-template guest the cluster
// reports on each poll.

func fcRow(typ, name, status string, template int) string {
	t := "0"
	if template == 1 {
		t = "1"
	}
	return `{"type":"` + typ + `","node":"dc1pve04","name":"` + name + `","vmid":104,"status":"` + status + `","template":` + t + `}`
}

func mkFollowSource(t *testing.T, f *fakeDoer, allowed []string) *Source {
	t.Helper()
	t.Setenv("PVE_LIVENESS_TEST_TOKEN", "env-tok")
	now := time.Date(2026, 11, 5, 21, 0, 0, 0, time.UTC)
	return New("https://pve:8006", "env:PVE_LIVENESS_TEST_TOKEN", allowed, "NL",
		WithHTTPClient(f), WithClock(func() time.Time { return now }), WithFollowCluster())
}

func TestFollowClusterWatchesAGuestCreatedAfterBoot(t *testing.T) {
	f := &fakeDoer{}
	s := mkFollowSource(t, f, []string{"dc1listed01"})
	f.body = resources(fcRow("lxc", "dc1listed01", "running", 0))
	if _, err := s.FetchActive(context.Background()); err != nil {
		t.Fatalf("poll1: %v", err)
	}
	// A guest that did not exist at boot appears, running, then stops.
	f.body = resources(fcRow("lxc", "dc1listed01", "running", 0) + "," + fcRow("qemu", "dc1brandnew01", "running", 0))
	if got, _ := s.FetchActive(context.Background()); len(got) != 0 {
		t.Fatalf("a newly observed running guest is not an edge, got %d", len(got))
	}
	f.body = resources(fcRow("lxc", "dc1listed01", "running", 0) + "," + fcRow("qemu", "dc1brandnew01", "stopped", 0))
	got, err := s.FetchActive(context.Background())
	if err != nil {
		t.Fatalf("poll3: %v", err)
	}
	if len(got) != 1 || got[0].Host != "dc1brandnew01" {
		t.Fatalf("TG-623: a guest created after boot must be watched under follow-cluster; got %+v", got)
	}
	if n := s.WatchedCount(); n != 2 {
		t.Fatalf("WatchedCount is the yield denominator and must count the followed guest: got %d, want 2", n)
	}
}

func TestFollowClusterNeverWatchesATemplate(t *testing.T) {
	f := &fakeDoer{}
	s := mkFollowSource(t, f, nil)
	f.body = resources(fcRow("qemu", "tmpl-debian12", "running", 1))
	_, _ = s.FetchActive(context.Background())
	f.body = resources(fcRow("qemu", "tmpl-debian12", "stopped", 1))
	if got, _ := s.FetchActive(context.Background()); len(got) != 0 {
		t.Fatalf("a template has no liveness and must never fire, got %d", len(got))
	}
	if n := s.WatchedCount(); n != 0 {
		t.Fatalf("a template must not be in the watched set, WatchedCount=%d", n)
	}
}

func TestFollowClusterWithEmptyAllowlistStillPolls(t *testing.T) {
	f := &fakeDoer{}
	s := mkFollowSource(t, f, nil)
	f.body = resources(fcRow("lxc", "dc1anyguest01", "running", 0))
	_, _ = s.FetchActive(context.Background())
	if f.calls == 0 {
		t.Fatal("follow-cluster with no allowlist must still read the cluster — the empty-allowlist short-circuit is for the static mode only")
	}
	f.body = resources(fcRow("lxc", "dc1anyguest01", "stopped", 0))
	if got, _ := s.FetchActive(context.Background()); len(got) != 1 {
		t.Fatalf("follow-cluster must fire for any non-template guest, got %d", len(got))
	}
}

func TestStaticModeIgnoresUnlistedGuestsEvenWithTemplateField(t *testing.T) {
	f := &fakeDoer{}
	s := mkSource(t, f, []string{"dc1listed01"}, time.Date(2026, 11, 5, 21, 0, 0, 0, time.UTC))
	if s.FollowsCluster() {
		t.Fatal("follow-cluster must be OFF unless WithFollowCluster is passed")
	}
	f.body = resources(fcRow("lxc", "dc1unlisted01", "running", 0))
	_, _ = s.FetchActive(context.Background())
	f.body = resources(fcRow("lxc", "dc1unlisted01", "stopped", 0))
	if got, _ := s.FetchActive(context.Background()); len(got) != 0 {
		t.Fatalf("static mode must stay byte-identical: an unlisted guest never fires, got %d", len(got))
	}
}

// TG-623: follow-cluster with no list is a WORKING detector — the self-test must pass and count the
// non-template guests it watches, not report "no managed guests configured".
func TestSelfTestPassesForFollowClusterWithNoList(t *testing.T) {
	rec := &probeSrv{}
	srv := newProbeServer(t, rec, serveJSON(200, `{"data":[`+
		fcRow("lxc", "dc1a01", "running", 0)+","+fcRow("qemu", "dc1b01", "stopped", 0)+","+
		fcRow("qemu", "tmpl-x", "stopped", 1)+`]}`))
	s := newProbeSource(t, srv.URL, nil)
	WithFollowCluster()(s)
	res, err := s.SelfTest(context.Background(), "alice@example")
	if err != nil {
		t.Fatalf("follow-cluster with no list must pass the self-test, got %v (%+v)", err, res)
	}
	if !strings.Contains(res.Summary, "2 non-template guest(s) watched") {
		t.Fatalf("summary must count the watched non-template guests (templates excluded), got %q", res.Summary)
	}
}
