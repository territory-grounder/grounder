package pve

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/config"
)

// fakeCluster answers the two PVE endpoints the reader calls.
type fakeCluster struct {
	resources string
	tasks     string
}

func (f fakeCluster) Do(req *http.Request) (*http.Response, error) {
	var body string
	switch {
	case strings.Contains(req.URL.Path, "/cluster/resources"):
		body = f.resources
	case strings.Contains(req.URL.Path, "/tasks"):
		body = f.tasks
	default:
		body = `{"data":[]}`
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
}

const ref = config.SecretRef("env:PVE_RO_TOKEN")

func TestReadProducesTypedLifecycleEvidence(t *testing.T) {
	t.Setenv("PVE_RO_TOKEN", "root@pam!tg-ro=secret")
	start := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	// The tasks fixture uses the REAL /nodes/{node}/tasks shape: the guest is named in "id" (a string), and a
	// token-authed action carries "user":"root@pam" WITH a separate "tokenid" (the full identity only exists
	// once recombined). A fixture that mirrors the code's field names would hide exactly the drop the live API
	// caused (an "id" vs "vmid" field mismatch), so this pins the wire shape, not the struct.
	fc := fakeCluster{
		resources: `{"data":[{"node":"pve03","name":"web01","vmid":101,"type":"lxc","status":"running"}]}`,
		tasks: `{"data":[
			{"upid":"UPID:pve03:001","user":"root@pam","type":"vzstop","id":"101","starttime":` + itoa(start.Add(-5*time.Minute).Unix()) + `,"status":"OK"},
			{"upid":"UPID:pve03:002","user":"root@pam","tokenid":"tg-actuate","type":"vzstart","id":"101","starttime":` + itoa(start.Add(-2*time.Minute).Unix()) + `,"status":"OK"},
			{"upid":"UPID:pve03:003","user":"root@pam","type":"vzstop","id":"101","starttime":` + itoa(start.Add(-2*time.Hour).Unix()) + `,"status":"OK"},
			{"upid":"UPID:pve03:004","user":"root@pam","type":"vzstop","id":"999","starttime":` + itoa(start.Add(-3*time.Minute).Unix()) + `,"status":"OK"},
			{"upid":"UPID:pve03:005","user":"root@pam","type":"aptupdate","id":"101","starttime":` + itoa(start.Add(-1*time.Minute).Unix()) + `,"status":"OK"}
		]}`,
	}
	m := New("https://pve03:8006", ref, WithHTTPClient(fc))
	out, err := m.Read(context.Background(), "web01", start.Add(-30*time.Minute), start)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// Only the in-window, this-guest, lifecycle rows survive: the vzstop + vzstart (not the 2h-old row,
	// not guest id 999, not the aptupdate non-lifecycle verb).
	if len(out) != 2 {
		t.Fatalf("want 2 lifecycle rows in-window for web01, got %d: %+v", len(out), out)
	}
	if out[0].Actor != "root@pam" || out[0].ActionKind != "vzstop" || out[0].Domain != "pve" || !out[0].Covered {
		t.Fatalf("the stop row must be typed pve/root@pam/vzstop/covered, got %+v", out[0])
	}
	// REGRESSION (self-identity): a token-authed row must recombine user+tokenid into "root@pam!tg-actuate" —
	// keying on "user" alone (the bare principal) would make attributed-self impossible to ever fire.
	if out[1].Actor != "root@pam!tg-actuate" || out[1].Ref == "" {
		t.Fatalf("the start row must name TG's own identity (user!tokenid) with its UPID ref, got %+v", out[1])
	}
}

// REGRESSION (panic): a non-2xx PVE response with a SHORT body (an empty 401, a 4-byte error) must return an
// error, never panic — the old unconditional [:200] slice crashed on any trimmed body under 200 bytes, and
// REQ-2307 requires an auth failure to degrade to "evidence absent", not take down the session.
func TestReadDegradesOnShortErrorBodyWithoutPanic(t *testing.T) {
	t.Setenv("PVE_RO_TOKEN", "root@pam!tg-ro=secret")
	for name, body := range map[string]string{"empty": "", "short": "401"} {
		t.Run(name, func(t *testing.T) {
			m := New("https://pve:8006", ref, WithHTTPClient(shortErrorDoer{body: body}))
			if _, err := m.Read(context.Background(), "web01", time.Now().Add(-time.Hour), time.Now()); err == nil {
				t.Fatal("a non-2xx response must return an error (advisory, REQ-2307)")
			}
		})
	}
}

// shortErrorDoer answers every request with a short non-2xx body (the panic trigger).
type shortErrorDoer struct{ body string }

func (d shortErrorDoer) Do(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 401, Body: io.NopCloser(strings.NewReader(d.body)), Header: http.Header{}}, nil
}

func TestReadFailsClosedOnAmbiguousOrMissingGuest(t *testing.T) {
	t.Setenv("PVE_RO_TOKEN", "root@pam!tg-ro=secret")
	for name, resources := range map[string]string{
		"missing":   `{"data":[{"node":"pve01","name":"db02","vmid":100,"type":"lxc"}]}`,
		"ambiguous": `{"data":[{"node":"pve01","name":"web01","vmid":100,"type":"lxc"},{"node":"pve03","name":"web01","vmid":101,"type":"qemu"}]}`,
	} {
		m := New("https://pve:8006", ref, WithHTTPClient(fakeCluster{resources: resources}))
		if _, err := m.Read(context.Background(), "web01", time.Now().Add(-time.Hour), time.Now()); err == nil {
			t.Fatalf("a %s guest must fail closed (no guessed node), got nil error", name)
		}
	}
}

func itoa(unix int64) string {
	return strconv.FormatInt(unix, 10)
}
