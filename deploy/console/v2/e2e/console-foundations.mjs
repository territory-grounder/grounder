// FOUNDATIONS THE WHOLE CONSOLE RESTS ON — asserted on the ASSEMBLED artifact operators are served.
//
// Every one of these was measured FAILING on https://territory-grounder.example.net on 2026-07-29
// with Playwright across 22 views x 2 viewports:
//
//   document.compatMode === "BackCompat"  the served artifact carried NO DOCTYPE, so every browser fell back
//                                         to QUIRKS MODE — legacy box model and table layout, on a console
//                                         used to authorise estate mutations.
//   <html> had no lang                    WCAG 3.1.1 (A): a screen reader cannot pick a pronunciation set.
//   0 <h1> on all 22 views                no document heading; nothing to navigate by.
//   no skip link                          a keyboard user traversed the entire 22-item rail to reach content.
//   21 of 22 nav items were <a> with NO   an anchor without href is not focusable and is not exposed as a
//   href, driven by click handlers        link — the nav was unreachable by keyboard.
//   mobile document 429px in a 390px      button.kill overflowed by exactly 39px, and that ONE element made
//   viewport on ALL 22 views              every view scroll sideways on a phone.
//
// Run: CONSOLE_BASE=http://127.0.0.1:8137 node console-foundations.mjs
import { chromium } from 'playwright';

// Checks that could not run in this environment. Reported separately at the end: a skip is not a pass.
const skipped = [];
const BASE = process.env.CONSOLE_BASE || 'http://127.0.0.1:8137';
let failed = 0;
const check = (name, ok, detail) => {
  if (ok) { console.log(`  ok   ${name}`); return; }
  failed++; console.log(`  FAIL ${name} — ${detail}`);
};

