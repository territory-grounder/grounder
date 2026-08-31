-- 0090 down: drop the canonical description column. Operator-authored descriptions are lost by this
-- down-migration and that is stated rather than hidden — there is no sidecar to park them in, and
-- inventing one would recreate the split-brain 0090 exists to avoid.
ALTER TABLE skill
  DROP COLUMN description;
