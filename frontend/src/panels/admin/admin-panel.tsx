import { canShowMutatingControl, type GuardedState } from "../../lib/guarded-state";
import { useMutationMode } from "../../mode/mode-provider";
import { a11yButton, a11yStatus } from "../../a11y/baseline";
import { RbacForms } from "./rbac-forms";

// The administrator surface the console reflects (REQ-612). These are the props shape the panel consumes;
// the caller fetches them through the generated OpenAPI client and hands them down — the panel never
// fetches, never computes authority, and never enforces a safety decision (spec/010). Roles/on-call/module
// state shown here are the SERVER's values; the browser only renders them.
export interface AdminUser {
  user_id: string;
  display_name: string;
  roles: string[];
}

export interface OnCallPolicy {
  current_on_call: string;
  escalation_chain: string[];
}

export interface ModuleToggle {
  module_id: string;
  enabled: boolean;
}

export interface AdminData {
  users: AdminUser[];
  assignable_roles: string[];
  on_call: OnCallPolicy;
  modules: ModuleToggle[];
}

// Write callbacks the owner wires to the generated client. They are invoked ONLY from controls that are
// gated behind canMutate, so in read-only mode / for a non-authorized caller they can never fire.
export interface AdminActions {
  onAddUser?: () => void;
  onAssignRole?: (userId: string, role: string) => void;
  onRevokeRole?: (userId: string, role: string) => void;
  onEditOnCall?: () => void;
  onSetModuleEnabled?: (moduleId: string, enabled: boolean) => void;
}

export interface AdminPanelProps {
  // Server-provided: whether the authenticated caller holds the administrator RBAC role.
  isAdministrator: boolean;
  // Already-fetched admin data wrapped so any auth/transport error fails closed to the restrictive state.
  state: GuardedState<AdminData>;
  actions?: AdminActions;
}

export function AdminPanel({ isAdministrator, state, actions = {} }: AdminPanelProps) {
  const mode = useMutationMode();

  // REQ-612: WHERE the caller lacks the administrator role, the console SHALL NOT expose the admin panel.
  // The browser fails closed — it renders NOTHING (not a disabled shell) for a non-administrator.
  if (!isAdministrator) {
    return null;
  }

  if (state.status !== "ready") {
    const label =
      state.status === "loading"
        ? "Loading administration"
        : `Administration unavailable (${state.reason})`;
    return (
      <section aria-label="Administration">
        <h2>Users, access &amp; modules</h2>
        <p {...a11yStatus(label)}>{label}.</p>
      </section>
    );
  }

  const data = state.data;
  // A mutating (write) control renders ONLY when the console is operational AND the server authorized this
  // caller. Read-only mode — the Phase 0/1 default — forces every write control off regardless of the data.
  const canMutate = mode === "operational" && canShowMutatingControl(state);

  return (
    <section aria-label="Administration">
      <h2>Users, access &amp; modules</h2>

      <section aria-label="Users">
        <h3>Users</h3>
        <ul>
          {data.users.map((u) => (
            <li key={u.user_id}>
              <span>{u.display_name} — {u.roles.join(", ") || "no roles"}</span>
            </li>
          ))}
        </ul>
        {canMutate && actions.onAddUser && (
          <button {...a11yButton("Add user", false)} onClick={() => actions.onAddUser?.()}>
            Add user
          </button>
        )}
      </section>

      <RbacForms
        users={data.users}
        roles={data.assignable_roles}
        canMutate={canMutate}
        onAssignRole={actions.onAssignRole}
        onRevokeRole={actions.onRevokeRole}
      />

      <section aria-label="On-call rotation and escalation">
        <h3>On-call rotation and escalation</h3>
        <p>On-call: {data.on_call.current_on_call}</p>
        <p>Escalation: {data.on_call.escalation_chain.join(" → ") || "none"}</p>
        {canMutate && actions.onEditOnCall && (
          <button
            {...a11yButton("Edit on-call rotation", false)}
            onClick={() => actions.onEditOnCall?.()}
          >
            Edit rotation
          </button>
        )}
      </section>

      <section aria-label="Module enablement">
        <h3>Module enablement</h3>
        <ul>
          {data.modules.map((m) => (
            <li key={m.module_id}>
              <span>{m.module_id} — {m.enabled ? "enabled" : "disabled"}</span>
              {canMutate && actions.onSetModuleEnabled && (
                <button
                  {...a11yButton(`${m.enabled ? "Disable" : "Enable"} ${m.module_id}`, false)}
                  onClick={() => actions.onSetModuleEnabled?.(m.module_id, !m.enabled)}
                >
                  {m.enabled ? "Disable" : "Enable"}
                </button>
              )}
            </li>
          ))}
        </ul>
      </section>
    </section>
  );
}
