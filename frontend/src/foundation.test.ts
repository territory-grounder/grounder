import { describe, it, expect } from "vitest";
import { modeFromServer, isReadOnly } from "./mode/mutation-mode";
import { guardedFromError, guardedReady, canShowMutatingControl } from "./lib/guarded-state";

describe("foundation: read-only by default + fail-closed", () => {
  it("defaults to read-only unless the server says mutation_enabled=true", () => {
    expect(modeFromServer(undefined)).toBe("read-only");
    expect(modeFromServer(false)).toBe("read-only");
    expect(modeFromServer(true)).toBe("operational");
    expect(isReadOnly(modeFromServer(false))).toBe(true);
  });
  it("resolves any error to the restrictive state (no data, no control)", () => {
    const s = guardedFromError({ status: 403 });
    expect(s.status).toBe("restricted");
    expect(canShowMutatingControl(s)).toBe(false);
  });
  it("shows a mutating control only when operational AND authorized", () => {
    expect(canShowMutatingControl(guardedReady({}, true, true))).toBe(true);
    expect(canShowMutatingControl(guardedReady({}, false, true))).toBe(false); // read-only
    expect(canShowMutatingControl(guardedReady({}, true, false))).toBe(false); // unauthorized
  });
});

describe("the console never enforces a safety decision client-side", () => {
  it("reflects the server's authorization; canMutate is the server flag, not computed from the band", () => {
    // A POLL_PAUSE band with a server 'authorized' grant may show a control; an AUTO band without the grant
    // may not — the BAND never determines canMutate. The browser reflects the server, it does not enforce.
    const authorized = guardedReady({ band: "POLL_PAUSE" }, true, true);
    const notAuthorized = guardedReady({ band: "AUTO" }, true, false);
    expect(canShowMutatingControl(authorized)).toBe(true);
    expect(canShowMutatingControl(notAuthorized)).toBe(false);
  });
});
