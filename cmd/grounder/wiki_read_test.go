package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/territory-grounder/grounder/core/wikicompile"
)

// An unset or absent corpus is an HONEST empty lessons section — never an error, never fabricated —
// while the embedded runbooks (compiled into the binary) are always served.
func TestFileWikiEmptyCorpusIsHonest(t *testing.T) {
	for _, fw := range []fileWiki{
		newFileWiki("", "", ""), // no corpus configured
		newFileWiki(filepath.Join(t.TempDir(), "does-not-exist.json"), "", ""), // configured but not yet written
	} {
		idx, err := fw.WikiIndex(context.Background())
		if err != nil {
			t.Fatalf("empty corpus must not error: %v", err)
		}
		if len(idx.Lessons) != 0 || idx.LessonTotal != 0 {
			t.Fatalf("lessons must be empty, got %+v", idx.Lessons)
		}
		if len(idx.Runbooks) < 3 {
			t.Fatalf("the embedded runbook set must serve (>=3 seed pages), got %+v", idx.Runbooks)
		}
	}
}

// A written corpus (the exact shape the worker's lessons loop persists) surfaces as lessons in the
// index and as a lesson page whose body is composed verbatim from the recorded fields.
func TestFileWikiServesCorpusLessons(t *testing.T) {
	p := filepath.Join(t.TempDir(), "corpus.json")
	corpus := `[{"external_ref":"librenms-4821","host":"dc1k8s-w3","alert_rule":"kubelet flap",` +
		`"summary":"kubelet flapped on a worker","resolution":"no action — self-recovered","tags":["k8s"]}]`
	if err := os.WriteFile(p, []byte(corpus), 0o600); err != nil {
		t.Fatal(err)
	}
	fw := newFileWiki(p, "", "")
	idx, err := fw.WikiIndex(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if idx.LessonTotal != 1 || len(idx.Lessons) != 1 || idx.Lessons[0].Slug != "librenms-4821" {
		t.Fatalf("lessons = %+v", idx.Lessons)
	}
	page, ok, err := fw.WikiPage(context.Background(), "librenms-4821")
	if err != nil || !ok {
		t.Fatalf("lesson page: ok=%v err=%v", ok, err)
	}
	if page.Kind != "lesson" || !strings.Contains(page.Body, "self-recovered") || page.Meta["host"] != "dc1k8s-w3" {
		t.Fatalf("lesson page = %+v", page)
	}
	// A malformed corpus is an ERROR (surfaced as 500), never silently rendered as "nothing learned".
	if err := os.WriteFile(p, []byte(`{"not":"an array"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := fw.WikiIndex(context.Background()); err == nil {
		t.Fatal("malformed corpus must error, not serve an empty fabrication")
	}
}

// Every embedded runbook page resolves by slug with a real title from its first heading; an unknown
// slug is found=false.
func TestFileWikiEmbeddedRunbooks(t *testing.T) {
	fw := newFileWiki("", "", "")
	for _, slug := range []string{"triage-protocol", "skill-lifecycle", "grounding-model"} {
		page, ok, err := fw.WikiPage(context.Background(), slug)
		if err != nil || !ok {
			t.Fatalf("%s: ok=%v err=%v", slug, ok, err)
		}
		if page.Kind != "runbook" || page.Title == slug || !strings.HasPrefix(page.Body, "# ") {
			t.Fatalf("%s: page = %+v", slug, page)
		}
	}
	if _, ok, _ := fw.WikiPage(context.Background(), "ghost"); ok {
		t.Fatal("unknown slug must be found=false")
	}
}

// writeArticlesFile is a helper: a compiled envelope on disk, as the worker would leave it.
func writeArticlesFile(t *testing.T, arts []wikicompile.Article, compiledAt time.Time) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "wiki.articles.json")
	f, err := os.Create(p)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer f.Close()
	if err := wikicompile.WriteArticles(f, wikicompile.Envelope{
		SchemaVersion: wikicompile.SchemaVersion, CompiledAt: compiledAt, Articles: arts,
	}); err != nil {
		t.Fatalf("write: %v", err)
	}
	return p
}

// TestLoadArticlesErrorPolicy — the four-way policy, which is the whole honesty contract of this surface.
// "The compiler has not run yet" and "the compiler is broken" must NOT render identically: one is an empty
// section, the other is a 500. Collapsing the second into the first is how a broken lane looks healthy.
//
// RED MUTATION CONTROL (executed 2026-08-01): returning (empty, nil) for the malformed case fails with
// "malformed articles must be an error"; restored green.
func TestLoadArticlesErrorPolicy(t *testing.T) {
	dir := t.TempDir()
	malformed := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(malformed, []byte(`{"schema_version":1,"articles":[{"slug":""}]}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if env, err := (fileWiki{}).loadArticles(); err != nil || len(env.Articles) != 0 {
		t.Errorf("unset path must be an honest empty, got %d articles err=%v", len(env.Articles), err)
	}
	missing := fileWiki{articlesPath: filepath.Join(dir, "never-written.json")}
	if env, err := missing.loadArticles(); err != nil || len(env.Articles) != 0 {
		t.Errorf("a not-yet-compiled file must be an honest empty (the worker has not run), got err=%v", err)
	}
	if _, err := (fileWiki{articlesPath: malformed}).loadArticles(); err == nil {
		t.Error("malformed articles must be an ERROR (500), never an empty section — a broken compiler " +
			"rendering as 'nothing compiled yet' is indistinguishable from a quiet estate")
	}
}

// TestArticleNeverShadowsRunbookOrLesson — article slugs are derived from LIVE DATA (a hostname out of an
// inbound alert payload); runbook slugs are authored at build time. If articles resolved first, an
// attacker-influenced or merely unlucky hostname could replace hand-written operator guidance with a
// generated page, silently.
//
// RED MUTATION CONTROL (executed 2026-08-01): moving the article lookup above the runbook read fails with
// Kind "article" where "runbook" is required; restored green.
func TestArticleNeverShadowsRunbookOrLesson(t *testing.T) {
	// "triage-protocol" is a real embedded runbook (docs/wiki/triage-protocol.md).
	p := writeArticlesFile(t, []wikicompile.Article{
		{Slug: "triage-protocol", Title: "impostor", Kind: "article", Body: "generated"},
		{Slug: "host-dc1mealie01", Title: "dc1mealie01", Kind: "article", Body: "real article"},
	}, time.Date(2026, 8, 1, 6, 0, 0, 0, time.UTC))
	fw := newFileWiki("", "", p)

	page, ok, err := fw.WikiPage(context.Background(), "triage-protocol")
	if err != nil || !ok {
		t.Fatalf("the embedded runbook must resolve: ok=%v err=%v", ok, err)
	}
	if page.Kind != "runbook" {
		t.Fatalf("a compiled article must NEVER shadow an authored runbook, got kind %q body %q",
			page.Kind, page.Body)
	}
	art, ok, err := fw.WikiPage(context.Background(), "host-dc1mealie01")
	if err != nil || !ok {
		t.Fatalf("a compiled article must still resolve on its own slug: ok=%v err=%v", ok, err)
	}
	if art.Kind != "article" || art.Body != "real article" {
		t.Errorf("wrong page served: %+v", art)
	}
	if art.Meta["compiled_at"] == "" {
		t.Error("an article page must carry compiled_at in META so staleness is visible — it is " +
			"deliberately absent from the body, which must stay byte-stable across compiles")
	}
}

// TestWikiIndexArticleTotalBeforeCap — mirrors the LessonTotal contract: a bounded list must never
// misrepresent how much exists.
//
// RED MUTATION CONTROL (executed 2026-08-01): assigning ArticleTotal after the truncation fails with
// total == cap; restored green.
func TestWikiIndexArticleTotalBeforeCap(t *testing.T) {
	arts := make([]wikicompile.Article, maxWikiArticles+7)
	for i := range arts {
		arts[i] = wikicompile.Article{
			Slug: fmt.Sprintf("host-h%05d", i), Title: fmt.Sprintf("h%05d", i), Kind: "article", Body: "b",
		}
	}
	fw := newFileWiki("", "", writeArticlesFile(t, arts, time.Now()))
	idx, err := fw.WikiIndex(context.Background())
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	if idx.ArticleTotal != maxWikiArticles+7 {
		t.Errorf("ArticleTotal must be the TRUE compiled count %d, got %d", maxWikiArticles+7, idx.ArticleTotal)
	}
	if len(idx.Articles) != maxWikiArticles {
		t.Errorf("the rendered list must be capped at %d, got %d", maxWikiArticles, len(idx.Articles))
	}
}

// TestDecisionRecordsAreServed — the 17 ADRs in docs/adr were written, versioned with the code, and
// reachable by nobody using the product. #wiki showed THREE pages.
//
// They are folded into the runbooks section rather than given their own: to an operator both are the
// same kind of thing — authored, versioned with the code, true regardless of what the estate is doing
// today. The distinction that matters is between these and the COMPILED articles (derived from the
// spine), not between a runbook and a decision record.
//
// RED MUTATION CONTROL (executed 2026-08-01): dropping the adrs source from the index loop fails with
// the runbook count back at 3; restored green.
func TestDecisionRecordsAreServed(t *testing.T) {
	fw := newFileWiki("", "", "")
	idx, err := fw.WikiIndex(context.Background())
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	if len(idx.Runbooks) < 18 {
		t.Fatalf("the wiki serves %d authored pages; docs/wiki has 3 runbooks and docs/adr has 17 decision "+
			"records, so anything under 18 means a whole embedded set is unreachable", len(idx.Runbooks))
	}
	// A specific ADR must resolve as a page, with its real title.
	var adrSlug string
	for _, d := range idx.Runbooks {
		if strings.HasPrefix(d.Slug, "0001-") {
			adrSlug = d.Slug
		}
	}
	if adrSlug == "" {
		t.Fatal("no ADR slug in the index — the decision records are indexed but not identifiable")
	}
	page, ok, err := fw.WikiPage(context.Background(), adrSlug)
	if err != nil || !ok {
		t.Fatalf("ADR %s must resolve: ok=%v err=%v", adrSlug, ok, err)
	}
	if page.Kind != "adr" {
		t.Errorf("a decision record must be labelled kind=adr so the surface can distinguish an authored "+
			"decision from a runbook, got %q", page.Kind)
	}
	if strings.TrimSpace(page.Body) == "" || page.Title == page.Slug {
		t.Errorf("the ADR page must carry its real body and its `# ` title, got title=%q body=%d bytes",
			page.Title, len(page.Body))
	}
}

// TestEmbeddedSlugCollisionIsRefusedNotTieBroken — two build-time sets now feed one section. A collision
// would make one page permanently unreachable, and silently: the index would show one title and the page
// fetch would serve the other set's body.
//
// Both sets are build-time, so a collision is a REPO defect a human can fix in a rename. Picking a winner
// would bury it, which is why the reader errors instead — the 500 is the surface saying "this is broken",
// which is the honest answer and the one that gets it fixed.
func TestEmbeddedSlugCollisionIsRefusedNotTieBroken(t *testing.T) {
	fw := fileWiki{runbooks: fstest.MapFS{"dup.md": {Data: []byte("# from runbooks")}},
		adrs: fstest.MapFS{"dup.md": {Data: []byte("# from adrs")}}}
	_, err := fw.WikiIndex(context.Background())
	if err == nil {
		t.Fatal("a slug present in BOTH embedded sets must be refused — otherwise one page is unreachable " +
			"and the index and the page fetch disagree about which")
	}
	if !strings.Contains(err.Error(), "unreachable") {
		t.Errorf("the error must say what is at stake, got %v", err)
	}
}
