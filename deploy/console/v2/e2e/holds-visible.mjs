// THE OPERATOR MUST BE ABLE TO SEE WHAT TG DECIDED NOT TO HEAL (TG-667, deep-review F6/F7).
//
// liveHoldsStrip() renders GET /v1/holds: the holds the autonomous approver (or an operator) decided and still
// remembers, and the investigations that ended failed:investigate with no retry. On 2026-11-07 ten holds and two
// unretried failures existed and NO surface showed them — from the console, an incident TG had chosen to leave
// alone looked like one it had healed.
//
// ★ ROWS ARE COUNTED AGAINST THE PAYLOAD, in all three states the strip handles, on BOTH return branches of
// views.command (populated live spine / empty spine) — the approvals strip was lost for weeks on exactly the
// branch its oracle never drove (see approvals-reachable.mjs). A bare "is there a held strip" check would stay
// green on an empty hold list, which is most of the time.
import { chromium } from 'playwright';

const BASE = process.env.CONSOLE_BASE || `http://127.0.0.1:${process.env.CONSOLE_E2E_PORT || '8137'}`;
let failed = 0;
const check = (name, ok, detail) => {
  console.log(`  ${ok ? 'ok  ' : 'FAIL'} ${name}${ok ? '' : ' — ' + detail}`);
  if (!ok) failed++;
};
const WHOAMI = { source: 'operator:test', mode: 'Semi-auto', may_actuate: true, posture_stale: false, posture_source: 'test' };
const mkHolds = (nHold, nFailed) => ({
  holds: Array.from({ length: nHold }, (_, i) => ({
    external_ref: 'tg-hold-' + i, host: 'dc1demo-hold0' + i, alert_rule: 'Devices-up/down', op_class: 'start-guest',
    reason_key: i % 2 ? 'human-deny' : 'actor-attributed-authorized',
    reading: i % 2 ? 'an operator denied it' : 'a person made this change deliberately',
    why: 'Held: the pve task log shows root@pam stopped it 4 minutes before the alert',
    actor_attribution: 'attributed-authorized', decided_by: 'tg:fallback-approver',
    decided_at: '2026-11-07T18:00:00Z', expires_at: '2026-11-08T18:00:00Z', sticky: true, age_seconds: 3600 * (i + 1),
  })),
  failed_investigations: Array.from({ length: nFailed }, (_, i) => ({
    external_ref: 'tg-failed-' + i, host: 'dc1demo-fail0' + i, alert_rule: 'Service-up/down',
    stop_reason: 'gateway-dial', failed_at: '2026-11-07T17:00:00Z', age_seconds: 7200,
  })),
  failed_lookback: '168h0m0s', as_of: '2026-11-07T22:00:00Z',
});

const SESSIONS_PAGE = {
  total: 1490,
  sessions: Array.from({ length: 12 }, (_, i) => ({
    external_ref: `librenms-dc1-${181900 + i}`,
    band: i % 3 === 0 ? 'POLL_PAUSE' : i % 3 === 1 ? 'AUTO' : 'AUTO_NOTICE',
    risk_level: 'low', action_id: `a${i}`.padEnd(8, '0'),
    verdict: i % 4 === 0 ? 'deviation' : 'match', classified_at: '2026-07-29T19:00:00Z',
  })),
};

const load = async (browser, holdsBody, status, spine = SESSIONS_PAGE) => {
  const ctx = await browser.newContext();
  const page = await ctx.newPage();
  await page.route('**/v1/**', async r => {
    const u = r.request().url();
    if (u.includes('/v1/whoami')) return r.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(WHOAMI) });
    if (u.includes('/v1/events')) return r.abort();
    if (u.includes('/v1/holds')) return status === 200
      ? r.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(holdsBody) })
      : r.fulfill({ status, contentType: 'application/json', body: '{}' });
    if (u.includes('/v1/decisions')) return r.fulfill({ status: 200, contentType: 'application/json', body: '{"decisions":[]}' });
    // The live spine is POPULATED (the production shape) unless a state asks for the empty branch.
    if (u.includes('/v1/sessions')) return r.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(spine) });
    return r.fulfill({ status: 200, contentType: 'application/json', body: '{}' });
  });
  await page.goto(`${BASE}/index.html`, { waitUntil: 'domcontentloaded' });
  await page.evaluate(() => {
    if (typeof setGate === 'function') { setGate(false); const r = document.querySelector('#appRoot'); if (r) r.hidden = false; }
    else { const r = document.querySelector('#appRoot'); if (r) r.hidden = false; } });
  await page.evaluate(async () => { try { await liveAdopt(); } catch (e) {} });
  await page.evaluate(() => new Promise(r => requestAnimationFrame(() => r())));
  const out = await page.evaluate(() => {
    try { route('command'); } catch (e) {}
    const v = document.querySelector('#view');
    const wrap = v.firstElementChild;
    const kids = wrap ? [...wrap.children] : [];
    const strip = kids.find(k => k.querySelector('.hold-row, .failed-row')
      || /LIVE · held|Nothing is held|\/v1\/holds is not served/i.test(k.textContent || ''));
    return { text: strip ? (strip.innerText || '') : '', found: !!strip,
             holds: v.querySelectorAll('.hold-row').length, failed: v.querySelectorAll('.failed-row').length };
  });
  await ctx.close();
  return out;
};

