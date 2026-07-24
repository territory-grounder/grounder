<!-- spec/023 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/023 — Design

Realizes REQ-2300..2313 on the Go/Temporal/Postgres stack, composing over the existing six-gate stack
(ACL · min_confidence · band · graduation · mode · floor) rather than adding a seventh gate that could
drift: attribution feeds the *existing* classifier and the *existing* workflow terminal outcomes.

## 1. Where attribution sits in the pipeline (REQ-2300, REQ-2305)

Attribution is computed in a runner activity between context assembly and classification, exactly where
`canaryPin` / `novelIncident` / `criticalityTier` derive their signals today
(`temporal/runner/activities.go` `ClassifyActivity` build site):

```
ingest → suppression → context (CMDB / estate / precedent)
      → ATTRIBUTE (readers → typed evidence → deterministic taxonomy → ruleset disposition)
      → agent investigation (evidence visible as delimited DATA, never authority)
      → classify (GatedInput carries the disposition; poll reasons compose most-restrictive-first)
      → gate → poll/approve → actuate (mode chokepoint) → verify → learn
```

Two distinct evidence questions are asked, because they resolve to different taxonomy values:

- **(a) Who caused the fault-shaped change?** (the stop, the config change, the deletion) — resolves
  toward `attributed-authorized` / `authorized-test` / `attributed-suspicious` / `unattributable`.
- **(b) Did TG's own actuation identity already remediate this (target, fault class) inside the
  self-recognition window?** — resolves toward `attributed-self` (REQ-2302). Question (b) consults
  TWO sources that must agree in direction: the domain reader (e.g. the PVE task log shows
  `root@pam!tg-actuate` started the guest after the fault) and TG's own governance ledger (a
  `verified_clean` actuation on the same target/op-class in the window). Either alone is admissible
  evidence; the ledger is authoritative when they disagree (it is TG's own append-only record).

The deterministic attributor lives in a new `core/attribution` package (one implementation, INV-18).
The model never sets the taxonomy: the agent sees the evidence as a delimited data block (like
`cmdbContext` / `trackerContext`) and can reason about it in its narrative, but the disposition is
computed from typed reader records only (REQ-2312, the `buildEvidence` lesson: captured, timestamped,
target-relevant — never cited free-text).

## 2. The typed model (REQ-2300, REQ-2312)

```go
// core/attribution
type Taxonomy int // closed enum; ZERO VALUE = Unattributable (see note below)
const (
    Unattributable Taxonomy = iota // absent/failed evidence — pre-feature ladder, NOT suspicious
    AttributedAuthorized           // sanctioned non-TG principal
    AttributedSelf                 // TG's own actuation identity, inside the window
    AttributedSuspicious           // positive evidence of an unsanctioned actor / missing audit entry
    AuthorizedTest                 // carve-out matched: sanctioned harness fault on an allowlisted pool host
)

type Evidence struct {          // one reader-captured record
    Domain     string           // "pve" | "journal" | "k8s-audit" | "netbox" | "gitops-mr" | "awx" | "docker"
    Actor      string           // principal as the domain records it, e.g. "root@pam!tg-actuate"
    ActionKind string           // domain verb, e.g. "vzstop", "vzstart", "sudo", "MR-merged"
    Target     string           // the investigated subject the record names
    ObservedAt time.Time        // domain timestamp (window-checked, REQ-2312)
    Ref        string           // domain-native id (UPID, journal cursor, audit-event id, changelog id)
    Covered    bool             // this reader AFFIRMATIVELY covers the target's audit trail (REQ-2304 half 2)
}

type Finding struct {           // the attributor's required-field output (INV-19 pattern)
    Taxonomy   Taxonomy
    RuleID     string           // matched mapping/carve-out rule id ("" = built-in default path)
    Candidates []Taxonomy       // >1 ⇒ contradiction (REQ-2310)
    Evidence   []Evidence
    Warnings   []string         // reader errors (REQ-2307) — recorded, never fatal
}
```

**Zero-value note (deliberate deviation, called out for review).** Everywhere else in TG the zero value
is the *most restrictive* state (Band zero = POLL_PAUSE, Reversibility zero = Irreversible). Here the
zero value is `Unattributable` = *pre-feature behavior*, per the owner's evidence-gated-honesty rule
(REQ-2303: absent evidence is not suspicion) and Constitution §3.3 (advisory enrichment fails OPEN).
This is safe precisely because of REQ-2305: no attribution value can ever *grant* autonomy, so the
"least restrictive" attribution equals the already-governed baseline — the mutation lane's own
fail-closed gates are untouched beneath it. The DISPOSITION enum keeps the fail-closed convention: an
unmapped or unloadable disposition resolves to escalate-to-human (REQ-2308).

## 3. The reader interface and the PVE first reader (REQ-2306, REQ-2307)

A new adapter seam, shaped like the existing module families (`modules/README.md`: loadable
implementations of `adapters/` interfaces, registered at startup, INV-17):

```go
// adapters/actorevidence
type Reader interface {
    Domain() string
    ReadOnly() bool // always true; a mutating reader is a compile-time contradiction of the seam
    // Read returns actor-evidence records for target within [since, until]. Errors are advisory:
    // the attributor logs, records a warning, and treats the domain's evidence as absent (REQ-2307).
    Read(ctx context.Context, target string, since, until time.Time) ([]attribution.Evidence, error)
}
```

- Registration mirrors `modules/registry.go`: a reader exists only if compiled in and explicitly
  registered; config supplies endpoint + `SecretRef` credential; an unconfigured domain has no reader
  and yields `unattributable` for its targets (REQ-2303). Every `Read` runs under a bounded
  per-reader timeout (config, with a compiled ceiling) so a hung audit endpoint cannot stall triage.
- **PVE task-log reader** (`modules/actorevidence/pve`): native REST like the existing
  `modules/actuation/proxmox` module (no subprocess — the worker is distroless), calling
  `GET /nodes/{node}/tasks?vmid={vmid}` after resolving the guest by name via `/cluster/resources`
  (the same fail-closed name→node/vmid resolution the actuation module uses). Task rows carry `user`
  (e.g. `root@pam`, `root@pam!tg-actuate`), `type` (`vzstop`, `vzstart`, `qmstop`, …), `starttime`,
  and the UPID (the `Ref`). The reader's token is a separate READ-ONLY PVE token — never the
  `tg-actuate` write token (REQ-2306, INV-13; the credential engine resolves it per spec/016).
  `Covered=true`: the PVE task log is authoritative for guest lifecycle, so a guest that stopped with
  NO corresponding task-log entry is REQ-2304's "mutation with no audit entry" signal (Phase 2
  enables that disposition; Phase 1 records it as a warning only).
