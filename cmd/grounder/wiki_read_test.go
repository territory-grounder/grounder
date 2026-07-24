package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An unset or absent corpus is an HONEST empty lessons section — never an error, never fabricated —
// while the embedded runbooks (compiled into the binary) are always served.
func TestFileWikiEmptyCorpusIsHonest(t *testing.T) {
	for _, fw := range []fileWiki{
		newFileWiki("", ""), // no corpus configured
		newFileWiki(filepath.Join(t.TempDir(), "does-not-exist.json"), ""), // configured but not yet written
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
	fw := newFileWiki(p, "")
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
	fw := newFileWiki("", "")
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
