import { describe, it, expect } from "vitest";
import { subscribe, type SseStatus } from "./sse";

describe("SSE stream: a drop shows disconnected and can reconnect", () => {
  it("transitions connecting -> connected -> disconnected on drop", () => {
    let src!: { onopen: (() => void) | null; onerror: (() => void) | null; close: () => void };
    const seen: SseStatus[] = [];
    const h = subscribe(() => {
      src = { onopen: null, onerror: null, close: () => {} };
      return src;
    }, (s) => seen.push(s));
    expect(seen).toContain("connecting");
    src.onopen!();
    expect(h.status()).toBe("connected");
    src.onerror!(); // the stream drops
    expect(h.status()).toBe("disconnected"); // the console shows the disconnected indicator
    expect(seen).toEqual(["connecting", "connected", "disconnected"]);
  });
});
