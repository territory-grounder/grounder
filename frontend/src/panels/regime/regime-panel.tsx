import type { RegimePage } from "../../api/generated-client";
import type { GuardedState } from "../../lib/guarded-state";
import { a11yStatus } from "../../a11y/baseline";
import {
  deriveLaneCoverage,
  deriveRegimeMap,
  derivePendingVerifications,
  deriveTemplateAllowlist,
  useRegimePage,
} from "./regime-client";

// The Actuation Regime Engine console surface (spec/017 T-017-7, REQ-1716). It renders REAL engine state from
// ONE authenticated read of GET /v1/regime — the per-target regime → lane map, the lane-coverage view (which
// lanes carry traffic vs which are inert / fail-closed), the AWX template-allowlist (each template's
// authorized op-class), the pending-verification queue of launched-but-unverified jobs, and the recent
// actuation tail. It computes no safety decision and exposes NO mutating control in ANY mode: the surface is
// read-only, and the ledger-audited allowlist EDITOR is a separate write follow-on. An auth or transport
// error fails closed to "restricted"; an empty spine (the expected state at mode Shadow, before the flip)
// renders an honest empty state, never a fabricated row.

// A per-verdict join so the actuation tail can show whether a launch has been deferred-verified yet.
function verdictFor(page: RegimePage, actionID: string): string {
  const v = page.deferred_verdicts.find((d) => d.action_id === actionID);
  return v ? `${v.status} · ${v.graduation}` : "pending-verification";
}

