// Console e2e — #gatemargins RENDERS THE REAL WITHIN-ε GATE BOUNDARY QUEUE, NEVER A FIXTURE (TG-178).
//
// ★ WHAT THIS GUARDS AGAINST is the documented false-green trap: a console view and its own e2e once
// agreed on an IMAGINED DTO while the server sent different field names, so the surface silently fell back
// to a fixture on every load and the e2e stayed green (see e2e/reasoning.mjs's header + the Go
// core/httpapi/session_detail_contract_test.go post-mortem). A test that builds its own request AND its own
// response can only ever prove the code agrees with itself.
//
// THE PAYLOAD BELOW IS THE REAL GateBoundaryPage SHAPE, field-for-field, from core/httpapi/gate_margins.go
// and the key set pinned by core/httpapi/gate_margins_contract_test.go:
//   GateBoundaryPage  = { epsilon, cases[] }
//   GateBoundaryCase  = { action_id, external_ref, ordinal, gate, verdict, reason, margin }
// Do NOT "tidy" the names — a rename breaks the Go contract test rather than silently reverting this surface.
//
// It asserts the surface renders the REAL cases (gate / action_id / signed margin), sorted by distance to
// threshold, tagged LIVE, and that it does NOT fall back to the module's fixture when live data is present
// (the fixture's distinctive demo refs and action_id must be absent). It then proves the honesty contract on
// the two non-table states: a 503 renders "could not be read" (never an empty queue, never a fixture) and a
// genuinely empty case set renders "no gate within ε" (never a fixture).
//
// Run: CONSOLE_BASE=http://127.0.0.1:8137 node gate-margins.mjs
import { chromium } from 'playwright';
const BASE = process.env.CONSOLE_BASE || 'http://127.0.0.1:8137';
const failures = [];
const ok = (c, m) => { if (!c) failures.push(m); };

// --- the REAL GateBoundaryPage shape, verbatim keys. Margins chosen so |margin| ordering (the reviewable
//     order) differs from array order: 0.0 < 0.004 < 0.012, so the zero-margin risk gate must sort FIRST. ---
const PAGE = {
  epsilon: 0.05,
  cases: [
    { action_id: 'ace15f0072b3d941', external_ref: 'librenms-dc1-183921', ordinal: 7, gate: 'policy',
      verdict: 'pass', reason: 'confidence 0.61 > min_confidence 0.60', margin: 0.012 },
    { action_id: 'b17c9e2240ff5a83', external_ref: 'crowdsec-dc1-5120', ordinal: 4, gate: 'confidence',
      verdict: 'refuse', reason: 'confidence 0.58 < min_confidence 0.60', margin: -0.004 },
    { action_id: 'c93a01de77b4620f', external_ref: 'prometheus-dc2-0231', ordinal: 9, gate: 'risk',
      verdict: 'refuse', reason: 'risk 0.70 == max_risk 0.70 at the boundary', margin: 0.0 },
  ],
};

// The fixture the module ships for the DESIGN PREVIEW. If ANY of these reach the screen on a live shell, the
// surface fell back to a fixture — the exact defect this oracle exists to catch. Kept in sync with
// modules/gatemargins/fixtures.txt by their distinctive "demo" refs and lead action_id.
const FIXTURE_MARKERS = ['9a8cca11', 'librenms-dc1-demo-181284', 'crowdsec-dc1-demo-4471'];

