<!-- spec/012 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/012 — Design: read-only Runner Temporal workflow

How the requirements in `requirements.md` are realized on the Go / Temporal stack. Where this design and
the code disagree, the code is the bug and this document is the intent. The predecessor logic
re-expressed here is the n8n Runner/session-orchestrator workflow set; none is vendored — it is
re-implemented as a native Temporal workflow over the governed Go primitives per
[`docs/PORTING-GUIDE.md`](../../docs/PORTING-GUIDE.md).

## Components

- **`runner.RunnerWorkflow`** (`temporal/runner/workflow.go`) — the deterministic orchestrator. It
  sequences the activities and stops at propose. It contains control flow only; the only pure
  computation it performs inline is deriving the content-hashed `action_id` (a deterministic SHA-256,
  safe under Temporal replay). It never calls the OS (REQ-1102).
- **`runner.Activities`** (`temporal/runner/activities.go`) — the activity implementations, closing over
  injected `Deps` so the Temporal test env drives them with in-memory fakes:
  - `SuppressActivity` runs the tier-1 suppression chain (spec/005) as the Runner's FIRST gate: a declared
    maintenance/chaos freeze, a known transient, or a duplicate must NOT spend a triage session, so the
    workflow stops early (`Outcome = "suppressed"`, read-only, no proposal) BEFORE any model spend. The chain
    is injected as an interface (`Deps.Suppress`) so the oracle can drive a plain `*suppression.Chain` and
    production the `LiveSuppressGate`. The gate composes three stages in spec/005 order: the config-not-code
    stateless operator-curated ones — declared freeze windows (`TG_SUPPRESSION_FREEZE_FILE` — an alert inside
    an active, in-scope window is suppressed even at critical severity because the operator declared it
    coming) and active-memory suppress-rules (`TG_SUPPRESSION_RULES_FILE` — `path.Match` host/rule globs, with
    a catch-all `*`/`*` refused so a config slip cannot silence the whole estate, critical/unknown never
    suppressed by a rule) — PLUS a live DEDUP stage. Because the dedup stage's anchor set is a fixed slice,
    the `LiveSuppressGate` re-assembles the chain PER INCIDENT, supplying the current entries from an
    in-memory, concurrency-safe, time-windowed `RecentTriageLog` (`TG_SUPPRESSION_DEDUP_WINDOW`) and recording
    each triage back into it — so a re-fire of the same `(host, alert_rule)` within the window (a DIFFERENT
    `external_ref` than the workflow-level reject-duplicate catches) is deduped without a second session. When
    a tracker is wired the dedup holds only WHILE the anchor incident is still open (its ticket not resolved,
    via `tracker.Read`) — a re-fire after the parent resolved is a genuine new incident and escalates, never
    silently deduped; a tracker read error fails open to that (investigate). The
    log is best-effort by design: per-worker, evicted past retention, fail-open (forgetting an anchor costs at
    most one extra session, never a missed real incident), so a durable shared log can replace it behind the
    same seam. It also runs the operator-declared KNOWN-TRANSIENT stage (`TG_SUPPRESSION_PATTERNS_FILE` —
    `alert_rule` + estate + confidence; a DECLARED pattern has no recency gate, but the stage still requires
    confidence ≥ 0.7 AND a transient-nature keyword in the rule, so a standing fault like `DiskFull` is never
    auto-suppressed). It also runs the operator-declared RECURRING REBOOT-SCHEDULE stage
    (`TG_SUPPRESSION_SCHEDULES_FILE` — host + cron + timezone + validity window, registered LIVE because an
    operator declaration IS the authorization, no observe-before-live): a reboot-class alert (a narrow
    host-down/reboot/restart/unreachable classifier on the rule) on that host inside the DST-correct cron
    window (the asymmetric `[fire − TG_SUPPRESSION_REBOOT_PRE_BUFFER, fire + TG_SUPPRESSION_REBOOT_WINDOW]`,
    default `[−5m, +10m]`) is suppressed, while a NON-reboot alert on the same host in
    the same window is investigated (the classifier keeps a real incident from being swept up by a reboot
    schedule). Finally it runs the operator-declared BLAST-RADIUS FOLD stage (`TG_SUPPRESSION_FOLDS_FILE` —
    host/rule scope + validity window): a matching CHILD alert is folded (posted as a notice, no session)
    while the policy is valid. An operator-declared fold has no learned staleness failure mode, so it is
    verified-at-load with effectively-infinite freshness — only its valid window gates it — and a catch-all
    (`*`/`*`) is refused so a config slip cannot fold the whole estate to notices. This completes all six
    tier-1 stages from operator config; the self-learning variants (scheduled-reboot DISCOVERY, the
    blast-radius suppression-proposal lane) are the spec/005 Phase-C overlay on top of these declared ones.
    Fail-open
    throughout: no gate wired, or a gate error, yields "not suppressed" so
    a suppression fault never silences a real alert; each decision is hash-chained into the governance ledger
    (INV-19).
  - `InvestigateActivity` runs the read-only agent loop (spec/011) and returns the proposal. Its seed is
    composed by the behavioral skill library (`agent/skills`) — the ported competence layer (proving-your-
    work, the debugging protocol, the conservative situation→action catalog) — selected deterministically by
    the execution class (INV-08: a pure selector, no model token chooses which skills load), so a cheap
    incident does not carry the heavyweight protocols a deep one needs. The seed also carries retrieval-
    augmented precedent: when a `knowledge.Retriever` is injected (`Deps.Retriever`, a config-gated corpus via
    `TG_KNOWLEDGE_FILE`), the most relevant prior resolved incidents are rendered into the seed as DATA
    (framed as precedent, never instructions — the agent still reasons and the gate still decides); empty when
    no corpus is wired. The corpus is held behind a `knowledge.Holder` and reloaded at runtime
    (`TG_KNOWLEDGE_REFRESH_INTERVAL`, off by default) so an operator or the lessons feed appending a resolved
    incident takes effect without a restart; a parse error keeps the last good corpus. That lessons feed is
    the persistence hop that closes the learn→retrieve loop: a resolved-incident source (`TG_LESSONS_SOURCE_FILE`
    — today an operator export, in Phase 2 the close-out path) is distilled to its CONFIRMED-CLEAN subset
    (`core/lessons.Merge` over `core/lessons.Distill`: a `match` verdict AND a confirmed-clear condition, so a
    deviation/partial/unconfirmed outcome never becomes precedent) and merged by `external_ref` into the same
    `TG_KNOWLEDGE_FILE` corpus the retriever reloads. The read-merge-write is serialized by a mutex and lands
    the corpus atomically (temp-file + rename, so the reload loop never sees a torn corpus), runs once at boot
    and on `TG_LESSONS_REFRESH_INTERVAL`, and is a no-op unless the feed contributes a net-new confirmed-clean
    lesson — so a verified resolution becomes citable precedent for the next similar incident without a restart.
    On top of the lexical channel sits the SEMANTIC channel (REQ-1110/REQ-1111, TG-40 / TG-38 audit R1 — the
    pgvector plane deploy provisioned but nothing used): when `TG_EMBED_MODEL` names an embedding model the
    LiteLLM gateway serves (`adapters/model.Embedder` over the OpenAI-compatible `/v1/embeddings`) and a
    durable store exists, the worker wraps the Holder in a `knowledge.FusedRetriever` — the query is embedded,
    matched cosine-top-K against `knowledge_embedding` (migration 0013: a NULLABLE `vector(768)` sidecar keyed
    by `external_ref` + a content hash, HNSW cosine index; the corpus FILE stays the source of truth for
    precedent content), thresholded (`TG_EMBED_MIN_SIMILARITY`, default 0.5, so a weak nearest-neighbor never
    fabricates precedent), resolved against the LIVE corpus (a stale index ref can never resurrect removed
    precedent), and fused with the lexical ranking by Reciprocal Rank Fusion (k=60, deterministic
    `external_ref` tie-break) — a precedent ranked by BOTH channels outranks single-channel hits, and every
    fused Hit keeps the explainable reasons ("same alert rule; semantic similarity 0.83"). The write path is
    best-effort and bounded: each corpus load/reload/lessons-merge triggers a background `knowledge.SyncIndex`
    (upsert refs; a changed content hash NULLs the row's vector so it re-embeds; vanished refs are pruned)
    plus a `knowledge.Backfiller` pass, and a sweep every `TG_EMBED_BACKFILL_INTERVAL` (default 10m, batch
    `TG_EMBED_BACKFILL_BATCH`=64) embeds rows still NULL — hash-guarded writes, so a vector is only ever bound
    to the exact text it was computed from, and `TG_EMBED_DIM` must equal the migrated column's dimension or
    the worker refuses to boot. The fallback contract is total (REQ-1111): no model configured, no store, an
    embed/search failure or timeout, or zero above-threshold matches ⇒ EXACTLY the lexical result for that
    query (logged; disabled states are logged once at boot) — the semantic channel only ever adds recall,
    never gates retrieval, and mutation posture is untouched (this plane reads and ranks; it actuates nothing).
    The seed ALSO carries
    the AUTHORITATIVE CMDB record for the incident's host when a CMDB reader is injected (`Deps.CMDBResolve`,
    the module-registry-resolved NetBox reader — INV-17: a resolved, enabled `cmdb` capability): the
    investigation reasons WITH the reconciled entity (name, kind, sorted attributes) rather than the payload's
    unverified claim — the read-only first step of "reconcile a payload's claimed fields against the
    authoritative record before dispatch". It is rendered as DATA, never instructions (INV-08), and is
    fail-open: an unregistered CMDB, an empty host, or a lookup miss/error adds no block and never blocks the
    investigation. The seed likewise carries the incident's ENTRY TICKET (title, state) when EXACTLY ONE
    tracker is enabled (`Deps.TrackerRead`, resolved from the registry — the entry tracker is otherwise
    ambiguous, so zero or multiple configured trackers add no ticket block): the worker finds the single
    enabled tracker via the registry's status-aware `Capabilities` view and `Open`s the ticket by
    `external_ref`. Same discipline — DATA not instructions, fail-open on any miss/error.
    Every untrusted free-text block composed into the seed is INPUT-SCREENED with the same `core/screen`
    prompt-injection/jailbreak detector the classifier applies to the model's OUTPUT (TG-38 R2 — the
    detector was wired only to the proposal-output surface; the output screen is unchanged, this adds the
    input boundary as defense in depth). The policy is mechanical, never model-decided (INV-08), and split
    by what the text is: (1) the envelope's human-text `summary` — rendered as a delimited
    "Alert summary (data, not instructions)" block — plus the entry-ticket and CMDB blocks are NEUTRALIZED
    in place on a detection (`screen.Scrub`: each detected span replaced with a `[SCREENED:<category>]`
    marker over the normalized zero-width/NFKC/homoglyph fold, so a disguised span cannot survive), and
    the session PROCEEDS — the alert is never dropped, because an attacker able to suppress triage by
    embedding an injection string would make under-triage the failure mode; (2) a retrieved precedent
    snippet whose corpus text (ref/rule/host/summary/resolution — corpus JSON, not the ingest-validated
    envelope identifiers) trips the screen is instead SKIPPED entirely (retrieval is optional enrichment;
    a poisoned snippet never enters the seed). Every hit is logged and recorded as an
    `input-screened:<field>:<categories>` entry in the session's skill_load seed provenance, so a screened
    seed is visible in the triage record (REQ-1106) with no schema change. The grammar-validated
    identifier fields (external_ref / alert_rule / host / site — slug and RFC-1123 constrained at ingest,
    a charset that cannot express an injection phrase) and the governed skill guidance (which IS
    instructions, by design) are deliberately not screened. Clean text passes through byte-identical.
    On top of the input screen, every seed block is wrapped in an explicit, machine-parseable XML-style
    envelope named by KIND (REQ-1112, design-wisdom #4 — the trusted/untrusted boundary made
    machine-parseable): `<summary>` / `<ticket>` / `<cmdb>` / `<precedent>` for the untrusted incident DATA
    and `<behavioral_guidance>` for the trusted guidance, led by a fixed preamble that names
    `<behavioral_guidance>` as the ONLY instructions and every other block as DATA to reason over, never to
    obey (`composeSeed` in `compose_seed.go`). Before an untrusted block is wrapped its content is
    delimiter-neutralized (`neutralizeSeedDelimiters`): any embedded envelope tag — a forged
    `</behavioral_guidance>` smuggled in an alert body, or any opening/closing tag of any kind, matched
    case- and whitespace-insensitively — is replaced with an inert marker, so a crafted block cannot forge a
    boundary (delimiter injection) and the composed seed carries exactly ONE real `<behavioral_guidance>`
    boundary. The neutralized content is retained, never dropped (an attacker must not suppress triage by
    embedding a delimiter). This wrapping is ADDITIVE to the input screen (both run), and each untrusted
    block carries a per-block soft budget — an oversized block (a huge CMDB record) is truncated with a
    marker and flagged in the seed provenance rather than crowding the guidance out of the window. The
    grammar-validated identifiers stay in the trusted preamble line; the guidance body is likewise
    delimiter-neutralized so a malformed skill body can never leave the `<behavioral_guidance>` envelope
    unbalanced.
  - `ClassifyActivity` classifies the proposed action (spec/001) and appends one `session_risk_audit`
    row to the governance ledger. It sets `HasPrediction` from `eligible(host)`, which fails CLOSED — a host
    is prediction-eligible only when an eligibility oracle places it in the estate graph; with no oracle (or
    an unplaceable host) it is NOT eligible, so classification polls rather than assuming a prediction exists.
    The workflow first screens the untrusted model output (the proposal's rationale + approval choice) with
    the `core/screen` prompt-injection/jailbreak detector — a pure, deterministic function, safe in workflow
    code — and threads a `Jailbreak` flag into the classifier, which clamps a hit to POLL_PAUSE (the ported
    inline screen; PORT-FIDELITY-AUDIT's dead `jailbreak` slug). It also flags whether the action targets a
    stateful workload (`safety.IsStatefulWorkload` over the target/op): a reversible action on a stateful
    target is mapped to reversible-MIXED, not fully reversible, which is what lets the classifier's
    stateful-workload gate fire and clamp the mutating stateful op to POLL_PAUSE. A fully-reversible mapping
    (the bug) left that gate unreachable — `rev` never became reversible-mixed — so a reversible stateful
    mutation (e.g. an etcd rollout-restart) silently AUTO-resolved; the predecessor's stateful denylist blocks
    exactly that carve (PORT-FIDELITY-AUDIT). It also flags whether the ACTUAL op is destructive
    (`safety.IsDestructiveOp` over the op + rationale) so a model that under-declares its op_class cannot hide
    a mutation. It also derives the
    alert category from the normalized `category` provider label and threads `safety.HighRiskCategory` so a
    maintenance / security-incident / deployment session is polled by default (the category-high-risk band
    driver, PORT-FIDELITY-AUDIT P2-22) — safe-direction only, an absent category adds no clamp. It derives
    `CriticalityTier` from the injected `Deps.CriticalityTier(host)` (an operator-declared P0-host set,
    config-not-code via `TG_CRITICALITY_TIER_HOSTS` — no hostnames in code) so a criticality-tier host is
    ceilinged at AUTO_NOTICE and never silently AUTO (PORT-FIDELITY-AUDIT P1-16); a nil set fails safe (no
    host is P0, the predecessor default). The estate graph supplies this attribute directly once the
    topology readers land, at which point the env set becomes the seed/override. It derives `BlastRadiusWide`
    from the injected `Deps.BlastRadiusWide(host)` — the host's predicted estate blast radius width against an
    operator-declared threshold (`TG_BLAST_RADIUS_WIDE_THRESHOLD`, default 8, config-not-code) — so a wide
    predicted cascade ceilings at AUTO_NOTICE and pages the on-call in parallel (PORT-FIDELITY-AUDIT P1-16);
    an empty estate makes no host wide (fail-safe), and it goes live as the topology readers seed the graph.
    It derives `NovelIncident` from the injected `Deps.PriorIncidents(host, alert_rule)` — a class is novel
    only when the prior count is POSITIVELY established as zero (known AND 0); an unknown count (nil oracle / no
    knowledge store) never fires, so missing data never invents a poll (PORT-FIDELITY-AUDIT P1-16, the
    predecessor's positively-established-novelty rule). At the composition root the worker wires
    `PriorIncidents` from the knowledge corpus (`knowledge.Holder.Count` — the exact (host, rule) prior-incident
    count over the same prior-resolved-incident history the retriever ranks over): a loaded corpus makes the
    count KNOWN, so a never-seen signature (count 0) forces the poll; no corpus leaves the oracle nil (novelty
    unknown, the gate inert — fail toward the existing behavior). It threads the silent-cognition guard (always active —
    INV-11 is non-negotiable): `AutoResolveMarked` when the proposal carried an [AUTO-RESOLVE] marker (parsed
    from the model's approval_choice as DATA, never authority — keyed on the marker itself, NOT on whether any
    evidence id happened to be cited), and `Evidence` built by binding each cited id against the
    orchestrator-captured tool results — a ref is admissible only when its capture also SUCCEEDED and is
    TARGET-RELEVANT (its output names the action's host). A marked AUTO-RESOLVE that binds no evidence — a
    hallucinated id, a failed tool, an off-target result, OR zero cited ids at all (the silent-cognition case) —
    has its AUTO-RESOLVE stripped and is polled (REQ-008). Deriving the marker from the count of cited ids left
    the zero-evidence case blind, sailing a silent auto-resolve through to AUTO (PORT-FIDELITY-AUDIT). It also derives `SelfProtectedRestart` —
    `safety.IsRestartClass(op, op_class)` AND the injected
    `Deps.SelfProtectedService(host+op)` (an operator-declared control-plane service set, config-not-code via
    `TG_SELF_PROTECTED_SERVICES`) — so a restart of the platform's own services is vetoed to POLL_PAUSE
    (PORT-FIDELITY-AUDIT P2-21). Both halves are server-side; the model cannot declare its way out. A nil set
    protects nothing (the floor/stateful/destructive gates still govern the action).
  - `GateActivity` commits the machine prediction and seals the `ActionManifest` (spec/002), returning
    the serializable binding facts (the in-process `GatedProposal` never crosses the activity boundary). When
    a `Deps.ManifestSink` is wired (the pgx `db.ManifestStore`) it durably records the sealed content-hashed
    manifest — even under mutation OFF the Runner seals it, so the immutable action binding (INV-07) is
    available for cross-session audit; a record failure fails the gate CLOSED (the authorization must be
    durable). Nil ⇒ the in-memory oracle path.
  - `NotifyActivity` delivers the governance notice to the human channel for the **notice/poll bands** —
    AUTO_NOTICE **pages** on-call, POLL_PAUSE solicits an **approval poll** — realizing "AUTO_NOTICE/POLL_PAUSE
    route to the configured on-call group". The notice is DATA (the backend redacts credentials/PII; INV-08 —
    never interpolated as control) bound to the incident's `external_ref` so a vote can answer exactly that
    decision (INV-12). Paging is the Phase-0/1 human-in-the-loop channel, NOT an estate mutation — it is never
    gated by the `MutationGate`. Delivery is FAIL-OPEN and fire-and-forget: a nil notifier (none wired, or more
    than one enabled — ambiguous for a single bound decision) or a delivery error yields `Delivered=false` and
    never fails the Runner (a notifier outage must not kill triage). AUTO is the silent lane (no page). The
    vote-consuming resume (a `workflow.Await` on the vote signal that gates execution) is Phase 2; here the
    notice is delivered but not awaited, so the read-only Runner still stops at propose. The single enabled
    notifier is resolved from the registry at the composition root (`Deps.Notify`).
  - `ExecuteActivity` / `VerifyActivity` are present but **no-op while mutation is off** — they assert
    the `MutationGate` and return `Executed=false` (REQ-1101). This is the Phase-2 seam.
    - **Effect dispatch routes through the Actuation Regime Engine (spec/017 REQ-1700/1702).** When the
      composition root wires `Deps.RegimeEngine` + `Deps.LaneEffect`, `ExecuteActivity` resolves the effect
      lane and drives that lane's UNEXPORTED leaf. **Lane selection is effect-kind-aware:** an op-class whose
      `effect_kind` names a specific channel (`awx-launch` → the awx-job regime; `proxmox-lifecycle` → the
      proxmox regime — via `effectKindRegime` → `RegimeEngine.LaneForRegime`) routes by that KIND regardless of
      the target host's management regime (a guest is native-ssh for a service restart but proxmox-mediated for
      start/stop); every other (target-managed) op routes by the target via
      `RegimeEngine.SelectLane(credential.Target{Host:
      TargetHost})`. Either way a resolved-but-unwired / ambiguous / unmatched lane FAILS CLOSED (refused,
      `Executed=false`). The selected lane's UNEXPORTED effect leaf is driven through a freshly-built spec/013
      interceptor via `LaneEffect.Apply` — the SAME
      admission → never-auto floor → policy → credential → mode-chokepoint → execute → verify chain (the
      composition seam is the only path to any lane's effect, so a routed lane is never a weaker path than the
      direct one). A resolved-but-unwired / ambiguous / unmatched-with-no-default target FAILS CLOSED (refused,
      `Executed=false`) and never falls through to a direct leaf. Both fields nil ⇒ the single native-ssh
      `Interceptor.Do` (behavior-preserving); the default lane IS native-ssh, so an SSH target routes to the
      identical effect chain it used before the wiring.
    - **The sealed EFFECT is shaped by the op-class's effect kind — `sealEffect` (spec/013 opschema, TG-139).**
      Before the Request is built, `sealEffect(Deps, Action, targetHost)` turns the sealed op-class + its
      structured params into the interceptor Request's `Argv` PLUS an optional `Stdin`, branching on the
      op-class's declared `effect_kind` (an EXACT opschema lookup, never a model-token branch — INV-08): an
      ARGV-ENCODED class (`ssh-argv` or `proxmox-lifecycle`, e.g. `start-guest` → `[start, <guest>]`) yields
      exactly `sealedArgv` (a fixed argv, no stdin — the proxmox one just routes to the proxmox lane by kind);
      an `awx-launch` class yields
      `Argv=[LaunchVerb]` + `Stdin=`a JSON `LaunchSpec` (`awxjob.EncodeLaunch`) whose template id comes from the
      operator's op-class→template config (`Deps.AWXTemplateForOpClass`, config-not-code — the worker inverts
      `TG_AWXJOB_ALLOWLIST` fail-closed on an empty/ambiguous binding), whose `extra_vars` are the typed params,
      and whose run `limit` is the incident target host. Every failure path (unregistered class, no template
      bound, encode error) yields an EMPTY argv, which the effect leaf refuses (INV-02/INV-09) — so an awx op
      NEVER actuates without an operator-declared template, and mutation stays gated by the identical interceptor
      chain. This seam only SHAPES the effect; it authorizes nothing (the awx-job leaf re-validates the template
      against its own allowlist + op-class binding at Exec — authoritative, defense in depth).
- **`cmd/worker`** — registers the workflow and activities on the `tg.runner` task queue and refuses to
  start if mutation is enabled. At boot it also populates the runtime module registry from the built
  connector fleet via `modules/bootstrap` (the composition root — the registry shipped unpopulated, so
  INV-17/INV-18 were enforced only in acceptance tests): registration fails closed on a duplicate
  (surface, source) pair (INV-18), an empty capability manifest refuses startup, and the declared
  capability set is logged. The config-free families declare unconditionally (model-provider descriptors the
  LiteLLM gateway fronts, the ingest push-receivers, the config-free observability exporters); the
  estate-mutating actuation family is declared but registered DISABLED, so INV-17 gives it no execution path
  in Phase 0/1. The config-driven families declare from config-not-code per `TG_<SOURCE>_URL` / `_TOKEN_REF`
  — a source is a capability only where its endpoint is declared; credentials are secret references, never
  literals: the issue trackers, the notifiers (each carrying its channel's approver roster, the INV-12
  human-authorization set, PLUS its destination — a per-project room/channel map or a single default
  destination via `TG_<NOTIFIER>_ROOMS`/`_CHANNELS` / `_DEFAULT_ROOM`/`_DEFAULT_CHANNEL`; without one the
  routed `#<project>-approvals` name is used verbatim, which is not a valid Matrix alias and rarely an
  existing Slack channel, so an approval poll would be undeliverable — config-not-code, the destination is
  the operator's real room id/alias or channel id; the email notifier likewise takes optional SMTP PLAIN auth
  via `TG_EMAIL_SMTP_USER`/`_TOKEN_REF`, resolved at send time and TLS-gated by net/smtp, since an
  auth-requiring relay refuses the no-auth path), the NetBox CMDB reader, the endpoint-driven observability exporters
  (OpenObserve / Langfuse / Healthchecks), and the LibreNMS ingest source. A tracker whose state model is
  instance-specific also carries its own scheme from config-not-code: the Jira backend takes the deployment's
  workflow transition ids (`TG_JIRA_TRANSITION_*`), ServiceNow its `incident.state` numeric codes
  (`TG_SERVICENOW_STATE_*`, defaulting to the out-of-box ITSM 2/6/1), and YouTrack its State-field value names
  and field name (`TG_YOUTRACK_STATE_*` — the default bundle has no `Resolved` value, its terminal is `Fixed`),
  rather than reference defaults a customized instance almost certainly does not use — POSTing a state the
  instance lacks silently 404s/no-ops every close-out. Each defaults to the reference scheme so an
  un-customized instance still works, and each honors the configured scheme on BOTH the write (transition) and
  read (fold-back) paths. That
  completes the declarable
  fleet at boot; surface-by-surface resolution FROM the manifest is the follow-on as each hot path migrates
  off direct wiring. The boot then RECONCILES the live capability manifest against an operator-declared
  expected set (`TG_EXPECTED_CAPABILITIES`, config-not-code): a deployment that pins its fleet refuses to
  start on any divergence — a capability present but undeclared (a config slip or supply-chain surprise) or
  declared but absent (a connector that failed to register) — the registry's "refuse a divergent live set"
  control; unset is opt-out (the fleet is logged, not pinned). From the same registry the worker also runs
  its self-telemetry: a config-gated (`TG_OBSERVABILITY_EXPORT_INTERVAL`, off by default) periodic loop
  exports liveness + declared-capability gauges (`modules/telemetry.CapabilitySamples`) to the ENABLED
  observability exporters resolved from the registry — the 4th surface made load-bearing (after ingest,
  CMDB, and tracker). When a live suppression gate is assembled the same loop also exports its running
  decision counts as per-outcome gauges (`tg_suppression_decisions{outcome=escalate|suppressed|notice}`
  via `modules/telemetry.SuppressionSamples`, fed from the gate's concurrency-safe `LiveSuppressGate.Counts`
  snapshot), so ops can dashboard the tier-1 suppression RATE and catch over-suppression. Fail-open: an
  export error is logged and skipped, never fatal, and no configured exporter means no loop. It
  also seeds the causal estate graph from the configured topology sources
  (config-not-code): the NetBox (`TG_NETBOX_URL`), LibreNMS (`TG_LIBRENMS_DEPLOYMENTS`), and PVE
  (`TG_PVE_URL`) live readers, plus an operator-declared edge file (`TG_ESTATE_DECLARED_FILE`, SourceDeclared
  0.85 — always out-ranked by a live source, so live device state stays the source of truth) and a learned
  incident-co-occurrence file (`TG_ESTATE_LEARNED_FILE`, SourceIncident, LearnedConfidence capped 0.75 —
  below every live source and the suppression cutoff, so it only enriches prediction), and a declared tunnel
  file (`TG_ESTATE_TUNNEL_FILE`, SourceTunnel 1.0 `routes_via` — the top tier, ground-truth cross-site paths).
  The six tiers (tunnel > pve > netbox/librenms > declared > learned) merge under the MAX-ratchet. Each source
  is per-source-isolated; a fetch/parse failure is logged and its edges omitted, never a silent empty topology.
  The graph is held behind an atomic `estate.Holder`, and `TG_ESTATE_REFRESH_INTERVAL` (off by default) drives
  a periodic re-read of the live topology sources WITHOUT a restart; a total-source-outage refresh keeps the
  last good graph rather than blanking the estate. The prediction closures + gate read the current snapshot
  through the holder, so a refresh takes effect immediately. The estate's SELF-LEARNING tier is fed live: the
  `InvestigateActivity` observes each incident's alert into a thread-safe `core/learn` co-occurrence learner
  (`Deps.Observe`), and every refresh folds the learner's current co-occurrences in as a `SourceIncident`
  edge source — so the estate improves itself from the observed incident stream (learned edges stay capped
  0.75, only ever enriching prediction). This closes the observe → learn → predict loop end-to-end.
  The SAME configured LibreNMS deployments (`TG_LIBRENMS_DEPLOYMENTS`) also compose the agent's READ-ONLY
  investigation toolset at boot: `librenms.NewTools` registers `get-device-status`, `get-device-eventlog`,
  and `get-active-alerts` into the `NewReadOnlyToolSet` the `InvestigateActivity` agent runs with, so triage
  grounds in OBSERVED live device state (up/down, recent event log, firing alerts) rather than inference alone.
  Each tool is GET-only and registration fails closed — a non-read-only tool aborts boot (INV-17) — resolves a
  host by sysName/hostname across the declared deployments, and returns a lookup miss as a `Success:false`
  result (data the agent adapts to), never a fatal error. Each typed response declares ONLY safe fields, so a
  device row's SNMP secrets (community/authpass/cryptopass) are dropped before anything can reach the model.
  Alongside LibreNMS, two SSH-based read-only investigation families join the same `NewReadOnlyToolSet` when
  configured: `syslogng.NewTools` (`TG_SYSLOGNG_DEPLOYMENTS`) reads the device-log window, and
  `hostdiag.NewTools` (`TG_HOSTDIAG_DEPLOYMENTS`) clones the predecessor's storage-specialist/triage-researcher
  ability — `check-host-disk` / `check-host-memory` / `check-host-services` / `check-host-load` SSH the
  alerting host and run a FIXED read-only diagnostic (`df`, `du`, `free`, `systemctl --failed`, `ps`, `uptime`),
  so the agent GROUNDS a resource alert (e.g. runs `df` on a disk-full LXC) instead of escalating blind. Both
  use the same native in-process crypto/ssh client (no ssh binary in the distroless image), fail closed without
  the operator-declared known_hosts, route a host to its SSH identity by an allowlist, and are GET/read-only —
  registration of a non-read-only tool aborts boot (INV-17). The prediction gate's store is the durable pgx-backed `db.PredictionStore` (append-only
  `infragraph_prediction`) when `TG_DB_DSN` is configured, else the in-memory oracle twin — both satisfy
  `predict.PredictionStore`, so the gate is identical either way (config-not-code; the DSN carries its own
  secret refs). The composition root ALSO binds the deviation-breaker recovery (spec/015 REQ-1525): once the
  armed `safety.MutationBreaker` and the live `policy.ModeController` (reached via the mode-transition
  activity's `Controller`) both exist, the worker binds a `breakerRearmer` into the controller, so an
  owner-gated escalation into an actuating mode re-arms a tripped breaker (ledgered `safety:breaker-rearm`)
  instead of leaving a single deviation trip a permanent, estate-wide actuation kill. Bound only when both
  collaborators are present; a controller-less / breaker-less boot skips it (the breaker is inert there).

## The pipeline (REQ-1101, REQ-1103)

`(topology) → investigate → classify → gate → (execute no-op → verify no-op)`. The topology step is the
execution-class decision (`core/execclass`) taken FIRST, from the cheap pre-context signals (severity today;
known-pattern / criticality / correlation as those land upstream), so a cheap incident need not pay the full
lifecycle a hard correlated one needs. It is a pure, deterministic, fail-safe function of the envelope
(INV-08) recorded on `RunnerResult.ExecClass` for the console, ledger and evaluation; the per-class fast/deep
paths consume it as they are built. After the gate, the workflow asserts `gate.ActionID == action_id` derived
from the proposed action; a mismatch means the action changed and the run fails closed to a re-gate
(REQ-1103). The agent producing no proposal short-circuits to a read-only stop (REQ-1104). On a proposed
action the workflow also surfaces the evidence the proposal cited — `RunnerResult.EvidenceIDs`, a read of the
already-committed `inv.Proposal.EvidenceIDs` the classifier bound against the captured tool results (INV-11) —
so the console, ledger and evaluation can see WHAT grounded an action, not merely that one was proposed. It is
a pure read that never participates in action identity.

## Why the GatedProposal stays inside one activity

`predict.GatedProposal` is sealed by an unexported field; it is not serializable across a Temporal
activity boundary. `GateActivity` therefore performs `Commit` + `BuildApprovalPoll` in one activity and
returns only the serializable binding (`action_id`, prediction hash, poll-built flag). The workflow
threads the `action_id` — identity, not the sealed object — which is exactly what the gate protects.

## Determinism and durability

The workflow uses only deterministic constructs (`workflow.ExecuteActivity`, `workflow.NewTimer`,
`workflow.Now`, pure hashing); it performs no `time.Now`, random, or map-order-dependent computation, so
Temporal's replay is deterministic. The session wall-clock budget (REQ-1117) reads `workflow.Now`, which
returns the workflow's history time (deterministic on replay — unlike `time.Now`), so the budget-deadline
timer's duration is itself deterministic. Durable pause/resume and the closed auto-resume loop
(`continue-as-new`) are Temporal-native; the acceptance oracle drives the workflow in the in-process test
env (`testsuite`), which replays history to assert determinism.

## Runaway bounds: bounded activity retries + the session wall-clock budget (REQ-1116, REQ-1117)

Two runaway bounds live in `temporal/runner/budget.go` and are wired in `workflow.go` (Gulli ch12 ·
Anthropic 6.4 · design-wisdom #8).

**Bounded activity retries (REQ-1116).** Temporal's default activity `RetryPolicy` is UNBOUNDED
(`MaximumAttempts 0` ⇒ retry forever). The Runner's base `ActivityOptions` previously set only a
`StartToCloseTimeout`, so every ordinary read-only pipeline activity inherited that unbounded default: a
persistently-failing activity would retry forever, either pinning the session open on a `.Get` that never
returns or — for the best-effort record/reconcile activities whose error is discarded — hanging the
session on a `.Get` that never resolves. `runnerActivityOptions()` bounds the base to
`MaximumAttempts=4` with capped exponential backoff (1s/2.0/30s) and a `NonRetryableErrorTypes`
short-circuit for deterministic (poison) inputs. Three retry CLASSES, all bounded: base pipeline `≤ 4`;
read-only investigate `≤ 2` (one retry over a transient blip — safe because the loop is read-only,
INV-21); and the two hazardous classes — the human-vote RECORD and the estate EXECUTE — pinned at exactly
`1` at their own call sites (at-least-once delivery plus one human approval must never double-append the
ledger nor execute the estate twice). Changing a `RetryPolicy` is replay-safe: it is an activity-option
attribute, not part of the workflow command SEQUENCE, so an in-flight activity keeps its recorded policy
and only newly-scheduled activities take the new one — no `GetVersion` guard is required.

**The session wall-clock budget (REQ-1117).** `WorkflowWallClockBudget` (26h = VoteWait 24h + 2h compute
headroom) caps a single session's total wall-clock, measured from workflow start via `workflow.Now`
deltas. The one place a session parks for a long span is the durable human-vote wait, so the Runner races
a budget-deadline timer against that wait: when the budget is exhausted before a decision arrives the
workflow stops **budget-exceeded** to the SAME terminal orphaned-poll hand-off a timed-out poll uses
(stand down fail-closed, record `session:budget-exceeded` once on the ledger, persist the triage row, hand
the incident to the escalation re-check lane), surfaced with the distinct outcome `escalated:budget-
exceeded`. Because the budget exceeds VoteWait, the deadline never fires before the 24h vote timeout in
production, so the poll window is fully honored — the budget bites only a session whose TOTAL wall-clock
exceeds a full day plus headroom, which bounded compute + one vote wait cannot legitimately reach (a
backstop against a future added loop or a misconfigured timeout). Adding the budget-deadline branch to the
vote-wait selector changes the command sequence, so it is guarded by the `GetVersion(ctx,
"wallclock-budget", DefaultVersion, 1)` marker: an in-flight poll started before this change gets
`DefaultVersion` on replay, adds no budget timer, and replays deterministically. `WorkflowWallClockBudget`
is a package `var` (not a `const`) purely so the acceptance oracle can inject a short ceiling and drive
the budget-exceeded branch deterministically in the in-process env (whose mock clock only advances on the
awaited vote timer); production never rebinds it.

**The spend-side runaway bound (spec/013 REQ-1211..1215).** The two bounds above cap wall-clock and retry
count; the DOLLAR cost is bounded by the cost/budget breaker, owned by spec/013. Its only touch-point in the
runner plane is the composition root: `cmd/worker/main.go` WRAPS the model gateway it injects as
`Deps.Model` with a `cost.MeteringCompleter` when a `TG_COST_*` budget/rate is configured, so every agent
completion accrues into the durable daily/session accumulator and a daily-budget/session-ceiling breach
force-Shadows the mode (a `cost:breaker-trip` ledger note). The wrap is transparent (it returns the
gateway's result unchanged) and fail-open (a cost-store outage never blocks inference), so the runner
workflow and its activities are unchanged — the meter lives entirely in the injected `Deps.Model` seam. See
`spec/013-actuation-interceptor/design.md` § "Cost/budget spend guard".

## Out of scope

A token/prompt budget stop (Anthropic 6.4) would require an activity-result signature change to surface
per-cycle token spend from the agent loop; it is a documented follow-on (activities.go is owned outside
this change). This change ships the time/cycle budget only.

The agent loop is spec/011; the classifier is spec/001; the prediction gate and verifier are spec/002;
the ingest envelope and the ledger are spec/006. Turning the execute/verify activities live (flipping
`mutation_enabled`) is Phase 2. Periodic read-only schedules (P1-9) are a Temporal Schedule surface
tracked separately.

## Estate snapshot publication (REQ-516 write side)

The worker owns the causal estate graph (`estate.Build` → `estate.Holder`, refreshed on
`TG_ESTATE_REFRESH_INTERVAL`). To let the read API serve the SAME graph the prediction gate reasons
over — without the grounder rebuilding it — the worker publishes a snapshot after the initial build and
after every refresh, via `db.EstateWriteStore.Publish(graph.Export(), sourceCount)` into the
schema-stamped `estate_snapshot` table (owned by spec/006 REQ-516). Publication is best-effort: it is
wired only when a durable DB pool exists (an in-memory worker never publishes, and the grounder then
honestly reports "no snapshot"), it never blocks or fails triage, and a write error is logged and the
prior snapshot keeps serving. This adds no new mutation path — it is a read-model projection of the
graph the worker already holds.

## Opt-in insecure TLS for internal estate pollers

Internal infrastructure (LibreNMS, Proxmox on :8006) is commonly served over self-signed certificates.
TG's estate topology pollers verify TLS strictly by default (fail-closed — a mismatched cert yields no
edges rather than a trusted-but-wrong topology). For those internal endpoints, `estateHTTPClient(true)`
— wired only when `TG_LIBRENMS_INSECURE` / `TG_PVE_INSECURE` is explicitly set — disables verification
for that specific READ poller. This is the pragmatic, EXPLICIT, per-source accommodation the predecessor
made unconditionally (`curl -k`); here it is default-off, opt-in, scoped to the estate read pollers, and
never touches ingress, actuation, or the model gateway. A boot log records when it is active.