- Follow-on readers behind the same interface: systemd journal + auth/sudo logs, k8s audit log,
  NetBox changelog, GitOps/MR history, AWX job history, docker events (§Phased delivery in tasks.json).
- TG's *own* actuation identity per domain is resolved from the credential engine's configuration
  (spec/016) — never a hardcoded token string — so self-recognition survives a token rotation.

## 4. Rules-as-data: the mapping and the carve-out (REQ-2308, REQ-2309)

The taxonomy→disposition mapping and the carve-out rules are DATA in the versioned ruleset store
(`core/policy` `ruleset_store` — the same load-time-validated, versioned, audited home as the policy
rules), under a new `actor_attribution` document section:

```json
{
  "actor_attribution": {
    "mapping": [
      { "id": "authorized-coordinate", "taxonomy": "attributed-authorized", "disposition": "stand-down-coordinate" },
      { "id": "self-noop",             "taxonomy": "attributed-self",       "disposition": "self-noop" },
      { "id": "unattributable-base",   "taxonomy": "unattributable",        "disposition": "ladder-unchanged" },
      { "id": "suspicious-escalate",   "taxonomy": "attributed-suspicious", "disposition": "security-escalate" },
      { "id": "authorized-test-heal",  "taxonomy": "authorized-test",       "disposition": "ladder-unchanged" }
    ],
    "sanctioned_principals": [
      { "id": "estate-admins", "domain": "pve", "actors": ["root@pam"] }
    ],
    "carve_outs": [
      { "id": "shadowbench-pool", "domain": "pve",
        "actors": ["root@pam", "root@pam!tg-actuate"],
        "hosts": ["dc1librespeed01", "dc1myspeed01"],
        "valid_from": "2026-07-23T00:00:00Z", "valid_until": "2026-10-01T00:00:00Z" }
    ]
  }
}
```

