<!-- spec/025 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/025 — Benchmark harness: the measurement plane is governed evidence

**Owning behavior family:** — (proof plane; no BEH row — it measures the governed behaviors rather than
adding one).
**Constitution / invariants:** INV-19 (evidence is append-only and attributable), INV-22 (no undeclared
test-gap), INV-21 (read-only — the harness never actuates).
**Phase:** Phase 2 (proof plane; read-only; mutation stays OFF).
**Status:** Draft.

The harness produces the v1.0 gate — the claim that TG exceeds its predecessor. It is the most
consequential output the project has, and it is the least governed code in the repository. Measured at
authoring time: **0 of 955 lockstep-lock lines cover it, 0 CI jobs run it, and `core/db/axis_read.go` —
408 lines of measurement SQL that computes every published axis — has no test file at all.** `tools/` is
explicitly excluded from the forbidden-pattern lint. Any axis can therefore be silently redefined between
two published numbers, by anyone, with nothing failing.

This is not hypothetical drift. In a single session the project found that its published head-to-head
overall pooled a dimension one side never competes on (flattering TG), that its verified-match rate pooled
executed actions with never-executed predictions (understating actuation accuracy by 26 points), and that
its MTTR figure was derived from a join that produced durations seven days negative. Each was a defect in
ungoverned measurement code, and each survived because nothing tested the SQL and nothing bound it to a
spec.

> **Read-only by construction.** Every requirement here reads state and computes numbers. The harness
> observes the estate and the decision spine; it never actuates, never gates a decision, and never writes to
> a governed table. The one component that *does* touch the estate — the fault injector — is governed by
> REQ-2508 precisely because it mutates production to generate evidence.

## Requirements

- **REQ-2500** — [O] INV-22 · [R] the axis definition is the claim.
  Every published benchmark axis SHALL have exactly ONE computing implementation, and that implementation
  SHALL be bound to this spec in `spec/.lockstep.lock`. An axis definition SHALL NOT change without the
  lockstep hash changing, so a redefinition between two published numbers is mechanically detectable rather
  than a matter of trust. WHERE an axis is reported in more than one surface (the scorer, the eval gate, a
  published artifact), those surfaces SHALL read the same computation rather than re-deriving it.

- **REQ-2501** — [O] INV-22 · [F] a test that cannot fail is not a test.
  The measurement SQL SHALL be covered by golden-fixture tests over a REAL PostgreSQL instance with every
  migration applied, whose expected values are hand-computed from the fixture rather than captured from the
  implementation's own output. Each covered axis SHALL additionally carry a MUTATION CONTROL: perturbing one
  predicate of that axis's query SHALL turn its golden test RED. An axis whose test still passes under a
  deliberately broken query is not covered, however green it reads.

- **REQ-2502** — [O] INV-19 · [R] a number without its denominator is not evidence.
  Every reported axis SHALL carry the population it was computed over: the denominator, the window, and —
  WHERE the axis excludes rows it cannot measure — the count excluded and the reason. A zero-numerator axis
  SHALL be published with its statistical upper bound rather than as a bare zero, since "0 observed in n
  trials" and "cannot happen" are different claims and only one of them is supported by the data.

- **REQ-2503** — [O] INV-22 · [R] measurement validity is a property of the join, not the number.
  WHERE an axis correlates two records that share no key, the correlation method and its bound SHALL be
  stated in the reported output, not only in the code. A correlation that widens silently (an unbounded
  window, a fuzzy match) SHALL be treated as a measurement defect. *Rationale:* the two obvious joins for
  time-to-recovery both produced confident, plausible-looking numbers — one from zero matching rows, one
  with durations seven days negative — and neither announced its own invalidity.

- **REQ-2522** — [O] INV-22 · [R] a recall axis SHALL report the LATENCY and the SOURCE of each detection,
  not only that a detection occurred. WHERE more than one ingest source can detect the same fault, the axis
  SHALL attribute the detection to the source that reported it FIRST, and SHALL NOT credit a later source
  for a fault already detected. *Rationale:* A1 answers a yes/no question, so a ~39-second detector and an
  ~11-minute one score identically in it. TG's own pve-liveness detector was built specifically to raise A1,
  is wired to the ingest ledger, and contributes correctly to recall — and the speed advantage that justified
  building it was reported by no surface at all, existing only as a note. The measurement is not new data:
  `injected_fault.injected_at` and `ingest_alert.received_at` were both already written, and the correlation
  is the same rule-class match the recall query uses. Crediting every source that eventually alerted, rather
  than the first, would score the slow path for faults it did not find and erase the fast detector's reason
  to exist.

- **REQ-2523** — [O] INV-22 · [R] the A1 fault-class→alert-rule mapping SHALL be checked against the
  injector's CLOSED ENUMERATION READ FROM ITS SOURCE, never against a hand-kept copy, and the mapping text
  SHALL exist in exactly one place. *Rationale:* the guard for this was already written after container-down
  shipped unmapped and published 0/18 — and it used a mirrored list in the test file, with a comment saying
  "if the two ever diverge the divergence is itself the finding". They diverged and nothing found it:
  log-fill shipped with no mapping AND no entry in the mirror, so the test written precisely to catch a class
  shipped without a mapping passed. A mirror is maintained by the same person who forgot the thing it guards.
  Separately, the companion assertion that both queries share the predicate compared the constant to ITSELF
  (`detectRuleMatch + detectRuleMatch`), so a query rewritten with a hand-copied predicate would have
  satisfied it forever; counting interpolation sites is also insufficient once a third query exists.

- **REQ-2504** — [O] INV-22 · [R] one rubric, one judge, one meaning.
  A judged comparison SHALL score both systems from the same rubric source and the same judge parameters,
  and SHALL record the rubric's identity with every verdict, so two numbers produced under different rubrics
  can never be pooled or compared without that being visible. An aggregate over judged dimensions SHALL
  include ONLY dimensions BOTH systems are scored on; a dimension only one system competes on SHALL be
  reported as a unilateral property and SHALL NOT enter a comparative mean or decide a head-to-head winner.

