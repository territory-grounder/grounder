// A FILTER MUST NOT OVERSTATE WHAT IT SEARCHED, AND AN AUDIT LIST MUST NOT FREEZE.
//
// Three defects from the final falsification pass, all on surfaces an operator uses to decide something:
//
// 1. #actions' caption quoted the POPULATION's deviation count (113 from /v1/actions counts) while the
//    Deviations facet filtered only the ~50 rows fetched. Selecting it showed a handful under a heading
//    asserting 113. Same family as the alerts badge that displayed its fetch cap as a count.
// 2. The "Floored" facet filtered `s.floored`, which the live mapper hard-codes to false for every row
//    because the server DTO has no such field. It could never match, and an empty result reads as "the
//    never-auto floor never fired" — about the safety control this system leans on hardest.
// 3. liveWfRuns() cached the governed-run list on the FIRST successful /v1/sessions read and never
//    invalidated it, so #workflows and the decision tracer were frozen at boot: new runs never appeared and
//    departed runs stayed selectable. On the surface whose purpose is auditing what the system did.
import { chromium } from 'playwright';

const BASE = process.env.CONSOLE_BASE || `http://127.0.0.1:${process.env.CONSOLE_E2E_PORT || '8137'}`;
let failed = 0;
const check = (name, ok, detail) => {
  console.log(`  ${ok ? 'ok  ' : 'FAIL'} ${name}${ok ? '' : ' — ' + detail}`);
  if (!ok) failed++;
};

// A fetched page much SMALLER than the population, with few deviations on the page and many in the
// population — the exact shape that made the caption lie.
const PAGE = Array.from({ length: 8 }, (_, i) => ({
  action_id: `act${i}`, op: 'restart-service', op_class: 'restart-service', target: `host${i}`,
  band: i % 3 === 0 ? 'POLL_PAUSE' : 'AUTO', verdict: i === 1 ? 'deviation' : 'match',
  reversible: true, classified: true, predicted: true, approved: true, executed: true,
  verified: i !== 1, has_confidence: false, sealed_at: '2026-07-29T20:00:00Z',
}));
const COUNTS = { total: 1240, verified: 1100, deviations: 113 };

let sessionsGen = 0;
const sessionRows = n => ({
  total: n,
  sessions: Array.from({ length: n }, (_, i) => ({
    external_ref: `run-${i}`, band: 'AUTO', verdict: 'match', risk_level: 'low',
    action_id: `a${i}`, op_class: 'restart-service', classified_at: '2026-07-29T20:00:00Z',
  })),
});

