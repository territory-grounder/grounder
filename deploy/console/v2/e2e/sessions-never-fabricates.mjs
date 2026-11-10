// A LIVE CONSOLE MUST NEVER OFFER A FABRICATED SESSION.
//
// `SESSIONS` (index.html ~2667) is the design-fixture array: five invented sessions with `s-<hex>` ids,
// estate-shaped-but-fake `*demo-*` hostnames (dc1demo-w3, dc2demo-nas01, ...) and a full invented
// reasoning chain (predicted:/observed:/diagnosis:/proposal:). Unlike LEDGER — which liveAdopt replaces in
// place — nothing ever writes live rows into SESSIONS; consumers pick live-vs-fixture at render time. The
// command palette (PAL_ITEMS, ~3684) did that pick wrong: it indexed the live rows only when
// `liveState.sessions` was a NON-EMPTY array, and fell through to the fixture otherwise — so on a LIVE
// console whose `/v1/sessions` read FAILED (sessions=null) or returned EMPTY, the palette offered the five
// fabricated sessions, and clicking one opened openSession() → a drawer of invented id/host/action/reasoning
// presented exactly like a real one. That is the TG-366/TG-401 hazard (fabricated estate-shaped data on the
// surface an operator uses to judge TG), and the owner ruling on TG-439 (TG-488 B3) is: the fixtures STAY as
// design-preview fuel, but they must be PROVEN unable to reach a live view.
//
// The fix gates the fixture fallback on `!live`: the fixture is indexed ONLY for the unauthenticated design
// preview (the console's own stated intent — "the fixture remains for the unauthenticated design preview").
// A live console with no sessions honestly offers no session rows rather than inventing five.
//
// This oracle drives the REAL liveAdopt() against intercepted /v1/sessions (failed / empty / real), reads
// the palette's own PAL_ITEMS() and the rendered #palRes, and asserts no fixture id / *demo-* host / fixture
// title reaches a live palette. Its non-vacuity guard proves the fixture still ships and is still offered in
// the (correct) unauthenticated preview — so a future deletion of the fixture cannot make these pass for the
// wrong reason, and reverting the !live gate turns them RED (the killing mutation).
import { chromium } from 'playwright';

const BASE = process.env.CONSOLE_BASE || `http://127.0.0.1:${process.env.CONSOLE_E2E_PORT || '8137'}`;

// Values that exist ONLY in the SESSIONS design fixture. Real live data cannot carry them: real session refs
// are source-scoped external_refs (librenms:dc1-<n>), never `s-<hex>`, and real estate hosts are
// dc1*/dc2* WITHOUT the "demo-" infix.
const FIXTURE_IDS = ['s-8f31', 's-7b02', 's-6c55', 's-5a12', 's-4d90'];
const DEMO_HOST_RE = /(?:dc1|dc2)demo-/;
const FIXTURE_TITLES = ['Disk pressure eviction cascade', 'Repeated auth failures', 'NAS volume 88%',
                        'Interface flap dc2demo', 'Payments-api p99 regression'];

let failed = 0;
const check = (name, ok, detail) => {
  console.log(`  ${ok ? 'ok  ' : 'FAIL'} ${name}${ok ? '' : ' — ' + detail}`);
  if (!ok) failed++;
};

const WHOAMI = { source: 'operator:test', mode: 'Semi-auto', may_actuate: true, posture_stale: false, posture_source: 'test' };

const reveal = () => { // Reveal the console the way the APP does — through setGate, not past it (the auth gate inerts #appRoot).
  if (typeof setGate === 'function') { setGate(false); const r = document.querySelector('#appRoot'); if (r) r.hidden = false; }
  else { const r = document.querySelector('#appRoot'); if (r) r.hidden = false; }
};

// Drive the REAL liveAdopt() with an intercepted /v1/sessions, then read the palette's session items and the
// live-state fields an assertion needs. whoami→200 makes the shell live; everything except the responder's
// target fails (each liveAdopt endpoint has its own catch, exactly as the ledger oracle relies on).
const adoptAndReadPalette = async (browser, sessionsResponder) => {
  const ctx = await browser.newContext();
  const page = await ctx.newPage();
  await page.route('**/v1/**', async r => {
    const u = r.request().url();
    if (u.includes('/v1/whoami')) return r.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(WHOAMI) });
    if (u.includes('/v1/sessions')) return sessionsResponder(r);
    return r.fulfill({ status: 500, contentType: 'application/json', body: '{}' });
  });
  await page.goto(`${BASE}/index.html`, { waitUntil: 'domcontentloaded' });
  await page.evaluate(reveal);
  await page.evaluate(async () => { try { await liveAdopt(); } catch (e) {} });
  await page.waitForFunction(() => typeof liveState === 'object' && liveState && liveState.on === true);
  const out = await page.evaluate(() => {
    const items = (typeof PAL_ITEMS === 'function') ? PAL_ITEMS() : [];
    return {
      on: !!liveState.on,
      sess: Array.isArray(liveState.sessions) ? liveState.sessions.length : (liveState.sessions === null ? 'null' : 'undef'),
      sessionItems: items.filter(x => x.type === 'session').map(x => ({ id: x.id, title: x.title, sub: x.sub })),
      sessionsFixtureRows: (typeof SESSIONS !== 'undefined') ? SESSIONS.length : -1,
    };
  });
  await ctx.close();
  return out;
};

// No palette session item may carry a fixture id, a *demo-* host, or a fixture title.
const leakReport = (items) => {
  const blob = items.map(i => `${i.id}|${i.title}|${i.sub}`).join('\n');
  const idHit = FIXTURE_IDS.filter(id => blob.includes(id));
  const hostHit = DEMO_HOST_RE.test(blob);
  const titleHit = FIXTURE_TITLES.filter(t => blob.includes(t));
  return { ok: idHit.length === 0 && !hostHit && titleHit.length === 0, idHit, hostHit, titleHit, blob };
};

