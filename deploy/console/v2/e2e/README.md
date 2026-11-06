# Console e2e — the browser oracle for the decision-tracer surfaces

The v2 operator console is a JS surface, so a Go acceptance oracle cannot drive it. This Playwright test IS
that oracle (spec/020 REQ-2012): it loads the **served** `../index.html`, mocks the `/api` backend so the REAL
console render code runs against a full enriched decision-tracer walk, and asserts the login gate is
fail-closed, every enriched step kind renders (classify → agent-cycle → credential → propose → predict →
policy → gate → verify), the credential identity is a scheme (never a secret, INV-13), and no uncaught JS
error fires.

## Run locally
```
cd deploy/console/v2/e2e
npm ci                        # installs the playwright package
npx playwright install chromium
npm test                      # serves index.html + runs the oracle (exit 0 = pass)
```
Set `E2E_SCREENSHOT=/path.png` to also capture a full-page screenshot.

CI runs it in the `console-e2e` job (verify stage), gated to `deploy/console/v2/**` changes.
