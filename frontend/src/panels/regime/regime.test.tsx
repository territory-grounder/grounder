import { describe, it, expect, vi, afterEach } from "vitest";
import { render, screen } from "@testing-library/react";
import { guardedFromError, guardedReady, type GuardedState } from "../../lib/guarded-state";
import type { RegimePage } from "../../api/generated-client";
import { RegimeView } from "./regime-panel";
import {
  CANONICAL_REGIMES,
  deriveLaneCoverage,
  deriveRegimeMap,
  derivePendingVerifications,
  deriveTemplateAllowlist,
  fetchRegimePage,
} from "./regime-client";

// A populated real-shape page: two resolved targets + one refusal, one launched-and-verified job, one
// launched-but-unverified job, and a coverage roll-up. The token appears ONLY as a SecretRef reference.
const populated: RegimePage = {
  resolutions: [
    { target: "librespeed01", regime: "native-ssh", lane: "native-ssh", outcome: "resolved", created_at: "2026-07-20T10:10:00Z" },
    { target: "awx-edge", regime: "awx-job", lane: "awx-job", rule_id: "awx-edge", outcome: "resolved", created_at: "2026-07-20T10:09:00Z" },
    // an older duplicate resolution for the same target — the map keeps the most recent (first-seen) row
    { target: "awx-edge", regime: "native-ssh", lane: "native-ssh", outcome: "resolved", created_at: "2026-07-20T09:00:00Z" },
    { target: "mystery01", outcome: "refused", created_at: "2026-07-20T10:08:00Z" },
  ],
  actuations: [
    { action_id: "act-1", lane: "awx-job", job_template_id: "42", op_class: "restart-service", job_id: "9001", token_ref: "store:awx.launch", created_at: "2026-07-20T10:07:00Z" },
    { action_id: "act-2", lane: "awx-job", job_template_id: "43", op_class: "gather-facts", job_id: "9002", token_ref: "store:awx.sensor", created_at: "2026-07-20T10:05:00Z" },
  ],
  deferred_verdicts: [
    { action_id: "act-1", job_id: "9001", status: "successful", verdict: "match", graduation: "verified_clean", created_at: "2026-07-20T10:06:00Z" },
  ],
  lane_coverage: [
    { lane: "awx-job", resolutions: 1, actuations: 2 },
    { lane: "native-ssh", resolutions: 1, actuations: 0 },
  ],
};

const empty: RegimePage = { resolutions: [], actuations: [], deferred_verdicts: [], lane_coverage: [] };

function renderView(state: GuardedState<RegimePage>) {
  return render(<RegimeView state={state} />);
}

describe("regime derivations (pure projections of the real /v1/regime tail)", () => {
  it("deriveRegimeMap keeps one row per target (most recent wins) and preserves refusals", () => {
    const map = deriveRegimeMap(populated.resolutions);
    expect(map).toHaveLength(3); // librespeed01, awx-edge (deduped), mystery01
    const awx = map.find((r) => r.target === "awx-edge");
    expect(awx?.lane).toBe("awx-job"); // the newer row, not the older native-ssh duplicate
    const refused = map.find((r) => r.target === "mystery01");
    expect(refused?.outcome).toBe("refused");
    expect(refused?.lane ?? "").toBe(""); // a refusal carries no lane (fail closed)
  });

  it("derivePendingVerifications is exactly the launches without a terminal verdict", () => {
    const pending = derivePendingVerifications(populated.actuations, populated.deferred_verdicts);
    expect(pending.map((a) => a.action_id)).toEqual(["act-2"]); // act-1 is verified, act-2 is not
  });

  it("deriveTemplateAllowlist is the distinct (template -> op-class) pairs observed on the actuation tail", () => {
    const al = deriveTemplateAllowlist([...populated.actuations, populated.actuations[0]]);
    expect(al).toEqual([
      { job_template_id: "42", op_class: "restart-service" },
      { job_template_id: "43", op_class: "gather-facts" },
    ]);
  });

  it("deriveLaneCoverage lists every canonical regime with real counts and a wired/inert flag", () => {
    const cov = deriveLaneCoverage(populated.lane_coverage);
    // all five canonical regimes are present even when the server reported only two
    for (const lane of CANONICAL_REGIMES) {
      expect(cov.find((c) => c.lane === lane)).toBeTruthy();
    }
    expect(cov.find((c) => c.lane === "awx-job")?.wired).toBe(true);
    expect(cov.find((c) => c.lane === "gitops-mr")?.wired).toBe(false); // inert / fail-closed, count 0
    expect(cov.find((c) => c.lane === "gitops-mr")?.actuations).toBe(0);
  });
});

