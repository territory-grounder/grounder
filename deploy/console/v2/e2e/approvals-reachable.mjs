// THE OPERATOR MUST BE ABLE TO ANSWER A DECISION THAT IS WAITING ON THEM.
//
// liveApprovalsStrip() renders the POLL_PAUSE decisions the Runner is blocked on, each with Approve/Deny
// gated on the server's own caller_can_act. It appeared EXACTLY TWICE in the whole bundle — its definition
// and the assembled mirror — and had ZERO call sites. Its own comment says it was "Extracted so the
// Workflows view can compose it", so the call site was lost, not withheld.
//
// Measured live at the moment of the fix: GET /v1/decisions returned 15 open decisions and ALL 15 carried
// caller_can_act:true for the signed-in operator. There was no way to vote on any of them from the console.
//
// ★ WHY THIS ORACLE COUNTS ROWS RATHER THAN ASKING "IS THERE AN APPROVE BUTTON". A bare existence check
// stays GREEN on an empty queue — which is most of the time — and would therefore have passed on the broken
// console any moment no decision happened to be open. So it drives all THREE states the strip already
// handles and asserts the row count matches the payload.
import { chromium } from 'playwright';

const BASE = process.env.CONSOLE_BASE || `http://127.0.0.1:${process.env.CONSOLE_E2E_PORT || '8137'}`;
let failed = 0;
const check = (name, ok, detail) => {
  console.log(`  ${ok ? 'ok  ' : 'FAIL'} ${name}${ok ? '' : ' — ' + detail}`);
  if (!ok) failed++;
};
const WHOAMI = { source: 'operator:test', mode: 'Semi-auto', may_actuate: true, posture_stale: false, posture_source: 'test' };
const mkDec = (n, canAct) => ({
  decisions: Array.from({ length: n }, (_, i) => ({
    external_ref: 'tg-test-' + i, action_id: 'aaaabbbb' + i, band: 'POLL_PAUSE',
    op_class: 'restart-service', host: 'dc1test0' + i, caller_can_act: canAct,
  })),
});

// SESSIONS_PAGE is a POPULATED live spine — the production shape. Its only job is to make
// views.command take the branch a real operator takes.
const SESSIONS_PAGE = {
  total: 1490,
  sessions: Array.from({ length: 12 }, (_, i) => ({
    external_ref: `librenms-dc1-${181900 + i}`,
    band: i % 3 === 0 ? 'POLL_PAUSE' : i % 3 === 1 ? 'AUTO' : 'AUTO_NOTICE',
    risk_level: 'low',
    action_id: `a${i}`.padEnd(8, '0'),
    verdict: i % 4 === 0 ? 'deviation' : 'match',
    classified_at: '2026-07-29T19:00:00Z',
  })),
};

const load = async (browser, decisionsBody, status, spine = SESSIONS_PAGE) => {
  const ctx = await browser.newContext();
  const page = await ctx.newPage();
  await page.route('**/v1/**', async r => {
    const u = r.request().url();
    if (u.includes('/v1/whoami')) return r.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(WHOAMI) });
    if (u.includes('/v1/events')) return r.abort();
    if (u.includes('/v1/decisions')) return status === 200
      ? r.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(decisionsBody) })
      : r.fulfill({ status, contentType: 'application/json', body: '{}' });
    // ★★★ THIS LINE MADE THE ORACLE GREEN FOR EXACTLY THE REASON PRODUCTION WAS BROKEN.
    // Fulfilling every other /v1/ call with `{}` left liveState.sessions un-populated, so views.command
    // only ever took its EMPTY-SPINE branch — the single branch where the element carrying the approvals
    // strip is the one returned. In production the spine ALWAYS has rows, the function returns a different
    // element, and the strip was discarded on every page load. This oracle asserted the control was
    // reachable in the only state production never reaches, and stayed green through a live defect that
    // left 15-17 POLL_PAUSE decisions unvotable, one of them for over 21 hours.
    // So /v1/sessions now returns a POPULATED page: the production branch is the one under test.
    if (u.includes('/v1/sessions')) return r.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(spine) });
    return r.fulfill({ status: 200, contentType: 'application/json', body: '{}' });
  });
  await page.goto(`${BASE}/index.html`, { waitUntil: 'domcontentloaded' });
  await page.evaluate(() => { // Reveal the console the way the APP does. Poking #appRoot.hidden used to be enough; since the auth
    // gate became a real modal it also inerts #appRoot, so a bare hidden=false leaves a console that is
    // visible and cannot take focus — which is the correct production behaviour for an unauthenticated
    // page, and the reason this line must go through setGate rather than reach past it.
    if (typeof setGate === 'function') { setGate(false); const r = document.querySelector('#appRoot'); if (r) r.hidden = false; }
    else { const r = document.querySelector('#appRoot'); if (r) r.hidden = false; } });
  await page.evaluate(async () => { try { await liveAdopt(); } catch (e) {} });
  // liveAdopt() is already fully awaited above — every in-chain fetch and the post-adopt re-render have
  // resolved by the time page.evaluate() returns — so this is margin for the DOM to settle, not a guess at
  // fetch latency.
  await page.evaluate(() => new Promise(r => requestAnimationFrame(() => r())));
  const out = await page.evaluate(() => {
    try { route('command'); } catch (e) {}
    const v = document.querySelector('#view');
    const btns = [...v.querySelectorAll('button')].map(b => (b.textContent || '').trim());
    // Scope to the STRIP. #command is dominated by the audit spine, so comparing the view's leading text
    // measured the wrong element in all three states and made the distinctness guard fire spuriously.
    const wrap = v.firstElementChild;
    const kids = wrap ? [...wrap.children] : [];
    const strip = kids.find(k => k.querySelector('.appr-row')
      || /awaiting a vote|not served by this control plane|circuit-breaker/i.test(k.textContent || ''));
    return { text: strip ? (strip.innerText || '') : '', viewText: v.innerText || '',
             found: !!strip, idx: kids.indexOf(strip),
             rows: v.querySelectorAll('.appr-row').length,
             approve: btns.filter(t => t === 'Approve').length, deny: btns.filter(t => t === 'Deny').length };
  });
  await ctx.close();
  return out;
};

