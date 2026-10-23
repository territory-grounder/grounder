// TWO PROMISES THE LIVE LAYER MADE AND DID NOT KEEP.
//
// 1. NO READ WAS BOUNDED. liveAdopt awaits liveGet 26 times in sequence with no timeout and no
//    AbortController, so ONE endpoint that was merely slow — not failing, not erroring, just hanging — stalled
//    the whole adoption behind it. The console sat on its skeleton with no error and no spinner, and an
//    operator could not distinguish "the estate is quiet" from "the console stopped reading". This is the one
//    failure the fail-closed rendering work could not catch, because a hanging read never reaches a branch.
//
// 2. THE COMMAND PALETTE'S ACCESSIBLE NAME says "search views, sessions and hosts". It searched views and the
//    FIXTURE session array (never replaced with live rows, unlike LEDGER), and indexed no host at all. On the
//    production console an operator typing a real external_ref or hostname got "no matches" — which reads as
//    "that does not exist", not "this box does not search that". For a screen-reader operator, who is told the
//    affordance exists, an unimplemented promise is worse than silence.
import { chromium } from 'playwright';

const BASE = process.env.CONSOLE_BASE || `http://127.0.0.1:${process.env.CONSOLE_E2E_PORT || '8137'}`;
let failed = 0;
const check = (name, ok, detail) => {
  console.log(`  ${ok ? 'ok  ' : 'FAIL'} ${name}${ok ? '' : ' — ' + detail}`);
  if (!ok) failed++;
};

const SESSIONS = Array.from({ length: 7 }, (_, i) => ({
  external_ref: `librenms-dc1-90${i}`, band: i % 2 ? 'AUTO' : 'POLL_PAUSE', verdict: 'match',
  host: `dc1probe${i}`, action_id: `a${i}`, op_class: 'restart-service',
  classified_at: new Date(Date.now() - i * 60000).toISOString(),
}));
const ESTATE = {
  available: true, node_count: 5, edge_count: 0, source_count: 1, captured_at: '2026-07-30T00:00:00Z',
  // a duplicated name, because the graph carries one node per (name,type) and the operator wants ONE hit
  nodes: [{ name: 'dc1uniquehost01', health: 'ok' }, { name: 'dc1uniquehost01', health: 'ok' },
          { name: 'dc1sickhost02', health: 'crit' }, { name: 'dc1other03', health: 'ok' }],
  edges: [],
};

