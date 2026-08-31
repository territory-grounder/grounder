// Console e2e — the Alerts table must say how much of the front door it is showing.
//
// The heading says "recent window" and the table is a limit=50 page over a front door that has admitted
// thousands. The BADGE was fixed to report the population (!665); if the view then implies its page is the
// whole record, the defect has simply moved one level up — an operator scanning 50 rows concludes the
// estate is quiet.
import { chromium } from 'playwright';
const BASE = process.env.CONSOLE_BASE || 'http://127.0.0.1:8137';
const failures = [];
const ok = (c, m) => { if (!c) failures.push(m); };

const alerts = Array.from({ length: 50 }, (_, i) => ({
  external_ref: `a-${i}`, source_type: 'librenms', source_id: 'lnms', alert_rule: 'Device-Down',
  severity: 'critical', host: 'dc1mealie01',
  received_at: '2026-07-28T20:00:00Z', observed_at: '2026-07-28T20:00:00Z',
}));

async function mount(page, counts) {
  await page.route('**/api/**', async route => {
    const p = route.request().url().split('/api')[1].split('?')[0];
    if (p === '/v1/whoami') return route.fulfill({ json: { source: 'operator:t', mode: 'Shadow', may_actuate: false, posture_stale: false, posture_source: 'runtime' } });
    if (p === '/v1/alerts') return route.fulfill({ json: counts === null ? { alerts } : { alerts, counts } });
    if (p === '/v1/sessions') return route.fulfill({ json: { sessions: [], total: 0 } });
    if (p === '/v1/actions') return route.fulfill({ json: { actions: [], counts: { total: 0, verified: 0, deviations: 0 } } });
    // The estate read is not optional for the console's adopt path: without it the live model has no node
    // set and the view never renders. Every other oracle in this directory mocks it for the same reason.
    if (p === '/v1/estate') return route.fulfill({ json: { available: true, node_count: 1, nodes: [{ name: 'dc1mealie01', type: 'lxc' }], edges: [] } });
    return route.fulfill({ json: {} });
  });
  await page.goto(BASE + '/index.html#alerts', { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('#appRoot:not([hidden])', { timeout: 20000 });
  // /v1/alerts (the coverage figures' source) is fetched in-chain inside liveAdopt(); lastRefresh, its
  // last statement, is set only after that fetch AND the post-adopt route() re-render.
  await page.waitForFunction(() => typeof liveState !== 'undefined' && liveState.lastRefresh != null).catch(() => {});
  await page.evaluate(() => { const a = [...document.querySelectorAll('.navi')].find(x => x.dataset.view === 'alerts'); if (a) a.click(); });
  // The click's route('alerts') is synchronous over already-loaded data; a reflow flush is enough margin
  // for the DOM to settle, not a guess at fetch latency that no longer applies here.
  await page.evaluate(() => new Promise(r => requestAnimationFrame(() => r())));
  return page.evaluate(() => document.querySelector('#view')?.innerText || '');
}

const browser = await chromium.launch();
try {
  {
    const page = await browser.newContext({ viewport: { width: 1600, height: 1100 } }).then(c => c.newPage());
    const text = await mount(page, { total: 1586, last_24h: 553 });
    ok(/showing 50 of 1586 accepted/.test(text),
      'the Alerts view does not state its window. It shows a 50-row page over 1586 accepted alerts and ' +
      'reads as the whole record, so an operator scanning it concludes the estate is quiet');
    ok(/553 in the last 24h/.test(text), 'the 24h figure is missing — "recent window" with no number is not a window');
    await page.close();
  }
  {
    const page = await browser.newContext({ viewport: { width: 1600, height: 1100 } }).then(c => c.newPage());
    const text = await mount(page, null);
    ok(!/showing \d+ of /.test(text),
      'a coverage claim was rendered with no counts from the control plane — a made-up denominator is ' +
      'worse than an absent one');
    await page.close();
  }
} finally { await browser.close(); }

if (failures.length) { console.error('ALERTS-COVERAGE E2E FAIL:\n  - ' + failures.join('\n  - ')); process.exit(1); }
console.log('ALERTS-COVERAGE E2E PASS — the alerts table states how much of the front door it shows, and stays silent without counts.');
