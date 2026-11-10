// TWO INVARIANTS THAT PROTECT THE ACTUATION CONTROLS, BOTH MEASURED BROKEN ON THE LIVE CONSOLE.
//
// 1. INERT IS REFERENCE-COUNTED. overlayClose released #appRoot's `inert` unconditionally, so closing an
//    overlay opened ON TOP of another handed the operator the whole background — including the KILL switch
//    and the Approve/Deny strip — while the lower modal was still painted over it.
//
// 2. AN ACTUATION CONTROL IS RESTORED BY IDENTITY OR NOT AT ALL. route()'s focus restore fell back to
//    ordinal position among same-tag siblings. Measured live: an operator holding Approve on decision
//    tg-liveness-dc1cloudbeaver01-1785313617…, another row answered and removed by someone else, and the
//    next 25s tick put focus on Approve for an UNRELATED action (librenms-dc1-1814075… start-guest).
//    Identical label, still attached, nothing visible changed. Enter after that tick actuates the wrong
//    target. The queue moves constantly (17 → 16 → 15 open inside one hour), so this is the ordinary case.
//
// Both are asserted over the REAL functions, and in BOTH directions: a fix that pinned focus permanently, or
// one that simply stopped restoring anything, must fail here too.
import { chromium } from 'playwright';

const BASE = process.env.CONSOLE_BASE || `http://127.0.0.1:${process.env.CONSOLE_E2E_PORT || '8137'}`;
let failed = 0;
const check = (name, ok, detail) => {
  console.log(`  ${ok ? 'ok  ' : 'FAIL'} ${name}${ok ? '' : ' — ' + detail}`);
  if (!ok) failed++;
};

// A populated decisions payload: the strip only renders vote buttons when caller_can_act is true.
const DECISIONS = {
  decisions: [0, 1, 2, 3].map(i => ({
    external_ref: `ref-${i}`, action_id: `act${i}0000000`, band: 'POLL_PAUSE',
    reversible: true, caller_can_act: true, prediction: `prediction ${i}`,
    plan: { approaches: [`approach ${i}`] },
  })),
};