- **REQ-2505** — [O] INV-19 · [R] the harness must not manufacture its own result.
  Evidence supporting a published claim SHALL be committed with a provenance manifest recording the code
  revision, the rubric identity, the model actually served (not the alias requested), the window bounds and
  the host set. WHERE the comparison population is contaminated — a host the comparator recognises, an
  injected fault the system under test cannot see — the contamination SHALL be recorded with the evidence
  rather than excluded silently.

- **REQ-2506** — [O] INV-22 · [F] pseudo-replication.
  A head-to-head claim SHALL state its INDEPENDENT sample size, not its row count, WHERE repeated
  observations share a cluster (the same incident, the same host). *Rationale:* a 21-row comparison drawn
  from six independent incidents, one contributing seven rows, is approximately six observations; reporting
  it as twenty-one overstates the evidence by more than a factor of three.

- **REQ-2507** — [O] INV-21/INV-22 · [R] a gate that cannot fail is not a gate.
  A CI job that gates on measured quality SHALL fail when its inputs are absent or unresolvable, rather than
  passing vacuously. A harness job SHALL run in CI on every change to the harness, and the forbidden-pattern
  lint SHALL cover the harness sources as it covers the runtime.

- **REQ-2508** — [O] INV-19/INV-21 · [R] the injector mutates production to make evidence.
  The fault injector SHALL record every injection and its restore obligation DURABLY before performing the
  effect, SHALL discharge outstanding obligations on start, on every cycle, and on shutdown, and SHALL
  refuse to inject when it cannot observe the estate, when the system under test cannot act on the target,
  or when a target already owes a restore. An obligation that cannot be verified as discharged SHALL leave
  its target quarantined rather than be assumed complete.
  An obligation SHALL be closed EARLY only on a failure PROVABLY PRIOR to any effect — a refused precondition
  that ran nothing — and an AMBIGUOUS failure SHALL leave it open. Discharge SHALL be established from an exit
  status that positively identifies the state; a status that merely differs from success SHALL be treated as
  UNKNOWN. *Rationale:* the injector's transport reports an unreachable host and a killed command as ordinary
  non-zero exits with no error, and both can occur AFTER the remote effect committed. Two controls read those
  as answers: the disk-fill verifier accepted any non-zero `test -e` as proof the fill was gone — so the very
  state in which a fill is most likely still present, an unreachable guest, reported as repaired — and an
  injection error was taken as proof nothing broke, closing the obligation. Both closes are PERMANENT, because
  the outstanding-work query re-reads only pending and failed rows, so the guest is stranded, the ledger
  asserts it was restored, and the quarantine that would have stopped another fault landing on it is released.
  Every UndoArgv is idempotent and discharge is verified against real state, so holding an obligation open
  costs at most one redundant repair; closing one wrongly costs a stranded guest.

