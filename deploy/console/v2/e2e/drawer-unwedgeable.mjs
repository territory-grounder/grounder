// NO DATA SHAPE MAY LEAVE THE SESSION DRAWER SHUT AND SILENT.
//
// openSession() builds its ENTIRE body before revealing the panel. Two reads were unguarded, and their
// positions produced two different failures from the same cause:
//   · `s.action.plan.forEach(...)` runs BEFORE the reveal — a session whose projection carries no
//     action.plan threw there and the operator got NOTHING: no drawer, no error, the panel left closed and
//     aria-hidden with focus nowhere. Clicking a row appeared to do nothing at all.
//   · `s.predicted.hosts` runs AFTER the reveal — that one left the drawer OPEN and half-rendered.
//
// Guarding those two fields fixes today. The failure is structural, so the invariant is asserted instead:
// whatever a section builder does, the drawer ends up open, named, and honest about what it could not show.
// The last check drives a builder that throws for a reason nothing in this file anticipates, which is the
// only version of the test that speaks to the section someone adds next month.
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
  // Wait for the app to define openSession + a sample session rather than a fixed sleep — this is the exact
  // readiness the check below asserts, so waiting on it directly is deterministic (and if it never arrives,
  // the check reports the precise miss instead of an opaque timeout throw).
  await page.waitForFunction(() => typeof openSession === 'function' && typeof SESSIONS !== 'undefined' && !!SESSIONS[0], null, { timeout: 20000 }).catch(() => {});

  const base = await page.evaluate(() => typeof openSession === 'function' && typeof SESSIONS !== 'undefined' && !!SESSIONS[0]);
  check('openSession and a sample session are available', base, 'nothing below would prove anything');

  const open = async (mutate) => page.evaluate(m => {
    const d = document.querySelector('#drawer');
    d.classList.remove('open'); d.innerHTML = ''; d.setAttribute('aria-hidden', 'true');
    const s = JSON.parse(JSON.stringify(SESSIONS[0]));
    // eslint-disable-next-line no-new-func
    new Function('s', m)(s);
    let threw = null;
    try { openSession(s); } catch (e) { threw = String(e).slice(0, 80); }
    return {
      threw,
      open: d.classList.contains('open'),
      hidden: d.getAttribute('aria-hidden'),
      role: d.getAttribute('role'),
      name: d.getAttribute('aria-label'),
      text: (d.innerText || ''),
      focusInside: d.contains(document.activeElement),
    };
  }, mutate);

  // 1. the field that threw BEFORE the reveal — the operator saw nothing at all
  const noPlan = await open('delete s.action.plan;');
  check('a session with NO action.plan still opens the drawer', noPlan.open && noPlan.hidden !== 'true',
        JSON.stringify({ open: noPlan.open, hidden: noPlan.hidden, threw: noPlan.threw }));
  check('and it SAYS the plan is absent rather than showing an empty box',
        /not in this projection/i.test(noPlan.text), `plan note absent from ${noPlan.text.length} chars of panel text`);
  check('the rest of the session still renders', noPlan.text.length > 60, `only ${noPlan.text.length} chars rendered`);

  // 2. the field that threw AFTER the reveal — half-rendered
  const noPred = await open('delete s.predicted;');
  check('a session with NO predicted block still opens', noPred.open && !noPred.threw,
        JSON.stringify({ open: noPred.open, threw: noPred.threw }));

  // 3. both at once
  const neither = await open('delete s.action.plan; delete s.predicted; delete s.observed;');
  check('a session missing plan, prediction AND observation still opens', neither.open && !neither.threw,
        JSON.stringify({ open: neither.open, threw: neither.threw }));

  // 4. ★ THE INVARIANT, not the patch. A builder that throws for a reason this file does not anticipate.
  const boom = await page.evaluate(() => {
    const d = document.querySelector('#drawer');
    d.classList.remove('open'); d.innerHTML = ''; d.setAttribute('aria-hidden', 'true');
    const s = SESSIONS[0];
    const orig = window.dsec;
    window.dsec = () => { throw new Error('synthetic section failure'); };
    let threw = null;
    try { openSession(s); } catch (e) { threw = String(e).slice(0, 60); }
    window.dsec = orig;
    return { threw, open: d.classList.contains('open'), hidden: d.getAttribute('aria-hidden'),
             name: d.getAttribute('aria-label'), text: (d.innerText || '') };
  });
  check('an UNANTICIPATED section failure does not wedge the drawer shut', boom.open && boom.hidden !== 'true',
        JSON.stringify(boom));
  check('it never throws out to the caller', !boom.threw, `threw: ${boom.threw}`);
  check('it says it could not render, rather than showing a blank panel',
        /could not (be displayed|render)/i.test(boom.text), `text: "${boom.text.slice(0, 140)}"`);
  check('and it is still a NAMED dialog in that state', /could not render/i.test(boom.name || ''),
        `aria-label="${boom.name}"`);

  // 5. the guard against passing for the wrong reason
  const healthy = await open('void 0;');
  check('a healthy session still renders its real content (the fix did not blank the drawer)',
        healthy.open && healthy.text.length > 100 && !/could not/i.test(healthy.text),
        `${healthy.text.length} chars: "${healthy.text.slice(0, 100)}"`);
} finally { await browser.close(); }

if (failed) { console.log(`\ndrawer-unwedgeable: ${failed} FAILED`); process.exit(1); }
console.log('\ndrawer-unwedgeable: all checks passed');