- The disposition enum is CLOSED and validated at load time exactly like the policy `Verdict` enum
  (`core/policy/schema.go` pattern) — the policy verdict trinary itself (spec/015 REQ-1503/REQ-1506)
  is untouched; dispositions are a distinct rule family, so no fourth verdict is smuggled in.
- Defaults ship as a data file next to `core/policy/default_ruleset.json` (the mapping above IS the
  default), satisfying "loadable, not hardcoded" while keeping a fresh install governed out of the
  box. An absent/corrupt section fails to REQ-2308's escalate-to-human defaults, computed by the
  loader's zero value — never by a permissive fallback.
- Carve-out rows are INV-20-shaped (temporally bounded, `valid_from`/`valid_until`); expiry fails in
  the SAFE direction — an expired carve-out reverts `authorized-test` to `attributed-authorized`,
  i.e. stand-down, which withholds actuation rather than granting it.
- The carve-out encodes the standing sanctioned-pool reality: harness faults (`root@pam` manual
  stops, the `tg-actuate` fault-injection cycle) on allowlisted pool hosts still heal — the learning
  regime's purpose — but the triage row records the attribution honestly (REQ-2309), so the corpus
  distinguishes organic incidents from manufactured ones.

## 5. Classifier and runner wiring (REQ-2301, REQ-2304, REQ-2305)

`risk.GatedInput` gains disposition-shaped fields, following the `CanaryPinned` precedent (an
activity-derived bool that can only RAISE review):

```go
AttributionStandDown  bool // disposition stand-down-coordinate → poll("actor-attributed-authorized")
AttributionSecurity   bool // disposition security-escalate    → poll("actor-attributed-suspicious") + security_escalation signal
AttributionEscalate   bool // mapping-missing / contradiction  → poll("actor-attribution-escalate")
```

- The three branches slot into `Classify` AFTER the mechanical floors (jailbreak, never-auto,
  server-destructive, stateful, self-protected) — which record the more fundamental poll reason when
  they also apply — and BEFORE the auto-eligible branches, beside the canary pin: they can raise an
  otherwise-AUTO action to a poll, never lower one (the safety-composition invariant, spec/001).
- `security_escalation` rides the Signals map onto the `session_risk_audit` row and the governance
  `Decision` record; the notifier routes a security-flagged POLL_PAUSE to the org-configured security
  channel in addition to the approver graph (REQ-2304). **No fourth Band is introduced** — the
  Constitution fixes the three-band vocabulary (§6); "SECURITY ESCALATION" is a routing + signal
  overlay on POLL_PAUSE, exactly as AUTO_NOTICE is a notify overlay on auto-approval.