- **REQ-2509** — [O] INV-19/INV-21 · a fault that stops a NAMED service takes its target from the operator.
  WHERE the injector supports a fault class that disables one named service on a host rather than the host
  itself, the target SHALL be OPERATOR-DECLARED per host, and the injector SHALL NOT discover, infer, or
  otherwise choose a target of its own. A host with no declared target SHALL be INELIGIBLE for that class
  rather than receive a chosen one, and an injection reaching the effect without a declared target SHALL abort
  before running anything on the host. The recorded restore obligation SHALL name the exact target that was
  stopped, and its discharge SHALL be VERIFIED by reading that target's state rather than inferred from the
  repair command's exit status (REQ-2508). No target name SHALL be compiled into a shipped artifact.
  *Rationale:* a host-level fault is self-describing — the host is the target — but a service-level fault is
  not, and a chosen one is unsafe in two directions. Selecting a victim by inspecting the host could stop a
  database, a log shipper or the monitoring agent rather than the intended service, and it would make the fault
  non-reproducible between runs, so the benchmark population would silently vary. Recording an obligation whose
  target is unnamed or wrong produces an obligation that can never be discharged, which is the stranding
  REQ-2508 exists to prevent, arriving by a different route. The declaration also keeps estate identities out
  of the repository, which the forbidden-pattern gate independently enforces.
  The injector's fault classes SHALL further form a CLOSED enumeration, and every declared class SHALL be
  either SCHEDULABLE or refused with a STATED reason — never rejected as merely unrecognised. *Rationale:* a
  class can be fully implemented — planner eligibility, effect, restore obligation, verified discharge, unit
  tests, this requirement — and remain unschedulable because one accept-list has never heard of its name. That
  is not a latent gap: arming such a class is fatal at startup, and it took the injector down in a crash loop
  the first time it happened (only the durable ledger's reconcile-on-start prevented a stranded guest). Tests
  of a class's BEHAVIOUR cannot catch it, because they exercise the planner and the effect directly and never
  traverse the configuration surface; the property must therefore be asserted over the enumeration itself.

- **REQ-2515** — [O] INV-22 · [R] an axis with no comparator is a property, not a victory.
  Every published axis SHALL declare whether a COMPARATOR exists on the incumbent side. An axis with no
  comparator SHALL be published as a property of the system that has it, with the absence stated, and SHALL
  NOT be reported as a win, a delta, or a comparison. WHERE an axis is partly comparable, the comparable and
  non-comparable parts SHALL be published separately and SHALL NOT be pooled.
  *Rationale:* the incumbent runs in SHADOW with mutation OFF — it receives the same alerts, reasons, records
  a band, and touches nothing. So for every axis that requires actuation (heal success, autonomy, fault-class
  breadth, false-actuation, safety violations) it has no score, and the error runs in BOTH directions.
  Reporting "TG 34 heals to 0" describes the experimental setup rather than a capability gap. Reporting
  false-actuation or safety-violation counts comparatively makes TG look permanently worse, because the only
  way to score a perfect false-actuation rate is to never actuate — penalising the system that does the work
  is the same error with its sign flipped. Diagnosis correctness is the one scored axis on which both systems
  genuinely compete, which is why the confirmatory primary endpoint is defined there.

- **REQ-2514** — [O] INV-22 · [R] two systems compared on different incidents are not being compared.
  A head-to-head PAIR SHALL require the same subject host, the SAME FAULT CLASS on both sides, and a bounded
  time separation; among qualifying candidates the selection SHALL be DETERMINISTIC and independent of storage
  order. A side whose fault class cannot be determined SHALL yield NO pair rather than a loose one, and the
  classifier SHALL be asserted over the CLOSED SET of alert rules the estate actually raises.
  *Rationale:* the pair is the unit of the whole campaign — if the two sides looked at different incidents the
  comparison is not a comparison, and no downstream statistical care recovers it. The previous rule was
  same-host plus a 12-hour window with first-match-wins, and on this estate one stopped guest raises FOUR
  distinct LibreNMS rules, so concurrent unrelated alerts on a host are the normal case: a TG disk-fill triage
  could pair against a predecessor device-down triage. First-match-wins additionally made the pairing depend on
  the order rows returned from the database, so two runs over identical data could pair differently — which
  cannot coexist with an exit criterion that a third party reproduces every number to the digit. The
  closed-set assertion is required because an unclassifiable side BLOCKS a pair rather than erroring: the first
  implementation wrote its patterns spaced while the real rule names are hyphenated, so every disk alert
  classified as unknown and the entire disk stratum would have disappeared from the campaign while the report
  still read as complete.
  The classifier SHALL consider EVERY identifying field a side carries, not the first non-empty one, and its
  closed-set assertion SHALL cover the vocabulary of BOTH sides. *Rationale:* the two systems label incidents
  in different vocabularies. The incumbent always populates a coarse category of its own —
  availability / maintenance / general / kubernetes / resource — none of which names a fault class, while the
  specific rule sits in the issue TITLE ("Infrastructure alert: host - Space on / is >= 90%"). A
  first-non-empty rule therefore always read the coarse field, always classified to nothing, and refused
  100% OF PAIRS — silently, since an unclassifiable side is defined to yield no pair rather than an error. The
  campaign would have accrued nothing while the harness printed "no aligned pairs yet", which is
  indistinguishable from a quiet estate. Asserting the classifier over one side's vocabulary is not enough;
  the check must exercise real incidents from BOTH.

- **REQ-2513** — [O] INV-22 · [R] a number must not overstate what is known about it.
  A statistic reported WITHOUT a computed confidence interval SHALL be rendered as a point estimate and SHALL
  NOT be given an interval derived from itself. A note attached to a measurement SHALL state the OBSERVATION
  and what must be checked; it SHALL NOT assert a CAUSE that the measurement does not establish, and in
  particular SHALL NOT exonerate the system under test.
  *Rationale:* two ways a report was claiming more than it knew. Cohen's kappa returned Lo = Hi = the estimate,
  which rendered as "95% CI -0.141–-0.141" — the tightest interval expressible, printed against the least
  certain number in the calibration report, and indistinguishable to a reader from a genuinely precise result;
  kappa's standard error is a different estimator from the Wilson form used for the proportions, so the honest
  output is no interval rather than a borrowed or fabricated one. Separately, the per-class detection note read
  "INSTRUMENTATION GAP: the monitoring rule does not cover these hosts; this is NOT a TG miss" whenever n >= 5
  and the rate was under 0.5 — an exoneration of the system under test inferred from a threshold, when those
  same two facts are equally consistent with a real detection failure. A low rate is a QUESTION; which cause
  produced it must be established, not asserted by the tool printing the number.

- **REQ-2512** — [O] INV-22 · [R] a mutation control must perturb the SHIPPED text, not a copy of it.
  WHERE a measurement is guarded by a mutation control, the control SHALL perturb the SAME artefact the
  implementation executes, and SHALL FAIL when the perturbation changes nothing. A control that reconstructs
  the logic under test and perturbs its own reconstruction SHALL NOT be counted as satisfying REQ-2501.
  *Rationale:* the A6b control wrote out its own copy of the recovery-correlation SQL, dropped the host match
  from that copy, and compared the two counts. It never called the implementation. It therefore demonstrated
  only that SQL means what SQL means: had the SHIPPED query lost its host predicate — the clause that stops a
  recovery on ANY host being attributed to this incident — the control would still have passed, while the
  measurement it guards silently inflated its correlated count and corrupted its percentiles. A control whose
  subject is a paraphrase cannot fail when the original breaks, and a control that cannot fail is not a
  control. The predicate is now a named constant interpolated into the query and perturbed by the test, and
  the test additionally asserts that the perturbation CHANGED something, so a fixture with no decoy cannot let
  it pass vacuously.

- **REQ-2511** — [O] INV-22 · [R] a mapping that fails closed fails SILENTLY.
  WHERE a measurement matches one external vocabulary onto another — the injector's fault classes onto the
  monitoring system's alert rules — every SCHEDULABLE class SHALL carry a mapping, and the mapping SHALL be
  expressed ONCE and shared by every query that uses it. A class present in a measurement's denominator with
  no mapping SHALL be treated as a defect, not as a miss.
  *Rationale:* failing closed on an unmapped class is the correct default — it stops a new class silently
  inflating recall — but it also makes the gap invisible, because nothing errors and the number merely comes
  out lower. `container-down` shipped 2026-07-27 with no entry: every injection added +1 to the A1 denominator
  and could never reach the numerator. Measured over seven days it detects 17/18 through the Service rule and
  was published as 0/18, understating pooled A1 from 83.3% to 78.5% — an understatement, which is the
  direction nobody investigates. The predicate had also been written out twice, and two copies of a mapping
  drift; one constant, interpolated, removes that. The obligation is discharged by an assertion over the
  CLOSED SET of classes, because a missing OR-clause is not a defect in any code path — it is an absence, and
  only an enumeration can see an absence.

- **REQ-2510** — [O] INV-22 · [R] a composite whose legs come from different populations is not one number.
  VISR (docs/TESTING-AND-BENCHMARK.md §2.1) SHALL NOT be published until all three of its legs — correct
  diagnosis, appropriate action, and an independently-confirmed postcondition — are computed over THE SAME
  incident population, and any published VISR SHALL state that population and its size. WHERE a leg is
  measured over a different population from another leg, the legs SHALL be published SEPARATELY under their
  own denominators rather than multiplied or averaged into a composite.
  *Rationale:* all three legs are already measurable, which is exactly what makes the composite tempting and
  wrong. Leg 1 is computed by `core/diagcorpus` over INJECTED faults with injector ground truth (298 faults);
  legs 2 and 3 publish today as A3 over the ACTUATED subset, which is a different and much smaller set — an
  incident TG declined to act on is in leg 1's denominator and absent from leg 3's entirely. Dividing verified
  successes by "replayed incidents" across those populations produces a ratio whose numerator and denominator
  describe different events, and the resulting number would look more authoritative than any of its parts.
  VISR is additionally defined over a REPLAY CORPUS of 200–300 curated historical incidents (§2.2) that does
  not exist; until it does, "replayed incidents" has no referent on this estate.
  This requirement therefore DEFERS VISR rather than implementing it, and is satisfied by the deferral being
  recorded. VISR is deliberately absent from P5's exit criterion, which asks for the diagnosis leg and judge
  calibration — both delivered — and not for the composite.

- **REQ-2516** — [O] INV-22 · [R] a fault class is only as real as its least-wired switch arm.
  Every class in the CLOSED enumeration `AllClasses()` SHALL be wired at EVERY registration point, and each
  point SHALL be asserted over that enumeration rather than over a hand-written list of class names: the
  planner's eligibility rule, the fault-handle rule, the deferred-restore arm, the undo, the repair verifier
  and the CLI accept-list. A class that OWES A RESTORE SHALL arm its deferred restore on a host that SURVIVES
  its own fault. An UNRECOGNISED class SHALL be refused at every one of those points — it SHALL NOT inherit a
  fault handle, an arm rule, an undo, or a repair verdict from another class. The repair verifier SHALL fail
  CLOSED: absent a verifier for the class, the obligation SHALL remain outstanding and its host quarantined.
  A `Provokes()` entry naming a slug absent from the live op-class registry SHALL be a finding.
  *Rationale:* `container-down` shipped fully implemented — planner, effect, obligation, verified discharge,
  ten unit tests, a spec requirement — and was still unschedulable, because ONE accept-list in the flag parser
  had never heard of it; the binary crash-looped on `unknown class`. That got a CLI guard, and the other five
  registration points did not, because behaviour tests cannot close this gap: they name the class they
  exercise, so a class nobody wrote a test for is a class nothing checks. Two of those five were measured
  fail-open when this requirement was written. `verifyRepaired`'s default returned `(true, nil)` — "verified
  repaired" without looking at anything — and the caller answers a true with `MarkRestored`, which is
  PERMANENT (`Outstanding` selects only `pending`/`failed`, so a closed row is never revisited); an unwired
  class would therefore close its obligation unverified and strand its fault on the estate forever, which is
  precisely the stranding this engine was built after. The fault-handle default returned the guest VMID, a
  plausible-looking value for EVERY class, so an unwired class would record a device-down handle and its undo
  would start a guest that was never stopped while the real fault stayed put. The arm-placement clause has a
  live incident behind it: docuseal01's cleanup was armed INSIDE the guest that was then stopped, the timer
  died with its target, and the fill was never cleaned — only `device-down` removes its own host, so only
  `device-down` may arm on the node.

- **REQ-2517** — [O] INV-10 · [R] a repair's exit code cannot distinguish "already fine" from "never ran";
  only a reading of real state can.
  An obligation SHALL be discharged ONLY on a positive verification of actual estate state. The repair
  command's exit code SHALL NOT, by itself, close an obligation NOR prevent verification from being attempted:
  the verifier SHALL be consulted whatever the repair returned. A verification that fails to read, or that
  reports the fault still present, SHALL leave the obligation outstanding with the reason recorded.
  *Rationale:* the reconciler treated `exit 255` as "ssh transport failure, retry", which is true of ssh and
  false of everything ssh runs — 255 is also a passthrough of the REMOTE command's exit code. `pct start` on a
  guest that is already running exits 255 with *"CT <id> already running"*, which is the DESIRED END STATE.
  Such an obligation could never discharge: it retried forever and held its host permanently "busy". Measured
  live 2026-07-28 — one stranded row plus four legitimate faults reached the `5/5 guests already faulted`
  throttle and stalled the entire campaign, so a fault class armed that same hour never ran once and the two
  op-classes it exists to provoke stayed frozen at 1/5 clean runs. Nothing errored; the log said "will retry"
  every three minutes, which reads like progress. The code already knew the answer and could not reach it:
  `UndoArgv` documents that *"`pct start` on an already-running guest exits non-zero but is harmless; the
  caller verifies by reading status rather than trusting the exit code"* — the caller never got to verify,
  because the exit code decided first. Deferring to the verifier is not weaker: a repair that genuinely never
  reached the host leaves the fault present and the verifier says so, and an unreachable host also fails the
  verifier's own read, so both quarantine with a recorded reason. Nothing closes except a positive reading.

- **REQ-2518** — [O] INV-22 · [R] detection is a monitoring STATE TRANSITION, not a state; a fault that raises
  no transition was never detectable and must not be counted as a miss.
  The planner SHALL NOT inject a fault class onto a target whose PREVIOUS fault of that same class was
  restored more recently than the configured settle window. The window SHALL be per (target, class) — a
  restore of one class SHALL NOT block another, since they raise different checks. A target with NO recorded
  recent restore of that class SHALL NOT be blocked; the guard SHALL key on evidence of a recent restore,
  never on its absence. A settle window of zero SHALL disable the guard entirely. WHERE the recent-restore
  lookup FAILS, the engine SHALL skip the injection rather than proceed, because an empty result is
  indistinguishable from "every target has settled".
  *Rationale:* measured live 2026-07-28. Two `service-down` faults hit one host two minutes apart — the first
  restored 04:15:22, the second injected 04:17:04 — while the LibreNMS service check polls every five minutes.
  The check never observed the recovered state, so it never flipped CRITICAL→OK→CRITICAL, the alert never
  cleared, it never re-raised, and TG received NOTHING for the second fault. The harm is not the missed heal:
  `injected_fault` recorded TWO faults while TG had ONE opportunity, so any detection rate computed as
  detections/injections scores the second as a MISS THAT WAS NEVER DETECTABLE. That is an instrument artefact
  read as a subject failure — the same error class that once buried the diagnosis number behind two sign
  errors, and precisely what INV-22 exists to prevent. With `restore-after` at 20m and a five-slot rotation
  cycling about every 15m, the collision is STRUCTURAL rather than bad luck. INVARIANT 2 already refuses to
  STACK faults and protects the ESTATE; this requirement carries the same idea past the restore and protects
  the MEASUREMENT.

- **REQ-2520** — [O] INV-22 · [R] an autonomy rate whose denominator is dominated by a property of the HARNESS
  is not a property of the system under test.
  WHERE an axis is computed over a population that includes incidents manufactured by the fault injector, the
  scorer SHALL report the COMPOSITION of that population, not only its size. For A4 specifically, the
  POLL_PAUSE denominator SHALL be broken down by the recorded `poll_reason`, and the count of attribution
  escalations raised on a host that was carrying an INJECTED fault SHALL be reported alongside the total.
  That artefact count SHALL be reported and SHALL NOT be subtracted from the axis, and attribution SHALL NOT
  be taught to recognise the fault injector.
  *Rationale:* measured 2026-07-28. A4 is the weakest axis at 41.7% among actionable proposals, and 605 of
  1038 proposals band POLL_PAUSE. Broken down: `actor-attribution-escalate` 344, `ood-novel-incident` 140,
  `actor-attributed-authorized` 59, the remainder under 30 each. Of the 344 escalations, **333 (97%) were
  raised on a host carrying an injected fault**. That SHARE alone is not evidence — about 97% of all incidents
  occur on injected hosts, so a 97% share is what pure base rate would produce. The RATE is what carries it:
  injected-fault incidents escalate at **39.6%** (345 of 871) against **5.0%** (11 of 219) for the rest, an
  ~8x elevation. Attribution does NOT simply fail on synthetic faults — it resolves most of them
  (`authorized-test` 700, `attributed-authorized` 62, `attributed-self` 6, against `unattributable` 109) — but
  it fails roughly eight times more often than on an organic incident. The dominant driver of the weakest
  axis is therefore a property of the instrument, and a published A4 that omits this invites the reader to
  attribute it to the agent. Subtracting it would flatter the axis; teaching attribution to recognise the
  injector would be worse still, because TG would then auto-heal BECAUSE a fault is synthetic — training on
  the instrument, generalising to nothing, and hollowing out the very security gate A7 measures. The bare
  band count is what REQ-2502 already forbids: a number without its population.

- **REQ-2519** — [O] INV-22 · **RETRACTED 2026-07-28, the same day it was written.**
  The engine SHALL NOT defer a due restore for an open approval; it SHALL restore on schedule. The mechanism
  this requirement described has been removed.
  *Why it was wrong:* every restore-owing injection ALSO arms an independent on-host timer —
  `systemd-run --collect --on-active=<restore-after> --unit=tg-restore-… <undo argv>` on the host that
  survives the fault. Nothing cancels, reschedules or reads it, and the reconciler has no handle on it. So the
  ESTATE is repaired at the original deadline whether or not the ledger row is held. Confirmed live in the
  guests' own journals: a `tg-restore-servicedown-…` timer armed at 15:46:25 and its service ran
  `/usr/bin/systemctl start nginx` at 16:06:34 — exactly restore-after later — while the reconciler was
  holding. The hold deferred only the ledger write.
  *Consequences:* the operator-facing log asserted "the fault must still be present for the vote to mean
  anything" at the exact moment that was false; the cost measured to justify narrowing the guard (mean
  lateness 1.8 → 4.7 min) was LEDGER lateness, not fault life; and because the settle window keys off
  `restored_at`, a held row inflated the (host, class) re-injection block by up to the full grace past actual
  recovery — a real cost for no benefit. Nothing in the accrual path reads `injected_fault`: a clean run is a
  post-state adjudication, so the hold could not have recovered an accrual even in principle.
  *The standing lesson:* a guard was designed, shipped, measured, narrowed on that measurement, and corrected
  again — four merge requests — before anyone asked whether the mechanism it guarded was the only thing
  repairing the estate. Read the WHOLE repair path before optimising one branch of it, and treat a
  measurement taken through the same wrong model as evidence of nothing. REQ-2518 (the settle window) is a
  separate control and is UNAFFECTED.

