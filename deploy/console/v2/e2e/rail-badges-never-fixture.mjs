// Console e2e — NO RAIL BADGE MAY SHOW A NUMBER THE BACKEND DID NOT PRODUCE.
//
// The rail badges are the densest claims in the console: one integer per view, read at a glance, never
// clicked through. Nine of them were seeded at boot from the fixture globals and then only overwritten if a
// live setter's guard fired — and most guards correctly test "did the fetch succeed", so a degraded or
// unwired surface kept its invented number indefinitely, indistinguishable from telemetry.
//
// Measured against the LIVE bundle on 2026-07-29 by starving every API from the first request:
//   wf_live 1 · estate 7 · estatedepth 2 · signals 2 · logs 58 · knowledge 3 · dev 1 · models 2 · modules 27/31
// all rendered with nothing behind them. "Estate Depth 2" was the worst of them: the real adopted graph
// held 12 not-ok nodes, so the one number that says how much of the estate is unhealthy under-reported it
// six-fold.
//
// THE ENUMERATION IS DERIVED FROM THE ARTIFACT, NOT LISTED HERE. The test collects every [data-badge] in the
// DOM, so a badge added later is covered automatically. A hand-written list would have to be maintained by
// the same person who forgets to wire the badge — the failure mode this is for.
//
// Run: CONSOLE_BASE=http://127.0.0.1:8137 node rail-badges-never-fixture.mjs
import { chromium } from 'playwright';
const BASE = process.env.CONSOLE_BASE || 'http://127.0.0.1:8137';
const failures = [];
const ok = (c, m) => { if (!c) failures.push(m); };

// Everything empty. Any badge value that is not "—" or a zero must have been computed from THIS payload,
// and there is nothing in it to compute from.
const EMPTY = {
  sessions: [], alerts: [], counts: { total: 0, last_24h: 0 }, actions: [], nodes: [], edges: [],
  decisions: [], entries: [], items: [], rules: [], pages: [], skills: [], models: [], modules: [],
  sources: [], resolutions: [], coverage: [], lane_coverage: [], available: false, node_count: 0,
};
const HONEST = /^(—|-|0|0%|0\/0)$/;

