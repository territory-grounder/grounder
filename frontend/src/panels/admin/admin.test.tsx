import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import { MutationModeProvider } from "../../mode/mode-provider";
import { guardedReady, type GuardedState } from "../../lib/guarded-state";
import { AdminPanel, type AdminData } from "./admin-panel";

const adminData: AdminData = {
  users: [
    { user_id: "u1", display_name: "Ada Approver", roles: ["approver"] },
    { user_id: "u2", display_name: "Otto Operator", roles: ["operator", "viewer"] },
  ],
  assignable_roles: ["approver", "operator", "administrator", "viewer"],
  on_call: { current_on_call: "Ada Approver", escalation_chain: ["Otto Operator", "administrator"] },
  modules: [
    { module_id: "connector.github", enabled: true },
    { module_id: "connector.slack", enabled: false },
  ],
};

function renderPanel(opts: {
  isAdministrator: boolean;
  mutationEnabled: boolean;
  authorized?: boolean;
}) {
  const state: GuardedState<AdminData> = guardedReady<AdminData>(
    adminData,
    opts.mutationEnabled,
    opts.authorized ?? true,
  );
  return render(
    <MutationModeProvider mutationEnabled={opts.mutationEnabled}>
      <AdminPanel isAdministrator={opts.isAdministrator} state={state} />
    </MutationModeProvider>,
  );
}

describe("AdminPanel (T-010-8 / REQ-612)", () => {
  it("renders the user, RBAC, on-call, and module management sections for an administrator caller", () => {
    renderPanel({ isAdministrator: true, mutationEnabled: true });

    // All four administrator management surfaces are present.
    expect(screen.getByRole("region", { name: "Users" })).toBeInTheDocument();
    expect(screen.getByRole("region", { name: "Role assignments" })).toBeInTheDocument();
    expect(
      screen.getByRole("region", { name: "On-call rotation and escalation" }),
    ).toBeInTheDocument();
    expect(screen.getByRole("region", { name: "Module enablement" })).toBeInTheDocument();

    // The role-assignment form lives in rbac-forms.tsx.
    expect(screen.getByRole("form", { name: "Assign RBAC roles" })).toBeInTheDocument();
  });

  it("is absent (renders nothing) for a caller without the administrator role", () => {
    const { container } = renderPanel({ isAdministrator: false, mutationEnabled: true });

    expect(container).toBeEmptyDOMElement();
    expect(screen.queryByRole("region", { name: "Administration" })).not.toBeInTheDocument();
  });

  it("shows no mutating control for an administrator while the console is read-only", () => {
    // Even for an administrator, read-only mode (mutation OFF) exposes no write control — the browser
    // reflects server authority and never enables a control the server did not grant (REQ-602).
    renderPanel({ isAdministrator: true, mutationEnabled: false });

    // The management sections still render read-only...
    expect(screen.getByRole("region", { name: "Role assignments" })).toBeInTheDocument();
    // ...but no grant / add-user / enablement write control is present.
    expect(screen.queryByRole("button")).not.toBeInTheDocument();
  });
});
