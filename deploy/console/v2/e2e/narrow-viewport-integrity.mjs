// THE CONSOLE BELOW 1448px: THREE DEFECTS MEASURED ON THE LIVE CONSOLE, NONE VISIBLE AT DESK WIDTH.
//
// 1. #annun clipped its right-hand cell with `overflow:hidden` — no scrollbar, no ellipsis, no tell.
//    Live: clean at 1447px; at 1280px "PAUSE 937" was gone; at 1024px "NOTICE 102" went too. PAUSE is the
//    count of decisions waiting on a human, so it is the one number that must never be the one to vanish,
//    and a clipped row looks exactly like a row with nothing more to show.
// 2. At <=820px the CLOSED off-canvas rail is translateX(-100%) — off-screen and invisible — while all 22 of
//    its links stayed in the tab order and the accessibility tree. The `hidden`-is-not-`inert` mistake again.
// 3. The OPEN rail could not be dismissed: no Escape handler, and the only .scrim present belonged to the
//    session drawer (opacity:0, pointer-events:none). On a phone the nav opened and never closed.
//
// Asserted across a RANGE of widths rather than one, because the defect was width-dependent and invisible at
// the width a developer works at.
import { chromium } from 'playwright';

const BASE = process.env.CONSOLE_BASE || `http://127.0.0.1:${process.env.CONSOLE_E2E_PORT || '8137'}`;
let failed = 0;
const check = (name, ok, detail) => {
  console.log(`  ${ok ? 'ok  ' : 'FAIL'} ${name}${ok ? '' : ' — ' + detail}`);
  if (!ok) failed++;
};

