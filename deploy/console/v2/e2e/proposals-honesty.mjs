// Console e2e — the PROPOSALS view renders live states honestly and offers no mutating control.
//
// spec/026 REQ-2607 (T-026-6). The open proposal plane records fixes the agent named with no registered
// op-class. Three honesty properties, each of which has a real failure mode behind it:
//   1. FAIL-CLOSED IS VISIBLE: a 503 from /v1/proposals (nil reader — the spine is deliberately unwired
//      until spec/027's composition seam lands) must render an honest unavailable state, never an
//      empty-but-plausible table (INV-15's fabricated-row class).
//   2. ROWS AND THE BADGE ARE REAL: with rows served, the table renders the persisted screened fields and
//      the rail badge shows the API's total — the badge must never display a number no API returned.
//   3. NO MUTATING CONTROL EXISTS: not even a disabled one. These rows seed spec/028's candidate dossiers;
//      a ratify/dismiss/execute control on THIS plane — functional or decorative — lies about what the
//      plane can do (ratification is spec/028's gated write lane).
import { chromium } from 'playwright';
const BASE = process.env.CONSOLE_BASE || 'http://127.0.0.1:8137';
const failures = [];
const ok = (c, m) => { if (!c) failures.push(m); };

const rows = [
  { external_ref: 'lnms-183900', host: 'dc1mealie01', alert_rule: 'Service up/down',
    op: 'renew', op_class: 'renew-certificate', rationale: 'cert expired per OBSERVATION[lnms-183900-a]',
    undo_sketch: 'reinstall the previous certificate from the backup bundle', confidence: 0.82,
    attribution: 'agent', created_at: '2026-07-31T12:00:00Z' },
  { external_ref: 'lnms-183901', host: 'dc1wallos01', alert_rule: 'Devices up/down',
    op: 'unlock', op_class: 'unlock-resource', rationale: 'stale lock held by finished job',
    undo_sketch: 're-create the lock file with the recorded owner', confidence: 0.71,
    attribution: 'agent', created_at: '2026-07-31T12:05:00Z' },
];

async function mount(page, proposalsReply) {
  await page.route('**/api/**', async route => {
    const p = route.request().url().split('/api')[1].split('?')[0];
    if (p === '/v1/whoami') return route.fulfill({ json: { source: 'operator:t', mode: 'Shadow', may_actuate: false, posture_stale: false, posture_source: 'runtime' } });
    if (p === '/v1/proposals') {
      if (proposalsReply === null) return route.fulfill({ status: 503, body: 'proposals unavailable' });
      return route.fulfill({ json: proposalsReply });
    }
    if (p === '/v1/sessions') return route.fulfill({ json: { sessions: [], total: 0 } });
    if (p === '/v1/actions') return route.fulfill({ json: { actions: [], counts: { total: 0, verified: 0, deviations: 0 } } });
    if (p === '/v1/alerts') return route.fulfill({ json: { alerts: [] } });
    // The estate read is not optional for the console's adopt path (same reason every oracle here mocks it).
    if (p === '/v1/estate') return route.fulfill({ json: { available: true, node_count: 1, nodes: [{ name: 'dc1mealie01', type: 'lxc' }], edges: [] } });
    return route.fulfill({ json: {} });
  });
  await page.goto(BASE + '/index.html#proposals', { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('#appRoot:not([hidden])', { timeout: 20000 });
  // propView() has no de-dupe guard: EVERY render (mid-boot, liveAdopt()'s post-adopt re-render, and the
  // click below) replaces the slot with a fresh "Loading live shadow proposals…" placeholder and starts a
  // fresh /v1/proposals fetch. Waiting past that placeholder to terminal content is the same trap as a
  // "Reading…" drawer paint elsewhere in this suite — a wait on non-empty #view text alone would resolve
  // against the placeholder itself.
  const settled = () => {
    const t = document.querySelector('#view')?.innerText || '';
    return t.length > 0 && !t.includes('Loading live shadow proposals');
  };
  await page.waitForFunction(settled).catch(() => {});
  await page.evaluate(() => { const a = [...document.querySelectorAll('.navi')].find(x => x.dataset.view === 'proposals'); if (a) a.click(); });
  await page.waitForFunction(settled).catch(() => {});
}

const browser = await chromium.launch();
try {
  // 1. fail-closed 503 → honest unavailable state, zero fabricated rows.
  {
    const page = await (await browser.newContext()).newPage();
    await mount(page, null);
    const text = await page.evaluate(() => document.querySelector('#view')?.innerText || '');
    ok(/unavailable|fail-closed/i.test(text), '503: the view must say the surface is unavailable/fail-closed');
    ok(!/renew-certificate|dc1mealie01/.test(text), '503: no fabricated proposal row may render');
    const badge = await page.evaluate(() => document.querySelector('[data-badge="proposals"]')?.textContent || '');
    ok(badge.trim() === '—', `503: the rail badge must stay em-dash (real counts only), got ${JSON.stringify(badge)}`);
    await page.context().close();
  }
  // 2. rows render honestly; the badge shows the API's real total.
  {
    const page = await (await browser.newContext()).newPage();
    await mount(page, { proposals: rows, total: rows.length });
    const text = await page.evaluate(() => document.querySelector('#view')?.innerText || '');
    ok(text.includes('renew-certificate'), 'rows: the free-form op_class must render');
    ok(text.includes('reinstall the previous certificate'), 'rows: the undo sketch must render');
    ok(/never execut/i.test(text), 'rows: the structurally-unexecutable statement must be visible');
    const badge = await page.evaluate(() => document.querySelector('[data-badge="proposals"]')?.textContent || '');
    ok(badge.trim() === '2', `rows: the badge must show the REAL total 2, got ${JSON.stringify(badge)}`);
    // 3. no mutating control: no button/input/select inside the view, no ratify/dismiss/execute affordance.
    const controls = await page.evaluate(() =>
      [...document.querySelectorAll('#view button, #view input, #view select, #view [role="button"]')].length);
    ok(controls === 0, `no-mutation: the view must contain zero interactive controls, found ${controls}`);
    ok(!/ratify|dismiss\b|execute now/i.test(text), 'no-mutation: no ratify/dismiss/execute affordance may appear');
    await page.context().close();
  }
  // empty spine → honest empty state (a real state, not an error).
  {
    const page = await (await browser.newContext()).newPage();
    await mount(page, { proposals: [], total: 0 });
    const text = await page.evaluate(() => document.querySelector('#view')?.innerText || '');
    ok(/No shadow proposals/i.test(text), 'empty: the honest empty state must render');
    ok(!/unavailable/i.test(text), 'empty: an empty spine is not the unavailable state');
    await page.context().close();
  }
} finally {
  await browser.close();
}
if (failures.length) {
  console.error('proposals-honesty FAILURES:\n - ' + failures.join('\n - '));
  process.exit(1);
}
console.log('proposals-honesty: OK');
