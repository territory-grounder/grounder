import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// Vite build for the grounder operator console. The dev server proxies /api to the control-plane; the
// console builds its API layer ONLY from the generated OpenAPI client (spec/010, single-contract).
export default defineConfig({
  plugins: [react()],
  server: { proxy: { "/api": "http://localhost:8080" } },
  test: {
    environment: "jsdom",
    globals: true,
    setupFiles: ["./src/test-setup.ts"],
    css: false,
  },
});
