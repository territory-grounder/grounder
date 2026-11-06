// Package httpapi registers Territory Grounder's read-only HTTP surface (stats + session-replay) on
// the mandatory-auth router. It owns no auth logic of its own — every route goes through core/auth,
// so an unauthenticated request is rejected before the handler runs and an auth=none route cannot be
// registered at all.
//
// Provenance: [O] INV-01 (mandatory auth, reject-before-parse), spec/006 REQ-501/REQ-504 · [O] H-01/P0-2
// (no privileged resume-with-prompt: a replay mints a NEW gated workflow from an immutable read-only
// snapshot). Phase 0/1 is read-only; these handlers never mutate.
package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/territory-grounder/grounder/core/auth"
	"github.com/territory-grounder/grounder/core/persist"
	"github.com/territory-grounder/grounder/core/trace"
)

// Stats is the read-only platform status returned by /v1/stats. mode + may_actuate reflect the WORKER's
// published live actuation posture (the authoritative chokepoint lives in the worker process, not the
// read-only grounder): the owner-set 4-mode value ("" = unknown) and its derived "can it act right now".
// posture_stale flags that the worker's published row is stale or absent — the surface then reports the
// freshest reading it holds but marks it unknown rather than a confident OFF; posture_source names where
// the value came from (worker / worker-stale / grounder-gate).
type Stats struct {
	Mode          string `json:"mode"`
	MayActuate    bool   `json:"may_actuate"`
	OpenSessions  int    `json:"open_sessions"`
	PendingPolls  int    `json:"pending_polls"`
	PostureStale  bool   `json:"posture_stale"`
	PostureSource string `json:"posture_source"`
}

// ContextSnapshot is the immutable, read-only seed for a session-replay. It carries NO caller-supplied
// mutable input: a replay re-runs the full gate from zero seeded only by this snapshot, so there is no
// resume-with-prompt primitive an attacker could ride (REQ-501, closes H-01/P0-2).
type ContextSnapshot struct {
	ExternalRef string
	Site        string
	Summary     string
	CapturedAt  time.Time
}

// StatsReader returns read-only platform stats for the authenticated principal.
type StatsReader interface {
	Stats(ctx context.Context, p auth.Principal) (Stats, error)
}

// SnapshotStore loads an immutable ContextSnapshot by external_ref UNDER the caller's RBAC authority.
// It returns found=false for BOTH an unknown id and an id the caller has no authority over, so the two
// are observationally identical to the client (REQ-504). Authority is resolved inside Get against the
// principal — never inferred from a request field.
type SnapshotStore interface {
	Get(ctx context.Context, externalRef string, p auth.Principal) (snap ContextSnapshot, found bool, err error)
}

// WorkflowStarter mints a NEW gated Temporal workflow from a read-only snapshot. It never resumes a
// mutating session; the returned id identifies the fresh run that re-executes the gate from zero.
type WorkflowStarter interface {
	StartFromSnapshot(ctx context.Context, snap ContextSnapshot) (workflowID string, err error)
}

