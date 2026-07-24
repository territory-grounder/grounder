import { describe, it, expect } from "vitest";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, resolve } from "node:path";
import { liveEndpoints, plannedEndpoints, endpoints } from "./generated-client";

// Read the generated server contract (docs/contracts/openapi.yaml, produced by tools/gencontracts) and
// collect the distinct /v1/... path keys it declares. This is the real check the old test only pretended to
// be: it binds the console's endpoint URLs to the surface the control plane actually serves, so the console
// can never again ship a route the server does not expose (the drift that left 5 phantom endpoints).
const here = dirname(fileURLToPath(import.meta.url));
const spec = readFileSync(resolve(here, "../../../docs/contracts/openapi.yaml"), "utf8");
const contractPaths = new Set([...spec.matchAll(/^ {2}(\/v1\/[^\s:]+):/gm)].map((m) => m[1]));
// Two-segment base of a path, robust to {param} templating: /v1/decisions/{id}/action -> /v1/decisions.
const base = (p: string) => "/" + p.split("/").slice(1, 3).join("/");
const contractBases = new Set([...contractPaths].map(base));
const serverPath = (u: string) => u.replace(/^\/api/, "");

describe("the console's API layer is bound to the generated server contract", () => {
  it("loads the contract paths (guards against an empty/mis-resolved spec passing vacuously)", () => {
    expect(contractPaths.size).toBeGreaterThan(0);
    expect(contractPaths.has("/v1/stats")).toBe(true);
  });

  it("every LIVE endpoint maps to a path the server actually serves", () => {
    for (const [name, url] of Object.entries(liveEndpoints)) {
      if (typeof url === "function") {
        // A TEMPLATED live route (e.g. /v1/skills/{name}): resolve it with placeholder args and base-match,
        // exactly as the planned check does below — an exact match is impossible against a {param} path key.
        const fn = url as (...args: unknown[]) => string;
        const resolved = serverPath(fn(...Array(fn.length).fill("x")));
        expect(
          contractBases.has(base(resolved)),
          `live endpoint '${name}' (${resolved}) is not served by the contract — the console would 404`,
        ).toBe(true);
      } else {
        expect(
          contractPaths.has(serverPath(url)),
          `live endpoint '${name}' (${url}) is not served by the contract — the console would 404`,
        ).toBe(true);
      }
    }
  });

  it("every PLANNED endpoint is still absent from the contract (promote it once served)", () => {
    const resolved: Record<string, string> = {
      manifest: plannedEndpoints.manifest("id"),
    };
    for (const [name, url] of Object.entries(resolved)) {
      expect(
        contractBases.has(base(serverPath(url))),
        `planned endpoint '${name}' (${url}) is now served — move it to liveEndpoints`,
      ).toBe(false);
    }
  });

  it("every endpoint is classified as either live or planned", () => {
    const classified = new Set([...Object.keys(liveEndpoints), ...Object.keys(plannedEndpoints)]);
    for (const name of Object.keys(endpoints)) {
      expect(classified.has(name), `endpoint '${name}' must be in liveEndpoints or plannedEndpoints`).toBe(true);
    }
  });
});
