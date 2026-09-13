// EVERYTHING ABOVE #command SHIPS ON EVERY BRANCH — the oracle for the single-construction-site refactor.
//
// views.command carried a "prepend to X, return Y" shape: parts were built, then an early return ~80 lines
// below the build chose WHICH element left the function — `strip` on a populated spine (every production
// load), the fixture `wrap` otherwise. Anything attached to the loser was built and DISCARDED. That exact
// defect shipped four times in 36 hours (!744 the Approve/Deny strip, !757/!760 the config-gap card), and
// each time the oracle of the day was green because its /v1/sessions stub was EMPTY — it exercised the only
// branch on which the feature existed, and production never takes that branch.
//
// The refactor removed the second place: one root element, one visible append sequence, one return. This
// oracle pins the OBSERVABLE half of that invariant, on the REAL path shape:
//   - /v1/sessions is stubbed with a POPULATED page in the server's own DTO shape (sessions.go), so the
//     branch under test is the one a real operator lands on;
//   - on that branch, ALL the furniture coexists as siblings of ONE root, in page order: config-gap card,
//     approvals strip, live spine — and no fixture content or fixture banner;
//   - on the EMPTY branch (a real state: fresh boot), the SAME furniture still renders, the fixture preview
//     appears beneath the honest empty state, and the GLOBAL fixture banner names Command for it — so
//     neither branch can silently drop what the other carries.
import { chromium } from 'playwright';

const BASE = process.env.CONSOLE_BASE || `http://127.0.0.1:${process.env.CONSOLE_E2E_PORT || '8137'}`;
let failed = 0;
const check = (name, ok, detail) => {
  console.log(`  ${ok ? 'ok  ' : 'FAIL'} ${name}${ok ? '' : ' — ' + detail}`);
  if (!ok) failed++;
};

// The server's own SessionSummary shape (core/httpapi/sessions.go), populated — the production branch.
const SPINE = {
  total: 1490,
  sessions: Array.from({ length: 9 }, (_, i) => ({
    external_ref: `librenms-dc1-${182300 + i}`,
    band: i % 3 === 0 ? 'POLL_PAUSE' : i % 3 === 1 ? 'AUTO' : 'AUTO_NOTICE',
    risk_level: i % 3 === 0 ? 'medium' : 'low',
    action_id: `ac${i}`.padEnd(16, '0'),
    plan_hash: `ph${i}`.padEnd(16, '0'),
    auto_approved: i % 3 !== 0,
    notify_required: i % 3 === 2,
    operator_override: false,
    verdict: i % 4 === 0 ? 'deviation' : 'match',
    classified_at: '2026-07-30T06:0' + (i % 10) + ':00Z',
  })),
};
const DECISIONS = { decisions: Array.from({ length: 4 }, (_, i) => ({
  external_ref: `tg-cmd-${i}`, action_id: 'dddd' + String(i).padEnd(12, '0'), band: 'POLL_PAUSE',
  op_class: 'restart-service', host: `dc1test0${i}`, caller_can_act: true,
  prediction: 'service returns to running', plan: { approaches: ['restart'] },
})) };
// A gap row EARLY + 60 later rows: the row must be retained from the FULL page (it falls outside slice(-40)).
const LEDGER = (() => {
  const t0 = Date.now() - 3 * 3600 * 1000;
  const entries = [{ seq: 1, decision: 'config:gap-at-boot',
    reason: 'domain "journal" armed with NO sanctioned principals — every non-TG actor there reads attributed-suspicious',
    action_id: 'boot-1', created_at: new Date(t0).toISOString(), hash: 'h1', prev: '' }];
  for (let i = 0; i < 60; i++) entries.push({ seq: 2 + i, decision: 'suppress:escalate', reason: 'routine',
    action_id: `a${i}`, created_at: new Date(t0 + (i + 1) * 60000).toISOString(), hash: `h${2 + i}`, prev: `h${1 + i}` });
  return { entries };
})();

