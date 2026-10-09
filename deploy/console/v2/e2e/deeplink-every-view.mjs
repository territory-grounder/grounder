// Console e2e — DEEP-LINK BOOT OVER THE CLOSED ENUMERATION OF VIEWS.
//
// WHY THIS EXISTS. Two oracles already guarded the deep-link boot race — deeplink.mjs and
// boot-resilience.mjs — and both were GREEN while production was broken, because each tests exactly ONE
// hand-picked view (#workflows and #estatedepth). #alerts was the one view no oracle ever landed on, and it
// was the one that threw. Measured live on 2026-07-28 against the real console with the owner's session:
// 21 of 22 views deep-linked to 27 API calls and a rendered page; /#alerts made 1 API call and rendered 0
// characters.
//
// The failure was not confined to the view. views.alerts first renders MID-BOOT from revealConsole(), where
// liveState.on is already true but no fetch has resolved, so liveState.alerts was still `undefined`; the
// renderer handled null and array but not undefined, and `!a.length` threw. Boot is
// `liveAdopt().catch(()=>{})`, so that rejection was swallowed whole and every later fetch — actions,
// alerts, estate, ledger, governance — never ran. The console then showed FIXTURE counts and a
// "MUTATION OFF · READ-ONLY" breaker for a control plane that actuates. A blank panel would have been the
// harmless version of this bug.
//
// So the enumeration is DERIVED FROM THE RAIL, never hand-listed: whatever views the console ships, this
// oracle lands on all of them. Adding a view adds coverage automatically, which is the property the two
// single-view oracles lacked.
import { chromium } from 'playwright';
const BASE = process.env.CONSOLE_BASE || 'http://127.0.0.1:8099';
const failures = [];
const ok = (c, m) => { if (!c) failures.push(m); };

const WHOAMI = { source: 'operator:tester', mode: 'Semi-auto', may_actuate: true, posture_stale: false, posture_source: 'runtime', role: 'trace-read' };
const alerts = Array.from({ length: 50 }, (_, i) => ({
  external_ref: `librenms-dc1-${180800 + i}`, source_type: 'librenms', source_id: 'lnms',
  alert_rule: 'Device-Down', severity: 'critical', host: 'dc1mealie01',
  received_at: '2026-07-28T20:00:00Z', observed_at: '2026-07-28T20:00:00Z',
}));

// whoamiDelayMs lets a test act on the page after boot has started but BEFORE the first render.
function mock(page, { whoamiDelayMs = 0 } = {}) {
  return page.route('**/api/**', async route => {
    const p = route.request().url().split('/api')[1].split('?')[0];
    if (p === '/v1/whoami') {
      if (whoamiDelayMs) await new Promise(r => setTimeout(r, whoamiDelayMs));
      return route.fulfill({ json: WHOAMI });
    }
    if (p === '/v1/alerts') return route.fulfill({ json: { alerts, counts: { total: 1586, last_24h: 553 } } });
    if (p === '/v1/estate') return route.fulfill({ json: { available: true, node_count: 1, nodes: [{ name: 'dc1mealie01', type: 'lxc' }], edges: [] } });
    if (p === '/v1/sessions') return route.fulfill({ json: { sessions: [], total: 0 } });
    if (p === '/v1/actions') return route.fulfill({ json: { actions: [], counts: { total: 0, verified: 0, deviations: 0 } } });
    if (p.endsWith('/stream') || p === '/v1/events') return route.fulfill({ status: 200, headers: { 'Content-Type': 'text/event-stream' }, body: '' });
    return route.fulfill({ json: {} });
  });
}