// Deps are the collaborators the read-only surface needs. All are interfaces so the handlers are
// oracle-testable with in-memory fakes (CI has no live DB/Temporal).
type Deps struct {
	Stats        StatsReader
	Snapshots    SnapshotStore
	Starter      WorkflowStarter
	Ledger       LedgerReader
	Ingesters    IngesterResolver
	Triage       TriageStarter
	Capabilities CapabilitiesReader
	// Sessions enables the browser operator-session surface (REQ-508). nil = the login/logout routes
	// are not registered and the read routes accept machine principals only — exactly today's posture.
	Sessions *auth.SessionAuthenticator
	// SessionsRead serves the sessions read surface (REQ-509) from the audit spine. nil = 503.
	SessionsRead SessionsReader
	// SessionDetailRead serves the per-session decision-tracer walk (spec/020 REQ-2011) — one incident
	// assembled from the correlation spine in decision-boundary order. Observe-only; nil = 503.
	SessionDetailRead SessionDetailReader
	// CommitConfirmChip loads the armed-revert window chip for one session (spec/029 T-029-5,
	// REQ-2906) — nil chip + nil error means "no window" (the ordinary case). Optional seam; nil
	// leaves every walk exactly as before.
	CommitConfirmChip func(ctx context.Context, externalRef string) (*CommitConfirmChipDTO, error)
	// SessionEvidenceRead serves the GROUND TRUTH behind one recorded step (TG-272) — what the console's
	// "ground truth <tool>" citation opens. Nil ⇒ 503 on that route only; the walk itself is unaffected, which
	// is the correct degradation for a deployment that has the tracer but not the evidence store.
	SessionEvidenceRead trace.AgentStepEvidenceReader
	// SessionDiagnosisRead serves the TYPED CLAIM behind one session's proposal (TG-201) — the structured,
	// source-bound diagnosis the console's #reasoning surface renders under the walk. Nil ⇒ 503 on that route
	// only; the walk and its evidence are unaffected, which is the correct degradation for a deployment that
	// has the tracer but not the claim store (every session recorded before migration 0056 is exactly that).
	SessionDiagnosisRead trace.DiagnosisReader
	// IngestRefused counts a REFUSED delivery, by reason (TG-371). The front door published fifteen
	// tg_ingest_* families and every one of them measured acceptance or upstream reachability — so a
	// source whose token rotated, whose payload stopped parsing, and one that genuinely has nothing to
	// send were ONE observable: tg_ingest_source_last_seen_seconds growing. The rejection points are
	// already well-typed with distinct statuses; they were simply never counted.
	//
	// nil is legitimate (a deployment with no metrics sink) and costs nothing — the handler still
	// refuses, it just does not tally.
	IngestRefused func(sourceType, reason string)
	// IngestPredrop counts an alert the front door ACCEPTED (2xx) but did NOT turn into a new triage
	// session, by reason (TG-380): a recovery transition (mints no triage) or a re-fire of an in-flight
	// incident (StartTriage returns the existing id). Distinct from IngestRefused, which counts non-2xx
	// turn-aways. This is the pre-admission drop the pve03 cascade could not measure — "the upstream sent
	// more than we triaged" was permanently unknowable. nil is legitimate and costs nothing.
	IngestPredrop func(reason string)
	// GateMargins serves the gate-decision BOUNDARY-CASE queue (TG-178): interceptor gates that passed or
	// refused within ε of their numeric threshold, read off the observe-only gate-verdict trail's signed
	// margins (migration 0076). nil ⇒ 503 on that route only — the surface holds an honest empty state.
	GateMargins GateMarginReader
	// Axes serves the benchmark-axis scoreboard (TG-480): the same axis.Scorecard the axisscore CLI
	// computes, so the console renders A1–A8 + G5/G6 off ONE authority. nil ⇒ 503.
	Axes AxesReader
	// GateMarginEpsilon is the DEFAULT review band the boundary-case queue uses when a caller names no eps=
	// (TG-178: ε is loadable configuration, not a compiled constant). A value in (0, maxGateMarginEpsilon]
	// overrides the compiled 0.05 default; 0/unset/out-of-range keeps it, so an absent config never widens or
	// breaks the band. A caller's explicit eps= still wins over this default.
	GateMarginEpsilon float64
	// Alerts records accepted envelopes and serves the alerts read surface (REQ-510). nil = 503.
	Alerts AlertLog
	// Suppression observes accepted alerts in SHADOW and records what incident-scoped suppression would
	// have dropped. Nil ⇒ no observation. It never suppresses; wiring the judgement to actually drop an
	// alert is a separate, evidence-gated change.
	Suppression SuppressionObserver
	// Actions serves the sealed-ActionManifest walk the console's Actions surface renders. Nil ⇒ 503,
	// so the surface holds an honest empty state instead of the invented incidents it replaced.
	Actions ActionManifestReader
	// Transitions durably captures provider recovery transitions (spec/012 clear-confirm). nil = recoveries
	// route as before (no capture — fail-safe, the feature is simply inert).
	Transitions TransitionRecorder
	// Governance serves the safety-posture read surface (REQ-511). nil = 503.
	Governance GovernanceReader
	// SecretsRead serves the secret-REFERENCE surface (REQ-512) — references only, never values.
	SecretsRead SecretsReader
	// EventsInterval paces the /v1/events posture stream (REQ-513); zero = the 5s default.
	EventsInterval time.Duration
	// Models serves the model-gateway passthrough surface (REQ-514). nil = 503.
	Models ModelsReader
	// Contract is the generated OpenAPI document served verbatim (REQ-515). empty = 503.
	Contract []byte
	// Estate serves the latest published estate snapshot (REQ-516). nil = 503.
	Estate EstateReader
	// Skills serves the versioned skill library + trial state (spec/014 REQ-1311/1313). nil = 503.
	Skills SkillsReader
	// SkillsWrite executes operator skill writes (spec/014 REQ-1301/1311): drafts as row inserts,
	// transitions through the worker (the ledger's single writer). nil = 503; session-only routes.
	SkillsWrite SkillsWriter
	// Grounding serves the grounding scorecard (REQ-517): live aggregates over the verdict/prediction/
	// audit tables — the evidence that the committed-prediction + mechanical-verifier loop works. nil = 503.
	Grounding GroundingReader
	// Proposals lists the shadow (free-form, never-executable) proposals for the console's proposals lane
	// (spec/026 REQ-2607). Read-only surface with no actuation control; nil = 503 fail-closed.
	Proposals ProposalsReader
	// Counterfactual is OPTIONAL: nil simply omits the headline. The list surface must not depend on it.
	Counterfactual CounterfactualReader
	// Manifest serves the auto-drafted world model for review (spec/027 REQ-2703): discovered entries with
	// provenance, confidence, and lifecycle. Read-only; nil = 503 fail-closed (never a fabricated row).
	Manifest ManifestReader
	// ManifestWrite executes the operator's closed-verb adopt/reject/retire through the worker (the
	// ledger's single writer). nil = 503; session-only routes. There is deliberately no create verb —
	// discovery authors rows, the operator only ever decides on them (paradigm rule 9).
	ManifestWrite ManifestWriter
	// OpClass serves the earned op-class review queue and dossiers (spec/028 REQ-2813). Read-only;
	// nil = 503 fail-closed. [COORDINATION: this field and its two routes below are spec/028 T-028-7's
	// only edits to this shared file — the deps.go/router.go coordinated-note precedent.]
	OpClass OpClassCandidateReader
	// OpClassWrite executes the closed verb set {ratify, dismiss, demote, revoke, export-embed} through the
	// worker. nil = 503; session-only routes. There is deliberately no verb that turns a model proposal into
	// a template: ratification is operator authorship (ADR-0016 decision 3).
	OpClassWrite OpClassWriter
	// Votes delivers an authenticated operator vote to the waiting Runner workflow (REQ-518, INV-12).
	// nil = the vote surface fails closed to 503.
	Votes VoteSignaler
	// PendingDecisions lists the open POLL_PAUSE decisions awaiting a human vote (REQ-519) — the projection
	// the Runner writes on POLL_PAUSE. A pure read; it can release nothing (that is /v1/vote). nil = 503.
	PendingDecisions persist.PendingReader
	// Config resolves the control-plane configuration with each knob's source (REQ-520, task #27 Phase A).
	// A pure read; LAW keys are pinned, no write path here, no secret value emitted. nil = 503.
	Config ConfigResolver
	// Wiki serves the living knowledge base (REQ-521): the distilled lessons corpus + the embedded
	// runbook pages; the skills section joins in from Skills above. nil = 503.
	Wiki WikiReader
	// AdminSessions enables the admin operator tier (task #27 Phase B, REQ-522): the step-up elevation
	// route + the config/secret write routes. nil = the admin lane does not exist at all (fail
	// closed), even when browser sessions are configured.
	AdminSessions *auth.AdminAuthenticator
	// ConfigWrite executes ledgered control-plane config overrides via the worker (REQ-523). nil = 503.
	ConfigWrite ConfigWriter
	// SecretsWrite seals and stores write-only secret material via the worker (REQ-524). nil = 503.
	SecretsWrite SealedSecretWriter
	// SealRewrap re-wraps stored DEKs under the current master-key version via the worker (TG-163).
	// nil = 503 — a deployment with no seal backend has nothing to re-key.
	SealRewrap SealRewrapper
	// SealedRead lists the sealed store's value-less inventory on /v1/secrets (REQ-524). nil = the
	// sealed section is empty.
	SealedRead SealedSecretsReader
	// Credentials serves the credential-engine read surface (REQ-526): the sync-source drift projection,
	// the per-target resolution history, and the coverage summary — REAL persisted state, non-secret by
	// construction (INV-13). nil = 503.
	Credentials CredentialsReader
	// CredentialOnboardingRead reports which named credentials the inventory sources bind to which hosts,
	// mapped and unmapped alike (TG-274). Nil ⇒ that route 503s; every other credential route is unaffected.
	CredentialOnboardingRead CredentialOnboardingReader
	// CredentialSync starts the worker-side "Sync now" lane for one registered source (TG-109). Nil ⇒ the
	// sync route 503s; the read routes are unaffected.
	CredentialSync CredentialSyncer
	// CredentialNativeRead lists the operator-authored DB-backed native rule rows (TG-109, REQ-1610).
	// Nil ⇒ GET /v1/credentials/native 503s; every other credential route is unaffected.
	CredentialNativeRead NativeRulesReader
	// NativeRuleWrite executes the ledgered native-rule add/delete via the worker (the ledger's single
	// writer; the entry is validated through ParseRules before anything persists). Nil ⇒ the native-rule
	// write routes fail closed to 503.
	NativeRuleWrite NativeRuleWriter
	// ObjectGroupRead lists the operator-authored object groups (TG-481, spec/016). Nil ⇒ GET
	// /v1/estate/groups 503s.
	ObjectGroupRead ObjectGroupsReader
	// ObjectGroupWrite executes the ledgered object-group add/delete via the worker (the ledger's single
	// writer; name/patterns validated before anything persists). Nil ⇒ the object-group write routes 503.
	ObjectGroupWrite ObjectGroupWriter
	// Policy serves the Policy Engine read surface (spec/015 T-015-12): the append-only decision audit, the
	// active autonomy mode + honest posture, the per-op-class graduation ladder, and the operator's
	// rules-as-data policy — REAL persisted state, non-secret by construction (INV-13). nil = 503.
	Policy PolicyReader
	// Tracer runs the policy packet-tracer (spec/015 TG-105): POST a hypothetical candidate action, get the
	// composed verdict + matched rule + why from the worker's REAL engine over Temporal. Read-only — it
	// evaluates and returns, writing no audit row and actuating nothing. nil = POST /v1/policy/trace 503s.
	Tracer PolicyTracer
	// Regime serves the Actuation Regime Engine read surface (spec/017 T-017-7, REQ-1716): the append-only
	// regime_resolution / regime_actuation / deferred_verdict audit tails and the per-lane coverage roll-up —
	// REAL persisted state, non-secret by construction (INV-13; token_ref is a SecretRef reference, never a
	// value). nil = 503. Empty at Shadow (no resolution/launch/verdict before the flip).
	Regime RegimeReader
	// ModeTransition executes an operator-invoked autonomy-mode transition via the worker (spec/015
	// REQ-1502) — the LAST gate before the mutation flip. nil = POST /v1/mode fails closed to 503. The
	// flip runs on the worker's single chokepoint-bound ModeController: the wired AuthorityChecker gates on
	// the flip-authorized operator AND, for any escalation, the green preflight; both outcomes are audited.
	// Mutation stays OFF until an operator posts a flip — wiring this never auto-transitions anything.
	ModeTransition ModeTransitioner
	// EngineToggle enables/disables the policy engine via the worker (spec/015 REQ-1519, "the operator owns
	// the paranoia dial") — the warn-don't-block admin toggle. nil = POST /v1/policy/engine-toggle fails
	// closed to 503. It runs on the worker's single live EngineToggle (the wired AuthorityChecker + the
	// warn-don't-block acknowledgement gate it; every attempt is audited); the constitutional never-auto floor
	// (INV-09) still clamps beneath. Wiring this never auto-toggles anything.
	EngineToggle EngineToggler
	// RulesetWrite replaces the ACTIVE rules-as-data policy document via the worker (spec/015 REQ-1503,
	// TG-104) — the "sealed, ledgered admin write" behind the Policy console's "Edit rules…" placeholder.
	// nil = POST /v1/policy/ruleset fails closed to 503. The write VALIDATES the document (ParseRuleSet,
	// fail-closed — a malformed ruleset is refused, never persisted), ledgers it BEFORE the row commits, and
	// persists it (active singleton + immutable version archive) through the single-writer worker; the
	// grounder never writes the ruleset itself. The read projection is GET /v1/policy/rules above.
	RulesetWrite RulesetWriter
	// The per-module configuration surface (TG-253/TG-252). Each nil ⇒ its route fails closed to 503.
	ModuleSchema ModuleSchemaReader
	ModuleTest   ModuleTester
	ModuleSecret ModuleSecretWriter
	// Rollback triggers the operator-facing MANUAL ROLLBACK of a previously-executed forward action (TG-462) —
	// the inverse traverses the SAME governed actuation chain (sealed inverse manifest, POLL_PAUSE, human
	// approval, mode chokepoint) with InvertsActionID set, inert under Shadow. nil = POST
	// /v1/actions/{action_id}/rollback fails closed to 503. The workflow runs in the WORKER; the grounder never
	// seals or actuates the inverse itself.
	Rollback Rollbacker
}

