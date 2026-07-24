# Federation Vision — a network of sovereign instances sharing re-validated distillate

> **Status: GOVERNED NORTH-STAR (vision, not a task-bearing spec).** Federation is far-future and
> depends on prerequisites that are not yet built. This document is the honest capture of the
> destination and its locked design stance so the destination cannot drift. It **graduates to
> `spec/021-federation` once its prerequisites are met** (see § Dependency chain); until then it
> bears no `tasks.json`, no acceptance oracles, and no build obligation.

*Provenance tags follow the house three-layer model (see `00-README.md`): **[F]** foundation
inherited from the predecessor, **[R]** the single-organization product reframe, **[O]** the audit
hardening / invariants. Federation is a new **[R+O]** thesis layered strictly on top of the
inviolable core in `CONSTITUTION.md`; nothing here relaxes that core.*

---

## 1. Thesis / vision

The predecessor is a single instance: one governed-autonomy brain, watching one estate, learning
one estate's incidents. Territory Grounder's stated mission is to *reach **and exceed*** it. Reaching
it is faithful migration; the *categorical* exceed is a different shape entirely — **a network of
sovereign governed-autonomy instances, each running its own estate under its own constitution, that
share distilled, re-validated operational wisdom with one another.** No single estate, however large,
ever sees enough of the fault space to calibrate against the long tail; a federation does. The
north star is that a rare failure diagnosed and *verified-resolved* once, on one estate, becomes a
graded hint available to every other member — while every member's raw estate, its map and its
secrets, never leaves the building. That is the one-paragraph destination: **a federation of
sovereign instances sharing re-validated distillate.**

---

## 2. The CrowdSec precedent

This is not a speculative pattern. It already runs, proven, **inside TG's own estate**: CrowdSec is
an ingest connector TG consumes today (`docs/CONNECTOR-INVENTORY.md`). CrowdSec is the working
existence-proof of exactly the architecture proposed here, in exactly this domain (infrastructure
security telemetry):

- a **local engine** makes local decisions from local signal first;
- **opt-in, anonymized** sharing contributes local observations to a community pool — the operator
  chooses to participate, and what leaves is a minimized signal (an offending IP + scenario), not the
  estate's raw logs;
- members **consume the community pool** (the blocklist) as an input to local decisions;
- contributions are **reputation-weighted** — a signal's influence is a function of the contributor's
  track record, not merely that it was submitted;
- and the design carries explicit **anti-poisoning** machinery, because a shared security control
  plane is a supply-chain target by construction.

CrowdSec de-risks the federation idea: the hard parts (opt-in minimized sharing, a consumable pool,
reputation, poisoning resistance) have a shipped, adversarially-tested reference in TG's own problem
space. TG's federation is the *generalization* of the CrowdSec pattern from "block this IP" to "here
is a graduated diagnosis-and-remediation for this class of fault," under TG's stricter governance.

---

## 3. Why it is valuable

- **It kills cold-start.** Every new instance today boots with an empty corpus — the exact defect
  TG-125 (corpus-seed) exists to work around within a single estate, and the documented root cause of
  weak retrieval/reasoning in the TG-38 source benchmark. A federated member instead boots against a
  pool of re-validatable wisdom. The keystone problem — "a new brain knows nothing" — is solved
  structurally, not by hand-seeding each estate.
- **It covers the long tail.** Rare faults are, by definition, rarely seen by any one estate. The
  network sees the union of every member's incidents, so the tail a single estate would take years to
  encounter is available on day one — as a hint to be re-validated, never as an unchecked answer.
- **It turns a per-instance flywheel into a network flywheel.** TG's flywheel (skill-store
  generate → offline gate → A/B → graduate) improves one estate from its own traffic. Federation
  compounds it: a graduated artifact from any member becomes candidate wisdom for all members, and
  each member's re-graduation feeds evidence back. The improvement rate scales with the *network*,
  not the estate.
- **It accelerates calibration and graduation.** Graduation binds promotion to *verified* outcomes
  (`core/policy` graduation ladder). More verified outcomes, sourced across many estates, means faster,
  better-calibrated promotion of an op-class from never-auto toward auto — without any single estate
  having to manufacture the volume of evidence alone.

---