- **REQ-2521** — [O] INV-22 · [R] a disk-pressure corpus made entirely of un-healable synthetic artifacts can
  never prove a remediation.
  The harness SHALL provide a `log-fill` fault class that grows an OPERATOR-DECLARED application log until
  root usage enters the alerting band, and whose restore TRUNCATES that log rather than removing it. The
  target path SHALL be operator-declared per guest (never compiled in, never discovered by scanning); an
  absent or invalid declaration SHALL make the guest ineligible rather than cause a path to be guessed. A
  declared path SHALL be validated at EVERY entry point — declaration, injection, and repair — and SHALL be
  refused when it is not an unambiguous absolute path or when it lies inside an EVIDENCE STORE (the system
  journal, the actuator-guard trail, the audit/wtmp family). The class SHALL declare no op-class pairing until
  a remediating verb is registered.
  *Rationale:* 100% of the estate's disk-pressure corpus is `disk-fill`'s `fallocate`d artifact at a path only
  the harness owns — benchmark instrumentation TG must never learn to delete, which is why that class is
  correctly DETECTION-ONLY and provokes nothing. The consequence is that the owner-authorized reclaim
  capability has no honest fault to prove itself against: measured 74 disk-fill faults, 12 proposals, 1 heal,
  and TG declining is the CORRECT behaviour throughout. `log-fill` supplies the runaway-log shape a real
  estate produces and an honest truncate/rotate remedy addresses. The evidence-store refusal is not
  defensive decoration: the reclaim red-team rejected `journalctl --vacuum-*` outright because journald
  carries the sudo lines the actor-attribution engine reads and (on the control-plane host) every TG
  container's own logs, and the guard trail is the last-line control's only record — a fault whose restore
  truncates those destroys the evidence TG's own safety controls depend on. Validating at all three entry
  points, rather than only at declaration, keeps the destructive default unreachable from a future caller that
  bypasses the pool loader or from a ledger row written by an older binary.

