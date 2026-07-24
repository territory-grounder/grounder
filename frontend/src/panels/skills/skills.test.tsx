import { describe, it, expect, vi, afterEach } from "vitest";
import { render, screen, fireEvent, within } from "@testing-library/react";
import { guardedFromError, guardedReady, type GuardedState } from "../../lib/guarded-state";
import type { SkillDetailView, SkillSummary, TrialView } from "../../api/generated-client";
import { SkillLibrary, SkillDetailPanel, TrialsBoard } from "./skills-panel";
import {
  appliesWhenSummary,
  availableTransitions,
  deriveArms,
  fetchCanWrite,
  fetchSkills,
  fetchTrials,
  isActiveTrial,
  trialPhase,
} from "./skills-client";

// Real-shape fixtures mirroring the Go DTOs (core/httpapi/skills.go). Two library skills (one pinned with a
// production version + an active trial), three trials (an active one mid-fill, one completed with a winner,
// one aborted on timeout), and one skill's version history with a rationale per version.
const skills: SkillSummary[] = [
  { name: "conservative-remediation", kind: "behavioral", pinned: true, position: 0, production_version: "3", production_source: "compiled-import", version_count: 3, active_trial: false },
  { name: "triage-first-hypothesis", kind: "behavioral", pinned: false, position: 1, production_version: "2", production_source: "flywheel", version_count: 4, active_trial: true },
];

const trials: TrialView[] = [
  {
    id: 501, skill_name: "triage-first-hypothesis", dimension: "prompt-body", status: "active",
    candidate_ids: [1002], control_version_id: 1001, min_samples_per_arm: 15, min_lift: 0.05, p_threshold: 0.05,
    ends_at: "2026-07-27T00:00:00Z", assignments: { "-1": 11, "0": 5 }, note: "", created_at: "2026-07-20T00:00:00Z",
  },
  {
    id: 502, skill_name: "conservative-remediation", dimension: "applies-when", status: "completed",
    candidate_ids: [2002], control_version_id: 2001, min_samples_per_arm: 12, min_lift: 0.05, p_threshold: 0.05,
    ends_at: "2026-07-18T00:00:00Z", assignments: { "-1": 12, "0": 12 }, winner_version_id: 2002,
    note: "candidate beat control by 0.11 (p=0.02) — graduated to production.", created_at: "2026-07-10T00:00:00Z", finalized_at: "2026-07-18T00:00:00Z",
  },
  {
    id: 503, skill_name: "syslog-correlation", dimension: "prompt-body", status: "aborted_timeout",
    candidate_ids: [3002], control_version_id: 3001, min_samples_per_arm: 15, min_lift: 0.05, p_threshold: 0.05,
    ends_at: "2026-07-12T00:00:00Z", assignments: { "-1": 4, "0": 3 }, note: "window elapsed with arms below the sample minimum.",
    created_at: "2026-06-28T00:00:00Z", finalized_at: "2026-07-12T00:00:00Z",
  },
];

const detail: SkillDetailView = {
  ...skills[1],
  versions: [
    { id: 1002, version: "3", status: "trial", body: "…", applies_when: { phases: ["1"], exec_classes: ["network-control"] }, content_hash: "ab", author: "flywheel", source: "flywheel", rationale: "flywheel candidate: lead with the single most-likely hypothesis before enumerating.", ledger_seq: 88, created_at: "2026-07-20T00:00:00Z", status_changed_at: "2026-07-20T00:00:00Z" },
    { id: 1001, version: "2", status: "production", body: "…", applies_when: {}, content_hash: "cd", author: "operator:kp", source: "hand", rationale: "hand-authored: promoted after the disk-fill trial win.", ledger_seq: 71, created_at: "2026-07-12T00:00:00Z", status_changed_at: "2026-07-12T00:00:00Z" },
  ],
};

const emptySkills: GuardedState<SkillSummary[]> = guardedReady([], false, false);
const emptyTrials: GuardedState<TrialView[]> = guardedReady([], false, false);

