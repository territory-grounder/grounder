// Console e2e — THE RECORDED A2 SIGNAL IS ON THE PROPOSAL-REVIEW SURFACE (TG-307).
//
// ★ WHAT THIS GUARDS. TG-201 gave the agent a typed, source-bound diagnosis and scored it (the
// diagnosis_grounded dimension); the #reasoning walk renders the whole claim. But an operator does not
// review a pending remedy on the walk — they review it on the PROPOSALS lane (spec/026 REQ-2607), and that
// lane showed the proposed fix, the rationale and the confidence while saying NOTHING about the one fact
// that most bears on whether to trust the remedy: that the agent cited GROUNDED evidence AGAINST its own
// root cause. That is the recorded A2 failure — TG proposing a restart while holding the observation that
// the guest was stopped deliberately — invisible at the exact moment it matters.
//
// The load-bearing property is not "a diagnosis renders". It is that a proposal WHOSE claim contradicts
// itself SAYS SO on this surface, that a proposal with a clean or an absent claim does NOT raise a false
// alarm, and that the claim TEXT stays where it is gated (the row deep-links to #reasoning, which enforces
// AuthTraceRead on its own fetch) rather than leaking onto this AuthReadOnly lane.
//
// KILLING MUTATION (executed while writing this file): remove the diagnosis render from
// deploy/console/v2/modules/proposals/js.txt (the propShapeCard contradiction banner + propDiagFlag), re-run
// assemble.py, re-run this file. It goes RED on section 1: "the proposal-review surface shows no
// 'evidence against its own root cause' signal at all". Restored, it passes.
//
// The per-occurrence flags live inside the collapsed <details> log, so innerText excludes them — every
// row-level assertion is a DOM query (querySelector / textContent / getAttribute), which does not depend on
// the log being expanded. The shape-card banner is always visible.
import { chromium } from 'playwright';
const BASE = process.env.CONSOLE_BASE || 'http://127.0.0.1:8137';
const failures = [];
const ok = (c, m) => { if (!c) failures.push(m); };

const CON = 'librenms-dc1-con-1'; // the A2 case: grounded evidence against its own root cause
// Three DISTINCT (op_class, op) shapes so each maps to exactly one row and the honest states are separable.
const proposals = [
  { external_ref: CON, host: 'dc1pve01', alert_rule: 'Service up/down',
    op: 'restart guest', op_class: 'restart-guest', band: 'POLL_PAUSE',
    rationale: 'guest down; propose restart', undo_sketch: 'stop the guest', confidence: 0.72,
    attribution: 'agent', created_at: '2026-08-04T08:41:00Z',
    diagnosis_recorded: true, diagnosis_contradicted: true, diagnosis_uncited: 1 },
  { external_ref: 'librenms-dc1-clean-1', host: 'dc1db01', alert_rule: 'DiskFull-90',
    op: 'prune journal', op_class: 'prune-journal', band: 'POLL_PAUSE',
    rationale: 'journal filled /', undo_sketch: 'none', confidence: 0.9,
    attribution: 'agent', created_at: '2026-08-04T07:00:00Z',
    diagnosis_recorded: true, diagnosis_contradicted: false, diagnosis_uncited: 0 },
  { external_ref: 'librenms-dc1-none-1', host: 'dc1cache01', alert_rule: 'Service up/down',
    op: 'flush cache', op_class: 'flush-cache', band: 'POLL_PAUSE',
    rationale: 'stale keys', undo_sketch: 'none', confidence: 0.6,
    attribution: 'agent', created_at: '2026-08-04T06:00:00Z',
    diagnosis_recorded: false, diagnosis_contradicted: false, diagnosis_uncited: 0 },
];

