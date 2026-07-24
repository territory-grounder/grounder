// Autonomy controls panel (spec/010, T-010-7).
//
// Renders one row per autonomy layer: the server-computed band (read-only display) plus MUTATING controls
// — band selectors and a kill-switch. Every mutating control renders ONLY when canShowMutatingControl(state)
// is true: the panel is ready AND the server reports mutation operational AND this caller is authorized. In
// read-only mode, or for a non-authorized caller, NO mutating control is rendered — the operator still sees
// the current band, but cannot act. Each autonomy layer is INDEPENDENTLY disableable (its own kill-switch).
//
// The console NEVER computes a band/verdict/floor/grant and NEVER enforces a safety decision client-side —
// it displays the server value and routes every change through the policy API endpoint from the generated
// client (never a host-local file). Mutations are delegated to the onDisableLayer / onSetBand props so the
// data flow stays testable and the parent owns the actual API call.

import type { PendingDecision } from "../../api/generated-client";
import { endpoints } from "../../api/generated-client";
import type { GuardedState } from "../../lib/guarded-state";
import { canShowMutatingControl } from "../../lib/guarded-state";
import { a11yButton, a11yLabel } from "../../a11y/baseline";
import { useMutationMode } from "../../mode/mode-provider";
import { isReadOnly } from "../../mode/mutation-mode";
import { killSwitchDisplayState } from "./kill-switch";

// Band is the server-authored autonomy band; the console only ever DISPLAYS the active one.
export type Band = PendingDecision["band"];

const BANDS: readonly Band[] = ["AUTO", "AUTO_NOTICE", "POLL_PAUSE"];

// Server-provided view of one autonomy layer. `band` and `enabled` are authoritative server values; the
// console reflects them and never recomputes them. `pending_write`/`disable_confirmed` drive the kill-switch
// display so that "disabled" is shown ONLY once the API confirms.
export interface AutonomyLayer {
  layer_id: string;
  name: string;
  band: Band;
  enabled: boolean;
  pending_write: boolean;
  disable_confirmed: boolean;
}

export interface AutonomyControlsProps {
  // Already-fetched server data (the layers to display).
  layers: AutonomyLayer[];
  // Guard for whether MUTATING controls may render at all (ready + operational + authorized).
  state: GuardedState<AutonomyLayer[]>;
  // Change handlers — the parent wires these to the policy API endpoint (generated client).
  onDisableLayer?: (layerId: string) => void;
  onSetBand?: (layerId: string, band: Band) => void;
}

export function AutonomyControls({
  layers,
  state,
  onDisableLayer,
  onSetBand,
}: AutonomyControlsProps): JSX.Element {
  const mode = useMutationMode();
  // Fail closed: a mutating control renders ONLY when the guard says so AND the mode is operational. Either
  // signal being restrictive removes every mutating control (read-only foundation, INV-09).
  const showMutating = canShowMutatingControl(state) && !isReadOnly(mode);

  return (
    <section data-testid="autonomy-controls">
      {layers.map((layer) => {
        const target = endpoints.vote; // served POLL_PAUSE release (POST /v1/vote) — repointed off the 404 /v1/decisions/*
        const ks = killSwitchDisplayState(layer.pending_write, layer.disable_confirmed);
        const killDisabled = ks === "disabled";
        const killLabel =
          ks === "disabled"
            ? `${layer.name} disabled`
            : ks === "pending"
              ? `Disabling ${layer.name}`
              : `Disable ${layer.name}`;

        return (
          <div key={layer.layer_id} data-testid={`layer-${layer.layer_id}`}>
            <span data-testid={`layer-name-${layer.layer_id}`}>{layer.name}</span>

            {/* Server-computed band — always shown, read-only. The console never computes this. */}
            <span
              data-testid={`band-current-${layer.layer_id}`}
              {...a11yLabel(`${layer.name} band ${layer.band}`)}
            >
              {layer.band}
            </span>

            {/* MUTATING controls: rendered ONLY when operational AND authorized. */}
            {showMutating && (
              <>
                {BANDS.map((b) => (
                  <button
                    key={b}
                    type="button"
                    data-testid={`set-band-${layer.layer_id}-${b}`}
                    data-endpoint={target}
                    aria-pressed={layer.band === b}
                    onClick={() => onSetBand?.(layer.layer_id, b)}
                    {...a11yButton(`Set ${layer.name} band to ${b}`, false)}
                  >
                    {b}
                  </button>
                ))}

                <button
                  type="button"
                  data-testid={`kill-${layer.layer_id}`}
                  data-endpoint={target}
                  data-state={ks}
                  onClick={() => {
                    if (!killDisabled) onDisableLayer?.(layer.layer_id);
                  }}
                  {...a11yButton(killLabel, killDisabled)}
                >
                  {killLabel}
                </button>
              </>
            )}
          </div>
        );
      })}
    </section>
  );
}
