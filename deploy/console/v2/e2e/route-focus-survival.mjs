// A RE-RENDER IS NOT A NAVIGATION, AND ONLY ONE OF THEM MAY MOVE FOCUS.
//
// I fixed focus survival inside #workflows and reported it fixed. The router has the SAME flaw one level up
// and I left it there: route() does `view.innerHTML=""` and `fb.innerHTML=""`, detaching every focusable
// element in the view and every facet chip. Measured live on the deployed console by an adversarial sweep
// and independently reproduced:
//
//   - the 25s poll calls liveAdopt(true) -> route(currentView) with NO user action, throwing a keyboard or
//     screen-reader operator to <body> on a timer;
//   - activating any of the 39 facet chips destroys the chip just pressed and drops focus to <body>, so
//     filtering to "Deviations" — the triage move before approving or denying an actuation — silently
//     returns the operator to the top of the page.
//
// This oracle asserts the DISTINCTION, in both directions, because a fix that simply pinned focus would
// break deliberate navigation: a route to the same view must KEEP focus, a route to a different view must
// MOVE it. Asserting only the first half would pass a console that had stopped navigating.
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
    if (typeof setGate === 'function') { setGate(false); const r = document.querySelector('#appRoot'); if (r) r.hidden = false; }
    route('command');
  });
  // No live mock here, so boot's own unawaited liveAdopt().catch() (index.html's last line) is racing this
  // reveal: its /v1/whoami 404s against the bare static server and lands in liveLoginOverlay(), which calls
  // setGate(true) — re-adding `inert` to #appRoot and undoing the setGate(false) above. Every check below
  // hinges on real focus (.focus() is a silent no-op inside an inert subtree), so the wait asserts the two
  // facts that actually matter — the reveal held AND route('command') painted something focusable — rather
  // than out-waiting a race with a guessed margin. If a regression re-inerts the app for good, this times out
  // and falls through to the "focusable element exists" check below, which then fails with the real reason.
  await page.waitForFunction(() => {
    const app = document.querySelector('#appRoot');
    return !!app && !app.hasAttribute('inert') &&
      !!document.querySelector('#view a[href], #view button:not([disabled]), #view [tabindex="0"], #view [role=button], #view [role=option]');
  }).catch(() => {});

  // ---- 1. THE UNPROVOKED CASE: a re-render of the SAME view keeps focus ----
  // This is what the 25s poll does. It is the half that mattered most, because no operator action causes it.
  const poll = await page.evaluate(() => {
    const first = document.querySelector('#view a[href], #view button:not([disabled]), #view [tabindex="0"], #view [role=button], #view [role=option]');
    if (!first) return { skip: 'no focusable element in #view' };
    first.focus();
    const before = document.activeElement === first;
    route('command');                       // exactly what liveAdopt(true) does
    const after = document.activeElement;
    return { before, skip: null, lostToBody: after === document.body, sameKind: after && after.tagName === first.tagName };
  });
  check('a focusable element exists to test with', !poll.skip, poll.skip || '');
  check('focus was taken before the re-render', poll.before === true, JSON.stringify(poll));
  check('a same-view re-render does NOT drop focus to <body>', poll.lostToBody === false,
    'the 25s poll throws a keyboard operator to the top of the document with no user action');

  // ---- 2. FACET ACTIVATION keeps focus on the chip the operator pressed ----
  // Over EVERY view that has facets, not one sample: the defect was 39 chips across 10 views.
  const facetViews = await page.evaluate(() => Object.keys(FACETS || {}));
  check('facet views are enumerable', facetViews.length >= 2, JSON.stringify(facetViews));
  const facetResults = [];
  for (const v of facetViews) {
    const r = await page.evaluate(async (view) => {
      route(view);
      await new Promise(res => setTimeout(res, 80));
      const chips = Array.from(document.querySelectorAll('#facets .facet'));
      const target = chips.find(c => c.getAttribute('aria-pressed') !== 'true') || chips[1] || chips[0];
      if (!target) return { view, skip: true };
      const k = target.getAttribute('data-k');
      target.focus();
      target.click();                       // route(view) fires inside the handler
      await new Promise(res => setTimeout(res, 80));
      const a = document.activeElement;
      return {
        view, skip: false, k,
        lostToBody: a === document.body,
        onSameChip: !!(a && a.getAttribute && a.getAttribute('data-k') === k && a.classList.contains('facet')),
        nowPressed: (document.querySelector(`#facets .facet[data-k="${k}"]`) || {}).getAttribute?.call(document.querySelector(`#facets .facet[data-k="${k}"]`), 'aria-pressed'),
      };
    }, v);
    if (!r.skip) facetResults.push(r);
  }
  check('facet chips were exercised', facetResults.length >= 2, `${facetResults.length} views with chips`);
  const bodyLosers = facetResults.filter(r => r.lostToBody);
  check('NO facet activation drops focus to <body>', bodyLosers.length === 0,
    `${bodyLosers.length} views lose focus: ${JSON.stringify(bodyLosers.map(r => r.view))}`);
  const offChip = facetResults.filter(r => !r.onSameChip);
  check('focus stays ON the chip the operator pressed', offChip.length === 0,
    `${offChip.length} views move focus off the pressed chip: ${JSON.stringify(offChip.map(r => r.view))}`);
  const notPressed = facetResults.filter(r => r.nowPressed !== 'true');
  check('and the chip actually became pressed (the filter still works)', notPressed.length === 0,
    JSON.stringify(notPressed.map(r => [r.view, r.nowPressed])));

  // ---- 3. THE OTHER DIRECTION: a route to a DIFFERENT view must MOVE focus ----
  // Without this, a fix that pinned focus unconditionally would pass — and deliberate navigation would
  // leave the operator's focus stranded on a control belonging to a view no longer on screen.
  const nav = await page.evaluate(async () => {
    route('command');
    await new Promise(res => setTimeout(res, 60));
    const first = document.querySelector('#view a[href], #view button:not([disabled]), #view [tabindex="0"], #view [role=button]');
    if (!first) return { skip: 'no focusable' };
    first.focus();
    const held = document.activeElement === first;
    const detachedBefore = first;
    route('ledger');                        // a DIFFERENT view: navigation
    await new Promise(res => setTimeout(res, 60));
    const a = document.activeElement;
    return {
      skip: null, held,
      stillOnOldElement: a === detachedBefore,
      oldElementDetached: !document.contains(detachedBefore),
    };
  });
  check('navigation test had a focusable element', !nav.skip, nav.skip || '');
  check('the old view really was torn down', nav.oldElementDetached === true, JSON.stringify(nav));
  check('a DIFFERENT-view route does not pin focus to the old view', nav.stillOnOldElement === false,
    'focus is stranded on a control from a view that is no longer rendered');

  // ★ NAVIGATION MUST NOT HAND FOCUS TO AN UNREQUESTED INTERACTIVE CONTROL.
  //
  // HONEST LIMITATION, RECORDED RATHER THAN GLOSSED: I could not build a mutation that makes this check
  // go red. Removing the `sameView` guard in route() — so focus is captured on navigation too — leaves this
  // suite GREEN, because the positional fallback finds no match in the freshly-rendered view and
  // routeRestoreFocus lands on the view CONTAINER, which this assertion permits (a container is a place; it
  // is not a control the operator did not choose). So the `sameView` guard is a CLARITY measure, not a
  // correctness one, and I am not claiming a control for it that I do not have.
  //
  // What this check DOES pin is the property that matters if the fallback ever changes: navigation may land
  // on the view or leave focus at the document root, and may never silently move the reading cursor onto an
  // arbitrary link or button inside content the operator has not been told about. Asserting only "focus is
  // not on the OLD element" would be vacuous — a detached node cannot hold focus, so that passes either way.
  const navLanding = await page.evaluate(async () => {
    route('command');
    await new Promise(res => setTimeout(res, 60));
    const first = document.querySelector('#view a[href], #view button:not([disabled]), #view [tabindex="0"], #view [role=button]');
    if (!first) return { skip: 'no focusable' };
    first.focus();
    route('api');                            // a different, content-rich view
    await new Promise(res => setTimeout(res, 80));
    const a = document.activeElement;
    const view = document.querySelector('#view');
    const interactive = !!(a && a !== view && a !== document.body && view.contains(a) &&
      a.matches('a[href],button,[role=button],[role=option],[tabindex="0"],input,select,textarea'));
    return { skip: null, tag: a ? a.tagName : null, isView: a === view, isBody: a === document.body, interactive };
  });
  check('navigation landing test ran', !navLanding.skip, navLanding.skip || '');
  check('navigation does NOT auto-focus an interactive control in the new view', navLanding.interactive === false,
    `focus landed on <${navLanding.tag}> inside the new view — the operator's reading cursor was moved to a ` +
    `control they never chose (this is what happens if focus is captured on navigation, not just on re-render)`);

  // ---- 4. scroll position is the same defect in another channel ----
  const scroll = await page.evaluate(async () => {
    route('command');
    await new Promise(res => setTimeout(res, 80));
    const view = document.querySelector('#view');
    view.scrollTop = 0;
    const scrollable = view.scrollHeight > view.clientHeight + 20;
    if (!scrollable) return { skip: 'view not scrollable at this size' };
    view.scrollTop = 40;
    const before = view.scrollTop;
    route('command');
    await new Promise(res => setTimeout(res, 60));
    return { skip: null, before, after: document.querySelector('#view').scrollTop };
  });
  if (scroll.skip) {
    console.log(`  ok   scroll check skipped — ${scroll.skip}`);
  } else {
    check('a same-view re-render does not scroll the operator back to the top', scroll.after === scroll.before,
      `scrollTop ${scroll.before} -> ${scroll.after}`);
  }
} finally { await browser.close(); }

console.log(failed ? `route-focus-survival: ${failed} FAILED` : 'route-focus-survival: all checks passed');
process.exit(failed ? 1 : 0);
