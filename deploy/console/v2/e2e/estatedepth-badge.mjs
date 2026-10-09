// Console e2e — THE ESTATE DEPTH BADGE COUNTS LIVE UNHEALTHY NODES, NOT THE FIXTURE'S.
//
// The rail badge reports how much of the estate is NOT OK. It was published once at boot by the assemble
// glue from the fixture globals and never recomputed, even though liveInjectEstateDepth() rebuilds
// ESTATE.nodes from /v1/estate a few lines later. Measured on the live console 2026-07-29: the rail read
// "Estate Depth 2" while the adopted graph held 217 nodes of which 12 were not ok. The single number that
// answers "how much of my estate is unhealthy" was under-reporting real unhealthy infrastructure six-fold.
//
// WHY THE ASSERTION IS "EQUALS THE COUNT DERIVED FROM THE API", NOT "IS NOT 2": pinning the wrong value
// would pass the moment anyone changed the fixture, while the badge stayed just as disconnected. The test
// derives the expected number from the SAME api payload it serves, so it tracks the data instead of a
// constant.
//
// Run: CONSOLE_BASE=http://127.0.0.1:8137 node estatedepth-badge.mjs
import { chromium } from 'playwright';
const BASE = process.env.CONSOLE_BASE || 'http://127.0.0.1:8137';
const failures = [];
const ok = (c, m) => { if (!c) failures.push(m); };

// 5 distinct hosts. Alerts make exactly 3 of them not-ok (2 critical, 1 warning); the other 2 stay ok.
// Chosen so the expected answer (3) cannot coincide with the fixture's own not-ok count.
const estate = {
  available: true, node_count: 5,
  nodes: [
    { name: 'dc1actualbudget01', type: 'lxc' },
    { name: 'dc1mealie01', type: 'lxc' },
    { name: 'dc1linkwarden02', type: 'lxc' },
    { name: 'dc1openarchiver01', type: 'lxc' },
    { name: 'dc1excalidraw01', type: 'lxc' },
  ],
  edges: [],
};
const alerts = [
  { host: 'dc1actualbudget01', severity: 'critical', rule: 'Service-up/down' },
  { host: 'dc1mealie01', severity: 'critical', rule: 'Device-Down' },
  { host: 'dc1linkwarden02', severity: 'warning', rule: 'Ping-loss' },
];
const EXPECTED_NOT_OK = new Set(alerts.map(a => a.host)).size; // 3, derived — never hard-coded

