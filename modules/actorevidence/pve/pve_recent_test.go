package pve

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type rrdDoer struct {
	guestType string
	points    []map[string]any
	status    int
}

func (d rrdDoer) Do(r *http.Request) (*http.Response, error) {
	reply := func(code int, v any) (*http.Response, error) {
		b, _ := json.Marshal(map[string]any{"data": v})
		return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(string(b))), Header: http.Header{}}, nil
	}
	if strings.HasSuffix(r.URL.Path, "/cluster/resources") {
		return reply(200, []map[string]any{{"name": "g1", "node": "n1", "vmid": 101, "type": d.guestType}})
	}
	if d.status != 0 {
		return reply(d.status, nil)
	}
	return reply(200, d.points)
}

func rrdModule(t *testing.T, d rrdDoer) *Module {
	t.Helper()
	t.Setenv("TG_TEST_PVE_TOKEN", "t")
	return New("https://pve.example:8006", "env:TG_TEST_PVE_TOKEN", WithHTTPClient(d))
}

func TestRanRecentlySeesARecentCPUSample(t *testing.T) {
	now := time.Now().Unix()
	m := rrdModule(t, rrdDoer{guestType: "lxc", points: []map[string]any{{"time": now - 7200}, {"time": now - 600, "cpu": 0.01}}})
	if ran, err := m.RanRecently(context.Background(), "g1", time.Hour); err != nil || !ran {
		t.Fatalf("ran=%v err=%v", ran, err)
	}
}

func TestRanRecentlyIgnoresOldSamplesAndNullCPU(t *testing.T) {
	now := time.Now().Unix()
	m := rrdModule(t, rrdDoer{guestType: "qemu", points: []map[string]any{{"time": now - 9000, "cpu": 0.5}, {"time": now - 60}}})
	if ran, err := m.RanRecently(context.Background(), "g1", time.Hour); err != nil || ran {
		t.Fatalf("ran=%v err=%v, want false", ran, err)
	}
}

func TestRanRecentlyFailsClosed(t *testing.T) {
	if _, err := rrdModule(t, rrdDoer{guestType: "lxc", status: 403}).RanRecently(context.Background(), "g1", time.Hour); err == nil {
		t.Fatal("a refused read must error")
	}
	if _, err := rrdModule(t, rrdDoer{guestType: "lxc"}).RanRecently(context.Background(), "g1", time.Hour); err == nil {
		t.Fatal("an empty history must error")
	}
	if _, err := rrdModule(t, rrdDoer{guestType: "lxc"}).RanRecently(context.Background(), "nope", time.Hour); err == nil {
		t.Fatal("an unknown guest must error")
	}
}

type permDoer struct{ body string }

func (d permDoer) Do(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(d.body)), Header: http.Header{}}, nil
}

func TestCheckCanReadAllTasks(t *testing.T) {
	t.Setenv("TG_TEST_PVE_TOKEN", "t")
	yes := New("https://pve.example:8006", "env:TG_TEST_PVE_TOKEN", WithHTTPClient(permDoer{`{"data":{"/nodes":{"Sys.Audit":1}}}`}))
	if err := yes.CheckCanReadAllTasks(context.Background()); err != nil {
		t.Fatalf("a token with Sys.Audit: %v", err)
	}
	no := New("https://pve.example:8006", "env:TG_TEST_PVE_TOKEN", WithHTTPClient(permDoer{`{"data":{"/vms":{"VM.Audit":1}}}`}))
	if err := no.CheckCanReadAllTasks(context.Background()); err == nil {
		t.Fatal("a token without Sys.Audit must fail")
	}
}
