// Console e2e — THE JOURNEY MADE VISIBLE (TG-236 oracle 3).
//
// The server has computed the remaining distance to candidacy since !799 and served it as
// `to_candidate`, derived from the SAME constants the promotion cron reads. Nothing rendered it. An
// operator saw a status and a recurrence count, and had no way to tell a row that is one incident from
// ratifiable apart from one that is twenty — which is the difference between a queue you check back on
// and one you stop opening.
//
// THE PAYLOADS HERE ARE THE SERVER'S REAL SHAPE, not a convenient one. That distinction is the reason
// this file exists as a sibling rather than a case in candidates-ratify.mjs: that suite stubs the queue
// with occurrences/hosts already populated, so it proved the renderer can display numbers the server
// was in fact sending as zeros for every row (fixed in !799). An e2e that invents the server's answer
// tests nothing about the seam it is named for.
import { chromium } from 'playwright';
const BASE = process.env.CONSOLE_BASE || 'http://127.0.0.1:8137';
const failures = [];
const ok = (c, m) => { if (!c) failures.push(m); };

// Exactly what core/httpapi/opclass_candidates.go emits: gap fields absent once the shape has arrived.
const queue = {
  candidates: [
    { candidate_key: 'far', op_class: 'reload-proxy', op: 'reload', status: 'observing',
      occurrences: 1, hosts: 1, tier: 'low-reversible', caller_can_act: true,
      to_candidate: { refs_needed: 2, hosts_needed: 1, span_hours_needed: 168, confidence_short: false } },
    { candidate_key: 'thin', op_class: 'restart-worker', op: 'restart', status: 'observing',
      occurrences: 3, hosts: 2, tier: 'low-reversible', caller_can_act: true,
      to_candidate: { refs_needed: 0, hosts_needed: 0, span_hours_needed: 0, confidence_short: true } },
    { candidate_key: 'arrived', op_class: 'start-guest', op: 'start', status: 'ratify_ready',
      occurrences: 9, hosts: 4, tier: 'low-reversible', caller_can_act: true },
  ],
  total: 3, caller_can_act: true,
};

async function mount(page) {
  await page.route('**/api/**', async route => {
    const p = route.request().url().split('/api')[1].split('?')[0];
    if (p === '/v1/whoami') return route.fulfill({ json: { source: 'operator:t', mode: 'Shadow', may_actuate: false, posture_stale: false, posture_source: 'runtime' } });
    if (p === '/v1/opclass/candidates') return route.fulfill({ json: queue });
    if (p === '/v1/policy/graduation') return route.fulfill({ json: { classes: [] } });
    if (p === '/v1/sessions') return route.fulfill({ json: { sessions: [], total: 0 } });
    if (p === '/v1/actions') return route.fulfill({ json: { actions: [], counts: { total: 0, verified: 0, deviations: 0 } } });
    if (p === '/v1/alerts') return route.fulfill({ json: { alerts: [] } });
    return route.fulfill({ json: {} });
  });
  await page.goto(BASE + '/index.html#candidates', { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('#appRoot:not([hidden])', { timeout: 20000 });
  await page.waitForFunction(() => [...document.querySelectorAll('.navi')].some(x => x.dataset.view === 'candidates'), null, { timeout: 20000 });
  await page.evaluate(() => { const a = [...document.querySelectorAll('.navi')].find(x => x.dataset.view === 'candidates'); if (a) a.click(); });
  await page.waitForFunction(() => {
    const t = document.querySelector('#view')?.innerText || '';
    return t.trim().length > 0 && !/Loading/i.test(t);
  }, null, { timeout: 20000 });
}

const browser = await chromium.launch();
try {
  const page = await (await browser.newContext()).newPage();
  await mount(page);
  const text = await page.evaluate(() => document.querySelector('#view')?.innerText || '');

  // 1. The distance is stated, in operator language, for a row that has not arrived.
  ok(/2 more incidents/.test(text), 'journey: a short candidate must say how many more incidents it needs');

  // 2. THE OR LEG. Candidacy needs >=3 refs AND (>=2 hosts OR >=7d span). Rendering both as required
  //    would overstate the bar and suppress a ratification TG is already ready for.
  ok(/1 more host or 7 more days of span/.test(text),
     'journey: the second leg is an OR — "host OR span", never "host AND span"');

  // 3. Confidence is a THRESHOLD, not a quantity to trade against. It must read as a separate blocker,
  //    never as something a few more incidents can offset.
  ok(/mean confidence is below the bar/.test(text),
     'journey: a confidence-blocked row must say so');
  ok(!/\d+ more confidence/.test(text), 'journey: confidence must never render as a countable remainder');

  // 4. AN ARRIVED SHAPE SHOWS NO COUNTDOWN. A queue behind an open door is not a queue, and a stale
  //    "needs N more" on something already ratifiable would actively mislead.
  const arrivedRow = await page.evaluate(() => {
    const tr = [...document.querySelectorAll('#view tr')].find(r => /start-guest/.test(r.innerText));
    return tr ? tr.innerText : '';
  });
  ok(arrivedRow.length > 0, 'journey: the ratify-ready row must render at all');
  ok(!/needs .* to become ratifiable/.test(arrivedRow),
     `journey: an ARRIVED shape must show no countdown, got ${JSON.stringify(arrivedRow)}`);

  // 5. The real recurrence counts (!799) must still render beside it — the journey is an addition, not
  //    a replacement for the number it qualifies.
  ok(/9× \/ 4 host\(s\)/.test(text), 'journey: real recurrence counts must remain visible');

  await page.context().close();
} finally {
  await browser.close();
}
if (failures.length) {
  console.error('candidates-journey FAILURES:\n - ' + failures.join('\n - '));
  process.exit(1);
}
console.log('candidates-journey: OK');
