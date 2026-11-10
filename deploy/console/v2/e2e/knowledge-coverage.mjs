// Console e2e — the Knowledge wiki must state how much of the incident history it actually covers.
//
// The pages are composed from GET /v1/sessions at limit=50. The spine holds ~1,300 sessions, so the surface
// presents 3.8% of the incident history. That is a reasonable page size; presenting it as though it were
// the whole record is not. A reader deciding "has this host had trouble before" gets a confident No from a
// wiki that only looked at the most recent slice.
//
// Run: CONSOLE_BASE=http://127.0.0.1:8137 node knowledge-coverage.mjs
import { chromium } from 'playwright';
const BASE = process.env.CONSOLE_BASE || 'http://127.0.0.1:8137';
const failures = [];
const ok = (c, m) => { if (!c) failures.push(m); };

const sessions = Array.from({ length: 50 }, (_, i) => ({
  external_ref: `ref-${i}`, band: 'AUTO', risk_level: 'low', verdict: 'match',
  signals: { host: 'dc1mealie01' }, classified_at: '2026-07-28T10:00:00Z',
}));

async function mount(page, total) {
  await page.route('**/api/**', async route => {
    const p = route.request().url().split('/api')[1].split('?')[0];
    if (p === '/v1/whoami') return route.fulfill({ json: { source: 'operator:t', mode: 'Shadow', may_actuate: false, posture_stale: false, posture_source: 'runtime' } });
    if (p === '/v1/sessions') return route.fulfill({ json: total === null ? { sessions } : { sessions, total } });
    if (p === '/v1/estate') return route.fulfill({ json: { available: true, node_count: 1, nodes: [{ name: 'dc1mealie01', type: 'lxc' }], edges: [] } });
    if (p === '/v1/alerts') return route.fulfill({ json: { alerts: [], counts: { total: 0, last_24h: 0 } } });
    if (p === '/v1/actions') return route.fulfill({ json: { actions: [], counts: { total: 0, verified: 0, deviations: 0 } } });
    return route.fulfill({ json: {} });
  });
  await page.goto(BASE + '/index.html#knowledge', { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('#appRoot:not([hidden])', { timeout: 20000 });
  // /v1/sessions (the coverage figure's source) is fetched in-chain inside liveAdopt(); lastRefresh, its
  // last statement, is set only after that fetch AND the post-adopt route() re-render.
  await page.waitForFunction(() => typeof liveState !== 'undefined' && liveState.lastRefresh != null).catch(() => {});
  await page.evaluate(() => { const a = [...document.querySelectorAll('.navi')].find(x => x.dataset.view === 'knowledge'); if (a) a.click(); });
  // The click's route('knowledge') is synchronous over already-loaded data; a reflow flush is enough
  // margin for the DOM to settle, not a guess at fetch latency that no longer applies here.
  await page.evaluate(() => new Promise(r => requestAnimationFrame(() => r())));
  return page.evaluate(() => document.querySelector('#view')?.innerText || '');
}

const browser = await chromium.launch();
try {
  // 1. The spine is far larger than the page — the surface must say so, with both numbers.
  {
    const page = await browser.newContext({ viewport: { width: 1600, height: 1100 } }).then(c => c.newPage());
    const text = await mount(page, 1306);
    ok(/most recent 50 of 1306/.test(text),
      'the Knowledge wiki does not state its coverage. It is composed from 50 of 1306 recorded incidents ' +
      'and reads as the complete record, so "has this host had trouble before" gets a confident No from a ' +
      'page that only looked at the newest slice');
    await page.close();
  }
  // 2. An older control plane sends no total — say NOTHING rather than something wrong.
  {
    const page = await browser.newContext({ viewport: { width: 1600, height: 1100 } }).then(c => c.newPage());
    const text = await mount(page, null);
    ok(!/most recent .* of /.test(text),
      'a coverage claim was rendered with no total from the control plane — an invented denominator is ' +
      'worse than an absent one');
    await page.close();
  }
} finally { await browser.close(); }

if (failures.length) { console.error('KNOWLEDGE-COVERAGE E2E FAIL:\n  - ' + failures.join('\n  - ')); process.exit(1); }
console.log('KNOWLEDGE-COVERAGE E2E PASS — the wiki states how much of the incident history it covers, and stays silent when the control plane gives it no denominator.');