const browser = await chromium.launch();
for (const vp of [{ n: 'desktop', width: 1600, height: 1100 }, { n: 'mobile', width: 390, height: 844 }]) {
  const ctx = await browser.newContext({ viewport: { width: vp.width, height: vp.height } });
  const page = await ctx.newPage();
  const errs = [];
  page.on('pageerror', e => errs.push(String(e).slice(0, 160)));
  await page.goto(BASE + '/index.html', { waitUntil: 'domcontentloaded', timeout: 30000 });
  // This file mocks no API, so whoami always fails and #appRoot stays on the unauthenticated boot path:
  // liveAdopt() catches the failed whoami and calls liveLoginOverlay(), whose ONLY further act is focusing
  // #agOp — the one observable signal that the (one) async step in this path (the whoami round-trip) has
  // settled, rather than guessing how long a local 404 takes.
  await page.waitForFunction(() => document.activeElement && document.activeElement.id === 'agOp').catch(() => {});
  const f = await page.evaluate(() => ({
    compatMode: document.compatMode,
    lang: document.documentElement.lang || '',
    h1: document.querySelectorAll('h1').length,
    skip: !!document.querySelector('.skip-link'),
    navi: document.querySelectorAll('.navi').length,
    naviHref: document.querySelectorAll('.navi[href]').length,
    docW: document.documentElement.scrollWidth,
    vw: document.documentElement.clientWidth,
    palLabelled: !!document.querySelector('#palInput[aria-label]'),
    // ★ MEASURE THE ELEMENTS, NOT JUST THE DOCUMENT. A document-width check alone is satisfied by
    // `overflow-x:hidden`, which HIDES an overflow instead of curing it — mutation CE proved exactly that
    // by staying GREEN. Asking which elements extend past the viewport cannot be silenced that way.
    // ★ AN ELEMENT INSIDE A HORIZONTAL SCROLLER IS SUPPOSED TO EXTEND PAST THE VIEWPORT — that is what
    // scrolling MEANS. Before this exclusion the check reported table/thead/tr/th/tbody as violations the
    // moment .wrapcard became a real scroller, i.e. it went RED because a defect had been FIXED. Measured
    // live: hiding every table left scrollWidth unchanged at 426, so those elements contribute nothing to
    // page scroll. Only an element whose overflow the user cannot reach is a defect.
    overflowing: Array.from(document.querySelectorAll('body *')).filter(e => {
      const r = e.getBoundingClientRect();
      if (!(r.width > 0 && r.right > document.documentElement.clientWidth + 2)) return false;
      if (getComputedStyle(e).position === 'fixed') return false;
      for (let n = e.parentElement; n && n !== document.documentElement; n = n.parentElement) {
        const ox = getComputedStyle(n).overflowX;
        if ((ox === 'auto' || ox === 'scroll') && n.scrollWidth > n.clientWidth + 1) return false;
      }
      return true;
    }).map(e => e.tagName + '.' + (e.className || '').toString().split(' ')[0] +
                ' right=' + Math.round(e.getBoundingClientRect().right)).slice(0, 5),
  }));
  console.log(`[${vp.n}]`);
  // ★ STANDARDS MODE IS THE ONE THAT CANNOT BE ASSERTED FROM THE SOURCE TEXT. A DOCTYPE can be present in
  // console.html and still absent from the assembled artifact; only the BROWSER's compatMode proves what a
  // real operator's engine decided.
  check('standards mode (not quirks)', f.compatMode === 'CSS1Compat', `compatMode=${f.compatMode}`);
  check('html[lang] is set', f.lang.length > 0, `lang=${JSON.stringify(f.lang)}`);
  check('exactly one <h1>', f.h1 === 1, `h1 count=${f.h1}`);
  check('skip link present', f.skip, 'no .skip-link');
  check('EVERY nav item is keyboard-reachable', f.navi > 0 && f.naviHref === f.navi,
    `${f.naviHref}/${f.navi} .navi have href — an <a> without href is not focusable`);
  check('command palette input has an accessible name', f.palLabelled, '#palInput has no aria-label');
  // The page body must never scroll sideways; wide content scrolls inside its own container instead.
  check('no horizontal page scroll', f.docW <= f.vw + 2, `document ${f.docW}px in a ${f.vw}px viewport`);
  check('NO element extends past the viewport', f.overflowing.length === 0,
    `${f.overflowing.length} overflow: ${f.overflowing.join(', ')}`);
  check('no JS errors', errs.length === 0, errs.join(' | '));
  await ctx.close();
}
// ★ AN ELEMENT MUST NOT CLAIM INTERACTIVITY IT DOES NOT HAVE. The estate heatmap rendered EVERY host as a
// <button>, but a cell's onclick only fires when that host has a matching session. Measured live 2026-07-29:
// 217 button.sig-cell, of which ZERO were actionable — a keyboard operator tabbed through 217 dead stops to
// cross the signals view. Cells without a session are now <span>.
//
// The check is on the SERVED page rather than the source because the condition is a RENDER decision: reading
// the module text would only prove the branch exists, not which branch the data takes.
{
  // Reuse the browser above. `await (await chromium.launch()).newContext(...)` launched a SECOND browser
  // and discarded the handle, so nothing could ever close it: the driver ChildProcess and six sockets
  // outlived browser.close(), node never exited, and run.sh — which runs the oracles SERIALLY — sat on
  // this one forever. Every oracle alphabetically after console-foundations silently never ran. The
  // suite printed 'all checks passed' first, so it looked like a healthy run that was merely slow.
  const ctx = await browser.newContext({ viewport: { width: 1600, height: 1100 } });
  const page = await ctx.newPage();
  await page.goto(BASE + '/index.html#signals', { waitUntil: 'domcontentloaded', timeout: 30000 });
  // This file mocks no API, so whoami always fails and #appRoot stays on the unauthenticated boot path:
  // liveAdopt() catches the failed whoami and calls liveLoginOverlay(), whose ONLY further act is focusing
  // #agOp — the one observable signal that the (one) async step in this path (the whoami round-trip) has
  // settled, rather than guessing how long a local 404 takes.
  await page.waitForFunction(() => document.activeElement && document.activeElement.id === 'agOp').catch(() => {});
  const cells = await page.evaluate(() => {
    const all = Array.from(document.querySelectorAll('.sig-cell'));
    const buttons = all.filter(e => e.tagName === 'BUTTON');
    // A cell is genuinely actionable only when its title carries a session id (4th ' · ' segment).
    const actionable = all.filter(e => (e.getAttribute('title') || '').split(' · ').length > 3);
    return { total: all.length, buttons: buttons.length, actionable: actionable.length,
             inertButtons: buttons.filter(e => (e.getAttribute('title') || '').split(' · ').length <= 3).length };
  });
  console.log('[signals heatmap]');
  if (cells.total === 0) {
    // NOT "ok". This oracle needs the authenticated console to render the heat-row, and unauthenticated the
    // sign-in gate covers the app so ZERO cells exist. Printing "ok" here is how an oracle that visits none
    // of its inputs reads as a pass — the exact shape that let 217 inert buttons ship. Say what it is.
    skipped.push('signals heatmap: 0 cells rendered — this run asserted NOTHING about cell semantics; ' +
                 'the binding proof is the authenticated live check');
    console.log('  SKIP  0 cells rendered (unauthenticated) — asserted NOTHING; proof is the live run');
  } else {
    check('no INERT cell is a <button>', cells.inertButtons === 0,
      `${cells.inertButtons} of ${cells.total} cells are buttons with no session — each is a dead keyboard stop`);
    check('every ACTIONABLE cell IS a <button>', cells.buttons >= cells.actionable,
      `${cells.actionable} actionable but only ${cells.buttons} buttons — an actionable cell must be operable`);
  }
  await ctx.close();
}

await browser.close();

if (skipped.length) {
  console.log('\nSKIPPED (proved nothing here — do not read the green above as covering these):');
  for (const s2 of skipped) console.log('  · ' + s2);
}
if (failed) { console.log(`\nconsole-foundations: ${failed} FAILED`); process.exit(1); }
console.log('\nconsole-foundations: all checks passed');
