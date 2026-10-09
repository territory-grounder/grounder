# eval/history — the committed, append-only quality record

Every gate run invoked with `--archive-dir eval/history` (both `eval-gate.sh change` and the nightly
trend-watch) writes one entry here: `<date>-<mode>-<sha>/{comparator,scorecard,verdict}.json`.

Discipline (replaces "paste the PASS table into the MR", docs/EVAL-GATE.md):

- **Commit the entry your gate run wrote, in the MR it gates.** The record rides the change.
- **Entries are append-only.** Re-running the same MR's gate may overwrite its own entry; nothing
  here is ever deleted or edited after merge.
- **A FAIL entry is evidence too.** A passing night pushes its entry together with the refreshed
  baseline (one commit); a failing night exits red before the push, so its entry is preserved as a
  30-day pipeline artifact and attached to the auto-filed regression issue's pipeline. The 2026-07
  lesson: the previous quality record lived in gitignored files, so the proposal-rate collapse from
  0.45 to 0.00 left no versioned trace across 1,377 commits. Never again route the quality trail
  through .gitignore.
