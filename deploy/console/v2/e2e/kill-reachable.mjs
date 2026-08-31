// THE EMERGENCY STOP MUST BE ON SCREEN AT EVERY WIDTH AN OPERATOR USES.
//
// Measured live before this fix, seven viewports:
//   viewport   documentElement.scrollWidth   KILL right edge
//   1024       1499                          1499   <- 475px past the right edge
//   1280       1499                          1499
//   1366       1499                          1499   <- a standard operator laptop
//   1440/1443/1444  1499                     1499
//   1600       1600                          1584   ok
// header.posture is a nowrap flex row whose children need 1499px and none of which could shrink. Below
// ~1499 the KILL button is not clipped — it is rendered entirely beyond the viewport, and the page has no
// horizontal scrollbar to reach it. On a 1366x768 machine the operator's emergency stop cannot be seen or
// clicked.
//
// The 390px case was fixed earlier the same day, for phones only, which is exactly why this survived: a
// breakpoint fix cannot cover a continuous band. This oracle therefore sweeps widths rather than checking
// one, and asserts the property that matters — KILL inside the viewport — not "the page does not scroll".
//
// ★ WHAT THIS ORACLE CANNOT PROVE, STATED PLAINLY. Its mutation control (revert .breaker to flex:0 0 auto)
// came back GREEN, so this file does NOT reproduce the overflow. The reason is content, not CSS: served
// UNAUTHENTICATED the header renders placeholders — the annunciator is "—", .acct is 20px, the breaker
// carries a short string — and measures 1024px at a 1024px viewport. The LIVE header carries the operator
// name, real band counts and "Mutation On · Operational", and needs 1499px. A defect that only exists when
// real data widens the row cannot be caught by a harness that has no real data.
// So the load-bearing assertion for THIS defect lives in the live e2e, which sweeps the same widths against
// the deployed console with a real session. What survives here is the control that DID fire: DN (display:none
// on .kill) turns this red, so it still guards the cheapest wrong fix — making the numbers work by removing
// the button. Keeping a check that catches one thing and saying which is better than deleting it or
// pretending it covers both.
import { chromium } from 'playwright';

const BASE = process.env.CONSOLE_BASE || `http://127.0.0.1:${process.env.CONSOLE_E2E_PORT || '8137'}`;
// Real operator screens plus the boundaries either side of the old failure point.
const WIDTHS = [1024, 1280, 1366, 1440, 1444, 1498, 1500, 1600, 1920];
let failed = 0;
const check = (name, ok, detail) => {
  console.log(`  ${ok ? 'ok  ' : 'FAIL'} ${name}${ok ? '' : ' — ' + detail}`);
  if (!ok) failed++;
};

const browser = await chromium.launch();
try {
  const rows = [];
  for (const w of WIDTHS) {
    const ctx = await browser.newContext({ viewport: { width: w, height: 900 } });
    const page = await ctx.newPage();
    await page.goto(`${BASE}/index.html`, { waitUntil: 'domcontentloaded' });
    await page.evaluate(() => { // Reveal the console the way the APP does. Poking #appRoot.hidden used to be enough; since the auth
    // gate became a real modal it also inerts #appRoot, so a bare hidden=false leaves a console that is
    // visible and cannot take focus — which is the correct production behaviour for an unauthenticated
    // page, and the reason this line must go through setGate rather than reach past it.
    if (typeof setGate === 'function') { setGate(false); const r = document.querySelector('#appRoot'); if (r) r.hidden = false; }
    else { const r = document.querySelector('#appRoot'); if (r) r.hidden = false; } });
    // Wait for the boot script to have parsed (views populated) rather than a fixed guess — the same
    // reveal-then-wait idiom aria-state.mjs uses for this exact setGate/hidden trick. The measurement below
    // reads rendered CSS geometry off static/fixture chrome, not live-fetched data.
    await page.waitForFunction(() => typeof views === 'object' && Object.keys(views).length > 5).catch(() => {});
    rows.push(await page.evaluate(() => {
      const de = document.documentElement;
      const k = document.querySelector('button.kill');
      const b = document.querySelector('.breaker');
      if (!k) return { noKill: true, cli: de.clientWidth };
      const r = k.getBoundingClientRect(), cs = getComputedStyle(k);
      return {
        cli: de.clientWidth, doc: de.scrollWidth,
        right: Math.round(r.right), left: Math.round(r.left),
        w: Math.round(r.width), h: Math.round(r.height),
        visible: r.width > 0 && r.height > 0 && cs.display !== 'none' && cs.visibility !== 'hidden',
        breaker: b ? (b.textContent || '').trim() : '',
      };
    }));
    await ctx.close();
  }

  for (let i = 0; i < WIDTHS.length; i++) {
    const w = WIDTHS[i], r = rows[i];
    check(`@${w}: the KILL button exists and is rendered`, !r.noKill && r.visible, JSON.stringify(r));
    if (r.noKill) continue;
    // The property that matters. "no page scroll" is NOT the same assertion: a page can avoid scrolling by
    // clipping, and a clipped emergency stop is exactly as unreachable as an overflowing one.
    check(`@${w}: KILL's right edge is INSIDE the viewport`, r.right <= r.cli,
          `right=${r.right} viewport=${r.cli} — ${r.right - r.cli}px beyond the edge, with no scrollbar to reach it`);
    check(`@${w}: KILL is fully within the viewport, not half-cut`, r.left >= 0 && r.right <= r.cli && r.w >= 40,
          `left=${r.left} right=${r.right} width=${r.w}`);
    check(`@${w}: the page does not scroll sideways`, r.doc <= r.cli + 1, `doc=${r.doc} cli=${r.cli}`);
  }

  // The shrink must cost information, not meaning: the posture text is a safety statement and must survive.
  const narrow = rows[0];
  check('the mutation posture is still legible at the narrowest width',
        !narrow.noKill && /MODE|Mode|May actuate|Read-only|On|Off|UNVERIFIED|unverified/i.test(narrow.breaker),
        `breaker read "${narrow.breaker}" at ${WIDTHS[0]}px — the shrink must not erase a safety statement`);

  // Guard: this must not pass because the button was hidden to make the numbers work.
  check('KILL is never hidden to make the layout fit', rows.every(r => r.noKill || (r.visible && r.h >= 24)),
        JSON.stringify(rows.map((r, i) => ({ w: WIDTHS[i], vis: r.visible, h: r.h }))));
} finally { await browser.close(); }

if (failed) { console.log(`\nkill-reachable: ${failed} FAILED`); process.exit(1); }
console.log('\nkill-reachable: all checks passed');
