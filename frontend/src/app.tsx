import { useEffect, useState, type ReactNode } from "react";
import { QueryClientProvider } from "@tanstack/react-query";
import { makeQueryClient } from "./api/query-client";
import { MutationModeProvider, useMutationMode } from "./mode/mode-provider";
import { guardedReady } from "./lib/guarded-state";
import { isReadOnly } from "./mode/mutation-mode";

import { ApprovalConsole } from "./panels/approval/approval-console";
import { ManifestTimeline } from "./panels/manifest/manifest-timeline";
import { LedgerView } from "./panels/ledger/ledger-view";
import { ExplainabilityPanel } from "./panels/explain/explainability-panel";
import { AutonomyControls } from "./panels/controls/autonomy-controls";
import { AdminPanel } from "./panels/admin/admin-panel";
import { RegimePanel } from "./panels/regime/regime-panel";
import { SkillsPanel } from "./panels/skills/skills-panel";
import * as sample from "./demo/sample-data";

const queryClient = makeQueryClient();

// Card frames a panel as one instrument on the deck: an eyebrow label + a spec reference + the panel body.
function Card({ label, req, span, children }: { label: string; req: string; span: string; children: ReactNode }) {
  return (
    <section className={`panel ${span}`}>
      <header>
        <div className="h">
          <span className="eyebrow">{label}</span>
        </div>
        <span className="eyebrow" aria-hidden>{req}</span>
      </header>
      <div className="body">{children}</div>
    </section>
  );
}

// The mutation breaker — the signature. It reflects the server-authoritative mutation mode as a mechanical
// switch locked in its state. Read-only is the safe default; operational "energizes" it.
function Breaker() {
  const mode = useMutationMode();
  const ro = isReadOnly(mode);
  return (
    <div className="breaker" data-mode={mode} role="status" aria-label={`Mutation ${ro ? "off, read-only" : "on, operational"}`}>
      <span className="k">Mutation</span>
      <span className="sw">
        <span className="nub" aria-hidden />
        {ro ? "Off · Read-only" : "On · Operational"}
      </span>
    </div>
  );
}

function Clock() {
  const [now, setNow] = useState("--:--:--");
  useEffect(() => {
    const tick = () => setNow(new Date().toISOString().slice(11, 19) + "Z");
    tick();
    const id = setInterval(tick, 1000);
    return () => clearInterval(id);
  }, []);
  return <span className="clock" aria-label="UTC clock">{now}</span>;
}

// The app shell composes every panel inside the MutationModeProvider, seeded from the server's
// mutation_enabled flag (default false ⇒ read-only). No panel computes a safety decision; each reflects
// server-provided state through the guarded-state gate. This preview renders representative data.
export function App({ mutationEnabled = false }: { mutationEnabled?: boolean }) {
  return (
    <QueryClientProvider client={queryClient}>
      <MutationModeProvider mutationEnabled={mutationEnabled}>
        <header className="bar">
          <div className="wordmark">
            <span className="tg">TG</span>
            <h1>Territory&nbsp;Grounder</h1>
            <span className="eyebrow" style={{ marginLeft: 4 }}>Operator console</span>
          </div>
          <div className="spacer" />
          <div className="readout">
            <span aria-label="Preview — not connected to a live stream"><span className="dot" />Preview data</span>
            <Clock />
          </div>
          <Breaker />
        </header>

        <main className="deck" aria-label="Territory Grounder operator console">
          <div className="spectrum" aria-label="Autonomy bands">
            <div className="seg auto"><b>AUTO</b><span className="n">act · silent</span></div>
            <div className="seg notice"><b>AUTO_NOTICE</b><span className="n">act · notify</span></div>
            <div className="seg pause"><b>POLL_PAUSE</b><span className="n">pause · human</span></div>
          </div>

          <div className="grid">
            <Card label="Approval feed" req="REQ-604" span="col-7">
              <ApprovalConsole state={guardedReady(sample.pendingDecisions, false, false)} />
            </Card>
            <Card label="Explainability" req="REQ-609" span="col-5">
              <ExplainabilityPanel state={guardedReady(sample.session, false, false)} />
            </Card>
            <Card label="Action manifest" req="REQ-606" span="col-7">
              <ManifestTimeline state={guardedReady(sample.manifest, false, false)} />
            </Card>
            <Card label="Governance ledger" req="REQ-608" span="col-5">
              <LedgerView state={guardedReady(sample.ledger, false, false)} />
            </Card>
            <Card label="Autonomy controls" req="REQ-610" span="col-6">
              <AutonomyControls layers={sample.autonomyLayers} state={guardedReady(sample.autonomyLayers, false, false)} />
            </Card>
            <Card label="Administration" req="REQ-612" span="col-6">
              <AdminPanel isAdministrator state={guardedReady(sample.admin, false, false)} />
            </Card>
            {/* The Actuation Regime Engine surface renders REAL engine state from GET /v1/regime (REQ-1716,
                "real engine state only") — it self-fetches through the shared query client rather than the
                preview sample data. With no backend/session it fails closed to an honest "unavailable". */}
            <Card label="Actuation regimes" req="REQ-1716" span="col-12">
              <RegimePanel />
            </Card>
            {/* The Skills / Flywheel surface renders REAL store state from GET /v1/skills, /v1/skills/trials,
                and /v1/skills/{name} (spec/014 REQ-1311/1313) — it self-fetches through the shared query
                client. The admin CRUD (draft/transition) sits behind the session-only write lane: controls
                render only for an operator session, the server re-enforces authority regardless. */}
            <Card label="Skills & flywheel" req="REQ-1311" span="col-12">
              <SkillsPanel />
            </Card>
          </div>

          <p className="eyebrow" style={{ marginTop: 28, textAlign: "center" }}>
            Preview · representative data · mutation OFF (read-only foundation)
          </p>
        </main>
      </MutationModeProvider>
    </QueryClientProvider>
  );
}