## 4. The design stance (LOCKED)

These five principles are the owner-approved, locked framing. They are enumerated crisply so they
can be lifted verbatim into `spec/021` requirements. They are subordinate to, and never override,
`CONSTITUTION.md` §3 (the inviolable mechanical safety core).

**4.1 Share the DISTILLATE, not the trace — a two-layer split.** Every unit of knowledge in TG is
cleaved into two layers, and only one of them is ever shareable:

- the **estate-specific layer** — hostnames, IPs, topology, credentials, ticket bodies, raw decision
  traces, the literal `action_id`-keyed record of what ran where — **NEVER leaves the instance.** The
  raw trace is a map of the estate (see § 6, hazard a); it is estate-sovereign data.
- the **generalizable wisdom layer** — the abstracted tuple *(alert-rule-**class** → diagnosis →
  resolution **op-class** → VERIFIED outcome)*, plus graduated skills / runbooks / rubrics — i.e. the
  **loadable-prose layer**. This is the shareable unit. It names *kinds*, never *instances*: "a BGP
  session-flap class on an edge-router role, diagnosed as X, resolved by restart-service op-class,
  verified `match`," never "restart bgpd on dc1edge03."

This split must be **built into the trace schema itself**, not bolted on at export time — the
generalizable layer is a first-class projection of the decision record, so that "share" can only ever
read the shareable projection and is *incapable* of reading the estate-specific one. (This is why the
decision-tracer archive, spec/020, is the keystone prerequisite — see § 7.)

**4.2 The shared unit is a GRADUATED artifact, RE-VALIDATED on each consumer — "federated
graduation."** Only artifacts that have already earned local trust via the flywheel (generate →
offline gate → A/B → graduate) are eligible to be shared. On import, a foreign artifact does **not**
inherit the exporter's trust. It re-enters the *consuming* estate's full graduation ladder and must be
**re-graduated against local traffic and local verified outcomes** before it earns any local standing.
Trust is never transferred across the boundary; it is *re-earned* on every estate. This is federated
graduation, and it is the reason poisoning cannot propagate as authority (§ 6, hazard b).

**4.3 Imported wisdom is SUBORDINATE, never AUTHORITY.** A federated artifact is, at most, a **hint
to the agent** — additional retrieval context, a candidate skill, a suggested op-class. It has **no
privileged path** and cannot short-circuit a single gate. It still passes, unchanged, the entire local
gate stack: the constitutional **never-auto floor** (`CONSTITUTION.md` §3.2, INV-09), the per-action
**policy verdict** (spec/015), the **mode chokepoint** (Shadow/Semi/Full, §3.8), the fail-closed
**prediction gate** and **mechanical verdict** (§3.4, INV-10), and **local verify** after any effect.
**The local constitution is sovereign.** A member's own inviolable core adjudicates every action
regardless of any imported hint's provenance or reputation. This is the primary anti-poisoning
defense: even a maliciously crafted, perfectly-reputed artifact can only *propose*, and a proposal
still has to survive a gate stack that trusts no model output — imported or not (INV-08).

**4.4 Reputation is weighted by VERIFIED-OUTCOME quality, not volume — and consumption is never
gated behind over-sharing.** A contributor's influence is a function of how often its shared artifacts,
when re-graduated elsewhere, produce *verified* (`match`) outcomes — not how many artifacts it dumped.
Volume earns nothing. Crucially, **consumption is never gated behind contribution**: a member that
shares little or nothing (because its estate is sensitive) is never throttled or penalized on what it
may consume. There is no seed-to-leech ratio and no pressure to over-share (§ 6, hazard c).

**4.5 The federation shape is AUTHENTICATED members + SIGNED/attested contributions + reputation —
NOT a public dump.** The default federation is a closed set of **authenticated** member instances;
every contribution is **cryptographically signed and attested** to its origin and its local
graduation evidence; and reputation rides on that verifiable identity. A *public* tier may exist
**only** for distillate that is provably zero-estate-specific (pure loadable-prose rubrics/runbooks
with no estate coupling), and even then it enters consumers as a subordinate hint under federated
graduation like any other.

---

## 5. The three hazards + mitigations

