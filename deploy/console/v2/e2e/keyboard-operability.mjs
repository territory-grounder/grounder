// EVERY CONTROL MUST BE OPERABLE WITHOUT A MOUSE (WCAG 2.1.1, Level A).
//
// Measured live at 1440x900 in BOTH themes: inside #workflows,
//   `a[href], button:not([disabled]), input, select, textarea, [tabindex]:not([tabindex="-1"])` -> 0
//   [role] -> 0, [tabindex] -> 0, button -> 0, a[href] -> 0
// while the view rendered 50 `.wf-run` rows (311x51px, cursor:pointer), 10 `.wf-nh.click` stage headers and
// several `.wf-kid` ReAct rows — every one a bare <div> carrying an onclick. A keyboard or screen-reader
// operator could reach the view and then do NOTHING in it: no run selectable, no stage expandable. The whole
// governed-walk inspector was mouse-only.
//
// This asserts on the RENDERED output of the real renderer, and it asserts ACTIVATION, not just attributes:
// a div with tabindex=0 and role=button that ignores Enter is still unusable, and an attribute-only check
// would call it fixed.
import { chromium } from 'playwright';

const BASE = process.env.CONSOLE_BASE || `http://127.0.0.1:${process.env.CONSOLE_E2E_PORT || '8137'}`;
let failed = 0;
const check = (name, ok, detail) => {
  console.log(`  ${ok ? 'ok  ' : 'FAIL'} ${name}${ok ? '' : ' — ' + detail}`);
  if (!ok) failed++;
};
const FOCUSABLE = 'a[href],button:not([disabled]),input:not([disabled]),select,textarea,[tabindex]:not([tabindex="-1"])';

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
  // reveal-then-wait idiom aria-state.mjs uses for this exact setGate/hidden trick; route('workflows') is
  // called fresh in the next evaluate below, against the design fixture, not live-fetched data.
  await page.waitForFunction(() => typeof views === 'object' && Object.keys(views).length > 5).catch(() => {});

  const wf = await page.evaluate(() => {
    try { route('workflows'); } catch (e) { return { err: String(e) }; }
    const v = document.querySelector('#view');
    const F = 'a[href],button:not([disabled]),input:not([disabled]),select,textarea,[tabindex]:not([tabindex="-1"])';
    const runs = [...v.querySelectorAll('.wf-run')];
    const heads = [...v.querySelectorAll('.wf-nh.click')];
    return {
      focusables: v.querySelectorAll(F).length,
      runs: runs.length,
      runsFocusable: runs.filter(r => r.getAttribute('tabindex') === '0').length,
      runsRole: runs.filter(r => r.getAttribute('role') === 'option').length,
      runsSelected: runs.filter(r => r.hasAttribute('aria-selected')).length,
      listRole: (v.querySelector('.wf-list') || {}).getAttribute ? v.querySelector('.wf-list').getAttribute('role') : null,
      listName: (v.querySelector('.wf-list') || {}).getAttribute ? v.querySelector('.wf-list').getAttribute('aria-label') : null,
      heads: heads.length,
      headsFocusable: heads.filter(h => h.getAttribute('tabindex') === '0').length,
      headsExpanded: heads.filter(h => h.hasAttribute('aria-expanded')).length,
    };
  });
  check('#workflows renders without throwing', !wf.err, wf.err || '');
  // ★ HARD GATE. The first run of this oracle passed 5 of 9 checks while the view had thrown a
  // ReferenceError and rendered NOTHING — every pass came from an `=== 0 ||` short-circuit with no subject
  // to violate. If the subjects are absent, nothing below is evidence, so say so and stop.
  if (wf.err || !wf.runs) {
    check('there are run rows to operate (without them nothing below is evidence)', false,
          wf.err ? ('view threw: ' + wf.err) : '0 .wf-run rows rendered');
    console.log(`\nkeyboard-operability: ${failed} FAILED`); process.exit(1);
  }
  check('#workflows has focusable controls at all (it had ZERO)', wf.focusables > 0, `${wf.focusables} focusable`);
  // ★ UPDATED 2026-07-30 WHEN THE LISTBOX GAINED ITS KEYBOARD CONTRACT. This used to demand that EVERY run
  // row carry tabindex=0, which was true of the pre-fix build and is the opposite of the ARIA listbox pattern:
  // a listbox is ONE tab stop, and its options are reached with the arrow keys. With 50 live runs the old
  // shape put 50 tab stops between the facet chips and the detail pane.
  //
  // The INTENT — every run must be reachable by keyboard — is preserved and is now checked against the
  // widget's real model: exactly one option is tabbable, it is the selected one, and arrow navigation exists
  // to reach the rest (asserted end-to-end in composite-widget-contract.mjs, which drives the actual keys).
  // Weakening this to "some row is focusable" would have let a build where NO row was reachable pass.
  check('the run list is exactly ONE tab stop (roving tabindex)', wf.runsFocusable === 1,
    `${wf.runsFocusable}/${wf.runs} options tabbable — a listbox must be one tab stop, and 0 means unreachable`);
  check('every run row is an option carrying selection state', wf.runsRole === wf.runs && wf.runsSelected === wf.runs,
    `role=option on ${wf.runsRole}/${wf.runs}, aria-selected on ${wf.runsSelected}/${wf.runs}`);
  check('run rows are options in a named listbox', wf.runsRole === wf.runs && wf.listRole === 'listbox' && !!wf.listName,
        JSON.stringify({ role: wf.runsRole, list: wf.listRole, name: wf.listName }));
  check('run rows report selection state', wf.runsSelected === wf.runs, `${wf.runsSelected}/${wf.runs} carry aria-selected`);
  check('there are expandable stage headers to check', wf.heads > 0, `${wf.heads} .wf-nh.click rendered`);
  check('expandable stage headers are reachable and report expansion',
        wf.heads > 0 && wf.headsFocusable === wf.heads && wf.headsExpanded === wf.heads,
        JSON.stringify({ heads: wf.heads, focusable: wf.headsFocusable, expanded: wf.headsExpanded }));

  // ★ ACTIVATION, not attributes. A role=button that ignores Enter is still unusable.
  const act = await page.evaluate(() => {
    const run = document.querySelector('#view .wf-run:not(.sel)');
    if (!run) return { noRun: true };
    run.focus();
    const wasFocused = document.activeElement === run;
    const before = (document.querySelector('#view .wf-run.sel') || {}).textContent || '';
    run.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    const after = (document.querySelector('#view .wf-run.sel') || {}).textContent || '';
    return { wasFocused, changed: before !== after };
  });
  check('a run row can take focus', !act.noRun && act.wasFocused, 'focus() did not land on the row');
  check('ENTER selects a run (attributes alone are not operability)', !act.noRun && act.changed,
        'Enter on a focused run changed nothing — the row is still mouse-only');

  const space = await page.evaluate(() => {
    const h = document.querySelector('#view .wf-nh.click');
    if (!h) return { none: true };
    h.focus();
    const before = h.getAttribute('aria-expanded');
    h.dispatchEvent(new KeyboardEvent('keydown', { key: ' ', bubbles: true }));
    const now = document.querySelector('#view .wf-nh.click');
    return { before, after: now ? now.getAttribute('aria-expanded') : null };
  });
  check('SPACE toggles a stage disclosure', !space.none && space.before !== space.after,
        `aria-expanded ${space.before} -> ${space.after}`);
} finally { await browser.close(); }

if (failed) { console.log(`\nkeyboard-operability: ${failed} FAILED`); process.exit(1); }
console.log('\nkeyboard-operability: all checks passed');
