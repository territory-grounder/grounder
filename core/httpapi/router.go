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
)

// Stats is the read-only platform status returned by /v1/stats. mutation_enabled reflects the WORKER's
// published live posture (the authoritative mutation gate lives in the worker process, not the read-only
// grounder). posture_stale flags that the worker's published row is stale or absent — the surface then
// reports the freshest reading it holds but marks it unknown rather than a confident OFF; posture_source
// names where the value came from (worker / worker-stale / grounder-gate).
type Stats struct {
	MutationEnabled bool   `json:"mutation_enabled"`
	OpenSessions    int    `json:"open_sessions"`
	PendingPolls    int    `json:"pending_polls"`
	PostureStale    bool   `json:"posture_stale"`
	PostureSource   string `json:"posture_source"`
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
	// Alerts records accepted envelopes and serves the alerts read surface (REQ-510). nil = 503.
	Alerts AlertLog
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
	// SealedRead lists the sealed store's value-less inventory on /v1/secrets (REQ-524). nil = the
	// sealed section is empty.
	SealedRead SealedSecretsReader
	// Credentials serves the credential-engine read surface (REQ-526): the sync-source drift projection,
	// the per-target resolution history, and the coverage summary — REAL persisted state, non-secret by
	// construction (INV-13). nil = 503.
	Credentials CredentialsReader
	// Policy serves the Policy Engine read surface (spec/015 T-015-12): the append-only decision audit, the
	// active autonomy mode + honest posture, the per-op-class graduation ladder, and the operator's
	// rules-as-data policy — REAL persisted state, non-secret by construction (INV-13). nil = 503.
	Policy PolicyReader
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
	// The alerts read surface (REQ-510): the recent accepted-envelope window for the console.
	rt.Handle("/v1/alerts", auth.AuthReadOnly, d.alertsHandler, http.MethodGet)
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
	// The grounding scorecard (REQ-517): the mechanical verifier's match/partial/deviation distribution,
	// the falsifiability signal (real vs shuffled-graph control), and the autonomy-band distribution —
	// TG's core differentiator, published as live evidence rather than asserted.
	rt.Handle("/v1/grounding", auth.AuthReadOnly, d.groundingHandler, http.MethodGet)
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
	// The credential-engine read surface (REQ-526): the sync-source drift projection (credential_sync_run),
	// the per-target resolution history (credential_resolution), and the coverage summary derived from that
	// history. All REAL persisted state, non-secret by construction — no response carries key material, a
	// SecretRef value, or a token (INV-13). Pure reads; a live resolve-probe is a documented follow-up.
	rt.Handle("/v1/credentials/sources", auth.AuthReadOnly, d.credentialSourcesHandler, http.MethodGet)
	rt.Handle("/v1/credentials/resolutions", auth.AuthReadOnly, d.credentialResolutionsHandler, http.MethodGet)
	rt.Handle("/v1/credentials/coverage", auth.AuthReadOnly, d.credentialCoverageHandler, http.MethodGet)
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
	// The Actuation Regime Engine read surface (spec/017 T-017-7, REQ-1716): the append-only
	// regime_resolution (target → regime → lane), regime_actuation (launch), and deferred_verdict tails plus
	// the per-lane coverage roll-up. All REAL persisted state, non-secret by construction — token_ref is a
	// SecretRef reference, never a value; no response carries an argv/host, key material, or a secret (INV-13).
	// A pure read; the console per-target map / template-allowlist editor / pending-verification queue is a
	// SEPARATE follow-on MR (T-017-7 console). Empty at Shadow.
	rt.Handle("/v1/regime", auth.AuthReadOnly, d.regimeHandler, http.MethodGet)
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
		// The admin operator tier (task #27 Phase B–D, REQ-522/523/524): registered ONLY when the
		// admin authenticator is configured — otherwise the elevation route and every admin write
		// route do not exist at all (fail closed), mirroring the browser-session pattern above.
		if d.AdminSessions != nil {
			// Step-up: a valid session + the separate admin credential mint a short-lived elevation.
			rt.Handle("/v1/session/elevate", auth.AuthAdminElevate, d.sessionElevateHandler, http.MethodPost)
			// Config overrides (REQ-523): admin-session-only; LAW keys refuse with 422 — the clamp
			// is the law. Writes execute in the worker, ledger-before-commit.
			rt.Handle("/v1/config/{key}", auth.AuthAdminSession, d.configWriteHandler, http.MethodPost)
			// Sealed secrets (REQ-524): write-only material in, a store:<name> reference out.
			rt.Handle("/v1/secrets/{name}", auth.AuthAdminSession, d.secretPutHandler, http.MethodPost)
			// Autonomy-mode transition (spec/015 REQ-1502): admin-session-only — the LAST gate before the
			// mutation flip. The flip executes in the WORKER on the single chokepoint-bound ModeController
			// (the wired AuthorityChecker gates on a flip-authorized operator + the green preflight); every
			// attempt is audited to the hash chain. Mutation stays OFF until an operator posts a flip.
			rt.Handle("/v1/mode", auth.AuthAdminSession, d.modeTransitionHandler, http.MethodPost)
		}
	}
}
