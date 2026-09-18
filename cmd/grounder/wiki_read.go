package main

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/territory-grounder/grounder/core/httpapi"
	"github.com/territory-grounder/grounder/core/knowledge"
	"github.com/territory-grounder/grounder/core/wikicompile"
	docsroot "github.com/territory-grounder/grounder/docs"
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
	// articlesPath is the COMPILED per-host wiki the worker's wikicompile lane writes. Unset, or not yet
	// written, is an honestly empty articles section — the same rule the corpus follows, and for the same
	// reason: "the compiler has not run" and "the compiler is broken" must not render identically.
	articlesPath string
	runbooks     fs.FS // the embedded docs/wiki pages
	// adrs are the embedded architecture decision records (docs/adr). They are authored, build-time
	// documents like the runbooks — NOT derived from the spine and not distilled from a resolution — so
	// they belong in the same section rather than beside the compiled articles. Their own package exists
	// because //go:embed cannot escape its directory.
	adrs fs.FS
}

func newFileWiki(corpusPath, seedPath, articlesPath string) fileWiki {
	// adrSub roots the embedded records at docs/adr so the reader's ReadDir(".")/ReadFile("<slug>.md")
	// work identically for both embedded sets. The embed itself lives at docs/ (see docs/adrembed.go:
	// docs/adr is a law surface and build plumbing does not belong behind an owner approval), so without
	// this the entries would arrive as "adr/0001-….md" and every slug would carry a directory prefix.
	adrs, err := fs.Sub(docsroot.ADRs, "adr")
	if err != nil {
		// Cannot happen with a compile-time embed of a real directory; if it ever does, an empty FS is the
		// honest degradation — the runbooks still serve and the decision records are simply absent, rather
		// than the whole wiki failing. The ZERO embed.FS is a valid empty filesystem, so this needs no
		// testing package in the production binary.
		var empty embed.FS
		adrs = empty
	}
	return fileWiki{corpusPath: corpusPath, seedPath: seedPath, articlesPath: articlesPath, runbooks: docswiki.FS, adrs: adrs}
}

// maxWikiArticles bounds the index articles list; ArticleTotal always carries the true compiled count, so
// a bounded list never misrepresents how much exists (the LessonTotal contract, applied to articles).
const maxWikiArticles = 1000

// loadArticles reads the compiled envelope, reproducing loadCorpus's error policy exactly:
//
//	unset          -> empty, nil  (the lane is not configured; the seam reports that separately)
//	not yet written-> empty, nil  (the worker has not compiled yet — honestly empty, never an error)
//	unreadable     -> error       (a present file we cannot read is a fault, not an absence)
//	malformed      -> error       (surfaced as 500: "the compiler is broken" must not render as "nothing
//	                               compiled yet", which is the difference between a bug and a quiet estate)
func (fw fileWiki) loadArticles() (wikicompile.Envelope, error) {
	if fw.articlesPath == "" {
		return wikicompile.Envelope{Articles: []wikicompile.Article{}}, nil
	}
	f, err := os.Open(fw.articlesPath)
	if err != nil {
		if os.IsNotExist(err) {
			return wikicompile.Envelope{Articles: []wikicompile.Article{}}, nil
		}
		return wikicompile.Envelope{}, fmt.Errorf("wiki: open compiled articles %s: %w", fw.articlesPath, err)
	}
	defer f.Close()
	env, perr := wikicompile.ParseArticles(f)
	if perr != nil {
		return wikicompile.Envelope{}, fmt.Errorf("wiki: compiled articles %s: %w", fw.articlesPath, perr)
	}
	return env, nil
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

	env, aerr := fw.loadArticles()
	if aerr != nil {
		return httpapi.WikiIndex{}, aerr
	}
	idx.ArticleTotal = len(env.Articles)
	arts := env.Articles
	if len(arts) > maxWikiArticles {
		arts = arts[:maxWikiArticles] // deterministically slug-ordered, so the cut is stable
	}
	for _, a := range arts {
		idx.Articles = append(idx.Articles, httpapi.WikiDoc{Slug: a.Slug, Title: a.Title})
	}
	if !env.CompiledAt.IsZero() {
		at := env.CompiledAt.UTC()
		idx.ArticlesCompiledAt = &at
	}

	// Both embedded sets feed the runbooks section: they are the same KIND of thing to an operator —
	// authored, versioned with the code, true regardless of what the estate is doing today. The
	// distinction that matters (derived vs authored) is between these and the compiled articles, not
	// between a runbook and a decision record.
	//
	// A slug collision across the two would silently hide one page, so it is refused rather than
	// tie-broken: both sets are build-time, so a collision is a REPO defect a human can fix, and picking
	// a winner would bury it.
	seen := map[string]string{}
	for _, src := range []struct {
		fsys fs.FS
		what string
	}{{fw.runbooks, "runbook"}, {fw.adrs, "decision record"}} {
		entries, err := fs.ReadDir(src.fsys, ".")
		if err != nil {
			return httpapi.WikiIndex{}, fmt.Errorf("wiki: embedded %ss: %w", src.what, err)
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			body, rerr := fs.ReadFile(src.fsys, e.Name())
			if rerr != nil {
				return httpapi.WikiIndex{}, fmt.Errorf("wiki: embedded %s %s: %w", src.what, e.Name(), rerr)
			}
			slug := strings.TrimSuffix(e.Name(), ".md")
			if prev, dup := seen[slug]; dup {
				return httpapi.WikiIndex{}, fmt.Errorf(
					"wiki: slug %q is both a %s and a %s — one of them would be unreachable; rename one", slug, prev, src.what)
			}
			seen[slug] = src.what
			idx.Runbooks = append(idx.Runbooks, httpapi.WikiDoc{Slug: slug, Title: docTitle(slug, string(body))})
		}
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
	// Decision records resolve here too — build-time slugs, ahead of anything derived from live data, for
	// the same reason: an inbound hostname must never be able to shadow an authored document.
	if body, err := fs.ReadFile(fw.adrs, slug+".md"); err == nil {
		return httpapi.WikiPage{
			Slug: slug, Title: docTitle(slug, string(body)), Kind: "adr", Body: string(body),
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

	// COMPILED ARTICLES RESOLVE LAST, and that order is deliberate. Runbook and lesson slugs are fixed at
	// build time or by an operator's external_ref; ARTICLE slugs are derived from LIVE DATA — a hostname
	// out of an inbound alert payload. Resolving them first would let an incoming alert name a host that
	// shadows a hand-written runbook, silently replacing authored guidance with a generated page. The
	// host- prefix already makes a collision unlikely; the ordering makes it harmless.
	env, aerr := fw.loadArticles()
	if aerr != nil {
		return httpapi.WikiPage{}, false, aerr
	}
	for _, a := range env.Articles {
		if a.Slug != slug {
			continue
		}
		meta := map[string]string{}
		for k, v := range a.Meta {
			meta[k] = v
		}
		// Staleness travels in META, never in the body: a timestamp in the body would make every article
		// differ on every compile, which is what the predecessor does and why all 86 of its files churn
		// nightly. An operator still needs to know how old the page is, so the envelope's instant rides here.
		if !env.CompiledAt.IsZero() {
			meta["compiled_at"] = env.CompiledAt.UTC().Format(time.RFC3339)
		}
		return httpapi.WikiPage{
			Slug: a.Slug, Title: a.Title, Kind: "article", Body: a.Body, Meta: meta,
		}, true, nil
	}
	return httpapi.WikiPage{}, false, nil
}