describe("skills derivations (pure projections of the real /v1/skills tails)", () => {
  it("deriveArms yields control first then each candidate, each against the sample floor", () => {
    const arms = deriveArms(trials[0]);
    expect(arms.map((a) => `${a.label} ${a.samples}/${a.needed}`)).toEqual(["control 11/15", "candidate 5/15"]);
    expect(arms[0].role).toBe("control");
    expect(arms[0].versionId).toBe(1001); // control_version_id
    expect(arms[1].versionId).toBe(1002); // candidate_ids[0]
    expect(arms.every((a) => a.filled)).toBe(false); // neither arm has reached 15
  });

  it("deriveArms flags a filled arm and numbers multiple candidates", () => {
    const filled = deriveArms(trials[1]); // 12/12 both arms
    expect(filled.every((a) => a.filled)).toBe(true);
    const multi = deriveArms({ ...trials[0], candidate_ids: [10, 11], assignments: { "-1": 2, "0": 3, "1": 7 } });
    expect(multi.map((a) => a.label)).toEqual(["control", "candidate 1", "candidate 2"]);
  });

  it("trialPhase splits active from the finalized (completed / aborted) terminal states", () => {
    expect(trialPhase("active")).toBe("active");
    expect(trialPhase("completed")).toBe("completed");
    expect(trialPhase("aborted_timeout")).toBe("aborted");
    expect(trialPhase("aborted_no_winner")).toBe("aborted");
    expect(isActiveTrial(trials[0])).toBe(true);
    expect(isActiveTrial(trials[1])).toBe(false);
  });

  it("availableTransitions maps a version's status to only the verbs the route table exposes", () => {
    expect(availableTransitions("draft").map((o) => o.verb)).toEqual(["trial"]);
    expect(availableTransitions("trial").map((o) => o.verb)).toEqual(["promote"]);
    expect(availableTransitions("production").map((o) => o.verb)).toEqual(["retire"]);
    expect(availableTransitions("retired")).toEqual([]);
    expect(availableTransitions("rejected")).toEqual([]);
  });

  it("appliesWhenSummary renders the typed predicate, or 'always applies' when empty", () => {
    expect(appliesWhenSummary({ phases: ["1"], exec_classes: ["net"] })).toBe("phases: 1 · classes: net");
    expect(appliesWhenSummary({})).toBe("always applies");
    expect(appliesWhenSummary(null)).toBe("always applies");
  });
});

