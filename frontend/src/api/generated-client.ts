// The console's API layer mirrors the generated OpenAPI client (docs/contracts/openapi.yaml, produced by
// tools/gencontracts). The endpoint URLs are split into two sets whose relationship to the served contract
// is ENFORCED by contract.test.ts (which reads openapi.yaml): `liveEndpoints` must each map to a path the
// control plane actually serves, and `plannedEndpoints` are specified-but-unbuilt and must each be ABSENT
// from the contract until their server route lands. This replaces an earlier, untested claim of "lockstep"
// with a check that fails if the console ever calls a route the server does not expose.

// The authoritative control-plane status the console reflects (it never computes these).
export interface ControlPlaneStatus {
  mutation_enabled: boolean;
}

// A governed decision awaiting approval (POLL_PAUSE / AUTO_NOTICE).
export interface PendingDecision {
  decision_id: string;
  action_id: string;
  band: "AUTO" | "AUTO_NOTICE" | "POLL_PAUSE";
  plan: { approaches: string[] };
  prediction: string;
  reversible: boolean;
  // server-computed authorization: whether THIS caller may act on THIS decision (RBAC + on-call).
  caller_can_act: boolean;
}

export interface LedgerEntry {
  seq: number;
  decision: string;
  action_id: string;
  hash: string;
  prev_hash: string;
}

export interface ManifestStage {
  stage: "predicted" | "approved" | "executed" | "verified";
  at: string;
}

// ---- Actuation Regime Engine read surface (GET /v1/regime, spec/017 T-017-7, REQ-1716) ----
// The console's window onto the engine's REAL persisted state (migration 0020). Every field mirrors the
// Go DTO json tags in core/httpapi/regime.go exactly and is NON-SECRET by construction — target labels,
// regime/lane slugs, rule ids, op-classes, AWX job ids, verdict/graduation slugs, and the AWX token only
// as a SecretRef REFERENCE (token_ref, e.g. "store:awx.launch"), never a token value. The read surface
// selects nothing and edits no allowlist (the allowlist editor is a separate write follow-on).

// One regime_resolution audit row: the record of one lane selection. A refusal carries an EMPTY
// regime/lane (fail closed); a resolved row names both.
export interface RegimeResolution {
  target: string;
  regime?: string;
  lane?: string;
  rule_id?: string; // matched rule id; empty = operator default lane
  outcome: string; // "resolved" | "refused"
  created_at: string; // RFC3339
}

// One regime_actuation audit row: the record of one job launch. token_ref is a SecretRef REFERENCE,
// never a value (INV-13).
export interface RegimeActuation {
  action_id: string;
  lane: string;
  job_template_id?: string;
  op_class?: string;
  job_id?: string;
  token_ref?: string; // a SecretRef reference (env:/file:/store:), never a value
  created_at: string; // RFC3339
}

// One deferred_verdict audit row: the terminal-status mechanical verdict + graduation outcome of one
// completed deferred verify (the launch was a prediction; this is the observed outcome).
export interface RegimeDeferredVerdict {
  action_id: string;
  job_id?: string;
  status: string; // terminal AWX status: successful | failed | error | canceled
  verdict: string; // match | deviation | unverified
  graduation: string; // verified_clean | deviated | no_credit
  created_at: string; // RFC3339
}

// One lane's coverage roll-up: how many resolved selections and launches it carries. At mode Shadow every
// count is zero (no lane has been routed through yet).
export interface RegimeLaneCoverage {
  lane: string;
  resolutions: number;
  actuations: number;
}

// The aggregated regime read view GET /v1/regime returns: the recent resolution / actuation /
// deferred-verdict tails plus the per-lane coverage roll-up.
export interface RegimePage {
  resolutions: RegimeResolution[];
  actuations: RegimeActuation[];
  deferred_verdicts: RegimeDeferredVerdict[];
  lane_coverage: RegimeLaneCoverage[];
}

// ---- Skill-store / flywheel read + write surface (spec/014 REQ-1301/1311/1313) ----
// The console's window onto the versioned skill library and the improvement flywheel's A/B trial state.
// Every field mirrors the Go DTO json tags in core/httpapi/skills.go + skills_write.go EXACTLY and is
// NON-SECRET by construction — skill names/kinds, version slugs + status, the append-only rationale log,
// author/source labels, and eval score blobs. The reads are AuthReadOnly; the create-draft + version
// transitions are on the session-only write lane (a machine principal has no write route at all).

// The lightweight applicability predicate a version carries (skillstore.AppliesWhen): the phases and
// execution classes a skill applies to. Optional on both sides; an empty object means "always applies".
export interface AppliesWhen {
  phases?: string[];
  exec_classes?: string[];
}

// One library-list row (GET /v1/skills → { skills: SkillSummary[] }). production_version is the slug of
// the version currently serving in production (empty when none is promoted yet); active_trial flags that
// the flywheel has an open A/B trial on this skill.
export interface SkillSummary {
  name: string;
  kind: string;
  pinned: boolean;
  position: number;
  production_version?: string;
  production_source?: string;
  version_count: number;
  active_trial: boolean;
}
export interface SkillsPage {
  skills: SkillSummary[];
}

// One version row as the console sees it (a member of SkillDetailView.versions, newest first). rationale is
// the append-only transition log — the WHY behind every draft / admission / promotion / rejection, so the
// system's self-improvement is inspectable. eval_offline / eval_online are opaque score blobs the panel
// surfaces verbatim (present only once a run has scored).
export interface SkillVersionView {
  id: number;
  version: string;
  status: string; // draft | trial | production | retired | rejected
  body: string;
  applies_when: AppliesWhen | null;
  content_hash: string;
  author: string;
  source: string;
  rationale: string;
  eval_offline?: unknown;
  eval_online?: unknown;
  ledger_seq?: number;
  created_at: string; // RFC3339
  status_changed_at: string; // RFC3339
}