async function mount(page, gatesResponder) {
  await page.route('**/api/**', async route => {
    const p = route.request().url().split('/api')[1].split('?')[0];
    if (p === '/v1/whoami') return route.fulfill({ json: { source: 'operator:tester', mode: 'Shadow', may_actuate: false, posture_stale: false, posture_source: 'runtime' } });
    if (p === '/v1/gates/within-epsilon') return gatesResponder(route);
    if (p === '/v1/sessions') return route.fulfill({ json: { sessions: [], total: 0 } });
    if (p === '/v1/alerts') return route.fulfill({ json: { alerts: [], counts: { total: 0, last_24h: 0 } } });
    if (p === '/v1/actions') return route.fulfill({ json: { actions: [], counts: { total: 0, verified: 0, deviations: 0 } } });
    return route.fulfill({ json: {} });
  });
  await page.goto(BASE + '/index.html#gatemargins', { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('#appRoot:not([hidden])', { timeout: 20000 });
  // liveState.lastRefresh is stamped at the END of liveAdopt — after the gates fetch AND the post-adopt
  // route() re-render. Waiting on it is deterministic: no fixed sleep can land after the render.
  await page.waitForFunction(() => typeof liveState !== 'undefined' && !!liveState.lastRefresh, { timeout: 20000 });
  // land on the view (post-adopt route already rendered location.hash, but click the rail to be explicit)
  await page.evaluate(() => { const a = [...document.querySelectorAll('.navi')].find(x => x.dataset.view === 'gatemargins'); if (a) a.click(); });
  // The click's route('gatemargins') is synchronous over already-loaded data (the lastRefresh wait above
  // already guaranteed that); a reflow flush is enough margin for the DOM to settle.
  await page.evaluate(() => new Promise(r => requestAnimationFrame(() => r())));
}

const viewText = page => page.evaluate(() => document.querySelector('#view')?.innerText || '');

const browser = await chromium.launch();
try {
  // =================== 1. LIVE DATA RENDERS, NO FIXTURE FALLBACK =====================================
  {
    const ctx = await browser.newContext({ viewport: { width: 1600, height: 1100 } });
    const page = await ctx.newPage();
    const errs = []; page.on('pageerror', e => errs.push(String(e)));
    await mount(page, route => route.fulfill({ json: PAGE }));

    const text = await viewText(page);
    ok(text.length > 200, `#gatemargins rendered only ${text.length} chars — nothing below is meaningful`);

    // the surface adopted the live read and says so
    // .lbl is text-transform:uppercase and innerText is layout-aware, so match case-insensitively
    ok(/LIVE · gate boundary queue/i.test(text), 'the surface is not tagged LIVE — it did not adopt the live read');
    const chip = await page.evaluate(() => !!document.querySelector('#view .sig-fix'));
    ok(!chip, 'a fixtureChip is on a LIVE shell — the surface is presenting fixture data as live');

    // a real table with one row per live case
    const rows = await page.evaluate(() => document.querySelectorAll('#view .gm-tbl tbody tr').length);
    ok(rows === PAGE.cases.length, `the boundary-case table has ${rows} rows, want ${PAGE.cases.length} — the live cases did not render`);

    // the EXACT DTO values reach the screen — this is what a wrong key name would break
    for (const g of ['policy', 'confidence', 'risk']) ok(text.includes(g), `gate "${g}" (from case.gate) is missing — the view is not reading the real key`);
    for (const a of ['ace15f00', 'b17c9e22', 'c93a01de']) ok(text.includes(a), `action_id short "${a}" (from case.action_id) is missing`);
    ok(text.includes('min_confidence'), 'case.reason did not render');

    // the signed margin — 0 is a REAL at-threshold value, shown as "0.000", never blank
    const margins = await page.evaluate(() => [...document.querySelectorAll('#view .gm-margin')].map(el => el.textContent.trim()));
    ok(margins.some(m => m.includes('0.012')), `positive margin 0.012 did not render (got ${JSON.stringify(margins)})`);
    ok(margins.some(m => m.includes('0.004')), `negative margin -0.004 did not render (got ${JSON.stringify(margins)})`);
    ok(margins.some(m => m === '0.000'), `the at-threshold margin did not render as "0.000" — a real 0 must never be blank (got ${JSON.stringify(margins)})`);

    // sorted by |margin| ascending — the zero-margin risk gate is closest to the threshold, so it is FIRST
    const firstAct = await page.evaluate(() => document.querySelector('#view .gm-tbl tbody tr .gm-act')?.textContent.trim());
    ok(firstAct === 'c93a01de', `the queue is not sorted by distance to threshold — first row is "${firstAct}", want the |margin|=0 case "c93a01de"`);

    // the margin SIDE is carried per row (negative sat on the refusing side, positive cleared, zero at it)
    const sides = await page.evaluate(() => [...document.querySelectorAll('#view .gm-tbl tbody tr')].map(r => r.getAttribute('data-side')));
    ok(sides.includes('neg') && sides.includes('pos') && sides.includes('zero'),
      `the per-row margin side is missing (got ${JSON.stringify(sides)}) — the sign of the distance is the whole point`);

    // ε is echoed so the reader never assumes the default
    ok(/ε = 0\.05/.test(text), 'the ε the set was selected on is not echoed in the header');

    // NO FIXTURE CONTENT anywhere on the live surface
    for (const marker of FIXTURE_MARKERS)
      ok(!text.includes(marker), `FIXTURE marker "${marker}" is on the LIVE surface — the view fell back to its fixture with live data present (the false-green trap)`);

    // the rail badge is the REAL live count, not a fixture number
    const badge = await page.evaluate(() => document.querySelector('[data-badge="gatemargins"]')?.textContent.trim());
    ok(badge === String(PAGE.cases.length), `the rail badge reads "${badge}", want the real live count "${PAGE.cases.length}"`);

    ok(errs.length === 0, `uncaught page errors (LIVE): ${errs.join(' | ')}`);
    await ctx.close();
  }

  // =================== 2. A 503 IS AN HONEST "COULD NOT READ", NEVER AN EMPTY QUEUE OR A FIXTURE ======
  {
    const ctx = await browser.newContext({ viewport: { width: 1600, height: 1100 } });
    const page = await ctx.newPage();
    const errs = []; page.on('pageerror', e => errs.push(String(e)));
    await mount(page, route => route.fulfill({ status: 503, body: 'gate margins unavailable' }));

    const text = await viewText(page);
    ok(/could not be read/i.test(text), 'a 503 did not render the honest "could not be read" state');
    ok(!/no gate decided within/i.test(text), 'a 503 rendered as an EMPTY queue — a failed read is not a quiet estate');
    const tbl = await page.evaluate(() => !!document.querySelector('#view .gm-tbl'));
    ok(!tbl, 'a 503 rendered a case table — a failed read must show no cases, not fabricated ones');
    for (const marker of FIXTURE_MARKERS)
      ok(!text.includes(marker), `FIXTURE marker "${marker}" appeared on a 503 — the view fell back to its fixture`);
    ok(errs.length === 0, `uncaught page errors (503): ${errs.join(' | ')}`);
    await ctx.close();
  }

  // =================== 3. A GENUINELY EMPTY QUEUE IS AN HONEST EMPTY STATE, NEVER A FIXTURE ===========
  {
    const ctx = await browser.newContext({ viewport: { width: 1600, height: 1100 } });
    const page = await ctx.newPage();
    const errs = []; page.on('pageerror', e => errs.push(String(e)));
    await mount(page, route => route.fulfill({ json: { epsilon: 0.05, cases: [] } }));

    const text = await viewText(page);
    ok(/no gate decided within/i.test(text), 'an empty case set did not render the honest "no gate within ε" state');
    const tbl = await page.evaluate(() => !!document.querySelector('#view .gm-tbl'));
    ok(!tbl, 'an empty case set rendered a table — there are no cases to show');
    for (const marker of FIXTURE_MARKERS)
      ok(!text.includes(marker), `FIXTURE marker "${marker}" appeared on an empty live queue — the view fell back to its fixture`);
    // an empty LIVE queue must NOT publish a fixture count on the rail; the real count here is 0
    const badge = await page.evaluate(() => document.querySelector('[data-badge="gatemargins"]')?.textContent.trim());
    ok(badge === '0', `an empty live queue put "${badge}" on the rail badge, want the real count "0"`);
    ok(errs.length === 0, `uncaught page errors (empty): ${errs.join(' | ')}`);
    await ctx.close();
  }

  // =================== 4. THE DESIGN PREVIEW (UNAUTHENTICATED) SHOWS THE FIXTURE, LABELLED ============
  {
    const ctx = await browser.newContext({ viewport: { width: 1600, height: 1100 } });
    const page = await ctx.newPage();
    const errs = []; page.on('pageerror', e => errs.push(String(e)));
    // whoami 401 => liveState.on stays false => the design preview renders the fixture behind fixtureChip()
    await page.route('**/api/**', route => {
      const p = route.request().url().split('/api')[1].split('?')[0];
      if (p === '/v1/whoami') return route.fulfill({ status: 401, body: 'unauthenticated' });
      return route.fulfill({ json: {} });
    });
    await page.goto(BASE + '/index.html', { waitUntil: 'domcontentloaded' });
    // whoami 401 means liveAdopt() never reveals the console or renders any view — the one thing to wait
    // for is the boot script itself finishing parsing (views populated), the same idiom aria-state.mjs and
    // print-and-signal.mjs use for this exact setGate/hidden trick.
    await page.waitForFunction(() => typeof views === 'object' && Object.keys(views).length > 5).catch(() => {});
    // the auth gate owns the screen unauthenticated; render the view directly to inspect the design preview
    const preview = await page.evaluate(() => {
      const el = typeof views === 'object' && views.gatemargins ? views.gatemargins() : null;
      return el ? el.innerText : '';
    });
    ok(preview.includes('9a8cca11') || preview.includes('demo'), 'the design preview does not render the representative fixture cases');
    const hasChip = await page.evaluate(() => {
      const el = typeof views === 'object' && views.gatemargins ? views.gatemargins() : null;
      return !!(el && el.querySelector('.sig-fix'));
    });
    ok(hasChip, 'the design preview does not carry fixtureChip() — a fixture must always be labelled as one');
    ok(errs.length === 0, `uncaught page errors (preview): ${errs.join(' | ')}`);
    await ctx.close();
  }

  await browser.close();
} catch (e) {
  await browser.close();
  console.error('GATE-MARGINS E2E ERROR:', e);
  process.exit(1);
}

if (failures.length) {
  console.error('GATE-MARGINS E2E FAIL:\n  - ' + failures.join('\n  - '));
  process.exit(1);
}
console.log('GATE-MARGINS E2E PASS — the real within-ε GateBoundaryPage renders (gate/action_id/signed margin, ε echoed, sorted by distance to threshold, tagged LIVE), the rail badge is the real count, a 503 is an honest "could not read", an empty case set is an honest empty state, and no fixture ever reaches a live shell.');