async function mount(page) {
  await page.route('**/api/**', async route => {
    const p = route.request().url().split('/api')[1].split('?')[0];
    if (p === '/v1/whoami') return route.fulfill({ json: { source: 'operator:t', mode: 'Shadow', may_actuate: false, posture_stale: false, posture_source: 'runtime' } });
    if (p === '/v1/proposals') return route.fulfill({ json: { proposals, total: proposals.length } });
    if (p === '/v1/sessions') return route.fulfill({ json: { sessions: [], total: 0 } });
    if (p === '/v1/actions') return route.fulfill({ json: { actions: [], counts: { total: 0, verified: 0, deviations: 0 } } });
    if (p === '/v1/alerts') return route.fulfill({ json: { alerts: [] } });
    if (p === '/v1/estate') return route.fulfill({ json: { available: true, node_count: 1, nodes: [{ name: 'dc1pve01', type: 'lxc' }], edges: [] } });
    return route.fulfill({ json: {} });
  });
  await page.goto(BASE + '/index.html#proposals', { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('#appRoot:not([hidden])', { timeout: 20000 });
  await page.waitForFunction(() => [...document.querySelectorAll('.navi')].some(x => x.dataset.view === 'proposals'), null, { timeout: 20000 });
  await page.evaluate(() => { const a = [...document.querySelectorAll('.navi')].find(x => x.dataset.view === 'proposals'); if (a) a.click(); });
  await page.waitForFunction(() => {
    const t = document.querySelector('#view')?.innerText || '';
    return t.trim().length > 0 && !/Loading live shadow/i.test(t);
  }, null, { timeout: 20000 });
}

const browser = await chromium.launch();
try {
  const page = await (await browser.newContext({ viewport: { width: 1600, height: 1100 } })).newPage();
  const errs = []; page.on('pageerror', e => errs.push(String(e)));
  await mount(page);

  // ---- VACUITY FLOOR: a scan that matches nothing must FAIL, not pass quietly ----
  const text = await page.evaluate(() => document.querySelector('#view')?.innerText || '');
  ok(text.length > 200, `#proposals rendered only ${text.length} characters — the view produced nothing, so every check below is vacuous`);
  const shapes = await page.evaluate(() => document.querySelectorAll('#view [data-q="shape"]').length);
  ok(shapes === 3, `three distinct (op_class, op) proposals must collapse into 3 shapes, got ${shapes}`);

  // ---- 1. THE LOAD-BEARING SIGNAL: a self-contradicting proposal SAYS SO, on this surface ----
  const banner = await page.evaluate(() => {
    const el = document.querySelector('#view [data-q="prop-contradicted"]');
    return el ? (el.textContent || '') : null;
  });
  ok(banner !== null,
    'the proposal-review surface shows no "evidence against its own root cause" signal at all — an operator ' +
    'weighing this remedy is back to reading the rationale, exactly the A2 blind spot TG-307 delivers against');
  ok(banner !== null && /evidence against its own root cause/i.test(banner),
    `the contradiction banner reads "${banner}" — it does not name the fact it exists to surface`);
  ok(banner !== null && /grounded evidence against its own diagnosis/i.test(banner),
    'the banner does not say the evidence was GROUNDED — an ungrounded contradiction is a conjured signal, ' +
    'and the surface must not blur the two');

  // ---- 2. HONEST, NOT NOISY: exactly the contradicted proposal is flagged — clean and absent claims are not
  const bannerCount = await page.evaluate(() => document.querySelectorAll('#view [data-q="prop-contradicted"]').length);
  ok(bannerCount === 1,
    `exactly one of three shapes recorded a grounded contradiction, but ${bannerCount} banners rendered — a ` +
    'false alarm on a clean or an unrecorded claim is as corrosive as a missed one');
  const rowFlags = await page.evaluate(() => document.querySelectorAll('#view [data-q="prop-row-contradicted"]').length);
  ok(rowFlags === 1, `exactly one occurrence is contradicted, got ${rowFlags} per-row flags`);
  // The clean claim renders "grounded" (recorded, nothing against it); the absent claim renders an honest
  // em-dash — NEVER a fabricated all-clear. One of each, no more.
  const okFlags = await page.evaluate(() => document.querySelectorAll('#view .prop-diag-ok').length);
  ok(okFlags === 1, `the one grounded-and-clean proposal must read "grounded", got ${okFlags} such cells`);
  const noneFlags = await page.evaluate(() => document.querySelectorAll('#view .prop-diag-none').length);
  ok(noneFlags === 1,
    `the proposal with no recorded claim must render an honest "—", got ${noneFlags} — an absent claim shown ` +
    'as grounded is the fabrication the honest-empty state exists to prevent');

  // ---- 3. THE EVIDENCE STAYS GATED: the signal DEEP-LINKS to #reasoning, it does not quote the claim here
  const href = await page.evaluate(() => document.querySelector('#view [data-q="prop-row-contradicted"]')?.getAttribute('href') || null);
  ok(href !== null && href.includes('session=') && href.includes(CON) && href.includes('#reasoning'),
    `the contradicted row does not deep-link to its own #reasoning walk (href=${JSON.stringify(href)}) — the ` +
    'claim TEXT is AuthTraceRead-gated, so the operator reaches it through the walk, not off this read-only lane');
  const tag = await page.evaluate(() => document.querySelector('#view [data-q="prop-row-contradicted"]')?.tagName || null);
  ok(tag === 'A', `the drill-in affordance must be a plain anchor (navigation), got <${tag}>`);

  // ---- 4. STILL NO MUTATING CONTROL (the diagnosis cell must not have smuggled one in) ----
  const controls = await page.evaluate(() =>
    [...document.querySelectorAll('#view button, #view input, #view select, #view [role="button"]')].length);
  ok(controls === 0, `this plane renders nothing that executes; found ${controls} interactive controls`);

  ok(errs.length === 0, `uncaught page errors: ${errs.join(' | ')}`);
  await page.context().close();
} finally {
  await browser.close();
}
if (failures.length) {
  console.error('proposals-diagnosis FAILURES:\n - ' + failures.join('\n - '));
  process.exit(1);
}
console.log('proposals-diagnosis: OK — the contradicted proposal names its own contradiction, clean and absent ' +
  'claims raise no false alarm, and the evidence stays behind the trace-read gate the row deep-links to.');
