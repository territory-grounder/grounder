// NO SURFACE MAY CONTRADICT THE LIVE MUTATION POSTURE.
//
// Measured live on the deployed console, 2026-07-29, on ONE page load and with no failure simulated:
//   topbar breaker : "Mutation On · Operational"  (data-mode="operational")
//   #governance    : "MUTATION: OFF — the estate is read-only."
//   KILL toast     : "Mutation is OFF — no autonomy is running to halt."
// The estate has been running mutation ON. A design-era placeholder became a false statement about a SAFETY
// state the moment the switch was thrown, in eleven places across six files, and nothing tied any of them to
// the one function that actually learns the posture. The KILL case is the sharpest: an operator reaching for
// the emergency stop during an incident was told there was nothing to halt.
//
// This oracle drives the REAL posture through setBreaker — the single place the console learns it — and then
// asserts that no view's rendered text claims the opposite. It walks EVERY view rather than #governance
// alone, because the claim was duplicated into workflows, estatedepth, models and modules too; checking the
// one view I happened to find it in would leave four copies free to drift.
import { chromium } from 'playwright';

const BASE = process.env.CONSOLE_BASE || `http://127.0.0.1:${process.env.CONSOLE_E2E_PORT || '8137'}`;
let failed = 0;
const check = (name, ok, detail) => {
  console.log(`  ${ok ? 'ok  ' : 'FAIL'} ${name}${ok ? '' : ' — ' + detail}`);
  if (!ok) failed++;
};

const browser = await chromium.launch();
try {
  const page = await (await browser.newContext()).newPage();
  await page.goto(`${BASE}/index.html`, { waitUntil: 'domcontentloaded' });
  await page.evaluate(() => { // Reveal the console the way the APP does. Poking #appRoot.hidden used to be enough; since the auth
    // gate became a real modal it also inerts #appRoot, so a bare hidden=false leaves a console that is
    // visible and cannot take focus — which is the correct production behaviour for an unauthenticated
    // page, and the reason this line must go through setGate rather than reach past it.
    if (typeof setGate === 'function') { setGate(false); const r = document.querySelector('#appRoot'); if (r) r.hidden = false; }
    else { const r = document.querySelector('#appRoot'); if (r) r.hidden = false; } });
  // Wait for the boot script to have parsed (views populated) rather than a fixed guess — the same
  // reveal-then-wait idiom aria-state.mjs uses for this exact setGate/hidden trick. setBreaker/postureClaim
  // /views are all script globals set at parse time, not live-fetched data.
  await page.waitForFunction(() => typeof views === 'object' && Object.keys(views).length > 5).catch(() => {});

  const api = await page.evaluate(() => ({
    setBreaker: typeof setBreaker === 'function',
    postureClaim: typeof postureClaim === 'function',
    views: typeof views === 'object' ? Object.keys(views).length : 0,
  }));
  check('the console exposes setBreaker() and postureClaim()', api.setBreaker && api.postureClaim,
        JSON.stringify(api) + ' — nothing below would prove anything');
  check('there are views to walk', api.views > 5, `${api.views} views`);

  // ★ FAIL-CLOSED AT BOOT. Before any live read resolves, the console has NOT learned the posture, and the
  // one thing it must never do is guess OFF: an operator who reads "the estate is read-only" during the boot
  // window concludes nothing can act while something is acting. setBreaker has exactly two callers, both on
  // the live path, so this state is what a fresh page shows until the worker's posture arrives.
  const boot = await page.evaluate(() => postureClaim().state);
  check('BEFORE any live read, the posture is UNVERIFIED (never a guessed OFF)', boot === 'unverified',
        `boot posture is "${boot}"`);

  // Every posture the chip itself can report (4-mode × may_actuate, TG-112). "unverified" is a real state, not an edge case: it is what
  // the console shows when the worker has not published a fresh posture, and it must never read as a
  // confident OFF.
  const CASES = [
    { name: 'Semi-auto · may actuate',  args: ['Semi-auto', true, false, 'test'],  want: 'MODE: SEMI-AUTO · MAY ACTUATE',  forbid: /MODE: [A-Z-]+ · READ-ONLY|ACTUATION POSTURE UNVERIFIED/ },
    { name: 'Full-auto · may actuate',  args: ['Full-auto', true, false, 'test'],  want: 'MODE: FULL-AUTO · MAY ACTUATE',  forbid: /MODE: [A-Z-]+ · READ-ONLY|ACTUATION POSTURE UNVERIFIED/ },
    { name: 'Shadow · read-only',       args: ['Shadow', false, false, 'test'],    want: 'MODE: SHADOW · READ-ONLY',       forbid: /· MAY ACTUATE/ },
    { name: 'HITL · read-only',         args: ['HITL', false, false, 'test'],      want: 'MODE: HITL · READ-ONLY',         forbid: /· MAY ACTUATE/ },
    { name: 'unverified',               args: [null, undefined, true, 'test'],     want: 'ACTUATION POSTURE UNVERIFIED',   forbid: /MODE: [A-Z-]+ · (MAY ACTUATE|READ-ONLY)/ },
  ];

  for (const c of CASES) {
    const claim = await page.evaluate(a => { setBreaker(a[0], a[1], a[2], a[3]); return postureClaim().label; }, c.args);
    check(`posture ${c.name}: postureClaim() says "${c.want}"`, claim === c.want, `got "${claim}"`);

    // Walk EVERY view and read what an operator would actually see.
    const bad = await page.evaluate(async (forbidSrc) => {
      const forbid = new RegExp(forbidSrc);
      const out = [];
      for (const v of Object.keys(views)) {
        try { route(v); } catch (e) { continue; }          // a view needing live data still renders its chrome
        const t = (document.querySelector('#view') || {}).innerText || '';
        const m = t.match(forbid);
        if (m) out.push(v + ': "' + t.slice(Math.max(0, m.index - 30), m.index + 40).replace(/\s+/g, ' ') + '"');
      }
      return out;
    }, c.forbid.source);
    check(`posture ${c.name}: NO view contradicts it`, bad.length === 0,
          `${bad.length} view(s) claim the opposite: ${bad.slice(0, 3).join(' | ')}`);
  }

  // The emergency stop's own help text is the one that matters most.
  const kill = await page.evaluate(() => {
    const said = [];
    for (const [mode, may, st] of [['Semi-auto', true, false], ['Shadow', false, false], [null, undefined, true]]) {
      setBreaker(mode, may, st, 'test');
      document.querySelector('#killBtn').click();
      said.push((document.querySelector('#toast') || {}).innerText || '');
    }
    return said;
  });
  check('KILL never says "nothing to halt" while the mode permits actuation', !/nothing (is running )?to halt/i.test(kill[0]),
        `ON toast: "${(kill[0] || '').slice(0, 90)}"`);
  check('KILL never says "nothing to halt" while the posture is UNVERIFIED', !/nothing (is running )?to halt/i.test(kill[2]),
        `unverified toast: "${(kill[2] || '').slice(0, 90)}"`);
} finally { await browser.close(); }

if (failed) { console.log(`\nposture-honesty: ${failed} FAILED`); process.exit(1); }
console.log('\nposture-honesty: all checks passed');