// The full history view for one skill (GET /v1/skills/{name}): the summary fields plus every version with
// its rationale, newest first.
export interface SkillDetailView extends SkillSummary {
  versions: SkillVersionView[];
}

// One trial row with its per-arm assignment counts (GET /v1/skills/trials → { trials: TrialView[] }).
// assignments maps the variant index (as a string) to its sample count, where "-1" is the CONTROL arm and
// "0", "1", … are the candidates in candidate_ids order; min_samples_per_arm is the fill target each arm
// must reach before the Welch decision runs. A finalized trial carries winner_version_id + note (the
// verdict/rationale). status is "active" while filling, then "completed" | "aborted_timeout" |
// "aborted_no_winner" once the daily decision terminates it.
export interface TrialView {
  id: number;
  skill_name: string;
  dimension: string;
  status: string;
  candidate_ids: number[];
  control_version_id?: number;
  min_samples_per_arm: number;
  min_lift: number;
  p_threshold: number;
  ends_at: string;
  assignments: Record<string, number>;
  winner_version_id?: number;
  note: string;
  created_at: string;
  finalized_at?: string;
}
export interface SkillTrialsPage {
  trials: TrialView[];
}

// The create-draft body (POST /v1/skills/{name}/versions). rationale is MANDATORY at the surface (the
// server 400s an empty one — every version states why it exists); applies_when is the typed predicate.
export interface SkillDraftRequest {
  version: string;
  body: string;
  applies_when: AppliesWhen;
  rationale: string;
}

// The synchronous result of a version transition (POST /v1/skills/versions/{id}/{verb}). ledger_seq is the
// hash-chain sequence the transition was audited at (the write executes in the worker, ledger-before-commit).
export interface SkillTransitionOutcome {
  version_id: number;
  skill_name: string;
  version: string;
  status: string;
  ledger_seq: number;
}

// liveEndpoints are served by the control plane TODAY. contract.test.ts asserts each maps to a real path in
// docs/contracts/openapi.yaml. status reflects the control-plane posture via /v1/stats (mutation_enabled);
// ledger is the governance-ledger tail. The nginx deploy proxies /api/ → the grounder, stripping /api.
export const liveEndpoints = {
  status: "/api/v1/stats",
  ledger: "/api/v1/ledger",
  // Repointed off the dead /v1/decisions/* endpoints the Phase-2 readiness review found 404ing (§4.B/§2).
  // The pending-decisions LIST is the served GET /v1/decisions (REQ-519). An authorized approver acts on a
  // held POLL_PAUSE decision via POST /v1/vote (REQ-518) — the real release, binding {external_ref,
  // action_id, approve} in the body — NOT a 404 /v1/decisions/{id}/action. The runtime mutation kill-switch
  // is the worker's POST /halt (internal admin surface), not a browser route.
  pendingDecisions: "/api/v1/decisions",
  vote: "/api/v1/vote",
  // The browser operator-session surface (REQ-508) the auth gate uses: GET /v1/whoami checks for an
  // existing session cookie (200 = authenticated, 401 = not); POST /v1/session mints one from an
  // operator name + token. Both are read/auth-check routes, never a data fetch.
  whoami: "/api/v1/whoami",
  session: "/api/v1/session",
  // The Actuation Regime Engine read surface (spec/017 T-017-7, REQ-1716): the append-only
  // regime_resolution (target → regime → lane), regime_actuation (launch), and deferred_verdict tails plus
  // the per-lane coverage roll-up. Read-only (AuthReadOnly); the console derives the regime/lane map, the
  // pending-verification queue, and the observed template-allowlist from this one authenticated read.
  regime: "/api/v1/regime",
  // The skill-store / flywheel surface (spec/014 REQ-1301/1311/1313). The library list + the A/B trial
  // state are static AuthReadOnly GETs; the per-skill detail, the create-draft, and the version transition
  // are TEMPLATED routes (all share the /v1/skills base). Every URL is built through one of these entries so
  // no panel ever hand-builds a skill path — the templated builders are resolved + base-matched against the
  // served contract by contract.test.ts, exactly as the planned endpoints are. The two write builders back
  // POSTs on the session-only write lane; the reads are AuthReadOnly.
  skills: "/api/v1/skills",
  skillTrials: "/api/v1/skills/trials",
  skillDetail: (name: string) => `/api/v1/skills/${encodeURIComponent(name)}`,
  skillDraft: (name: string) => `/api/v1/skills/${encodeURIComponent(name)}/versions`,
  skillTransition: (versionID: number, verb: string) => `/api/v1/skills/versions/${versionID}/${verb}`,
} as const;

// plannedEndpoints are specified by the console's data model but NOT yet served. They are enumerated here —
// never hidden inline in a panel — so the gap is explicit and testable. contract.test.ts asserts each is
// still absent from the served contract; the day the server begins serving one, that test fails and forces
// its promotion into liveEndpoints, so the console and the served surface can never silently re-diverge.
export const plannedEndpoints = {
  manifest: (actionID: string) => `/api/v1/manifests/${actionID}`,
} as const;

// endpoints is the combined view existing call sites consume; the live/planned split above is the source of
// truth for what the server actually serves.
export const endpoints = { ...liveEndpoints, ...plannedEndpoints } as const;

// A minimal typed client; the real build swaps this for the generated client with identical shapes.
export interface ApiError {
  status: number;
  message: string;
}
export type ApiResult<T> = { ok: true; data: T } | { ok: false; error: ApiError };