- **REQ-2524** — [O] INV-22 · [R] a measurement reachable only by a human running a command is not a
  measurement of a running system.
  `core/db.AxisReadStore.Aggregate` derives axis A1's recall AND its per-source detection-LATENCY distribution,
  and it had exactly ONE caller in the tree: `cmd/axisscore`. So every axis existed only when someone
  remembered to run a CLI — ungraphable, unalertable, and unable to show a regression.
  *(Extension, 2026-08-14 / TG-480: the same rationale reaches the OPERATOR — the scorecard computation now
  lives in `core/axis` (`axis.Score` + `axis.Scorecard`, extracted verbatim from the CLI, JSON tags frozen as
  the published artifact shape) serving BOTH `cmd/axisscore` and the authenticated `GET /v1/axes` console
  read. The lockstep hash-bind follows the computation: `core/axis/scorecard.go` joins this spec's locked
  set, so the axis definitions stay spec-governed in their new home.)*
  The consequence is specific and was measured live: `pve-liveness` is the FIRST reporter on 70 device-down
  faults at ~34s median against `librenms` at ~612s, an 18x improvement — while its effect on A1 RECALL is one
  fault (89.3% → 89.6%), because the slower source eventually reports nearly everything too. **Recall is not
  the axis a faster detector moves; latency is**, and latency had no operational surface at all. A capability
  whose only benefit is invisible to the metrics will be removed by the next person reading a dashboard.
  Therefore the axes SHALL be sampled periodically from THAT SAME aggregate — never a second copy of the SQL,
  so the dashboard and the CLI cannot disagree — and published on `/metrics`. Detection latency SHALL be
  labelled PER SOURCE: an unlabelled series averages the fast detector into the slow one and hides the
  improvement it exists to show. A1 SHALL be published as its numerator and denominator, not only a ratio,
  because "9 of 10" and "900 of 1000" are different facts and a bare percentage distinguishes neither.
  The sampler SHALL emit NOTHING until a read has succeeded — a zero recall published as a reading asserts
  "the system detects nothing", the most alarming false statement available about a healthy estate — and a
  failed refresh SHALL retain the previous sample while exposing its AGE, so a frozen number cannot read as a
  current one. Sampling SHALL never block or fail triage.

