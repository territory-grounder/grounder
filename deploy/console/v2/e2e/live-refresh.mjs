// THE CONSOLE MUST RE-READ, AND MUST NEVER HIDE THAT IT HAS NOT.
//
// Measured live across two independent sessions: 29 requests to /api/ during login, then ZERO — across a
// 90-second stationary window AND across #alerts -> #command -> #alerts -> #governance -> #logs. Leaving a
// view and returning forced no refetch (reqsDuringRenav: 0). The SSE stream opened once and carried POSTURE
// events only. Meanwhile the header's chain-seq advanced live (6004 -> 6007 while the page sat open) and the
// connection chip read "SSE LIVE" at all 10 samples.
//
// So the single indicator an operator trusts was actively reassuring them the data was current while every
// list below it was a snapshot frozen at login. During an incident that is the worst failure a console can
// have: it looks alive and shows the past. The verifier tried to kill this four ways and it survived all four.
//
// This oracle asserts BEHAVIOUR, not the presence of a timer: it counts real network requests over a window,
// and it drives the three guards that must suppress a refresh. A test that only checked "setInterval was
// called" would pass on a loop that throws on its first tick.
import { chromium } from 'playwright';

const BASE = process.env.CONSOLE_BASE || `http://127.0.0.1:${process.env.CONSOLE_E2E_PORT || '8137'}`;
let failed = 0;
const check = (name, ok, detail) => {
  console.log(`  ${ok ? 'ok  ' : 'FAIL'} ${name}${ok ? '' : ' — ' + detail}`);
  if (!ok) failed++;
};
const WHOAMI = { source: 'operator:test', mode: 'Semi-auto', may_actuate: true, posture_stale: false, posture_source: 'test' };

