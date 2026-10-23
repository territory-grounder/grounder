// Console e2e — NO FIXTURE MAY ATTRIBUTE AN INVENTED INCIDENT TO A REAL MACHINE.
//
// Measured 2026-07-29 before the fix: with every /api mocked EMPTY (so everything rendered is fixture), SIX
// views named real-pattern estate hosts — including "Repeated auth failures" pinned on dc1fw01, the REAL
// production ASA. An operator (or anyone reading a screenshot) can act on a fabricated incident attached to
// a real machine; that is strictly worse than an obvious demo. Fixture estates now use self-declaring
// `<site>demo-*` hostnames, which keep the site prefix (the estate map's site clustering, liveEstSite
// parsing and the logs module's entity-highlight regex all still work) while being unmistakably synthetic.
//
// THE ORACLE'S SUBJECT IS THE RENDERED PAGE, NOT THE SOURCE: it mocks all APIs empty, walks EVERY view
// derived from the rail (never a hand-list — the deep-link incident taught why), and fails on any rendered
// hostname matching the real estate pattern that is not a demo- name. A future fixture naming a real host
// fails here by view name, whatever file it hides in.
import { chromium } from 'playwright';
const BASE = process.env.CONSOLE_BASE || 'http://127.0.0.1:8099';
const failures = [];
const ok = (c, m) => { if (!c) failures.push(m); };

// Real estate pattern WITHOUT the demo marker. The pattern is the naming law of this estate
// (<site><nn><role><nn>, sites nllei/grskg) — the same one the logs module highlights.
const REAL = /(dc1|dc2)(?!demo-)[a-z0-9-]+/g;

const browser = await chromium.launch();
try {
  const page = await browser.newContext({ viewport: { width: 1600, height: 1000 } }).then(c => c.newPage());
  const pageErrors = []; page.on('pageerror', e => pageErrors.push(String(e).slice(0, 100)));
  await page.route('**/api/**', r => {
    const u = r.request().url().split('/api')[1].split('?')[0];
    if (u === '/v1/whoami') return r.fulfill({ json: { source: 'operator:tester', mode: 'Shadow', may_actuate: false, posture_stale: false, posture_source: 'runtime' } });
    return r.fulfill({ json: {} }); // EVERYTHING else empty: whatever renders is fixture content
  });
  await page.goto(BASE + '/index.html', { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('#appRoot:not([hidden])', { timeout: 20000 });

  const views = await page.evaluate(() => [...document.querySelectorAll('.navi')].map(n => n.dataset.view).filter(Boolean));
  ok(views.length >= 15, `only ${views.length} views in the rail — the enumeration is broken and a green run proves nothing`);

  let demoSeen = 0;
  for (const k of views) {
    await page.evaluate(kk => { const a = [...document.querySelectorAll('.navi')].find(x => x.dataset.view === kk); if (a) a.click(); }, k);
    // console.html wires every .navi with `addEventListener("click", e => { e.preventDefault(); route(...) })`
    // and route() is synchronous, so the click's route(k) has already run by the time click() returns — a
    // reflow flush is enough margin for the DOM to settle, not a guess at fetch latency (every /api response
    // in this file is the generic empty fixture, so nothing here waits on a network round-trip either).
    await page.evaluate(() => new Promise(r => requestAnimationFrame(() => r())));
    const txt = await page.evaluate(() => (document.querySelector('#view')?.innerText || ''));
    const hits = [...new Set(txt.match(REAL) || [])];
    ok(hits.length === 0, `#${k}: fixture content names real-pattern host(s) ${hits.slice(0, 5).join(', ')} — ` +
      'an invented incident attributed to a real machine; rename to <site>demo-* or wire the surface live');
    if (/demo-/.test(txt)) demoSeen++;
  }
  // Scanner self-check: the demo estate must actually render somewhere, or an over-eager empty-state hid
  // every fixture and the zero-hits result proved nothing about the renames.
  ok(demoSeen >= 3, `demo- hostnames rendered in only ${demoSeen} view(s) — the fixtures did not render, so ` +
    'the no-real-hosts result is vacuous');
  ok(pageErrors.length === 0, 'uncaught JS errors: ' + pageErrors.join(' | '));
} finally { await browser.close(); }

if (failures.length) { console.error('FIXTURE-HOSTNAMES E2E FAIL:\n  - ' + failures.join('\n  - ')); process.exit(1); }
console.log('FIXTURE-HOSTNAMES E2E PASS — with all APIs empty, no view attributes fixture content to a real-pattern hostname, and the demo estate visibly renders.');
