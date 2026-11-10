/* approval-header-never-invents — the decision tracer's header is read BEFORE APPROVING AN ACTION.
 *
 * #workflows renders a header of five values — exec, reversible, risk, conf, blast — plus an elapsed clock.
 * The live projection carried none of them for a real session, and every default fell on the reassuring
 * side of the question the operator is actually asking:
 *
 *   reversible: d.reversible !== undefined ? d.reversible : true   -> the DANGEROUS state could never render
 *   risk:       WF_RISK_SCORE[label] ?? 0.4                        -> an unknown category became a 0.40 meter
 *   conf:       Number(d.conf) || 0                                -> absent became indistinguishable from measured 0
 *   blast:      d.blast || 0                                       -> "0 hosts" for an unknown blast radius
 *   elapsed:    0, and the ticker iterated the FIXTURE array       -> live runs read "elapsed 0s" forever
 *
 * "Unknown" and "safe" are not the same claim, and on this header only one of them is ever honest. The
 * elapsed one is its own trap: 0s does not read as "no data", it reads as "this just started" — the precise
 * opposite of the fact an operator wants when asking how long something has been stuck awaiting a vote.
 *
 * This oracle drives a session whose DTO omits those fields and asserts the header says so.
 *
 * RED MUTATION CONTROLS, executed 2026-08-01, each restored green:
 *   1. reversible default back to `true`  -> "reversible reads "yes" for a record that does not carry it"
 *   2. risk default back to 0.4           -> "risk reads "0.40" …"
 *   3. clock iterates WF_RUNS again       -> "live elapsed stayed "0s" across two ticks"
 */
import { chromium } from 'playwright';

const BASE = process.env.CONSOLE_BASE || `http://127.0.0.1:${process.env.CONSOLE_E2E_PORT || '8137'}`;
const failures = [];
const ok = (cond, msg) => { if (!cond) failures.push(msg); };

const EMPTY = {
  alerts: [], sessions: [], actions: [], proposals: [], candidates: [], entries: [], decisions: [],
  items: [], rules: [], pages: [], skills: [], models: [], modules: [], sources: [], resolutions: [],
  coverage: [], lane_coverage: [], refs: [], sealed: [], classes: [], available: false, node_count: 0,
};
const WHOAMI = { source: 'operator:tester', mode: 'Shadow', may_actuate: false, posture_stale: false, posture_source: 'runtime' };

/* Classified 42 minutes ago and still awaiting a vote — the shape an operator is deciding about. The row
 * carries classified_at (a REAL instant the spine owns) and the detail carries NO reversible / risk /
 * conf / blast, which is exactly what a real session detail looks like today. */
const CLASSIFIED_AT = new Date(Date.now() - 42 * 60000).toISOString();
const SESSION = {
  external_ref: 'librenms-dc1-1406', band: 'POLL_PAUSE', risk_level: '', action_id: 'act-1406',
  auto_approved: false, notify_required: true, operator_override: false, classified_at: CLASSIFIED_AT,
};
const DETAIL = { id: 'librenms-dc1-1406', ref: 'librenms-dc1-1406', host: 'dc1mealie01', band: 'POLL_PAUSE' };

