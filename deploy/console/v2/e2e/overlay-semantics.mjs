// EVERY OVERLAY OBEYS ONE MODAL DISCIPLINE.
//
// Measured live on the deployed console, both themes, 1440x900, before this change:
//   · the session drawer was exposed to a screen reader as an unnamed "complementary" landmark, never a
//     dialog, and once opened even ONCE its CLOSED body left 4 permanent off-screen tab stops on EVERY view
//     (baseline walk of 55 tab stops before any open: 0 inside #drawer; after one open: 4);
//   · no overlay trapped focus — with the drawer open, Tab #5 left it for BODY then the skip link, and 41
//     headings/links/buttons outside it stayed in the accessibility tree;
//   · tabbing past the last item of the OPEN account menu landed directly on the KILL button, menu still open;
//   · the admin step-up — the credential prompt gating every control-plane write — had NO Escape path at all;
//   · closing the drawer stranded focus on the now-hidden panel instead of returning it to the trigger.
//
// The assertions below are deliberately behavioural: they drive real keyboard events against the real
// handlers and read document.activeElement, because every one of those defects was invisible in the source
// and only showed up when something actually pressed Tab.
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
  const errs = [];
  page.on('pageerror', e => errs.push(String(e).slice(0, 160)));
  await page.goto(`${BASE}/index.html`, { waitUntil: 'domcontentloaded' });
  await page.evaluate(() => { // Reveal the console the way the APP does. Poking #appRoot.hidden used to be enough; since the auth
    // gate became a real modal it also inerts #appRoot, so a bare hidden=false leaves a console that is
    // visible and cannot take focus — which is the correct production behaviour for an unauthenticated
    // page, and the reason this line must go through setGate rather than reach past it.
    if (typeof setGate === 'function') { setGate(false); const r = document.querySelector('#appRoot'); if (r) r.hidden = false; }
    else { const r = document.querySelector('#appRoot'); if (r) r.hidden = false; } });
  await page.waitForFunction(() => { const r = document.querySelector('#appRoot'); return r && !r.hidden; });

  check('the shared overlay discipline exists', await page.evaluate(() => typeof overlayOpen === 'function' && typeof overlayClose === 'function'),
        'overlayOpen/overlayClose missing — nothing below would prove anything');

  // ---------- command palette ----------
  await page.evaluate(() => { const t = document.querySelector('#palOpen'); if (t) t.focus(); });
  await page.keyboard.press('Control+k');
  await page.waitForFunction(() => !!document.querySelector('#palScrim.open') && document.activeElement === document.querySelector('#palInput'));
  const palOpen = await page.evaluate(() => {
    const s = document.querySelector('#palScrim');
    return { modal: s.getAttribute('aria-modal'), role: s.getAttribute('role'), name: s.getAttribute('aria-label'),
             inInput: document.activeElement === document.querySelector('#palInput'),
             appInert: document.querySelector('#appRoot').hasAttribute('inert') };
  });
  check('palette: role=dialog, aria-modal, named', palOpen.role === 'dialog' && palOpen.modal === 'true' && !!palOpen.name, JSON.stringify(palOpen));
  check('palette: focus moves into it', palOpen.inInput, 'focus did not enter the palette');
  check('palette: the page behind it is INERT', palOpen.appInert, 'the app behind the modal is still reachable');
  // Tab must not escape: press it more times than the overlay has stops.
  for (let i = 0; i < 8; i++) await page.keyboard.press('Tab');
  const stillIn = await page.evaluate(() => document.querySelector('#palScrim').contains(document.activeElement));
  check('palette: Tab is TRAPPED inside', stillIn, 'focus escaped the palette after 8 tabs');
  await page.keyboard.press('Escape');
  await page.waitForFunction(() => !document.querySelector('#palScrim.open') && document.activeElement && document.activeElement.id === 'palOpen');
  const palClosed = await page.evaluate(() => ({
    open: !!document.querySelector('#palScrim.open'),
    active: document.activeElement ? (document.activeElement.id || document.activeElement.tagName) : null,
    appInert: document.querySelector('#appRoot').hasAttribute('inert'),
  }));
  check('palette: Escape closes it', !palClosed.open, 'still open');
  check('palette: focus RETURNS to the trigger', palClosed.active === 'palOpen', `focus landed on "${palClosed.active}"`);
  check('palette: the page is reachable again', !palClosed.appInert, 'the app stayed inert after close');

  // ---------- session drawer ----------
  const drawerBaseline = await page.evaluate(() => document.querySelectorAll('#drawer a[href],#drawer button,#drawer input,#drawer [tabindex]:not([tabindex="-1"])').length);
  const drawerOpen = await page.evaluate(() => {
    const trigger = document.querySelector('#palOpen'); trigger.focus();
    openSession((typeof SESSIONS !== 'undefined' && SESSIONS[0]) ? SESSIONS[0] : null);
    const d = document.querySelector('#drawer');
    return { role: d.getAttribute('role'), modal: d.getAttribute('aria-modal'), name: d.getAttribute('aria-label'),
             hidden: d.getAttribute('aria-hidden'), inside: d.contains(document.activeElement),
             appInert: document.querySelector('#appRoot').hasAttribute('inert') };
  });
  check('drawer: is a DIALOG, not an unnamed landmark', drawerOpen.role === 'dialog' && !!drawerOpen.name, JSON.stringify(drawerOpen));
  check('drawer: aria-modal and not aria-hidden while open', drawerOpen.modal === 'true' && drawerOpen.hidden !== 'true', JSON.stringify(drawerOpen));
  check('drawer: focus moves inside', drawerOpen.inside, 'focus stayed outside the open drawer');
  check('drawer: the page behind it is INERT', drawerOpen.appInert, 'background reachable behind the drawer');
  for (let i = 0; i < 10; i++) await page.keyboard.press('Tab');
  check('drawer: Tab is TRAPPED inside', await page.evaluate(() => document.querySelector('#drawer').contains(document.activeElement)),
        'focus escaped the drawer after 10 tabs');
  await page.keyboard.press('Escape');
  await page.waitForFunction(() => document.querySelector('#drawer')?.hasAttribute('inert') && document.activeElement && document.activeElement.id === 'palOpen');
  const drawerClosed = await page.evaluate(() => ({
    active: document.activeElement ? (document.activeElement.id || document.activeElement.tagName) : null,
    stops: document.querySelectorAll('#drawer a[href],#drawer button,#drawer input,#drawer [tabindex]:not([tabindex="-1"])').length,
    inert: document.querySelector('#drawer').hasAttribute('inert'),
    appInert: document.querySelector('#appRoot').hasAttribute('inert'),
  }));
  check('drawer: Escape returns focus to the trigger', drawerClosed.active === 'palOpen', `focus landed on "${drawerClosed.active}"`);
  check('drawer: a CLOSED drawer leaves no reachable tab stops', drawerClosed.inert,
        `closed drawer still holds ${drawerClosed.stops} focusable descendants and is not inert (baseline before any open: ${drawerBaseline})`);
  check('drawer: the page is reachable again', !drawerClosed.appInert, 'the app stayed inert after close');

  // ---------- account menu (a MENU: Tab closes it, Escape returns focus) ----------
  const menu = await page.evaluate(async () => {
    const btn = document.querySelector('#acctBtn'); if (!btn) return null;
    btn.focus(); btn.click();
    return { open: !document.querySelector('#acctMenu').hidden, expanded: btn.getAttribute('aria-expanded') };
  });
  if (!menu) { check('account menu present', false, '#acctBtn not found'); }
  else {
    check('account menu: opens and reports aria-expanded', menu.open && menu.expanded === 'true', JSON.stringify(menu));
    await page.keyboard.press('Tab');
    await page.waitForFunction(() => document.querySelector('#acctMenu')?.hidden);
    check('account menu: Tab CLOSES it (never left open while focus walks to KILL)',
          await page.evaluate(() => document.querySelector('#acctMenu').hidden), 'menu stayed open while focus moved on');
    await page.evaluate(() => { const b = document.querySelector('#acctBtn'); b.focus(); b.click(); });
    await page.keyboard.press('Escape');
    await page.waitForFunction(() => document.querySelector('#acctMenu')?.hidden && document.activeElement && document.activeElement.id === 'acctBtn');
    const esc = await page.evaluate(() => ({ hidden: document.querySelector('#acctMenu').hidden,
      active: document.activeElement ? (document.activeElement.id || document.activeElement.tagName) : null }));
    check('account menu: Escape closes and returns focus to the trigger', esc.hidden && esc.active === 'acctBtn', JSON.stringify(esc));
  }

  // ---------- admin step-up ----------
  const adm = await page.evaluate(() => {
    if (typeof cfgElevateOverlay !== 'function') return { missing: true };
    const trigger = document.querySelector('#palOpen'); trigger.focus();
    cfgElevateOverlay();
    const el = document.querySelector('#cfgElevate');
    return { present: !!el, role: el && el.getAttribute('role'), modal: el && el.getAttribute('aria-modal'),
             name: el && el.getAttribute('aria-label'),
             inside: el ? el.contains(document.activeElement) : false,
             nameLabel: (document.querySelector('#cfgAdmName') || {}).getAttribute
               ? document.querySelector('#cfgAdmName').getAttribute('aria-label') : null };
  });
  if (adm.missing) check('cfgElevateOverlay is reachable', false, 'function not exposed — the admin modal cannot be checked');
  else {
    check('admin step-up: opens as a named modal dialog', adm.present && adm.modal === 'true' && !!adm.name, JSON.stringify(adm));
    check('admin step-up: focus moves into it', adm.inside, 'focus stayed outside the credential prompt');
    check('admin step-up: its inputs carry real names, not just placeholders', adm.nameLabel === 'Admin name', `aria-label=${JSON.stringify(adm.nameLabel)}`);
    await page.keyboard.press('Escape');
    await page.waitForFunction(() => !document.querySelector('#cfgElevate') && document.activeElement && document.activeElement.id === 'palOpen');
    const admClosed = await page.evaluate(() => ({ gone: !document.querySelector('#cfgElevate'),
      active: document.activeElement ? (document.activeElement.id || document.activeElement.tagName) : null }));
    check('admin step-up: ESCAPE dismisses it', admClosed.gone, 'the credential prompt could not be dismissed with Escape');
    check('admin step-up: focus returns to the trigger', admClosed.active === 'palOpen', `focus landed on "${admClosed.active}"`);
  }

  check('no JS errors while driving every overlay', errs.length === 0, errs.slice(0, 2).join(' | '));
} finally { await browser.close(); }

if (failed) { console.log(`\noverlay-semantics: ${failed} FAILED`); process.exit(1); }
console.log('\noverlay-semantics: all checks passed');