export function RegimeView({ state }: { state: GuardedState<RegimePage> }) {
  if (state.status === "loading") {
    return (
      <section aria-label="Actuation regimes">
        <div {...a11yStatus("Loading regime engine state")}>Loading regime engine state…</div>
      </section>
    );
  }

  if (state.status === "restricted") {
    const reason =
      state.reason === "auth"
        ? "not authorized to view the regime engine"
        : "regime engine currently unavailable";
    return (
      <section aria-label="Actuation regimes">
        <div {...a11yStatus(`Regime engine restricted: ${reason}`)}>Regime engine unavailable — {reason}.</div>
      </section>
    );
  }

  const page = state.data;
  const regimeMap = deriveRegimeMap(page.resolutions);
  const coverage = deriveLaneCoverage(page.lane_coverage);
  const allowlist = deriveTemplateAllowlist(page.actuations);
  const pending = derivePendingVerifications(page.actuations, page.deferred_verdicts);

  return (
    <section aria-label="Actuation regimes">
      {/* ---- per-target regime → lane map (from the resolution tail; a refusal is fail-closed) ---- */}
      <section aria-label="Regime to lane map">
        <h3>Regime → lane map</h3>
        {regimeMap.length === 0 ? (
          <p {...a11yStatus("No regime resolutions recorded")} data-testid="regime-map-empty">
            No regime resolutions recorded yet — the engine has routed no target (expected at mode Shadow).
          </p>
        ) : (
          <table>
            <caption>Per-target regime and selected effect lane</caption>
            <thead>
              <tr>
                <th scope="col">Target</th>
                <th scope="col">Regime</th>
                <th scope="col">Lane</th>
                <th scope="col">Rule</th>
                <th scope="col">Outcome</th>
              </tr>
            </thead>
            <tbody>
              {regimeMap.map((r) => (
                <tr key={r.target} data-testid="regime-map-row" data-outcome={r.outcome}>
                  <td>
                    <code>{r.target}</code>
                  </td>
                  <td>{r.regime || "—"}</td>
                  <td>{r.lane || "— (refused, fail-closed)"}</td>
                  <td>{r.rule_id ? <code>{r.rule_id}</code> : "default"}</td>
                  <td>{r.outcome}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </section>

      {/* ---- lane coverage: which lanes carry traffic (wired) vs inert / fail-closed ---- */}
      <section aria-label="Lane coverage">
        <h3>Lane coverage</h3>
        <table>
          <caption>Which effect lane reaches a target — wired lanes carry traffic; inert lanes carry none</caption>
          <thead>
            <tr>
              <th scope="col">Lane</th>
              <th scope="col">Resolutions</th>
              <th scope="col">Actuations</th>
              <th scope="col">Status</th>
            </tr>
          </thead>
          <tbody>
            {coverage.map((c) => (
              <tr key={c.lane} data-testid="lane-coverage-row" data-wired={String(c.wired)}>
                <td>
                  <code>{c.lane}</code>
                </td>
                <td>{c.resolutions}</td>
                <td>{c.actuations}</td>
                <td>{c.wired ? "wired" : "inert / fail-closed"}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </section>

      {/* ---- AWX template-allowlist (observed): each launched template with its authorized op-class ---- */}
      <section aria-label="AWX template allowlist">
        <h3>AWX template allowlist</h3>
        {allowlist.length === 0 ? (
          <p {...a11yStatus("No AWX templates launched")} data-testid="allowlist-empty">
            No AWX job templates launched yet — none observed on the actuation tail (expected at mode Shadow).
          </p>
        ) : (
          <table>
            <caption>AWX job templates the engine routed through, each with its authorized op-class</caption>
            <thead>
              <tr>
                <th scope="col">Job template</th>
                <th scope="col">Authorized op-class</th>
              </tr>
            </thead>
            <tbody>
              {allowlist.map((a) => (
                <tr key={`${a.job_template_id} ${a.op_class}`} data-testid="allowlist-row">
                  <td>
                    <code>{a.job_template_id}</code>
                  </td>
                  <td>{a.op_class || "—"}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
        <p className="eyebrow">
          Observed from the actuation tail. The full configured allowlist and its ledger-audited editor are a
          separate write follow-on (read-only here).
        </p>
      </section>

      {/* ---- pending-verification queue: launched-but-unverified async jobs (no clean run yet) ---- */}
      <section aria-label="Pending verification queue">
        <h3>Pending verification</h3>
        {pending.length === 0 ? (
          <p {...a11yStatus("No jobs pending verification")} data-testid="pending-empty">
            No jobs pending verification — every launched job has reached a terminal deferred verdict.
          </p>
        ) : (
          <table>
            <caption>Launched jobs awaiting a terminal deferred verdict — each counts as no clean run yet</caption>
            <thead>
              <tr>
                <th scope="col">Action</th>
                <th scope="col">Lane</th>
                <th scope="col">Job template</th>
                <th scope="col">Op-class</th>
                <th scope="col">Job</th>
              </tr>
            </thead>
            <tbody>
              {pending.map((a) => (
                <tr key={a.action_id} data-testid="pending-row">
                  <td>
                    <code>{a.action_id}</code>
                  </td>
                  <td>{a.lane}</td>
                  <td>{a.job_template_id ? <code>{a.job_template_id}</code> : "—"}</td>
                  <td>{a.op_class || "—"}</td>
                  <td>{a.job_id ? <code>{a.job_id}</code> : "—"}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </section>

      {/* ---- recent actuation tail: the launch audit, token shown as a REFERENCE (never a value) ---- */}
      <section aria-label="Recent actuations">
        <h3>Recent actuations</h3>
        {page.actuations.length === 0 ? (
          <p {...a11yStatus("No actuations recorded")} data-testid="actuations-empty">
            No job launches recorded yet (expected at mode Shadow — mutating lanes stay off until the flip).
          </p>
        ) : (
          <table>
            <caption>Recent AWX-job launches — token shown as a SecretRef reference, never a value</caption>
            <thead>
              <tr>
                <th scope="col">Action</th>
                <th scope="col">Lane</th>
                <th scope="col">Job template</th>
                <th scope="col">Op-class</th>
                <th scope="col">Job</th>
                <th scope="col">Token ref</th>
                <th scope="col">Verify</th>
              </tr>
            </thead>
            <tbody>
              {page.actuations.map((a) => (
                <tr key={a.action_id} data-testid="actuation-row">
                  <td>
                    <code>{a.action_id}</code>
                  </td>
                  <td>{a.lane}</td>
                  <td>{a.job_template_id ? <code>{a.job_template_id}</code> : "—"}</td>
                  <td>{a.op_class || "—"}</td>
                  <td>{a.job_id ? <code>{a.job_id}</code> : "—"}</td>
                  <td>{a.token_ref ? <code>{a.token_ref}</code> : "—"}</td>
                  <td>{verdictFor(page, a.action_id)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </section>
    </section>
  );
}

// RegimePanel is the live container: it performs the single authenticated read via useRegimePage and hands
// the resulting GuardedState to the presentational RegimeView. The panel is read-only in every mode.
export function RegimePanel() {
  return <RegimeView state={useRegimePage()} />;
}
