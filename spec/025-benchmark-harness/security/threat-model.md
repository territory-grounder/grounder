<!-- spec/025 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/025 — Threat model: the benchmark harness (STRIDE slice)

Per-feature threat slice for the measurement plane. The system-wide model is
[`docs/THREAT-MODEL.md`](../../docs/THREAT-MODEL.md); this file scopes the harness's own trust boundary and
is the security half of the spec's definition-of-done.

**Trust boundary.** The harness reads the decision spine (the triage/verdict/execution tables), reads the
estate through the same read-only surfaces the worker uses, and calls a judge model. Its outputs are
NUMBERS and an evidence artifact — but those numbers decide whether TG is declared fit to run autonomously,
so the ASSET here is the *integrity of the claim*, not the availability of a service. One component,
`tools/faultinjector`, deliberately mutates production hosts to generate evidence; it is inside this
boundary and carries its own obligations (REQ-2508).

**The distinguishing property of this threat model:** for most of TG, the adversary is external and the harm
is an unsafe action. Here, the most likely adversary is *us*, and the harm is a number that is believed. A
benchmark harness fails in the direction of its author's hopes. Every mitigation below is chosen because it
is mechanical — it does not depend on the author wanting the right answer.

## STRIDE

**Spoofing — a number attributed to a computation that did not produce it.**
An axis reported from one query while its documented definition describes another; a verdict attributed to a
judge model that was not the one served (the alias `primary` can resolve to a fallback). *Mitigations:* one
computing implementation per axis, lockstep-bound (REQ-2500); the provenance manifest records the model
ACTUALLY SERVED rather than the alias requested (REQ-2505); rubric identity stamped on every verdict
(REQ-2504).

**Tampering — the definition changes between two published numbers.**
The realistic form is not malice but drift: an axis query edited to "fix" an inconvenient result, a rubric
reworded, a window widened. Undetectable today — the harness is bound to nothing. *Mitigations:* lockstep
binding makes any edit hash-visible and forces a spec amendment (REQ-2500); the mutation control ensures the
golden tests actually constrain the query rather than tracking it (REQ-2501); a restatement publishes the old
number and the reason (REQ-2502).

**Repudiation — a published result that cannot be reproduced or traced.**
An artifact whose inputs are gone, whose contaminated rows were dropped silently, or whose excluded
population is invisible. *Mitigations:* the evidence manifest (REQ-2505); contamination recorded WITH the
evidence rather than filtered out; excluded rows reported with their count and reason (REQ-2502).

**Information disclosure — the harness leaks what the runtime protects.**
The judge prompt carries real trajectories, and the predecessor's shadow log is known to contain a live
credential. The harness also runs outside the runtime's lint. *Mitigations:* every trajectory passes the
shared redaction before entering a prompt, payload or log; the forbidden-pattern lint extends over `tools/`
(REQ-2507); evidence files are scanned by the same secret gate as the runtime.

**Denial of service — the harness harms what it measures.**
The injector breaks production hosts by design. Its failure mode is not "the benchmark stops" but "a
production guest is left broken" — which has happened: two guests were stranded at 97% disk ~80 minutes past
their restore deadline. *Mitigations:* REQ-2508's full set — durable obligations recorded before the effect,
reconcile on start/cycle/shutdown, refuse when the estate is unobservable, never stack a fault on a target
that owes a restore, quarantine rather than assume on an unverified discharge.

**Elevation of privilege — the measurement plane reaching the effect channel.**
The harness must never become an actuation path. *Mitigations:* the harness reads through read-only surfaces
and holds no actuator; the injector's mutations run over its own operator-supplied identity and go nowhere
near the interceptor chain, so they are estate changes made BY the operator FOR the benchmark, never governed
actuations TG is credited with. A harness that could actuate would also be able to manufacture its own
success, which is the same failure as tampering wearing different clothes.

## Residual risks, stated rather than mitigated

- **Structural conflict of interest.** TG's team authors the harness, the rubric, the judge prompt and the
  injector, and TG's own model gateway hosts the judge. No mechanism inside this repository removes that. The
  only real mitigations are one-command third-party reproduction and an external suite; the first is funded
  by REQ-2505, the second is explicitly out of scope.
- **Meta-contamination.** The comparator recognises some benchmark hosts. Recorded with the evidence
  (REQ-2505), not solved.
- **Judge calibration.** The judge's agreement with ground truth is unmeasured. REQ-2504 constrains
  SYMMETRY, not accuracy; a systematically wrong judge applied fairly to both systems still yields a wrong
  comparison.
