import { describe, it, expect } from "vitest";
import { a11yButton, a11yStatus, a11yLabel } from "./baseline";

describe("a11y baseline: interactive controls are keyboard-operable with WAI-ARIA role/name/state", () => {
  it("an enabled button is focusable and labelled", () => {
    const b = a11yButton("Approve", false);
    expect(b.role).toBe("button");
    expect(b["aria-label"]).toBe("Approve");
    expect(b.tabIndex).toBe(0);
    expect(b["aria-disabled"]).toBe(false);
  });
  it("a disabled button is removed from the tab order and exposes disabled state", () => {
    const b = a11yButton("Approve", true);
    expect(b.tabIndex).toBe(-1);
    expect(b["aria-disabled"]).toBe(true);
  });
  it("a status region exposes role=status and a name", () => {
    expect(a11yStatus("Disconnected").role).toBe("status");
    expect(a11yStatus("Disconnected")["aria-label"]).toBe("Disconnected");
  });
  it("a status region is a live region, not a tab stop", () => {
    // A status message is announced, not focused — it must never join the tab order.
    expect("tabIndex" in a11yStatus("Loading")).toBe(false);
  });
  it("a static label exposes a name but is neither a live region nor a tab stop", () => {
    const l = a11yLabel("Band AUTO");
    expect(l["aria-label"]).toBe("Band AUTO");
    expect("role" in l).toBe(false);
    expect("tabIndex" in l).toBe(false);
  });
});