const browser = await chromium.launch();
try {
  const page = await browser.newContext({ viewport: { width: 1600, height: 1200 } }).then(c => c.newPage());
  const errs = []; page.on('pageerror', e => errs.push(String(e)));
  await page.route('**/api/**', route => {
    const p = route.request().url().split('/api')[1].split('?')[0];
    if (p === '/v1/whoami') return route.fulfill({ json: { source: 'operator:tester', mode: 'Shadow', may_actuate: false, posture_stale: false, posture_source: 'runtime' } });
    return route.fulfill({ json: EMPTY });
  });
  await page.goto(BASE + '/index.html#command', { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('#appRoot:not([hidden])', { timeout: 20000 });
  // Most badges this file enumerates are (re)computed inside liveAdopt()'s sequential-await chain, whose
  // last statement is liveState.lastRefresh — but TWO (skills, wiki) are monkey-patched onto liveAdopt as
  // fire-and-forget calls (skLoadList()/wkLoadIndex(), see modules/skills+wiki/js.txt) that are kicked off
  // AFTER the wrapped liveAdopt() — and thus lastRefresh — already resolved, and are never awaited by it.
  // Waiting on lastRefresh alone raced those two loaders and flaked reading their boot-time fixture value
  // ("6"/"9") instead of the recomputed one. skBadge()/wkBadge() (the actual badge-paint calls) run only
  // after EVERY fetch each loader awaits resolves — wkLoadIndex has one (wkLive.idx/err), but skLoadList
  // has TWO sequential ones (list/listErr, then trials/trialsErr) and paints only after the SECOND, so
  // keying on skLive.list alone still raced skBadge() and flaked ~1 in 8 runs even after the first fix —
  // skLive.trials/trialsErr (the last pair it sets, immediately before skBadge()) is the real signal.
  await page.waitForFunction(() =>
    typeof liveState !== 'undefined' && liveState.lastRefresh != null &&
    typeof skLive !== 'undefined' && (skLive.trials !== null || skLive.trialsErr !== null) &&
    typeof wkLive !== 'undefined' && (wkLive.idx !== null || wkLive.err !== null)
  ).catch(() => {});

  const badges = await page.evaluate(() => {
    const out = {};
    document.querySelectorAll('[data-badge]').forEach(n => { out[n.dataset.badge] = n.textContent.trim(); });
    return out;
  });

  ok(Object.keys(badges).length > 0,
    'no [data-badge] elements found at all — the selector this whole oracle rests on no longer matches, so ' +
    'it would pass vacuously forever');

  for (const [key, value] of Object.entries(badges)) {
    ok(HONEST.test(value),
      `rail badge "${key}" reads "${value}" with EVERY api empty. Nothing produced that number, so it came ` +
      `from a fixture. A badge with no live source must read "—": an empty slot is a visible gap, an ` +
      `invented count is an invisible lie an operator reads as telemetry`);
  }

  ok(errs.length === 0, `uncaught page errors: ${errs.join(' | ')}`);
  await page.close();

  // ---- and the converse: a badge with real data behind it must still populate ----------------------------
  // Without this, deleting every badge-setting line would satisfy everything above — the vacuous-oracle
  // failure mode. "—" is the honest answer to no data, not a way to pass the test.
  {
    const page2 = await browser.newContext({ viewport: { width: 1600, height: 1200 } }).then(c => c.newPage());
    const alerts = [
      { host: 'dc1actualbudget01', severity: 'critical', rule: 'Service-up/down', state: 'active' },
      { host: 'dc1mealie01', severity: 'warning', rule: 'Device-Down', state: 'active' },
    ];
    await page2.route('**/api/**', route => {
      const p = route.request().url().split('/api')[1].split('?')[0];
      if (p === '/v1/whoami') return route.fulfill({ json: { source: 'operator:tester', mode: 'Shadow', may_actuate: false, posture_stale: false, posture_source: 'runtime' } });
      if (p === '/v1/alerts') return route.fulfill({ json: { alerts, counts: { total: 1717, last_24h: 12 } } });
      if (p === '/v1/estate') return route.fulfill({ json: { available: true, node_count: 217, nodes: [{ name: 'dc1actualbudget01', type: 'lxc' }, { name: 'dc1mealie01', type: 'lxc' }], edges: [] } });
      return route.fulfill({ json: EMPTY });
    });
    await page2.goto(BASE + '/index.html#command', { waitUntil: 'domcontentloaded' });
    await page2.waitForSelector('#appRoot:not([hidden])', { timeout: 20000 });
    // Same three-signal wait as the starved case above (lastRefresh covers alerts/estate/estatedepth, the
    // three badges checked below; skLive.trials/wkLive.idx cover the two fire-and-forget loaders that race
    // past it — see the long comment on the equivalent wait above for why trials, not list, is the signal).
    await page2.waitForFunction(() =>
      typeof liveState !== 'undefined' && liveState.lastRefresh != null &&
      typeof skLive !== 'undefined' && (skLive.trials !== null || skLive.trialsErr !== null) &&
      typeof wkLive !== 'undefined' && (wkLive.idx !== null || wkLive.err !== null)
    ).catch(() => {});
    const live = await page2.evaluate(() => {
      const out = {};
      document.querySelectorAll('[data-badge]').forEach(n => { out[n.dataset.badge] = n.textContent.trim(); });
      return out;
    });
    ok(live.alerts === '1717',
      `the alerts badge reads "${live.alerts}" when the API reported a total of 1717 — a live value must ` +
      `still reach the rail. Silencing every badge would pass the starvation check above and break the ` +
      `feature entirely`);
    ok(live.estate === '217',
      `the estate badge reads "${live.estate}" when the API reported node_count 217`);
    ok(live.estatedepth === '2',
      `the estate-depth badge reads "${live.estatedepth}" when both live nodes carry an active alert`);
    await page2.close();
  }
} finally { await browser.close(); }

if (failures.length) { console.error('RAIL-BADGES E2E FAIL:\n  - ' + failures.join('\n  - ')); process.exit(1); }
console.log('RAIL-BADGES E2E PASS — with every API empty no badge invents a count, and with real data behind them the badges still populate.');
