package main

import (
	"strings"

	"github.com/territory-grounder/grounder/core/estatedoc"
	"github.com/territory-grounder/grounder/core/metrics"
	"github.com/territory-grounder/grounder/core/screen"
)

// startEstateDocCoverageJob loads TG's estate DOCUMENTATION once at boot (TG-86 slice 1b) and reports its
// grounding COVERAGE as gauges Prometheus scrapes. Two transports arm coverage, so the gauge lights under
// BOTH deployment shapes (TG-487):
//
//   - PRODUCER — TG_ESTATE_DOCS_DIR set: walk the live docs dir, scrub each chunk, and (if TG_ESTATE_DOC_CORPUS
//     is also set) persist the scrubbed corpus for the eval-gated slice-2 retriever. This is the box that HAS
//     the raw IaC.
//   - CONSUMER — TG_ESTATE_DOC_CORPUS set but no docs dir: read the pre-built scrubbed corpus and arm coverage
//     from IT. This is the corpus-only transport (the distroless worker mounts ONLY the scrubbed corpus, never
//     the raw IaC) — before TG-487 this shape emitted NOTHING because the gauge was published only from a live
//     docs-dir walk and never read TG_ESTATE_DOC_CORPUS.
//
// TG_ESTATE_DOCS_DIR wins when both are set (the producer holds the authoritative source). UNCONFIGURED (both
// unset) it emits NOTHING — no phantom coverage, the same honest-absence discipline the self-dependency
// capability gauges follow (TG-394): a deployment without any grounding mount reads as "not grounded", never
// "grounded to zero, silently". A load/read/persist failure is NON-FATAL — estate grounding is a
// competence-plane read that never actuates, so a failure surfaces as tg_estate_doc_load_failed=1 rather than
// crashing the worker. Loading is synchronous at boot (a few hundred KiB of markdown/JSON), not on the scrape
// path, so a scrape never walks the filesystem.
func startEstateDocCoverageJob(getenv func(string, string) string, logf func(string, ...any)) func() []metrics.Sample {
	root := getenv("TG_ESTATE_DOCS_DIR", "")
	corpusPath := getenv("TG_ESTATE_DOC_CORPUS", "")

	switch {
	case root != "":
		scrub, pathMatch, redactErr := estateDocProducerScrub(getenv)
		if redactErr != nil {
			// The operator ASKED for identifier redaction and it could not arm — fail closed, exactly like a
			// load failure. Falling back to an unredacted walk would silently produce the identifier-laden
			// corpus TG-486 exists to prevent.
			logf("estate-doc grounding: TG_ESTATE_DOC_REDACT_PATTERNS set but redaction could not arm (%v) — refusing the docs-dir ingest (fail-closed)", redactErr)
			return estateDocLoadFailedGauge()
		}
		corpus, err := estatedoc.Load(root, estatedoc.DefaultMaxChunkBytes, scrub)
		if err != nil {
			logf("estate-doc grounding: ingest of %q FAILED at boot (non-fatal; coverage reads failed): %v", root, err)
			return estateDocLoadFailedGauge()
		}
		if pathMatch != nil {
			corpus.Redactions += estatedoc.RedactCorpusPaths(&corpus, pathMatch)
		}
		switch {
		case corpusPath != "" && pathMatch == nil:
			// PERSISTING an identifier-laden corpus is the STONITH-class act (TG-486): this walk redacted
			// SECRETS only, so the artifact would carry every hostname/site-code the offline producer strips —
			// with no abort-on-survivor gate on this path. Coverage still arms from the in-memory corpus; only
			// the WRITE is refused.
			logf("estate-doc grounding: REFUSING to persist to %q — TG_ESTATE_DOC_REDACT_PATTERNS is unset, so this corpus retains estate identifiers. "+
				"Mount the redaction pattern files and set TG_ESTATE_DOC_REDACT_PATTERNS, or produce the corpus with scripts/estate-docs-corpus.sh (coverage-only from here)",
				corpusPath)
		case corpusPath != "":
			if wrote, werr := estatedoc.WriteCorpus(corpusPath, corpus); werr != nil {
				logf("estate-doc grounding: persist to %q FAILED (non-fatal): %v", corpusPath, werr)
			} else {
				logf("estate-doc grounding: %d chunks over %d files from %s (%d redactions incl. identifiers; corpus persisted=%v for slice-2 retrieval)",
					len(corpus.Chunks), corpus.Files, root, corpus.Redactions, wrote)
			}
		default:
			logf("estate-doc grounding: %d chunks over %d files from %s (%d redactions; TG_ESTATE_DOC_CORPUS unset, coverage-only)",
				len(corpus.Chunks), corpus.Files, root, corpus.Redactions)
		}
		return estateDocCoverageGauges(corpus)
	case corpusPath != "":
		// Consumer transport (TG-487): no raw docs dir ships to this box, so arm coverage by READING the
		// pre-built scrubbed corpus. A missing/corrupt corpus is a non-fatal load failure, same as the walk.
		corpus, err := estatedoc.ReadCorpus(corpusPath)
		if err != nil {
			logf("estate-doc grounding: read pre-built corpus %q FAILED at boot (non-fatal; coverage reads failed): %v", corpusPath, err)
			return estateDocLoadFailedGauge()
		}
		logf("estate-doc grounding: %d chunks over %d files from pre-built corpus %s (%d redactions; corpus-only transport)",
			len(corpus.Chunks), corpus.Files, corpusPath, corpus.Redactions)
		return estateDocCoverageGauges(corpus)
	default:
		return func() []metrics.Sample { return nil }
	}
}