async function open(page, { failDetail = false } = {}) {
  await page.route('**/api/**', route => {
    const p = route.request().url().split('/api')[1].split('?')[0];
    if (p === '/v1/whoami') return route.fulfill({ json: WHOAMI });
    if (p === '/v1/sessions') return route.fulfill({ json: { sessions: [SESSION], total: 1 } });
    if (p.startsWith('/v1/sessions/')) {
      return failDetail ? route.fulfill({ status: 503, json: {} }) : route.fulfill({ json: DETAIL });
    }
    return route.fulfill({ json: EMPTY });
  });
  await page.goto(`${BASE}/index.html#workflows`, { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('#appRoot:not([hidden])', { timeout: 20000 });
  // The header this file reads (.wf-met / .wf-el) is painted from the run object liveWfRuns() builds off
  // the SESSION row (available immediately) and then merges the detail DTO into once wfOnSelect's fetch
  // resolves — a fire-and-forget lazy-load kicked off from inside views.workflows()'s own render, never
  // awaited by liveAdopt(), so liveState.lastRefresh does not cover it (the same race batch4 found in the
  // skills/wiki badges).
  //
  // A second, sneakier trap sits behind that one: document.querySelector('.wf-el') matches the RUN-LIST
  // row's elapsed badge (workflows/js.txt renders it via the unguarded wfElapsedF(r.elapsed), and r.elapsed
  // is always null) BEFORE the header's own .wf-el (wfElapsedText(r), which correctly derives from
  // r.startedMs) — so the very first paint after the merge reads "nulls" on the list row, and only the
  // elapsed ticker's next tick (which repaints every [data-wf-clock] element, list row included, via the
  // correct wfElapsedText) overwrites it. A run._loaded/_failed condition resolves the instant the merge
  // lands — before that first tick — and reads "nulls" almost every time; the original fixed 3000ms sleep
  // merely outlasted the ticker by luck. Waiting on the SAME format the first assertion below checks
  // sidesteps both races at once: it is false on "nulls" (and on the pre-merge scaffold), so it can only
  // resolve once the list row has been repainted with a real duration.
  await page.waitForFunction(() => {
    const t = document.querySelector('.wf-el')?.textContent?.trim() || '';
    return /^\d+m \d+s$/.test(t) || /^\d+h/.test(t);
  }).catch(() => {});
}
/* Read one header metric by its label — structural, not a proximity regex over the page text. */
const met = (page, key) => page.evaluate(k => {
  const m = [...document.querySelectorAll('.wf-met')]
    .find(el => (el.querySelector('.wf-mk')?.textContent || '').trim().toLowerCase() === k);
  return m ? (m.querySelector('.wf-mv')?.textContent || '').trim() : null;
}, key);

const browser = await chromium.launch();
try {
  const page = await browser.newContext({ viewport: { width: 1600, height: 1200 } }).then(c => c.newPage());
  const errs = []; page.on('pageerror', e => errs.push(String(e)));
  await open(page);

  ok(errs.length === 0, `uncaught page errors: ${errs.join(' | ')}`);

  const rev = await met(page, 'reversible');
  ok(rev !== null, 'VACUITY: no .wf-met labelled "reversible" — the header selector stopped matching');
  ok(rev === null || /unknown/i.test(rev),
    `reversible reads ${JSON.stringify(rev)} for a record that does not carry it. "yes" here is the ` +
    `console telling an operator an action can be undone on no evidence — the one default that must ` +
    `never fall on the reassuring side.`);

  const risk = await met(page, 'risk');
  ok(risk === '—',
    `risk reads ${JSON.stringify(risk)} with no risk category on the record — a two-decimal figure looks ` +
    `measured, and 0.40 was a placeholder wearing the costume of a measurement`);

  const conf = await met(page, 'conf');
  ok(conf === '—',
    `conf reads ${JSON.stringify(conf)} with no confidence on the record — "0.00" is indistinguishable ` +
    `from a real measured zero, which is a different fact`);

  const blast = await met(page, 'blast');
  ok(blast === '—',
    `blast reads ${JSON.stringify(blast)} with no blast radius on the record — "0 hosts" asserts an ` +
    `unusually safe action, which is the opposite of "we do not know"`);

  /* The meters are pictures of measurements; with nothing measured there must be nothing drawn. */
  const meters = await page.evaluate(() => document.querySelectorAll('.wf-head .wf-meter').length);
  ok(meters === 0,
    `${meters} meter bar(s) drawn for values the record does not carry — a bar at a default position is a ` +
    `measurement claim made in pixels rather than digits`);

  /* ELAPSED. The run was classified 42 minutes ago, so the honest answer is ~42m — never "0s", and never
   * frozen. Two samples a second apart prove the clock is actually attached to the live run. */
  const e1 = await page.evaluate(() => document.querySelector('.wf-el')?.textContent?.trim() || '');
  ok(/^\d+m \d+s$/.test(e1) || /^\d+h/.test(e1),
    `elapsed reads ${JSON.stringify(e1)} for a run classified 42 minutes ago — expected a real duration ` +
    `derived from classified_at. "0s" reads as "this just started", the opposite of the truth.`);
  const mins = parseInt((e1.match(/^(\d+)m/) || [])[1] || '0', 10);
  ok(mins >= 41 && mins <= 43,
    `elapsed reads ${JSON.stringify(e1)} but the run was classified 42 minutes ago — the clock must be ` +
    `derived from the real instant, not counted from page load`);

  // Wait for the clock to actually tick — i.e. for .wf-el's text to differ from the e1 sample — rather than
  // guessing an interval long enough to contain one tick.
  await page.waitForFunction(prev => {
    const t = document.querySelector('.wf-el')?.textContent?.trim() || '';
    return t !== '' && t !== prev;
  }, e1).catch(() => {});
  const e2 = await page.evaluate(() => document.querySelector('.wf-el')?.textContent?.trim() || '');
  ok(e2 !== '' && e2 !== '0s',
    `elapsed collapsed to ${JSON.stringify(e2)} after a tick — the ticker is not reading the same run ` +
    `source the renderer does (the original defect: it iterated the fixture array)`);

  await page.context().close();
} finally {
  await browser.close();
}

if (failures.length) {
  console.error('approval-header-never-invents FAILURES:\n - ' + failures.join('\n - '));
  process.exit(1);
}
console.log('approval-header-never-invents: OK');
