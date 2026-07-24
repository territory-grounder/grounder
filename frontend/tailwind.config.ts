import type { Config } from "tailwindcss";

// Tailwind config for the console. Design tokens map risk bands + verdicts to colour, but colour is
// NEVER the sole signal (WAI-ARIA state also conveys them, spec/010 a11y).
const config: Config = {
  content: ["./index.html", "./src/**/*.{ts,tsx}"],
  theme: {
    extend: {
      colors: {
        band: { auto: "#1f7a3f", notice: "#b8860b", poll: "#a01f1f" },
      },
    },
  },
  plugins: [],
};
export default config;