const browser = await chromium.launch();
try {
  // 1. the strip is COMPOSED, and one row per payload item
  const some = await load(browser, mkHolds(3, 2), 200);
  check('the held strip is reachable from the default view', some.found, 'no held strip on #command');
  check('one .hold-row per open hold', some.holds === 3, `${some.holds} rows for 3 holds`);
  check('one .failed-row per unretried failed investigation', some.failed === 2, `${some.failed} rows for 2 failures`);
  check('a hold row says WHO decided and WHY (the reading + the rationale)',
        /fallback-approver/.test(some.text) && /a person made this change deliberately/.test(some.text) && /root@pam stopped it/.test(some.text),
        `text: "${some.text.slice(0, 200)}"`);
  check('a failed row says TG will not return on its own', /will not return to it on its own/.test(some.text),
        `text: "${some.text.slice(0, 200)}"`);

  // 2. an EMPTY list reads as empty, not broken — and a FAILED read never reads as empty
  const empty = await load(browser, mkHolds(0, 0), 200);
  check('an empty list says nothing is held', /Nothing is held/.test(empty.text) && empty.holds === 0 && empty.failed === 0,
        `holds=${empty.holds} failed=${empty.failed} text="${empty.text.slice(0, 120)}"`);
  const broken = await load(browser, null, 503);
  check('a 503 does not read as "nothing is held"', !/Nothing is held/.test(broken.text) && /not served/i.test(broken.text),
        `text: "${broken.text.slice(0, 140)}"`);
  // A 500 is NOT "not built": the holds are unknown, and the strip must say so rather than claim either.
  const err500 = await load(browser, null, 500);
  check('a 500 reads as UNKNOWN, neither "nothing is held" nor "not served"',
        !/Nothing is held/.test(err500.text) && !/not served/i.test(err500.text) && /UNKNOWN|could not be read/.test(err500.text),
        `text: "${err500.text.slice(0, 140)}"`);
  // An answer without the lists (an unrouted {} fall-through) may not claim an empty list either.
  const noLists = await load(browser, {}, 200);
  check('a 200 without a hold list does not read as "nothing is held"',
        !/Nothing is held/.test(noLists.text) && /without a hold list|UNKNOWN/.test(noLists.text) && noLists.holds === 0,
        `text: "${noLists.text.slice(0, 140)}"`);

  // 3. both return branches of views.command
  const emptySpine = await load(browser, mkHolds(2, 1), 200, { total: 0, sessions: [] });
  check('the strip survives the EMPTY-spine return branch too', emptySpine.holds === 2 && emptySpine.failed === 1,
        `${emptySpine.holds} hold rows / ${emptySpine.failed} failed rows with an empty live spine`);

  // 4. the guard that stops this passing for the wrong reason
  check('the strip is FOUND in all three states', some.found && empty.found && broken.found,
        `found: populated=${some.found} empty=${empty.found} failed=${broken.found}`);
  check('the three states are DISTINGUISHABLE from each other',
        new Set([some.text, empty.text, broken.text]).size === 3,
        `populated="${some.text.slice(0,60)}" | empty="${empty.text.slice(0,60)}" | failed="${broken.text.slice(0,60)}"`);
} finally {
  await browser.close();
}
if (failed) { console.log(`holds-visible: ${failed} check(s) FAILED`); process.exit(1); }
console.log('holds-visible: all checks passed');
