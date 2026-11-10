package pve

// TG-693: a PVE task run under TG's own actuation token is TG's only when TG's own execution record explains it, and
// the reader fails closed (an error ⇒ indeterminate ⇒ HOLD) whenever it cannot tell.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/attribution"
)

const tg693Self = "tg-actuate@pve!lane"

// tg693Cluster is the live 2026-11-08 sequence on a pool guest: the injector's stop (06:46:38Z), TG's heal
// (06:46:54Z, recorded as execution 589 at 06:46:59.7Z), the injector's re-fault (07:07:14Z) — plus, optionally,
// a second task under TG's token.
func tg693Cluster(extra string) fakeCluster {
	return fakeCluster{
		resources: `{"data":[{"node":"pve03","name":"pool03","vmid":101101233,"type":"lxc","status":"stopped"}]}`,
		tasks: `{"data":[
			{"upid":"UPID:a","user":"root@pam","type":"vzstop","id":"101101233","starttime":` + itoa(tg693At(6, 46, 38).Unix()) + `,"status":"OK"},
			{"upid":"UPID:b","user":"tg-actuate@pve","tokenid":"lane","type":"vzstart","id":"101101233","starttime":` + itoa(tg693At(6, 46, 54).Unix()) + `,"status":"OK"},
			{"upid":"UPID:c","user":"root@pam","type":"vzstop","id":"101101233","starttime":` + itoa(tg693At(7, 7, 14).Unix()) + `,"status":"OK"}` + extra + `
		]}`,
	}
}

func tg693At(h, m, s int) time.Time { return time.Date(2026, 11, 8, h, m, s, 0, time.UTC) }

type tg693Execs struct {
	times    []time.Time
	err      error
	guest    string
	from, to time.Time
	calls    int
}

func (e *tg693Execs) read(_ context.Context, guest string, from, to time.Time) ([]time.Time, error) {
	e.calls++
	e.guest, e.from, e.to = guest, from, to
	return e.times, e.err
}

func tg693Read(t *testing.T, fc fakeCluster, opts ...Option) ([]attribution.Evidence, error) {
	t.Helper()
	t.Setenv("PVE_RO_TOKEN", "root@pam!tg-ro=secret")
	m := New("https://pve03:8006", ref, append([]Option{WithHTTPClient(fc)}, opts...)...)
	until := tg693At(7, 8, 0)
	return m.Read(context.Background(), "pool03", until.Add(-30*time.Minute), until)
}

func tg693Find(t *testing.T, out []attribution.Evidence, actor string) []attribution.Evidence {
	t.Helper()
	var got []attribution.Evidence
	for _, e := range out {
		if e.Actor == actor {
			got = append(got, e)
		}
	}
	return got
}

// KILLING MUTATION: drop the correlateSelf call in Read — RED (TG's task comes back unmarked, which attribution then
// reads as a stranger's token on a split deployment: the live TG-693 hold).
func TestTGsOwnTaskIsCorrelatedOnlyBesideItsExecution(t *testing.T) {
	ex := &tg693Execs{times: []time.Time{tg693At(6, 46, 59).Add(717 * time.Millisecond)}}
	out, err := tg693Read(t, tg693Cluster(""), WithSelfCorrelation(func() string { return tg693Self }, ex.read))
	if err != nil {
		t.Fatal(err)
	}
	mine := tg693Find(t, out, tg693Self)
	if len(mine) != 1 || !mine[0].SelfCorrelated || mine[0].SelfUnexplained {
		t.Fatalf("TG's heal task beside execution 589 must read correlated, got %+v", mine)
	}
	for _, e := range tg693Find(t, out, "root@pam") {
		if e.SelfCorrelated || e.SelfUnexplained {
			t.Fatalf("the injector's stops are not TG's and must never be marked, got %+v", e)
		}
	}
	until := tg693At(7, 8, 0)
	if ex.guest != "pool03" || !ex.from.Equal(until.Add(-30*time.Minute-SelfExecutionBefore)) || !ex.to.Equal(until.Add(SelfExecutionAfter)) {
		t.Fatalf("the record must be read for the guest over the window widened by the tolerance, got %q [%v, %v]", ex.guest, ex.from, ex.to)
	}

	// The same task with no execution behind it (the TG-691 ledger gap, or a stolen token) reads unexplained.
	none := &tg693Execs{}
	out, err = tg693Read(t, tg693Cluster(""), WithSelfCorrelation(func() string { return tg693Self }, none.read))
	if err != nil {
		t.Fatal(err)
	}
	if mine := tg693Find(t, out, tg693Self); len(mine) != 1 || !mine[0].SelfUnexplained || mine[0].SelfCorrelated {
		t.Fatalf("TG's token task with no TG execution must read unexplained, got %+v", mine)
	}
}

