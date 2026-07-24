import { render, screen, fireEvent } from "@testing-library/react";
import { describe, it, expect, vi } from "vitest";
import { MutationModeProvider } from "../../mode/mode-provider";
import { guardedReady, type GuardedState } from "../../lib/guarded-state";
import { AutonomyControls, type AutonomyLayer } from "./autonomy-controls";
import { killSwitchDisplayState } from "./kill-switch";

function layer(over: Partial<AutonomyLayer> = {}): AutonomyLayer {
  return {
    layer_id: "poll",
    name: "Poll loop",
    band: "POLL_PAUSE",
    enabled: true,
    pending_write: false,
    disable_confirmed: false,
    ...over,
  };
}

function ready(layers: AutonomyLayer[], operational: boolean, authorized: boolean): GuardedState<AutonomyLayer[]> {
  return guardedReady(layers, operational, authorized);
}

describe("autonomy-controls panel (T-010-7)", () => {
  it("exposes NO mutating control in read-only mode", () => {
    const layers = [layer()];
    render(
      <MutationModeProvider mutationEnabled={false}>
        <AutonomyControls layers={layers} state={ready(layers, false, true)} />
      </MutationModeProvider>,
    );
    // The read-only view still shows the server band, but there is not a single mutating control.
    expect(screen.getByTestId("band-current-poll")).toHaveTextContent("POLL_PAUSE");
    expect(screen.queryByRole("button")).toBeNull();
    expect(screen.queryByTestId("kill-poll")).toBeNull();
  });

  it("does not render a mutating control for a non-authorized caller even when operational", () => {
    const layers = [layer()];
    render(
      <MutationModeProvider mutationEnabled={true}>
        <AutonomyControls layers={layers} state={ready(layers, true, false)} />
      </MutationModeProvider>,
    );
    expect(screen.queryByRole("button")).toBeNull();
  });

  it("renders an enabled control when mutation is operational AND authorized", () => {
    const layers = [layer()];
    render(
      <MutationModeProvider mutationEnabled={true}>
        <AutonomyControls layers={layers} state={ready(layers, true, true)} />
      </MutationModeProvider>,
    );
    const kill = screen.getByTestId("kill-poll");
    expect(kill).toBeInTheDocument();
    expect(kill).toHaveAttribute("aria-disabled", "false");
    expect(kill).toHaveAttribute("tabindex", "0"); // keyboard-operable
    // The change is routed through the policy API endpoint (generated client), never a host-local file.
    expect(kill).toHaveAttribute("data-endpoint", "/api/v1/vote");
  });

  it("shows the kill-switch DISABLED state only after the API confirms (pure logic)", () => {
    expect(killSwitchDisplayState(false, false)).toBe("enabled");
    expect(killSwitchDisplayState(true, false)).toBe("pending"); // pending write => NOT disabled
    expect(killSwitchDisplayState(true, true)).toBe("disabled"); // confirmed => disabled
    expect(killSwitchDisplayState(false, true)).toBe("disabled");
  });

  it("does not show the kill-switch as disabled while a write is pending, only once confirmed", () => {
    const pending = [layer({ pending_write: true, disable_confirmed: false })];
    const { rerender } = render(
      <MutationModeProvider mutationEnabled={true}>
        <AutonomyControls layers={pending} state={ready(pending, true, true)} />
      </MutationModeProvider>,
    );
    // Pending write: reflected as "pending", NOT disabled.
    const pendingKill = screen.getByTestId("kill-poll");
    expect(pendingKill).toHaveAttribute("data-state", "pending");
    expect(pendingKill).toHaveAttribute("aria-disabled", "false");

    // API confirms the write: only now is the kill-switch shown disabled.
    const confirmed = [layer({ pending_write: false, disable_confirmed: true })];
    rerender(
      <MutationModeProvider mutationEnabled={true}>
        <AutonomyControls layers={confirmed} state={ready(confirmed, true, true)} />
      </MutationModeProvider>,
    );
    const confirmedKill = screen.getByTestId("kill-poll");
    expect(confirmedKill).toHaveAttribute("data-state", "disabled");
    expect(confirmedKill).toHaveAttribute("aria-disabled", "true");
  });

  it("disables each autonomy layer independently", () => {
    const layers = [
      layer({ layer_id: "poll", name: "Poll loop", band: "POLL_PAUSE" }),
      layer({ layer_id: "notice", name: "Auto notice", band: "AUTO_NOTICE" }),
    ];
    const onDisableLayer = vi.fn();
    render(
      <MutationModeProvider mutationEnabled={true}>
        <AutonomyControls layers={layers} state={ready(layers, true, true)} onDisableLayer={onDisableLayer} />
      </MutationModeProvider>,
    );
    // Each layer has its OWN kill-switch.
    const kills = screen.getAllByTestId(/^kill-/);
    expect(kills).toHaveLength(2);

    // Disabling one layer targets only that layer, leaving the others untouched.
    fireEvent.click(screen.getByTestId("kill-poll"));
    expect(onDisableLayer).toHaveBeenCalledTimes(1);
    expect(onDisableLayer).toHaveBeenCalledWith("poll");

    fireEvent.click(screen.getByTestId("kill-notice"));
    expect(onDisableLayer).toHaveBeenCalledTimes(2);
    expect(onDisableLayer).toHaveBeenLastCalledWith("notice");
  });
});
