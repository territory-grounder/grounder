// WCAG 2.4.3 — closing the command palette must return focus to whatever opened it.
//
// Measured on the LIVE authenticated console: Ctrl+K opened the palette with focus correctly in the input,
// Escape closed it, and document.activeElement came back as <body>. A keyboard operator who opened the
// palette from a control was dropped at the top of the tab order and had to traverse the page again.
//
// WHY THIS CAN RUN UNAUTHENTICATED, unlike the heatmap check next door: the palette's focus behaviour does
// not depend on any data or on the session — only on the app being visible. So the oracle reveals #appRoot
// and drives the real handlers. It never fabricates a result: it asserts on document.activeElement after
// real keyboard events, and it fails if the palette cannot be opened at all (which would make the whole
// check vacuous — the failure mode that let 217 inert buttons ship).
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
  await page.evaluate(() => {
    // Reveal the console the way the APP does. Poking hidden used to be enough; since the auth gate became
    // a real modal it also inerts #appRoot, so a bare hidden=false leaves a console that is visible and
    // cannot take focus — correct production behaviour for an unauthenticated page, and the reason this
    // must go through setGate rather than reach past it.
    if (typeof setGate === 'function') setGate(false);
    const r = document.querySelector('#appRoot');
    if (r) r.hidden = false;
    const gate = document.querySelector('#authGate') || document.querySelector('#agWrap');
    if (gate) gate.style.display = 'none';
  });
  await page.waitForFunction(() => !!(document.querySelector('#palOpen') || document.querySelector('.navi[data-view]')));

  // Focus a real, identifiable control, then open the palette from there.
  const anchored = await page.evaluate(() => {
    const t = document.querySelector('#palOpen') || document.querySelector('.navi[data-view]');
    if (!t) return null;
    t.id = t.id || 'tg-focus-origin';
    t.focus();
    return document.activeElement === t ? t.id : null;
  });
  check('a control can hold focus before opening the palette', !!anchored,
        'nothing focusable found — the rest of this oracle would prove nothing');

  await page.keyboard.press('Control+k');
  await page.waitForFunction(() => !!document.querySelector('#palScrim.open') && document.activeElement === document.querySelector('#palInput'));
  const opened = await page.evaluate(() => ({
    open: !!document.querySelector('#palScrim.open'),
    inInput: document.activeElement === document.querySelector('#palInput'),
  }));
  check('Ctrl+K opens the palette and focuses its input', opened.open && opened.inInput,
        `open=${opened.open} focusedInput=${opened.inInput} — a palette that never opened cannot prove a focus return`);

  await page.keyboard.press('Escape');
  await page.waitForFunction(() => !document.querySelector('#palScrim.open'));
  const closed = await page.evaluate(() => ({
    open: !!document.querySelector('#palScrim.open'),
    active: document.activeElement ? (document.activeElement.id || document.activeElement.tagName) : null,
  }));
  check('Escape closes the palette', !closed.open, 'palette stayed open');
  check('focus RETURNS to the control that opened it', anchored && closed.active === anchored,
        `focus landed on "${closed.active}", expected "${anchored}" — the operator loses their place in the tab order`);
} finally { await browser.close(); }

if (failed) { console.log(`\npalette-focus: ${failed} FAILED`); process.exit(1); }
console.log('\npalette-focus: all checks passed');