const browser = await chromium.launch();
try {
  {
    const page = await browser.newContext({ viewport: { width: 1600, height: 1100 } }).then(c => c.newPage());
    const errs = []; page.on('pageerror', e => errs.push(String(e)));
    await page.route('**/api/**', route => {
      const p = route.request().url().split('/api')[1].split('?')[0];
      if (p === '/v1/whoami') return route.fulfill({ json: { source: 'operator:tester', mode: 'Shadow', may_actuate: false, posture_stale: false, posture_source: 'runtime' } });
      if (p === '/v1/estate') return route.fulfill({ json: estate });
      if (p === '/v1/alerts') return route.fulfill({ json: { alerts, counts: { total: alerts.length, last_24h: alerts.length } } });
      if (p === '/v1/sessions') return route.fulfill({ json: { sessions: [] } });
      return route.fulfill({ json: {} });
    });
    await page.goto(BASE + '/index.html#estatedepth', { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('#appRoot:not([hidden])', { timeout: 20000 });
    // [data-badge="estatedepth"] stays the static "—" placeholder until liveInjectEstateDepth() + the
    // estate-depth badge block (the only writer of this element, run at the tail of liveAdopt() after ~20
    // other sequential awaited reads) recompute it from live data. Waiting for it to become a digit is the
    // exact completion signal the checks below need, not a guess at fetch latency.
    await page.waitForFunction(() =>
      /^\d+$/.test((document.querySelector('[data-badge="estatedepth"]')?.textContent || '').trim())
    ).catch(() => {});

    const badge = await page.evaluate(() =>
      (document.querySelector('[data-badge="estatedepth"]')?.textContent || '').trim());
    const graphNotOk = await page.evaluate(() =>
      (typeof ESTATE !== 'undefined' && Array.isArray(ESTATE.nodes))
        ? ESTATE.nodes.filter(n => n && n.health !== 'ok').length : -1);

    ok(graphNotOk === EXPECTED_NOT_OK,
      `the adopted graph has ${graphNotOk} not-ok nodes but the served alerts imply ${EXPECTED_NOT_OK} — ` +
      `the live inject itself is wrong, so the badge assertion below would be meaningless`);
    ok(badge === String(EXPECTED_NOT_OK),
      `the rail badge reads "${badge}" but the LIVE estate has ${EXPECTED_NOT_OK} not-ok nodes. This is the ` +
      `production defect: the badge is published once at boot from the fixture and never recomputed, so the ` +
      `one number that says "how much of my estate is unhealthy" reports invented data`);
    ok(errs.length === 0, `uncaught page errors: ${errs.join(' | ')}`);
    await page.close();
  }

  // ---- with no estate behind it, the badge states UNKNOWN — never 0, and never the fixture --------------
  //
  // ★ THIS CASE CAUGHT A HOLE IN THE FIX AND THEN IN ITS OWN FIRST VERSION. The fix originally returned
  // early when the graph was empty, leaving the boot placeholder — the FIXTURE's count — on screen. This
  // assertion originally read `badge !== '0'`, which the fixture value "2" satisfies, so the control passed
  // while the defect it exists to catch was present. Asserting the honest value EXACTLY is what closes it:
  // "not the wrong answer I thought of" is a weaker property than "the right answer".
  const FIXTURE_BADGE = await (async () => {
    // read the fixture's own count out of the page, so this never hard-codes a number that can drift
    const page = await browser.newContext().then(c => c.newPage());
    await page.route('**/api/**', route => route.abort());
    await page.goto(BASE + '/index.html', { waitUntil: 'domcontentloaded' }).catch(() => {});
    const v = await page.evaluate(() =>
      (typeof ESTATE !== 'undefined' && Array.isArray(ESTATE.nodes))
        ? String(ESTATE.nodes.filter(n => n && n.health !== 'ok').length) : '(none)');
    await page.close();
    return v;
  })();

  for (const c of [
    { name: 'estate read fails (503)', estate: route => route.fulfill({ status: 503, json: { error: 'unavailable' } }) },
    { name: 'estate is reachable but empty', estate: route => route.fulfill({ json: { available: false, node_count: 0, nodes: [], edges: [] } }) },
  ]) {
    const page = await browser.newContext({ viewport: { width: 1600, height: 1100 } }).then(x => x.newPage());
    await page.route('**/api/**', route => {
      const p = route.request().url().split('/api')[1].split('?')[0];
      if (p === '/v1/whoami') return route.fulfill({ json: { source: 'operator:tester', mode: 'Shadow', may_actuate: false, posture_stale: false, posture_source: 'runtime' } });
      if (p === '/v1/estate') return c.estate(route);
      return route.fulfill({ json: {} });
    });
    await page.goto(BASE + '/index.html#estatedepth', { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('#appRoot:not([hidden])', { timeout: 20000 });
    // Both branches here expect the badge to STAY "—" (a failed/empty estate must never paint a count), so
    // there is no value-change on the badge itself to key on. liveState.lastRefresh is set exactly once,
    // unconditionally, as the very last statement of liveAdopt() — strictly after the estate-depth badge
    // block and the post-adopt route() re-render — so waiting on it is the real "boot settled" signal.
    await page.waitForFunction(() => typeof liveState !== 'undefined' && liveState.lastRefresh != null).catch(() => {});
    const badge = await page.evaluate(() =>
      (document.querySelector('[data-badge="estatedepth"]')?.textContent || '').trim());

    ok(badge === '—',
      `${c.name}: the badge reads "${badge}", want "—". With nothing behind it the badge must say so. ` +
      `"0" would read as "nothing is unhealthy" about an estate nobody can see, and anything else is a ` +
      `number nothing produced`);
    ok(badge !== FIXTURE_BADGE || FIXTURE_BADGE === '—',
      `${c.name}: the badge reads "${badge}", which is exactly the FIXTURE's own not-ok count — the boot ` +
      `placeholder survived. This is the defect the whole change exists to remove, and asserting only ` +
      `"not 0" let it through`);
    await page.close();
  }
} finally { await browser.close(); }

if (failures.length) { console.error('ESTATEDEPTH-BADGE E2E FAIL:\n  - ' + failures.join('\n  - ')); process.exit(1); }
console.log('ESTATEDEPTH-BADGE E2E PASS — the rail badge equals the live not-ok node count, and a failed estate read never publishes a reassuring zero.');

// ---- an empty estate must not invent a host to inspect --------------------------------------------------
// core/httpapi/estate.go normalizes an absent snapshot to an EMPTY ARRAY, so every Array.isArray guard
// upstream passes and the view reached its `|| {id:"dc1demo-w3"}` fallback — rendering a full node card
// (identity, role, health chip, sparklines) for a machine that does not exist, with nothing marking it as a
// sample. Measured on the live bundle under API starvation 2026-07-29.
{
  const browser2 = await chromium.launch();
  const page = await browser2.newContext({ viewport: { width: 1600, height: 1100 } }).then(c => c.newPage());
  await page.route('**/api/**', route => {
    const p = route.request().url().split('/api')[1].split('?')[0];
    if (p === '/v1/whoami') return route.fulfill({ json: { source: 'operator:tester', mode: 'Shadow', may_actuate: false, posture_stale: false, posture_source: 'runtime' } });
    if (p === '/v1/estate') return route.fulfill({ json: { available: false, node_count: 0, nodes: [], edges: [] } });
    return route.fulfill({ json: {} });
  });
  await page.goto(BASE + '/index.html#estatedepth', { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('#appRoot:not([hidden])', { timeout: 20000 });
  // Same reasoning as the empty/failed branches above: liveState.lastRefresh is the unconditional
  // "liveAdopt() has finished, including the post-adopt route() re-render" signal, which is what the #view
  // text read below actually depends on.
  await page.waitForFunction(() => typeof liveState !== 'undefined' && liveState.lastRefresh != null).catch(() => {});
  const text = await page.evaluate(() => document.querySelector('#view')?.innerText || '');
  ok(!/demo-w3/i.test(text),
    'an EMPTY estate produced a node card for the invented host dc1demo-w3 — an operator reading their ' +
    'own estate sees one machine and no sign that the snapshot is unreadable');
  ok(/empty|no estate|unreadable/i.test(text),
    'an empty estate rendered neither nodes nor an explanation; the operator cannot tell "estate is fine and ' +
    'small" from "estate could not be read"');
  await page.close(); await browser2.close();
}
if (failures.length) { console.error('ESTATEDEPTH-BADGE E2E FAIL (empty-estate case):\n  - ' + failures.join('\n  - ')); process.exit(1); }