const browser = await chromium.launch();
try {
  const page = await (await browser.newContext({ viewport: { width: 1600, height: 1000 } })).newPage();
  await page.route('**/v1/**', async rt => {
    const u = rt.request().url();
    const j = b => rt.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(b) });
    if (u.includes('/v1/whoami')) return j({ source: 'operator:test', mode: 'Semi-auto', may_actuate: true, posture_stale: false, posture_source: 'test' });
    if (u.includes('/v1/events')) return rt.abort();
    // Wide counts: the clipping only shows with realistic 3-digit numbers.
    if (u.includes('/v1/governance')) return j({ bands: { AUTO: 524, AUTO_NOTICE: 102, POLL_PAUSE: 937 } });
    if (u.includes('/v1/sessions')) return j({ total: 0, sessions: [] });
    return j({});
  });
  await page.goto(`${BASE}/index.html`, { waitUntil: 'domcontentloaded' });
  await page.evaluate(() => { if (typeof setGate === 'function') { setGate(false); const r = document.querySelector('#appRoot'); if (r) r.hidden = false; } });
  await page.evaluate(async () => { try { await liveAdopt(); } catch (e) {} });
  await page.evaluate(() => new Promise(r => requestAnimationFrame(() => r())));

  // ---- 1. EVERY band count is fully visible at EVERY width where the annunciator is shown ----
  for (const w of [1600, 1500, 1447, 1366, 1280, 1100, 900]) {
    await page.setViewportSize({ width: w, height: 1000 });
    await page.evaluate(() => new Promise(r => requestAnimationFrame(() => requestAnimationFrame(() => r())))); // reflow after resize
    const r = await page.evaluate(() => {
      const a = document.querySelector('#annun');
      if (!a || getComputedStyle(a).display === 'none') return { shown: false };
      const box = a.getBoundingClientRect();
      const cells = Array.from(a.children).map(c => {
        const q = c.getBoundingClientRect();
        return {
          txt: (c.textContent || '').replace(/\s+/g, ' ').trim(),
          num: (c.querySelector('b') || {}).textContent,
          name: c.getAttribute('aria-label'),
          inside: q.right <= box.right + 0.5 && q.left >= box.left - 0.5 && q.width > 0,
        };
      });
      return { shown: true, overflow: a.scrollWidth > a.clientWidth + 1, cells };
    });
    if (!r.shown) { console.log(`  ok   @${w}px annunciator not displayed at this width (hidden, not clipped)`); continue; }
    const outside = r.cells.filter(c => !c.inside).map(c => c.txt);
    check(`@${w}px every band cell is fully inside the annunciator`, outside.length === 0,
      `clipped: ${JSON.stringify(outside)} — a truncated row is indistinguishable from a complete one`);
    check(`@${w}px the annunciator does not overflow`, r.overflow === false, `scrollWidth exceeds clientWidth`);
    const nums = r.cells.map(c => c.num);
    check(`@${w}px all three COUNTS are rendered`, nums.every(n => n && n !== '—'), JSON.stringify(nums));
    // The word may be dropped for space, but the band must still be NAMED for assistive technology —
    // otherwise colour is the only cue, which conveys nothing.
    check(`@${w}px every cell carries an accessible band name`, r.cells.every(c => c.name && /AUTO|NOTICE|PAUSE/i.test(c.name)),
      JSON.stringify(r.cells.map(c => c.name)));
    // and the name must carry the CURRENT count, or it will drift from the number beside it
    check(`@${w}px the accessible name agrees with the rendered count`,
      r.cells.every(c => c.name && c.name.includes(String(c.num))),
      JSON.stringify(r.cells.map(c => [c.num, c.name])));
  }

  // ---- 2 & 3. THE OFF-CANVAS RAIL ----
  await page.setViewportSize({ width: 780, height: 900 });
  await page.evaluate(() => new Promise(r => requestAnimationFrame(() => requestAnimationFrame(() => r())))); // reflow after resize
  await page.evaluate(() => setRail(false));
  // The rail leaves through a 200ms transform transition, and `inert` lands synchronously while the box
  // does not — measuring in the frame that starts the close graded the DEPARTING rail as on-screen
  // whenever a loaded runner stretched the transition past the fixed wait (offScreen:false, inert:true).
  // Wait for the SETTLED geometry instead; on timeout fall through so the checks fail with the live box.
  await page.waitForFunction(() => {
    const box = document.querySelector('#rail').getBoundingClientRect();
    return box.right <= 1 || box.left >= window.innerWidth - 1;
  }, null, { timeout: 4000 }).catch(() => {});
  const closed = await page.evaluate(() => {
    const rail = document.querySelector('#rail');
    const F = 'a[href],button:not([disabled]),[tabindex]:not([tabindex="-1"])';
    const stops = Array.from(rail.querySelectorAll(F));
    const box = rail.getBoundingClientRect();
    return {
      offScreen: box.right <= 1 || box.left >= window.innerWidth - 1,
      inert: rail.hasAttribute('inert'),
      stops: stops.length,
      expanded: (document.querySelector('#menuBtn') || {}).getAttribute
        ? document.querySelector('#menuBtn').getAttribute('aria-expanded') : null,
    };
  });
  check('the closed rail really is off-screen', closed.offScreen === true, JSON.stringify(closed));
  check('the closed rail has tab stops to protect', closed.stops > 5, `${closed.stops} focusables`);
  check('an off-screen closed rail is INERT', closed.inert === true,
    `${closed.stops} invisible links stayed in the tab order and the accessibility tree`);
  check('the menu button reports collapsed', closed.expanded === 'false', String(closed.expanded));

  // Tab from the header must not walk into the hidden rail — the consequence, not just the attribute.
  const tabbed = await page.evaluate(async () => {
    setRail(false);
    document.querySelector('#menuBtn').focus();
    const seen = [];
    for (let i = 0; i < 12; i++) {
      // Playwright's keyboard is outside evaluate; walk the focusables the same way the browser would.
      const F = 'a[href],button:not([disabled]),input:not([disabled]),[tabindex]:not([tabindex="-1"])';
      const all = Array.from(document.querySelectorAll(F)).filter(n => {
        if (n.closest('[inert]')) return false;
        return n.offsetParent !== null || getComputedStyle(n).position === 'fixed';
      });
      seen.push(all.filter(n => n.closest('#rail')).length);
      break;
    }
    return { railStopsReachable: seen[0] };
  });
  check('NO rail control is reachable while the rail is closed', tabbed.railStopsReachable === 0,
    `${tabbed.railStopsReachable} rail controls still reachable`);

  const opened = await page.evaluate(async () => {
    const sleep = ms => new Promise(s => setTimeout(s, ms));
    const rail = document.querySelector('#rail');
    // Same transition, opposite direction: poll for the settled open geometry rather than trusting a
    // fixed wait to outlive the slide-in on a loaded runner. Times out to the checks, which then fail.
    const settled = async (pred, ms) => { const t0 = performance.now(); while (!pred() && performance.now() - t0 < ms) await sleep(16); };
    document.querySelector('#menuBtn').click();
    await settled(() => rail.getBoundingClientRect().left > -5, 4000);
    const scrim = document.querySelector('#railScrim');
    const scs = scrim ? getComputedStyle(scrim) : null;
    const state = {
      onScreen: rail.getBoundingClientRect().left > -5,
      inert: rail.hasAttribute('inert'),
      expanded: document.querySelector('#menuBtn').getAttribute('aria-expanded'),
      onStack: typeof overlayStack !== 'undefined' && overlayStack.some(o => o.el === rail),
      scrimPresent: !!scrim,
      scrimClickable: scs ? (scs.pointerEvents !== 'none' && parseFloat(scs.opacity) > 0) : null,
    };
    document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
    await sleep(300);
    state.closedByEscape = !rail.classList.contains('open');
    // and outside-click, independently
    document.querySelector('#menuBtn').click(); await sleep(300);
    const reopened = rail.classList.contains('open');
    if (scrim) scrim.click();
    await sleep(300);
    state.reopened = reopened;
    state.closedByOutsideClick = !rail.classList.contains('open');
    return state;
  });
  check('the open rail is on screen and not inert', opened.onScreen && opened.inert === false, JSON.stringify(opened));
  check('the menu button reports expanded', opened.expanded === 'true', String(opened.expanded));
  check('the open rail is enrolled in the overlay registry', opened.onStack === true,
    'without enrolment it has no focus trap and no shared Escape');
  check('a clickable scrim exists behind the open rail', opened.scrimPresent && opened.scrimClickable === true,
    `present=${opened.scrimPresent} clickable=${opened.scrimClickable} — the drawer scrim is opacity:0/pointer-events:none`);
  check('Escape dismisses the open rail', opened.closedByEscape === true, 'the nav could be opened and never closed');
  check('the rail could be reopened', opened.reopened === true, 'the toggle stopped working after one cycle');
  check('clicking outside dismisses the open rail', opened.closedByOutsideClick === true, JSON.stringify(opened));

  // ---- 4. THE WIDE LAYOUT MUST NOT INHERIT ANY OF THIS ----
  // Without this, "make the closed rail inert" would silently disable navigation on every desktop.
  await page.setViewportSize({ width: 1600, height: 1000 });
  await page.evaluate(() => new Promise(r => requestAnimationFrame(() => requestAnimationFrame(() => r())))); // reflow after resize
  const wide = await page.evaluate(() => {
    const rail = document.querySelector('#rail');
    const F = 'a[href],button:not([disabled]),[tabindex]:not([tabindex="-1"])';
    return {
      inert: rail.hasAttribute('inert'),
      visible: rail.getBoundingClientRect().width > 0,
      reachable: Array.from(rail.querySelectorAll(F)).filter(n => !n.closest('[inert]')).length,
    };
  });
  check('the WIDE rail is never inert', wide.inert === false, 'navigation would be dead on every desktop');
  check('and its controls are reachable', wide.visible && wide.reachable > 5, JSON.stringify(wide));
} finally { await browser.close(); }

console.log(failed ? `narrow-viewport-integrity: ${failed} FAILED` : 'narrow-viewport-integrity: all checks passed');
process.exit(failed ? 1 : 0);