const load = async (browser, spine) => {
  const ctx = await browser.newContext({ viewport: { width: 1500, height: 1000 } });
  const page = await ctx.newPage();
  const errs = []; page.on('pageerror', e => errs.push(String(e)));
  await page.route('**/v1/**', async rt => {
    const u = rt.request().url();
    const j = b => rt.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(b) });
    if (u.includes('/v1/whoami')) return j({ source: 'operator:test', mode: 'Semi-auto', may_actuate: true, posture_stale: false, posture_source: 'test' });
    if (u.includes('/v1/events')) return rt.abort();
    if (u.includes('/v1/sessions')) return j(spine);
    if (u.includes('/v1/decisions')) return j(DECISIONS);
    if (u.includes('/v1/ledger')) return j(LEDGER);
    return j({});
  });
  await page.goto(`${BASE}/index.html`, { waitUntil: 'domcontentloaded' });
  await page.evaluate(() => { if (typeof setGate === 'function') { setGate(false); const r = document.querySelector('#appRoot'); if (r) r.hidden = false; } });
  await page.evaluate(async () => { try { await liveAdopt(); } catch (e) {} });
  // liveAdopt() is already fully awaited above — every in-chain fetch and the post-adopt re-render have
  // resolved by the time page.evaluate() returns — so this is margin for the DOM to settle, not a guess at
  // fetch latency.
  await page.evaluate(() => new Promise(r => requestAnimationFrame(() => r())));
  const out = await page.evaluate(() => {
    route('command');
    const v = document.querySelector('#view');
    const root = v.firstElementChild;
    const kids = root ? [...root.children] : [];
    // Locate each stratum AS A DIRECT CHILD of the one root — the single-assembly contract, not just
    // "somewhere on the page": a stratum that rendered inside a different container would mean a second
    // construction site grew back.
    const idxOf = pred => kids.findIndex(pred);
    const cfgIdx = idxOf(k => /CONTROL-PLANE CONFIGURATION/i.test(k.textContent || ''));
    const apprIdx = idxOf(k => k.querySelector('.appr-row') || /awaiting a vote|circuit-breaker/i.test(k.textContent || ''));
    const spineIdx = idxOf(k => /LIVE · audit spine/i.test(k.textContent || ''));
    const banner = document.querySelector('#fixtureBanner');
    return {
      rootChildren: kids.length,
      cfgIdx, apprIdx, spineIdx,
      apprRows: v.querySelectorAll('.appr-row').length,
      spineRows: [...v.querySelectorAll('td')].filter(td => /^librenms-dc1-\d+$/.test((td.textContent || '').trim())).length,
      spinePopulated: Array.isArray(liveState.sessions) && liveState.sessions.length > 0,
      configRetained: !!liveState.configGap,
      saysSpineEmpty: /The spine is empty/i.test(v.innerText || ''),
      fixtureContent: /dc1demo|dc2demo|s-8f31/.test(v.innerText || ''),
      bannerHidden: banner ? banner.hidden : null,
      bannerText: banner ? (banner.textContent || '').trim() : '',
      text: (v.innerText || '').slice(0, 400),
    };
  });
  out.errs = errs;
  await ctx.close();
  return out;
};

const browser = await chromium.launch();
try {
  // ---- the branch production takes: POPULATED spine, in the server's own shape ----
  const full = await load(browser, SPINE);
  check('the fixture drives the POPULATED-spine branch (spinePopulated === true)', full.spinePopulated === true,
    'an empty stub exercises the only branch the defect never lived on — the exact blindness that let it recur');
  check('the config-gap card is a direct child of the ONE root', full.cfgIdx >= 0,
    `not found among ${full.rootChildren} root children — built-and-discarded, or attached to a second container`);
  check('the approvals strip is a direct child of the ONE root', full.apprIdx >= 0,
    `not found among ${full.rootChildren} root children`);
  check('the live spine strip is a direct child of the ONE root', full.spineIdx >= 0,
    `not found among ${full.rootChildren} root children`);
  check('page order holds: config-gap card, approvals, spine', full.cfgIdx < full.apprIdx && full.apprIdx < full.spineIdx,
    `cfg=${full.cfgIdx} appr=${full.apprIdx} spine=${full.spineIdx}`);
  check('one approvals row per open decision', full.apprRows === DECISIONS.decisions.length,
    `${full.apprRows} rows for ${DECISIONS.decisions.length} decisions`);
  check('one spine row per fetched session', full.spineRows === SPINE.sessions.length,
    `${full.spineRows} rows for ${SPINE.sessions.length} sessions`);
  check('the config-gap row was retained from the FULL ledger page', full.configRetained === true,
    'the gap row fell off the render window and nothing kept it — the finding is discarded exactly as it was live');
  check('NO fixture content beneath a populated spine', full.fixtureContent === false, full.text);
  check('the global fixture banner is HIDDEN on the live branch', full.bannerHidden === true,
    `banner: "${full.bannerText}"`);

  // ---- the mirror branch: a real, EMPTY spine (fresh boot) — the same furniture must survive ----
  const empty = await load(browser, { total: 0, sessions: [] });
  check('EMPTY branch: the config-gap card still renders', empty.cfgIdx >= 0,
    'the other branch of the assembly dropped a stratum — the two-place shape is growing back');
  check('EMPTY branch: the approvals strip still renders with its rows', empty.apprIdx >= 0 && empty.apprRows === DECISIONS.decisions.length,
    `idx=${empty.apprIdx} rows=${empty.apprRows}`);
  check('EMPTY branch: the live strip states the spine is empty', empty.saysSpineEmpty === true, empty.text);
  check('EMPTY branch: the fixture preview renders beneath the honest empty state', empty.fixtureContent === true,
    'no design preview rendered — the fallback disappeared instead of being named');
  check('EMPTY branch: the GLOBAL fixture banner names Command for it',
    empty.bannerHidden === false && /Command/.test(empty.bannerText) && /FIXTURE DATA/i.test(empty.bannerText),
    `hidden=${empty.bannerHidden} text="${empty.bannerText}"`);
  check('no uncaught JS error on either branch', full.errs.length === 0 && empty.errs.length === 0,
    JSON.stringify([...full.errs, ...empty.errs]));
} finally { await browser.close(); }

if (failed) { console.log(`\ncommand-assembly: ${failed} FAILED`); process.exit(1); }
console.log('\ncommand-assembly: all checks passed');