const browser = await chromium.launch();
try {
  const ctx = await browser.newContext({ viewport: { width: 1440, height: 900 } });

  // The enumeration itself, read off the shipped rail rather than written down here.
  const probe = await ctx.newPage();
  await mock(probe);
  await probe.goto(BASE + '/index.html', { waitUntil: 'domcontentloaded' });
  await probe.waitForSelector('#appRoot:not([hidden])', { timeout: 20000 });
  const views = await probe.evaluate(() => [...document.querySelectorAll('.navi')].map(n => n.dataset.view).filter(Boolean));
  await probe.close();
  ok(views.length >= 15, `only ${views.length} views found in the rail — the enumeration is not being read correctly, so a green run would prove nothing`);

  for (const key of views) {
    const page = await ctx.newPage();
    const pageErrors = []; page.on('pageerror', e => pageErrors.push(String(e).slice(0, 120)));
    // The safety net in revealConsole() logs here. A clean boot must never need it: if this fires, a view
    // threw during its mid-boot render and only the net kept the console alive.
    const netCatches = [];
    page.on('console', m => { if (/boot render error/i.test(m.text())) netCatches.push(key + ': ' + m.text().slice(0, 140)); });
    const apis = new Set();
    page.on('request', r => { const u = r.url(); if (u.includes('/api/v1/')) apis.add(u.split('/api/v1/')[1].split('?')[0]); });

    await mock(page);
    await page.goto(`${BASE}/index.html#${key}`, { waitUntil: 'domcontentloaded' });
    // liveState.lastRefresh is set exactly once, unconditionally, as the LAST statement of liveAdopt() —
    // after every fetch, the estate-depth/badge work, and the post-adopt route() re-render. It is the one
    // signal that is meaningful across EVERY view in this loop (unlike a per-view DOM element), and if a
    // regression aborts liveAdopt() early (the exact production bug this file exists to catch) it never
    // gets set, so this falls through to the timeout and the checks below read — and correctly fail on —
    // the genuinely stuck state, rather than a guess at how long boot takes.
    await page.waitForFunction(() => typeof liveState !== 'undefined' && liveState.lastRefresh != null).catch(() => {});

    const st = await page.evaluate(() => ({
      chars: (document.querySelector('#view')?.innerText || '').length,
      gated: !!document.querySelector('#appRoot')?.hidden,
    }));

    // (a) boot must get PAST whoami. One API call means liveAdopt died on the first render — the exact
    // production signature. This is the check that would have caught the live bug.
    ok(apis.size > 1, `#${key}: boot made ${apis.size} API call(s) — liveAdopt died before fetching data, so ` +
      `every panel falls back to fixtures and the mutation breaker shows a posture nothing verified`);
    ok(!st.gated, `#${key}: the console never revealed — deep-linking here leaves the operator at the gate`);
    ok(st.chars > 0, `#${key}: rendered 0 characters — the operator deep-linked to a blank page`);
    ok(netCatches.length === 0, `#${key}: a view threw during its mid-boot render and only the revealConsole ` +
      `safety net kept boot alive: ${netCatches.join(' | ')}. The net is a backstop, not a licence to throw — ` +
      `fix the view's not-yet-loaded state`);
    ok(pageErrors.length === 0, `#${key}: uncaught JS error — ${pageErrors.join(' | ')}`);
    await page.close();
  }

  // ---------------------------------------------------------------------------------------------------
  // The safety net itself, controlled INDEPENDENTLY of any view's correctness.
  //
  // The checks above cannot prove the net works: once every view handles its loading state, nothing throws,
  // so deleting the try/catch in revealConsole() leaves them all green. This makes a view throw on purpose —
  // patched in during the whoami delay, i.e. after boot starts and before the first render — and asserts the
  // console still finishes booting. Delete the try/catch and this fails; it is the only check that can say so.
  // ---------------------------------------------------------------------------------------------------
  const page = await ctx.newPage();
  const apis = new Set();
  page.on('request', r => { const u = r.url(); if (u.includes('/api/v1/')) apis.add(u.split('/api/v1/')[1].split('?')[0]); });
  await mock(page, { whoamiDelayMs: 800 });
  await page.goto(BASE + '/index.html#alerts', { waitUntil: 'domcontentloaded' });
  // Wait for the boot script to have parsed and populated `views` — not a delay guess — so the patch below
  // lands as early as possible, well inside the (mocked) 800ms whoami delay and before boot's first render.
  await page.waitForFunction(() => typeof views === 'object' && Object.keys(views).length > 5).catch(() => {});
  const patched = await page.evaluate(() => {
    if (typeof views === 'undefined' || !views.alerts) return false;
    views.alerts = () => { throw new TypeError('injected mid-boot render failure'); };
    return true;
  });
  ok(patched, 'could not inject a throwing view — the control proves nothing, so treat it as failed rather ' +
    'than as evidence the net works');
  // Same completion signal as the main loop above: liveState.lastRefresh is the last statement of
  // liveAdopt(), so it is set here ONLY if the mid-boot throw (revealConsole()'s route() call, now patched
  // to throw) was actually caught and boot ran to completion — the exact property "the net still works"
  // means. If the safety net were removed, the throw would abort liveAdopt() before this line ever runs and
  // the wait times out, letting the checks below read (and fail on) the real stuck state.
  await page.waitForFunction(() => typeof liveState !== 'undefined' && liveState.lastRefresh != null).catch(() => {});
  const after = await page.evaluate(() => ({
    revealed: !!(document.querySelector('#appRoot') && !document.querySelector('#appRoot').hidden),
    navLinks: document.querySelectorAll('.navi').length,
  }));
  ok(after.revealed, 'a throwing view during the mid-boot render left the console unrevealed');
  ok(after.navLinks >= 15, 'a throwing view during the mid-boot render destroyed the rail');
  ok(apis.size > 1, `a throwing view during the mid-boot render killed the data load (${apis.size} API call(s)) — ` +
    `revealConsole() is not catching it, so any future view can silently disable the whole console`);
  await page.close();
} finally { await browser.close(); }

if (failures.length) { console.error('DEEPLINK-EVERY-VIEW E2E FAIL:\n  - ' + failures.join('\n  - ')); process.exit(1); }
console.log('DEEPLINK-EVERY-VIEW E2E PASS — every view in the rail deep-link-boots to a rendered page with a live data load, no view needs the boot safety net, and the net still catches an injected mid-boot throw.');