const browser = await chromium.launch();
try {
  const page = await (await browser.newContext({ viewport: { width: 1500, height: 1000 } })).newPage();
  await page.route('**/v1/**', async rt => {
    const u = rt.request().url();
    const j = b => rt.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(b) });
    if (u.includes('/v1/whoami')) return j({ source: 'operator:test', mode: 'Semi-auto', may_actuate: true, posture_stale: false, posture_source: 'test' });
    if (u.includes('/v1/events')) return rt.abort();
    if (u.includes('/v1/sessions')) return j({ total: SESSIONS.length, sessions: SESSIONS });
    if (u.includes('/v1/estate')) return j(ESTATE);
    return j({});
  });
  await page.goto(`${BASE}/index.html`, { waitUntil: 'domcontentloaded' });
  await page.evaluate(() => { if (typeof setGate === 'function') { setGate(false); const r = document.querySelector('#appRoot'); if (r) r.hidden = false; } });
  await page.evaluate(async () => { try { await liveAdopt(); } catch (e) {} });
  // liveAdopt() is already fully awaited above (sessions/estate — the two sources PAL_ITEMS() reads via
  // liveState/HOSTS — are read in its sequential in-chain, so both are already settled) — one frame is
  // enough margin before the reads/DOM calls below, not a guess at fetch latency.
  await page.evaluate(() => new Promise(r => requestAnimationFrame(() => r())));

  // ---- 1. THE READ BUDGET EXISTS AND IS BOUNDED ----
  const budget = await page.evaluate(() => ({
    declared: typeof LIVE_READ_TIMEOUT_MS !== 'undefined' ? LIVE_READ_TIMEOUT_MS : null,
    usesAbort: /AbortController/.test(String(liveGet)),
    usesSignal: /signal/.test(String(liveGet)),
  }));
  check('a read timeout is declared', typeof budget.declared === 'number' && budget.declared > 0, String(budget.declared));
  check('the budget is bounded, not a decorative large number', budget.declared <= 30000,
    `${budget.declared}ms — a budget longer than an operator will wait is not a budget`);
  check('liveGet uses an AbortController', budget.usesAbort === true, 'without abort the socket stays parked even after the timeout');
  check('and passes its signal to fetch', budget.usesSignal === true, 'an AbortController that is never wired aborts nothing');

  // ---- THE CONSEQUENCE: A HANGING READ REJECTS INSTEAD OF STALLING ----
  // Drive the real liveGet against an endpoint that never responds. Before the fix this promise never settles.
  await page.route('**/v1/hangforever', async () => { await new Promise(() => {}); });
  // ★ THE ORACLE'S OWN WAIT IS BOUNDED TOO. Proving this control red made the check HANG — which is exactly
  // the operator's experience, and exactly how a regression here would wedge CI for its whole job timeout
  // instead of reporting a failure. An oracle that hangs on the defect it guards is not a guard.
  // Promise.race turns "never settles" into a reported FAILURE with the right diagnosis.
  const hang = await page.evaluate(async () => {
    const t0 = Date.now();
    const attempt = liveGet('/v1/hangforever', { timeoutMs: 900 })
      .then(() => ({ settled: true, rejected: false, ms: Date.now() - t0 }))
      .catch(e => ({ settled: true, rejected: true, ms: Date.now() - t0, timeout: !!e.timeout, msg: String(e.message || '') }));
    const ceiling = new Promise(res => setTimeout(() => res({ settled: false, rejected: false, ms: Date.now() - t0 }), 4000));
    return Promise.race([attempt, ceiling]);
  });
  check('the read SETTLES at all (an unbounded read hangs here, and would wedge this job)', hang.settled === true,
    `still pending after ${hang.ms}ms — liveGet has no timeout, so every awaited read behind it is stalled too`);
  check('a hanging read REJECTS rather than hanging forever', hang.rejected === true,
    'the promise never settled — every awaited read behind it is stalled with it');
  check('it rejects within its budget', hang.ms < 5000, `${hang.ms}ms for a 900ms budget`);
  check('the rejection is marked as a TIMEOUT, distinguishable from a server error', hang.timeout === true,
    `${JSON.stringify(hang)} — "we do not know" must never be reported as "the endpoint said no"`);

  // A NORMAL read must still succeed — otherwise the "fix" is just breaking every fetch.
  const okRead = await page.evaluate(async () => {
    try { const r = await liveGet('/v1/sessions'); return { ok: true, n: (r.sessions || []).length }; }
    catch (e) { return { ok: false, err: String(e.message || '') }; }
  });
  check('an ordinary read still succeeds', okRead.ok === true && okRead.n === SESSIONS.length, JSON.stringify(okRead));

  // ---- 2. THE PALETTE INDEXES WHAT ITS NAME PROMISES ----
  const pal = await page.evaluate(() => {
    const items = PAL_ITEMS();
    const byType = {};
    items.forEach(i => { byType[i.type] = (byType[i.type] || 0) + 1; });
    return {
      byType,
      sessionIds: items.filter(i => i.type === 'session').map(i => i.id),
      hostIds: items.filter(i => i.type === 'host').map(i => i.id),
      label: (document.querySelector('#palOpen') || {}).getAttribute?.call(document.querySelector('#palOpen'), 'aria-label') || '',
    };
  });
  check('the palette still indexes views', (pal.byType.view || 0) > 5, JSON.stringify(pal.byType));
  check('the palette indexes the LIVE sessions, not the fixture', (pal.byType.session || 0) === SESSIONS.length,
    `${pal.byType.session} sessions indexed for ${SESSIONS.length} live rows: ${JSON.stringify(pal.sessionIds.slice(0, 3))}`);
  check('a real external_ref is findable', pal.sessionIds.includes(SESSIONS[0].external_ref),
    `${SESSIONS[0].external_ref} not in ${JSON.stringify(pal.sessionIds.slice(0, 4))}`);
  check('the palette indexes estate HOSTS (the third promise in its own name)', (pal.byType.host || 0) > 0,
    `no host indexed, while the accessible name reads "${pal.label}"`);
  check('host entries are de-duplicated by name', (pal.byType.host || 0) === 3,
    `${pal.byType.host} host entries for 4 graph nodes with one duplicated name — an operator wants one hit`);
  check('a real hostname is findable', pal.hostIds.includes('dc1sickhost02'), JSON.stringify(pal.hostIds));

  // AND THE SEARCH ITSELF must return them — an index nothing queries is the same defect one layer down.
  const search = await page.evaluate(() => {
    openPal();
    const inp = document.querySelector('#palInput');
    inp.value = 'sickhost02';
    inp.dispatchEvent(new Event('input', { bubbles: true }));
    const rows = Array.from(document.querySelectorAll('#palScrim .palrow, #palScrim [role=option], #palScrim .pal-item'));
    const txt = (document.querySelector('#palScrim') || {}).innerText || '';
    closePal();
    return { rowCount: rows.length, mentions: /sickhost02/i.test(txt) };
  });
  check('typing a real hostname surfaces it in the palette', search.mentions === true,
    `${JSON.stringify(search)} — the index is populated but the query does not reach it`);
} finally { await browser.close(); }

console.log(failed ? `live-layer-contract: ${failed} FAILED` : 'live-layer-contract: all checks passed');
process.exit(failed ? 1 : 0);