describe("RegimeView (T-017-7, REQ-1716) fails closed and renders only real engine state", () => {
  it("shows a loading state", () => {
    renderView({ status: "loading" });
    expect(screen.getByRole("status")).toHaveTextContent(/loading regime engine/i);
  });

  it("an auth error fails closed to 'not authorized' (no data fabricated)", () => {
    renderView(guardedFromError<RegimePage>({ status: 403 }));
    expect(screen.getByRole("status")).toHaveTextContent(/not authorized/i);
    expect(screen.queryByTestId("regime-map-row")).toBeNull();
  });

  it("a transport error fails closed to 'unavailable'", () => {
    renderView(guardedFromError<RegimePage>({ status: 0 }));
    expect(screen.getByRole("status")).toHaveTextContent(/unavailable/i);
  });

  it("an empty spine renders an honest empty state, not a fabricated row", () => {
    renderView(guardedReady(empty, false, false));
    expect(screen.getByTestId("regime-map-empty")).toBeInTheDocument();
    expect(screen.getByTestId("allowlist-empty")).toBeInTheDocument();
    expect(screen.getByTestId("pending-empty")).toBeInTheDocument();
    expect(screen.getByTestId("actuations-empty")).toBeInTheDocument();
    // lane coverage still lists the five canonical lanes, all inert
    expect(screen.getAllByTestId("lane-coverage-row")).toHaveLength(CANONICAL_REGIMES.length);
    for (const row of screen.getAllByTestId("lane-coverage-row")) {
      expect(row).toHaveAttribute("data-wired", "false");
    }
  });

  it("renders the regime map, coverage, allowlist, pending queue, and actuation tail from real state", () => {
    renderView(guardedReady(populated, false, false));

    // regime -> lane map: deduped targets, with the refusal marked fail-closed
    const mapRows = screen.getAllByTestId("regime-map-row");
    expect(mapRows).toHaveLength(3);
    const refused = mapRows.find((r) => r.getAttribute("data-outcome") === "refused");
    expect(refused).toBeTruthy();
    expect(refused!).toHaveTextContent(/refused, fail-closed/i);

    // allowlist: the two observed templates with their op-classes
    const allowRows = screen.getAllByTestId("allowlist-row");
    expect(allowRows).toHaveLength(2);
    expect(allowRows[0]).toHaveTextContent("restart-service");

    // pending queue: only the unverified launch (act-2), not the verified act-1
    const pendingRows = screen.getAllByTestId("pending-row");
    expect(pendingRows).toHaveLength(1);
    expect(pendingRows[0]).toHaveTextContent("act-2");

    // actuation tail: both launches, the verified one carries its terminal verdict
    expect(screen.getAllByTestId("actuation-row")).toHaveLength(2);
    expect(screen.getByText(/successful · verified_clean/)).toBeInTheDocument();

    // wired lane is flagged wired
    const awx = screen.getAllByTestId("lane-coverage-row").find((r) => r.textContent?.includes("awx-job"));
    expect(awx).toHaveAttribute("data-wired", "true");
  });

  it("shows the AWX token only as a SecretRef reference — never a plaintext value (INV-13)", () => {
    const { container } = renderView(guardedReady(populated, false, false));
    expect(screen.getByText("store:awx.launch")).toBeInTheDocument();
    // no plaintext token value ever appears in the rendered surface
    expect(container.textContent).not.toMatch(/s3cr3t|bearer\s|oauth2-token-value/i);
  });

  it("exposes no mutating control over the read-only regime surface, in any mode", () => {
    renderView(guardedReady(populated, true, true));
    expect(screen.queryByRole("button")).toBeNull();
  });
});

describe("fetchRegimePage fails closed on the wire (guarded-state maps the error)", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("throws an ApiError with the HTTP status on a non-2xx (401 -> auth, restrictive)", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response("nope", { status: 401 })));
    await expect(fetchRegimePage()).rejects.toMatchObject({ status: 401 });
  });

  it("throws a status-0 ApiError on a network failure (transport, restrictive)", async () => {
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new Error("boom")));
    await expect(fetchRegimePage()).rejects.toMatchObject({ status: 0 });
  });

  it("returns the parsed page on 200", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(JSON.stringify(empty), { status: 200 })));
    await expect(fetchRegimePage()).resolves.toEqual(empty);
  });
});