const browser = await chromium.launch();
try {
  const page = await (await browser.newContext({ viewport: { width: 1500, height: 1000 } })).newPage();
  await page.route('**/v1/**', async rt => {
    const u = rt.request().url();
    const j = b => rt.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(b) });
    if (u.includes('/v1/whoami')) return j({ source: 'operator:test', mode: 'Semi-auto', may_actuate: true, posture_stale: false, posture_source: 'test' });
    if (u.includes('/v1/events')) return rt.abort();
    if (u.includes('/v1/actions')) return j({ actions: PAGE, counts: COUNTS });
    // Each adopt sees MORE runs than the last — the estate moving under the cache.
    if (u.includes('/v1/sessions')) return j(sessionRows(3 + sessionsGen * 2));
    return j({});
  });
  await page.goto(`${BASE}/index.html`, { waitUntil: 'domcontentloaded' });
  await page.evaluate(() => { if (typeof setGate === 'function') { setGate(false); const r = document.querySelector('#appRoot'); if (r) r.hidden = false; } });
  await page.evaluate(async () => { try { await liveAdopt(); } catch (e) {} });
  // liveAdopt() is already fully awaited above — every in-chain fetch and the post-adopt re-render have
  // resolved by the time page.evaluate() returns — so this is margin for the DOM to settle, not a guess at
  // fetch latency.
  await page.evaluate(() => new Promise(r => requestAnimationFrame(() => r())));

  // ---- 1. THE FILTER'S SCOPE IS STATED HONESTLY ----
  const unfiltered = await page.evaluate(() => {
    facetState.actions = 'all'; route('actions');
    return { caption: (document.querySelector('#view .sec') || {}).innerText || '', rows: document.querySelectorAll('#view .ribbon-row, #view .rr-top').length };
  });
  check('unfiltered caption reports the population', /1240/.test(unfiltered.caption), JSON.stringify(unfiltered.caption.slice(0, 160)));

  const filtered = await page.evaluate(() => {
    facetState.actions = 'deviation'; route('actions');
    const caption = (document.querySelector('#view .sec') || {}).innerText || '';
    return { caption, shownRows: document.querySelectorAll('#view .rr-top').length };
  });
  // The load-bearing property: with a filter active, the caption must NOT present the population's deviation
  // count as the thing that was filtered. It must say what page it searched.
  check('a filtered caption says it searched only the fetched page',
    /fetched/i.test(filtered.caption) && /page only|this page/i.test(filtered.caption),
    JSON.stringify(filtered.caption.slice(0, 220)));
  check('a filtered caption still reports the population separately', /population/i.test(filtered.caption) && /1240/.test(filtered.caption),
    'the population is the more important number and must not be dropped in the name of honesty');
  check('the filtered row count matches what the page actually holds', filtered.shownRows === 1,
    `${filtered.shownRows} rows shown; the fetched page holds exactly 1 deviation`);

  // An empty filtered result must not read as a statement about the estate.
  const empty = await page.evaluate(() => {
    facetState.actions = 'match';
    // force a page with no matches at all
    liveState.actions = liveState.actions.map(a => Object.assign({}, a, { verdict: 'deviation' }));
    route('actions');
    return (document.querySelector('#view .empty') || {}).innerText || '';
  });
  check('an empty filtered result says it is about the PAGE, not the estate', /page/i.test(empty) && /1240/.test(empty),
    JSON.stringify(empty.slice(0, 200)));

  // ---- 2. THE UNFALSIFIABLE FACET IS NOT OFFERED ----
  const facets = await page.evaluate(() => (FACETS.actions || []).map(f => f[0]));
  check('no "floor" facet is offered on actions', facets.includes('floor') === false,
    `FACETS.actions = ${JSON.stringify(facets)} — it filters a field the live DTO never populates, so it can only ever answer "none"`);
  const mapperConstant = await page.evaluate(() => {
    // If a future change re-adds the facet, this states the precondition it must satisfy first.
    const rows = liveState.actions || [];
    return { anyFloored: rows.some(r => r.floored === true), allPresent: rows.every(r => 'floored' in r) };
  });
  check('and no live row claims to be floored while the DTO cannot say so', mapperConstant.anyFloored === false,
    'a client-side default is being presented as a policy fact');

  // ---- 3. THE GOVERNED-RUN LIST TRACKS THE ESTATE, KEEPING IDENTITY ----
  const first = await page.evaluate(() => {
    const runs = liveWfRuns();
    return { n: runs.length, refs: runs.map(r => r._row.external_ref) };
  });
  check('the run list is populated', first.n === 3, JSON.stringify(first));

  const second = await page.evaluate(async () => {
    const before = liveWfRuns();
    const keptRef = before[0]._row.external_ref;
    const keptObj = before[0];
    // The estate moves. liveAdopt()'s only job here is to refresh liveState.sessions, so the honest way to
    // drive the poll path is to write what a later adopt would have written — the route interceptor lives in
    // Node and cannot be re-programmed from inside the page.
    liveState.sessions = Array.from({ length: 5 }, (_, i) => ({
      external_ref: `run-${i}`, band: 'AUTO', verdict: 'match', risk_level: 'low',
      action_id: `a${i}`, op_class: 'restart-service', classified_at: '2026-07-29T20:00:00Z',
    }));
    const after = liveWfRuns();
    return {
      n: after.length,
      grew: after.length > before.length,
      identityKept: after.some(r => r === keptObj && r._row.external_ref === keptRef),
      refs: after.map(r => r._row.external_ref),
    };
  });
  check('the run list GROWS when the estate has more runs', second.grew === true,
    `${first.n} -> ${second.n}: the list was cached on first read and never invalidated`);
  check('and a run that persists keeps its object identity', second.identityKept === true,
    'rebuilding blindly would collapse an open decision-tracer walk on every poll');

  // The other direction: a shrinking estate must drop departed runs rather than keep offering them.
  const third = await page.evaluate(async () => {
    liveState.sessions = [{ external_ref: 'run-0', band: 'AUTO', verdict: 'match', action_id: 'a0', classified_at: '2026-07-29T20:00:00Z' }];
    const runs = liveWfRuns();
    return { n: runs.length, refs: runs.map(r => r._row.external_ref) };
  });
  check('a departed run is no longer offered', third.n === 1 && third.refs[0] === 'run-0',
    `${JSON.stringify(third)} — a stale run stays selectable in the decision tracer`);
} finally { await browser.close(); }

console.log(failed ? `filter-scope-and-list-freshness: ${failed} FAILED` : 'filter-scope-and-list-freshness: all checks passed');
process.exit(failed ? 1 : 0);
