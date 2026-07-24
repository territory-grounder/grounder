// Console e2e — the nav-actually-switches-the-VIEW guard (not just the hash). The failure the owner hit:
// on #workflows, clicking a sidemenu item changed the URL but the workflows view stayed mounted. This test
// lands on #workflows, lets it fully settle (auto-selects a run, opens any SSE), then clicks each sidemenu
// item and asserts the workflows view (.wf-wrap) is GONE and the target renders — then navigates BACK to
// #workflows and asserts it returns. Zero uncaught errors. Run: CONSOLE_BASE=... node nav-switch.mjs
import { chromium } from 'playwright';
const BASE = process.env.CONSOLE_BASE || 'http://127.0.0.1:8099';
const failures = [];
const ok = (c, m) => { if (!c) failures.push(m); };

// a NON-TERMINAL session so the workflows view opens its SSE stream — the exact condition under which a
// snapshot/done handler could yank the view back to #workflows.
const detail = { id: 'am-x', ref: 'am-x', title: 'live one', host: 'h1', band: 'POLL_PAUSE', status: 'proposed', risk: 'medium', conf: 0.6, action: 'a1', verdict: '',
  nodes: [{ t: 'classify', lb: 'Risk classification', ts: '2026-07-21T09:47:01Z', st: 'ok', src: 'classify', band: 'POLL_PAUSE', pay: 'admitted' }] };

async function mock(page) {
  await page.route('**/api/**', async route => {
    const p = route.request().url().split('/api')[1].split('?')[0];
    if (p === '/v1/whoami') return route.fulfill({ json: { source: 'operator:tester', mutation_enabled: false, posture_stale: false, posture_source: 'runtime', role: 'trace-read' } });
    if (p === '/v1/sessions') return route.fulfill({ json: { sessions: [{ external_ref: 'am-x', band: 'POLL_PAUSE', risk_level: 'medium', verdict: null }] } });
    if (p === '/v1/sessions/am-x') return route.fulfill({ json: detail });
    if (p.endsWith('/stream')) return route.fulfill({ status: 200, headers: { 'Content-Type': 'text/event-stream', 'Cache-Control': 'no-cache' }, body: `event: snapshot\ndata: ${JSON.stringify(detail)}\n\n` }); // stream stays "open" (no done)
    if (p === '/v1/estate') return route.fulfill({ json: { available: true, node_count: 1, nodes: [{ name: 'h1', type: 'vm' }], edges: [] } });
    return route.fulfill({ json: {} }); // benign for alerts/knowledge/ledger/governance/etc.
  });
}

const browser = await chromium.launch();
try {
  const page = await browser.newContext({ viewport: { width: 1440, height: 900 } }).then(c => c.newPage());
  const pageErrors = []; page.on('pageerror', e => pageErrors.push(String(e)));
  await mock(page);
  await page.goto(BASE + '/index.html#workflows', { waitUntil: 'networkidle' });
  await page.waitForTimeout(1600); // settle: adopt, auto-select the run, open its SSE

  ok(await page.evaluate(() => !!document.querySelector('.wf-wrap')), 'the workflows view did not mount to begin with');

  // click AWAY from workflows; wait long enough that an SSE snapshot would have yanked back if that were the bug
  for (const v of ['estate', 'knowledge', 'ledger', 'governance', 'alerts']) {
    await page.evaluate((vv) => { const a = [...document.querySelectorAll('.navi')].find(x => x.getAttribute('data-view') === vv); if (a) a.click(); }, v);
    await page.waitForTimeout(900);
    const stuck = await page.evaluate(() => ({ wf: !!document.querySelector('.wf-wrap'), hash: location.hash }));
    ok(!stuck.wf, 'clicking "' + v + '" left the workflows view mounted (nav switched the hash but not the VIEW): hash=' + stuck.hash);
  }
  // navigate BACK to workflows — it must return
  await page.evaluate(() => { const a = [...document.querySelectorAll('.navi')].find(x => x.getAttribute('data-view') === 'workflows'); if (a) a.click(); });
  await page.waitForTimeout(1000);
  ok(await page.evaluate(() => !!document.querySelector('.wf-wrap')), 'navigating back to #workflows did not re-mount the view');
  ok(pageErrors.length === 0, 'uncaught JS errors during nav: ' + pageErrors.join(' | '));
} finally { await browser.close(); }

if (failures.length) { console.error('NAV-SWITCH E2E FAIL:\n  - ' + failures.join('\n  - ')); process.exit(1); }
console.log('NAV-SWITCH E2E PASS — clicking a sidemenu item actually switches the VIEW (workflows unmounts, target renders, and back returns), no SSE yank-back, no JS error.');