// estateDocProducerScrub builds the producer-side scrub for a docs-dir walk on THIS worker. With
// TG_ESTATE_DOC_REDACT_PATTERNS unset it is core/screen alone (secrets only) and pathMatch is nil — callers
// must then treat the corpus as identifier-laden and refuse to persist it. With the env set (comma-separated
// pattern files, the denylist format — mount github-sync/denylist.txt + scripts/estate-redact-extra.txt),
// identifier redaction composes in exactly as cmd/estatedoc-ingest's -redact-patterns, and an arm failure is
// an ERROR the caller fails closed on, never a silent fallback to the unredacted walk.
func estateDocProducerScrub(getenv func(string, string) string) (estatedoc.ScrubFunc, func(string) bool, error) {
	spec := getenv("TG_ESTATE_DOC_REDACT_PATTERNS", "")
	if spec == "" {
		return screen.Scrub, nil, nil
	}
	patterns, err := estatedoc.LoadRedactPatterns(strings.Split(spec, ",")...)
	if err != nil {
		return nil, nil, err
	}
	redact, err := estatedoc.NewPatternRedactor(patterns)
	if err != nil {
		return nil, nil, err
	}
	match, err := estatedoc.NewPatternMatcher(patterns)
	if err != nil {
		return nil, nil, err
	}
	return estatedoc.ComposeScrub(screen.Scrub, redact), match, nil
}

// estateDocCoverageGauges publishes the four grounding-coverage gauges from a loaded or read corpus. Both
// transports (docs-dir walk and pre-built corpus read) end here, so the coverage numbers mean the same thing
// regardless of how the corpus reached the worker.
func estateDocCoverageGauges(corpus estatedoc.Corpus) func() []metrics.Sample {
	files, chunks, red := float64(corpus.Files), float64(len(corpus.Chunks)), float64(corpus.Redactions)
	return func() []metrics.Sample {
		return []metrics.Sample{
			{Name: "tg_estate_doc_files", Kind: metrics.Gauge, Value: files,
				Help: "estate documentation files ingested into the grounding corpus (TG-86)"},
			{Name: "tg_estate_doc_chunks", Kind: metrics.Gauge, Value: chunks,
				Help: "scrubbed retrievable chunks in the estate-doc grounding corpus (TG-86)"},
			{Name: "tg_estate_doc_redactions", Kind: metrics.Gauge, Value: red,
				Help: "secrets/identifiers redacted during estate-doc ingest; a non-zero count is healthy, not an error (TG-86)"},
			{Name: "tg_estate_doc_load_failed", Kind: metrics.Gauge, Value: 0,
				Help: "1 when the estate-doc ingest failed at boot (TG-86)"},
		}
	}
}

// estateDocLoadFailedGauge is the single-sample failure surface: the corpus could not be loaded (walk) or read
// (pre-built) at boot, so grounding is unavailable but the worker keeps running.
func estateDocLoadFailedGauge() func() []metrics.Sample {
	return func() []metrics.Sample {
		return []metrics.Sample{{
			Name: "tg_estate_doc_load_failed", Kind: metrics.Gauge,
			Help:  "1 when the estate-doc ingest failed at boot; the grounding corpus is unavailable (TG-86)",
			Value: 1,
		}}
	}
}

