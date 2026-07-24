// Minimal ambient declarations for the Node built-ins used ONLY by tests that read repo files (e.g.
// contract.test.ts reading the generated openapi.yaml). The console runtime never imports these — declaring
// just the few functions used keeps @types/node out of the browser build's type surface while letting
// `tsc --noEmit` typecheck the test that reads the server contract.
declare module "node:fs" {
  export function readFileSync(path: string, encoding: "utf8"): string;
}
declare module "node:url" {
  export function fileURLToPath(url: string | URL): string;
}
declare module "node:path" {
  export function dirname(path: string): string;
  export function resolve(...paths: string[]): string;
}
