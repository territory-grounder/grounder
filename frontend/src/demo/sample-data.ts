// Representative governed data for the served preview. It mirrors the estate the platform runs against
// (LibreNMS/Prometheus alerts, the NL/GR sites) so the console renders a populated, believable operations
// deck. The panels still enforce their real guarded-state + read-only logic over this data.

import type { PendingDecision } from "../api/generated-client";
import type { ManifestData } from "../panels/manifest/replay";
import type { LedgerData } from "../panels/ledger/ledger-view";
import type { ExplainabilitySession } from "../panels/explain/explainability-panel";
import type { AutonomyLayer } from "../panels/controls/autonomy-controls";
import type { AdminData } from "../panels/admin/admin-panel";

export const pendingDecisions: PendingDecision[] = [
  {
    decision_id: "TG-2041#restart",
    action_id: "9f2c14ae7b0d",
    band: "POLL_PAUSE",
    plan: {
      approaches: [
        "Restart the frr service on nl-frr01 to re-establish the BFD session (least blast radius).",
        "Clear the stale neighbor and let the adjacency re-form on the next hello interval.",
      ],
    },
    prediction: "BFD session nl-frr01↔gr-frr01 returns to Up; no dependent route withdrawal expected.",
    reversible: true,
    caller_can_act: false,
  },
  {
    decision_id: "TG-2039#reload",
    action_id: "c73b0aa19e41",
    band: "AUTO_NOTICE",
    plan: {
      approaches: [
        "Reload nginx on sw-core-01 to pick up the renewed certificate.",
        "Graceful worker cycle so in-flight requests drain first.",
      ],
    },
    prediction: "TLS handshake errors clear; request success rate returns to baseline within one scrape.",
    reversible: true,
    caller_can_act: true,
  },
];

const ACTION = "9f2c14ae7b0d3e58";
export const manifest: ManifestData = {
  actionId: ACTION,
  stages: [
    { stage: "predicted", at: "2026-07-16T00:41:02Z", action_id: ACTION },
    { stage: "approved", at: "2026-07-16T00:41:37Z", action_id: ACTION },
    { stage: "executed", at: "2026-07-16T00:41:41Z", action_id: ACTION },
    { stage: "verified", at: "2026-07-16T00:42:03Z", action_id: ACTION },
  ],
};

export const ledger: LedgerData = {
  entries: [
    { seq: 1, decision: "classify:POLL_PAUSE", action_id: "9f2c14ae7b0d", hash: "3a1f…c92e", prev_hash: "" },
    { seq: 2, decision: "gate:deny", action_id: "9f2c14ae7b0d", hash: "b74d…10ac", prev_hash: "3a1f…c92e" },
    { seq: 3, decision: "classify:AUTO_NOTICE", action_id: "c73b0aa19e41", hash: "e058…4471", prev_hash: "b74d…10ac" },
    { seq: 4, decision: "verdict:match", action_id: "c73b0aa19e41", hash: "1c83…f8e1", prev_hash: "e058…4471" },
  ],
  verification: { verified: true },
};

export const session: ExplainabilitySession = {
  retrieval: [
    { chunk_id: "rag-8821", source: "runbook/bfd-recovery.md", snippet: "A flapping BFD session is most often a stale neighbor; a targeted frr restart re-establishes the adjacency without a full reconverge." },
    { chunk_id: "rag-8830", source: "netbox/device/nl-frr01", snippet: "Role: border-router · Site: dc1 · Redundant peer: gr-frr01 (anycast)." },
  ],
  band: "POLL_PAUSE",
  signals: ["criticality_tier:host", "blast_radius:wide", "reversible:mixed"],
  execution_class: "network-control",
  confidence_trajectory: [
    { at: "00:41:02", confidence: 0.42 },
    { at: "00:41:12", confidence: 0.67 },
    { at: "00:41:24", confidence: 0.81 },
  ],
  evidence_ids: ["tr-librenms-40817", "tr-prom-am-2041", "tr-netbox-nl-frr01"],
};

export const autonomyLayers: AutonomyLayer[] = [
  { layer_id: "ingest", name: "Ingest & admission", band: "AUTO", enabled: true, pending_write: false, disable_confirmed: false },
  { layer_id: "triage", name: "Triage & prediction", band: "AUTO_NOTICE", enabled: true, pending_write: false, disable_confirmed: false },
  { layer_id: "actuation", name: "Actuation (effect channel)", band: "POLL_PAUSE", enabled: false, pending_write: false, disable_confirmed: true },
];

export const admin: AdminData = {
  users: [
    { user_id: "u-oncall", display_name: "On-call (NL)", roles: ["approver", "operator"] },
    { user_id: "u-lead", display_name: "SRE lead", roles: ["administrator", "approver"] },
  ],
  assignable_roles: ["approver", "operator", "administrator", "read-only"],
  on_call: { current_on_call: "On-call (NL)", escalation_chain: ["SRE lead", "Duty manager"] },
  modules: [
    { module_id: "ingest/librenms", enabled: true },
    { module_id: "actuation/ssh", enabled: false },
    { module_id: "notifier/matrix", enabled: true },
  ],
};
