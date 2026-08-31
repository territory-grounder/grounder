<!-- Default MR template. Keep the checklist; delete lines that genuinely don't apply and say why. -->

## What & why

<!-- one paragraph: the change and the reason. Link the YouTrack TG issue. -->

## Checklist

- [ ] `make all` is green (vet · lint · spec · test · build).
- [ ] Governed files touched? spec↔code lockstep re-stamped and the owning spec updated.
- [ ] No literal secrets; every new route authenticated; no actuation path bypasses the mode chokepoint (fail-closed to Shadow).
- [ ] **Prompt / skill / model / agent-reasoning change? Paste a `make eval-gate` PASS below**
      (required — the fresh-base-arm A/B: candidate vs current `origin/main`, same on-box window; see
      [`docs/EVAL-GATE.md`](../docs/EVAL-GATE.md)). If this MR is pure test/CI/docs infra with no
      agent-behavior surface, tick and note "eval-exempt: infra-only".

<!-- Paste the eval-gate PASS table here for prompt/skill/model changes:
== TG eval gate ==
...
GATE: PASS
-->