// Register wires the read-only console/ops surface onto the authenticated router. Pure reads register
// AuthReadOnly (machine principals as before, plus GET-only browser sessions, REQ-508); the ingest and
// replay routes stay machine-only (AuthHMAC) — a browser session can never reach them. A route with
// auth=none is impossible — auth.Router.Handle panics at registration (INV-01).
func Register(rt *auth.Router, d Deps) {
	rt.Handle("/v1/whoami", auth.AuthReadOnly, d.whoamiHandler, http.MethodGet)
	rt.Handle("/v1/stats", auth.AuthReadOnly, d.statsHandler, http.MethodGet)
	rt.Handle("/v1/sessions/{external_ref}/replay", auth.AuthHMAC, d.replayHandler, http.MethodPost)
	// Read-only console data endpoint (spec/010 consumer): the governance ledger. A pure read over the
	// immutable, hash-chained audit spine.
	rt.Handle("/v1/ledger", auth.AuthReadOnly, d.ledgerHandler, http.MethodGet)
	// The alert front door: an authenticated source POSTs its raw payload; the ingester is RESOLVED from the
	// module registry (INV-17 — an unregistered/disabled source has no execution path) and normalized against
	// its grammar (INV-04). Registry-backed resolution, read-only triage (Phase 0/1). Machine-only.
	// The ingest front door: HMAC/mTLS callers are admitted exactly as before (tried first); a push source
	// that cannot body-sign (Alertmanager) may instead present its per-source static bearer token, which is
	// fail-closed unless that source has an ingest_token_ref provisioned (0008, AuthIngestPush).
	rt.Handle("/v1/ingest/{source_type}", auth.AuthIngestPush, d.ingestHandler, http.MethodPost)
	// Read-only fleet visibility: the declared connector capabilities and their enablement (a disabled
	// member has no execution path, INV-17). For the console/ops.
	rt.Handle("/v1/capabilities", auth.AuthReadOnly, d.capabilitiesHandler, http.MethodGet)
	// The generated-dialog SHAPE. Read-only and value-free: current values come from /v1/config with
	// their provenance, and a second answer to "what is configured" could disagree with the first.
	rt.Handle("/v1/modules/schema", auth.AuthReadOnly, d.moduleSchemaHandler, http.MethodGet)
	// The sessions read surface (REQ-509): the audit spine's recent triage sessions for the console.
	rt.Handle("/v1/sessions", auth.AuthReadOnly, d.sessionsHandler, http.MethodGet)
	// The per-session decision-tracer walk (spec/020 REQ-2011): one incident assembled from the correlation
	// spine in decision-boundary order. Observe-only. Gated behind the distinct, ELEVATED trace-read role
	// (REQ-2014, AuthTraceRead) — a machine caller or an admin-eligible (tg-admins) session; a plain read-only
	// operator session, which satisfies the AuthReadOnly console surfaces, is REFUSED here.
	rt.Handle("/v1/sessions/{external_ref}", auth.AuthTraceRead, d.sessionDetailHandler, http.MethodGet)
	// The per-session STEP CHANNEL (spec/020 REQ-2013/REQ-2010): the SAME walk, streamed as SSE so a queued or
	// live-running session animates from REAL boundary events (not a client-side clock). Same elevated trace-read
	// role as the detail endpoint; observe-only.
	rt.Handle("/v1/sessions/{external_ref}/stream", auth.AuthTraceRead, d.sessionStreamHandler, http.MethodGet)
	// The GROUND TRUTH behind one step (TG-272): the screened tool payload the console's citation opens. SAME
	// AuthTraceRead gate as the walk — the payload is a detail OF the walk, and a weaker gate here would be a
	// way around the tracer's own authority.
	rt.Handle("/v1/sessions/{external_ref}/evidence/{evidence_id}", auth.AuthTraceRead, d.sessionEvidenceHandler, http.MethodGet)
	// The TYPED CLAIM behind the walk (TG-201): root cause, mechanism, what supports it, what CONTRADICTS it,
	// and what was ruled out. SAME elevated AuthTraceRead gate as the walk and the evidence citation, for the
	// same reason — it is a detail OF the walk and it quotes screened host output. AuthReadOnly here would let
	// a plain console session read reasoning the tracer itself refuses it.
	rt.Handle("/v1/sessions/{external_ref}/diagnosis", auth.AuthTraceRead, d.sessionDiagnosisHandler, http.MethodGet)
	// The gate-decision BOUNDARY-CASE queue (TG-178): interceptor gates that decided within ε of their
	// threshold. It reads the decision-tracer's own gate-verdict margins, so it carries the SAME elevated
	// AuthTraceRead authority as the walk — a plain read-only operator session is refused here.
	rt.Handle("/v1/gates/within-epsilon", auth.AuthTraceRead, d.gateMarginsHandler, http.MethodGet)
	// The alerts read surface (REQ-510): the recent accepted-envelope window for the console.
	rt.Handle("/v1/alerts", auth.AuthReadOnly, d.alertsHandler, http.MethodGet)
	rt.Handle("/v1/actions", auth.AuthReadOnly, d.actionsHandler, http.MethodGet)
	// The governance posture (REQ-511) and the secret-reference list (REQ-512) for the console.
	rt.Handle("/v1/governance", auth.AuthReadOnly, d.governanceHandler, http.MethodGet)
	rt.Handle("/v1/secrets", auth.AuthReadOnly, d.secretsHandler, http.MethodGet)
	// The liveness stream (REQ-513): SSE posture events for the console's live indicator.
	rt.Handle("/v1/events", auth.AuthReadOnly, d.eventsHandler, http.MethodGet)
	// The models surface (REQ-514): the gateway's own model inventory, relayed verbatim.
	rt.Handle("/v1/models", auth.AuthReadOnly, d.modelsHandler, http.MethodGet)
	// The contract surface (REQ-515): the generated endpoint map, drift-gated against this very table.
	rt.Handle("/v1/contract", auth.AuthReadOnly, d.contractHandler, http.MethodGet)
	// The estate surface (REQ-516): the worker's published causal graph, latest snapshot.
	rt.Handle("/v1/estate", auth.AuthReadOnly, d.estateHandler, http.MethodGet)
	// The skill library (spec/014 REQ-1311/1313): versions with rationale/scores, and the trial state.
	// chi matches by SPECIFICITY (a literal /trials segment beats the {name} wildcard) regardless of
	// registration order — proven by the routed-dispatch test in skills_test.go.
	rt.Handle("/v1/skills", auth.AuthReadOnly, d.skillsHandler, http.MethodGet)
	rt.Handle("/v1/skills/trials", auth.AuthReadOnly, d.skillTrialsHandler, http.MethodGet)
	rt.Handle("/v1/skills/{name}", auth.AuthReadOnly, d.skillDetailHandler, http.MethodGet)
	// SKILL.md interchange (TG-55/TG-476, ADR-0012): the production row rendered as frontmatter + body.
	// Export only — a SKILL.md enters as a draft through the existing write flow, never a parallel import.
	rt.Handle("/v1/skills/{name}/export", auth.AuthReadOnly, d.skillExportHandler, http.MethodGet)
	// The grounding scorecard (REQ-517): the mechanical verifier's match/partial/deviation distribution,
	// the falsifiability signal (real vs shuffled-graph control), and the autonomy-band distribution —
	// TG's core differentiator, published as live evidence rather than asserted.
	rt.Handle("/v1/grounding", auth.AuthReadOnly, d.groundingHandler, http.MethodGet)
	// The benchmark-axis scoreboard (TG-480): the CLI/eval scorecard, served read-only for the console.
	rt.Handle("/v1/axes", auth.AuthReadOnly, d.axesHandler, http.MethodGet)
	// The open proposal plane's read surface (spec/026 REQ-2607): shadow proposals, read-only, no verb of
	// any kind — the console renders what TG WOULD do where no registered op-class exists yet.
	rt.Handle("/v1/proposals", auth.AuthReadOnly, d.proposalsHandler, http.MethodGet)
	// The pending-decisions read surface (REQ-519): the POLL_PAUSE decisions awaiting a human vote, so the
	// console can list them and an operator can act via /v1/vote. A pure read; caller_can_act is
	// server-computed and a machine principal sees the queue read-only.
	rt.Handle("/v1/decisions", auth.AuthReadOnly, d.decisionsHandler, http.MethodGet)
	// The control-plane configuration read surface (REQ-520): the resolved config with each knob's source
	// (law/env/console). LAW keys are read-only; no write path (Phase B), no secret value emitted.
	rt.Handle("/v1/config", auth.AuthReadOnly, d.configHandler, http.MethodGet)
	// The wiki read surface (REQ-521): the living knowledge base — lessons distilled from
	// confirmed-clean resolved incidents (the retriever's own corpus), embedded runbook pages, and the
	// production skill library by reference. Pure reads over recorded knowledge, never fabricated.
	rt.Handle("/v1/wiki", auth.AuthReadOnly, d.wikiHandler, http.MethodGet)
	rt.Handle("/v1/wiki/{slug}", auth.AuthReadOnly, d.wikiPageHandler, http.MethodGet)
	// Store-backed runbook pages (TG-476): the production runbook-class rows the index lists under
	// runbook/<name>. A distinct two-segment route — chi matches per segment, so the store namespace can
	// never shadow an embedded page, a lesson ref, or a compiled host article.
	rt.Handle("/v1/wiki/runbook/{name}", auth.AuthReadOnly, d.wikiRunbookPageHandler, http.MethodGet)
	// The credential-engine read surface (REQ-526): the sync-source drift projection (credential_sync_run),
	// the per-target resolution history (credential_resolution), and the coverage summary derived from that
	// history. All REAL persisted state, non-secret by construction — no response carries key material, a
	// SecretRef value, or a token (INV-13). Pure reads; a live resolve-probe is a documented follow-up.
	// The NATIVE lane (TG-109) joins the family below: GET /v1/credentials/native lists the operator-
	// authored DB-backed rule rows (elevated tier — the rows carry SecretRef strings), and the admin-tier
	// /v1/credentials/native/rules writes ride the single-writer worker lane.
	rt.Handle("/v1/credentials/sources", auth.AuthReadOnly, d.credentialSourcesHandler, http.MethodGet)
	rt.Handle("/v1/credentials/resolutions", auth.AuthReadOnly, d.credentialResolutionsHandler, http.MethodGet)
	rt.Handle("/v1/credentials/coverage", auth.AuthReadOnly, d.credentialCoverageHandler, http.MethodGet)
	// "Sync now" (TG-109): re-run ONE registered source's read-only sync on demand, in the worker. Admin
	// session + the shared write guard — it drives a real pull against a third-party system, the same tier
	// as the module TEST button. The outcome carries only non-secret SyncRun facts (INV-13).
	rt.Handle("/v1/credentials/sources/{source_id}/sync", auth.AuthAdminSession, d.credentialSyncHandler, http.MethodPost)
	// The credential-onboarding first screen (TG-274): which named credential governs which hosts, and
	// whether TG can use it. Names, scopes and SecretRef strings only — never key material (INV-13).
	//
	// ELEVATED (AuthTraceRead), NOT AuthReadOnly — TG-294. The three /v1/credentials reads above are
	// operational history (what was resolved, what drifted). THIS one is a ranked map of the estate's
	// unprotected surface: migration 0054 stores `hosts integer -- blast radius` beside `mapped boolean`
	// and indexes them together as (mapped, hosts DESC), so one GET answers "which credential owns the
	// most hosts, and which of those are unprotected" already sorted. That ordering is target selection,
	// and it does not become safe by carrying references instead of values — naming the ten unmapped
	// credentials and ranking them by host count is the reconnaissance step, whoever holds the key.
	//
	// AuthTraceRead is the right tier rather than AuthAdminSession: it is the elevated READ tier, so a
	// machine principal (HMAC/mTLS) still satisfies it as a trusted system caller, and a browser session
	// needs admin standing (LDAP tg-admins, or a live step-up). AuthAdminSession would be an admin-WRITE
	// gate on a read, and — being registered only when an admin authenticator is configured — would make
	// the surface vanish entirely on a deployment without one. The operator who acts on this list already
	// needs the admin tier for the write that resolves it (POST /v1/modules/{surface}/{source}/secret,
	// AuthAdminSession below), so requiring elevation to READ the work list costs no one their workflow.
	// The tier is pinned by TestCredentialOnboardingRequiresElevatedTraceReadTier — an authz level with no
	// test drifts on the next refactor, which is how it arrived at read-only in the first place.
	rt.Handle("/v1/credentials/onboarding", auth.AuthTraceRead, d.credentialOnboardingHandler, http.MethodGet)
	// The operator-authored DB-backed native rule rows (TG-109, REQ-1610).
	//
	// ELEVATED (AuthTraceRead), NOT AuthReadOnly — the /v1/credentials/onboarding rationale (TG-294)
	// applies row for row: each entry is a per-target identity mapping carrying the SecretRef STRING that
	// unlocks it (a reference, never a value — INV-13), so the list is a map of which credential reaches
	// which target pattern. That is reconnaissance whoever holds the keys, and it does not become safe by
	// carrying references instead of values. Same tier as onboarding for the same reason: a machine
	// principal (HMAC/mTLS) still reads it as a trusted system caller, a browser session needs admin
	// standing, and the operator who acts on this list already needs the admin tier for the writes below.
	rt.Handle("/v1/credentials/native", auth.AuthTraceRead, d.credentialNativeRulesHandler, http.MethodGet)
	// The native-rule WRITE lane (TG-109): add one validated rule / take one back. Admin session + the
	// shared write guard, registered unconditionally like the "Sync now" route above (the auth layer fails
	// closed without an admin tier). DISTINCT patterns from the GET, not verbs on it: this Router registers
	// by PATTERN (a second Handle on one pattern REPLACES the first — see /v1/config/{key} below) and the
	// GET carries a DIFFERENT auth class, so one pattern cannot serve both tiers. The writes execute in
	// the WORKER (validated via ParseRules — exactly one rule; ledgered BEFORE the row commits).
	rt.Handle("/v1/credentials/native/rules", auth.AuthAdminSession, d.nativeRuleAddHandler, http.MethodPost)
	rt.Handle("/v1/credentials/native/rules/{id}", auth.AuthAdminSession, d.nativeRuleDeleteHandler, http.MethodDelete)
	// The OBJECT GROUP surface (TG-481, spec/016): GET the authored object groups (trace-read tier — group
	// membership reveals actuation-policy structure, though it carries no secret value); add/delete on DISTINCT
	// /entries patterns (pattern-key Router + a different read auth class, exactly like the native-rule lane
	// above). The writes execute in the WORKER (name/patterns validated; ledgered BEFORE the row commits).
	rt.Handle("/v1/estate/groups", auth.AuthTraceRead, d.objectGroupsHandler, http.MethodGet)
	rt.Handle("/v1/estate/groups/entries", auth.AuthAdminSession, d.objectGroupAddHandler, http.MethodPost)
	rt.Handle("/v1/estate/groups/entries/{id}", auth.AuthAdminSession, d.objectGroupDeleteHandler, http.MethodDelete)
	// The Policy Engine read surface (spec/015 T-015-12): the append-only per-decision audit
	// (policy_decision), the single active autonomy mode + honest posture (policy_mode), the per-op-class
	// earned-autonomy ladder (policy_graduation), and the operator's active rules-as-data policy
	// (policy_ruleset). All REAL persisted state, non-secret by construction — no response carries an
	// argv/host, key material, a credential, or a secret (INV-13). Pure reads; the console ASA editor /
	// packet-tracer / mode selector is a SEPARATE follow-on MR.
	rt.Handle("/v1/policy/decisions", auth.AuthReadOnly, d.policyDecisionsHandler, http.MethodGet)
	rt.Handle("/v1/policy/mode", auth.AuthReadOnly, d.policyModeHandler, http.MethodGet)
	rt.Handle("/v1/policy/graduation", auth.AuthReadOnly, d.policyGraduationHandler, http.MethodGet)
	rt.Handle("/v1/policy/rules", auth.AuthReadOnly, d.policyRulesHandler, http.MethodGet)
	// The policy packet-tracer (spec/015 TG-105): POST a HYPOTHETICAL candidate action, get the composed
	// verdict + matched rule + effective band + why from the worker's REAL engine. AuthReadOnly — it
	// evaluates and returns, actuating nothing and (unlike the interceptor's audited engine) writing no
	// policy_decision row. A machine principal and a browser operator session may both ask "may TG act?".
	rt.Handle("/v1/policy/trace", auth.AuthReadOnly, d.policyTraceHandler, http.MethodPost)
	// The Actuation Regime Engine read surface (spec/017 T-017-7, REQ-1716): the append-only
	// regime_resolution (target → regime → lane), regime_actuation (launch), and deferred_verdict tails plus
	// the per-lane coverage roll-up. All REAL persisted state, non-secret by construction — token_ref is a
	// SecretRef reference, never a value; no response carries an argv/host, key material, or a secret (INV-13).
	// A pure read; the console per-target map / template-allowlist editor / pending-verification queue is a
	// SEPARATE follow-on MR (T-017-7 console). Empty at Shadow.
	rt.Handle("/v1/regime", auth.AuthReadOnly, d.regimeHandler, http.MethodGet)
	// The auto-drafted world model review surface (spec/027 REQ-2703): discovered estate entries with
	// provenance, confidence, and status. A pure read; caller_can_act is server-computed so the console
	// never renders an adopt control a machine principal could not use. Empty until discovery runs.
	rt.Handle("/v1/manifest", auth.AuthReadOnly, d.manifestHandler, http.MethodGet)
	// The earned op-class review surface (spec/028 REQ-2813). A pure read: the queue, and the five-question
	// dossier for one candidate. The dossier serves the MODEL's words as clearly-named exhibits so an
	// operator can judge the proposal — and in a shape no form can consume, because a spec that arrives
	// pre-filled with model text is model admission wearing a human's name (ADR-0016 decision 3).
	rt.Handle("/v1/opclass/candidates", auth.AuthReadOnly, d.opClassCandidatesHandler, http.MethodGet)
	rt.Handle("/v1/opclass/candidates/{key}", auth.AuthReadOnly, d.opClassDossierHandler, http.MethodGet)
	// Browser operator sessions (REQ-508): registered ONLY when configured — otherwise the browser
	// path does not exist at all (fail closed).
	if d.Sessions != nil {
		rt.Handle("/v1/session", auth.AuthOperatorLogin, d.sessionLoginHandler, http.MethodPost)
		rt.Handle("/v1/session/logout", auth.AuthSession, d.sessionLogoutHandler, http.MethodPost)
		// The vote intake (REQ-518): an authenticated operator releases or denies a POLL_PAUSE-held
		// decision (INV-12). Session-only — registered ONLY with the browser path, like login/logout.
		rt.Handle("/v1/vote", auth.AuthSession, d.voteHandler, http.MethodPost)
		// The skill write path (spec/014 REQ-1301/1311): session-only like /v1/vote — a machine
		// principal has NO write route. Rationale mandatory; transitions run in the worker.
		rt.Handle("/v1/skills/{name}/versions", auth.AuthSession, d.skillDraftHandler, http.MethodPost)
		rt.Handle("/v1/skills/versions/{id}/{verb}", auth.AuthSession, d.skillTransitionHandler, http.MethodPost)
		// The world-model review lane (spec/027 REQ-2703): session-only, like every other write. The
		// verb table is CLOSED (adopt/reject/retire) and contains no create — an operator decides on what
		// discovery found and can never hand-author an actuation target (paradigm rule 9).
		rt.Handle("/v1/manifest/entries/{id}/{verb}", auth.AuthSession, d.manifestTransitionHandler, http.MethodPost)
		// The earned op-class ratify lane (spec/028 REQ-2813): session-only, like every other write. The
		// verb set is CLOSED and split by the noun it governs, which is REQ-2817's ActionID rule made
		// routable — during candidacy the governed artifact is the candidate_key, after a grant it is the
		// class. A verb aimed at the wrong noun is refused by the router rather than discovered mid-handler.
		rt.Handle("/v1/opclass/candidates/{key}/{verb}", auth.AuthSession, d.opClassCandidateVerbHandler, http.MethodPost)
		rt.Handle("/v1/opclass/classes/{class}/{verb}", auth.AuthSession, d.opClassClassVerbHandler, http.MethodPost)
		// The admin operator tier (task #27 Phase B–D, REQ-522/523/524): registered ONLY when the
		// admin authenticator is configured — otherwise the elevation route and every admin write
		// route do not exist at all (fail closed), mirroring the browser-session pattern above.
		if d.AdminSessions != nil {
			// Step-up: a valid session + the separate admin credential mint a short-lived elevation.
			rt.Handle("/v1/session/elevate", auth.AuthAdminElevate, d.sessionElevateHandler, http.MethodPost)
			// Config overrides (REQ-523): admin-session-only; LAW keys refuse with 422 — the clamp
			// is the law. Writes execute in the worker, ledger-before-commit.
			// ONE handler, two verbs: this Router registers by PATTERN only (auth.Router.Handle →
			// mux.Handle), so a second Handle on the same path REPLACES the first rather than adding a
			// method. Registering DELETE separately silently unrouted POST — caught by the config-write
			// handler tests, which began receiving the clear outcome. Method dispatch belongs inside.
			rt.Handle("/v1/config/{key}", auth.AuthAdminSession, d.configKeyHandler, http.MethodPost, http.MethodDelete)
			// Admin-only despite being read-only in TG's terms: Test causes a visible side effect in a
			// third-party system other people watch, and the secret write stores a credential.
			rt.Handle("/v1/modules/{surface}/{source}/test", auth.AuthAdminSession, d.moduleTestHandler, http.MethodPost)
			rt.Handle("/v1/modules/{surface}/{source}/secret", auth.AuthAdminSession, d.moduleSecretHandler, http.MethodPost)
			// Sealed secrets (REQ-524): write-only material in, a store:<name> reference out.
			rt.Handle("/v1/secrets/{name}", auth.AuthAdminSession, d.secretPutHandler, http.MethodPost)
			// DEK rewrap (TG-163): re-key the sealed store onto the CURRENT Transit key version so the
			// previous one can be retired. Explicitly operator-driven — there is no timer behind it.
			// NOT registered under /v1/secrets/… on purpose: "rewrap" matches the secret-name pattern
			// above, so a static sibling there would win the match and make a secret of that name
			// permanently unwritable.
			rt.Handle("/v1/seal/rewrap", auth.AuthAdminSession, d.sealRewrapHandler, http.MethodPost)
			// Autonomy-mode transition (spec/015 REQ-1502): admin-session-only — the LAST gate before the
			// mutation flip. The flip executes in the WORKER on the single chokepoint-bound ModeController
			// (the wired AuthorityChecker gates on a flip-authorized operator + the green preflight); every
			// attempt is audited to the hash chain. Mutation stays OFF until an operator posts a flip.
			rt.Handle("/v1/mode", auth.AuthAdminSession, d.modeTransitionHandler, http.MethodPost)
			// Policy-engine enable/disable (spec/015 REQ-1519): admin-session-only — the warn-don't-block toggle.
			// The change executes in the WORKER on the single live EngineToggle (authz + acknowledgement + audited);
			// disabling in an actuating mode needs the red double-confirmation. nil ⇒ 503 fail closed.
			rt.Handle("/v1/policy/engine-toggle", auth.AuthAdminSession, d.engineToggleHandler, http.MethodPost)
			// Active-ruleset replacement (spec/015 REQ-1503, TG-104): admin-session-only — the sealed,
			// ledgered admin write behind the Policy console's disabled "Edit rules…" placeholder. The write
			// executes in the WORKER: the submitted rules-as-data document is VALIDATED (a malformed ruleset is
			// refused, never persisted — a bad ruleset governs actuation), ledgered BEFORE the row commits, then
			// persisted as the active singleton + an immutable version archive. A DISTINCT path from the
			// AuthReadOnly GET /v1/policy/rules read above: this Router registers by pattern (rt.mux.Handle), so
			// a POST on /v1/policy/rules would REPLACE the read registration and silently unroute it.
			rt.Handle("/v1/policy/ruleset", auth.AuthAdminSession, d.rulesetWriteHandler, http.MethodPost)
			// Operator-facing MANUAL ROLLBACK (TG-462): admin-session-only — a rollback is a MUTATION, so it
			// lives on the same admin-write tier as the mode flip, never a plain session or a machine principal.
			// The WORKER seals a fresh content-hashed inverse manifest, binds the forward execution record as
			// evidence, classifies it POLL_PAUSE, and hands it to the interceptor with InvertsActionID set — the
			// SAME chain a forward heal traverses, inert under Shadow. The grounder never seals or actuates it.
			rt.Handle("/v1/actions/{action_id}/rollback", auth.AuthAdminSession, d.rollbackHandler, http.MethodPost)
		}
	}
}
