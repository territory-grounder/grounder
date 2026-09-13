// EVERY CONTROL HAS A NAME THAT SURVIVES TYPING, AND A REFUSAL EXPLAINS ITSELF.
//
// Two defects, both measured on the live console.
//
// 1. THIRTEEN controls resolved their accessible name ONLY through the placeholder fallback. A placeholder is
//    not a label: it is destroyed by the first keystroke, so the name a screen-reader user hears vanishes the
//    moment they start typing. Chrome's own accname engine (CDP Accessibility.getPartialAXTree, name.sources)
//    reported winningSource="placeholder" on every one. An earlier sweep found TEN of them; deriving the set
//    from the source found thirteen — the three extra were in #logs and #skills, views the sampler never
//    opened.
// 2. The sealed-secret form refused input SILENTLY. Nine distinct invalid states measured observationally
//    identical: aria-invalid null, aria-describedby null, no pattern, no required, the page's only
//    [role=alert] empty, the reserved message div empty, the container innerText the same 241 characters
//    every time. The only signal was a disabled button — disabled for FOUR different reasons the operator
//    could not tell apart. Two of the enforced rules were stated nowhere: 63 a's enabled the button, 64
//    disabled it, and the placeholder mentions neither the cap nor the first-character rule.
//
// This oracle asserts the ACCESSIBLE NAME, not the presence of an attribute — a control could carry
// aria-label="" and pass an attribute check while remaining nameless.
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
  // reveal-then-wait idiom aria-state.mjs uses for this exact setGate/hidden trick, and exactly the
  // condition the enumeration below reads.
  await page.waitForFunction(() => typeof views === 'object' && Object.keys(views).length > 5).catch(() => {});

  // ---- 1. no control may depend on its placeholder for a name, across EVERY view ----
  const views = await page.evaluate(() => (typeof views === 'object' ? Object.keys(views) : []));
  check('views are enumerable', views.length > 5, `${views.length} views`);
  const nameless = [];
  for (const v of views) {
    const bad = await page.evaluate(vv => {
      try { route(vv); } catch (e) { return []; }
      const out = [];
      for (const el of document.querySelectorAll('#view input, #view textarea, #view select')) {
        const aria = (el.getAttribute('aria-label') || '').trim();
        const by = el.getAttribute('aria-labelledby');
        const byText = by ? (document.getElementById(by) || {}).textContent : '';
        const lab = el.id ? document.querySelector(`label[for="${CSS.escape(el.id)}"]`) : null;
        const wrapped = el.closest('label');
        const named = aria || (byText || '').trim() || (lab && lab.textContent.trim()) || (wrapped && wrapped.textContent.trim());
        if (!named) out.push(vv + ' ' + el.tagName.toLowerCase() + '.' + (el.className || '').toString().split(' ')[0] +
                             ' placeholder=' + JSON.stringify((el.getAttribute('placeholder') || '').slice(0, 32)));
      }
      return out;
    }, v);
    nameless.push(...bad);
  }
  check(`no control anywhere is named ONLY by its placeholder (${views.length} views walked)`, nameless.length === 0,
        `${nameless.length}: ${nameless.slice(0, 4).join(' | ')}`);

  // ---- 2. the seal form must say WHY it refuses ----
  const seal = await page.evaluate(() => {
    if (typeof cfgSealForm !== 'function') return { missing: true };
    const box = cfgSealForm();
    document.body.appendChild(box);
    const ins = box.querySelectorAll('input, textarea');
    const ni = ins[0], vi = ins[1], ra = box.querySelector('textarea');
    const msg = box.querySelector('[role="status"]');
    const go = box.querySelector('button');
    const set = (el, val) => { el.value = val; el.dispatchEvent(new Event('input', { bubbles: true })); };
    const snap = () => ({ msg: (msg && msg.textContent || '').trim(), invalid: ni.getAttribute('aria-invalid'), dis: go.disabled });
    const out = {};
    out.pristine = snap();
    set(ni, 'LibreNMS.Token'); out.upper = snap();
    set(ni, '.librenms');      out.leadDot = snap();
    set(ni, 'a'.repeat(64));   out.tooLong = snap();
    set(ni, 'a'.repeat(63));   out.okName = snap();
    set(ni, 'librenms.token'); set(vi, ''); out.noValue = snap();
    set(vi, 'x'); set(ra, ''); out.noRationale = snap();
    set(ra, 'because'); out.valid = snap();
    out.describedby = ni.getAttribute('aria-describedby');
    out.msgId = msg && msg.id;
    document.body.removeChild(box);
    return out;
  });
  if (seal.missing) check('cfgSealForm reachable', false, 'not exposed — nothing below proves anything');
  else {
    check('pristine form does not shout at the operator', seal.pristine.msg === '', `said "${seal.pristine.msg}"`);
    check('an UPPERCASE name is explained', /lowercase/i.test(seal.upper.msg), `said "${seal.upper.msg}"`);
    check('a leading dot is explained', /start with/i.test(seal.leadDot.msg), `said "${seal.leadDot.msg}"`);
    check('a 64-char name names the LIMIT (a rule stated nowhere before)', /63/.test(seal.tooLong.msg), `said "${seal.tooLong.msg}"`);
    check('63 chars is accepted (the boundary is where it claims)', seal.okName.dis === false || /value|rationale/i.test(seal.okName.msg),
          `dis=${seal.okName.dis} msg="${seal.okName.msg}"`);
    check('a missing VALUE is distinguished from a bad name', /value/i.test(seal.noValue.msg), `said "${seal.noValue.msg}"`);
    check('a missing RATIONALE is distinguished too', /rationale/i.test(seal.noRationale.msg), `said "${seal.noRationale.msg}"`);
    check('a valid form is silent and enabled', seal.valid.msg === '' && seal.valid.dis === false, JSON.stringify(seal.valid));
    check('the name field is marked aria-invalid when it is', seal.upper.invalid === 'true' && seal.valid.invalid === 'false',
          `upper=${seal.upper.invalid} valid=${seal.valid.invalid}`);
    check('the message is BOUND to the field (aria-describedby)', !!seal.describedby && seal.describedby === seal.msgId,
          `describedby=${seal.describedby} msgId=${seal.msgId}`);
    // The four refusals must not read the same — that identity WAS the defect.
    const msgs = [seal.upper.msg, seal.tooLong.msg, seal.noValue.msg, seal.noRationale.msg];
    check('the four refusals are DISTINCT (they were byte-identical before)', new Set(msgs).size === 4,
          `${new Set(msgs).size} distinct: ${JSON.stringify(msgs)}`);
  }
} finally { await browser.close(); }

if (failed) { console.log(`\nform-semantics: ${failed} FAILED`); process.exit(1); }
console.log('\nform-semantics: all checks passed');