Federation of an infrastructure control plane is genuinely dangerous, and this section states the
three hazards plainly so no reader mistakes the vision for a free lunch. Each maps to a locked
principle above.

**(a) Anonymizing a control plane's telemetry is brutal — the trace *is* a map of your estate.**
An incident trace encodes topology, naming, addressing, dependency structure, and operational
posture; robust anonymization of that is an unsolved, high-stakes problem, and a botched
anonymization leaks the estate. **Mitigation: don't try to anonymize the trace — never share it.**
Share only the generalizable *(class → diagnosis → op-class → verified-outcome)* distillate, cleaved
off at the schema level (§ 4.1). The estate-specific layer has no export path at all, so there is no
anonymization to get wrong.

**(b) Poisoning is a supply-chain attack on the control plane's *brain*.** A hostile contributor's
goal is to get a bad diagnosis or a destructive op-class adopted as trusted automation across the
network. **Mitigation: subordinate-not-authority (§ 4.3) + local re-graduation (§ 4.2) + reputation
(§ 4.4).** Imported wisdom can only *hint*; it must be re-graduated against the consumer's own verified
outcomes before it earns any standing; and it can never clear the local never-auto floor or policy
verdict on the strength of its origin. A poisoned artifact that cannot produce local `match` verdicts
never graduates locally and never gains reputation — the attack fails closed on every consuming
estate independently.

**(c) Torrent-style seed-to-leech has a perverse incentive.** Any "you must contribute to consume"
rule pressures sensitive-estate operators to over-share exactly the data they must not. **Mitigation:
reputation-by-verified-outcome with no over-share pressure (§ 4.4).** Consumption is decoupled from
contribution; influence is earned by outcome quality, not volume; and there is no ratio to game. The
safe choice (share little, share only clean distillate) is never punished.

---

## 6. Governance / legal / trust

- **Opt-in, org-admin-level, DEFAULT-OFF.** Joining a federation, exporting anything, and consuming
  anything are each an explicit, authenticated, org-admin-authority action, audited to the ledger —
  never a default, never a per-user toggle. A fresh instance federates *nothing* until an org admin
  deliberately turns it on. (This mirrors the constitution's autonomy-disabled-by-default stance,
  §3.8, applied to the federation boundary.)
- **Data residency & compliance.** Because only the generalizable distillate can ever leave, and the
  estate-specific layer is export-incapable by construction (§ 4.1), the residency question narrows to
  the distillate itself. Even so, export destination, retention, and jurisdiction are org policy;
  every shared artifact carries a declared retention and provenance like any other governed record
  (INV-14). Members in regulated environments may run share-nothing (consume-only) or federate-nothing
  and lose no local capability.
- **Who runs the coordinator.** The federation needs a coordinator (member directory, signed-artifact
  distribution, reputation ledger). Its trust model must be explicit: it distributes and attests, it
  does **not** adjudicate any member's actions — a member's own constitution is sovereign (§ 4.3), so a
  compromised coordinator can at worst offer bad hints, which still die at each local gate stack. The
  coordinator holds no effect authority over any estate.
- **Un-deletability of shared artifacts.** Once distillate is shared to the network it must be treated
  as **unrecallable** — a member cannot guarantee retraction from every peer's local store. This is a
  first-order reason the share decision is high-authority, default-off, and constrained to
  zero-estate-specific distillate: the export gate is the last point of control, because there is no
  delete afterward. This property must be stated to the org admin at the moment of opt-in.

---

## 7. Dependency chain (why this is far-future)

Federation is deliberately *not* on the near roadmap. It sits on top of a stack of prerequisites that
are themselves incomplete. It graduates to `spec/021` only when these are met:

1. **The decision-tracer archive (spec/020) — the keystone.** Federation shares projections of the
   decision record; if the signal is not durably persisted with the two-layer split *in its schema*,
   there is nothing honest to project. Spec/020 (persist-the-signal) is the foundation the entire
   thesis stands on.
2. **The two-layer split built into the trace schema.** Not an export-time filter — a first-class
   estate-specific vs generalizable cleavage in the persisted record (§ 4.1), so "share" is
   *structurally incapable* of reading estate-specific data.
