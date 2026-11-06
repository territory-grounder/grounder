package httpfailover

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// countingRT counts round trips per host so a test can prove where requests went.
type countingRT struct {
	base http.RoundTripper
	hits map[string]*atomic.Int64
}

func (c *countingRT) RoundTrip(r *http.Request) (*http.Response, error) {
	if n, ok := c.hits[r.URL.Host]; ok {
		n.Add(1)
	}
	return c.base.RoundTrip(r)
}

// deadURL returns the URL of a listener that has been closed: a real "connection refused".
func deadURL(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	u := "http://" + l.Addr().String()
	_ = l.Close()
	return u
}

func liveServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body+" "+r.Method+" "+r.URL.Path)
	}))
	t.Cleanup(s.Close)
	return s
}

func hostOf(u string) string { return strings.TrimPrefix(u, "http://") }

func TestFailsOverFromADeadPrimaryAndSticks(t *testing.T) {
	dead := deadURL(t)
	live := liveServer(t, 200, "pve03")
	rt := &countingRT{base: http.DefaultTransport, hits: map[string]*atomic.Int64{hostOf(dead): {}, hostOf(live.URL): {}}}
	var logged int
	tr := New(rt, []string{dead, live.URL}, func(string, ...any) { logged++ })
	c := &http.Client{Transport: tr}

	for i := 0; i < 3; i++ {
		resp, err := c.Get(dead + "/api2/json/cluster/resources?type=vm")
		if err != nil {
			t.Fatalf("request %d: a dead primary must fail over to a live member, got %v", i, err)
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if !strings.Contains(string(b), "pve03 GET /api2/json/cluster/resources") {
			t.Fatalf("request %d: path, method must be preserved on the member, got %q", i, b)
		}
	}
	if n := rt.hits[hostOf(dead)].Load(); n != 1 {
		t.Fatalf("STICKY: a dead primary must cost one failed dial per process, not one per request — dialed %d times", n)
	}
	if logged != 1 {
		t.Fatalf("the switch to a new member must be logged exactly once, logged %d", logged)
	}
}

func TestAnHTTPResponseIsTheClustersAnswerNeverAFailover(t *testing.T) {
	primary := liveServer(t, 401, "primary")
	other := liveServer(t, 200, "other")
	rt := &countingRT{base: http.DefaultTransport, hits: map[string]*atomic.Int64{hostOf(other.URL): {}}}
	c := &http.Client{Transport: New(rt, []string{primary.URL, other.URL}, nil)}
	resp, err := c.Get(primary.URL + "/api2/json/version")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("a 401 from the primary must be returned as-is, got %d", resp.StatusCode)
	}
	if n := rt.hits[hostOf(other.URL)].Load(); n != 0 {
		t.Fatalf("an HTTP response must never fail over (would turn an auth refusal into a retry storm), other member hit %d", n)
	}
}

func TestNonMemberPassesThroughUntouched(t *testing.T) {
	dead := deadURL(t)
	outside := liveServer(t, 200, "librenms")
	c := &http.Client{Transport: New(http.DefaultTransport, []string{dead, deadURL(t)}, nil)}
	resp, err := c.Get(outside.URL + "/api/v0/devices")
	if err != nil {
		t.Fatalf("a non-member host must pass through: %v", err)
	}
	_ = resp.Body.Close()
}

func TestLifecyclePostWithNoBodyFailsOver(t *testing.T) {
	dead := deadURL(t)
	live := liveServer(t, 200, "pve04")
	c := &http.Client{Transport: New(http.DefaultTransport, []string{dead, live.URL}, nil)}
	resp, err := c.Post(dead+"/api2/json/nodes/pve04/lxc/101/status/start", "", nil)
	if err != nil {
		t.Fatalf("a bodiless lifecycle POST is safe to re-address: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(b), "POST /api2/json/nodes/pve04/lxc/101/status/start") {
		t.Fatalf("the POST must reach the member unchanged, got %q", b)
	}
}

func TestANonReplayableBodyIsNeverResent(t *testing.T) {
	dead := deadURL(t)
	live := liveServer(t, 200, "live")
	rt := &countingRT{base: http.DefaultTransport, hits: map[string]*atomic.Int64{hostOf(live.URL): {}}}
	tr := New(rt, []string{dead, live.URL}, nil)
	req, _ := http.NewRequest(http.MethodPost, dead+"/x", io.NopCloser(strings.NewReader("payload")))
	req.GetBody = nil
	if _, err := tr.RoundTrip(req); err == nil {
		t.Fatal("a request whose body cannot be replayed must not be re-sent to another member")
	}
	if n := rt.hits[hostOf(live.URL)].Load(); n != 0 {
		t.Fatalf("the live member must not receive a half-consumed body, hit %d", n)
	}
}

func TestEveryMemberDeadIsAnError(t *testing.T) {
	d1, d2 := deadURL(t), deadURL(t)
	c := &http.Client{Transport: New(http.DefaultTransport, []string{d1, d2}, nil)}
	if _, err := c.Get(d1 + "/api2/json/version"); err == nil || !strings.Contains(err.Error(), "every member") {
		t.Fatalf("all members dead must surface an error naming the group, got %v", err)
	}
}

func TestMembersNormalizeAndDedupe(t *testing.T) {
	tr := New(nil, []string{"https://PVE04.example.net:8006", "https://pve04.example.net:8006/", "not a url", " ", "https://pve03.example.net"}, nil)
	got := tr.Members()
	if len(got) != 2 || got[0] != "https://pve04.example.net:8006" || got[1] != "https://pve03.example.net:443" {
		t.Fatalf("members must normalize scheme/host/port and drop duplicates and junk, got %v", got)
	}
}