// estateDocSeedTopK bounds how many doc chunks the seed grounding folds in per incident — small, because the
// <estate> block is soft-budgeted (untrustedBlockBudgetRunes) alongside the graph context, and the single most
// on-point section (a component's own CLAUDE.md) is most of the grounding value.
const estateDocSeedTopK = 2

// estateDocSeedGrounding builds the TG-86 slice-2b seed-grounding func — or nil (grounding OFF, byte-identical
// seed). Armed by TG_ESTATE_DOC_SEED, it loads the estate-doc corpus by the same two transports the coverage
// job uses — a live docs-dir walk (TG_ESTATE_DOCS_DIR, the producer) or the pre-built scrubbed corpus
// (TG_ESTATE_DOC_CORPUS, the distroless consumer) — and returns a func that ranks the operator's docs for a
// host and renders the grounding block. It ranks the SAME secret-scrubbed chunks; the doc prose is re-screened
// at the seed boundary (activities.go folds it into <estate>, which screenSeedBlock scrubs and composeSeed
// delimiter-neutralizes). A load failure or empty corpus is NON-FATAL — grounding is a read that never
// actuates — so it returns nil and the worker runs ungrounded. (This is a second boot-time corpus read
// independent of the coverage load; sharing one load is a follow-on dedup.)
func estateDocSeedGrounding(getenv func(string, string) string, logf func(string, ...any), hosts func() []string) func(host, summary string) string {
	if !truthy(getenv("TG_ESTATE_DOC_SEED", "")) {
		return nil
	}
	var (
		corpus estatedoc.Corpus
		err    error
	)
	switch {
	case getenv("TG_ESTATE_DOCS_DIR", "") != "":
		// Same composition as the coverage producer: with TG_ESTATE_DOC_REDACT_PATTERNS armed, the in-memory
		// grounding corpus is identifier-free too (belt to the model-facing NewIdentifierRedactor below, whose
		// live-hostname vocabulary misses site-codes/domains/IPs). A redaction-arm failure fails closed.
		scrub, pathMatch, redactErr := estateDocProducerScrub(getenv)
		if redactErr != nil {
			logf("estate-doc grounding: TG_ESTATE_DOC_SEED armed but TG_ESTATE_DOC_REDACT_PATTERNS could not arm (%v) — seed grounding OFF (fail-closed)", redactErr)
			return nil
		}
		corpus, err = estatedoc.Load(getenv("TG_ESTATE_DOCS_DIR", ""), estatedoc.DefaultMaxChunkBytes, scrub)
		if err == nil && pathMatch != nil {
			corpus.Redactions += estatedoc.RedactCorpusPaths(&corpus, pathMatch)
		}
	case getenv("TG_ESTATE_DOC_CORPUS", "") != "":
		corpus, err = estatedoc.ReadCorpus(getenv("TG_ESTATE_DOC_CORPUS", ""))
	default:
		logf("estate-doc grounding: TG_ESTATE_DOC_SEED armed but no corpus source (TG_ESTATE_DOCS_DIR / TG_ESTATE_DOC_CORPUS) — seed grounding OFF")
		return nil
	}
	if err != nil {
		logf("estate-doc grounding: TG_ESTATE_DOC_SEED armed but corpus load FAILED (%v; non-fatal) — seed grounding OFF", err)
		return nil
	}
	if len(corpus.Chunks) == 0 {
		logf("estate-doc grounding: TG_ESTATE_DOC_SEED armed but the corpus is empty — seed grounding OFF")
		return nil
	}
	r := estatedoc.NewRetriever(corpus)
	logf("estate-doc grounding: SEED grounding ARMED (TG-86 slice 2b) — %d chunks over %d files, top-%d per incident, folded into <estate>",
		len(corpus.Chunks), corpus.Files, estateDocSeedTopK)
	return func(host, summary string) string {
		grounding := estatedoc.GroundingContext(r.Retrieve(host, summary, estateDocSeedTopK))
		if grounding == "" || hosts == nil {
			return grounding
		}
		// Redact estate identifiers (TG-486) from the MODEL-FACING grounding using the LIVE host set. This is
		// where the identifier source is real: the per-incident graph is populated, unlike the boot-time corpus
		// load, and the on-box corpus is never mirrored — so the model exposure is exactly here. NewIdentifierRedactor
		// is probe-first (byte-identical when the grounding names no host), so an empty host set is a no-op.
		redacted, _ := estatedoc.NewIdentifierRedactor(hosts())(grounding)
		return redacted
	}
}
