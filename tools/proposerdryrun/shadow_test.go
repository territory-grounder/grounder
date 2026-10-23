package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/skillstore"
)

// fakeLive is the live store the shadow delegates reads to; it records every call so a test can prove no write
// reached it (it has no write methods at all — the compiler is the first oracle, this file the second).
type fakeLive struct {
	calls []string
	prod  skillstore.Version
}

func (f *fakeLive) GetVersion(_ context.Context, id int64) (skillstore.Version, error) {
	f.calls = append(f.calls, "GetVersion")
	if id == f.prod.ID {
		return f.prod, nil
	}
	return skillstore.Version{}, errors.New("no such live row")
}

func (f *fakeLive) GetSkill(_ context.Context, name string) (skillstore.Skill, error) {
	f.calls = append(f.calls, "GetSkill")
	return skillstore.Skill{Name: name, Kind: "procedure"}, nil
}

func (f *fakeLive) ProductionVersion(_ context.Context, _ string) (skillstore.Version, bool, error) {
	f.calls = append(f.calls, "ProductionVersion")
	return f.prod, true, nil
}

// draft is what a proposer hands CreateVersion: body, rationale and the content hash it computed. The skill name
// is NOT a compiled one, so the selector law (TG-588) does not couple this test to the compiled registry.
func draft(body string) skillstore.Version {
	aw := skillstore.AppliesWhen{}
	return skillstore.Version{SkillName: "dryrun-rehearsal-skill", Version: "1.0.0-dryrun", Body: body, Author: "test", Source: "test",
		Rationale: "[draft] rehearsal", AppliesWhen: aw, ContentHash: skillstore.ContentHash(body, aw)}
}

// A created draft lives under a negative id, carries draft status, timestamps and the content hash the chain
// would record — and the live store saw only reads.
func TestShadowCreateAssignsNegativeIDsAndNeverWritesLive(t *testing.T) {
	live := &fakeLive{prod: skillstore.Version{ID: 42, SkillName: "dryrun-rehearsal-skill", Status: skillstore.StatusProduction, Body: "prod"}}
	s := newShadowStore(live)
	s.now = func() time.Time { return time.Date(2026, 10, 23, 5, 0, 0, 0, time.UTC) }
	ctx := context.Background()

	a, err := s.CreateVersion(ctx, draft("# a body of procedure"))
	if err != nil {
		t.Fatalf("create a: %v", err)
	}
	b, err := s.CreateVersion(ctx, draft("# another body"))
	if err != nil {
		t.Fatalf("create b: %v", err)
	}
	if a.ID != -1 || b.ID != -2 {
		t.Fatalf("ids = %d, %d; want -1, -2", a.ID, b.ID)
	}
	if a.Status != skillstore.StatusDraft || a.CreatedAt.IsZero() || a.StatusChangedAt != a.CreatedAt {
		t.Fatalf("draft stamping wrong: %+v", a)
	}
	if a.ContentHash != skillstore.ContentHash(a.Body, a.AppliesWhen) {
		t.Fatal("content hash must be the caller's chain hash of body+applies_when")
	}
	bad := draft("# body whose hash lies")
	bad.ContentHash = "deadbeef"
	if _, err := s.CreateVersion(ctx, bad); err == nil || !strings.Contains(err.Error(), "content hash mismatch") {
		t.Fatalf("a wrong content hash must be refused like the database refuses it, got %v", err)
	}
	got, err := s.GetVersion(ctx, -2)
	if err != nil || got.Body != "# another body" {
		t.Fatalf("GetVersion(-2) = %+v, %v", got, err)
	}
	if _, err := s.GetVersion(ctx, 42); err != nil {
		t.Fatalf("a live id must still read through: %v", err)
	}
	for _, c := range live.calls {
		if strings.HasPrefix(c, "Create") || strings.HasPrefix(c, "Update") {
			t.Fatalf("the live store saw a write: %v", live.calls)
		}
	}
	if n := len(s.Shadow()); n != 2 {
		t.Fatalf("shadow rows = %d, want 2", n)
	}
	if w := s.Writes(); len(w) != 2 || !strings.Contains(w[0], "shadow id -1") {
		t.Fatalf("writes audit = %v", w)
	}
}

// The real CreateVersion validates before it inserts; the shadow must refuse the same drafts (here: no rationale).
func TestShadowCreateRunsTheRealDraftValidation(t *testing.T) {
	s := newShadowStore(&fakeLive{prod: skillstore.Version{ID: 1, SkillName: "dryrun-rehearsal-skill", Status: skillstore.StatusProduction}})
	v := draft("# body")
	v.Rationale = ""
	if _, err := s.CreateVersion(context.Background(), v); !errors.Is(err, skillstore.ErrRationaleRequired) {
		t.Fatalf("a rationale-less draft must be refused like the database refuses it, got %v", err)
	}
	if len(s.Shadow()) != 0 {
		t.Fatal("a refused draft must not be stored")
	}
}

