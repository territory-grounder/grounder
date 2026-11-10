package pve

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// prodReadRetryBackoff is the SHIPPED schedule, captured before init() shrinks it for fast tests (package
// variables are initialised before init functions run).
var prodReadRetryBackoff = append([]time.Duration(nil), readRetryBackoff...)

func init() {
	readRetryBackoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond, time.Millisecond}
}

// flakyCluster answers the resources read with `status` for its first `fail` calls, then the real cluster.
type flakyCluster struct {
	fail   int32
	status int
	calls  atomic.Int32
	inner  fakeCluster
}

func (f *flakyCluster) Do(req *http.Request) (*http.Response, error) {
	if strings.Contains(req.URL.Path, "/cluster/resources") && f.calls.Add(1) <= f.fail {
		return &http.Response{StatusCode: f.status, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}}, nil
	}
	return f.inner.Do(req)
}

func retryTestCluster() fakeCluster {
	return fakeCluster{
		resources: `{"data":[{"node":"pve03","name":"web01","vmid":101,"type":"lxc","status":"stopped"}]}`,
		tasks:     `{"data":[]}`,
	}
}

// TG-652: the 2026-11-07 shape. A 596 from an overloaded node is retried, and the read then answers.
func TestReadRetriesATransient596ThenAnswers(t *testing.T) {
	t.Setenv("PVE_RO_TOKEN", "root@pam!tg-ro=secret")
	f := &flakyCluster{fail: 2, status: 596, inner: retryTestCluster()}
	out, err := New("https://pve03:8006", ref, WithHTTPClient(f)).Read(context.Background(), "web01", time.Now().Add(-time.Hour), time.Now())
	if err != nil {
		t.Fatalf("a transient 596 must be retried, not returned: %v", err)
	}
	if len(out) != 1 || !out[0].Covered {
		t.Fatalf("after the retry the read must answer (a coverage marker here), got %+v", out)
	}
	if n := f.calls.Load(); n != 3 {
		t.Fatalf("expected 2 failed attempts then success (3 resources calls), got %d", n)
	}
}

// A 4xx is a verdict, never retried.
func TestReadNeverRetriesA4xx(t *testing.T) {
	t.Setenv("PVE_RO_TOKEN", "root@pam!tg-ro=secret")
	for _, st := range []int{401, 403, 404} {
		f := &flakyCluster{fail: 99, status: st, inner: retryTestCluster()}
		if _, err := New("https://pve03:8006", ref, WithHTTPClient(f)).Read(context.Background(), "web01", time.Now().Add(-time.Hour), time.Now()); err == nil {
			t.Fatalf("status %d must fail", st)
		}
		if n := f.calls.Load(); n != 1 {
			t.Fatalf("status %d was attempted %d times; a 4xx must be attempted exactly once", st, n)
		}
	}
}

// A node that keeps failing still fails, after exactly the bounded schedule, with PVE's own error text.
func TestReadPersistent5xxFailsAfterTheBoundedSchedule(t *testing.T) {
	t.Setenv("PVE_RO_TOKEN", "root@pam!tg-ro=secret")
	f := &flakyCluster{fail: 99, status: 596, inner: retryTestCluster()}
	_, err := New("https://pve03:8006", ref, WithHTTPClient(f)).Read(context.Background(), "web01", time.Now().Add(-time.Hour), time.Now())
	if err == nil || !strings.Contains(err.Error(), "→ 596") {
		t.Fatalf("a persistent 596 must fail with PVE's own status in the error, got %v", err)
	}
	if want := int32(len(readRetryBackoff) + 1); f.calls.Load() != want {
		t.Fatalf("made %d attempts, want exactly %d (1 + bounded schedule)", f.calls.Load(), want)
	}
}

// The shipped schedule fits inside the default per-Read timeout, so the retry never extends how long a dead
// endpoint can hold a triage.
func TestReadRetryScheduleFitsTheReadTimeout(t *testing.T) {
	var total time.Duration
	for _, d := range prodReadRetryBackoff {
		total += d
	}
	if def := New("https://pve:8006", ref).timeout; total >= def {
		t.Fatalf("retry schedule %s must fit inside the default read timeout %s", total, def)
	}
	if len(prodReadRetryBackoff) == 0 {
		t.Fatal("the shipped schedule must retry at least once (TG-652)")
	}
}
