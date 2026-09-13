// STATUS MESSAGES MUST BE ANNOUNCED, NOT JUST SHOWN.
//
// Measured live across all 22 views: exactly ONE node matched
// [aria-live],[role=alert],[role=status],[role=log],[role=alertdialog],[role=marquee],[role=timer],output
// — #agErr, the SIGN-IN GATE's error region — and post-login its offsetParent is null in 22 of 22 views.
// A node that is not rendered is not announced, so this console had, in practice, ZERO live regions once an
// operator was actually using it. Its ~25 status writes (17 distinct strings: "sealing…", "saved", "admin
// elevation required — step up to write", every write failure) reached a sighted operator and nobody else.
//
// ★ THE FIX IS ON THE CONTAINER, NOT THE WRITE SITE, and that choice is the point. Routing individual
// `msg.textContent = …` calls through an announce() helper is fragile: a regex over this bundle matched 7 of
// ~25, and the next status write anyone adds would silently not announce. A container marked role=status
// announces EVERY write into it, including from code that does not know the region exists. The app-level
// #tgAnnounce region is kept for toasts, which have no per-form container.
import { chromium } from 'playwright';

const BASE = process.env.CONSOLE_BASE || `http://127.0.0.1:${process.env.CONSOLE_E2E_PORT || '8137'}`;
let failed = 0;
const check = (name, ok, detail) => {
  console.log(`  ${ok ? 'ok  ' : 'FAIL'} ${name}${ok ? '' : ' — ' + detail}`);
  if (!ok) failed++;
};
const LIVE_SEL = '[aria-live],[role=alert],[role=status],[role=log],[role=alertdialog],output';

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
  // reveal-then-wait idiom aria-state.mjs uses for this exact setGate/hidden trick. #tgAnnounce itself is
  // static markup, but toast()/announce()/cfgSealForm()/cfgElevateOverlay() below are bundle globals that
  // do not exist until the script has run this far.
  await page.waitForFunction(() => typeof views === 'object' && Object.keys(views).length > 5).catch(() => {});

  // 1. an app-level region that is RENDERED (the defect was a region that existed but was hidden)
  const ann = await page.evaluate(sel => {
    const r = document.querySelector('#tgAnnounce');
    if (!r) return { missing: true };
    const cs = getComputedStyle(r);
    return { role: r.getAttribute('role'), live: r.getAttribute('aria-live'), atomic: r.getAttribute('aria-atomic'),
             display: cs.display, visibility: cs.visibility, inApp: !!r.closest('#appRoot'),
             rendered: cs.display !== 'none' && cs.visibility !== 'hidden',
             regions: document.querySelectorAll(sel).length };
  }, LIVE_SEL);
  check('an app-level announcer exists', !ann.missing, '#tgAnnounce not found');
  if (!ann.missing) {
    check('it is polite, atomic and a status region', ann.role === 'status' && ann.live === 'polite' && ann.atomic === 'true', JSON.stringify(ann));
    check('it lives INSIDE #appRoot (survives view switches, unlike the gate region)', ann.inApp, 'it is outside the app root');
    check('it is RENDERED, not display:none (a hidden region announces nothing)', ann.rendered,
          `display=${ann.display} visibility=${ann.visibility} — this is exactly why #agErr was inert`);
  }

  // 2. a toast must reach it
  const toasted = await page.evaluate(async () => {
    toast('probe message for the announcer', 'lock');
    await new Promise(r => requestAnimationFrame(() => requestAnimationFrame(r)));
    return (document.querySelector('#tgAnnounce') || {}).textContent || '';
  });
  check('a toast is announced, not only shown', /probe message/.test(toasted), `announcer held "${toasted}"`);

  // 3. repeating the SAME string must re-fire (a live region ignores an identical value)
  const repeated = await page.evaluate(async () => {
    const r = document.querySelector('#tgAnnounce');
    announce('same string'); await new Promise(x => requestAnimationFrame(() => requestAnimationFrame(x)));
    const first = r.textContent;
    r.__seenEmpty = false;
    const obs = new MutationObserver(() => { if (!r.textContent) r.__seenEmpty = true; });
    obs.observe(r, { childList: true, characterData: true, subtree: true });
    announce('same string'); await new Promise(x => requestAnimationFrame(() => requestAnimationFrame(x)));
    obs.disconnect();
    return { first, second: r.textContent, cleared: r.__seenEmpty };
  });
  check('repeating an identical message CLEARS first so it re-announces', repeated.cleared,
        `no clear observed — a screen reader would stay silent on the repeat (${JSON.stringify(repeated)})`);

  // 4. every per-form status container is itself a live region
  const forms = await page.evaluate(() => {
    const out = {};
    if (typeof cfgSealForm === 'function') {
      const b = cfgSealForm(); document.body.appendChild(b);
      const m = b.querySelector('[role="status"],[aria-live]');
      out.seal = !!m; document.body.removeChild(b);
    }
    if (typeof cfgElevateOverlay === 'function') {
      cfgElevateOverlay();
      const e = document.querySelector('#cfgAdmErr');
      out.admin = e ? { role: e.getAttribute('role'), live: e.getAttribute('aria-live') } : null;
      const el = document.querySelector('#cfgElevate'); if (el) el.remove();
    }
    return out;
  });
  check('the seal form carries its own live region', forms.seal === true, 'no [role=status] inside the seal form');
  check('the admin step-up error is an ALERT (a failed elevation is urgent)',
        forms.admin && forms.admin.role === 'alert' && forms.admin.live === 'assertive', JSON.stringify(forms.admin));

  // 5. the guard: this must not pass because someone deleted every message container
  check('there is MORE than one live region now (the pre-fix count was 1, and it was hidden)',
        ann.regions > 1, `${ann.regions} region(s) — the console had exactly 1 before, and it was inert`);
} finally { await browser.close(); }

if (failed) { console.log(`\nannouncements: ${failed} FAILED`); process.exit(1); }
console.log('\nannouncements: all checks passed');
