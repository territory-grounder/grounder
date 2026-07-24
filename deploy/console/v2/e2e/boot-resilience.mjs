// Console e2e — boot resilience guard. A data-shape surprise in the live spine (here: a null node in
// /v1/estate, which makes liveInjectEstateDepth throw) must NEVER break the console boot: the shell renders,
// nav routes, no uncaught pageerror. Proves the try/catch around the live-inject + post-adopt render. This is
// the guard for the "whole page broken, no links work" failure class. Run: CONSOLE_BASE=... node boot-resilience.mjs
import { chromium } from 'playwright';
const BASE = process.env.CONSOLE_BASE || 'http://127.0.0.1:8099';
const failures = [];
const ok = (c, m) => { if (!c) failures.push(m); };

// /v1/estate with a NULL node — liveInjectEstateDepth's est.nodes.map(n=>n.name) throws on it.
const badEstate = { available: true, node_count: 2, nodes: [{ name: 'good-01', type: 'vm' }, null], edges: [] };

const browser = await chromium.launch();
try {
  const page = await browser.newContext({ viewport: { width: 1440, height: 900 } }).then(c => c.newPage());
  const pageErrors = []; page.on('pageerror', e => pageErrors.push(String(e)));
  await page.route('**/api/**', async route => {
    const p = route.request().url().split('/api')[1].split('?')[0];
    if (p === '/v1/whoami') return route.fulfill({ json: { source: 'operator:tester', mutation_enabled: false, posture_stale: false, posture_source: 'runtime', role: 'trace-read' } });
    if (p === '/v1/estate') return route.fulfill({ json: badEstate });      // the poison pill
    if (p === '/v1/sessions') return route.fulfill({ json: { sessions: [] } });
    if (p === '/v1/alerts') return route.fulfill({ json: { alerts: [] } });
    return route.fulfill({ json: {} });
  });

  // land directly on #estatedepth (the view whose inject throws) — the worst case
  await page.goto(BASE + '/index.html#estatedepth', { waitUntil: 'networkidle' });
  await page.waitForTimeout(1500);

  const state = await page.evaluate(() => ({
    navLinks: document.querySelectorAll('.navi').length,
    revealed: !!(document.querySelector('#appRoot') && !document.querySelector('#appRoot').hasAttribute('hidden')),
    atGate: /Sign in to the operator console/i.test(document.body.innerText),
  }));
  ok(state.navLinks >= 15, 'the sidemenu did not render after a bad-estate boot (only ' + state.navLinks + ' nav links)');
  ok(state.revealed, 'the console shell did not reveal after a bad-estate boot');
  ok(!state.atGate, 'the console fell back to the login gate after a bad-estate boot');

  // nav must still route despite the inject throwing
  const nav = [];
  for (const v of ['workflows', 'alerts', 'command']) {
    await page.evaluate((vv) => { const a = [...document.querySelectorAll('.navi')].find(x => x.getAttribute('data-view') === vv); if (a) a.click(); }, v);
    await page.waitForTimeout(400);
    nav.push(await page.evaluate(() => location.hash));
  }
  ok(nav[0] === '#workflows' && nav[1] === '#alerts' && nav[2] === '#command', 'nav links stopped working after a bad-estate boot: ' + nav.join(','));
  ok(pageErrors.length === 0, 'an UNCAUGHT error escaped the boot try/catch: ' + pageErrors.join(' | '));
} finally { await browser.close(); }

if (failures.length) { console.error('BOOT-RESILIENCE E2E FAIL:\n  - ' + failures.join('\n  - ')); process.exit(1); }
console.log('BOOT-RESILIENCE E2E PASS — a poison-pill estate response can no longer break the console: shell renders, nav routes, no uncaught error.');
