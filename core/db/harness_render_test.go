package db

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/harnessid"
	"github.com/territory-grounder/grounder/core/judge"
)

func tg600Parts(guidance string) harnessid.Parts {
	return harnessid.Parts{
		Prose:          "PROTOCOL\n" + harnessid.ToolCatalogPlaceholder + "\n" + harnessid.OpClassCatalogPlaceholder + "\n",
		ToolCatalog:    "- read_file: reads a file",
		OpClassCatalog: "- restart-service: params.unit",
		Guidance:       guidance,
	}
}

// TestHarnessRenderStoreRoundTrip drives the REAL pgx path (TG-600, migration 0117, spec/020 REQ-2025):
// the first Ensure writes the rendering and its build row; a second Ensure from a fresh store (no
// in-process cache) writes nothing; the same configuration under another build adds ONE lineage row; a
// record keyed by another configuration's fingerprint is REFUSED before any SQL; List counts the sessions
// that carry the fingerprint; Get by prefix returns parts that reassemble to the exact bytes the key
// hashes. Gated on TG_TEST_POSTGRES_DSN (CI has no Postgres).
func TestHarnessRenderStoreRoundTrip(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to a migrated database to run the harness render round-trip test")
	}
	ctx := context.Background()
	if err := Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer p.Close()

	uniq := fmt.Sprintf("tg600-%d", os.Getpid())
	parts := tg600Parts("## Guidance " + uniq)
	rec := harnessid.Record{HarnessFP: harnessid.Fingerprint(parts, "preamble/test"), PromptVersion: "preamble/test", BuildSHA: "build-a-" + uniq, Parts: parts}
	sessRef := uniq + "-session"
	defer func() {
		_, _ = p.Exec(ctx, "DELETE FROM session_triage WHERE external_ref = $1", sessRef)
		_, _ = p.Exec(ctx, "DELETE FROM harness_render_build WHERE harness_fp = $1", rec.HarnessFP)
		_, _ = p.Exec(ctx, "DELETE FROM harness_render WHERE harness_fp = $1", rec.HarnessFP)
	}()

	first, err := NewHarnessRenderStore(p).Ensure(ctx, rec)
	if err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	if !first.RenderInserted || !first.BuildInserted || first.Cached {
		t.Fatalf("the first session under a configuration must write the rendering AND its build row, got %+v", first)
	}
	again, err := NewHarnessRenderStore(p).Ensure(ctx, rec) // a fresh store: the DB, not the cache, must de-duplicate
	if err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	if again.RenderInserted || again.BuildInserted || again.Cached {
		t.Fatalf("a recorded configuration must write nothing on a later session, got %+v", again)
	}
	s := NewHarnessRenderStore(p)
	if out, err := s.Ensure(ctx, rec); err != nil || out.Cached || out.RenderInserted {
		t.Fatalf("a store's first call hits the DB (not cached), writes nothing: %+v %v", out, err)
	}
	if out, err := s.Ensure(ctx, rec); err != nil || !out.Cached {
		t.Fatalf("a store's second call for the same (fingerprint, build) is a cache hit: %+v %v", out, err)
	}
	otherBuild := rec
	otherBuild.BuildSHA = "build-b-" + uniq
	lineage, err := s.Ensure(ctx, otherBuild)
	if err != nil {
		t.Fatalf("other build: %v", err)
	}
	if lineage.RenderInserted || !lineage.BuildInserted {
		t.Fatalf("the same configuration under a new build adds ONE lineage row and no rendering, got %+v", lineage)
	}

	// Integrity: a record keyed by ANOTHER configuration's fingerprint is refused, and nothing lands.
	forged := rec
	forged.Parts.Guidance = "## a different guidance half"
	if _, err := s.Ensure(ctx, forged); err == nil || !strings.Contains(err.Error(), "does not derive") {
		t.Fatalf("a rendering that does not hash to its key must be refused, got %v", err)
	}
	var stored string
	if err := p.QueryRow(ctx, "SELECT guidance FROM harness_render WHERE harness_fp = $1", rec.HarnessFP).Scan(&stored); err != nil || stored != parts.Guidance {
		t.Fatalf("the refused record must not have overwritten the stored rendering: %q %v", stored, err)
	}

	// A session carrying the fingerprint is counted; Get by prefix returns parts that reassemble to the key's bytes.
	if err := NewTriageStore(p).RecordTriage(ctx, judge.TriageRow{ExternalRef: sessRef, Host: "web01", AlertRule: "HostDown", Outcome: "proposal", HarnessFP: rec.HarnessFP, PromptVersion: "preamble/test"}); err != nil {
		t.Fatalf("record triage: %v", err)
	}
	list, err := s.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var mine *HarnessRenderSummary
	for i := range list {
		if list[i].HarnessFP == rec.HarnessFP {
			mine = &list[i]
		}
	}
	if mine == nil {
		t.Fatal("the recorded configuration must appear in the list")
	}
	if mine.Sessions != 1 || len(mine.Builds) != 2 || mine.Builds[0] != rec.BuildSHA || mine.Builds[1] != otherBuild.BuildSHA || mine.Bytes != parts.Bytes() || mine.PromptVersion != "preamble/test" {
		t.Fatalf("list row must carry sessions=1, both builds oldest-first, the assembled byte size and the version, got %+v", *mine)
	}
	row, err := s.Get(ctx, rec.HarnessFP[:16])
	if err != nil {
		t.Fatalf("get by prefix: %v", err)
	}
	if harnessid.Assemble(row.Parts) != harnessid.Assemble(parts) || harnessid.Fingerprint(row.Parts, row.PromptVersion) != rec.HarnessFP {
		t.Fatal("the stored parts must reassemble to the exact system half the fingerprint hashes")
	}
	if row.Hashes != parts.Hashes() || row.Sessions != 1 || len(row.Builds) != 2 {
		t.Fatalf("get must carry the component hashes, the session count and the lineage, got %+v", row)
	}
	if _, err := s.Get(ctx, "abc"); err == nil {
		t.Fatal("a prefix shorter than 4 hex must be refused")
	}
	if _, err := s.Get(ctx, "ZZZZ"); err == nil {
		t.Fatal("a non-hex prefix must be refused, never passed to the query")
	}
	if _, err := s.Get(ctx, "0000000000000000000000000000000000000000000000000000000000000000"); !errors.Is(err, ErrHarnessRenderNotFound) {
		t.Fatalf("an unknown fingerprint must read not-found, got %v", err)
	}
}

