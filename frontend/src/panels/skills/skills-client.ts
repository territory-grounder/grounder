// The skills / flywheel panel's data access + the pure projections it renders. The reads come from three
// authenticated GETs — GET /v1/skills (the library), GET /v1/skills/trials (the A/B trial state), and
// GET /v1/skills/{name} (one skill's version history, fetched on demand) — plus GET /v1/whoami to learn
// whether THIS session may use the session-only write lane. An auth or transport error fails closed to the
// restrictive guarded-state, so the browser never fabricates a skill, a version, or a trial row. The two
// writes (draft a version, transition a version) POST to the session-only routes; a machine principal has
// no write route at all, so the panel only shows write controls to an operator session and the server
// re-enforces regardless. The derivations below are pure so they are unit-tested without rendering.
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  endpoints,
  type ApiError,
  type SkillDetailView,
  type SkillDraftRequest,
  type SkillSummary,
  type SkillTransitionOutcome,
  type SkillVersionView,
  type TrialView,
} from "../../api/generated-client";
import { guardedFromError, guardedReady, type GuardedState } from "../../lib/guarded-state";

// The closed set of route verbs the transition surface exposes (POST /v1/skills/versions/{id}/{verb}).
// This mirrors core/httpapi/skills_write.go transitionForRoute EXACTLY — the console never invents a verb.
export type TransitionVerb = "trial" | "promote" | "retire";

// ---- reads -----------------------------------------------------------------------------------------

// getJSON performs one authenticated read: the browser session cookie is the credential (same mechanism
// session-client + the regime panel use). A non-2xx or a network failure throws an ApiError whose status
// guarded-state maps to the restrictive (auth/transport) state, so a caller never renders fabricated data.
async function getJSON<T>(url: string, what: string): Promise<T> {
  let res: Response;
  try {
    res = await fetch(url, { credentials: "same-origin" });
  } catch {
    const err: ApiError = { status: 0, message: `network error reading ${what}` };
    throw err;
  }
  if (!res.ok) {
    const err: ApiError = { status: res.status, message: `${what} read failed (${res.status})` };
    throw err;
  }
  const body = (await res.json().catch(() => null)) as T | null;
  if (body === null) {
    const err: ApiError = { status: 0, message: `${what} read returned no body` };
    throw err;
  }
  return body;
}

export function fetchSkills(): Promise<SkillSummary[]> {
  return getJSON<{ skills: SkillSummary[] }>(endpoints.skills, "the skill library").then((p) => p.skills ?? []);
}

export function fetchTrials(): Promise<TrialView[]> {
  return getJSON<{ trials: TrialView[] }>(endpoints.skillTrials, "the flywheel trials").then((p) => p.trials ?? []);
}

export function fetchSkillDetail(name: string): Promise<SkillDetailView> {
  return getJSON<SkillDetailView>(endpoints.skillDetail(name), `skill "${name}"`);
}

// canWrite asks whether the CURRENT principal may use the session-only skill-write lane. The honest signal
// the server hands the browser is the source id: an operator browser session is "operator:<name>", whereas a
// machine (HMAC/mTLS) read-only principal carries a bare source id and has NO write route. So a write control
// renders only for an operator session; the server re-enforces the session + same-origin + rationale anyway.
export async function fetchCanWrite(): Promise<boolean> {
  const who = await getJSON<{ source?: string }>(endpoints.whoami, "the caller identity");
  return typeof who.source === "string" && who.source.startsWith("operator:");
}

export function useSkills(): GuardedState<SkillSummary[]> {
  const q = useQuery<SkillSummary[], ApiError>({ queryKey: ["skills"], queryFn: fetchSkills });
  if (q.isPending) return { status: "loading" };
  if (q.isError) return guardedFromError<SkillSummary[]>(q.error);
  return guardedReady(q.data, false, false);
}

export function useTrials(): GuardedState<TrialView[]> {
  const q = useQuery<TrialView[], ApiError>({ queryKey: ["skill-trials"], queryFn: fetchTrials });
  if (q.isPending) return { status: "loading" };
  if (q.isError) return guardedFromError<TrialView[]>(q.error);
  return guardedReady(q.data, false, false);
}

// useSkillDetail fetches the selected skill's history on demand (enabled only when a name is selected). A
// null name resolves to the "restricted/read-only" state, which the view renders as "select a skill".
export function useSkillDetail(name: string | null): GuardedState<SkillDetailView> {
  const q = useQuery<SkillDetailView, ApiError>({
    queryKey: ["skill-detail", name],
    queryFn: () => fetchSkillDetail(name as string),
    enabled: name !== null,
  });
  if (name === null) return { status: "restricted", reason: "read-only" };
  if (q.isPending) return { status: "loading" };
  if (q.isError) return guardedFromError<SkillDetailView>(q.error);
  return guardedReady(q.data, false, false);
}

// useCanWrite reflects the session's write capability (see fetchCanWrite). It fails closed to false — an
// unknown identity is treated as read-only, so a fetch error never speculatively reveals a write control.
export function useCanWrite(): boolean {
  const q = useQuery<boolean, ApiError>({ queryKey: ["skills-can-write"], queryFn: fetchCanWrite });
  return q.data === true;
}

// ---- writes ----------------------------------------------------------------------------------------

