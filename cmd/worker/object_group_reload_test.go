package main

import (
	"context"
	"errors"
	"testing"

	"github.com/territory-grounder/grounder/core/credential"
	"github.com/territory-grounder/grounder/core/db"
)

type fakeOGLister struct {
	rows []db.EstateObjectGroupRow
	err  error
}

func (f fakeOGLister) List(context.Context) ([]db.EstateObjectGroupRow, error) { return f.rows, f.err }

type spyOGSink struct {
	got    []credential.ObjectGroup
	setCnt int
}

func (s *spyOGSink) SetObjectGroups(g []credential.ObjectGroup) { s.got = g; s.setCnt++ }

// A successful load converts every row to a credential.ObjectGroup (name + host-glob patterns) and hands the
// WHOLE set to the resolver in one replace.
func TestLoadObjectGroupsInto_ConvertsAndSets(t *testing.T) {
	lister := fakeOGLister{rows: []db.EstateObjectGroupRow{
		{ID: 1, Name: "webservers", Patterns: []string{"dc1demo-web*"}, Precedence: "union"},
		{ID: 2, Name: "edge-fw", Patterns: []string{"dc1demo-fw01", "dc1demo-fw02"}, Precedence: "union"},
	}}
	sink := &spyOGSink{}
	n, err := loadObjectGroupsInto(context.Background(), lister, sink)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if n != 2 || sink.setCnt != 1 || len(sink.got) != 2 {
		t.Fatalf("expected 2 groups set once, got n=%d setCnt=%d len=%d", n, sink.setCnt, len(sink.got))
	}
	if sink.got[0].Name != "webservers" || len(sink.got[0].Patterns) != 1 || sink.got[0].Patterns[0] != "dc1demo-web*" {
		t.Errorf("group0 mis-converted: %+v", sink.got[0])
	}
	if sink.got[1].Name != "edge-fw" || len(sink.got[1].Patterns) != 2 {
		t.Errorf("group1 mis-converted: %+v", sink.got[1])
	}
}

// An empty store still SETS an empty set (a replace, not a skip) — a deleted-down-to-zero store disarms the
// seam, so resolution runs with no groups exactly as before TG-481.
func TestLoadObjectGroupsInto_EmptyDisarms(t *testing.T) {
	sink := &spyOGSink{}
	n, err := loadObjectGroupsInto(context.Background(), fakeOGLister{}, sink)
	if err != nil || n != 0 {
		t.Fatalf("empty: n=%d err=%v", n, err)
	}
	if sink.setCnt != 1 || len(sink.got) != 0 {
		t.Errorf("empty store must still SET an empty set (replace), got setCnt=%d len=%d", sink.setCnt, len(sink.got))
	}
}

// A read failure returns the error and NEVER touches the resolver — the caller keeps the last good set, so a
// transient DB blip cannot silently disarm live resolution.
func TestLoadObjectGroupsInto_ReadErrLeavesResolverUntouched(t *testing.T) {
	sink := &spyOGSink{}
	_, err := loadObjectGroupsInto(context.Background(), fakeOGLister{err: errors.New("boom")}, sink)
	if err == nil {
		t.Fatal("expected the read error to propagate")
	}
	if sink.setCnt != 0 {
		t.Errorf("a failed read must not touch the resolver, setCnt=%d", sink.setCnt)
	}
}

// A non-positive interval disables the poll and never spawns the goroutine (the boot load still applies).
func TestStartObjectGroupRefresh_DisabledInterval(t *testing.T) {
	sink := &spyOGSink{}
	startObjectGroupRefresh(fakeOGLister{}, sink, 0, make(chan interface{}))
	if sink.setCnt != 0 {
		t.Errorf("a disabled refresh must not load, setCnt=%d", sink.setCnt)
	}
}
