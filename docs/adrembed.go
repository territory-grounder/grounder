// Package docs embeds the architecture decision records so the deployed grounder can serve them.
//
// WHY THEY ARE EMBEDDED RATHER THAN COMPILED. Every other page the wiki serves is either DERIVED from
// the spine (core/wikicompile) or DISTILLED from a confirmed-clean resolution (the lessons corpus). An
// ADR is neither: it is an authored decision with a date and a rationale, and its value is precisely
// that it does NOT change when the estate does. Compiling one would be a category error — there is
// nothing to derive it from.
//
// They ship inside the binary for the same reason the runbooks do: the deployed grounder is a static
// image with no docs/ tree on disk, so embedding is the only way to serve them honestly rather than
// render a broken link.
//
// WHY THIS FILE IS HERE AND NOT IN docs/adr/. That directory is a LAW SURFACE
// (scripts/lint-protected-paths.sh: `docs/adr/`), so every change inside it requires an owner's
// Law-Change-Approved-By trailer. That protection is for the RECORDS — the decisions themselves — and
// build plumbing does not belong behind it: an embed directive is not law, and putting it there would
// make every future change to how ADRs are served require a law approval. //go:embed cannot escape its
// own directory but CAN descend into a subdirectory, so the plumbing sits one level up and the law
// surface keeps containing only law.
package docs

import "embed"

// ADRs holds the embedded decision records (docs/adr/*.md). Slug is the filename without .md; title is
// the first `# ` heading, the same convention the runbook pages follow.
//
//go:embed adr/*.md
var ADRs embed.FS
