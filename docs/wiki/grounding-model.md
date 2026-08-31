# The grounding model — predictions, verdicts, and the scorecard

Territory Grounder's core claim is that its model of the estate (the map) is continuously tested
against the estate itself (the territory). This page explains the mechanism that makes the claim
falsifiable.

## Committed predictions

Before an action would execute, the system predicts its consequences — which hosts and services the
blast radius touches, derived from the worker's confidence-weighted causal graph. The prediction is
**content-hashed into the ActionManifest** at commit time, so it cannot be edited after reality has
been observed. A prediction you can rewrite afterwards is not a prediction.

## The mechanical verifier

After the fact, a deterministic verifier compares observed reality against the committed prediction
and authors exactly one verdict:

- **match** — reality stayed inside the predicted map.
- **partial** — some of the prediction held; remediation stays human-gated until a clean match.
- **deviation** — the territory diverged from the map. The verifier's deviation is final (the acting
  model can never overturn it) and permanently demotes that bound action from unattended execution.

The verifier is the sole verdict author. No agent, prompt, or operator assertion can substitute for
it.

## The falsifiability control

A prediction that "beats chance" must be shown to, not assumed to. Each real blast-radius prediction
is scored beside a **degree-preserving shuffled-graph control** — the same prediction machinery run
over a topology-randomized copy of the estate graph. The **signal ratio** is the average real
true-positive count over the control's (floored at 1 to keep the division honest): a ratio above 1
means the real graph carries signal the shuffled graph does not.

## The scorecard

`GET /v1/grounding` publishes the live aggregates: the match/partial/deviation distribution and
match rate, blast-radius precision and recall, the signal ratio, and the autonomy-band distribution
with the never-auto floor-hold count. Every figure is computed from real rows in the verdict,
prediction, and audit tables. An empty spine reports zeros — the scorecard never invents a match
rate it cannot substantiate.
