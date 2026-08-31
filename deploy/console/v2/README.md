# Operator console v2 (approved design, self-contained)

This is the **approved** Territory Grounder operator console (2026-07-17): a single self-contained
`index.html` (fonts inlined, no external requests) served by the console image as the site root.

- `console.html` — the editable source (markers: `/*FONTS*/`, `/*EXT_CSS*/`, `/*EXT_JS*/`)
- `fonts.css` — IBM Plex Sans/Mono latin 400/500/600 as data-URIs (generated from the repo's
  `@fontsource` packages)
- `modules/<name>/{css,fixtures,js,wiring}.txt` — the 7 surface modules (workflows, logs, signals,
  models, modules, secrets, estate2) + `_charts` (svgEl/sparkline/areaChart/metricPanel/donut/hbar)
- `assemble.py` — deterministic build: `python3 assemble.py` → `index.html` (byte-reproducible)

**Honesty contract:** every value shown is a labeled fixture (`DESIGN PREVIEW`, `FIXTURE` chips);
every actuation/config-write control is rendered but gated with a *mutation OFF* explanation. This
stays true in production until the browser-auth path + read endpoints land and the React port
(`frontend/src`, spec/010) is wired to live data — at which point the Dockerfile override that serves
this file is removed and the Vite build takes over again.

14 surfaces: Command · Workflows · Reasoning · Estate · Estate Depth · Alerts · Signals ·
Logs/Evidence · Actions · Ledger · Governance · Models · Modules · Secrets.
Verified: 42 view×theme×viewport combos, 0 console errors, 0 horizontal overflow (Playwright).