// Transition on a shadow row updates the shadow; the SAME call aimed at a live id is refused, never forwarded.
func TestShadowUpdateMovesOwnRowsAndRefusesLiveOnes(t *testing.T) {
	live := &fakeLive{prod: skillstore.Version{ID: 7, SkillName: "dryrun-rehearsal-skill", Status: skillstore.StatusProduction, Body: "prod", ContentHash: skillstore.ContentHash("prod", skillstore.AppliesWhen{})}}
	s := newShadowStore(live)
	lg := &memLedger{}
	ctx := context.Background()
	d, err := s.CreateVersion(ctx, draft("# proposal"))
	if err != nil {
		t.Fatal(err)
	}
	moved, err := skillstore.Transition(ctx, s, lg, d.ID, skillstore.StatusRejected, "composer: withdrawn in favour of the composition")
	if err != nil {
		t.Fatalf("transition of a shadow row must succeed: %v", err)
	}
	if moved.Status != skillstore.StatusRejected {
		t.Fatalf("status = %s, want rejected", moved.Status)
	}
	if e := lg.Entries(); len(e) != 1 || e[0].Seq != 1 {
		t.Fatalf("ledger entries = %+v, want one with seq 1", e)
	}
	if _, err := skillstore.Transition(ctx, s, lg, 7, skillstore.StatusRetired, "a write aimed at production"); !errors.Is(err, errRealWrite) {
		t.Fatalf("a transition of a LIVE row must be refused with errRealWrite, got %v", err)
	}
	if live.prod.Status != skillstore.StatusProduction {
		t.Fatal("the live row must be untouched")
	}
}

func TestMemLedgerSequences(t *testing.T) {
	lg := &memLedger{}
	for i := 1; i <= 3; i++ {
		e, err := lg.Append(audit.GovDecision{Decision: "skill:transition", Reason: "r", ActionID: "a"})
		if err != nil || e.Seq != int64(i) {
			t.Fatalf("append %d: %+v, %v", i, e, err)
		}
	}
}

func TestDescribeComposition(t *testing.T) {
	drafts := []skillstore.Version{{ID: -1, Version: "1.0.0-inv"}, {ID: -2, Version: "1.0.0-dom"}}
	cases := []struct {
		composed skillstore.Version
		kept     bool
		want     string
	}{
		{skillstore.Version{}, true, "KEEP_BASE"},
		{skillstore.Version{}, false, "no composition"},
		{skillstore.Version{ID: -3, Version: "1.0.0-composed-abcdef12"}, false, "MERGED"},
		{skillstore.Version{ID: -2, Version: "1.0.0-dom"}, false, "ADOPT-ONE"},
	}
	for _, c := range cases {
		if got := describeComposition(c.composed, c.kept, drafts); !strings.HasPrefix(got, c.want) {
			t.Errorf("describe(%+v, %v) = %q, want prefix %q", c.composed, c.kept, got, c.want)
		}
	}
}

// The connection is read-only by construction, whatever the DSN's role; an operator's explicit options are kept.
func TestReadOnlyDSNAddsTheServerSideGuard(t *testing.T) {
	cases := []struct {
		in, wantSub string
		guarded     bool
	}{
		{"postgres://u:p@h:5432/db?sslmode=disable", "options=-c+default_transaction_read_only%3Don", true},
		{"postgres://u:p@h:5432/db", "options=-c+default_transaction_read_only%3Don", true},
		{"postgres://u:p@h/db?options=-c%20statement_timeout%3D5s", "options=-c%20statement_timeout%3D5s", false}, // operator's own options win — and the caller is told
		{"host=h dbname=db user=u", " options='-c default_transaction_read_only=on'", true},
		{"host=h options='-c x=y'", "options='-c x=y'", false},
	}
	for _, c := range cases {
		got, guarded, err := readOnlyDSN(c.in)
		if err != nil {
			t.Fatalf("%s: %v", c.in, err)
		}
		if !strings.Contains(got, c.wantSub) {
			t.Errorf("readOnlyDSN(%q) = %q, want it to contain %q", c.in, got, c.wantSub)
		}
		if guarded != c.guarded {
			t.Errorf("readOnlyDSN(%q) guarded = %v, want %v (the run must say when the server guard is absent)", c.in, guarded, c.guarded)
		}
		if strings.Count(got, "default_transaction_read_only") > 1 {
			t.Errorf("readOnlyDSN(%q) = %q adds the guard twice", c.in, got)
		}
	}
	if _, _, err := readOnlyDSN("postgres://u:p@h:bad port/db"); err == nil {
		t.Fatal("an unparseable URL DSN must be refused, not guessed")
	}
}