- `ladder-unchanged` and absent attribution set none of the three fields: byte-identical classifier
  behavior to today (REQ-2303's oracle is a differential test over the pre-feature classifier).
- The `self-noop` disposition never reaches the classifier: the workflow terminates at the attribute
  step with terminal outcome `already-remediated` (§6). Defense in depth: if a self-noop finding DOES
  reach classification (a wiring bug), the runner maps it to `AttributionEscalate` — fail toward the
  human, never toward re-actuation.

## 6. The self no-op is a terminal outcome, not a silent drop (REQ-2302)

Constitution 4.15: a session is done only when its knowledge is written back. `already-remediated`
becomes a first-class terminal outcome alongside the existing ones: the `session_triage` row is
written (outcome, attribution column, evidence blob), a governance `Decision` record is appended
(reason `attributed-self-noop`, the evidence refs in signals), the ticket reconciles through the
band-aware reconciler, and the decision tracer shows the short-circuit as a trace step. This is what
"graceful" means mechanically — auditable, ticket-closing, corpus-feeding; never a swallowed return.

## 7. Persistence and the tracer (REQ-2311, REQ-2313)

Migration `0034_actor_attribution` (additive, defaulted, backward-compatible — the 0024 pattern):

```sql
ALTER TABLE session_triage
    ADD COLUMN actor_attribution text  NOT NULL DEFAULT '',   -- '' = pre-feature rows / unattributable
    ADD COLUMN actor_evidence    jsonb NOT NULL DEFAULT '[]'; -- []Evidence, redacted, retention-bounded
```

- The evidence blob rows carry `expires_at` handling under the org retention policy (INV-14); the
  attribution *value* and rule id are derived audit facts and ride the immutable spine (the ledger
  signals), while the raw evidence blob is purgeable operational body — the 4.5 split.
- Journal/auth evidence is minimized before persist (actor, verb, timestamp, ref — never raw log
  lines) and passes `core/screen` scrubbing at every boundary (REQ-2313).
- The decision tracer (spec/020) gains an `attribute` step between context and classify, reading the
  new columns + ledger signals — read-side only, no new write path.

## 8. What is explicitly NOT changed

The six-gate stack, the never-auto floor, the mode chokepoint, the prediction gate, the mechanical
verdict, the single-parse grammar, and the policy verdict trinary are untouched. Attribution adds one
advisory-collected, restrictively-composed input to the classifier plus one new terminal outcome; it
authorizes nothing, and removing the ruleset section returns the platform to pre-feature behavior
minus the recorded attribution column.

## 9. Cross-spec lockstep note (resolved before implementation)

The `GatedInput` fields and the three classifier branches touch `core/risk/input.go` +
`core/risk/classifier.go`, which are spec/001-governed lockstep files, and the `ClassifyActivity`
build-site edit touches `temporal/runner/activities.go` (spec/012). They are deliberately NOT in any
T-023 `files_owned`: the same MR that lands T-023-5 carries a companion spec/001 amendment (a REQ-01x
addition mirroring REQ-2301/2304/2305's classifier-visible behavior, canary-pin style) and re-stamps
`spec/.lockstep.lock` — the authorized, audited path (SDD-WORKFLOW §6).

## 10. Phase 2 — generalising beyond Proxmox (REQ-2314..2321)

Phase 1 answered WHO for one domain (the PVE task log). Phase 2 answers it across the estate, and adds
the identity dimension the owner named ("why only proxmox users? — LDAP, RADIUS, linux audit"). The
design keeps two structurally-distinct concerns apart, and — critically — leaves the shipped,
adversarially-reviewed deterministic core (`core/attribution/attribution.go`) byte-for-byte unchanged.

### 10.1 Two seams, one of which is new

- **Action-evidence readers** (REQ-2314) answer "actor X did action Y on target Z at time T". They
  implement the *unchanged* `adapters/actorevidence.Reader` seam and emit `Evidence` records — exactly
  like the PVE reader. The first Phase-2 reader is `modules/actorevidence/journal` (systemd-journal /
  sudo / auditd), reusing the hardened native-SSH read transport `syslogng.NewNativeRunner` (host-key
  verified, key-only, no PTY, fixed POSIX-quoted argv, distroless-safe — no shell, INV-02) and the
  credential engine's per-host read-only identity (`credential.IdentityResolver`, gated by an operator
  allowlist + a mandatory known-hosts file), mirroring `modules/observability/hostdiag`. Unlike hostdiag
  it *deterministically parses* `journalctl -o json` / `ausearch` output into `[]attribution.Evidence`
  and never hands raw log text to the model (REQ-2312/2313). The k8s-audit / netbox / awx / gitops /
  docker readers slot into this same seam later with no seam change.

- **Identity/auth enrichment** (REQ-2315) answers a different question — "is principal X a known,
  enabled, non-service member of a sanctioned admin group?" — which has no target/action/time and so
  cannot be an `Evidence` record. It is a new, small **non-Reader** seam
  (`adapters/actorevidence.SanctionResolver`): `Resolve(ctx, domain, actors) -> (confirmed, disabled,
  warnings, err)`. It answers only for actors *already named* by an admissible action-evidence record,
  so it can never by itself mint a taxonomy (REQ-2315 preserves REQ-2312).

### 10.2 Dynamic sanctioning without touching the pure core (REQ-2316)

The static `sanctioned_principals` list stays the deploy-time seed and the fail-open floor. The
enrichment runs only in the side-effecting `AttributeActivity`, between the reader fan-out and the call
to `attribution.Attribute`, over a **per-session copy** of `cfg.Sanctioned[domain]`:

- **Promotion** (REQ-2317): a confirmed enabled, non-service member of the domain's configured
  sanctioned admin group (`SanctionedGroups`, loadable rules-as-data, REQ-2308) is *added* to the copy —
  the existing `contains(cfg.Sanctioned[e.Domain], e.Actor)` check then classifies it
  `attributed-authorized` (stand-down) instead of `attributed-suspicious`. A live promotion is exactly
  as auditable as a hand-added static entry.
- **Demotion** (REQ-2318): a statically-sanctioned principal that a *fresh positive* directory fact
  reports locked / disabled / expired / service is *removed* from the copy — the unchanged classifier
  then falls it through to `attributed-suspicious` (security-escalate). A disabled admin credential in
  use is a compromise indicator; this is the security capability Phase 1 lacked.

`attribution.Attribute` is called identically and is not modified — the promotion/demotion are pure set
operations on the copied allowlist that the *existing* classifier consumes. No lockstep-governed file
changes for the enrichment logic.

### 10.3 The fail direction, reconciled (REQ-2319)

The action readers fail *open-to-unattributable* (REQ-2307). The identity enrichment fails
*open-to-static*: on any resolver error, timeout, ambiguity, or non-affirmative result, the copy equals
the static list — no promotion, no demotion — reproducing Phase-1 byte-identically. Both refinements
fire only on **fresh positive evidence**, never on absence, so a dead resolver can neither mint
`attributed-suspicious` nor grant a stand-down. This direction is deliberate and security-reasoned:
failing *closed-to-suspicious* on a directory outage would flip every session to security-escalate
during a routine FreeIPA restart — a false-suspicious flood that trains operators to ignore the signal
and masks a real intrusion. An optional, off-by-default, per-domain `StrictIdentity` lever (convert the
demotion outage-gap into escalate-to-human, never toward auto) is reserved as a future refinement, not
built in Phase 2.

### 10.4 Reuse, PII, and scope discipline (REQ-2320, REQ-2321)

The LDAP resolver (`modules/actorevidence/ldapident`) reuses the mature `modules/credsource/ldap`
transport (go-ldap/v3, LDAPS:636 CA-verified `SecretRef`, ordered NL->GR replica failover, size/time
bounds, `SecretRef` binds) and adds the one query the bulk credsource Sync lacks: a bounded
single-principal search reading `nsAccountLock`, `krbPrincipalExpiration`, `memberOf`, and `objectClass`
(to reject `ipaservice` accounts). Identity data (usernames, group cns, DNs, commands) is
retention-bounded (`expires_at`) and redacted at every boundary; resolver warnings carry provider +
domain only, never the principal (REQ-2320). RADIUS is **deferred** (REQ-2321): a RADIUS record names a
NAS/login not the mutated target (fails REQ-2312), and a missing auth event is not proof of absence
(key/console/local logins bypass RADIUS), so it may enter only as an advisory `auth-observed`
annotation, never a taxonomy-minting reader or a classification mover.

### 10.5 Explicitly deferred (recorded, not built in Phase 2)

Cross-realm actor->uid mapping (pve `root@pam` <-> LDAP `root`; k8s ServiceAccount <-> LDAP) — Phase 2
scopes the identity dimension to domains where the actor is a bare directory uid (journal / sudo /
auditd on directory-enrolled hosts); pve keeps its static list. Also deferred: the k8s-audit / netbox /
awx / gitops / docker readers; OIDC / Kerberos providers; the `StrictIdentity` lever; the
service-account-on-interactive-action demotion; and the REQ-2304-half-2 covered-trail-with-no-entry
disposition.
