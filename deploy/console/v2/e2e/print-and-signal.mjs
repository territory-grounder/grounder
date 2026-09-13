// PAPER, AND STATE THAT DOES NOT DEPEND ON HUE.
//
// Three findings, all measured on the live console and each confirmed by an independent verifier.
//
// 1. PRINT. An operator PDF-ing an incident for a post-mortem got ONE page of a scrolled viewport. .app is a
//    100dvh grid whose panes scroll internally, and paper has no scrollbar, so everything below the fold
//    simply vanished — #ledger printed 1 page / 12 rows against 41 rows on screen, #actions 1 page / 11 rows
//    against 25. The screen furniture (rail, posture bar, KILL) was also printed, wasting the top of page 1.
//
// 2. THE VERDICT WAS HUE-ONLY. .stage.verified-match / -partial / -dev differ solely in the dot's colour.
//    Pushed through a severity-1.0 deuteranopia matrix, --match #33a891 and --partial #d7a03a converge; a
//    MATCH misread as a PARTIAL is a different operator decision. The bundle already had the second channel
//    (wfVerdictChip's MATCH ✓ / PARTIAL ◐ / DEVIATION ✕) and the spine did not use it.
//
// 3. THE SPINE'S POSITIONAL AXIS WAS IN EVERY ROW'S NAME. The .caps labels are byte-identical across all 50
//    rows on #actions, and they were entering each row's accessible name — so the same five words preceded
//    every row, while the REAL per-stage state lived only in per-dot title attributes that land on a depth-4
//    generic and are never announced.
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
  // reveal-then-wait idiom aria-state.mjs uses for this exact setGate/hidden trick.
  await page.waitForFunction(() => typeof views === 'object' && Object.keys(views).length > 5).catch(() => {});

  // ---- print ----
  await page.emulateMedia({ media: 'print' });
  // A reflow flush: getComputedStyle() below forces a synchronous style recalculation regardless, so this is
  // margin for the frame to settle, not a guess at how long the media-emulation switch takes.
  await page.evaluate(() => new Promise(r => requestAnimationFrame(() => r())));
  const pr = await page.evaluate(() => {
    const gone = s => { const e = document.querySelector(s); return !e || getComputedStyle(e).display === 'none'; };
    const app = document.querySelector('.app'); const view = document.querySelector('.view');
    return {
      rail: gone('.rail'), posture: gone('.posture'), kill: gone('.kill'), subbar: gone('.subbar'),
      appDisplay: app ? getComputedStyle(app).display : null,
      bodyOverflow: getComputedStyle(document.body).overflow,
      bodyHeight: getComputedStyle(document.body).height,
      viewOverflow: view ? getComputedStyle(view).overflow : null,
      theadGroup: (() => { const t = document.querySelector('.tbl thead'); return t ? getComputedStyle(t).display : null; })(),
    };
  });
  check('print: the rail, posture bar, sub-bar and KILL are dropped', pr.rail && pr.posture && pr.subbar && pr.kill, JSON.stringify(pr));
  check('print: .app stops being a viewport-height grid', pr.appDisplay === 'block', `display=${pr.appDisplay} — a 100dvh grid prints one screen`);
  check('print: the page may run longer than one screen', pr.bodyOverflow === 'visible' && pr.bodyHeight !== '100%',
        `overflow=${pr.bodyOverflow} height=${pr.bodyHeight}`);
  check('print: the view pane does not clip', pr.viewOverflow === 'visible', `overflow=${pr.viewOverflow}`);
  check('print: table headers repeat on every page', pr.theadGroup === 'table-header-group' || pr.theadGroup === null,
        `thead display=${pr.theadGroup}`);
  await page.emulateMedia({ media: 'screen' });

  // ---- verdict has a non-colour channel, and the spine names itself ----
  const spine = await page.evaluate(() => {
    if (typeof spine !== 'function' && typeof window.spine !== 'function') return { missing: true };
    const mk = (verdict) => {
      const s = { live: false, verdict, floored: false,
        stage: { classified: 1, predicted: 1, approved: 1, executed: 1, verified: 1 } };
      const el = (window.spine || spine)(s);
      const nodes = [...el.querySelectorAll('.stage .node')];
      const vNode = nodes[nodes.length - 1];
      return { glyph: (vNode.textContent || '').trim(), title: vNode.getAttribute('title') || '',
               label: el.getAttribute('aria-label') || '',
               capsHidden: (el.querySelector('.caps') || {}).getAttribute
                 ? el.querySelector('.caps').getAttribute('aria-hidden') : null };
    };
    return { match: mk('match'), partial: mk('partial'), dev: mk('deviation') };
  });
  if (spine.missing) check('spine() is reachable', false, 'not exposed — nothing below proves anything');
  else {
    const gl = [spine.match.glyph, spine.partial.glyph, spine.dev.glyph];
    check('each verdict carries a DISTINCT non-colour glyph', new Set(gl).size === 3 && gl.every(g => g),
          `glyphs ${JSON.stringify(gl)} — colour was the only channel`);
    check('the verdict is also in text on the node', /match/.test(spine.match.title) && /partial/.test(spine.partial.title),
          `titles ${JSON.stringify([spine.match.title, spine.partial.title])}`);
    check('the positional axis is hidden from the accessible name', spine.match.capsHidden === 'true',
          `.caps aria-hidden=${spine.match.capsHidden} — identical in all 50 rows, it belongs to no row`);
    check('the row NAMES its own per-stage state', /classified done/.test(spine.match.label) && /verdict match/.test(spine.match.label),
          `label="${spine.match.label}"`);
    check('a different verdict produces a different row name', spine.match.label !== spine.dev.label,
          'match and deviation rows announce identically');
  }
} finally { await browser.close(); }

if (failed) { console.log(`\nprint-and-signal: ${failed} FAILED`); process.exit(1); }
console.log('\nprint-and-signal: all checks passed');