const browser = await chromium.launch();
try {
  const page = await (await browser.newContext({ viewport: { width: 1400, height: 950 } })).newPage();
  await page.route('**/v1/**', async rt => {
    const u = rt.request().url();
    const j = b => rt.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(b) });
    if (u.includes('/v1/whoami')) return j({ source: 'operator:test', mode: 'Semi-auto', may_actuate: true, posture_stale: false, posture_source: 'test' });
    if (u.includes('/v1/events')) return rt.abort();
    if (u.includes('/v1/decisions')) return j(DECISIONS);
    if (u.includes('/v1/sessions')) return j({ total: 4, sessions: [] });
    return j({});
  });
  await page.goto(`${BASE}/index.html`, { waitUntil: 'domcontentloaded' });
  await page.evaluate(() => { if (typeof setGate === 'function') { setGate(false); const r = document.querySelector('#appRoot'); if (r) r.hidden = false; } });
  await page.evaluate(async () => { try { await liveAdopt(); } catch (e) {} });
  // liveAdopt() is already fully awaited above (decisions/sessions are read in its sequential in-chain, so
  // liveState is already settled) — one frame is enough margin for the DOM route('command') call below, not
  // a guess at fetch latency.
  await page.evaluate(() => new Promise(r => requestAnimationFrame(() => r())));

  // ---------- 1. NESTED OVERLAYS ----------
  const nest = await page.evaluate(async () => {
    const sleep = ms => new Promise(s => setTimeout(s, ms));
    const app = document.querySelector('#appRoot');
    // Two synthetic overlays outside #appRoot, which is exactly the drawer/palette relationship.
    const mk = id => {
      const el = document.createElement('div');
      el.id = id; el.style.position = 'fixed'; el.style.inset = '20px';
      el.appendChild(Object.assign(document.createElement('button'), { textContent: 'x' }));
      document.body.appendChild(el);
      return el;
    };
    const outer = mk('t-outer'), inner = mk('t-inner');
    overlayOpen(outer, { label: 'outer' });
    const afterOuter = app.hasAttribute('inert');
    overlayOpen(inner, { label: 'inner' });
    const afterInner = app.hasAttribute('inert');
    overlayClose(inner);
    await sleep(30);
    const afterInnerClosed = app.hasAttribute('inert');
    const outerStillOpen = overlayStack.some(o => o.el === outer);
    overlayClose(outer);
    await sleep(30);
    const afterBothClosed = app.hasAttribute('inert');
    outer.remove(); inner.remove();
    return { afterOuter, afterInner, afterInnerClosed, outerStillOpen, afterBothClosed };
  });
  check('opening an overlay inerts the app root', nest.afterOuter === true, JSON.stringify(nest));
  check('the outer overlay is still on the stack after the inner closes', nest.outerStillOpen === true, JSON.stringify(nest));
  check('closing the INNER overlay does NOT release inert while the outer is open', nest.afterInnerClosed === true,
    'the background — KILL switch, Approve/Deny — became reachable behind a modal that is still open');
  // The other direction: reference counting must still RELEASE. A counter that never decrements would leave
  // the console permanently inert, which this catches.
  check('closing the LAST overlay does release inert', nest.afterBothClosed === false,
    'inert leaked after every overlay closed — the console would be permanently unusable');

  // ---------- 2. ACTUATION CONTROLS ARE RESTORED BY IDENTITY ----------
  const strip = await page.evaluate(() => {
    route('command');
    const rows = Array.from(document.querySelectorAll('.appr-row'));
    const btns = rows.flatMap(r => Array.from(r.querySelectorAll('button')));
    return {
      rows: rows.length,
      voteButtons: btns.length,
      keyed: btns.filter(b => b.getAttribute('data-appr-key')).length,
      distinctKeys: new Set(btns.map(b => b.getAttribute('data-appr-key'))).size,
    };
  });
  check('the approvals strip rendered its rows', strip.rows >= 3, JSON.stringify(strip));
  check('every vote button carries a stable identity key', strip.keyed === strip.voteButtons,
    `${strip.keyed}/${strip.voteButtons} keyed — an unkeyed vote button falls to the ordinal path`);
  check('the keys are DISTINCT per button', strip.distinctKeys === strip.voteButtons,
    `${strip.distinctKeys} distinct keys for ${strip.voteButtons} buttons — a shared key rebinds across decisions`);

  // THE LIVE SCENARIO, REPRODUCED: hold Approve on row 3, another row is answered and vanishes, restore.
  const rebind = await page.evaluate(() => {
    route('command');
    const rows = Array.from(document.querySelectorAll('.appr-row'));
    const target = rows[2].querySelector('button');
    const heldKey = target.getAttribute('data-appr-key');
    target.focus();
    const key = routeFocusKey();
    rows[0].remove();                       // the queue moved
    routeRestoreFocus(key);
    const landed = document.activeElement;
    return {
      capturedByOrdinal: !!(key && key.nth !== undefined),
      heldKey,
      landedKey: landed && landed.getAttribute ? landed.getAttribute('data-appr-key') : null,
      landedOnHeldButton: landed === target,
    };
  });
  check('the captured key is NOT ordinal for a vote button', rebind.capturedByOrdinal === false,
    'an ordinal key rebinds to whatever now occupies the position');
  check('focus returns to the SAME decision\'s button after the queue moves', rebind.landedOnHeldButton === true,
    `held ${rebind.heldKey} but landed on ${rebind.landedKey} — this is approving an action the operator never read`);

  // ---------- 3. AN UNKEYED ACTUATION CONTROL DECLINES RESTORE RATHER THAN REBINDING ----------
  // This is the class guard: the fix must hold for a control nobody remembered to key.
  const unkeyed = await page.evaluate(() => {
    route('command');
    const rows = Array.from(document.querySelectorAll('.appr-row'));
    const b = rows[1].querySelector('button');
    b.removeAttribute('data-appr-key');     // a future verb, added without a key
    b.focus();
    const key = routeFocusKey();
    return { key: key === null ? 'declined' : JSON.stringify(key), isActuation: isActuationControl(b) };
  });
  check('an unkeyed actuation control is recognised as one', unkeyed.isActuation === true, JSON.stringify(unkeyed));
  check('and it DECLINES ordinal restore instead of rebinding', unkeyed.key === 'declined',
    `got ${unkeyed.key} — losing focus costs a Tab; landing on a stranger's Approve costs an unintended actuation`);

  // A NON-actuation control must still get the ordinal fallback, or this "fix" just deleted focus survival.
  const benign = await page.evaluate(() => {
    route('ledger');
    const el = document.querySelector('#view a[href], #view td, #view [tabindex="0"]');
    if (!el) return { skip: 'no benign focusable' };
    if (!el.hasAttribute('tabindex')) el.setAttribute('tabindex', '0');
    el.focus();
    const key = routeFocusKey();
    return { skip: null, gotKey: key !== null, kind: key ? (key.nth !== undefined ? 'ordinal' : 'selector') : 'none' };
  });
  if (benign.skip) console.log(`  ok   benign-control check skipped — ${benign.skip}`);
  else check('a NON-actuation control still gets a focus key (ordinal is fine there)', benign.gotKey === true,
    'the guard was applied too broadly and ordinary focus survival was removed');
} finally { await browser.close(); }

console.log(failed ? `overlay-nesting-and-actuation-focus: ${failed} FAILED` : 'overlay-nesting-and-actuation-focus: all checks passed');
process.exit(failed ? 1 : 0);
