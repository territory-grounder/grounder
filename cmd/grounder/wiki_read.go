package main

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"

	"github.com/territory-grounder/grounder/core/httpapi"
	"github.com/territory-grounder/grounder/core/knowledge"
	docswiki "github.com/territory-grounder/grounder/docs/wiki"
)

// fileWiki is the production httpapi.WikiReader (REQ-521): lessons come from the SAME knowledge
// corpus the worker maintains (TG_KNOWLEDGE_FILE — the distilled confirmed-clean resolved incidents
// the retriever reloads), read per request so a lessons-loop merge is visible without a restart;
// runbooks come from the docs/wiki pages embedded in the binary at build time (the deployed grounder
// is a static image with no docs/ tree on disk). An absent corpus file — or an unset TG_KNOWLEDGE_FILE
// — is an HONEST empty lessons section; a present-but-malformed corpus is an error (surfaced as 500),
// never silently shown as "nothing learned".
type fileWiki struct {
	corpusPath string // "" = no maintained corpus configured
	seedPath   string // "" = no read-only seed corpus — the lessons section is honestly empty when both are unset
	runbooks   fs.FS  // the embedded docs/wiki pages
}

func newFileWiki(corpusPath, seedPath string) fileWiki {
	return fileWiki{corpusPath: corpusPath, seedPath: seedPath, runbooks: docswiki.FS}
}

// maxWikiLessons bounds the index lessons list; LessonTotal always carries the true corpus size.
const maxWikiLessons = 1000

// loadCorpus reads the lessons corpus as the seed ∪ maintained UNION: the read-only bootstrap seed
// (tracked, deploy-synced) plus the maintained corpus the worker writes (untracked, deploy-persistent).
// (nil, nil) when neither is configured or neither file exists yet — the empty state, not an error.
// A malformed MAINTAINED corpus is an error (surfaced as 500, never silently shown as "nothing
// learned"); a seed problem degrades to maintained-only — the worker already logs seed failures loudly
// at its own load, and a bootstrap gap must never 500 the wiki.
func (fw fileWiki) loadCorpus() ([]knowledge.Incident, error) {
	var seed []knowledge.Incident
	if fw.seedPath != "" {
		if sf, serr := os.Open(fw.seedPath); serr == nil {
			seed, _ = knowledge.ParseCorpus(sf)
			sf.Close()
		}
	}
	if fw.corpusPath == "" {
		return knowledge.MergeCorpus(seed, nil), nil
	}
	f, err := os.Open(fw.corpusPath)
	if os.IsNotExist(err) {
		return knowledge.MergeCorpus(seed, nil), nil // the worker has not distilled a lesson yet — honestly empty
	}
	if err != nil {
		return nil, fmt.Errorf("wiki: corpus %s: %w", fw.corpusPath, err)
	}
	defer f.Close()
	maintained, perr := knowledge.ParseCorpus(f)
	if perr != nil {
		return nil, perr
	}
	return knowledge.MergeCorpus(seed, maintained), nil
}

// docTitle extracts a page's title: the first `# ` heading, else the slug itself.
func docTitle(slug, body string) string {
	for _, ln := range strings.Split(body, "\n") {
		if t := strings.TrimSpace(strings.TrimPrefix(ln, "# ")); strings.HasPrefix(ln, "# ") && t != "" {
			return t
		}
	}
	return slug
}

// lessonTitle names a lesson page from its recorded fields (never invented).
func lessonTitle(inc knowledge.Incident) string {
	switch {
	case inc.AlertRule != "" && inc.Host != "":
		return inc.AlertRule + " on " + inc.Host
	case inc.AlertRule != "":
		return inc.AlertRule
	case inc.Host != "":
		return inc.Host
	default:
		return inc.ExternalRef
	}
}

func (fw fileWiki) WikiIndex(_ context.Context) (httpapi.WikiIndex, error) {
	idx := httpapi.WikiIndex{}

	corpus, err := fw.loadCorpus()
	if err != nil {
		return httpapi.WikiIndex{}, err
	}
	idx.LessonTotal = len(corpus)
	if len(corpus) > maxWikiLessons {
		corpus = corpus[:maxWikiLessons] // the corpus is deterministically ordered by external_ref
	}
	for _, inc := range corpus {
		idx.Lessons = append(idx.Lessons, httpapi.WikiLesson{
			Slug: inc.ExternalRef, ExternalRef: inc.ExternalRef,
			Host: inc.Host, AlertRule: inc.AlertRule, Site: inc.Site,
			Summary: inc.Summary, Resolution: inc.Resolution, Tags: inc.Tags,
		})
	}

	entries, err := fs.ReadDir(fw.runbooks, ".")
	if err != nil {
		return httpapi.WikiIndex{}, fmt.Errorf("wiki: embedded runbooks: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		body, rerr := fs.ReadFile(fw.runbooks, e.Name())
		if rerr != nil {
			return httpapi.WikiIndex{}, fmt.Errorf("wiki: embedded runbook %s: %w", e.Name(), rerr)
		}
		slug := strings.TrimSuffix(e.Name(), ".md")
		idx.Runbooks = append(idx.Runbooks, httpapi.WikiDoc{Slug: slug, Title: docTitle(slug, string(body))})
	}
	sort.Slice(idx.Runbooks, func(i, j int) bool { return idx.Runbooks[i].Slug < idx.Runbooks[j].Slug })
	return idx, nil
}

func (fw fileWiki) WikiPage(_ context.Context, slug string) (httpapi.WikiPage, bool, error) {
	// Embedded runbooks resolve first (their slugs are fixed at build time and can never collide with
	// an operator's external_ref namespace by accident without being visible in review).
	if body, err := fs.ReadFile(fw.runbooks, slug+".md"); err == nil {
		return httpapi.WikiPage{
			Slug: slug, Title: docTitle(slug, string(body)), Kind: "runbook", Body: string(body),
		}, true, nil
	}

	corpus, err := fw.loadCorpus()
	if err != nil {
		return httpapi.WikiPage{}, false, err
	}
	for _, inc := range corpus {
		if inc.ExternalRef != slug {
			continue
		}
		// The lesson page body is composed VERBATIM from the recorded fields — markdown framing only.
		var b strings.Builder
		if inc.Summary != "" {
			b.WriteString("## What happened\n\n" + inc.Summary + "\n\n")
		}
		if inc.Resolution != "" {
			b.WriteString("## What resolved it\n\n" + inc.Resolution + "\n\n")
		}
		b.WriteString("*Distilled from a confirmed-clean resolution (mechanical verdict `match` + " +
			"confirmed clear) — the only outcomes that become citable precedent.*\n")
		meta := map[string]string{}
		if inc.Host != "" {
			meta["host"] = inc.Host
		}
		if inc.AlertRule != "" {
			meta["alert_rule"] = inc.AlertRule
		}
		if inc.Site != "" {
			meta["site"] = inc.Site
		}
		if len(inc.Tags) > 0 {
			meta["tags"] = strings.Join(inc.Tags, ", ")
		}
		return httpapi.WikiPage{
			Slug: slug, Title: lessonTitle(inc), Kind: "lesson", Body: b.String(), Meta: meta,
		}, true, nil
	}
	return httpapi.WikiPage{}, false, nil
}