// A second task under TG's token beside ONE execution is a token holder riding on a real heal.
func TestASecondTGTokenTaskBesideOneExecutionIsUnexplained(t *testing.T) {
	second := `,{"upid":"UPID:d","user":"tg-actuate@pve","tokenid":"lane","type":"vzstop","id":"101101233","starttime":` + itoa(tg693At(6, 47, 30).Unix()) + `,"status":"OK"}`
	ex := &tg693Execs{times: []time.Time{tg693At(6, 46, 59)}}
	out, err := tg693Read(t, tg693Cluster(second), WithSelfCorrelation(func() string { return tg693Self }, ex.read))
	if err != nil {
		t.Fatal(err)
	}
	mine := tg693Find(t, out, tg693Self)
	if len(mine) != 2 || !mine[0].SelfCorrelated || !mine[1].SelfUnexplained {
		t.Fatalf("one execution explains one task: want [correlated, unexplained], got %+v", mine)
	}
}

// Every way the reader cannot tell fails the read (indeterminate ⇒ HOLD), never a guess. KILLING MUTATIONS: return
// nil instead of the error on an unreadable record / a missing record / an unpublished identity — RED on the case.
func TestTheReaderFailsClosedWhenItCannotTellTGsTaskFromAStrangers(t *testing.T) {
	self := func() string { return tg693Self }
	cases := []struct {
		name string
		opt  Option
		want string
	}{
		{"the execution record is unreadable", WithSelfCorrelation(self, (&tg693Execs{err: errors.New("db: connection refused")}).read), "unreadable"},
		{"no execution record is wired", WithSelfCorrelation(self, nil), "no execution record"},
		{"TG's identity is not published yet", WithSelfCorrelation(func() string { return "" }, (&tg693Execs{}).read), "not published"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := tg693Read(t, tg693Cluster(""), tc.opt)
			if err == nil || !strings.Contains(err.Error(), tc.want) || out != nil {
				t.Fatalf("want a %q error and no evidence, got %v / %+v", tc.want, err, out)
			}
		})
	}
}

// Narrow by construction: TG's PVE identity is always a token principal, so a read holding only password/ticket
// principals is answered even while the identity is unpublished, and the record is not even consulted.
// KILLING MUTATION: make the unpublished-identity branch fail on ANY row — RED here.
func TestAnUnpublishedIdentityBlindsOnlyReadsHoldingATokenTask(t *testing.T) {
	adminOnly := fakeCluster{
		resources: `{"data":[{"node":"pve03","name":"pool03","vmid":101101233,"type":"lxc","status":"stopped"}]}`,
		tasks:     `{"data":[{"upid":"UPID:a","user":"root@pam","type":"vzstop","id":"101101233","starttime":` + itoa(tg693At(7, 7, 14).Unix()) + `,"status":"OK"}]}`,
	}
	ex := &tg693Execs{}
	out, err := tg693Read(t, adminOnly, WithSelfCorrelation(func() string { return "" }, ex.read))
	if err != nil || len(out) != 1 || out[0].Actor != "root@pam" {
		t.Fatalf("an admin-only read must answer while TG's identity is unpublished, got %v / %+v", err, out)
	}
	if ex.calls != 0 {
		t.Fatalf("with no TG record in the read the execution record must not be consulted, got %d reads", ex.calls)
	}
}

// Not wired ⇒ the pre-TG-693 behaviour, byte for byte: rows pass through unmarked.
func TestAnUnwiredReaderMarksNothing(t *testing.T) {
	out, err := tg693Read(t, tg693Cluster(""))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range out {
		if e.SelfCorrelated || e.SelfUnexplained {
			t.Fatalf("an unwired reader must not mark any row, got %+v", e)
		}
	}
}
