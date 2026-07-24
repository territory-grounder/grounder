import { useState, type FormEvent } from "react";
import type {
  SkillDetailView,
  SkillDraftRequest,
  SkillSummary,
  SkillVersionView,
  TrialView,
} from "../../api/generated-client";
import type { GuardedState } from "../../lib/guarded-state";
import { a11yButton, a11yStatus } from "../../a11y/baseline";
import {
  appliesWhenSummary,
  availableTransitions,
  deriveArms,
  isActiveTrial,
  trialPhase,
  useCanWrite,
  useSkillDetail,
  useSkills,
  useSkillWrites,
  useTrials,
  type TransitionVerb,
} from "./skills-client";

// The Skills / Flywheel console surface (spec/014 REQ-1301/1311/1313). It makes the improvement flywheel —
// TG's headline "exceeds the predecessor" capability — VISIBLE and OPERABLE: the versioned skill library
// with each skill's production version + status, the live A/B trials with their per-arm fill toward the
// sample floor (the exceed-proof, made legible at a glance), and one skill's full version history with the
// rationale behind every draft / admission / promotion / rejection (self-improvement, inspectable). The
// admin CRUD — draft a version, transition a version — sits behind the session-only write lane: the controls
// render only for an operator session, and the server re-enforces the session, same-origin, and mandatory
// rationale regardless. An auth or transport error fails closed to "restricted"; an empty store renders an
// honest empty state (an invitation, not an error), never a fabricated row.

// ---- shared status helpers -------------------------------------------------------------------------

function Restricted({ label, reason }: { label: string; reason: "auth" | "transport" | "read-only" }) {
  const why =
    reason === "auth" ? "not authorized" : reason === "transport" ? "currently unavailable" : "read-only";
  return <p {...a11yStatus(`${label} restricted: ${why}`)}>{label} unavailable — {why}.</p>;
}

// ---- flywheel trials -------------------------------------------------------------------------------

