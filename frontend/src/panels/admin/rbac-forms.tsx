import { a11yButton } from "../../a11y/baseline";
import type { AdminUser } from "./admin-panel";

export interface RbacFormsProps {
  users: AdminUser[];
  // Assignable RBAC roles (approver, operator, administrator, viewer) — the server's set, not computed here.
  roles: string[];
  // True ONLY when the console is operational AND the server authorized this caller (canShowMutatingControl).
  canMutate: boolean;
  onAssignRole?: (userId: string, role: string) => void;
  onRevokeRole?: (userId: string, role: string) => void;
}

// The role-assignment form (spec/010 T-010-8). Every write is issued through the generated client so the API
// re-checks RBAC server-side; the browser never grants a role locally. When the console is not operational or
// the caller is not authorized (canMutate === false) NO grant/revoke control renders — the section shows the
// current server-side assignments read-only.
export function RbacForms({ users, roles, canMutate, onAssignRole, onRevokeRole }: RbacFormsProps) {
  return (
    <section aria-label="Role assignments">
      <h3>Role assignments</h3>
      <form aria-label="Assign RBAC roles" onSubmit={(e) => e.preventDefault()}>
        <ul>
          {users.map((u) => (
            <li key={u.user_id}>
              <span>{u.display_name}: {u.roles.join(", ") || "none"}</span>
              {canMutate &&
                roles.map((role) => {
                  const held = u.roles.includes(role);
                  const label = `${held ? "Revoke" : "Grant"} ${role} for ${u.display_name}`;
                  return (
                    <button
                      key={role}
                      {...a11yButton(label, false)}
                      onClick={() =>
                        held ? onRevokeRole?.(u.user_id, role) : onAssignRole?.(u.user_id, role)
                      }
                    >
                      {held ? `Revoke ${role}` : `Grant ${role}`}
                    </button>
                  );
                })}
            </li>
          ))}
        </ul>
      </form>
    </section>
  );
}
