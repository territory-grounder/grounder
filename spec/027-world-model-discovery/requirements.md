<!-- spec/027 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/027 — Auto-drafted world model: the admin reviews, never authors

**Owning behavior family:** BEH-13 (narrative row lands via a separate law MR, BEH-10/11 precedent).
**Constitution / invariants:** INV-13 (no secret literals), INV-14 (retention on every row), INV-15
(no fabricated console rows), INV-17 (compiled-in modules), INV-19 (one ledger).
**Phase:** P2.
**Status:** Draft.
**Epic:** TG-227 / TG-229 (plane 2). Design provenance: workflow wf_3a385a3f-a58, 2026-07-31.

Discovery over the estate's own sources auto-drafts a reviewable manifest (hosts, guests, units,
containers, blast-radius edges — each with provenance and confidence). The admin REVIEWS diffs and
adopts with one click; approved entries materialize the allowlists while leaf-level default-deny
enforcement stays byte-untouched. Scheduled re-discovery surfaces drift as one-click adopts.
Paradigm rule 9 (CONSTITUTION.md:237): estate knowledge is self-populating — there is NO endpoint
for hand-authoring manifest entries.

## Requirements

- **REQ-2701** — [F] estate substrate reuse · INV-17.
  Discovery SHALL extend the existing estate substrate — the EdgeSource seam with per-source-isolated
  Build (`core/estate/build.go:8-56`), the fixed confidence table with MAX-ratchet and winning
  provenance (`core/estate/estate.go:63-110,187-216`), and the closed entity and relation
  vocabularies that loud-reject unknowns (`core/estate/declared.go:41-56`) — reusing the existing
  netbox, pve, librenms, and pveliveness modules and adding exactly two new compiled-in EdgeSources
  (systemd units, docker containers), both registered per INV-17 (`modules/registry.go:19-40`) and
  transport-injectable for oracle fakes.

- **REQ-2702** — [F] manifest store · migration 0047 · INV-14/INV-19.
  The system SHALL persist discovered entries in a `manifest_entry` table (entity type CHECKed
  against the closed estate vocabulary; per-type identity fields; edges jsonb with per-edge
  provenance and confidence; source; confidence; discovery timestamps; status CHECK IN
  `draft|approved|stale|retired` DEFAULT `draft`; server-derived approver; mandatory rationale;
  ledger_seq; retention_expires_at NOT NULL) whose state transitions occur ONLY through a single
  audited Transition function (clone of `core/skillstore/transition.go:15-104`: allowedTransitions
  map, mandatory rationale, ledger append BEFORE row update, no resurrection), with ledger decision
  strings `manifest:draft|adopt|reject|retire|drift` on the one chain.

- **REQ-2703** — [R] review-not-author flow.
  The console SHALL render draft-versus-approved diffs in a new `manifest` view (nine-touchpoint
  recipe; skDiffPanel reuse, `deploy/console/v2/modules/skills/js.txt:214-235`; provenance labeling
  per the knowledge module) and SHALL expose adopt, reject, and retire ONLY as
  `POST /v1/manifest/entries/{id}/{verb}` with the verb drawn from a closed table
  (`core/httpapi/skills_write.go:46-47` pattern), AuthSession step-up, mandatory rationale, execution
  through a worker Temporal workflow with ledger-before-row, and the vote-lane hardening kit
  (same-origin check, per-caller rate limit, honest 409-versus-503, server-derived principal,
  `vote.go:97-153`); buttons SHALL be data-keyed with server-computed caller_can_act.

- **REQ-2704** — [F] allowlist materialization · leaf default-deny untouched.
  Approved manifest entries SHALL become an allowlist SOURCE via injected AllowlistProvider seams at
  the per-target lane constructor (`cmd/worker/main.go:3830-3846`), the static leaf
  (`main.go:1515-1516`), and the proxmox actuator (`main.go:3912`), composed with the boot-frozen env
  allowlists (`bootstrap.go:326-350`) as a UNION with both sources provenance-labeled on the console,
  while the leaf-level default-deny gates (`modules/actuation/ssh/ssh.go:136-153`;
  `modules/actuation/proxmox/proxmox.go:63-90,117-131`) stay byte-untouched; `tools/guardallow`
  SHALL accept adopted manifest entries as its subject input with the host guard remaining final
  authority.

- **REQ-2705** — [F] re-discovery and drift · safe direction.
  A singleton Temporal cron (finalizer shape, `temporal/skilltrial/finalizer.go:19-24,70-150`:
  per-item errors never abort the pass; visible Outcome) SHALL re-run discovery, diff the fresh
  snapshot against approved rows, mark disappeared entries `stale` and never auto-retire them, draft
  new or changed entries, append `manifest:drift`, and surface each as a one-click adopt; a source
  that fails to build SHALL contribute nothing and be reported loudly, and absence of evidence SHALL
  never retire an approved entry.

- **REQ-2706** — [F] confidence law.
  Discovered entries SHALL carry their source confidence from the fixed table; adoption SHALL never
  lower confidence (MAX-ratchet); learned-tier contributions SHALL stay hard-capped below 0.80; the
  manifest SHALL never write into the estate graph with confidence above its source's table entry.

- **REQ-2707** — [R] interface to the earned catalog (spec/028).
  The ratify form SHALL read this store to render a "targets covered by approved manifest" chip, and
  a ratified class whose targets are not adopted SHALL parse and poll but SHALL NOT touch the host
  (leaf default-deny) — two independent operator grants that are never merged.