- **REQ-2525** — [O] INV-22 · [R] one axis NAME over two different measurements is drift, whichever half you
  happen to read. (TG-205, 2026-08-04)
  `docs/BENCHMARK-AXES.md` defined A6 as **MTTR** — "resolving faster … detection latency, decision latency,
  actuation path" — while EVERY implementation of it measured decision STEPS: `cmd/axisscore`'s
  `a6a_mean_decision_steps`, `eval/gate`'s `MeanDecisionSteps`, and `session_triage.step_count`. The frozen
  vocabulary and the code therefore answered different questions under one label, and no scored surface
  reported time at all. The only clock in the system was `tg_agent_run_seconds_total`, a cumulative sum over
  all agent loops with no distribution, no per-incident attribution, and a reset on restart — so TG could not
  state time-to-decision for ANY incident, including its own measured ~39s-vs-~11min detection result, which
  was consequently unpublishable as a latency claim.
  Therefore the axis SHALL be SPLIT and its halves SHALL NOT be conflated: **A6a** is decision STEPS (the
  deterministic, agent-controlled figure the change-gate carries) and **A6b** is WALL-CLOCK (reported, never
  gated — the original rejection of wall-clock as a merge bar, gateway-dominated and noisy, stands).
  Steps SHALL NOT be described as a latency proxy: the same two-cycle decision costs seconds on the fast tier
  and minutes on the reasoning tier, which is exactly the manipulated variable of the model-tier A/B.
  A6b SHALL carry the TIME-TO-DECISION leg — the agent loop's wall-clock to the terminal proposal or grounded
  stop — persisted PER INCIDENT (`session_triage.decision_ms`, migration 0058) rather than accumulated in a
  process counter, so it can be sliced by tier, op-class and outcome and survives a restart. It SHALL be
  recorded on EVERY terminus a session can reach, not only the propose path: a stand-down is the commonest
  terminus and instrumenting one branch biases the published median toward it.
  The two A6b legs — time-to-decision and time-to-recovery — SHALL be published SEPARATELY and never pooled:
  the second is dominated by the monitoring system's recovery poll and by the provider, so their sum measures
  neither (this is REQ-2510's rule applied to the same axis).
  A session with no recorded timing (predating the column, or suppressed before the loop ran) SHALL be
  EXCLUDED from the A6b denominator and SHALL NOT be counted as an instant decision, and the exclusion SHALL
  be visible as the gap between the measured n and the window's incident count (REQ-2502). A window in which
  nothing was timed SHALL be published as a NAMED coverage gap, never as a zero-second median.
  The scope A6b does NOT cover SHALL be stated wherever it is published: the ingest→workflow-start leg is
  unmeasured, so time-to-decision is a LOWER bound on alert→decision, not the end-to-end figure.

- **REQ-2526** — [O] INV-22 · [R] a falsifiability rate whose denominator includes windows where the model
  made NO CLAIM is not a measurement of the model; it is a measurement of how often the estate was quiet,
  reported under the model's name.
  The G5 axis — the real causal graph against its own degree-preserving shuffled control — SHALL be published
  with the count of windows in which the model MADE A CLAIM as its denominator, and that denominator SHALL be
  printed beside the rate, never the bare percentage (REQ-2502's rule applied to this axis).
  `ControlScore.Ratio()` is `ControlTP / max(RealTP, 1)` and `Falsifiable()` is `Ratio() <= 0.5`, so a window
  in which the real arm found NOTHING and the control found nothing computes `0/1` and PASSES. Such windows
  SHALL be counted as NO CLAIM and EXCLUDED from the rate. Measured 2026-08-06 over 173 windows: 150 passed,
  123 of those had `real_tp = 0`, so the naive rate publishes 86.7% for a model that made a claim in 44
  windows and won 27 — the honest figure is 61.4%.
  The no-claim count SHALL remain visible rather than being filtered away: a model that makes no claim in 129
  of 173 windows is itself the finding, and a published axis that hides it presents silence as coverage.
  The real-vs-control TP sums SHALL be taken over CLAIMED windows only. A no-claim window in which the
  SHUFFLE scored (`real_tp = 0`, `control_tp > 0`) otherwise credits the control arm on a window the model
  never entered, and makes the shuffle look level with the real graph.
  The axis SHALL also publish the rate it would have reported had no-claim windows been counted as passes,
  computed from the MEASURED pass count rather than derived arithmetically — 6 of the 129 no-claim windows
  were not marked falsifiable, so `claimed_passed + no_claim` overstates the overstatement, and a figure
  quoted to expose a wrong number must itself be right.
- **REQ-2527** — [O] INV-22 · [R] two postures published as one number answer neither question. (TG-249 item
  3, 2026-08-06)
  Axis **A5** capability breadth counts the op-classes TG can heal autonomously. It filtered
  `policy_graduation.level = 'auto'` alone, so every class at the `auto_notice` rung was invisible — while
  acting without a vote the entire time, because `core/policy.Level.Verdict` gives BOTH rungs the `auto`
  verdict and applies the notice downstream as a band floor. The undercount was systematic rather than
  occasional: `auto_notice` is the MANDATORY intermediate rung every class holds before silent `auto`, so
  the classes omitted were precisely the newly-autonomous ones.
  Counting both rungs fixes the undercount and creates a second ambiguity: a class at `auto` heals silently,
  a class at `auto_notice` heals and pages a human, and one figure cannot say which. "How much can TG do"
  and "how much does TG do without anyone hearing" are different questions, and autonomy review turns on the
  second.
  Therefore A5 SHALL publish the autonomous breadth as BOTH rungs, AND SHALL publish the `auto_notice`
  SUBSET separately, so silent autonomy is never conflated with acts-and-pages. The subset SHALL be read
  from the ladder rather than derived by a consumer — which rung a class sits on is a fact the ladder owns —
  and silent autonomy SHALL be reportable as the difference. Both figures SHALL be emitted even at zero: a
  reader must be able to tell "none of it is silent yet" from "nobody computed the split".

- **REQ-2528** — [O] INV-19/INV-21 · [R] the guest being back is not the estate being back. (TG-226,
  2026-08-07)
  A device-down restore's discharge was established from `pct status` alone. That reads the GUEST, and a
  hard stop can return a guest whose applications come back with their downstream connection pools WEDGED —
  the process is up, the port is open, static responses are served, and every request that traverses the
  data path buffers until it times out. Found live 2026-07-31: a Node application's connection pool stayed
  wedged for ~5 hours after a device-down fault while its database was healthy, its liveness endpoint
  answered, and ICMP, device-status and `pct status` all read healthy for the whole outage. Because
  REQ-2508's close is PERMANENT, that restore would have been recorded as discharged and the guest's
  quarantine released while the application was still down.
  Therefore WHERE a fault class stops a whole guest, its discharge SHALL additionally be established from an
  OPERATOR-DECLARED probe that exercises the guest's primary service THROUGH ITS DATA PATH, and the guest
  check SHALL be evaluated FIRST so a stopped guest is never probed. The probe SHALL be declared per guest
  under REQ-2509's rule — never discovered, inferred, or compiled into a shipped artifact — and SHALL be
  executed as FIXED ARGV, never through a shell. A declaration that could not run as written (one carrying
  shell metacharacters, which fixed-argv execution would pass as literal arguments) SHALL be REFUSED AT
  DECLARATION TIME rather than treated as absent: an absent probe is a legitimate declaration meaning "no
  app-level check", whereas a malformed one is an operator who believes a check is in force when none is.
  A guest with NO declared probe SHALL NOT fail its restore — requiring one everywhere would strand an
  entire pool on first run — but the harness SHALL RECORD that the restore was verified at guest level only.
  *Rationale:* this is REQ-2508's own principle applied one layer in. A verifier that cannot report "there
  was nothing to check" is indistinguishable from one that checked everything, and the resulting record
  claims a coverage the run does not have.

- **REQ-2529** — [O] INV-22 · [R] an axis that has graded 2 of 3,371 sessions is not "passing"; it is silent,
  and a surface that publishes only its mean cannot tell the two apart. (TG-360, 2026-08-08)
  The two deterministic judge axes (`diagnosis_grounded`, `estate_grounded`) were built, calibrated and
  rubric-bumped twice, and had scored 2 and 1 of 3,371 sessions respectively. Both are correctly N/A-when-
  unknowable — a thin graph must stay silent rather than mark every diagnosis impossible — but the per-
  dimension MEANS ride the scorecard while judged/eligible was published NOWHERE, so on every surface a
  silent axis read identically to a working one. `estate_grounded scored 1/47 — no-relation-derived=46`
  existed in one worker log line and nowhere queryable.
  Therefore per-dimension judge coverage SHALL be published as an operational surface: for each DECLARED
  dimension, how many sessions in the window it SCORED (a `session_judgment` row exists — an N/A writes no
  row), beside the total DISTINCT sessions judged as the shared denominator (REQ-2502's rule applied to the
  judge's own axes). The DECLARED dimension set SHALL be emitted AT ZERO — a dimension that scored nothing
  publishes `scored=0`, never an absent series — because an absent axis and a healthy one are
  indistinguishable on a dashboard, which is the whole defect. The denominator SHALL count sessions, not
  judgments, or every per-axis ratio is wrong. A mean SHALL be emitted only for a dimension that scored at
  least one session: a mean over zero rows is a fabricated number.
  `core/db.AxisReadStore.JudgmentCoverage` is the read; the worker axis sampler publishes it, gated
  fail-quiet like the falsifiability axis so a coverage-read failure never silences the A1 numbers and vice
  versa. *Rationale:* the rubric's own doctrine is "no data is a problem, not everything passed", and until
  now it was not applied to the rubric's own axes.

- **REQ-2530** — [O] INV-22 · [R] breadth bought by bypassing the falsifiable core is drift wearing a
  capability's name.
  An auto-heal that ACTS without a committed prediction, or whose outcome the deterministic grader
  (`core/verify`) never graded, traded the differentiated core for raw A5/A3 breadth — the erosion the
  mission guardrail (TG-191, epic TG-187) exists to forbid. The harness SHALL publish a G6 axis — loop-
  bypassing heals — that counts, over the window, every executed action (an `action_execution` row) that has
  NO committed `infragraph_prediction` bound to its plan-adherence id OR no per-execution `core/verify`
  grade, and this count SHALL be 0. A positive count SHALL be SPLIT into its two limbs — acted un-predicted,
  and executed-but-ungraded — so the drift is NAMED rather than merely totalled, and SHALL be published
  beside the A5/A3 breadth axes it protects, because a rising breadth number that is really loop-bypass is
  not capability.
  The prediction join SHALL be by `action_id`, the content-addressed plan-adherence fingerprint INV-07
  threads UNCHANGED from the committed prediction to the execution; `action_execution` carries no `plan_hash`,
  so `action_id` (indexed on `infragraph_prediction`) is the sound and only key, taken as an EXISTS semi-join
  so the many executions of one recycled shape do not multiply against its single first-wins prediction row.
  The GRADE SHALL be read from the per-execution `action_execution.verdict`, NOT from a join to the first-wins
  `action_verdict` shape row: migration 0043 exists precisely because a re-cycled action shape would otherwise
  inherit its FIRST execution's verdict forever, so a later ungraded re-execution SHALL be judged on its own
  NULL verdict — the unverifiable, TG-182 fail-closed record of "we acted and could not prove it worked".
  A SEALED INVERSE — a structure-gated compensating rollback (TG-462) whose release is authorized by a human
  approval asserted against a durably sealed action identity rather than by a committed model prediction —
  SHALL NOT count against the acted-un-predicted limb: it commits no `infragraph_prediction` BY DESIGN (the
  interceptor's structure gate, not the prediction gate, is on its path), so counting it as loop-bypass would
  fire the guardrail on a compliant action. The seal is recorded on the row ONLY as a non-null
  `action_execution.inverts_action_id` (the interceptor persists the inverse reference but not the structure-gate
  flag), and TG-462's `RollbackWorkflow` is the SOLE producer of inverse rows and always structure-gates — so a
  non-null `inverts_action_id` is the sound proxy for a structure-gated inverse on today's estate; a future
  inverse producer that does NOT structure-gate SHALL re-gate this exclusion on a queryable seal rather than on
  the inverse reference alone. The exclusion SHALL remain NARROW along two axes: it excuses the PREDICTION limb
  ONLY — an inverse that executed but could not be graded SHALL STILL count against the ungraded limb — and it
  applies to inverse rows ONLY, so a forward action with no prediction (`inverts_action_id` NULL) SHALL STILL be
  flagged as a genuine loop-bypass. (TG-448, 2026-08-13.)
  A window with no executions SHALL report a clean zero DISTINGUISHED as "nothing to audit" rather than
  presented as a passing zero (REQ-2502's rule applied to this axis): absent is not a pass.
  `core/db.AxisReadStore.LoopBypass` is the read; `cmd/axisscore` renders it as the G6 line, gated fail-quiet
  like the falsifiability axis so a loop-bypass read failure never silences the other axes. *Rationale:* the
  existing anti-drift rule forces axis-naming; this ADDS that the axis it moves must not be BOUGHT by skipping
  the loop, and makes "don't erode the core" auditable rather than aspirational. (TG-191, 2026-08-11.)