3. **The flywheel actually graduating an artifact — which has never happened yet.** Federated
   graduation (§ 4.2) shares *graduated* artifacts and re-graduates them on consumers. If the local
   flywheel has never completed one full generate→gate→A/B→graduate cycle, there is no graduated unit
   to federate and no re-graduation machinery to reuse. Proving one local graduation is a hard
   precondition.
4. **Loadable-not-hardcoded artifacts — 4/5 are still Go literals.** The shareable unit *is* the
   loadable-prose layer (prompts, skills, op-class/tool schemas, model-tiers, rubrics, knowledge). Prose
   that is compiled into the binary cannot be exported, versioned, signed, or re-graduated. Until the
   loadable-not-hardcoded work lands (TG-114/116/125 + the tool-schema issue), there is nothing
   portable to share.

Only with (1)–(4) in place does federation become buildable rather than aspirational.

---

## 8. Phased roadmap

The roadmap is intentionally slow and starts with the export path **dormant**. Each phase is a
gate; nothing advances until the prior phase is proven.

- **v1 — LOCAL-ONLY (dormant export).** The two-layer split exists in the trace schema; the
  generalizable projection is computed and stored; **nothing is shared** and no export path is wired.
  This is the honest first step: build the *shape* of federation with the door closed. It delivers
  value locally (clean separation, a portable distillate projection) with zero federation risk.
- **Phase F1 — signed export.** An org admin can export the zero-estate-specific distillate as
  signed, attested artifacts to a file/registry. Still no live network; export is auditable and
  opt-in.
- **Phase F2 — authenticated federation.** A closed set of authenticated members exchange signed
  artifacts through a coordinator. Consume-with-**federated-graduation** (§ 4.2): imported artifacts
  are subordinate hints that must re-graduate locally.
- **Phase F3 — reputation.** Verified-outcome-weighted reputation (§ 4.4) rides on member identity;
  consumption remains decoupled from contribution.
- **Phase F4 — consumption at scale + optional public tier.** Mature consumption ergonomics and, only
  if warranted, a public tier restricted to provably zero-estate-specific distillate (§ 4.5).

> **This document graduates to `spec/021-federation` once the § 7 prerequisites are met.** At that
> point the § 4 principles become EARS requirements, the § 8 phases become the `tasks.json` DAG, and
> the § 5/§ 6 hazards and governance become STRIDE entries and acceptance oracles.

---

## 9. Relationship to the mission

The mission is to *reach **and exceed*** the predecessor. Reaching it is faithful migration of a
single battle-tested instance. Federation is the **apex of the exceed**: it is the one capability the
predecessor structurally cannot have — a single absent-operator homelab is, by construction, a single
instance. A network of sovereign governed-autonomy instances compounding re-validated wisdom is not a
better homelab; it is a categorically different thing, and it is the furthest point on TG's
"reach-and-exceed" axis. Everything before it — the tracer, the flywheel's first graduation, the
loadable-prose migration — is also independently valuable, which is why the dependency chain is a
feature: TG earns federation by finishing the work that makes TG excellent on a single estate first.

---

## Prior art & differentiation

*STUB — to be completed.* A survey of prior art (federated learning, threat-intel sharing including
the CrowdSec reference, gossip/reputation systems, supply-chain-integrity models for shared artifacts)
and TG's differentiation lives in **[`PRIOR-ART-tracer-and-federation.md`](PRIOR-ART-tracer-and-federation.md)**
(produced in parallel). This section links out rather than duplicating it; no competitor claims are
made here.

---

## Appendix — provenance quick map

- **[F] foundation:** the flywheel (generate → offline gate → A/B → graduate) and verified-outcome
  graduation ladder inherited from the predecessor; confidence/verify/verdict semantics; the
  loadable-prose disposition.
- **[R] reframe:** federation itself as a single-org-to-network capability; opt-in org-admin
  default-off boundary; the CrowdSec-pattern generalization; the exceed-the-predecessor apex framing.
- **[O] overlay:** the two-layer estate-specific/generalizable schema split; federated graduation
  (re-earned local trust); subordinate-not-authority under the full local gate stack (INV-08, INV-09,
  §3.4/§3.8, spec/015); reputation-by-verified-outcome; signed/attested contributions; un-deletability
  as a first-order export constraint; export-incapable-by-construction residency posture (INV-14).