// The in-memory twin keeps the SAME law as the pgx store: the key check runs first, insert-if-absent,
// one lineage row per (configuration, build). It is what the spec/020 acceptance scenario drives.
func TestMemHarnessRenderStoreMirrorsTheLaw(t *testing.T) {
	ctx := context.Background()
	m := NewMemHarnessRenderStore()
	parts := tg600Parts("## Guidance A")
	rec := harnessid.Record{HarnessFP: harnessid.Fingerprint(parts, "preamble/5"), PromptVersion: "preamble/5", BuildSHA: "b1", Parts: parts}
	if out, err := m.Ensure(ctx, rec); err != nil || !out.RenderInserted || !out.BuildInserted {
		t.Fatalf("first ensure: %+v %v", out, err)
	}
	if out, err := m.Ensure(ctx, rec); err != nil || out.RenderInserted || out.BuildInserted {
		t.Fatalf("second ensure must write nothing: %+v %v", out, err)
	}
	b2 := rec
	b2.BuildSHA = "b2"
	if out, err := m.Ensure(ctx, b2); err != nil || out.RenderInserted || !out.BuildInserted {
		t.Fatalf("a new build adds one lineage row: %+v %v", out, err)
	}
	forged := rec
	forged.Parts.Guidance = "## Guidance B"
	if _, err := m.Ensure(ctx, forged); err == nil {
		t.Fatal("the twin must refuse a key that does not derive from the parts")
	}
	got, builds, ok := m.Get(rec.HarnessFP)
	if !ok || got.Parts != parts || len(builds) != 2 || m.Len() != 1 {
		t.Fatalf("twin state: ok=%v builds=%v len=%d", ok, builds, m.Len())
	}
}