// postJSON drives one session-only write: same-origin POST with the session cookie. It throws an ApiError
// carrying the server's own message on a non-2xx (the server states WHY — "rationale required — every
// version states why it exists", a bad transition, a pinned skill), so the form shows the real reason.
async function postJSON<T>(url: string, body: unknown, what: string): Promise<T> {
  let res: Response;
  try {
    res = await fetch(url, {
      method: "POST",
      credentials: "same-origin",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
  } catch {
    const err: ApiError = { status: 0, message: `network error — ${what} not sent` };
    throw err;
  }
  if (!res.ok) {
    const text = (await res.text().catch(() => "")).trim();
    const err: ApiError = { status: res.status, message: text || `${what} failed (${res.status})` };
    throw err;
  }
  return (await res.json().catch(() => ({}))) as T;
}

export function draftVersion(name: string, req: SkillDraftRequest): Promise<SkillVersionView> {
  return postJSON<SkillVersionView>(endpoints.skillDraft(name), req, "draft");
}

export function transitionVersion(versionID: number, verb: TransitionVerb, rationale: string): Promise<SkillTransitionOutcome> {
  return postJSON<SkillTransitionOutcome>(endpoints.skillTransition(versionID, verb), { rationale }, `${verb} transition`);
}

// useSkillWrites wires the two writes to react-query mutations and invalidates the affected reads on success
// (the library, the trials, and the touched skill's detail) so the panel reflects the new state without a
// manual refetch. The panel calls draft.mutate / transition.mutate and reads the shared pending/error state.
export function useSkillWrites(skillName: string | null) {
  const qc = useQueryClient();
  const invalidate = () => {
    void qc.invalidateQueries({ queryKey: ["skills"] });
    void qc.invalidateQueries({ queryKey: ["skill-trials"] });
    if (skillName) void qc.invalidateQueries({ queryKey: ["skill-detail", skillName] });
  };
  const draft = useMutation<SkillVersionView, ApiError, SkillDraftRequest>({
    mutationFn: (req) => draftVersion(skillName as string, req),
    onSuccess: invalidate,
  });
  const transition = useMutation<SkillTransitionOutcome, ApiError, { versionID: number; verb: TransitionVerb; rationale: string }>({
    mutationFn: ({ versionID, verb, rationale }) => transitionVersion(versionID, verb, rationale),
    onSuccess: invalidate,
  });
  return { draft, transition };
}

// ---- pure projections ------------------------------------------------------------------------------

// TrialArm is one arm's fill toward the sample minimum. role distinguishes the CONTROL arm (the incumbent)
// from each CANDIDATE; filled is samples ≥ needed (the arm has reached the Welch decision's sample floor).
export interface TrialArm {
  key: string; // the assignments key: "-1" = control, "0" | "1" | … = candidate index
  label: string; // "control" | "candidate" | "candidate 2" …
  role: "control" | "candidate";
  versionId?: number; // the version this arm serves (control_version_id, or candidate_ids[i])
  samples: number;
  needed: number;
  filled: boolean;
}

// deriveArms projects a trial's raw assignment counts into the per-arm fill the console renders: the control
// arm first (assignments["-1"]), then each candidate in candidate_ids order (assignments["0"], "1", …), each
// against min_samples_per_arm. This is the exceed-proof made legible — "control 11/15 · candidate 5/15".
export function deriveArms(t: TrialView): TrialArm[] {
  const needed = t.min_samples_per_arm;
  const a = t.assignments ?? {};
  const control = a["-1"] ?? 0;
  const arms: TrialArm[] = [
    { key: "-1", label: "control", role: "control", versionId: t.control_version_id, samples: control, needed, filled: control >= needed },
  ];
  const many = (t.candidate_ids ?? []).length > 1;
  (t.candidate_ids ?? []).forEach((vid, i) => {
    const s = a[String(i)] ?? 0;
    arms.push({
      key: String(i),
      label: many ? `candidate ${i + 1}` : "candidate",
      role: "candidate",
      versionId: vid,
      samples: s,
      needed,
      filled: s >= needed,
    });
  });
  return arms;
}

// TrialPhase collapses the raw status into the active-vs-finalized distinction the board groups by. The raw
// status is still shown (e.g. "aborted_timeout") — this is only for the active/completed/aborted split.
export type TrialPhase = "active" | "completed" | "aborted";

export function trialPhase(status: string): TrialPhase {
  if (status === "active") return "active";
  if (status === "completed") return "completed";
  return "aborted"; // aborted_timeout | aborted_no_winner | any other terminal status
}

export function isActiveTrial(t: TrialView): boolean {
  return trialPhase(t.status) === "active";
}

// TransitionOption is one operator-available transition for a version, given its current status. It maps a
// route verb to a plain-language label. The mapping mirrors the state machine's allowed edges that the HTTP
// verb table actually exposes: draft → trial, trial → production (promote), production → retired (retire).
export interface TransitionOption {
  verb: TransitionVerb;
  label: string;
}

export function availableTransitions(status: string): TransitionOption[] {
  switch (status) {
    case "draft":
      return [{ verb: "trial", label: "Admit to trial" }];
    case "trial":
      return [{ verb: "promote", label: "Promote to production" }];
    case "production":
      return [{ verb: "retire", label: "Retire" }];
    default:
      return []; // retired | rejected are terminal — no operator transition
  }
}

// appliesWhenSummary renders the typed predicate as a short human line ("phases: 1,2 · classes: net") for
// the version history. An empty predicate means the skill always applies.
export function appliesWhenSummary(w: SkillVersionView["applies_when"]): string {
  const phases = w?.phases ?? [];
  const classes = w?.exec_classes ?? [];
  const parts: string[] = [];
  if (phases.length) parts.push(`phases: ${phases.join(", ")}`);
  if (classes.length) parts.push(`classes: ${classes.join(", ")}`);
  return parts.length ? parts.join(" · ") : "always applies";
}
