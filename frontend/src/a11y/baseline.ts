// a11y baseline: helpers that stamp the WAI-ARIA attributes the console's elements need. Three roles, kept
// distinct on purpose (spec/010 a11y):
//
//   - a11yButton — an interactive control: role=button, an accessible name, disabled state, and tab-order
//       membership (tabIndex 0 enabled / -1 disabled).
//   - a11yStatus — a LIVE REGION for asynchronous, changing status: a loading placeholder, a restricted /
//       unavailable notice, an empty state, or a verification verdict. It is announced when it appears or
//       changes, and it is NOT a tab stop — you don't Tab to a status message.
//   - a11yLabel  — a static accessible name for a persistent data element whose visible text needs framing
//       (e.g. a bare hash that should read as "Action <id>"). NOT a live region and NOT focusable.
//
// Reserve a11yStatus for content that actually changes. Persistent data uses a11yLabel — or just its own
// visible text — so a screen reader is not flooded with live-region announcements on load. Colour is never
// the sole signal: the accessible name carries the same meaning.
export interface A11yButtonProps {
  role: "button";
  "aria-label": string;
  "aria-disabled": boolean;
  tabIndex: number;
}

export interface A11yStatusProps {
  role: "status";
  "aria-label": string;
}

export interface A11yLabelProps {
  "aria-label": string;
}

export function a11yButton(label: string, disabled: boolean): A11yButtonProps {
  return { role: "button", "aria-label": label, "aria-disabled": disabled, tabIndex: disabled ? -1 : 0 };
}

// A polite live region for asynchronous / changing status. Deliberately not focusable — a status message is
// not an interactive control, so it never joins the tab order.
export function a11yStatus(label: string): A11yStatusProps {
  return { role: "status", "aria-label": label };
}

// A static accessible name for persistent data. No role, no live region, no tab stop — use it only when the
// element's visible text alone does not carry the framing a screen-reader user needs.
export function a11yLabel(label: string): A11yLabelProps {
  return { "aria-label": label };
}
