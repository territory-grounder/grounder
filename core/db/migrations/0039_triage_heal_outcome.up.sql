-- 0039: carry the executed-mutation flag + the orchestrator-confirmed clear into the durable triage record
-- (benchmark axis A3 — heal success rate, docs/BENCHMARK-AXES.md).
--
-- A3 = of the incidents where TG ACTUATED a remediation, what fraction had the ORIGINAL fault CONFIRM-CLEAR
-- afterward. The Runner already computes both facts — res.Mutated (exec.Executed: the estate actually changed)
-- and ConfirmedClear (the orchestrator-captured post-condition re-observation that the incident's own
-- (host, alert_rule) went quiet after the mutation, TG-124) — but discards them at the triage DB boundary, so
-- the live-axis scorer (cmd/axisscore) could not measure A3 and listed it as an unmeasurable coverage gap.
--
-- WHY THIS, NOT action_verdict: the live heal path is native-ssh (guest-down -> start-guest), whose success
-- signal is ConfirmedClear. The async action_verdict (match/deviation/partial) is a DIFFERENT thing — a MATCH
-- deliberately EXCLUDES the target's own alert, so a match can never mean "the original condition cleared"
-- (temporal/runner/activities.go). ConfirmedClear is the faithful A3 numerator for the live actuation path.
--
-- A3 read off these columns is a FLOOR: ConfirmedClear is fail-closed (an unobserved clear stays false), so a
-- slow provider recovery past the bound reads as unconfirmed rather than as a failed heal — the scorer frames
-- A3 as a lower bound accordingly.
--
-- OBSERVABILITY ONLY — both are persisted decision RECORDS, they re-enter no gate. Additive, defaulted,
-- backward-compatible: pre-migration rows read as false (mutated/cleared unknown), exactly what they were.
-- mutated is written at first insert (known at record time); confirmed_clear is written by a follow-up update
-- once the bounded clear-observe loop resolves (it is not known when the triage row is first inserted).
ALTER TABLE session_triage
    ADD COLUMN mutated boolean NOT NULL DEFAULT false,
    ADD COLUMN confirmed_clear boolean NOT NULL DEFAULT false;
