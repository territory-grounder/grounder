// Package wiki embeds the curated operator runbook pages served by the wiki read surface
// (GET /v1/wiki, spec/006 REQ-521). The pages ship inside the binary (go:embed) because the deployed
// grounder is a static image with no docs/ tree on disk — embedding is the only way the runbook
// section can be served honestly rather than fabricated. Content discipline: every page documents
// REAL, code-enforced behavior of the running system; a page describing behavior TG does not have is
// a doc bug.
package wiki

import "embed"

// FS holds the embedded runbook pages (*.md). The slug of a page is its filename without the .md
// extension; the title is its first `# ` heading.
//
//go:embed *.md
var FS embed.FS
