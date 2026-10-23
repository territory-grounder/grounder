-- TG-190(a) / CONSTITUTION §4.9: "synthetic canaries against an isolated throwaway DB
-- (live-DB-leak counter must stay 0)".
--
-- THE TRIPWIRE COMES BEFORE THE CANARY, DELIBERATELY. A synthetic-incident injector drives known traffic
-- through the real loop against a THROWAWAY database; the risk it carries is that a bug lets one of those
-- rows reach the LIVE database, where the judge would score it, the flywheel would learn from it, and every
-- rate computed over the corpus would be measuring TG's own test traffic. Building the injector first would
-- mean running that risk with no way to observe it.
--
-- A STRUCTURAL MARKER, NOT A NAME CONVENTION. core/db/governance_read.go already refused the shortcut and
-- said why: "inventing one from a name convention would silently drop real sessions out of the denominator
-- on a naming coincidence — which inflates the judged fraction and hides a dead judge." A column cannot be
-- collided with by an external_ref that happens to start with the wrong prefix.
--
-- DEFAULT FALSE is the load-bearing choice. Every existing row, and every row any current writer produces,
-- is real by construction — so the counter below reads 0 today and any non-zero reading is a NEW fact about
-- the system rather than a backfill artefact.
ALTER TABLE session_triage
  ADD COLUMN IF NOT EXISTS synthetic boolean NOT NULL DEFAULT false;

COMMENT ON COLUMN session_triage.synthetic IS
  'TG-190: true ONLY for synthetic-canary traffic. On the LIVE database this must always be false — a true row here is a canary that leaked out of its throwaway database (tg_synthetic_rows_live). Set by the canary injector; never by the runner.';

-- Partial index: the query this exists for is "are there ANY true rows", and a partial index over the
-- true-only subset stays empty-and-free on the live database while making the leak count O(1) rather than a
-- sequential scan of the whole corpus every scrape.
CREATE INDEX IF NOT EXISTS session_triage_synthetic_leak
  ON session_triage (created_at) WHERE synthetic;