const browser = await chromium.launch();
try {
  // --- LIVE + /v1/sessions FAILED: the palette must not fall back to the fabricated fixture ---
  {
    const r = await adoptAndReadPalette(browser, rq => rq.fulfill({ status: 500, contentType: 'application/json', body: '{}' }));
    check('LIVE+failed: shell is live (whoami adopted)', r.on === true, `liveState.on=${r.on}`);
    check('LIVE+failed: liveState.sessions is null (the failed read)', r.sess === 'null', `sessions=${r.sess}`);
    const v = leakReport(r.sessionItems);
    check('LIVE+failed: palette offers NO fabricated session', v.ok,
      `leaked ids=${JSON.stringify(v.idHit)} demoHost=${v.hostHit} titles=${JSON.stringify(v.titleHit)} :: "${v.blob.slice(0, 180)}"`);
  }

  // --- LIVE + /v1/sessions EMPTY: an honest empty must not read as five fabricated sessions ---
  {
    const r = await adoptAndReadPalette(browser, rq => rq.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ sessions: [] }) }));
    check('LIVE+empty: shell is live', r.on === true, `on=${r.on}`);
    const v = leakReport(r.sessionItems);
    check('LIVE+empty: palette offers NO fabricated session', v.ok,
      `leaked ids=${JSON.stringify(v.idHit)} demoHost=${v.hostHit} titles=${JSON.stringify(v.titleHit)}`);
  }

  // --- LIVE + real sessions: the palette indexes the live refs, never the fixture ---
  {
    const real = { sessions: [{ external_ref: 'librenms:dc1-90001', host: 'dc1k8s-w1', band: 'auto', op_class: 'k8s-drain' }] };
    const r = await adoptAndReadPalette(browser, rq => rq.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(real) }));
    check('LIVE+real: the live session ref IS indexed', r.sessionItems.some(i => (i.id || '').includes('librenms:dc1-90001')),
      `items=${JSON.stringify(r.sessionItems).slice(0, 180)}`);
    const v = leakReport(r.sessionItems);
    check('LIVE+real: no fixture session appears next to the live one', v.ok, `leaked ids=${JSON.stringify(v.idHit)} demoHost=${v.hostHit}`);
  }

  // --- NON-VACUITY: the unauthenticated design PREVIEW still offers the fixture (so the checks above are not
  //     vacuous, and a future deletion of the fixture cannot make them pass for the wrong reason) ---
  {
    const ctx = await browser.newContext();
    const page = await ctx.newPage();
    await page.route('**/v1/**', r => r.fulfill({ status: 500, contentType: 'application/json', body: '{}' })); // nothing adopts → not live
    await page.goto(`${BASE}/index.html`, { waitUntil: 'domcontentloaded' });
    await page.evaluate(reveal);
    await page.waitForFunction(() => typeof PAL_ITEMS === 'function' && typeof liveState === 'object' && liveState && liveState.on === false);
    const r = await page.evaluate(() => {
      const items = PAL_ITEMS();
      return { on: !!liveState.on, ids: items.filter(x => x.type === 'session').map(x => x.id),
               n: (typeof SESSIONS !== 'undefined') ? SESSIONS.length : -1 };
    });
    check('PREVIEW: shell is NOT live', r.on === false, `on=${r.on}`);
    check('PREVIEW: the design fixture STILL ships (the checks above are not vacuous)', r.n >= 5, `SESSIONS has ${r.n} rows`);
    check('PREVIEW: the fixture sessions ARE offered in the unauthenticated preview', FIXTURE_IDS.every(id => r.ids.includes(id)),
      `preview session ids=${JSON.stringify(r.ids)}`);
    await ctx.close();
  }

  // --- DOM integration: on a LIVE+failed console, typing a fixture id into the REAL palette shows nothing fabricated ---
  {
    const ctx = await browser.newContext();
    const page = await ctx.newPage();
    await page.route('**/v1/**', async r => {
      const u = r.request().url();
      if (u.includes('/v1/whoami')) return r.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(WHOAMI) });
      return r.fulfill({ status: 500, contentType: 'application/json', body: '{}' }); // sessions + all else fail
    });
    await page.goto(`${BASE}/index.html`, { waitUntil: 'domcontentloaded' });
    await page.evaluate(reveal);
    await page.evaluate(async () => { try { await liveAdopt(); } catch (e) {} });
    await page.waitForFunction(() => typeof liveState === 'object' && liveState && liveState.on === true);
    const t = await page.evaluate(() => { openPal(); renderPal('s-8f31'); return (document.querySelector('#palRes') || {}).innerText || ''; });
    // The leak signal is the fabricated ROW's content (a *demo-* host / a fixture title), NOT the id string
    // itself — the honest "No matches for “s-8f31”" empty-state echoes the query, so an id check would
    // false-fail on the correct result. Assert the honest empty renders and no fabricated content does.
    check('LIVE+failed DOM: searching "s-8f31" renders the honest "no matches", never a fabricated row',
      /no matches/i.test(t) && !DEMO_HOST_RE.test(t) && !FIXTURE_TITLES.some(x => t.includes(x)),
      `palette results: "${t.slice(0, 180)}"`);
    await ctx.close();
  }
} finally { await browser.close(); }

if (failed) { console.log(`\nsessions-never-fabricates: ${failed} FAILED`); process.exit(1); }
console.log('\nsessions-never-fabricates: all checks passed');