const browser = await chromium.launch();
try {
  const ctx = await browser.newContext();
  const page = await ctx.newPage();
  const hits = [];
  await page.route('**/v1/**', async r => {
    const u = r.request().url();
    hits.push({ t: Date.now(), u });
    if (u.includes('/v1/whoami')) return r.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(WHOAMI) });
    if (u.includes('/v1/events')) return r.abort();                       // no SSE in the harness
    return r.fulfill({ status: 200, contentType: 'application/json', body: '{}' });
  });
  await page.goto(`${BASE}/index.html`, { waitUntil: 'domcontentloaded' });
  await page.evaluate(() => { // Reveal the console the way the APP does. Poking #appRoot.hidden used to be enough; since the auth
    // gate became a real modal it also inerts #appRoot, so a bare hidden=false leaves a console that is
    // visible and cannot take focus — which is the correct production behaviour for an unauthenticated
    // page, and the reason this line must go through setGate rather than reach past it.
    if (typeof setGate === 'function') { setGate(false); const r = document.querySelector('#appRoot'); if (r) r.hidden = false; }
    else { const r = document.querySelector('#appRoot'); if (r) r.hidden = false; } });

  const api = await page.evaluate(() => ({
    adopt: typeof liveAdopt === 'function', refresh: typeof liveRefresh === 'function',
    fresh: typeof liveFreshness === 'function', ms: typeof LIVE_REFRESH_MS === 'number' ? LIVE_REFRESH_MS : null }));
  check('the refresh machinery exists', api.adopt && api.refresh && api.fresh, JSON.stringify(api));
  check('the interval is a sane operator cadence (10-60s)', api.ms >= 10000 && api.ms <= 60000, `${api.ms}ms`);

  // Boot, then count requests in a window with NO further interaction. liveAdopt() is already fully
  // awaited above (every fetch inside it is sequential and awaited), so afterBoot is already the real
  // count by the time we reach this line — the frame flush is margin for hits.length bookkeeping, not a
  // guess at fetch latency (measured: afterBoot lands around 50+, far clear of the >5 floor below).
  await page.evaluate(async () => { try { await liveAdopt(); } catch (e) {} });
  await page.evaluate(() => new Promise(r => requestAnimationFrame(() => r())));
  const afterBoot = hits.length;
  check('boot actually reads the control plane', afterBoot > 5, `${afterBoot} requests during boot`);

  // Drive the loop directly rather than waiting 25s of wall-clock: the point is that a refresh REFETCHES.
  // Same reasoning as afterBoot above — liveRefresh() is already fully awaited.
  const before = hits.length;
  await page.evaluate(async () => { await liveRefresh(); });
  await page.evaluate(() => new Promise(r => requestAnimationFrame(() => r())));
  const refetched = hits.length - before;
  check('a refresh RE-READS the lists (the defect was 0 requests, forever)', refetched > 5,
        `${refetched} requests on refresh — a frozen console makes zero`);

  // DRAIN THE EAGER-LIVE-LOAD WIRING before any Class-3 negative proof takes its baseline. modules/
  // skills/wiring.txt and modules/wiki/wiring.txt each wrap liveAdopt() to kick off skLoadList()/
  // wkLoadIndex() the moment a session is live — FIRE-AND-FORGET (not awaited by the wrapper that calls
  // them), so the `await liveRefresh()` above can resolve while either request is still in flight. Left
  // undrained, that leftover from THIS visible refresh can land inside a hidden-tab/overlay window below
  // and get miscounted as "the background tab polled" — measured ~1-in-4 on CI. It is not a console
  // suppression gap: liveRefresh()'s three guards (visibility, in-flight, drawer/scrim/cfgElevate) all
  // return BEFORE it ever calls liveAdopt(), so a genuinely suppressed refresh never reaches these loaders
  // in the first place — only a PRECEDING unsuppressed refresh's tail can still be running. Wait for each
  // loader's own in-flight flag (the same guard skLoadList()/wkLoadIndex() use to no-op a re-entrant call)
  // to clear, so beforeHidden/beforeOverlay below are captured after the tail has actually landed.
  await page.waitForFunction(() => skLive.loading === false && wkLive.loading === false);

  // ---- the three guards, each driven for real ----
  const g1 = await page.evaluate(async () => {
    Object.defineProperty(document, 'visibilityState', { get: () => 'hidden', configurable: true });
    const n = window.__hits = 0; await liveRefresh(); return n;
  });
  const beforeHidden = hits.length;
  await page.waitForTimeout(300); // Class-3 measurement window: intentional fixed wait, MUST NOT become a
  // condition-wait (proves a hidden tab issues ZERO requests over a real window — there is no DOM event
  // for "nothing happened" to wait on instead)
  check('a HIDDEN tab does not poll the control plane', hits.length === beforeHidden,
        `${hits.length - beforeHidden} requests fired from a background tab`);
  await page.evaluate(() => { Object.defineProperty(document, 'visibilityState', { get: () => 'visible', configurable: true }); });

  const beforeOverlay = hits.length;
  await page.evaluate(async () => {
    const d = document.querySelector('#drawer'); d.classList.add('open');
    await liveRefresh();
    d.classList.remove('open');
  });
  await page.waitForTimeout(300); // Class-3 measurement window: intentional fixed wait, MUST NOT become a
  // condition-wait (proves an open overlay suppresses the refresh — ZERO requests over a real window,
  // with no DOM event for "nothing happened" to wait on instead)
  check('an OPEN overlay suppresses the refresh (re-rendering yanks focus out of a dialog)',
        hits.length === beforeOverlay, `${hits.length - beforeOverlay} requests fired under an open drawer`);

  // ---- freshness must be visible, and must go loud when stale ----
  const fresh = await page.evaluate(() => {
    const el = document.querySelector('#liveFresh');
    if (!el) return { missing: true };
    liveState.lastRefresh = Date.now(); liveState.refreshFailed = false; liveFreshness();
    const ok = { text: el.textContent, color: el.style.color };
    liveState.lastRefresh = Date.now() - (LIVE_REFRESH_MS * 3); liveFreshness();
    const old = { text: el.textContent, color: el.style.color };
    liveState.lastRefresh = Date.now(); liveState.refreshFailed = true; liveFreshness();
    const failedS = { text: el.textContent, color: el.style.color };
    return { ok, old, failedS };
  });
  check('the data AGE is shown, not just the stream state', !fresh.missing && /updated/.test(fresh.ok.text), JSON.stringify(fresh.ok));
  check('data older than two intervals is called OLD, in the deviation colour',
        !fresh.missing && /old/.test(fresh.old.text) && /deviation/.test(fresh.old.color), JSON.stringify(fresh.old));
  check('a FAILED refresh is surfaced even when the age is small',
        !fresh.missing && /old/.test(fresh.failedS.text), JSON.stringify(fresh.failedS));

  // ---- a refresh must not evict a working session on one transient whoami failure ----
  const evict = await page.evaluate(async () => {
    const gate = document.querySelector('#authGate');
    const before = gate ? gate.hidden : null;
    window.__forceWhoamiFail = true;
    return { gateBefore: before };
  });
  await page.route('**/v1/whoami', r => r.fulfill({ status: 500, body: '{}' }));
  await page.evaluate(async () => { await liveRefresh(); });
  // liveRefresh() already resolved above, and its own finally{} block synchronously sets refreshFailed and
  // calls liveFreshness() before that promise settles — the REFRESH-mode failure branch never calls
  // liveLoginOverlay(), so there is no further async re-gating to wait out. One frame of margin only.
  await page.evaluate(() => new Promise(r => requestAnimationFrame(() => r())));
  const after = await page.evaluate(() => ({
    gateHidden: (document.querySelector('#authGate') || {}).hidden,
    appHidden: (document.querySelector('#appRoot') || {}).hidden,
    failedFlag: liveState.refreshFailed }));
  check('a transient refresh failure does NOT evict the session', after.appHidden !== true, JSON.stringify(after));
  check('but it IS recorded as a failure, not swallowed', after.failedFlag === true, JSON.stringify(after));

  await ctx.close();
} finally { await browser.close(); }

if (failed) { console.log(`\nlive-refresh: ${failed} FAILED`); process.exit(1); }
console.log('\nlive-refresh: all checks passed');
