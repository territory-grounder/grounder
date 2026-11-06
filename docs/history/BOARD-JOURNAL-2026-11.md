# BOARD journal — 2026-11 (verbatim historical archive)

**Historical journal, verbatim, not a queue.** Entries are as-of their stated dates; evidence
anchors (`file:line`, `!MR` states, counts) are as-of their entry date and may since have
drifted. The live board is `docs/BOARD.md`; the open-work inventory is YouTrack
`project: TG #Unresolved`. Nothing may exist ONLY here — every entry names its tracker id.

## 2026-11-03 — the MR gate unblocked: a calendar-exposed eval-gate defect and a stale gateway pin

- **Board bullet retired today (verbatim; resume budget — its full entry is the 2026-10-24 journal):**
  - **2026-10-24 — TG-600 Fixed** (prod `*:ec6af1bb`, H2-1c; journal): the rendering BEHIND each harness fingerprint
    is persisted (`harness_render`, mig 0117) and diffed via `tools/harnessdiff`; boot line ON, table self-arms on the
    first session. Sidecar follow-up = TG-601 (not started).

!1770 (README: link the Mission and Use Policy) could not merge. Every MR pipeline was red on two failures that
were already on main, so no MR's diff caused either.

- **`build-test`: TestCLI_TrendRubricCrossingDoesNotReanchorOverAnAbsoluteFloor → TG-621 Fixed.**
  - The test hardcoded its anchor as `MeasuredAt "2026-10-18"`. From about 10-26 that date was past
    `gate.TrendMaxStaleness` (8d), so the CLI reached `ShouldRefreshTrend`'s stale branch (TG-424).
  - That branch re-anchored on ANY regression, including a run that had failed the proposal_recall floor
    (0.00 < 0.50). The calendar exposed a real hole, not just a rotten fixture.
  - The stale branch now goes through `ReanchorEligible`, the allow-list the rubric-crossing path (TG-595)
    already used. The fixture is clock-relative and runs fresh and stale. A killing mutation turns RED.
  - No live impact: the committed anchor is fresh (11-03), and the last 8 nightlies held the floor at
    0.54–0.71.
  - Fresh-eyes review: APPROVE. Its one medium finding is pre-existing and shared with the crossing path:
    `ReanchorEligible` ignores `Verdict.Unmeasured`. Filed as **TG-622**.
- **`trivy-image-scan`: 4 fixable CRITICALs in the pinned gateway image → TG-589 Fixed.**
  - The findings: wolfi ca-certificates-bundle CVE-2026-91403 and libbrotli×3 CVE-2026-111497.
  - Re-pinned to main-stable `@37e2dafb`, which moves litellm from 1.104.2 to 1.106.6. It ships certifi
    2026.10.19, so the last `.trivyignore` entry (GHSA-6q48-6c7w-9gw2, expiring 11-21) is retired.
  - Verified with the TG-573 procedure: trivy with the gate's flags exits 0, and the old digest is the exit-1
    control. Old and new were smoke-booted with the real config: identical 13 routes, healthcheck OK.
  - After deploy, live calls on `fast` and `primary` answered.
- **Landed:** !1771 (merge `3535985f`, MR pipeline 62735 21/21, main 62736 green incl. deploy). !1770 rebased
  onto it and merged (`2d1b0339`).
- **Not touched:** the scheduled 03:53 main pipeline is still red every night on `delivery-witnesses` and
  `actuation-guard-coverage` (TG-557).
  - The deployed-sha witness is BLIND structurally: the tip is always the nightly's skip-ci commit, and
    neither git (absent from the witness image) nor the commits-API fallback resolves the last buildable
    ancestor.
  - The coverage check reports 111/195 hosts AWX-unreachable.