describe("TrialsBoard renders the flywheel state and fails closed", () => {
  it("an empty spine is an honest invitation, not an error or a fabricated row", () => {
    render(<TrialsBoard state={emptyTrials} />);
    expect(screen.getByTestId("trials-empty")).toBeInTheDocument();
    expect(screen.queryByTestId("trial-card")).toBeNull();
  });

  it("an auth error fails closed to 'not authorized' (no trials shown)", () => {
    render(<TrialsBoard state={guardedFromError<TrialView[]>({ status: 403 })} />);
    expect(screen.getByRole("status")).toHaveTextContent(/not authorized/i);
    expect(screen.queryByTestId("trial-card")).toBeNull();
  });

  it("shows the per-arm fill at a glance and splits active from finalized", () => {
    render(<TrialsBoard state={guardedReady(trials, false, false)} />);
    const cards = screen.getAllByTestId("trial-card");
    expect(cards).toHaveLength(3);
    // the exceed-proof, legible: control 11/15 · candidate 5/15
    expect(screen.getByText("control 11/15 · candidate 5/15")).toBeInTheDocument();
    // an active trial is marked active; a finalized one carries its verdict + rationale
    const active = cards.find((c) => c.getAttribute("data-phase") === "active");
    expect(active).toBeTruthy();
    const completed = cards.find((c) => c.getAttribute("data-status") === "completed")!;
    expect(within(completed).getByTestId("trial-verdict")).toHaveTextContent(/winner: v#2002/);
    expect(within(completed).getByTestId("trial-verdict")).toHaveTextContent(/graduated to production/);
    const aborted = cards.find((c) => c.getAttribute("data-phase") === "aborted")!;
    expect(within(aborted).getByTestId("trial-verdict")).toHaveTextContent(/no winner/);
  });

  it("renders one progressbar per arm with the sample floor as its max", () => {
    render(<TrialsBoard state={guardedReady([trials[0]], false, false)} />);
    const bars = screen.getAllByRole("progressbar");
    expect(bars).toHaveLength(2);
    expect(bars[0]).toHaveAttribute("aria-valuenow", "11");
    expect(bars[0]).toHaveAttribute("aria-valuemax", "15");
  });
});

describe("SkillLibrary lists the versioned library and selects a skill", () => {
  it("shows an honest empty state when the store is unseeded", () => {
    render(<SkillLibrary state={emptySkills} selected={null} onSelect={() => {}} />);
    expect(screen.getByTestId("skills-empty")).toBeInTheDocument();
  });

  it("lists skills with their production version and marks the selected + trial + pinned ones", () => {
    const onSelect = vi.fn();
    render(<SkillLibrary state={guardedReady(skills, false, false)} selected="triage-first-hypothesis" onSelect={onSelect} />);
    const rows = screen.getAllByTestId("skill-row");
    expect(rows).toHaveLength(2);
    expect(screen.getByText(/^v3$/)).toBeInTheDocument(); // production version shown for the first skill
    const selected = rows.find((r) => r.getAttribute("data-selected") === "true")!;
    expect(selected).toHaveTextContent("triage-first-hypothesis");
    expect(within(selected).getByText("trial")).toBeInTheDocument(); // active_trial chip
    fireEvent.click(rows[0]);
    expect(onSelect).toHaveBeenCalledWith("conservative-remediation");
  });
});

describe("SkillDetailPanel exposes the per-version rationale and gates the write CRUD on a session", () => {
  const noop = () => {};

  it("invites a selection when no skill is chosen (read-only guarded state)", () => {
    render(<SkillDetailPanel state={{ status: "restricted", reason: "read-only" }} canWrite={false} onDraft={noop} onTransition={noop} pending={false} error={null} />);
    expect(screen.getByTestId("detail-none")).toBeInTheDocument();
  });

  it("shows every version's rationale (self-improvement is inspectable)", () => {
    render(<SkillDetailPanel state={guardedReady(detail, false, false)} canWrite={false} onDraft={noop} onTransition={noop} pending={false} error={null} />);
    const rationales = screen.getAllByTestId("version-rationale");
    expect(rationales).toHaveLength(2);
    expect(rationales[0]).toHaveTextContent(/flywheel candidate: lead with the single most-likely hypothesis/);
    expect(rationales[1]).toHaveTextContent(/promoted after the disk-fill trial win/);
  });

  it("a read-only session sees the data but NO write controls", () => {
    render(<SkillDetailPanel state={guardedReady(detail, false, false)} canWrite={false} onDraft={noop} onTransition={noop} pending={false} error={null} />);
    expect(screen.getByTestId("detail-readonly")).toBeInTheDocument();
    expect(screen.queryByTestId("open-draft")).toBeNull();
    expect(screen.queryByTestId("transition-promote")).toBeNull();
  });

  it("an operator session can draft a version — the payload carries the typed predicate + rationale", () => {
    const onDraft = vi.fn();
    render(<SkillDetailPanel state={guardedReady(detail, false, false)} canWrite onDraft={onDraft} onTransition={noop} pending={false} error={null} />);
    fireEvent.click(screen.getByTestId("open-draft"));
    fireEvent.change(screen.getByTestId("draft-version"), { target: { value: "4" } });
    fireEvent.change(screen.getByTestId("draft-body"), { target: { value: "new body" } });
    fireEvent.change(screen.getByTestId("draft-phases"), { target: { value: "1, 2" } });
    fireEvent.change(screen.getByTestId("draft-classes"), { target: { value: "network-control" } });
    fireEvent.change(screen.getByTestId("draft-rationale"), { target: { value: "operator draft: tighten the applies-when." } });
    fireEvent.click(screen.getByRole("button", { name: /create draft version/i }));
    expect(onDraft).toHaveBeenCalledWith({
      version: "4",
      body: "new body",
      rationale: "operator draft: tighten the applies-when.",
      applies_when: { phases: ["1", "2"], exec_classes: ["network-control"] },
    });
  });

  it("an operator session transitions a version behind a mandatory-rationale gate", () => {
    const onTransition = vi.fn();
    render(<SkillDetailPanel state={guardedReady(detail, false, false)} canWrite onDraft={noop} onTransition={onTransition} pending={false} error={null} />);
    // the v3 version is in 'trial' → the only exposed verb is promote
    fireEvent.click(screen.getByTestId("transition-promote"));
    // confirm is blocked until a rationale is entered
    const confirm = screen.getByRole("button", { name: /confirm promote/i });
    expect(confirm).toBeDisabled();
    fireEvent.change(screen.getByTestId("transition-rationale"), { target: { value: "trial won — promote." } });
    fireEvent.click(screen.getByRole("button", { name: /confirm promote/i }));
    expect(onTransition).toHaveBeenCalledWith(1002, "promote", "trial won — promote.");
  });
});

describe("the skill reads fail closed on the wire (guarded-state maps the error)", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("fetchSkills throws an ApiError with the HTTP status on a non-2xx (401 → auth)", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response("nope", { status: 401 })));
    await expect(fetchSkills()).rejects.toMatchObject({ status: 401 });
  });

  it("fetchTrials throws a status-0 ApiError on a network failure (transport)", async () => {
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new Error("boom")));
    await expect(fetchTrials()).rejects.toMatchObject({ status: 0 });
  });

  it("fetchCanWrite is true ONLY for an operator session, false for a machine principal", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(JSON.stringify({ source: "operator:kp" }), { status: 200 })));
    await expect(fetchCanWrite()).resolves.toBe(true);
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(JSON.stringify({ source: "hmac:librenms" }), { status: 200 })));
    await expect(fetchCanWrite()).resolves.toBe(false);
  });
});