const browser = await chromium.launch();
try {
  // 1. the strip is COMPOSED at all — the entire defect was a missing call site
  const three = await load(browser, mkDec(3, true), 200);
  check('the approvals strip is reachable from the default view', three.rows > 0,
        `0 .appr-row on #command — the control is defined and never called`);
  check('one row per open decision (not "an Approve button exists somewhere")', three.rows === 3,
        `${three.rows} rows for 3 decisions`);
  check('each actionable row offers Approve AND Deny', three.approve === 3 && three.deny === 3,
        `${three.approve} approve / ${three.deny} deny`);

  // 2. authority is the SERVER's call, and the strip must honour it
  const noAuth = await load(browser, mkDec(3, false), 200);
  check('rows still render when the operator lacks authority', noAuth.rows === 3, `${noAuth.rows} rows`);
  check('but NO vote control is offered without caller_can_act', noAuth.approve === 0 && noAuth.deny === 0,
        `${noAuth.approve} approve / ${noAuth.deny} deny rendered for an unauthorised operator`);
  check('and it says why, rather than showing a dead button', /read-only session|authority/i.test(noAuth.text),
        `text: "${noAuth.text.slice(0, 140)}"`);

  // 3. an EMPTY queue must read as empty, not as broken — and vice versa
  const empty = await load(browser, { decisions: [] }, 200);
  check('an empty queue says nothing is waiting', /No decisions awaiting a vote|awaiting a vote/i.test(empty.text) && empty.rows === 0,
        `rows=${empty.rows} text="${empty.text.slice(0, 120)}"`);
  const broken = await load(browser, null, 500);

  // ★ BOTH RETURN BRANCHES, because views.command returns a DIFFERENT ELEMENT depending on whether the live
  // spine has rows, and the strip must survive either. Testing only the populated branch would leave the
  // empty one able to regress silently — which is the mirror image of the defect this oracle just missed:
  // it previously tested ONLY the empty branch and stayed green through a live production failure.
  // An empty spine is a real state (a fresh boot, a quiet estate), not a synthetic one.
  const emptySpine = await load(browser, mkDec(2, true), 200, { total: 0, sessions: [] });
  check('the strip survives the EMPTY-spine return branch too', emptySpine.rows === 2,
    `${emptySpine.rows} rows with an empty live spine — the other branch of the return drops the control`);
  check('and offers its vote controls there', emptySpine.approve === 2 && emptySpine.deny === 2,
    `${emptySpine.approve} approve / ${emptySpine.deny} deny`);
  check('a FAILED read does not read as an empty queue',
        !/No decisions awaiting a vote/i.test(broken.text), `text: "${broken.text.slice(0, 140)}"`);

  // 4. the guard that stops this passing for the wrong reason
  check('the strip is FOUND in all three states', three.found && empty.found && broken.found,
        `found: populated=${three.found} empty=${empty.found} failed=${broken.found}`);
  check('the three states are DISTINGUISHABLE from each other',
        new Set([three.text, empty.text, broken.text]).size === 3,
        `populated="${three.text.slice(0,60)}" | empty="${empty.text.slice(0,60)}" | failed="${broken.text.slice(0,60)}"`);
  check('the strip sits near the top of the operator landing view', three.idx >= 0 && three.idx <= 2,
        `index ${three.idx} — a control for an unanswered queue must not be buried`);
} finally { await browser.close(); }

if (failed) { console.log(`\napprovals-reachable: ${failed} FAILED`); process.exit(1); }
console.log('\napprovals-reachable: all checks passed');