// ArmFill renders one arm's progress toward the sample minimum: the numeric fill carries the meaning (colour
// is never the sole signal), and the bar is a decorative mirror of it. data-filled marks a full arm.
function ArmFill({ label, samples, needed, filled, versionId }: { label: string; samples: number; needed: number; filled: boolean; versionId?: number }) {
  const pct = needed > 0 ? Math.min(100, Math.round((samples / needed) * 100)) : filled ? 100 : 0;
  return (
    <div className="arm" data-testid="trial-arm" data-role={label.startsWith("control") ? "control" : "candidate"} data-filled={String(filled)}>
      <div className="arm-head">
        <span className="arm-label">
          {label}
          {versionId ? <span className="arm-ver"> · v#{versionId}</span> : null}
        </span>
        <span className="arm-count">
          {samples} / {needed}
          {filled ? <span className="arm-full"> full</span> : null}
        </span>
      </div>
      <div className="arm-bar" role="progressbar" aria-label={`${label} arm ${samples} of ${needed} samples`} aria-valuenow={samples} aria-valuemin={0} aria-valuemax={needed}>
        <div className="arm-bar-fill" style={{ width: `${pct}%` }} aria-hidden />
      </div>
    </div>
  );
}

function TrialCard({ trial }: { trial: TrialView }) {
  const arms = deriveArms(trial);
  const phase = trialPhase(trial.status);
  const summary = arms.map((a) => `${a.label} ${a.samples}/${a.needed}`).join(" · ");
  const active = phase === "active";
  return (
    <article className="trial" data-testid="trial-card" data-phase={phase} data-status={trial.status}>
      <header>
        <span className="trial-skill">
          <code>{trial.skill_name}</code>
        </span>
        <span className="trial-dim">{trial.dimension || "—"}</span>
        <span className="chip" data-phase={phase}>{trial.status}</span>
      </header>

      {/* the exceed-proof, legible at a glance: per-arm fill toward the sample floor */}
      <p className="arm-summary" data-testid="arm-summary">{summary}</p>
      <div className="arms">
        {arms.map((a) => (
          <ArmFill key={a.key} label={a.label} samples={a.samples} needed={a.needed} filled={a.filled} versionId={a.versionId} />
        ))}
      </div>

      {active ? (
        <p className="trial-meta">
          min lift {trial.min_lift} · p&lt;{trial.p_threshold}
          {trial.ends_at ? ` · ends ${trial.ends_at}` : ""}
        </p>
      ) : (
        // the verdict + rationale of a finalized trial (winner, if any, and the append-only note)
        <p className="trial-verdict" data-testid="trial-verdict">
          {trial.winner_version_id ? <>winner: <code>v#{trial.winner_version_id}</code> — </> : "no winner — "}
          {trial.note || "no rationale recorded"}
        </p>
      )}
    </article>
  );
}

export function TrialsBoard({ state }: { state: GuardedState<TrialView[]> }) {
  if (state.status === "loading") {
    return (
      <section aria-label="Flywheel trials">
        <h3>Flywheel trials</h3>
        <p {...a11yStatus("Loading flywheel trials")}>Loading flywheel trials…</p>
      </section>
    );
  }
  if (state.status === "restricted") {
    return (
      <section aria-label="Flywheel trials">
        <h3>Flywheel trials</h3>
        <Restricted label="Flywheel trials" reason={state.reason} />
      </section>
    );
  }
  const trials = state.data;
  const activeTrials = trials.filter(isActiveTrial);
  const finalized = trials.filter((t) => !isActiveTrial(t));

  return (
    <section aria-label="Flywheel trials">
      <h3>Flywheel trials</h3>
      {trials.length === 0 ? (
        <p {...a11yStatus("No flywheel trials yet")} data-testid="trials-empty">
          No flywheel trials yet — the improvement flywheel opens an A/B trial when it drafts a candidate.
          When one runs, its arm-fill shows here.
        </p>
      ) : (
        <>
          <section aria-label="Active trials">
            <p className="eyebrow">Active · {activeTrials.length}</p>
            {activeTrials.length === 0 ? (
              <p {...a11yStatus("No active trials")} data-testid="active-empty">No trial is currently filling.</p>
            ) : (
              <div className="trial-list">
                {activeTrials.map((t) => (
                  <TrialCard key={t.id} trial={t} />
                ))}
              </div>
            )}
          </section>
          {finalized.length > 0 && (
            <section aria-label="Completed trials">
              <p className="eyebrow">Finalized · {finalized.length}</p>
              <div className="trial-list">
                {finalized.map((t) => (
                  <TrialCard key={t.id} trial={t} />
                ))}
              </div>
            </section>
          )}
        </>
      )}
    </section>
  );
}

// ---- skill library ---------------------------------------------------------------------------------

function SkillRow({ skill, selected, onSelect }: { skill: SkillSummary; selected: boolean; onSelect: (name: string) => void }) {
  return (
    <li>
      <button
        type="button"
        className="skill-row"
        data-testid="skill-row"
        data-selected={String(selected)}
        aria-pressed={selected}
        onClick={() => onSelect(skill.name)}
      >
        <span className="skill-name">
          <code>{skill.name}</code>
          {skill.pinned ? <span className="chip" data-tone="pause" title="pinned — the never-override floor">pinned</span> : null}
        </span>
        <span className="skill-prod">
          {skill.production_version ? <>v{skill.production_version}</> : "no production version"}
        </span>
        <span className="skill-meta">
          {skill.kind} · {skill.version_count} version{skill.version_count === 1 ? "" : "s"}
          {skill.active_trial ? <span className="chip" data-phase="active">trial</span> : null}
        </span>
      </button>
    </li>
  );
}

export function SkillLibrary({ state, selected, onSelect }: { state: GuardedState<SkillSummary[]>; selected: string | null; onSelect: (name: string) => void }) {
  if (state.status === "loading") {
    return (
      <section aria-label="Skill library">
        <h3>Skill library</h3>
        <p {...a11yStatus("Loading skill library")}>Loading skill library…</p>
      </section>
    );
  }
  if (state.status === "restricted") {
    return (
      <section aria-label="Skill library">
        <h3>Skill library</h3>
        <Restricted label="Skill library" reason={state.reason} />
      </section>
    );
  }
  const skills = [...state.data].sort((a, b) => (a.position || 0) - (b.position || 0));
  return (
    <section aria-label="Skill library">
      <h3>Skill library</h3>
      {skills.length === 0 ? (
        <p {...a11yStatus("The skill library is empty")} data-testid="skills-empty">
          The skill library is empty — the boot importer seeds the store from the compiled registry on the
          worker's first start. Once seeded, each skill and its production version show here.
        </p>
      ) : (
        <ul className="skill-list">
          {skills.map((s) => (
            <SkillRow key={s.name} skill={s} selected={s.name === selected} onSelect={onSelect} />
          ))}
        </ul>
      )}
    </section>
  );
}

// ---- write controls (session-only) -----------------------------------------------------------------

// TransitionControls renders the operator-available transitions for a version, each behind a mandatory
// rationale gate (the server 400s an empty rationale — every transition states why). Rendered only when
// canWrite is true (an operator session); a read-only session sees the version history without these.
function TransitionControls({ version, canWrite, onTransition, pending }: {
  version: SkillVersionView;
  canWrite: boolean;
  onTransition: (versionID: number, verb: TransitionVerb, rationale: string) => void;
  pending: boolean;
}) {
  const [openVerb, setOpenVerb] = useState<TransitionVerb | null>(null);
  const [rationale, setRationale] = useState("");
  const options = availableTransitions(version.status);
  if (!canWrite || options.length === 0) return null;

  if (openVerb) {
    const confirm = (e: FormEvent) => {
      e.preventDefault();
      const r = rationale.trim();
      if (!r) return;
      onTransition(version.id, openVerb, r);
      setOpenVerb(null);
      setRationale("");
    };
    return (
      <form className="transition-gate" onSubmit={confirm} aria-label={`Rationale for ${openVerb} of version ${version.version}`}>
        <label className="sk-field">
          <span className="sk-field-label">Why {openVerb} v{version.version}? (required)</span>
          <textarea
            data-testid="transition-rationale"
            value={rationale}
            onChange={(e) => setRationale(e.target.value)}
            rows={2}
            autoFocus
          />
        </label>
        <div role="group" aria-label="Confirm transition">
          <button type="submit" {...a11yButton(`Confirm ${openVerb} v${version.version}`, pending || !rationale.trim())} disabled={pending || !rationale.trim()}>
            Confirm {openVerb}
          </button>
          <button type="button" {...a11yButton("Cancel transition", false)} onClick={() => { setOpenVerb(null); setRationale(""); }}>
            Cancel
          </button>
        </div>
      </form>
    );
  }

  return (
    <div role="group" aria-label={`Transitions for version ${version.version}`} className="transition-verbs">
      {options.map((o) => (
        <button
          key={o.verb}
          type="button"
          data-testid={`transition-${o.verb}`}
          data-action={o.verb === "retire" ? "veto" : undefined}
          {...a11yButton(`${o.label} v${version.version}`, pending)}
          disabled={pending}
          onClick={() => setOpenVerb(o.verb)}
        >
          {o.label}
        </button>
      ))}
    </div>
  );
}

function VersionRow({ version, canWrite, onTransition, pending }: {
  version: SkillVersionView;
  canWrite: boolean;
  onTransition: (versionID: number, verb: TransitionVerb, rationale: string) => void;
  pending: boolean;
}) {
  return (
    <li className="version-row" data-testid="version-row" data-status={version.status}>
      <div className="version-head">
        <span className="version-slug"><code>v{version.version}</code></span>
        <span className="chip" data-status={version.status}>{version.status}</span>
        <span className="version-src">{version.source || version.author || "—"}</span>
        {version.ledger_seq ? <span className="version-seq">ledger #{version.ledger_seq}</span> : null}
      </div>
      <p className="version-applies">{appliesWhenSummary(version.applies_when)}</p>
      {/* the WHY behind this version — the append-only rationale log, always visible (inspectable) */}
      <p className="version-rationale" data-testid="version-rationale">
        {version.rationale || "no rationale recorded"}
      </p>
      <TransitionControls version={version} canWrite={canWrite} onTransition={onTransition} pending={pending} />
    </li>
  );
}

// DraftForm creates a new version of the selected skill (POST /v1/skills/{name}/versions). Rendered only for
// an operator session. Rationale is required (the submit stays disabled until it is filled), mirroring the
// server's own mandatory-rationale rule; phases + classes build the typed applies_when predicate.
function DraftForm({ skillName, onDraft, pending, error }: {
  skillName: string;
  onDraft: (req: SkillDraftRequest) => void;
  pending: boolean;
  error: string | null;
}) {
  const [open, setOpen] = useState(false);
  const [version, setVersion] = useState("");
  const [body, setBody] = useState("");
  const [rationale, setRationale] = useState("");
  const [phases, setPhases] = useState("");
  const [classes, setClasses] = useState("");

  if (!open) {
    return (
      <button type="button" data-testid="open-draft" {...a11yButton(`Draft a new version of ${skillName}`, false)} onClick={() => setOpen(true)}>
        Draft a new version
      </button>
    );
  }

  const list = (s: string) => s.split(",").map((x) => x.trim()).filter(Boolean);
  const submit = (e: FormEvent) => {
    e.preventDefault();
    if (!version.trim() || !rationale.trim()) return;
    onDraft({
      version: version.trim(),
      body,
      rationale: rationale.trim(),
      applies_when: { phases: list(phases), exec_classes: list(classes) },
    });
  };

  return (
    <form className="draft-form" onSubmit={submit} aria-label={`Draft a new version of ${skillName}`}>
      <label className="sk-field">
        <span className="sk-field-label">Version label (required)</span>
        <input data-testid="draft-version" value={version} onChange={(e) => setVersion(e.target.value)} autoCapitalize="none" spellCheck={false} />
      </label>
      <label className="sk-field">
        <span className="sk-field-label">Body</span>
        <textarea data-testid="draft-body" value={body} onChange={(e) => setBody(e.target.value)} rows={4} />
      </label>
      <div className="sk-field-row">
        <label className="sk-field">
          <span className="sk-field-label">Phases (comma-separated)</span>
          <input data-testid="draft-phases" value={phases} onChange={(e) => setPhases(e.target.value)} />
        </label>
        <label className="sk-field">
          <span className="sk-field-label">Exec classes (comma-separated)</span>
          <input data-testid="draft-classes" value={classes} onChange={(e) => setClasses(e.target.value)} />
        </label>
      </div>
      <label className="sk-field">
        <span className="sk-field-label">Rationale — why this version exists (required)</span>
        <textarea data-testid="draft-rationale" value={rationale} onChange={(e) => setRationale(e.target.value)} rows={2} />
      </label>
      {error ? <p className="sk-error" role="alert">{error}</p> : null}
      <div role="group" aria-label="Draft actions">
        <button type="submit" {...a11yButton("Create draft version", pending || !version.trim() || !rationale.trim())} disabled={pending || !version.trim() || !rationale.trim()}>
          {pending ? "Drafting…" : "Create draft"}
        </button>
        <button type="button" {...a11yButton("Cancel draft", false)} onClick={() => setOpen(false)}>Cancel</button>
      </div>
    </form>
  );
}

// ---- skill detail (version history + rationale + write CRUD) ---------------------------------------

export function SkillDetailPanel({ state, canWrite, onDraft, onTransition, pending, error }: {
  state: GuardedState<SkillDetailView>;
  canWrite: boolean;
  onDraft: (req: SkillDraftRequest) => void;
  onTransition: (versionID: number, verb: TransitionVerb, rationale: string) => void;
  pending: boolean;
  error: string | null;
}) {
  if (state.status === "restricted" && state.reason === "read-only") {
    // no skill selected yet — an invitation, not an error
    return (
      <section aria-label="Skill detail">
        <h3>Version history</h3>
        <p {...a11yStatus("No skill selected")} data-testid="detail-none">
          Select a skill from the library to see its version history and the rationale behind every change.
        </p>
      </section>
    );
  }
  if (state.status === "loading") {
    return (
      <section aria-label="Skill detail">
        <h3>Version history</h3>
        <p {...a11yStatus("Loading skill detail")}>Loading version history…</p>
      </section>
    );
  }
  if (state.status === "restricted") {
    return (
      <section aria-label="Skill detail">
        <h3>Version history</h3>
        <Restricted label="Skill detail" reason={state.reason} />
      </section>
    );
  }
  const detail = state.data;
  return (
    <section aria-label="Skill detail">
      <h3>Version history — <code>{detail.name}</code></h3>
      <p className="eyebrow">
        {detail.kind} · {detail.production_version ? `production v${detail.production_version}` : "no production version"} · {detail.version_count} version{detail.version_count === 1 ? "" : "s"}
      </p>
      {canWrite ? (
        <DraftForm skillName={detail.name} onDraft={onDraft} pending={pending} error={error} />
      ) : (
        <p className="eyebrow" data-testid="detail-readonly">Read-only session — sign in as an operator to draft or transition versions.</p>
      )}
      {detail.versions.length === 0 ? (
        <p {...a11yStatus("No versions recorded")} data-testid="versions-empty">No versions recorded for this skill yet.</p>
      ) : (
        <ul className="version-list">
          {detail.versions.map((v) => (
            <VersionRow key={v.id} version={v} canWrite={canWrite} onTransition={onTransition} pending={pending} />
          ))}
        </ul>
      )}
    </section>
  );
}

// ---- composed view + live container ----------------------------------------------------------------

export interface SkillsViewProps {
  skills: GuardedState<SkillSummary[]>;
  trials: GuardedState<TrialView[]>;
  detail: GuardedState<SkillDetailView>;
  selected: string | null;
  canWrite: boolean;
  onSelect: (name: string) => void;
  onDraft: (req: SkillDraftRequest) => void;
  onTransition: (versionID: number, verb: TransitionVerb, rationale: string) => void;
  writePending: boolean;
  writeError: string | null;
}

// SkillsView is the presentational composition — fully prop-driven so it is rendered under test without a
// network. It computes no safety decision: the write controls are gated purely on canWrite (an operator
// session), and the server re-enforces authority on every write regardless of what the browser shows.
export function SkillsView(props: SkillsViewProps) {
  return (
    <section aria-label="Skills and flywheel">
      <TrialsBoard state={props.trials} />
      <div className="skills-split">
        <SkillLibrary state={props.skills} selected={props.selected} onSelect={props.onSelect} />
        <SkillDetailPanel
          state={props.detail}
          canWrite={props.canWrite}
          onDraft={props.onDraft}
          onTransition={props.onTransition}
          pending={props.writePending}
          error={props.writeError}
        />
      </div>
    </section>
  );
}

// SkillsPanel is the live container: it performs the three authenticated reads (library, trials, and — once
// a skill is selected — that skill's history) plus the whoami write-capability check, wires the two writes to
// the shared query client (invalidating on success), and hands the resulting state to SkillsView.
export function SkillsPanel() {
  const [selected, setSelected] = useState<string | null>(null);
  const skills = useSkills();
  const trials = useTrials();
  const detail = useSkillDetail(selected);
  const canWrite = useCanWrite();
  const { draft, transition } = useSkillWrites(selected);

  const writeError = (draft.error?.message || transition.error?.message) ?? null;
  const writePending = draft.isPending || transition.isPending;

  return (
    <SkillsView
      skills={skills}
      trials={trials}
      detail={detail}
      selected={selected}
      canWrite={canWrite}
      onSelect={setSelected}
      onDraft={(req) => draft.mutate(req)}
      onTransition={(versionID, verb, rationale) => transition.mutate({ versionID, verb, rationale })}
      writePending={writePending}
      writeError={writeError}
    />
  );
}
